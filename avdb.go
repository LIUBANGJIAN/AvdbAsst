package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// maxUpstreamBody 限制单次读取上游响应的字节数，避免异常上游把进程内存打满。
const maxUpstreamBody = 8 << 20 // 8 MiB

// maxRequestAttempts 是幂等 GET 的最大尝试次数（首次 + 1 次重试）。
const maxRequestAttempts = 2

// Torrent 是上游 /api/v1/articles/torrents 返回的单条资源。
//
// 字段命名与上游 JSON 保持一致。注意两个坑：
//  1. id 实测出现过 10080460000，超过 int32，必须用 int64；
//  2. category 实测会返回 null，用 string 接收时 Go 会安全跳过（保持空串），
//     这里额外用空值归一保证前端拿到的一直是字符串。
type Torrent struct {
	ID           int64   `json:"id"`
	Number       string  `json:"number"`
	Site         string  `json:"site"`
	Website      string  `json:"website"`
	Section      string  `json:"section"`
	Category     string  `json:"category"`
	SizeMB       float64 `json:"size_mb"`
	SizeBytes    int64   `json:"size_bytes"`
	PostTime     string  `json:"post_time"`
	Seeders      int     `json:"seeders"`
	Title        string  `json:"title"`
	DownloadURL  string  `json:"download_url"`
	PreviewImage string  `json:"preview_image"`
	Free         bool    `json:"free"`
	Chinese      bool    `json:"chinese"`
	Uncensored   bool    `json:"uncensored"`
	UC           bool    `json:"uc"`
	HD           bool    `json:"hd"`
	UHD          bool    `json:"uhd"`
	Mosaic       bool    `json:"mosaic"`
	Emby         int     `json:"emby"`

	// SortTS 由 PostTime 解析而来，仅供前端排序，不参与 JSON 编解码。
	SortTS int64 `json:"-"`
}

// apiEnvelope 是上游统一响应外壳。
type apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// UpstreamError 表示上游返回的、可以直接展示给用户的错误。
type UpstreamError struct {
	Status int
	Msg    string
}

func (e *UpstreamError) Error() string { return e.Msg }

// AvdbClient 是上游 Avdb API 的最小客户端。
type AvdbClient struct {
	base string
	key  string
	http *http.Client
}

// NewAvdbClient 构造客户端。超时同时作用于传输层与整体请求，双重保险。
func NewAvdbClient(cfg Config) *AvdbClient {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeoutSec * time.Second
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,
	}
	return &AvdbClient{
		base: strings.TrimRight(cfg.APIBaseURL, "/"),
		key:  cfg.APIKey,
		http: &http.Client{Transport: transport, Timeout: timeout},
	}
}

// SearchTorrents 按关键词检索资源，并返回归一化后的结果。
func (c *AvdbClient) SearchTorrents(ctx context.Context, keyword string) ([]Torrent, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, nil
	}

	body, err := c.getWithRetry(ctx, "/api/v1/articles/torrents", url.Values{"keyword": {keyword}})
	if err != nil {
		return nil, err
	}

	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("上游返回的不是合法 JSON：%w", err)
	}
	if env.Code != 0 {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = fmt.Sprintf("上游返回业务错误码 %d", env.Code)
		}
		return nil, &UpstreamError{Status: http.StatusOK, Msg: msg}
	}

	var list []Torrent
	if len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, &list); err != nil {
			return nil, fmt.Errorf("上游 data 字段结构异常：%w", err)
		}
	}
	if list == nil {
		list = []Torrent{}
	}
	for i := range list {
		normalizeTorrent(&list[i])
	}
	return list, nil
}

// Ping 用一次最小代价的检索验证"地址 + 令牌"确实可用，
// 供设置页面的"测试连接"按钮调用。
//
// 刻意选用一个正常但几乎不可能命中的关键词：既能走通鉴权、路由与数据库查询，
// 又不会拉回一堆数据。比打 /stats 之类的接口更稳——那些接口未必存在于所有版本。
func (c *AvdbClient) Ping(ctx context.Context) error {
	_, err := c.SearchTorrents(ctx, "avdbasst-connectivity-probe")
	return err
}

// SubmitDownload 把某条资源交给上游的下载器。
// 返回值：(上游原始响应体, 上游 HTTP 状态码, error)。
//
// 关于参数：上游把这个接口的 downloader 与 save_path 声明成了**必填** query 参数
// （官方文档标注"选填"，但实测缺失即返回
// 422 {"detail":[{"type":"missing","loc":["query","save_path"]...}]}）。
// 两个参数的空值语义是"继承服务端全局设置"，因此这里**始终发送**：
// 既满足上游的必填校验，又让"不填"成为一个合法且最不容易出错的选项。
func (c *AvdbClient) SubmitDownload(ctx context.Context, tid, downloader, savePath string) ([]byte, int, error) {
	q := url.Values{
		"tid":        {tid},
		"downloader": {strings.TrimSpace(downloader)},
		"save_path":  {strings.TrimSpace(savePath)},
	}
	return c.request(ctx, http.MethodGet, "/api/v1/articles/download/manul", q)
}

// getWithRetry 对幂等 GET 做一次重试，吸收偶发的连接抖动。
func (c *AvdbClient) getWithRetry(ctx context.Context, path string, q url.Values) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < maxRequestAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(300 * time.Millisecond):
			}
		}
		body, status, err := c.request(ctx, http.MethodGet, path, q)
		if err == nil {
			_ = status
			return body, nil
		}
		lastErr = err

		// 只有明确属于瞬态的错误才重试；参数错误、鉴权失败重试毫无意义。
		var upErr *UpstreamError
		if errors.As(err, &upErr) && !isRetryableStatus(upErr.Status) {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

// request 执行一次上游请求，返回响应体与状态码。
func (c *AvdbClient) request(ctx context.Context, method, path string, q url.Values) ([]byte, int, error) {
	endpoint := c.base + path
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("构造上游请求失败：%w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "AvdbAsst/"+version)
	if c.key != "" {
		req.Header.Set("X-API-Key", c.key)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, &UpstreamError{Status: http.StatusGatewayTimeout, Msg: "请求上游超时，请稍后重试"}
		}
		return nil, 0, fmt.Errorf("无法连接上游服务：%w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	// 多读 1 字节用于判定是否超限。
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody+1))
	if readErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取上游响应失败：%w", readErr)
	}
	if len(body) > maxUpstreamBody {
		return nil, resp.StatusCode, fmt.Errorf("上游响应超过 %d MiB，已拒绝", maxUpstreamBody>>20)
	}

	if resp.StatusCode == http.StatusOK {
		return body, resp.StatusCode, nil
	}
	return body, resp.StatusCode, &UpstreamError{Status: resp.StatusCode, Msg: describeStatus(resp.StatusCode, body)}
}

// describeStatus 把上游状态码翻译成用户看得懂的话，并附带少量原文便于排查。
func describeStatus(status int, body []byte) string {
	// 状态码含义依据 Avdb 官方 API 文档：
	// 401 鉴权失败、403 禁止操作、404 资源不存在、422 参数校验失败、503 上游不可用。
	var hint string
	switch status {
	case http.StatusUnauthorized:
		hint = "上游鉴权失败，请检查 API Key 是否正确"
	case http.StatusForbidden:
		hint = "上游拒绝该操作（403），当前令牌可能无相应权限"
	case http.StatusNotFound:
		hint = "上游接口不存在，请确认 Avdb 版本是否匹配"
	case http.StatusUnprocessableEntity:
		hint = "上游参数校验失败（422）"
	case http.StatusTooManyRequests:
		hint = "上游限流，请稍后重试"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		hint = "上游服务暂时不可用"
	default:
		hint = "上游返回异常状态"
	}

	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 200 {
		snippet = snippet[:200] + "…"
	}
	snippet = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, snippet)

	// 422 换个说法：FastAPI 返回的是结构化校验报告，直接甩原文等于没说。
	if status == http.StatusUnprocessableEntity {
		if detail := fastAPIValidationMessage(body); detail != "" {
			return fmt.Sprintf("%s：%s", hint, detail)
		}
	}

	if snippet == "" {
		return fmt.Sprintf("%s（HTTP %d）", hint, status)
	}
	return fmt.Sprintf("%s（HTTP %d）：%s", hint, status, snippet)
}

// fastAPIValidationMessage 把 FastAPI 的 422 校验报告翻译成一句人话。
//
// 上游参数校验失败时返回的是结构化 JSON，例如：
//
//	{"detail":[{"type":"missing","loc":["query","save_path"],"msg":"Field required"}]}
//
// 这种内容贴给用户毫无意义。这里翻译成"查询参数 → save_path：Field required"，
// 让人一眼看出是少传了哪个参数。解析不出来就返回空串，由调用方走原文兜底。
func fastAPIValidationMessage(body []byte) string {
	const maxItems = 3 // 报错再多也只列前几条，避免糊满界面

	var payload struct {
		Detail []struct {
			Loc []json.RawMessage `json:"loc"`
			Msg string            `json:"msg"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Detail) == 0 {
		return ""
	}

	parts := make([]string, 0, maxItems)
	for _, item := range payload.Detail {
		if len(parts) >= maxItems {
			parts = append(parts, "…")
			break
		}
		where := describeLoc(item.Loc)
		msg := strings.TrimSpace(item.Msg)
		switch {
		case where != "" && msg != "":
			parts = append(parts, where+"："+msg)
		case where != "":
			parts = append(parts, where)
		case msg != "":
			parts = append(parts, msg)
		}
	}
	return strings.Join(parts, "；")
}

// describeLoc 把 FastAPI 的 loc 数组翻成中文链路。
// loc 可能形如 ["query","save_path"]，也可能是 ["body",0,"magnet"] 这种带下标的混合形式。
func describeLoc(loc []json.RawMessage) string {
	labels := map[string]string{
		"query":  "查询参数",
		"body":   "请求体",
		"path":   "路径参数",
		"header": "请求头",
	}
	parts := make([]string, 0, len(loc))
	for _, raw := range loc {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if label, ok := labels[s]; ok {
				parts = append(parts, label)
			} else {
				parts = append(parts, s)
			}
			continue
		}
		var n int
		if err := json.Unmarshal(raw, &n); err == nil {
			parts = append(parts, fmt.Sprintf("[%d]", n))
		}
	}
	return strings.Join(parts, " → ")
}

// isRetryableStatus 判断状态码是否属于值得重试的瞬态故障。
func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		http.StatusInternalServerError:
		return true
	default:
		return false
	}
}

// normalizeTorrent 就地清洗单条记录：剥离站点名中的表情、规整时间、
// 校验 URL 协议，保证输出到前端的每个字段都是"可信且可直接渲染"的。
func normalizeTorrent(t *Torrent) {
	t.Title = sanitizeText(t.Title)
	t.Section = strings.TrimSpace(t.Section)
	t.Category = strings.TrimSpace(t.Category)
	t.Number = sanitizeText(t.Number)
	t.Website = strings.TrimSpace(t.Website)
	t.Site = extractSiteName(t.Site, t.Section)

	// 上游偶尔只给 size_bytes，补算一次，避免前端显示 0.00 MB。
	if t.SizeMB <= 0 && t.SizeBytes > 0 {
		t.SizeMB = float64(t.SizeBytes) / (1024 * 1024)
	}
	if t.SizeMB < 0 {
		t.SizeMB = 0
	}
	if t.Seeders < 0 {
		t.Seeders = 0
	}

	t.SortTS = parseTimeToUnix(t.PostTime)
	if t.PostTime == "" {
		t.PostTime = ""
	}

	// 只放行白名单协议：避免上游被污染后把 javascript: 之类的串带进前端。
	t.DownloadURL = safeURL(t.DownloadURL, "magnet", "http", "https", "ed2k", "thunder", "ftp")
	t.PreviewImage = safeURL(t.PreviewImage, "http", "https")
}

// safeURL 仅当 URL 使用允许的协议时才返回原值，否则返回空串。
// 无协议的相对路径一律拒绝，因为上游给的都是绝对地址。
func safeURL(raw string, allowedSchemes ...string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// 去掉可能存在的控制字符再做判断。
	raw = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, raw)
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	for _, s := range allowedSchemes {
		if strings.HasPrefix(lower, s+":") {
			return raw
		}
	}
	return ""
}

// sanitizeText 净化上游文本。
//
// 关键细节：控制字符要替换成空格而不是直接删除。
// 若直接删除，标题里的 "e\x00f" 会被粘成 "ef"，凭空造出一个不存在的词，
// 破坏后续的关键词匹配。
// 零宽字符则相反——它们本身不可见，删掉才不会让 "磁力\u200b链接" 匹配失败。
func sanitizeText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return ' '
		case r == '\u200b' || r == '\ufeff' || r == '\u200e' || r == '\u200f':
			return -1
		default:
			return r
		}
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// isEmoji 判断某个码位是否属于表情符号区块。
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1F9FF,
		r >= 0x2600 && r <= 0x26FF,
		r >= 0x2700 && r <= 0x27BF,
		r >= 0x1F1E0 && r <= 0x1F1FF,
		r >= 0xFE00 && r <= 0xFE0F,
		r >= 0x2B00 && r <= 0x2BFF:
		return true
	default:
		return false
	}
}

// cleanSiteName 去掉站点名里的表情与格式字符。
//
// 与旧实现的区别：这里只剔除表情区块和零宽字符，不再用 unicode.IsSymbol
// 一刀切，否则 "+"、"~" 这类合法的站点名字符会被误删。
func cleanSiteName(site string) string {
	if site == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(site))
	for _, r := range site {
		if isEmoji(r) {
			continue
		}
		if r == '\u200b' || r == '\ufeff' || r == '\u200e' || r == '\u200f' {
			continue
		}
		if unicode.IsControl(r) {
			// 控制字符换成空格，避免把两侧的词粘成一个。
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// extractSiteName 把 "🌸色花堂🍑欧美无码" + 板块 "欧美无码" 归一成 "色花堂"。
// 上游习惯把"站点+板块"拼在一个字段里，前端筛选时按纯站点名更清晰。
func extractSiteName(site, section string) string {
	// 顺序很重要：必须先在**原始串**上按换行切出第一行，再清洗。
	// 反过来的话，清洗阶段会把换行替换成空格，"只取第一行"就彻底失效了。
	if idx := strings.IndexAny(site, "\n\r"); idx >= 0 {
		site = site[:idx]
	}
	site = cleanSiteName(site)
	if site == "" {
		return ""
	}
	if section != "" && strings.HasSuffix(site, section) {
		site = strings.TrimSpace(strings.TrimSuffix(site, section))
	}
	return site
}

// parseTimeToUnix 尽最大努力把上游时间串解析成 Unix 秒，失败返回 0。
// 返回 0 时前端排序会把它排在最后，不会污染正常数据。
func parseTimeToUnix(postTime string) int64 {
	postTime = strings.TrimSpace(postTime)
	if postTime == "" {
		return 0
	}
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		time.RFC3339,
		"2006-01-02",
		"2006/01/02 15:04:05",
		"2006/01/02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, postTime); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// formatSize 把 MB 数值格式化成人类可读的字符串。
func formatSize(sizeMB float64) string {
	if sizeMB < 0 {
		sizeMB = 0
	}
	switch {
	case sizeMB >= 1024*1024:
		return fmt.Sprintf("%.2f TB", sizeMB/(1024*1024))
	case sizeMB >= 1024:
		return fmt.Sprintf("%.2f GB", sizeMB/1024)
	default:
		return fmt.Sprintf("%.2f MB", sizeMB)
	}
}

// datePrefixPattern 匹配上游时间串开头的日期部分，兼容 - / . 三种分隔符。
var datePrefixPattern = regexp.MustCompile(`^(\d{4})[-/.](\d{1,2})[-/.](\d{1,2})`)

// formatDate 直接采用**上游给出的日期**，不做任何时区换算。
//
// 为什么不能走时间戳：
// 上游的 post_time（如 "2026-08-14 20:13:14"）不带时区，是发布站点的墙上时间。
// 若先解析成时间戳再用本地时区格式化，东八区会把 20:13 抬到次日，日期整整差一天。
// 所以这里直接从原串里取出年月日，保证"上游写什么，页面就显示什么"。
//
// 日期解析失败时按 rune 截断兜底——按 byte 截会把中文切成乱码。
func formatDate(postTime string) string {
	postTime = strings.TrimSpace(postTime)
	if postTime == "" {
		return ""
	}
	if m := datePrefixPattern.FindStringSubmatch(postTime); m != nil {
		year, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		if month >= 1 && month <= 12 && day >= 1 && day <= 31 {
			return fmt.Sprintf("%04d-%02d-%02d", year, month, day)
		}
	}
	runes := []rune(postTime)
	if len(runes) > 10 {
		return string(runes[:10])
	}
	return postTime
}

// maxKeywordRunes 限制关键词长度。超长关键词既无意义，
// 又会把上游 URL 撑爆，属于纯防御。
const maxKeywordRunes = 200

// normalizeKeyword 归一化用户输入的关键词，使其稳定支持
// 中文、英文字母、数字、日文假名以及混合写法。
//
// 三项处理：
//  1. 全角转半角——中文输入法下极容易打出全角番号（ＢＯＢＢ－４５６），
//     直接送上游会一条都搜不到，这是本工具最容易踩的坑；
//  2. 压缩空白——多个空格/全角空格/制表符统一成单个半角空格；
//  3. 剔除控制字符与非法 UTF-8 字节（range 会把坏字节变成 U+FFFD）。
func normalizeKeyword(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			// 全角 ASCII 区间（！ 到 ～）整体平移 0xFEE0 即得半角。
			b.WriteRune(r - 0xFEE0)
		case unicode.IsControl(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}

	// strings.Fields 以 unicode 空白切分，顺带处理全角空格 U+3000。
	out := strings.Join(strings.Fields(b.String()), " ")

	if runes := []rune(out); len(runes) > maxKeywordRunes {
		out = string(runes[:maxKeywordRunes])
	}
	return out
}

// collectFacets 汇总去重且**排序稳定**的筛选项。
//
// 旧实现直接遍历 map 生成下拉框，导致同一份数据每次刷新顺序都不一样；
// 这里显式排序，保证结果可复现。
func collectFacets(items []Torrent) (sites, sections, categories []string) {
	siteSet := make(map[string]struct{})
	sectionSet := make(map[string]struct{})
	categorySet := make(map[string]struct{})
	for _, t := range items {
		if t.Site != "" {
			siteSet[t.Site] = struct{}{}
		}
		if t.Section != "" {
			sectionSet[t.Section] = struct{}{}
		}
		if t.Category != "" {
			categorySet[t.Category] = struct{}{}
		}
	}
	return sortedKeys(siteSet), sortedKeys(sectionSet), sortedKeys(categorySet)
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
