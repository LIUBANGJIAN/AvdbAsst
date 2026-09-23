package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// authCookieName 是访问口令校验通过后下发的标记 Cookie。
	authCookieName = "avdb_token"

	// searchCacheTTL 是搜索结果的进程内缓存时长。
	//
	// 为什么需要它：翻页与"改每页条数"都是同一关键词的重复查询。上游对
	// "SSIS" 这种宽泛词会一次返回 3000+ 条（实测），不缓存的话每翻一页都要
	// 重新等上游几秒。缓存只存**成功**的结果，TTL 很短，改完上游配置最多
	// 两分钟后必然生效。
	searchCacheTTL = 2 * time.Minute

	// searchCacheEntries 是缓存条目上限。单用户场景下同时活跃的关键词很少，
	// 8 条足够，超出按插入顺序淘汰最旧的一条。
	searchCacheEntries = 8

	// batchDownloadLimit 是单次批量下载请求最多接受的 tid 数量。
	// 前端会把更大的选择拆成多次调用，这里只做防御性上限。
	batchDownloadLimit = 100

	// batchDownloadWorkers 是批量提交时的并发度。
	// 上游是本地服务，4 路并发既能把 100 条压到几秒内完成，又不会把它打爆。
	batchDownloadWorkers = 4

	// siteName 是界面上显示的产品名。
	//
	// 刻意不含上游产品的字样：站点名会出现在浏览器标签、截图和搜索结果快照里，
	// 而用户明确要求界面上不要出现那个词。需要说明"数据来自哪里"时，
	// 文案统一用「上游」指代。
	siteName = "资源搜索"
)

// tidPattern 限定 tid 的合法字符集。上游用纯数字，这里放宽到常见 ID 字符，
// 但坚决挡掉斜杠、点、问号等会污染上游 URL 的字符。
var tidPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// App 持有全部依赖，便于在测试中替换。
//
// cfg 与 client 会被设置页面在线改写，因此用读写锁保护：
// 所有请求都通过 snapshot() 取一份快照使用，避免读到改到一半的状态
// （读写锁的 map/slice 撕裂、client 与 cfg 不匹配都属于这类问题）。
type App struct {
	mu     sync.RWMutex
	cfg    Config
	client *AvdbClient

	tpl     *template.Template
	static  fs.FS
	favicon []byte

	startedAt time.Time
	version   string
	build     string
	log       *slog.Logger

	// searchCache 缓存"同一关键词 + 同一上游"的搜索结果，供翻页复用。
	searchCache *resultCache

	// 配置目录可写性探测结果的缓存，由 mu 保护。
	writableValue bool
	writableAt    time.Time
}

// ------------------------------------------------------- 搜索结果短缓存

// cachedResult 是一条缓存记录。存的是**归一化后的完整结果**，
// 分页在切片阶段做，所以翻页不会改变任何字段。
type cachedResult struct {
	key   string
	items []Torrent
	at    time.Time
}

// resultCache 是带 TTL 与容量上限的极小缓存。
//
// 刻意做成"最多 N 条 + 按插入顺序淘汰"：单用户场景下不需要 LRU 的精度，
// 而少一个依赖路径就少一处能写错的地方。
type resultCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries []cachedResult
}

func newResultCache(ttl time.Duration) *resultCache {
	return &resultCache{ttl: ttl}
}

func (c *resultCache) get(key string, now time.Time) ([]Torrent, bool) {
	if c == nil || c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.entries {
		e := c.entries[i]
		if e.key != key {
			continue
		}
		if now.Sub(e.at) > c.ttl {
			c.entries = append(c.entries[:i], c.entries[i+1:]...)
			return nil, false
		}
		return e.items, true
	}
	return nil, false
}

func (c *resultCache) put(key string, items []Torrent, now time.Time) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// 同 key 覆盖，避免同一关键词堆出多条。
	for i := range c.entries {
		if c.entries[i].key == key {
			c.entries[i] = cachedResult{key: key, items: items, at: now}
			return
		}
	}
	c.entries = append(c.entries, cachedResult{key: key, items: items, at: now})
	if len(c.entries) > searchCacheEntries {
		c.entries = c.entries[len(c.entries)-searchCacheEntries:]
	}
}

// writableTTL 是配置目录可写性探测结果的缓存时长。
const writableTTL = 10 * time.Second

// NewApp 构造应用。所有依赖在这里显式注入，不使用全局变量。
//
// 刻意**不**在这里跑 normalizeConfig：那会按 TimeoutSec 重算 Timeout，
// 把"直接指定亚秒级超时"的能力抹掉（测试正依赖这一点）。
// 生产路径由 ResolveConfig 负责归一化。
func NewApp(cfg Config, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.Default()
	}
	app := &App{
		cfg:         cfg,
		client:      NewAvdbClient(cfg),
		tpl:         parseTemplates(),
		static:      staticRoot(),
		startedAt:   time.Now(),
		version:     version,
		build:       build,
		log:         logger,
		searchCache: newResultCache(searchCacheTTL),
	}
	if data, err := fs.ReadFile(webRoot(), "favicon.svg"); err == nil {
		app.favicon = data
	}
	return app
}

// snapshot 一次性取出配置与对应客户端，保证二者版本一致。
func (a *App) snapshot() (Config, *AvdbClient) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg, a.client
}

// currentConfig 只取配置时使用。
func (a *App) currentConfig() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

// applyConfig 热更新配置：重建客户端并原子替换。
// 页面保存后立即生效，不需要重启容器。
func (a *App) applyConfig(cfg Config) {
	client := NewAvdbClient(cfg)
	a.mu.Lock()
	a.cfg = cfg
	a.client = client
	a.mu.Unlock()
}

// Routes 组装路由与中间件链。
//
// 路由刻意做得宽容，因为"浏览器自定义搜索"的模板由用户手写，
// 多支持几种写法能显著降低配置出错率：
//
//	/s?q=xxx          ← 主推，也是 OpenSearch 使用的形式
//	/s?keyword=xxx    ← 兼容旧版
//	/search?q=xxx     ← 兼容旧版
//	/s/xxx            ← 路径式，某些书签工具更顺手
func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	a.registerRoutes(mux)
	return a.recoverMW(a.logMW(a.securityMW(a.authMW(mux))))
}

// registerRoutes 只负责挂载路由，便于测试单独取用裸 mux 做对比。
func (a *App) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", a.handleSearch)
	mux.HandleFunc("GET /s", a.handleSearch)
	mux.HandleFunc("GET /s/{q}", a.handleSearch)
	mux.HandleFunc("GET /search", a.handleSearch)
	mux.HandleFunc("GET /search/{q}", a.handleSearch)

	mux.HandleFunc("GET /api/search", a.handleAPISearch)

	// 提交下载会改变上游状态、且动用到本站保管的上游令牌，因此做两道收口：
	//  1. 只接受 POST —— 外部页面无法用 <img src="..."> 这类 GET 触发；
	//  2. 叠加同源校验，进一步堵住跨站表单提交。
	// 这两点缺一不可：仅靠来源校验挡不住把 referrerpolicy 设为 no-referrer 的 GET。
	mux.HandleFunc("POST /download", a.handleDownload)
	// 批量提交：结果页全选后一次最多上千条，逐条往返太贵，放服务端控并发。
	mux.HandleFunc("POST /download/batch", a.handleDownloadBatch)

	// 设置页面：GET 渲染表单，POST 保存（PRG 模式，避免刷新重复提交）。
	// 用 POST 而不是 PUT/DELETE，是为了在浏览器禁用 JS 时依然能提交表单。
	mux.HandleFunc("GET /settings", a.handleSettings)
	mux.HandleFunc("POST /settings", a.handleSettingsSave)
	mux.HandleFunc("POST /api/settings/test", a.handleSettingsTest)

	// 上游配置探测：只读操作，但能读上游设置，因此同样强制 POST + 同源。
	//   - default-rule：读上游全局默认下载目标（默认下载器 + 默认目录）；
	//   - directories：校验下载器标识并列出其下可选目录。
	// 两者都只用访问令牌，不需要上游账号密码。
	mux.HandleFunc("POST /api/settings/default-rule", a.handleSettingsDefaultRule)
	mux.HandleFunc("POST /api/settings/directories", a.handleSettingsDirectories)
	// 探测"上游到底配了哪个下载器"。上游把 downloader 改成必填之后，
	// 这是唯一一条用访问令牌就能问出该填什么的路。
	mux.HandleFunc("POST /api/settings/downloaders", a.handleSettingsDownloaders)

	mux.HandleFunc("GET /opensearch.xml", a.handleOpenSearch)
	mux.HandleFunc("GET /healthz", a.handleHealthz)
	mux.HandleFunc("GET /favicon.svg", a.handleFavicon)
	mux.HandleFunc("GET /favicon.ico", a.handleFavicon)
	mux.Handle("GET /static/", a.staticHandler())
}

// ---------------------------------------------------------------- 中间件

// statusRecorder 记录实际写出的状态码与字节数，供日志中间件使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (a *App) logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		a.log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"cost", time.Since(start).Round(time.Millisecond).String(),
			"ip", clientIP(r),
		)
	})
}

func (a *App) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// http.ErrAbortHandler 是客户端主动断开，不属于程序缺陷。
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				a.log.Error("请求处理 panic", "path", r.URL.Path, "panic", fmt.Sprint(rec))
				// 此时响应头可能已写出，只能尽力而为。
				w.Header().Set("Connection", "close")
				http.Error(w, "500 服务内部错误", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// securityMW 下发一组基础安全响应头。
// CSP 之所以可以收紧到 script-src 'self'，是因为前端没有任何内联脚本。
func (a *App) securityMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		// same-origin 而非 no-referrer：跨站跳转依旧不泄露本站地址，
		// 但同源请求会带上 Referer，sameOrigin 才能拿它做 Origin 缺失时的兜底判据。
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", strings.Join([]string{
			"default-src 'none'",
			"script-src 'self'",
			"style-src 'self'",
			"img-src http: https: data:",
			"connect-src 'self'",
			"form-action 'self'",
			"base-uri 'none'",
			"frame-ancestors 'none'",
		}, "; "))
		next.ServeHTTP(w, r)
	})
}

// authMW 实现可选访问口令。未配置 AVDB_ACCESS_TOKEN 时完全放行。
func (a *App) authMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := a.currentConfig().AccessToken
		if expected == "" {
			next.ServeHTTP(w, r)
			return
		}
		// 健康检查必须免鉴权，否则容器编排无法探活。
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		token := r.URL.Query().Get("token")
		viaQuery := token != ""
		if token == "" {
			token = r.Header.Get("X-Auth-Token")
		}
		if token == "" {
			if c, err := r.Cookie(authCookieName); err == nil {
				token = c.Value
			}
		}

		// 定长比较，避免通过响应时间侧信道爆破口令。
		if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("WWW-Authenticate", "Token")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "401 需要访问口令\n\n请在 URL 后追加 ?token=你的口令，例如：\n  /s?q=关键词&token=你的口令\n\n口令校验通过后会写入 Cookie，后续请求无需重复携带。\n")
			return
		}

		if viaQuery {
			setAuthCookie(w, token)
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- 视图数据

// pageSizeOption 是「每页条数」下拉里的一个选项。
type pageSizeOption struct {
	Value int
	Label string
}

// pageLink 是分页条上的一个页码。
type pageLink struct {
	Number  int
	URL     string
	Current bool
}

// viewData 是首页模板的数据模型。
type viewData struct {
	Keyword  string
	Torrents []Torrent

	Error      string
	Truncated  bool
	Sites      []string
	Sections   []string
	Categories []string

	// NeedSetup 表示尚未配置 API Key，页面首屏会给出引导。
	NeedSetup bool

	BaseURL string
	Version string
	Build   string

	// ---- 分页 ----
	//
	// Paged 为 false 时（每页条数选了"全部"，或结果没超过一页）不渲染分页条。
	// StartedAt 是当前页第一条的**全局序号**，模板用它给每行编号，
	// 这样第二页的第一行是 101 而不是 1。
	PageSize        int
	PageSizeOptions []pageSizeOption
	Page            int
	TotalPages      int
	Total           int
	StartedAt       int
	Paged           bool
	FirstURL        string
	PrevURL         string
	NextURL         string
	LastURL         string
	PageLinks       []pageLink
}

// pageWindow 描述"完整结果集里的哪一段要渲染"。
type pageWindow struct {
	Page       int
	TotalPages int
	Total      int
	StartedAt  int // 1-based 全局序号起点
	Items      []Torrent
}

// slicePage 按每页条数切出当前页。
//
// size <= 0 表示"全部显示"：只有一页，也不会有分页条。
// page 会被夹到 [1, TotalPages]，所以手工改 URL 传 page=9999 只会落到最后一页，
// 不会切出一个空页让人以为"搜索结果没了"。
func slicePage(items []Torrent, page, size int) pageWindow {
	total := len(items)
	if size <= 0 {
		return pageWindow{Page: 1, TotalPages: 1, Total: total, StartedAt: 1, Items: items}
	}
	totalPages := (total + size - 1) / size
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * size
	end := start + size
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	return pageWindow{
		Page:       page,
		TotalPages: totalPages,
		Total:      total,
		StartedAt:  start + 1,
		Items:      items[start:end],
	}
}

// pagerURL 生成分页链接。只带 q 与 page 两个参数：
// 每页条数是已持久化的偏好，不该被翻页链接反复重发。
func pagerURL(keyword string, page int) string {
	q := url.Values{}
	if keyword != "" {
		q.Set("q", keyword)
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	if len(q) == 0 {
		return "/s"
	}
	return "/s?" + q.Encode()
}

// pageLinks 生成分页条上的页码窗口：当前页附近最多 7 个，两端始终可见。
func pageLinks(keyword string, current, totalPages int) []pageLink {
	const window = 7

	first, last := 1, totalPages
	if last > window {
		first = current - window/2
		if first < 1 {
			first = 1
		}
		last = first + window - 1
		if last > totalPages {
			last = totalPages
			first = last - window + 1
			if first < 1 {
				first = 1
			}
		}
	}

	out := make([]pageLink, 0, last-first+1)
	for n := first; n <= last; n++ {
		out = append(out, pageLink{Number: n, URL: pagerURL(keyword, n), Current: n == current})
	}
	return out
}

// applyPageSizeQuery 处理结果页上「每页条数」的选择，并**就地持久化**。
//
// 这是全站唯一一处"GET 顺手改状态"的地方，是刻意的：用户要求这个设置
// "永久生效，直到下次再设置"。若只当成一次性参数，一翻页、一刷新就丢了，
// 等于没设置。
//
// 风险面很小：取值被白名单收敛，改的只是渲染条数，不触发任何上游写操作。
// 代价是别的站点可以让你的浏览器去加载 /s?q=x&page_size=10 把你的偏好改掉——
// 影响仅为显示条数，可接受；真要防，就得改成 POST，那样结果页上每点一次
// 都要一次额外请求。
func (a *App) applyPageSizeQuery(r *http.Request, cfg Config) Config {
	raw := strings.TrimSpace(r.URL.Query().Get("page_size"))
	if raw == "" {
		return cfg
	}
	n, err := strconv.Atoi(raw)
	if err != nil || !IsAllowedPageSize(n) || n == cfg.PageSize {
		return cfg
	}

	cfg.PageSize = n
	if err := cfg.Save(); err != nil {
		// 存不下不影响本次显示，但要留痕，否则用户会以为"设置了没生效"。
		a.log.Warn("保存每页条数失败（本次仍按新值渲染）", "err", err)
		return cfg
	}
	a.applyConfig(cfg)
	a.log.Info("每页条数已更新", "page_size", n, "human", PageSizeLabel(n))
	return cfg
}

// searchTorrents 带短缓存地检索。
//
// 缓存只写成功结果，且 key 里带上上游地址——换了上游还回旧结果是最难查的一类错。
func (a *App) searchTorrents(ctx context.Context, client *AvdbClient, cfg Config, keyword string) ([]Torrent, error) {
	key := cfg.APIBaseURL + "\x00" + keyword
	now := time.Now()
	if items, ok := a.searchCache.get(key, now); ok {
		return items, nil
	}
	items, err := client.SearchTorrents(ctx, keyword)
	if err != nil {
		return nil, err
	}
	a.searchCache.put(key, items, now)
	return items, nil
}

// pageSizeOptions 把白名单转成模板用的选项列表。
func pageSizeOptions(current int) []pageSizeOption {
	sizes := AllowedPageSizes()
	out := make([]pageSizeOption, 0, len(sizes))
	for _, n := range sizes {
		label := PageSizeLabel(n)
		if n > 0 {
			label = strconv.Itoa(n) + " 条"
		}
		out = append(out, pageSizeOption{Value: n, Label: label})
	}
	return out
}

// handleSearch 渲染搜索页。结果在服务端直出，因此即使浏览器禁用 JS、
// 或插件只做"打开 URL"这一动作，页面依然是完整的。
func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	keyword := keywordFrom(r)
	cfg, client := a.snapshot()
	cfg = a.applyPageSizeQuery(r, cfg)

	ctx, cancel := context.WithTimeout(r.Context(), cfg.Timeout)
	defer cancel()

	data := viewData{
		Keyword:  keyword,
		BaseURL:  requestBase(r),
		Version:  a.version,
		Build:    a.build,
		PageSize: cfg.PageSize,
		// 还没配好上游地址/令牌时，在首屏直接引导去设置页面，
		// 而不是让用户对着一条看不懂的报错发呆。
		NeedSetup:       cfg.APIKey == "",
		PageSizeOptions: pageSizeOptions(cfg.PageSize),
	}

	if keyword != "" {
		items, err := a.searchTorrents(ctx, client, cfg, keyword)
		switch {
		case err != nil:
			a.log.Warn("搜索失败", "keyword", keyword, "err", err)
			data.Error = searchErrorMessage(err)
		default:
			// MaxResults 是可选安全阀：默认 0 表示不限制，搜索结果全部保留。
			if cfg.MaxResults > 0 && len(items) > cfg.MaxResults {
				items = items[:cfg.MaxResults]
				data.Truncated = true
			}
			data.Sites, data.Sections, data.Categories = collectFacets(items)

			win := slicePage(items, pageFromQuery(r), cfg.PageSize)
			data.Torrents = win.Items
			data.Page = win.Page
			data.TotalPages = win.TotalPages
			data.Total = win.Total
			data.StartedAt = win.StartedAt
			data.Paged = win.TotalPages > 1
			if data.Paged {
				data.FirstURL = pagerURL(keyword, 1)
				data.LastURL = pagerURL(keyword, win.TotalPages)
				if win.Page > 1 {
					data.PrevURL = pagerURL(keyword, win.Page-1)
				}
				if win.Page < win.TotalPages {
					data.NextURL = pagerURL(keyword, win.Page+1)
				}
				data.PageLinks = pageLinks(keyword, win.Page, win.TotalPages)
			}
		}
	}

	a.render(w, r, data)
}

// pageFromQuery 取页码，非数字或小于 1 都当第 1 页。
func pageFromQuery(r *http.Request) int {
	n := parseIntOr(r.URL.Query().Get("page"), 1)
	if n < 1 {
		return 1
	}
	return n
}

// searchErrorMessage 把底层错误翻译成给用户看的一句话。
func searchErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	var upErr *UpstreamError
	if errors.As(err, &upErr) {
		return upErr.Msg
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "搜索超时，请稍后重试"
	}
	if errors.Is(err, context.Canceled) {
		return "请求已取消"
	}
	return "搜索失败：" + err.Error()
}

// renderPage 先渲染到缓冲区再写出。
// 直接渲染到 ResponseWriter 的话，模板执行到一半出错会写出半截 HTML，
// 同时响应头已经发出去，无法再改成 500。
func (a *App) renderPage(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	var buf bytes.Buffer
	if err := a.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		a.log.Error("模板渲染失败", "template", name, "err", err)
		http.Error(w, "500 页面渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// render 是首页的便捷包装。
func (a *App) render(w http.ResponseWriter, r *http.Request, data viewData) {
	a.renderPage(w, r, http.StatusOK, "index.html", data)
}

// apiResponse 是 /api/search 的 JSON 结构。
//
// Count 是**当前页**条数，Total 才是命中总数；两者刻意分开，
// 因为加了分页之后"count 到底指什么"是调用方最容易搞错的地方。
type apiResponse struct {
	Keyword    string    `json:"keyword"`
	Count      int       `json:"count"`
	Total      int       `json:"total"`
	Page       int       `json:"page"`
	TotalPages int       `json:"total_pages"`
	PageSize   int       `json:"page_size"`
	Torrents   []Torrent `json:"torrents"`
	Sites      []string  `json:"sites"`
	Sections   []string  `json:"sections"`
	Categories []string  `json:"categories"`
	Truncated  bool      `json:"truncated"`
	Error      string    `json:"error,omitempty"`
}

func (a *App) handleAPISearch(w http.ResponseWriter, r *http.Request) {
	keyword := keywordFrom(r)
	if keyword == "" {
		writeJSON(w, http.StatusBadRequest, apiResponse{
			Torrents: []Torrent{},
			Error:    "缺少关键词，请使用 ?q=关键词",
		})
		return
	}

	cfg, client := a.snapshot()
	ctx, cancel := context.WithTimeout(r.Context(), cfg.Timeout)
	defer cancel()

	items, err := a.searchTorrents(ctx, client, cfg, keyword)
	if err != nil {
		a.log.Warn("搜索失败(API)", "keyword", keyword, "err", err)
		writeJSON(w, http.StatusBadGateway, apiResponse{
			Keyword:  keyword,
			Torrents: []Torrent{},
			Error:    searchErrorMessage(err),
		})
		return
	}

	truncated := false
	if cfg.MaxResults > 0 && len(items) > cfg.MaxResults {
		items = items[:cfg.MaxResults]
		truncated = true
	}

	// 筛选项基于**全量**结果，否则翻到第 2 页时下拉框里的站点会凭空少几个。
	sites, sections, categories := collectFacets(items)

	win := slicePage(items, pageFromQuery(r), cfg.PageSize)
	resp := apiResponse{
		Keyword:    keyword,
		Count:      len(win.Items),
		Total:      win.Total,
		Page:       win.Page,
		TotalPages: win.TotalPages,
		PageSize:   cfg.PageSize,
		Torrents:   win.Items,
		Sites:      sites,
		Sections:   sections,
		Categories: categories,
		Truncated:  truncated,
	}
	if resp.Torrents == nil {
		resp.Torrents = []Torrent{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------- 下载

type downloadResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	TID     string `json:"tid,omitempty"`
}

// upstreamResult 覆盖上游可能返回的几种结果外壳。
// 用指针区分"字段缺失"与"字段为零值"，否则 code=0 和缺席无法分辨。
type upstreamResult struct {
	Success *bool           `json:"success"`
	Code    *int            `json:"code"`
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Error   string          `json:"error"`
	Detail  string          `json:"detail"`
	Data    json.RawMessage `json:"data"`
}

func (a *App) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		a.log.Warn("拒绝跨站下载提交",
			"origin", r.Header.Get("Origin"), "referer", r.Header.Get("Referer"))
		writeJSON(w, http.StatusForbidden, downloadResponse{Success: false, Message: originDiagnosis(r)})
		return
	}

	// tid 允许放在 query 或表单体里：前端用 fetch POST 提交，命令行用 curl -d 都顺手。
	tid := strings.TrimSpace(r.URL.Query().Get("tid"))
	if tid == "" {
		tid = strings.TrimSpace(r.PostFormValue("tid"))
	}
	if tid == "" {
		writeJSON(w, http.StatusBadRequest, downloadResponse{Success: false, Message: "缺少参数 tid"})
		return
	}
	if !tidPattern.MatchString(tid) {
		writeJSON(w, http.StatusBadRequest, downloadResponse{Success: false, Message: "参数 tid 格式非法"})
		return
	}

	cfg, client := a.snapshot()

	// 上游把 downloader 与 save_path 都改成了必填，且不接受空的保存目录。
	// 这两个值在提交前必须先解析出来，否则用户只会拿到一句看不懂的英文校验错误。
	downloader, savePath, problem, ok := cfg.resolveDownloadTarget()
	if !ok {
		writeJSON(w, http.StatusBadRequest, downloadResponse{Success: false, Message: problem, TID: tid})
		return
	}

	// 用请求自身的 context，这样用户关掉页面时上游请求会被取消。
	ctx, cancel := context.WithTimeout(r.Context(), cfg.Timeout)
	defer cancel()

	body, status, err := client.SubmitDownload(ctx, tid, downloader, savePath)
	if err != nil {
		var upErr *UpstreamError
		msg := "无法连接上游服务：" + err.Error()
		if errors.As(err, &upErr) {
			msg = upErr.Msg
		}
		a.log.Warn("提交下载失败", "tid", tid, "err", err)
		// HTTP 状态码描述"本服务处理请求的结果"，业务成败由 success 字段表达，
		// 这样前端只需判断 success，不必区分 4xx/5xx。
		writeJSON(w, http.StatusBadGateway, downloadResponse{Success: false, Message: msg, TID: tid})
		return
	}

	result := interpretDownloadResult(status, body)
	result.TID = tid
	if !result.Success {
		result.Message = humanizeDownloadFailure(result.Message, downloader)
		a.log.Warn("上游拒绝下载", "tid", tid, "status", status, "message", result.Message)
	} else {
		a.log.Info("提交下载成功", "tid", tid, "downloader", downloader)
	}
	writeJSON(w, http.StatusOK, result)
}

// ---------------------------------------------------------------- 批量下载

// batchDownloadTimeout 是单次批量请求的**整体**等待上限。
//
// 必须小于 http.Server 的 WriteTimeout（60s），否则响应还没写出去连接就被掐了，
// 用户会看到"网络错误"而完全不知道其实上游已经在收了。
const batchDownloadTimeout = 45 * time.Second

type batchDownloadItem struct {
	TID     string `json:"tid"`
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type batchDownloadResponse struct {
	Success   bool                `json:"success"`
	Message   string              `json:"message"`
	Total     int                 `json:"total"`
	OKCount   int                 `json:"ok"`
	FailCount int                 `json:"failed"`
	Results   []batchDownloadItem `json:"results"`
}

// handleDownloadBatch 一次提交多条资源。
//
// 为什么要有这个接口：结果页支持全选（一页最多 1000 条），逐条发请求会产生
// 上千次往返；放在服务端做，可以控制并发度、复用一个 context，
// 也能把"哪几条失败了、为什么"一次性带回去。
//
// 单条失败**不影响**其余条目：批量场景下最忌讳的就是一条坏数据让整批停摆。
func (a *App) handleDownloadBatch(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		a.log.Warn("拒绝跨站批量下载提交",
			"origin", r.Header.Get("Origin"), "referer", r.Header.Get("Referer"))
		writeJSON(w, http.StatusForbidden, batchDownloadResponse{Message: originDiagnosis(r)})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, batchDownloadResponse{Message: "表单解析失败"})
		return
	}

	raw := firstNonEmpty(r.PostFormValue("tids"), r.URL.Query().Get("tids"))
	tids := parseTIDList(raw)
	if len(tids) == 0 {
		writeJSON(w, http.StatusBadRequest, batchDownloadResponse{Message: "没有可提交的 tid"})
		return
	}
	if len(tids) > batchDownloadLimit {
		writeJSON(w, http.StatusBadRequest, batchDownloadResponse{
			Message: fmt.Sprintf("单批最多 %d 条，本次收到 %d 条，请分批提交", batchDownloadLimit, len(tids)),
			Total:   len(tids),
		})
		return
	}

	cfg, client := a.snapshot()
	downloader, savePath, problem, ok := cfg.resolveDownloadTarget()
	if !ok {
		writeJSON(w, http.StatusBadRequest, batchDownloadResponse{Message: problem, Total: len(tids)})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), batchDownloadTimeout)
	defer cancel()

	results := make([]batchDownloadItem, len(tids))
	okCount := 0

	// 固定大小的 worker 池：并发度可控，且 goroutine 数量与输入规模无关。
	sem := make(chan struct{}, batchDownloadWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i, tid := range tids {
		wg.Add(1)
		go func(i int, tid string) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = batchDownloadItem{TID: tid, Message: "整体等待超时，未提交"}
				return
			}

			item := a.submitOne(ctx, client, tid, downloader, savePath)
			results[i] = item
			if item.Success {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}(i, tid)
	}
	wg.Wait()

	resp := batchDownloadResponse{
		Success:   okCount > 0,
		Total:     len(tids),
		OKCount:   okCount,
		FailCount: len(tids) - okCount,
		Results:   results,
	}
	switch {
	case okCount == len(tids):
		resp.Message = fmt.Sprintf("已提交 %d 条", okCount)
	case okCount == 0:
		resp.Message = fmt.Sprintf("%d 条全部提交失败", len(tids))
	default:
		resp.Message = fmt.Sprintf("提交完成：成功 %d 条，失败 %d 条", okCount, len(tids)-okCount)
	}
	a.log.Info("批量提交下载", "total", len(tids), "ok", okCount, "downloader", downloader)
	writeJSON(w, http.StatusOK, resp)
}

// submitOne 提交单条，并把各种失败都归一成一句人话。
func (a *App) submitOne(ctx context.Context, client *AvdbClient, tid, downloader, savePath string) batchDownloadItem {
	body, status, err := client.SubmitDownload(ctx, tid, downloader, savePath)
	if err != nil {
		var upErr *UpstreamError
		msg := "无法连接上游：" + err.Error()
		if errors.As(err, &upErr) {
			msg = upErr.Msg
		}
		return batchDownloadItem{TID: tid, Message: msg}
	}
	result := interpretDownloadResult(status, body)
	msg := result.Message
	if !result.Success {
		msg = humanizeDownloadFailure(msg, downloader)
	}
	return batchDownloadItem{TID: tid, Success: result.Success, Message: msg}
}

// parseTIDList 解析逗号/空白分隔的 tid 列表：去空白、去重、保序、逐个校验格式。
//
// 非法 tid 直接丢弃而不是整批报错：一个被改坏的 id 不该让另外 99 条白提交。
func parseTIDList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ';'
	})
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || !tidPattern.MatchString(f) {
			continue
		}
		if _, dup := seen[f]; dup {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

// downloaderNotFoundHints 是上游"下载器不存在"报错的特征片段。
// 上游原文形如 `未找到下载器: xxx`，不同版本也可能是英文表述。
var downloaderNotFoundHints = []string{
	"未找到下载器", "下载器不存在", "找不到下载器", "下载器配置",
	"downloader not found", "unknown downloader", "invalid downloader",
}

// savePathRejectedHints 是上游拒绝保存目录时的特征片段。
var savePathRejectedHints = []string{"保存目录不能为空", "保存路径不能为空", "save_path"}

// humanizeDownloadFailure 给上游的下载失败信息补上"下一步该做什么"。
//
// 上游的原始文案多为 `未找到下载器: xxx` 或 `保存目录不能为空`，
// 用户拿到这两句完全无从下手：既不知道去哪改，也不知道该填什么。
// 这里按失败类型补上明确的动作，并保留上游原文，方便排查时对照。
//
// downloader 必须是**本次真正发出去**的那个值，而不是配置里可能为空的原始值——
// 否则用户会看到"请检查你填的 xxx"，而那句 xxx 他从来没填过。
func humanizeDownloadFailure(message, downloader string) string {
	switch {
	case containsAnyFold(message, downloaderNotFoundHints):
		return fmt.Sprintf(
			"上游没有找到下载器 %q。请到「设置」页点「探测可用下载器」，"+
				"把上游真正配置过的标识填进「下载器」；上游没配任何下载器时，"+
				"要先在 Avdb 里配好一个。（上游原文：%s）",
			downloader, message,
		)
	case containsAnyFold(message, savePathRejectedHints):
		return "上游要求保存目录必须是一个具体路径（不接受空值）。" +
			"请到「设置」页填写「保存路径」，可用同页的「校验下载器并列出目录」读取上游已配置的目录。" +
			"（上游原文：" + message + "）"
	default:
		return message
	}
}

// interpretDownloadResult 把五花八门的上游响应统一成 {success, message}。
//
// 上游的返回格式并不统一（success / code / 嵌套 data 三种都见过），
// 前端不应该去猜，所以在这里一次性收敛。
func interpretDownloadResult(status int, body []byte) downloadResponse {
	ok := status >= 200 && status < 300

	var parsed upstreamResult
	if err := json.Unmarshal(body, &parsed); err == nil {
		if result, found := resolveUpstreamResult(parsed, ok); found {
			return result
		}
		// 是合法 JSON，但没有任何我们认识的字段。
		// 不要把原始 JSON 甩到界面上——那对用户毫无信息量。
		if ok {
			return downloadResponse{Success: true, Message: "已提交到下载器"}
		}
		return downloadResponse{Success: false, Message: fmt.Sprintf("上游返回 HTTP %d", status)}
	}

	// 压根不是 JSON（例如上游返回 HTML 错误页）：只能按状态码判断，
	// 并把截断后的原文留作线索，方便定位问题。
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	if text == "" {
		if ok {
			text = "已提交到下载器"
		} else {
			text = fmt.Sprintf("上游返回 HTTP %d", status)
		}
	}
	return downloadResponse{Success: ok, Message: text}
}

// resolveUpstreamResult 按 success → code → 嵌套 data → 仅 message 的顺序
// 尝试解读上游结果。第二个返回值为 false 表示"认不出来"。
func resolveUpstreamResult(r upstreamResult, ok bool) (downloadResponse, bool) {
	if r.Success != nil {
		return downloadResponse{Success: *r.Success, Message: messageFor(r, *r.Success)}, true
	}
	// code 约定：0 表示成功。
	if r.Code != nil {
		return downloadResponse{Success: *r.Code == 0, Message: messageFor(r, *r.Code == 0)}, true
	}
	// 部分接口把结果包在 data 对象里。
	if len(r.Data) > 0 && r.Data[0] == '{' {
		var inner upstreamResult
		if err := json.Unmarshal(r.Data, &inner); err == nil {
			if inner.Success != nil {
				return downloadResponse{Success: *inner.Success, Message: messageFor(inner, *inner.Success)}, true
			}
			if inner.Code != nil {
				return downloadResponse{Success: *inner.Code == 0, Message: messageFor(inner, *inner.Code == 0)}, true
			}
		}
	}
	// 只带了一句提示语：按 HTTP 状态码判断成败。
	if firstNonEmpty(r.Message, r.Msg, r.Error, r.Detail) != "" {
		return downloadResponse{Success: ok, Message: messageFor(r, ok)}, true
	}
	return downloadResponse{}, false
}

// messageFor 依次挑选可用的提示语，全都没有时按成败给默认文案。
func messageFor(r upstreamResult, ok bool) string {
	if msg := firstNonEmpty(r.Message, r.Msg, r.Error, r.Detail); msg != "" {
		return msg
	}
	if ok {
		return "已提交到下载器"
	}
	return "上游未说明失败原因"
}

// firstNonEmpty 返回第一个非空白字符串（已去除首尾空白）。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------- 设置页面

// settingsViewData 是设置页面的数据模型。
type settingsViewData struct {
	BaseURL string
	Version string
	Build   string

	APIBaseURL string
	Downloader string
	SavePath   string

	// DownloaderOptions 是「下载器」下拉的选项（内置候选 + 当前配置值）。
	// 用下拉而不是自由文本：用户无从知道合法取值有哪些，
	// 而自由文本一旦输入就无法"取消选择"，只能删掉重打。
	DownloaderOptions []string

	// PageSize 是每页展示条数，PageSizeOptions 是可选值（含 0 = 全部）。
	PageSize        int
	PageSizeOptions []pageSizeOption

	// SavePathOptions 是上次从上游列出的目录候选，只用于给输入框提供
	// 下拉提示（<datalist>），不改变任何默认行为。
	SavePathOptions []string

	// 密钥类字段只回显掩码，永远不回显明文。
	// Avdb 官方文档明确要求令牌不得进入前端，掩码既能让用户确认"已配置"，
	// 又不会在 HTML、浏览器缓存或代理日志里留下完整凭证。
	APIKeyMasked      string
	AccessTokenMasked string
	HasAPIKey         bool
	HasAccessToken    bool

	ConfigPath     string
	ConfigDir      string
	ConfigWritable bool

	Saved  bool
	Error  string
	Notice string
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	cfg := a.currentConfig()
	data := a.buildSettingsData(cfg)
	data.BaseURL = requestBase(r)
	data.Saved = r.URL.Query().Get("saved") == "1"
	a.renderPage(w, r, http.StatusOK, "settings.html", data)
}

// buildSettingsData 把当前配置投影成页面数据。
func (a *App) buildSettingsData(cfg Config) settingsViewData {
	return settingsViewData{
		Version:    a.version,
		Build:      a.build,
		APIBaseURL: cfg.APIBaseURL,
		Downloader: cfg.Downloader,
		SavePath:   cfg.SavePath,

		DownloaderOptions: downloaderChoices(cfg.Downloader),

		PageSize:        cfg.PageSize,
		PageSizeOptions: pageSizeOptions(cfg.PageSize),

		SavePathOptions: cfg.SavePathOptions,

		APIKeyMasked:      MaskSecret(cfg.APIKey),
		AccessTokenMasked: MaskSecret(cfg.AccessToken),
		HasAPIKey:         cfg.APIKey != "",
		HasAccessToken:    cfg.AccessToken != "",

		ConfigPath:     cfg.ConfigPath(),
		ConfigDir:      cfg.ConfigDir,
		ConfigWritable: a.configWritable(cfg),
	}
}

// configWritable 返回配置目录是否可写，结果缓存 10 秒。
//
// 为什么要缓存：探测本身要在磁盘上创建并删除一个文件，实测在 Windows
// （杀软实时扫描新文件）上单次约 40ms，NAS 网络存储上更慢。而设置页每次
// 渲染都会问一次——任何人反复刷新设置页都会造成无谓的文件抖动。
//
// TTL 取 10 秒而不是永久，是为了让"用户刚把卷挂好"能较快自愈；
// 真写入失败时 Save() 仍会给出明确错误，不会被这个缓存掩盖。
func (a *App) configWritable(cfg Config) bool {
	a.mu.RLock()
	fresh := !a.writableAt.IsZero() && time.Since(a.writableAt) < writableTTL
	cached := a.writableValue
	a.mu.RUnlock()

	if fresh {
		return cached
	}

	value := cfg.ConfigDirWritable()

	a.mu.Lock()
	a.writableValue, a.writableAt = value, time.Now()
	a.mu.Unlock()
	return value
}

// handleSettingsSave 处理设置表单提交。
//
// 采用 POST + 303 重定向（PRG 模式）：浏览器禁用 JS 也能用，
// 且用户刷新结果页不会重复提交。
func (a *App) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	// 纵深防御：SameSite=Lax 已经挡住跨站表单携带 Cookie，
	// 但在"未设置访问口令"的场景下根本没有 Cookie 可挡，必须再校验来源。
	if !sameOrigin(r) {
		a.log.Warn("拒绝跨站设置提交",
			"origin", r.Header.Get("Origin"),
			"referer", r.Header.Get("Referer"),
			"host", r.Host,
			"x_forwarded_host", r.Header.Get("X-Forwarded-Host"),
		)
		http.Error(w, "403 跨站请求被拒绝\n\n"+originDiagnosis(r), http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "400 表单解析失败", http.StatusBadRequest)
		return
	}

	cfg := a.currentConfig()
	next := cfg

	next.APIBaseURL = strings.TrimSpace(r.PostFormValue("api_base_url"))
	next.Downloader = strings.TrimSpace(r.PostFormValue("default_downloader"))
	next.SavePath = strings.TrimSpace(r.PostFormValue("default_save_path"))

	// 密钥字段：留空 = 保持原值；勾选"清除"才清空。
	// 这样既支持"只想改地址不想重填令牌"，也支持主动清除。
	next.APIKey = resolveSecret(r.PostFormValue("api_key"), r.PostFormValue("clear_api_key") != "", cfg.APIKey)
	next.AccessToken = resolveSecret(r.PostFormValue("access_token"), r.PostFormValue("clear_access_token") != "", cfg.AccessToken)

	next.PageSize = settingsPageSize(r, cfg.PageSize)
	next = normalizeConfig(next)

	if err := next.Validate(); err != nil {
		a.renderSettingsError(w, r, next, "上游地址不合法："+err.Error())
		return
	}

	if err := next.Save(); err != nil {
		a.log.Error("保存配置失败", "path", next.ConfigPath(), "err", err)
		a.renderSettingsError(w, r, next, "保存失败："+err.Error())
		return
	}

	a.applyConfig(next)
	a.log.Info("配置已更新并生效",
		"upstream", next.APIBaseURL,
		"key_configured", next.APIKey != "",
		"token_configured", next.AccessToken != "",
		"path", next.ConfigPath(),
	)

	// 若本次设置/更换了访问口令，立刻给当前浏览器下发 Cookie。
	// 否则紧接着的 303 跳转会被我们刚设的口令拦在门外，形成"改完就进不去"。
	if next.AccessToken != "" {
		setAuthCookie(w, next.AccessToken)
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

// settingsPageSize 解析设置表单里的每页条数。
//
// 三条规则，缺一不可：
//   - 字段缺失（老页面、直接调接口）→ 保持原值，不做静默重置；
//   - 0 是合法值（= 全部），所以不能套用"<=0 就取默认值"的写法；
//   - 白名单之外的取值一律忽略，避免 ?page_size=99999 让结果页一次渲染十万行。
func settingsPageSize(r *http.Request, current int) int {
	raw := strings.TrimSpace(r.PostFormValue("page_size"))
	if raw == "" {
		return current
	}
	n, err := strconv.Atoi(raw)
	if err != nil || !IsAllowedPageSize(n) {
		return current
	}
	return n
}

// renderSettingsError 在校验/保存失败时重新渲染表单，
// 并保留用户已经填好的非密钥字段，避免重新输入。
func (a *App) renderSettingsError(w http.ResponseWriter, r *http.Request, cfg Config, message string) {
	data := a.buildSettingsData(cfg)
	data.BaseURL = requestBase(r)
	data.Error = message
	a.renderPage(w, r, http.StatusBadRequest, "settings.html", data)
}

// handleSettingsTest 用**表单里当前填的**地址与令牌做一次连通性探测，
// 让用户在保存之前就能确认配置是否正确。
func (a *App) handleSettingsTest(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		a.log.Warn("拒绝跨站测试请求",
			"origin", r.Header.Get("Origin"),
			"referer", r.Header.Get("Referer"),
			"host", r.Host,
			"x_forwarded_host", r.Header.Get("X-Forwarded-Host"),
		)
		writeJSON(w, http.StatusForbidden, downloadResponse{
			Success: false,
			Message: "跨站请求被拒绝。" + originDiagnosis(r),
		})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, downloadResponse{Success: false, Message: "表单解析失败"})
		return
	}

	saved := a.currentConfig()
	candidate := saved
	candidate.APIBaseURL = strings.TrimSpace(r.PostFormValue("api_base_url"))
	candidate.APIKey = resolveSecret(r.PostFormValue("api_key"), false, saved.APIKey)
	candidate.TimeoutSec = clampInt(parseIntOr(r.PostFormValue("timeout_seconds"), saved.TimeoutSec), 1, 600)
	candidate = normalizeConfig(candidate)

	if err := candidate.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, downloadResponse{Success: false, Message: "地址不合法：" + err.Error()})
		return
	}

	// 探测用较短超时，不然用户要干等整个请求超时。
	probeCfg := candidate
	if probeCfg.Timeout > 15*time.Second {
		probeCfg.Timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeCfg.Timeout)
	defer cancel()

	if err := NewAvdbClient(probeCfg).Ping(ctx); err != nil {
		message := err.Error()
		var upErr *UpstreamError
		if errors.As(err, &upErr) {
			message = upErr.Msg
		}
		writeJSON(w, http.StatusOK, downloadResponse{Success: false, Message: "连接失败：" + message})
		return
	}
	writeJSON(w, http.StatusOK, downloadResponse{Success: true, Message: "连接正常，地址与令牌均可用"})
}

// settingsProbeResponse 是 /api/settings/* 各探测接口的统一响应。
type settingsProbeResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`

	// DefaultRule 相关：直接回传上游的默认下载目标，供页面填进输入框。
	// 这几个字段都不含凭据，可以安全下发。
	Downloader    string `json:"downloader,omitempty"`
	SavePath      string `json:"save_path,omitempty"`
	SavePathLabel string `json:"save_path_label,omitempty"`

	Directories []string `json:"directories,omitempty"`

	// Downloaders 是「探测可用下载器」的逐项结果，Best 是其中第一个已配置的标识。
	Downloaders []DownloaderProbe `json:"downloaders,omitempty"`
	Best        string            `json:"best,omitempty"`
}

// guardSameOriginJSON 收敛探测接口的来源校验。
//
// 这些接口虽然只读，但能读上游设置，因此与"保存设置"享受同等待遇：
// 必须 POST + 同源。
func (a *App) guardSameOriginJSON(w http.ResponseWriter, r *http.Request, action string) bool {
	if sameOrigin(r) {
		return true
	}
	a.log.Warn("拒绝跨站探测请求",
		"action", action,
		"origin", r.Header.Get("Origin"),
		"referer", r.Header.Get("Referer"),
		"host", r.Host,
	)
	writeJSON(w, http.StatusForbidden, settingsProbeResponse{
		Message: "跨站请求被拒绝。" + originDiagnosis(r),
	})
	return false
}

// probeTarget 从表单解析"要探测哪台上游、用哪个 API Key"。
//
// 两个字段都遵循同一个约定：**留空即沿用已保存的值**。
// 早先 api_base_url 是"留空即空地址"，和 api_key 的语义不一致——
// 设置页总会带上字段值，所以页面上看不出问题，但任何直接调这两个接口的
// 调用方都会莫名其妙地收到"上游 API 地址为空"。口径统一后才说得通。
func (a *App) probeTarget(r *http.Request) (Config, error) {
	saved := a.currentConfig()
	candidate := saved
	candidate.APIBaseURL = firstNonEmpty(strings.TrimSpace(r.PostFormValue("api_base_url")), saved.APIBaseURL)
	candidate.APIKey = resolveSecret(r.PostFormValue("api_key"), false, saved.APIKey)
	candidate.TimeoutSec = clampInt(parseIntOr(r.PostFormValue("timeout_seconds"), saved.TimeoutSec), 1, 600)
	candidate = normalizeConfig(candidate)

	if err := candidate.Validate(); err != nil {
		return candidate, err
	}
	// 探测用较短超时：让用户等满一个完整请求周期去发现"地址写错了"体验很差。
	if candidate.Timeout > 15*time.Second {
		candidate.Timeout = 15 * time.Second
	}
	return candidate, nil
}

// describeProbeError 把探测过程中的错误翻成人话。
func describeProbeError(err error) string {
	var upErr *UpstreamError
	if errors.As(err, &upErr) {
		return upErr.Msg
	}
	return err.Error()
}

// handleSettingsDefaultRule 读取上游的全局默认下载目标（默认下载器 + 默认目录）。
//
// 这是"不用在页面上配置下载器"的关键：上游自己知道该用哪个下载器，
// 我们只需要把它读出来给用户看/填，而不是让用户去猜一个标识。
//
// 该接口鉴权为 `API Key/JWT`，**不需要登录**——这也正是本站不做登录功能的原因。
// 读到的值不写盘：它只是给用户看一眼、然后决定要不要固化成自己的配置。
func (a *App) handleSettingsDefaultRule(w http.ResponseWriter, r *http.Request) {
	if !a.guardSameOriginJSON(w, r, "读取上游默认下载器") {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, settingsProbeResponse{Message: "表单解析失败"})
		return
	}

	candidate, err := a.probeTarget(r)
	if err != nil {
		writeJSON(w, http.StatusOK, settingsProbeResponse{Message: "上游地址不合法：" + err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), candidate.Timeout)
	defer cancel()

	rule, err := NewAvdbClient(candidate).FetchDefaultRule(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, settingsProbeResponse{
			Message: "读取上游默认下载目标失败：" + describeProbeError(err),
		})
		return
	}

	// 三种情形要分开说，不能笼统地报"读到了"。
	//
	// 这里曾有一个只会在"上游配了目录、没配下载器"时出现的缺陷：判空用的是
	// IsZero()（三个字段都空才算空），于是这种情况下会回一句
	// 「上游默认下载器：，默认目录：/xxx」并标 success —— 前半句是空的，
	// 用户会以为下载器已经填好了，结果下载依旧失败。而本接口存在的**唯一目的**
	// 就是拿到那个下载器标识，所以必须以 Downloader 是否为空来分岔。
	response := settingsProbeResponse{
		Downloader:    rule.Downloader,
		SavePath:      rule.SavePath,
		SavePathLabel: rule.SavePathLabel,
	}

	switch {
	case rule.Downloader == "":
		// 无论下游目录配没配，没有下载器标识就解决不了用户的问题。
		response.Message = "上游没有配置默认下载器，读不到可用的标识。" +
			"请在 Avdb 里指定一个默认下载器，或手工填写标识后用「校验下载器并列出目录」确认是否有效。"
		if rule.SavePath != "" {
			response.Message += "（但读到了默认目录：" + rule.SavePath + "，可以先用上）"
		}
	default:
		// 提示刻意不复述目录名：它已经填进下面的输入框了。
		// 界面上重复一遍同样的信息只是噪音，用户一眼能看到字段里的值。
		msg := fmt.Sprintf("已填入上游默认下载器「%s」", rule.Downloader)
		if rule.SavePath != "" {
			msg += "与默认目录"
		}
		msg += "，点「保存设置」固化。"
		response.Success = true
		response.Message = msg
	}

	writeJSON(w, http.StatusOK, response)
}

// handleSettingsDirectories 校验下载器标识是否有效，并列出其下的可选目录。
//
// 这一步只用到 API Key（上游对 /config/downloader/directories 标注的鉴权是
// `API Key/JWT`），所以**不需要登录**。它把"填错下载器标识"从
// "提交下载时才炸"提前到了"配置阶段就能发现"。
func (a *App) handleSettingsDirectories(w http.ResponseWriter, r *http.Request) {
	if !a.guardSameOriginJSON(w, r, "目录探测") {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, settingsProbeResponse{Message: "表单解析失败"})
		return
	}

	candidate, err := a.probeTarget(r)
	if err != nil {
		writeJSON(w, http.StatusOK, settingsProbeResponse{Message: "上游地址不合法：" + err.Error()})
		return
	}

	downloaderID := strings.TrimSpace(r.PostFormValue("downloader_id"))
	if downloaderID == "" {
		downloaderID = candidate.Downloader
	}

	ctx, cancel := context.WithTimeout(r.Context(), candidate.Timeout)
	defer cancel()

	dirs, err := NewAvdbClient(candidate).DownloaderDirectories(ctx, downloaderID)
	if err != nil {
		writeJSON(w, http.StatusOK, settingsProbeResponse{
			Message: fmt.Sprintf("下载器 %q 校验失败：%s", downloaderID, describeProbeError(err)),
		})
		return
	}

	message := fmt.Sprintf("下载器 %q 可用，读到 %d 个目录，可在「保存路径」里直接选。", downloaderID, len(dirs))
	if len(dirs) == 0 {
		message = fmt.Sprintf("下载器 %q 请求成功，但上游没有返回目录；保存路径留空即可。", downloaderID)
	}

	next := candidate
	next.SavePathOptions = normalizeStringList(dirs, 100)
	if err := next.Save(); err != nil {
		a.log.Warn("缓存目录清单失败（不影响本次结果）", "err", err)
	} else {
		a.applyConfig(next)
	}

	writeJSON(w, http.StatusOK, settingsProbeResponse{
		Success:     true,
		Message:     message,
		Directories: next.SavePathOptions,
	})
}

// handleSettingsDownloaders 探测上游**真正配置过**的下载器标识。
//
// 动机：上游把 downloader 改成必填之后，"该填什么"成了一个必须回答的问题，
// 而上游没有任何"用访问令牌列出全部下载器"的接口（那类接口只认登录 JWT）。
// 但对每个候选类型调一次目录接口，就能从它的回答里反推出三类结论：
//
//	未找到该下载工具配置 → 类型合法但没配
//	不支持的下载工具     → 类型不存在
//	其余（含 CloudDrive 读目录失败）→ 已配置 ✅
//
// 全程只用访问令牌，不需要账号密码，也不把上游配置原文带回浏览器。
func (a *App) handleSettingsDownloaders(w http.ResponseWriter, r *http.Request) {
	if !a.guardSameOriginJSON(w, r, "探测下载器") {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, settingsProbeResponse{Message: "表单解析失败"})
		return
	}

	candidate, err := a.probeTarget(r)
	if err != nil {
		writeJSON(w, http.StatusOK, settingsProbeResponse{Message: "上游地址不合法：" + err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), candidate.Timeout)
	defer cancel()

	probes, best := NewAvdbClient(candidate).ProbeDownloaders(ctx, supportedDownloaderIDs)

	var configured, known, unsupported, failed []string
	for _, p := range probes {
		switch p.Status {
		case probeStatusConfigured:
			configured = append(configured, p.ID)
		case probeStatusKnown:
			known = append(known, p.ID)
		case probeStatusUnsupported:
			unsupported = append(unsupported, p.ID)
		default:
			failed = append(failed, p.ID)
		}
	}

	response := settingsProbeResponse{Downloaders: probes, Best: best}
	if best != "" {
		response.Success = true
		response.Message = fmt.Sprintf("上游已配置的下载器：%s。可直接填入「下载器」。",
			strings.Join(configured, "、"))
		// 顺手把它的目录缓存下来做下拉提示——这一步不改变任何默认行为。
		for _, p := range probes {
			if p.ID == best && len(p.Directories) > 0 {
				next := candidate
				next.SavePathOptions = normalizeStringList(p.Directories, 100)
				if err := next.Save(); err == nil {
					a.applyConfig(next)
				}
				response.Directories = next.SavePathOptions
			}
		}
	} else {
		response.Message = "没有探测到已配置的下载器。" +
			"请先在 Avdb 的「下载设置」里配置一个下载工具，否则任何下载提交都会被上游拒绝。"
		if len(known) > 0 {
			response.Message += "（以下类型上游认识但未配置：" + strings.Join(known, "、") + "）"
		}
		if len(failed) > 0 {
			response.Message += "（探测失败，结论未知：" + strings.Join(failed, "、") + "）"
		}
		if len(configured) == 0 && len(known) == 0 {
			response.Message += "（上游连这些类型都不认识：" + strings.Join(unsupported, "、") +
				"，说明它的下载器类型枚举变了，请把上面这句反馈给维护者）"
		}
	}
	writeJSON(w, http.StatusOK, response)
}

// resolveSecret 统一处理密钥类字段的"留空即不修改"语义。
func resolveSecret(input string, clear bool, existing string) string {
	if clear {
		return ""
	}
	if v := strings.TrimSpace(input); v != "" {
		return v
	}
	return existing
}

func parseIntOr(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func clampInt(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}

// sameOrigin 校验请求确实来自本站，用于所有会改状态的非幂等接口。
//
// 判定"本站"的候选主机必须与 requestBase() 保持一致 —— 它同样信任
// X-Forwarded-Host。此前两处对"本站"的定义不一致，导致一个自相矛盾的故障：
// 反向代理下页面能正常打开（requestBase 认 X-Forwarded-Host），
// 但保存设置必被 403（sameOrigin 只认 r.Host，而 r.Host 是代理转发时的内部地址）。
//
// 判定规则：
//  1. 取来源主机：优先 Origin；Origin 缺失或为 "null"（隐私上下文、沙箱 iframe）时退回 Referer。
//  2. 与本地候选主机（X-Forwarded-Host、Host）逐一比较，端口按默认值归一化。
//  3. 完全取不到来源信息时放行 —— 那不是浏览器发起的（curl、脚本），交由鉴权把关。
//     浏览器发起的跨站请求必带 Origin，且 SameSite=Lax 已阻止其携带 Cookie。
func sameOrigin(r *http.Request) bool {
	if originCheckDisabled() {
		return true
	}
	source := originHost(r)
	if source == "" {
		return true
	}
	for _, candidate := range localHosts(r) {
		if hostEqual(source, candidate) {
			return true
		}
	}
	return false
}

// originHost 提取请求来源的主机（host[:port]）。
func originHost(r *http.Request) string {
	if origin := firstHeaderValue(r.Header.Get("Origin")); origin != "" && !strings.EqualFold(origin, "null") {
		if host := hostOf(origin); host != "" {
			return host
		}
	}
	// Referer 兜底：仅在 Origin 缺失或为 "null" 时才有意义。
	return hostOf(firstHeaderValue(r.Header.Get("Referer")))
}

// localHosts 返回"本站"的全部候选主机，与 requestBase() 的口径一致。
func localHosts(r *http.Request) []string {
	hosts := make([]string, 0, 2)
	if h := firstHeaderValue(r.Header.Get("X-Forwarded-Host")); h != "" {
		hosts = append(hosts, h)
	}
	if r.Host != "" {
		hosts = append(hosts, r.Host)
	}
	return hosts
}

// hostOf 从 URL 文本中解析出主机部分，解析失败返回空串。
func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Host
}

// hostEqual 比较两个 host[:port] 是否等价。
//
// 忽略 scheme（TLS 常常终止在反向代理上，浏览器看到 https、后端只看到 http），
// 并把 80/443 视同缺省端口，避免 "example.com:80" 与 "example.com" 被判为不同主机。
func hostEqual(a, b string) bool {
	hostA, portA := splitHostPort(a)
	hostB, portB := splitHostPort(b)
	if hostA == "" || hostB == "" {
		return false
	}
	return strings.EqualFold(hostA, hostB) && portA == portB
}

// splitHostPort 拆分主机与端口，默认端口（80/443）归一化为空串。
func splitHostPort(s string) (host, port string) {
	s = strings.TrimSuffix(strings.TrimSpace(s), ".")
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		// 不含端口；IPv6 因缺少方括号也会落到这里，统一去掉括号便于比较。
		return strings.Trim(s, "[]"), ""
	}
	switch port {
	case "80", "443":
		port = ""
	}
	return host, port
}

// originCheckDisabled 是给极端部署环境留的逃生舱：默认开启来源校验。
// 仅当显式设置 AVDB_DISABLE_ORIGIN_CHECK=1 时关闭。
func originCheckDisabled() bool {
	v := strings.TrimSpace(os.Getenv("AVDB_DISABLE_ORIGIN_CHECK"))
	return v == "1" || strings.EqualFold(v, "true")
}

// originDiagnosis 在来源校验失败时输出可自助排查的现场信息。
//
// 只回一句"跨站请求被拒绝"会让部署在反向代理后的用户无从下手，
// 把代理配置疏漏误判成程序缺陷 —— 这正是本次故障的教训。
func originDiagnosis(r *http.Request) string {
	var b strings.Builder
	b.WriteString("来源校验未通过。实际收到的请求头如下，请对照排查：\n\n")
	fmt.Fprintf(&b, "  Origin           : %q\n", r.Header.Get("Origin"))
	fmt.Fprintf(&b, "  Referer          : %q\n", r.Header.Get("Referer"))
	fmt.Fprintf(&b, "  Host             : %q\n", r.Host)
	fmt.Fprintf(&b, "  X-Forwarded-Host : %q\n", r.Header.Get("X-Forwarded-Host"))
	b.WriteString("\n若经由反向代理访问，请让代理回传原始主机，例如 Nginx：\n\n")
	b.WriteString("  proxy_set_header Host $host;\n")
	b.WriteString("  proxy_set_header X-Forwarded-Host $host;\n")
	b.WriteString("\n确属同源却被拦截时，可设环境变量 AVDB_DISABLE_ORIGIN_CHECK=1 关闭该校验。\n")
	return b.String()
}

// setAuthCookie 下发访问口令 Cookie，供后续请求复用。
func setAuthCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
	})
}

// ---------------------------------------------------------------- 其他端点

func (a *App) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"version":    a.version,
		"build":      a.build,
		"uptime_sec": int(time.Since(a.startedAt).Seconds()),
	})
}

func (a *App) handleFavicon(w http.ResponseWriter, r *http.Request) {
	if len(a.favicon) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(a.favicon)
}

// handleOpenSearch 输出 OpenSearch Description，
// 让浏览器地址栏出现"添加此搜索引擎"的提示，一键接入。
func (a *App) handleOpenSearch(w http.ResponseWriter, r *http.Request) {
	base := requestBase(r)
	tmplURL := a.searchTemplate(base, "{searchTerms}", a.currentConfig())

	out := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
  <ShortName>%s</ShortName>
  <Description>%s</Description>
  <InputEncoding>UTF-8</InputEncoding>
  <Image width="16" height="16" type="image/svg+xml">%s</Image>
  <Url type="text/html" method="get" template="%s"/>
</OpenSearchDescription>
`,
		xmlEscape(siteName), xmlEscape(siteName+"：资源检索"), xmlEscape(base+"/favicon.svg"), xmlEscape(tmplURL),
	)

	w.Header().Set("Content-Type", "application/opensearchdescription+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, out)
}

func (a *App) staticHandler() http.Handler {
	// a.static 的根目录**就是** web/static，所以必须先剥掉 /static/ 前缀，
	// 否则 FileServerFS 会去找一个名叫 "static/app.css" 的文件，直接 404。
	fileServer := http.StripPrefix("/static/", http.FileServerFS(a.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 静态资源随二进制走版本号，可以放心长缓存。
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fileServer.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- 辅助

// keywordFrom 从 query 或路径中提取关键词，兼容多种参数名。
//
// 浏览器插件/自定义搜索引擎写模板的方式五花八门（%s、{q}、{searchTerms}），
// 拼出来的参数名可能是 q、keyword、kw、wd，这里全部接住。
// 取到之后统一归一化，保证中文、英文、数字、全角写法都能正常检索。
func keywordFrom(r *http.Request) string {
	if v := normalizeKeyword(r.PathValue("q")); v != "" {
		return v
	}
	q := r.URL.Query()
	for _, key := range []string{"q", "keyword", "kw", "wd", "query", "s"} {
		if v := normalizeKeyword(q.Get(key)); v != "" {
			return v
		}
	}
	return ""
}

// searchTemplate 生成带占位符的搜索 URL，并按需附加访问口令。
func (a *App) searchTemplate(base, placeholder string, cfg Config) string {
	u := base + "/s?q=" + placeholder
	if cfg.AccessToken != "" {
		u += "&token=" + url.QueryEscape(cfg.AccessToken)
	}
	return u
}

// requestBase 推断对外可访问的站点根地址，供 OpenSearch 与页面提示使用。
// 优先信任反向代理传来的 X-Forwarded-*，其次看 TLS。
func requestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := firstHeaderValue(r.Header.Get("X-Forwarded-Proto")); v != "" {
		v = strings.ToLower(v)
		if v == "http" || v == "https" {
			scheme = v
		}
	}

	host := firstHeaderValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	host = sanitizeHost(host)
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host
}

// firstHeaderValue 取逗号分隔头的第一个值。
func firstHeaderValue(v string) string {
	if v == "" {
		return ""
	}
	return strings.TrimSpace(strings.Split(v, ",")[0])
}

// sanitizeHost 只保留主机名允许的字符，避免请求头把奇怪内容带进 XML/HTML。
func sanitizeHost(host string) string {
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == ':', r == '[', r == ']', r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// xmlEscape 转义 XML 文本与属性值。
func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		// 响应头已发出，只能记录，无法再改状态码。
		slog.Default().Warn("写出 JSON 失败", "err", err)
	}
}

// clientIP 提取客户端地址用于日志。只取 IP，不取端口。
func clientIP(r *http.Request) string {
	if v := firstHeaderValue(r.Header.Get("X-Forwarded-For")); v != "" {
		return v
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}
