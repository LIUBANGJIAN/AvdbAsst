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

	// 首页最多渲染这么多条，避免超大结果集把 HTML 撑到几十 MB。
	// 与 MaxResults 的区别：MaxResults 是"最多接受上游多少条"，
	// 这里是"最多往页面里塞多少条"。
	maxRenderRows = 1000
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
	log       *slog.Logger

	// 配置目录可写性探测结果的缓存，由 mu 保护。
	writableValue bool
	writableAt    time.Time
}

// writableTTL 是配置目录可写性探测结果的缓存时长。
const writableTTL = 10 * time.Second

// NewApp 构造应用。所有依赖在这里显式注入，不使用全局变量。
//
// 这里只补一条构造期不变量：**下载器不能为空**。
// 生产路径（ResolveConfig → NewApp）本来就归一化过，但测试与将来的其它入口
// 可能直接构造 Config；一旦漏掉，就会变成"下载器为空 → 上游回一句看不懂的
// 『未找到下载器』"。
//
// 刻意只做这一条、而不是整体跑 normalizeConfig：后者会按 TimeoutSec 重算
// Timeout，把"直接指定亚秒级超时"的能力一并抹掉（测试正依赖这一点）。
func NewApp(cfg Config, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.Default()
	}
	cfg.Downloader = withDownloaderDefault(cfg.Downloader)
	app := &App{
		cfg:       cfg,
		client:    NewAvdbClient(cfg),
		tpl:       parseTemplates(),
		static:    staticRoot(),
		startedAt: time.Now(),
		version:   version,
		log:       logger,
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

	// 设置页面：GET 渲染表单，POST 保存（PRG 模式，避免刷新重复提交）。
	// 用 POST 而不是 PUT/DELETE，是为了在浏览器禁用 JS 时依然能提交表单。
	mux.HandleFunc("GET /settings", a.handleSettings)
	mux.HandleFunc("POST /settings", a.handleSettingsSave)
	mux.HandleFunc("POST /api/settings/test", a.handleSettingsTest)

	// 上游凭据探测：只读操作，但会携带用户刚输入的密码，因此同样强制 POST + 同源。
	//   - login：邮箱/用户名 + 密码换 JWT，随后探测可用下载器清单；
	//   - directories：用 API Key 校验下载器标识并列出可选保存目录。
	mux.HandleFunc("POST /api/settings/login", a.handleSettingsLogin)
	mux.HandleFunc("POST /api/settings/directories", a.handleSettingsDirectories)

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
}

// handleSearch 渲染搜索页。结果在服务端直出，因此即使浏览器禁用 JS、
// 或插件只做"打开 URL"这一动作，页面依然是完整的。
func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	keyword := keywordFrom(r)
	cfg, client := a.snapshot()

	ctx, cancel := context.WithTimeout(r.Context(), cfg.Timeout)
	defer cancel()

	base := requestBase(r)
	data := viewData{
		Keyword: keyword,
		BaseURL: base,
		Version: a.version,
		// 还没配好上游地址/令牌时，在首屏直接引导去设置页面，
		// 而不是让用户对着一条看不懂的报错发呆。
		NeedSetup: cfg.APIKey == "",
	}

	if keyword != "" {
		items, err := client.SearchTorrents(ctx, keyword)
		switch {
		case err != nil:
			a.log.Warn("搜索失败", "keyword", keyword, "err", err)
			data.Error = searchErrorMessage(err)
		default:
			if len(items) > cfg.MaxResults {
				items = items[:cfg.MaxResults]
				data.Truncated = true
			}
			if len(items) > maxRenderRows {
				items = items[:maxRenderRows]
				data.Truncated = true
			}
			data.Torrents = items
			data.Sites, data.Sections, data.Categories = collectFacets(items)
		}
	}

	a.render(w, r, data)
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
type apiResponse struct {
	Keyword    string    `json:"keyword"`
	Count      int       `json:"count"`
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

	items, err := client.SearchTorrents(ctx, keyword)
	if err != nil {
		a.log.Warn("搜索失败(API)", "keyword", keyword, "err", err)
		writeJSON(w, http.StatusBadGateway, apiResponse{
			Keyword:  keyword,
			Torrents: []Torrent{},
			Error:    searchErrorMessage(err),
		})
		return
	}

	resp := apiResponse{Keyword: keyword, Torrents: items}
	if len(items) > cfg.MaxResults {
		resp.Torrents = items[:cfg.MaxResults]
		resp.Truncated = true
	}
	resp.Count = len(resp.Torrents)
	resp.Sites, resp.Sections, resp.Categories = collectFacets(resp.Torrents)
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
	// 用请求自身的 context，这样用户关掉页面时上游请求会被取消。
	ctx, cancel := context.WithTimeout(r.Context(), cfg.Timeout)
	defer cancel()

	body, status, err := client.SubmitDownload(ctx, tid, cfg.Downloader, cfg.SavePath)
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
		result.Message = humanizeDownloadFailure(result.Message, cfg)
		a.log.Warn("上游拒绝下载", "tid", tid, "status", status, "message", result.Message)
	} else {
		a.log.Info("提交下载成功", "tid", tid)
	}
	writeJSON(w, http.StatusOK, result)
}

// downloaderNotFoundHints 是上游"下载器不存在"报错的特征片段。
// 上游原文形如 `未找到下载器: xxx`，不同版本也可能是英文表述。
var downloaderNotFoundHints = []string{
	"未找到下载器", "下载器不存在", "找不到下载器",
	"downloader not found", "unknown downloader", "invalid downloader",
}

// humanizeDownloadFailure 给上游的下载失败信息补上"下一步该做什么"。
//
// 上游对下载器标识不存在只回一句 `未找到下载器: xxx`，用户拿到这句完全无从下手：
// 既不知道去哪改，也不知道该填什么。这里补上入口与当前配置值，
// 并保留上游原文，方便排查时对照。
func humanizeDownloadFailure(message string, cfg Config) string {
	lower := strings.ToLower(message)
	for _, hint := range downloaderNotFoundHints {
		if strings.Contains(lower, strings.ToLower(hint)) {
			return fmt.Sprintf(
				"上游没有找到下载器 %q。请到「设置」页确认「下载器」标识，或用「登录上游」自动读出可用清单。（上游原文：%s）",
				cfg.Downloader, message,
			)
		}
	}
	return message
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

	APIBaseURL string
	Downloader string
	SavePath   string
	TimeoutSec int
	MaxResults int

	// DownloaderOptions / SavePathOptions 是上次从上游探测到的候选，
	// 只用于给输入框提供下拉提示（<datalist>），不改变任何默认行为。
	DownloaderOptions []DownloaderOption
	SavePathOptions   []string

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
		APIBaseURL: cfg.APIBaseURL,
		Downloader: cfg.Downloader,
		SavePath:   cfg.SavePath,
		TimeoutSec: cfg.TimeoutSec,
		MaxResults: cfg.MaxResults,

		DownloaderOptions: cfg.DownloaderOptions,
		SavePathOptions:   cfg.SavePathOptions,

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

	next.TimeoutSec = clampInt(parseIntOr(r.PostFormValue("timeout_seconds"), cfg.TimeoutSec), 1, 600)
	next.MaxResults = clampInt(parseIntOr(r.PostFormValue("max_results"), cfg.MaxResults), 1, 5000)
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

// settingsProbeResponse 是 /api/settings/* 两个探测接口的统一响应。
type settingsProbeResponse struct {
	Success     bool               `json:"success"`
	Message     string             `json:"message"`
	Need2FA     bool               `json:"need_2fa,omitempty"`
	OTPToken    string             `json:"otp_token,omitempty"`
	Downloaders []DownloaderOption `json:"downloaders,omitempty"`
	Directories []string           `json:"directories,omitempty"`
	Trace       []string           `json:"trace,omitempty"`
}

// guardSameOriginJSON 收敛探测接口的来源校验。
//
// 这两个接口虽然是只读的，但会携带用户刚输入的密码，且能读上游设置，
// 因此与"保存设置"享受同等待遇：必须 POST + 同源。
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

// handleSettingsLogin 用用户名 + 密码换 JWT，并顺带读出上游的下载器清单。
//
// 凭据处理红线（本项目最敏感的一段代码）：
//   - 密码只在本次请求内使用，**绝不**写入配置文件、绝不写日志、绝不回显；
//   - JWT 同样只活在本次请求里，用完即弃，不落盘；
//   - 只有"下载器标识 + 显示名"这类非敏感结果会被缓存下来做下拉提示。
//
// 上游配置对象本身很可能含网盘 Cookie / 网盘账号，因此**绝不整段回传前端**。
func (a *App) handleSettingsLogin(w http.ResponseWriter, r *http.Request) {
	if !a.guardSameOriginJSON(w, r, "登录探测") {
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

	client := NewAvdbClient(candidate)

	// 已拿到 otp_token 说明这是两步验证的第二步。
	if otpToken := strings.TrimSpace(r.PostFormValue("otp_token")); otpToken != "" {
		jwt, err := client.Login2FA(ctx, otpToken, r.PostFormValue("otp_code"))
		if err != nil {
			writeJSON(w, http.StatusOK, settingsProbeResponse{Message: describeProbeError(err)})
			return
		}
		a.respondWithDownloaders(w, r, candidate, client, jwt)
		return
	}

	result, err := client.Login(ctx, r.PostFormValue("username"), r.PostFormValue("password"))
	if err != nil {
		writeJSON(w, http.StatusOK, settingsProbeResponse{Message: describeProbeError(err)})
		return
	}
	if result.Needs2FA() {
		writeJSON(w, http.StatusOK, settingsProbeResponse{
			Need2FA:  true,
			OTPToken: result.OTPToken,
			Message:  "该账号开启了两步验证，请填写验证器上的动态码后继续。",
		})
		return
	}
	a.respondWithDownloaders(w, r, candidate, client, result.JWT)
}

// respondWithDownloaders 用 JWT 探测下载器清单，命中后缓存进配置以便下次直接给下拉提示。
func (a *App) respondWithDownloaders(w http.ResponseWriter, r *http.Request, candidate Config, client *AvdbClient, jwt string) {
	ctx, cancel := context.WithTimeout(r.Context(), candidate.Timeout)
	defer cancel()

	opts, trace, err := client.ProbeDownloaders(ctx, jwt)
	if err != nil {
		writeJSON(w, http.StatusOK, settingsProbeResponse{Message: describeProbeError(err), Trace: trace})
		return
	}
	if len(opts) == 0 {
		writeJSON(w, http.StatusOK, settingsProbeResponse{
			Message: "登录成功，但上游配置里没有读出下载器清单。可手工填写标识，再用「校验并列出目录」确认是否有效。",
			Trace:   trace,
		})
		return
	}

	next := candidate
	next.DownloaderOptions = normalizeDownloaderOptions(opts)
	// 缓存失败不影响本次结果，只是下次打开设置页看不到下拉提示。
	if err := next.Save(); err != nil {
		a.log.Warn("缓存下载器清单失败（不影响本次结果）", "err", err)
	} else {
		a.applyConfig(next)
	}

	writeJSON(w, http.StatusOK, settingsProbeResponse{
		Success:     true,
		Message:     fmt.Sprintf("登录成功，从上游读到 %d 个下载器。选中一个保存即可。", len(next.DownloaderOptions)),
		Downloaders: next.DownloaderOptions,
		Trace:       trace,
	})
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
		xmlEscape("Avdb"), xmlEscape("Avdb 资源搜索"), xmlEscape(base+"/favicon.svg"), xmlEscape(tmplURL),
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
