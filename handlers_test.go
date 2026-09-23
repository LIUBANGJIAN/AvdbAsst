package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- 测试夹具

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// testDownloaderID 是测试夹具里的下载器标识。
//
// 提成常量是因为它被"夹具"和"断言"两处引用：改一处忘一处就会出现
// "测试还在断言旧值"这种假失败，而假失败最消耗注意力。
const testDownloaderID = "115"

// newTestApp 用假的"上游"构造一个完整应用，避免测试依赖真实的 Avdb 服务。
func newTestApp(t *testing.T, upstream http.HandlerFunc, mutate ...func(*Config)) *App {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	cfg := Config{
		APIBaseURL: server.URL,
		APIKey:     "test-key",
		Downloader: testDownloaderID,
		SavePath:   "/media",
		Addr:       ":0",
		TimeoutSec: 5,
		Timeout:    5 * time.Second,
		// 与生产默认值保持一致：安全阀关闭（0 = 不限制），每页 100 条。
		// 早先这里是 100/100，会让"翻页"根本没有发生的空间。
		MaxResults: defaultMaxResults,
		PageSize:   defaultPageSize,
		// 每个测试实例都指向独立的临时配置目录，
		// 既不污染工作区，也让"保存/重载"类测试互不干扰。
		ConfigDir: t.TempDir(),
	}
	for _, fn := range mutate {
		fn(&cfg)
	}
	return NewApp(cfg, discardLogger())
}

// doForm 提交一个 application/x-www-form-urlencoded 表单。
// origin 为空时不带 Origin 头（模拟 curl 这类非浏览器客户端）。
func doForm(app *App, target string, form url.Values, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)
	return rec
}

// upstreamWith 返回一个总是回同一段 JSON 的假上游。
func upstreamWith(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func do(app *App, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)
	return rec
}

// doDownload 以 POST 提交下载——这是浏览器前端的实际形态，
// 也是服务端唯一接受的形态（GET 一律 405，用来堵住 <img src> 触发的 CSRF）。
func doDownload(app *App, tid string) *httptest.ResponseRecorder {
	form := url.Values{"tid": {tid}}
	req := httptest.NewRequest(http.MethodPost, "/download", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)
	return rec
}

// excerpt 在断言失败时截取响应片段，便于定位问题而不是打印整页 HTML。
func excerpt(body, marker string) string {
	idx := strings.Index(body, marker)
	if idx < 0 {
		if len(body) > 400 {
			return "(未找到标记 " + marker + ") " + body[:400]
		}
		return "(未找到标记 " + marker + ") " + body
	}
	end := idx + 400
	if end > len(body) {
		end = len(body)
	}
	return body[idx:end]
}

// mustParseQuery 从 "path?a=1&b=2" 里取出查询参数，失败直接终止测试。
func mustParseQuery(t *testing.T, rawURI string) url.Values {
	t.Helper()
	u, err := url.Parse(rawURI)
	if err != nil {
		t.Fatalf("上游请求 URI 无法解析: %v", err)
	}
	return u.Query()
}

// ---------------------------------------------------------------- 搜索页

func TestSearchPageRendersResults(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	rec := do(app, http.MethodGet, "/s?q=abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `id="results"`) {
		t.Error("页面缺少结果容器")
	}
	if !strings.Contains(body, "色花堂") {
		t.Error("页面未渲染出归一化后的站点名")
	}
	// 两条结果都应出现，且计数正确。
	if !strings.Contains(body, `id="result-count">2<`) {
		t.Errorf("结果计数不正确，期望 2；片段: %s", excerpt(body, `id="result-count"`))
	}
	// 筛选下拉应包含去重后的站点，且按字典序（x1080x 在 色花堂 之后）。
	if !strings.Contains(body, `<option value="色花堂">`) {
		t.Error("站点筛选缺少 色花堂")
	}
	if !strings.Contains(body, `<option value="x1080x">`) {
		t.Error("站点筛选缺少 x1080x")
	}

	// 真正要保证的不是"某种特定顺序"，而是"同一输入永远得到同一顺序"。
	// 旧实现遍历 map 生成下拉框，每次刷新顺序都不同。
	for i := 0; i < 20; i++ {
		again := do(app, http.MethodGet, "/s?q=abc").Body.String()
		if optionBlock(again) != optionBlock(body) {
			t.Fatalf("第 %d 次请求的筛选项顺序与首次不一致，说明存在 map 遍历顺序泄漏", i)
		}
	}
}

// optionBlock 抽出所有站点 option，用于比较筛选项的生成顺序。
func optionBlock(body string) string {
	var b strings.Builder
	rest := body
	for {
		idx := strings.Index(rest, `<option value="`)
		if idx < 0 {
			return b.String()
		}
		rest = rest[idx:]
		end := strings.Index(rest, "</option>")
		if end < 0 {
			return b.String()
		}
		b.WriteString(rest[:end+len("</option>")])
		b.WriteByte('\n')
		rest = rest[end+len("</option>"):]
	}
}

// TestSearchPageEscapesUpstreamHTML 是本项目的关键安全回归测试。
//
// 上游标题/磁力链接属于外部不可信输入。旧实现用 innerHTML 直接拼接，
// 会被上游（或其数据源）注入脚本。这里断言恶意载荷不会以可执行形式出现。
func TestSearchPageEscapesUpstreamHTML(t *testing.T) {
	const payload = `</script><img src=x onerror=alert(1)>`
	const magnet = `magnet:?xt=urn:btih:AAA' onmouseover='alert(2)`
	const siteName = `<b>bold</b>`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "message": "ok",
			"data": []map[string]any{{
				"id": 1, "title": payload, "site": siteName, "section": "s",
				"download_url": magnet, "size_mb": 100.0,
			}},
		})
	}))
	defer upstream.Close()

	app := NewApp(Config{APIBaseURL: upstream.URL, Timeout: 5 * time.Second, MaxResults: 10, Addr: ":0"},
		discardLogger())
	rec := do(app, http.MethodGet, "/s?q=x")
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}

	// 危险片段绝不能原样出现。
	dangerous := []string{
		payload, // 闭合 script 标签的注入
		"<img src=x onerror=alert(1)>",
		"onmouseover='alert(2)'", // 属性越界注入
		"<b>bold</b>",            // 站点名里的 HTML
	}
	for _, bad := range dangerous {
		if strings.Contains(body, bad) {
			t.Errorf("响应中出现未转义的危险片段: %q", bad)
		}
	}

	// 内容应以转义形式保留，而不是被整条丢掉。
	if !strings.Contains(body, "&lt;img") {
		t.Error("标题内容被丢弃，应以转义形式保留")
	}
}

func TestSearchRoutesAreEquivalent(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	targets := []string{
		"/s?q=abc",
		"/s?keyword=abc",
		"/search?q=abc",
		"/search?keyword=abc",
		"/search?kw=abc",
		"/s/abc",
		"/search/abc",
	}
	for _, target := range targets {
		rec := do(app, http.MethodGet, target)
		if rec.Code != http.StatusOK {
			t.Errorf("%s 状态码 = %d, 期望 200", target, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), `id="results"`) {
			t.Errorf("%s 未渲染出结果", target)
		}
	}
}

// TestKeywordReachesUpstream 验证"关键字支持中文、英文字母数字等"这条要求
// 在 HTTP 全链路上是真的成立的：从 URL 解码 → 归一化 → 再编码送上游。
func TestKeywordReachesUpstream(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   string // 上游最终应收到什么
	}{
		{
			name:   "中文关键词",
			target: "/s?" + url.Values{"q": {"少林足球"}}.Encode(),
			want:   "少林足球",
		},
		{
			name:   "英文加数字番号",
			target: "/s?" + url.Values{"q": {"BOBB-456"}}.Encode(),
			want:   "BOBB-456",
		},
		{
			name:   "中英数混排带空格",
			target: "/s?" + url.Values{"q": {"少林足球 Shaolin 2001"}}.Encode(),
			want:   "少林足球 Shaolin 2001",
		},
		{
			name:   "全角输入自动折半角",
			target: "/s?" + url.Values{"q": {"ＢＯＢＢ－４５６"}}.Encode(),
			want:   "BOBB-456",
		},
		{
			name:   "路径式中文关键词",
			target: "/s/" + url.PathEscape("中文"),
			want:   "中文",
		},
		{
			name:   "日文假名",
			target: "/s?" + url.Values{"q": {"あいうえお"}}.Encode(),
			want:   "あいうえお",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Query().Get("keyword")
				_, _ = io.WriteString(w, `{"code":0,"message":"ok","data":[]}`)
			})

			rec := do(app, http.MethodGet, tc.target)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d, 期望 200", rec.Code)
			}
			if got != tc.want {
				t.Errorf("上游收到 keyword=%q, 期望 %q（请求 %s）", got, tc.want, tc.target)
			}
		})
	}
}

func TestSearchEmptyKeywordShowsHome(t *testing.T) {
	var called bool
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	for _, target := range []string{"/", "/s", "/s?q=", "/s?q=%20%20"} {
		rec := do(app, http.MethodGet, target)
		if rec.Code != http.StatusOK {
			t.Errorf("%s 状态码 = %d, 期望 200", target, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "hero") {
			t.Errorf("%s 应展示首屏引导页", target)
		}
	}
	if called {
		t.Error("关键词为空时不应请求上游")
	}
}

func TestSearchUpstreamErrorIsFriendly(t *testing.T) {
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"invalid api key"}`)
	})

	rec := do(app, http.MethodGet, "/s?q=abc")

	// 上游鉴权失败不应让本站 500，而是渲染出可读的提示。
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（错误应体现在页面内容里）", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "鉴权失败") {
		t.Errorf("未给出鉴权失败提示；片段: %s", excerpt(body, "class=\"alert"))
	}
	if !strings.Contains(body, `class="alert`) {
		t.Error("缺少错误提示条")
	}
}

func TestSearchTimeoutIsFriendly(t *testing.T) {
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		_, _ = io.WriteString(w, `{"code":0,"data":[]}`)
	}, func(c *Config) {
		c.Timeout = 200 * time.Millisecond
	})

	start := time.Now()
	rec := do(app, http.MethodGet, "/s?q=abc")
	elapsed := time.Since(start)

	// 超时必须被自己的超时控制住，不能一直挂着。
	if elapsed > 3*time.Second {
		t.Errorf("超时未生效，耗时 %v", elapsed)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "超时") && !strings.Contains(body, "失败") {
		t.Errorf("未给出可读的超时提示；body=%s", excerpt(body, "class=\"alert\""))
	}
}

// TestSearchSafetyValveTruncates 验证"结果安全阀"确实会拦下异常大的结果集。
//
// 注意语义变化：MaxResults 已经不是页面上的"结果上限"（那一项按用户要求
// 去掉了，搜索结果本身没有上限），它现在的职责是防止异常上游一次返回几十万条
// 把进程内存打满。触发时仍然要给出提示。
func TestSearchSafetyValveTruncates(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.MaxResults = 1 })

	rec := do(app, http.MethodGet, "/s?q=abc")
	body := rec.Body.String()

	if !strings.Contains(body, `id="result-count">1<`) {
		t.Errorf("应被截断为 1 条；片段: %s", excerpt(body, "id=\"result-count\""))
	}
	if !strings.Contains(body, "结果过多") {
		t.Error("触发安全阀时应给出提示")
	}
}

func TestSearchNoResult(t *testing.T) {
	app := newTestApp(t, upstreamWith(`{"code":0,"message":"操作成功","data":[]}`))

	rec := do(app, http.MethodGet, "/s?q=zzz")
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	if !strings.Contains(body, "没有找到") {
		t.Error("应展示空结果提示")
	}
	// 空态里必须回显关键词，否则用户看不出自己搜的是什么。
	if !strings.Contains(body, "zzz") {
		t.Error("空结果提示应包含原关键词")
	}
}

func TestSearchBusinessErrorFromUpstream(t *testing.T) {
	app := newTestApp(t, upstreamWith(`{"code":4001,"message":"关键词过短"}`))

	rec := do(app, http.MethodGet, "/s?q=a")
	if !strings.Contains(rec.Body.String(), "关键词过短") {
		t.Error("上游业务错误信息应透传给用户")
	}
}

// ---------------------------------------------------------------- JSON API

func TestAPISearch(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	rec := do(app, http.MethodGet, "/api/search?q=abc")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}

	var payload apiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if payload.Count != 2 || len(payload.Torrents) != 2 {
		t.Errorf("Count=%d len=%d, 期望均为 2", payload.Count, len(payload.Torrents))
	}
	if payload.Keyword != "abc" {
		t.Errorf("Keyword = %q", payload.Keyword)
	}
	if len(payload.Sites) != 2 {
		t.Errorf("sites = %v, 期望 2 个", payload.Sites)
	}
	if payload.Torrents[0].Site != "色花堂" {
		t.Errorf("站点未归一化: %q", payload.Torrents[0].Site)
	}
}

func TestAPISearchMissingKeyword(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	rec := do(app, http.MethodGet, "/api/search")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 400", rec.Code)
	}
	var payload apiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if payload.Error == "" {
		t.Error("应返回 error 字段")
	}
	// torrents 必须是 []，不能是 null，否则前端要额外判空。
	if !strings.Contains(rec.Body.String(), `"torrents":[]`) {
		t.Errorf("空结果应序列化为 []，实际: %s", rec.Body.String())
	}
}

func TestAPISearchUpstreamFailure(t *testing.T) {
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "bad gateway")
	})

	rec := do(app, http.MethodGet, "/api/search?q=abc")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("状态码 = %d, 期望 502", rec.Code)
	}
	var payload apiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if payload.Error == "" {
		t.Error("上游失败时应带 error 字段")
	}
}

// ---------------------------------------------------------------- 结果列表

// TestResultsRenderAsListWithoutPoster 锁定"列表形式展示、不加载海报"这一产品要求。
// preview_image 在客户端仍做协议白名单（见下一个用例），但页面不应再渲染任何图片：
// 海报既拖慢首屏，也不是列表这种高密度扫读场景需要的信息。
func TestResultsRenderAsListWithoutPoster(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	body := do(app, http.MethodGet, "/s?q=abc").Body.String()

	if !strings.Contains(body, `class="results"`) {
		t.Error("结果区应为列表容器 .results")
	}
	if !strings.Contains(body, `class="row"`) {
		t.Error("每条结果应为列表项 .row")
	}
	if strings.Contains(body, "<img") {
		t.Errorf("结果列表不应包含任何 <img>；片段: %s", excerpt(body, "<img"))
	}
	if strings.Contains(body, "preview_image") {
		t.Error("页面不应再引用 preview_image")
	}
	// 旧实现的缩略图/卡片相关标记必须彻底消失，否则说明只删了模板没清样式。
	for _, gone := range []string{"thumb", "hero-grid", "card-"} {
		if strings.Contains(body, gone) {
			t.Errorf("旧卡片结构残留标记 %q", gone)
		}
	}
}

// TestPosterFieldStillSanitized 说明"不显示"不等于"不过滤"：
// 客户端依旧对 preview_image 做协议白名单，API 消费方拿到的仍是干净数据。
func TestPosterFieldStillSanitized(t *testing.T) {
	upstream := `{"code":0,"message":"操作成功","data":[
		{"id":1,"number":"A-1","title":"T","preview_image":"javascript:alert(1)"},
		{"id":2,"number":"A-2","title":"T","preview_image":"https://cdn.example.com/a.jpg"}
	]}`
	app := newTestApp(t, upstreamWith(upstream))

	rec := do(app, http.MethodGet, "/api/search?q=abc")
	var payload apiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(payload.Torrents) != 2 {
		t.Fatalf("期望 2 条结果，实际 %d", len(payload.Torrents))
	}
	if got := payload.Torrents[0].PreviewImage; got != "" {
		t.Errorf("危险协议的 preview_image 应被清空，实际 %q", got)
	}
	if got := payload.Torrents[1].PreviewImage; !strings.HasPrefix(got, "https://") {
		t.Errorf("合法的 https 图片地址应保留，实际 %q", got)
	}
}

// ---------------------------------------------------------------- 下载

func TestDownloadMissingTID(t *testing.T) {
	app := newTestApp(t, upstreamWith(`{"code":0}`))

	rec := doDownload(app, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 400", rec.Code)
	}
	var resp downloadResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Success {
		t.Error("缺少 tid 时 success 应为 false")
	}
}

func TestDownloadInvalidTID(t *testing.T) {
	var hit bool
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"code":0}`)
	})

	for _, tid := range []string{
		"../../etc/passwd", // 路径穿越
		"1%00",             // 百分号
		"a b",              // 空格
		"x?y=1",            // 查询串注入
		"a/b",
		"a.b",
		"a#b",
		"abcdefghij0123456789abcdefghij0123456789abcdefghij0123456789abcdefghij", // 超长
	} {
		// 表单编码交给 url.Values，避免手工拼接引入的编码歧义。
		rec := doDownload(app, tid)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("tid=%q 状态码 = %d, 期望 400", tid, rec.Code)
		}
	}
	if hit {
		t.Error("非法 tid 不应触达上游")
	}
}

// TestDownloadRejectsGET 锁定一个真实存在过的安全缺口：
// 旧实现用 GET 触发上游下载提交，外部页面只要放一个
// <img src="http://本站/download?tid=1"> 就能替用户提交下载。
func TestDownloadRejectsGET(t *testing.T) {
	var hit bool
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"code":0}`)
	})

	rec := do(app, http.MethodGet, "/download?tid=3691410")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET 状态码 = %d, 期望 405", rec.Code)
	}
	if hit {
		t.Error("GET 不应触达上游")
	}
}

func TestDownloadSuccess(t *testing.T) {
	var gotPath, gotKey, gotMethod string
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotKey = r.Header.Get("X-API-Key")
		gotMethod = r.Method
		_, _ = io.WriteString(w, `{"code":0,"message":"操作成功","data":{}}`)
	})

	rec := doDownload(app, "3691410")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}

	var resp downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !resp.Success {
		t.Errorf("code=0 应判定为成功，实际 message=%q", resp.Message)
	}
	if resp.TID != "3691410" {
		t.Errorf("TID = %q", resp.TID)
	}
	// 必须把配置里的下载器与保存路径透传上游。
	if gotMethod != http.MethodGet {
		t.Errorf("后端请求上游应使用 GET，实际 %s", gotMethod)
	}
	if !strings.Contains(gotPath, "downloader="+testDownloaderID) || !strings.Contains(gotPath, "save_path=%2Fmedia") {
		t.Errorf("上游请求缺少参数: %s", gotPath)
	}
	if gotKey != "test-key" {
		t.Errorf("上游请求未携带 API Key，实际 %q", gotKey)
	}
}

// TestDownloadRequiresConfiguredSavePath 守住"保存目录必填且不能为空"。
//
// 上游对空 save_path 的回答是「保存目录不能为空」，对缺失的回答是 422。
// 两种都不该让用户看到——本站必须在**发请求之前**拦下，并告诉他去哪补：
// 一句英文校验错误对用户毫无信息量，而"去设置页填保存路径"立刻可执行。
//
// 同时断言"没有触达上游"：既然参数注定不合法，发出去只是白等一轮超时。
func TestDownloadRequiresConfiguredSavePath(t *testing.T) {
	var hit bool
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"code":0,"message":"操作成功"}`)
	}, func(c *Config) {
		// 模拟"用户什么都没填"的最常见部署形态。
		c.Downloader = ""
		c.SavePath = ""
	})

	rec := doDownload(app, "3691410")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400", rec.Code)
	}
	if hit {
		t.Error("保存路径没配时不应向上游发请求：注定会失败，只是白等一轮")
	}

	var resp downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if resp.Success {
		t.Error("保存路径为空时 success 必须为 false")
	}
	for _, want := range []string{"保存路径", "设置"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("提示里应包含 %q，实际 %q", want, resp.Message)
		}
	}
}

// TestDownloadFallsBackToDefaultDownloader 守住"下载器绝不会是空串"。
//
// 上游把 downloader 改成必填之后，"配置留空"再也不能翻译成"不发这个参数"。
// 本站的处理是给一个实测有效的兜底标识；这里断言它确实被发了出去，
// 而不是留下一个空值去撞 422。
func TestDownloadFallsBackToDefaultDownloader(t *testing.T) {
	var gotPath string
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		_, _ = io.WriteString(w, `{"code":0,"message":"操作成功"}`)
	}, func(c *Config) {
		c.Downloader = ""
		c.SavePath = "/media/movies"
	})

	rec := doDownload(app, "3691410")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（上游已配好下载器与目录时应当成功）；body=%s",
			rec.Code, rec.Body.String())
	}

	query := mustParseQuery(t, gotPath)
	if got := query.Get("downloader"); got != fallbackDownloaderID {
		t.Errorf("downloader = %q，期望兜底值 %q（URI: %s）", got, fallbackDownloaderID, gotPath)
	}
	if got := query.Get("save_path"); got != "/media/movies" {
		t.Errorf("save_path = %q，期望原样发送", got)
	}
}

// TestDownloadRejectsCrossSitePost 验证跨站表单提交被拦下。
func TestDownloadRejectsCrossSitePost(t *testing.T) {
	var hit bool
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"code":0}`)
	})

	form := url.Values{"tid": {"3691410"}}
	req := httptest.NewRequest(http.MethodPost, "/download", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example.com")
	req.Header.Set("Referer", "http://evil.example.com/attack.html")
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("状态码 = %d, 期望 403", rec.Code)
	}
	if hit {
		t.Error("跨站请求不应触达上游")
	}
	// 403 必须给出可自助排查的现场信息，而不是一句话了事。
	if !strings.Contains(rec.Body.String(), "Origin") {
		t.Error("403 响应应回显实际收到的来源请求头")
	}
}

func TestDownloadUpstreamUnreachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	target := server.URL
	server.Close() // 立刻关闭，制造"连不上"

	app := NewApp(Config{
		APIBaseURL: target,
		Timeout:    2 * time.Second,
		Addr:       ":0",
		// 下载目标必须配齐：否则会在发请求前就被"保存目录不能为空"拦下，
		// 测不到"连不上上游"这条路径。
		Downloader: "clouddrive",
		SavePath:   "/media",
	}, discardLogger())
	rec := doDownload(app, "1")

	if rec.Code != http.StatusBadGateway {
		t.Errorf("状态码 = %d, 期望 502", rec.Code)
	}
	var resp downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if resp.Success {
		t.Error("连不上上游时 success 应为 false")
	}
}

func TestInterpretDownloadResult(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantSuccess bool
		wantMessage string
	}{
		{"code=0 视为成功", 200, `{"code":0,"message":"操作成功"}`, true, "操作成功"},
		{"code!=0 视为失败", 200, `{"code":4001,"message":"下载器未配置"}`, false, "下载器未配置"},
		{"显式 success=true", 200, `{"success":true,"message":"已加入队列"}`, true, "已加入队列"},
		{"显式 success=false 覆盖状态码", 200, `{"success":false,"message":"空间不足"}`, false, "空间不足"},
		{"success 无 message 用默认文案", 200, `{"success":true}`, true, "已提交到下载器"},
		{"success=false 无 message", 200, `{"success":false}`, false, "上游未说明失败原因"},
		{"嵌套 data.success", 200, `{"data":{"success":true,"message":"已提交"}}`, true, "已提交"},
		{"嵌套 data.code", 200, `{"data":{"code":1,"message":"失败"}}`, false, "失败"},
		{"只有 message 时按状态码判定", 200, `{"message":"处理中"}`, true, "处理中"},
		{"只有 message 且状态码为错", 500, `{"message":"内部错误"}`, false, "内部错误"},
		{"未知 JSON 结构不裸奔", 200, `{"foo":"bar"}`, true, "已提交到下载器"},
		{"非 JSON 按状态码判定(成功)", 200, `oops`, true, "oops"},
		{"非 JSON 按状态码判定(失败)", 502, `boom`, false, "boom"},
		{"空响应体且成功", 200, ``, true, "已提交到下载器"},
		{"空响应体且失败", 503, ``, false, "上游返回 HTTP 503"},
		{"msg 字段别名", 200, `{"code":0,"msg":"已提交到下载器"}`, true, "已提交到下载器"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := interpretDownloadResult(tc.status, []byte(tc.body))
			if got.Success != tc.wantSuccess {
				t.Errorf("Success = %v, 期望 %v", got.Success, tc.wantSuccess)
			}
			if got.Message != tc.wantMessage {
				t.Errorf("Message = %q, 期望 %q", got.Message, tc.wantMessage)
			}
		})
	}
}

// ---------------------------------------------------------------- 访问口令

func TestAuthDisabledByDefault(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))
	if rec := do(app, http.MethodGet, "/s?q=a"); rec.Code != http.StatusOK {
		t.Errorf("未配置口令时应放行，实际 %d", rec.Code)
	}
}

func TestAuthTokenRequired(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.AccessToken = "s3cret" })

	// 无口令
	if rec := do(app, http.MethodGet, "/s?q=a"); rec.Code != http.StatusUnauthorized {
		t.Errorf("无口令访问状态码 = %d, 期望 401", rec.Code)
	}
	// 错误口令
	if rec := do(app, http.MethodGet, "/s?q=a&token=wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("错误口令访问状态码 = %d, 期望 401", rec.Code)
	}
	// 正确口令 + 下发 Cookie
	rec := do(app, http.MethodGet, "/s?q=a&token=s3cret")
	if rec.Code != http.StatusOK {
		t.Errorf("正确口令访问状态码 = %d, 期望 200", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), authCookieName) {
		t.Errorf("通过口令校验后应下发 Cookie，实际 %q", rec.Header().Get("Set-Cookie"))
	}

	// 健康检查必须免鉴权，否则容器探活会一直失败。
	if rec := do(app, http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("健康检查应免鉴权，实际 %d", rec.Code)
	}
}

func TestAuthCookieAndHeader(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.AccessToken = "s3cret" })

	req := httptest.NewRequest(http.MethodGet, "/s?q=a", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: "s3cret"})
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("带正确 Cookie 状态码 = %d, 期望 200", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/s?q=a", nil)
	req.Header.Set("X-Auth-Token", "s3cret")
	rec = httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("带正确请求头状态码 = %d, 期望 200", rec.Code)
	}
}

func TestAuthTokenAppearsInSearchTemplate(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.AccessToken = "s3cret" })

	// 首页已不再展示搜索模板（那段引导按需求移除），所以这里只校验
	// OpenSearch 描述文档：浏览器自动发现它时必须带上口令，
	// 否则用户照着加出来的站点搜索会一直 401。
	rec := do(app, http.MethodGet, "/opensearch.xml?token=s3cret")
	if !strings.Contains(rec.Body.String(), "token=s3cret") {
		t.Error("OpenSearch 模板应包含访问口令")
	}
}

// ---------------------------------------------------------------- 其他端点

func TestHealthz(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	rec := do(app, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if payload["status"] != "ok" {
		t.Errorf("status = %v", payload["status"])
	}
}

func TestOpenSearchDescription(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	rec := do(app, http.MethodGet, "/opensearch.xml")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "opensearchdescription+xml") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Error("缺少 XML 声明")
	}
	if !strings.Contains(body, `template="http://example.com/s?q={searchTerms}"`) {
		t.Errorf("搜索模板不正确: %s", body)
	}
	for _, want := range []string{"<ShortName>", "<Description>", "<Url ", "OpenSearchDescription"} {
		if !strings.Contains(body, want) {
			t.Errorf("OpenSearch 文档缺少 %s", want)
		}
	}
}

func TestOpenSearchHonoursForwardedProto(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	req := httptest.NewRequest(http.MethodGet, "/opensearch.xml", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "avdb.example.com")
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	// 反向代理后面必须能生成正确的对外地址，否则浏览器添加的搜索会是错的。
	if !strings.Contains(rec.Body.String(), `https://avdb.example.com/s?q={searchTerms}`) {
		t.Errorf("未正确识别代理头: %s", rec.Body.String())
	}
}

func TestStaticAssets(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	for _, path := range []string{"/static/app.css", "/static/app.js", "/favicon.svg", "/favicon.ico"} {
		rec := do(app, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s 状态码 = %d, 期望 200", path, rec.Code)
			continue
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s 返回空内容", path)
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	// 只读端点拒绝 POST。
	for _, path := range []string{"/s", "/api/search", "/healthz"} {
		if rec := do(app, http.MethodPost, path); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s 状态码 = %d, 期望 405", path, rec.Code)
		}
	}
	// 下载是唯一的写操作，反向要求：只接受 POST。
	if rec := do(app, http.MethodGet, "/download?tid=1"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /download 状态码 = %d, 期望 405", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	rec := do(app, http.MethodGet, "/s?q=a")
	h := rec.Header()
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("缺少 X-Content-Type-Options")
	}
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP 未限制脚本来源: %q", csp)
	}
	// 前端不能有内联脚本，所以 CSP 里不该出现 unsafe-inline。
	if strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP 不应包含 unsafe-inline: %q", csp)
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Error("搜索结果页不应被缓存")
	}
}

func TestRecoverMiddleware(t *testing.T) {
	app := &App{log: discardLogger()}
	handler := app.recoverMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("故意炸一下")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("panic 应被兜成 500，实际 %d", rec.Code)
	}
}

// ---------------------------------------------------------------- 设置页面

func TestSettingsPageRenders(t *testing.T) {
	// 不设访问口令，否则下面的匿名 GET 会被鉴权挡掉（那条路径由
	// TestSettingsBehindAuth 单独覆盖）。
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) {
		c.APIKey = "super-secret-api-key-123456"
	})

	rec := do(app, http.MethodGet, "/settings")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	body := rec.Body.String()

	for _, name := range []string{
		`name="api_base_url"`, `name="api_key"`, `name="default_downloader"`,
		`name="default_save_path"`, `name="access_token"`, `name="page_size"`,
	} {
		if !strings.Contains(body, name) {
			t.Errorf("设置页面缺少字段 %s", name)
		}
	}
	// 「请求超时」「结果上限」已按用户要求下线：搜索结果不再设上限，
	// 超时属于部署细节。它们必须彻底从表单里消失，而不是藏起来——
	// 留着输入框就会让人以为改了有用。
	for _, name := range []string{`name="timeout_seconds"`, `name="max_results"`} {
		if strings.Contains(body, name) {
			t.Errorf("设置页面不应再出现字段 %s", name)
		}
	}
	if !strings.Contains(body, `id="btn-test"`) {
		t.Error("设置页面缺少测试连接按钮")
	}
	// 下载相关只保留一个「检测」按钮：早先的三个按钮（探测 / 读默认 / 校验）
	// 对用户是同一个问题，拆开后第一反应变成"我该点哪个"。
	if !strings.Contains(body, `id="btn-detect"`) {
		t.Error("设置页面缺少「检测」按钮")
	}
	for _, gone := range []string{`id="btn-probe-downloaders"`, `id="btn-default-rule"`, `id="btn-check-downloader"`} {
		if strings.Contains(body, gone) {
			t.Errorf("设置页面不应再出现已合并的按钮 %s", gone)
		}
	}
	// 「下载器」必须是下拉选择：自由文本一旦输入就无法"取消选择"，只能删掉重打。
	if !strings.Contains(body, `<select id="default_downloader"`) {
		t.Error("「下载器」应当是下拉选择，而不是自由文本输入框")
	}
}

// TestSettingsPageNeverLeaksSecrets 是本项目的关键安全回归测试。
//
// Avdb 官方文档明确要求访问令牌不得进入前端；设置页面只需要让用户知道
// "已配置"，因此只能显示掩码。
func TestSettingsPageNeverLeaksSecrets(t *testing.T) {
	const apiKey = "fpOJkZUFAxAzuUr6MsqxjkSw2fyZi1BZ"
	const accessToken = "my-access-token-please-hide"

	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) {
		c.APIKey = apiKey
		c.AccessToken = accessToken
	})

	// 带口令访问，否则会被鉴权挡在门外。
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.Header.Set("X-Auth-Token", accessToken)
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	body := rec.Body.String()

	if strings.Contains(body, apiKey) {
		t.Error("设置页面泄露了完整 API Key")
	}
	if strings.Contains(body, accessToken) {
		t.Error("设置页面泄露了完整访问口令")
	}
	// 中段尤其不能出现。
	if strings.Contains(body, apiKey[4:len(apiKey)-4]) {
		t.Error("设置页面泄露了 API Key 中段")
	}
	// 但要让用户看得到"已配置"的提示。
	if !strings.Contains(body, "已配置") {
		t.Error("设置页面应提示密钥已配置")
	}
}

func TestSettingsSavePersistsAndApplies(t *testing.T) {
	dir := t.TempDir()
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.ConfigDir = dir })

	form := url.Values{
		"api_base_url":       {"http://new-host:9999"},
		"api_key":            {"new-secret-key"},
		"default_downloader": {"clouddrive"},
		"default_save_path":  {"/影视/新目录"},
		"page_size":          {"200"},
	}
	rec := doForm(app, "/settings", form, "http://example.com")

	// PRG 模式：成功后排 303，避免刷新重复提交。
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303；body=%s", rec.Code, excerpt(rec.Body.String(), ""))
	}
	if loc := rec.Header().Get("Location"); loc != "/settings?saved=1" {
		t.Errorf("Location = %q", loc)
	}

	// 1) 落盘：配置文件必须真的写出来。
	data, err := os.ReadFile(filepath.Join(dir, configFileName))
	if err != nil {
		t.Fatalf("配置文件未生成: %v", err)
	}
	if !strings.Contains(string(data), "new-secret-key") {
		t.Error("配置文件里没有写入新的 API Key")
	}

	// 2) 热更新：内存中的配置立即生效，不需要重启。
	cfg := app.currentConfig()
	if cfg.APIBaseURL != "http://new-host:9999" {
		t.Errorf("热更新失败，APIBaseURL = %q", cfg.APIBaseURL)
	}
	if cfg.APIKey != "new-secret-key" {
		t.Errorf("热更新失败，APIKey = %q", cfg.APIKey)
	}
	if cfg.PageSize != 200 {
		t.Errorf("热更新失败，PageSize = %d", cfg.PageSize)
	}
	if cfg.Downloader != "clouddrive" {
		t.Errorf("热更新失败，Downloader = %q", cfg.Downloader)
	}
	if cfg.SavePath != "/影视/新目录" {
		t.Errorf("中文保存路径被破坏: %q", cfg.SavePath)
	}

	// 3) 重新读取配置（等价于容器重启），值必须保持。
	t.Chdir(t.TempDir())
	clearConfigEnv(t)
	t.Setenv("AVDB_CONFIG_DIR", dir)
	reloaded, _ := ResolveConfig()
	if reloaded.APIKey != "new-secret-key" || reloaded.APIBaseURL != "http://new-host:9999" {
		t.Errorf("重启后配置丢失: key=%q base=%q", reloaded.APIKey, reloaded.APIBaseURL)
	}
	// 每页条数是"永久设置"：用户要求"直到下次再设置"之前一直生效，
	// 所以重启后也必须保持，不能被 normalizeConfig 悄悄改回 100。
	if reloaded.PageSize != 200 {
		t.Errorf("重启后每页条数丢失: PageSize = %d，期望 200", reloaded.PageSize)
	}
}

// TestSettingsSaveHotReloadsUpstream 验证保存之后搜索真的走了新地址，
// 而不只是内存变量被改了。
func TestSettingsSaveHotReloadsUpstream(t *testing.T) {
	oldUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":[]}`)
	}))
	defer oldUpstream.Close()

	newUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"message":"ok","data":[{"id":1,"title":"来自新上游的结果","site":"新站","section":"s","size_mb":10}]}`)
	}))
	defer newUpstream.Close()

	app := NewApp(Config{
		APIBaseURL: oldUpstream.URL, APIKey: "k", Addr: ":0",
		Timeout: 5 * time.Second, MaxResults: 50, ConfigDir: t.TempDir(),
	}, discardLogger())

	if body := do(app, http.MethodGet, "/s?q=x").Body.String(); strings.Contains(body, "来自新上游的结果") {
		t.Fatal("前置条件不成立：旧上游不该返回新数据")
	}

	rec := doForm(app, "/settings", url.Values{
		"api_base_url": {newUpstream.URL},
		"api_key":      {"k"},
	}, "http://example.com")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("保存设置失败，状态码 = %d", rec.Code)
	}

	if body := do(app, http.MethodGet, "/s?q=x").Body.String(); !strings.Contains(body, "来自新上游的结果") {
		t.Error("保存后搜索未切换到新的上游地址")
	}
}

func TestSettingsSaveBlankSecretKeepsExisting(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) {
		c.APIKey = "existing-key"
	})

	// 密钥框留空 —— 用户只想改地址，不想重填令牌。
	rec := doForm(app, "/settings", url.Values{
		"api_base_url": {"http://changed-host:1234"},
		"api_key":      {""},
	}, "http://example.com")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303", rec.Code)
	}
	if got := app.currentConfig().APIKey; got != "existing-key" {
		t.Errorf("留空不应清空密钥，实际 %q", got)
	}
}

func TestSettingsSaveClearSecret(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) {
		c.APIKey = "existing-key"
	})

	rec := doForm(app, "/settings", url.Values{
		"api_base_url":  {"http://host:1"},
		"api_key":       {""},
		"clear_api_key": {"1"},
	}, "http://example.com")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303", rec.Code)
	}
	if got := app.currentConfig().APIKey; got != "" {
		t.Errorf("勾选清除后密钥应为空，实际 %q", got)
	}
}

func TestSettingsSaveRejectsInvalidURL(t *testing.T) {
	dir := t.TempDir()
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.ConfigDir = dir })

	for _, bad := range []string{"", "192.168.1.1:8999", "ftp://host", "javascript:alert(1)", "://x"} {
		rec := doForm(app, "/settings", url.Values{
			"api_base_url": {bad},
			"api_key":      {"k"},
		}, "http://example.com")

		if rec.Code != http.StatusBadRequest {
			t.Errorf("地址 %q 状态码 = %d, 期望 400", bad, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "不合法") && !strings.Contains(rec.Body.String(), "失败") {
			t.Errorf("地址 %q 未给出可读错误", bad)
		}
	}

	// 校验失败时绝不能留下半截配置文件。
	if _, err := os.Stat(filepath.Join(dir, configFileName)); err == nil {
		t.Error("校验失败时不应写出配置文件")
	}
	// 内存配置也不能被污染。
	if app.currentConfig().APIBaseURL != "" && !strings.HasPrefix(app.currentConfig().APIBaseURL, "http://") {
		t.Error("校验失败后内存配置被污染")
	}
}

func TestSettingsPageShowsValidationError(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	rec := doForm(app, "/settings", url.Values{
		"api_base_url": {"不是一个地址"},
	}, "http://example.com")

	body := rec.Body.String()
	if !strings.Contains(body, "class=\"alert\"") {
		t.Error("校验失败应展示错误提示条")
	}
	// 应保留用户填错的内容，便于就地修改。
	if !strings.Contains(body, "不是一个地址") {
		t.Error("校验失败时应回显用户输入")
	}
}

func TestSettingsSaveRejectsCrossOrigin(t *testing.T) {
	dir := t.TempDir()
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.ConfigDir = dir })

	rec := doForm(app, "/settings", url.Values{
		"api_base_url": {"http://attacker-controlled:1"},
	}, "http://evil.example.com")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨站提交状态码 = %d, 期望 403", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, configFileName)); err == nil {
		t.Error("跨站请求竟写入了配置")
	}
	if app.currentConfig().APIBaseURL == "http://attacker-controlled:1" {
		t.Error("跨站请求竟修改了内存配置")
	}
}

func TestSettingsSaveAcceptsNonBrowserClient(t *testing.T) {
	// 不带 Origin / Referer 的请求（curl、脚本）不在 CSRF 防护范围内，
	// 交给访问口令去把关。
	app := newTestApp(t, upstreamWith(upstreamSample))
	rec := doForm(app, "/settings", url.Values{
		"api_base_url": {"http://host:1"},
		"api_key":      {"k"},
	}, "")

	if rec.Code != http.StatusSeeOther {
		t.Errorf("状态码 = %d, 期望 303", rec.Code)
	}
}

// TestSameOriginMatrix 穷举来源判定的各类真实场景。
//
// 这张表来自一次真实故障：用户保存设置必然 403。根因是 sameOrigin 只认 r.Host，
// 而反向代理转发时 r.Host 是后端内部地址、浏览器的 Origin 却是外部域名；
// 同一项目的 requestBase() 却信任 X-Forwarded-Host —— 两处对"本站"的口径不一致。
// 因此下面的"反代""端口规范化"用例都是回归用例，防止判据被再次收窄。
func TestSameOriginMatrix(t *testing.T) {
	cases := []struct {
		name      string
		host      string
		origin    string
		referer   string
		forwarded string
		want      bool
	}{
		// ---- 直连：基线，行为不得回归 ----
		{"直连 Origin 一致", "127.0.0.1:8080", "http://127.0.0.1:8080", "", "", true},
		{"直连 仅有同源 Referer", "127.0.0.1:8080", "", "http://127.0.0.1:8080/settings", "", true},
		{"直连 无任何来源信息", "127.0.0.1:8080", "", "", "", true},

		// ---- 反向代理：本次故障的核心场景 ----
		{"反代 代理回传原始主机", "127.0.0.1:8080", "http://nas.example.com:5000", "http://nas.example.com:5000/settings", "nas.example.com:5000", true},
		{"HTTPS 反代 代理回传原始主机", "127.0.0.1:8080", "https://avdb.example.com", "https://avdb.example.com/settings", "avdb.example.com", true},
		{"反代 代理未回传任何外部主机", "127.0.0.1:8080", "http://nas.example.com:5000", "", "", false},

		// ---- Origin: null（隐私上下文、沙箱 iframe）----
		{"Origin 为 null 且有同源 Referer", "127.0.0.1:8080", "null", "http://127.0.0.1:8080/settings", "", true},
		{"Origin 为 null 且无 Referer", "127.0.0.1:8080", "null", "", "", true},

		// ---- 主机名与端口规范化 ----
		{"Host 带默认端口 80", "example.com:80", "http://example.com", "", "", true},
		{"Host 带默认端口 443", "example.com:443", "https://example.com", "", "", true},
		{"主机名大小写不同", "Example.COM:8080", "http://example.com:8080", "", "", true},
		{"主机名带尾点", "example.com.", "http://example.com", "", "", true},
		{"非默认端口不同 应拒绝", "127.0.0.1:8080", "http://127.0.0.1:9090", "", "", false},

		// ---- 真实跨站：必须始终拒绝 ----
		{"跨站 Origin", "127.0.0.1:8080", "http://evil.example.com", "http://evil.example.com/attack", "", false},
		{"跨站 仅靠 Referer 兜底", "127.0.0.1:8080", "", "http://evil.example.com/attack", "", false},
		{"跨站 同端口不同主机", "127.0.0.1:8080", "http://192.168.1.99:8080", "", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/settings", nil)
			req.Host = c.host
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			if c.referer != "" {
				req.Header.Set("Referer", c.referer)
			}
			if c.forwarded != "" {
				req.Header.Set("X-Forwarded-Host", c.forwarded)
			}
			if got := sameOrigin(req); got != c.want {
				t.Errorf("sameOrigin() = %v, 期望 %v（Host=%q Origin=%q Referer=%q XFH=%q）",
					got, c.want, c.host, c.origin, c.referer, c.forwarded)
			}
		})
	}
}

// TestSettingsSaveThroughReverseProxy 端到端锁定本次故障：
// 请求经反向代理到达，r.Host 是后端内部地址，浏览器 Origin 是外部域名，
// 代理用 X-Forwarded-Host 回传原始主机。修复前此用例必然 403。
func TestSettingsSaveThroughReverseProxy(t *testing.T) {
	dir := t.TempDir()
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.ConfigDir = dir })

	body := url.Values{"api_base_url": {"http://upstream.internal:8999"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://nas.example.com:5000")
	req.Header.Set("Referer", "http://nas.example.com:5000/settings")
	req.Header.Set("X-Forwarded-Host", "nas.example.com:5000")
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Host = "127.0.0.1:8080" // 代理转发后后端看到的 Host

	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("反代场景保存状态码 = %d, 期望 303\n响应正文：%s", rec.Code, rec.Body.String())
	}
}

// TestOriginCheckEscapeHatch 逃生舱：极端部署下可显式关闭来源校验。
func TestOriginCheckEscapeHatch(t *testing.T) {
	t.Setenv("AVDB_DISABLE_ORIGIN_CHECK", "1")
	req := httptest.NewRequest(http.MethodPost, "/settings", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Origin", "http://totally-unrelated.example.com")
	if !sameOrigin(req) {
		t.Error("设置 AVDB_DISABLE_ORIGIN_CHECK=1 后仍被来源校验拦截")
	}
}

// TestSettingsSaveSetsCookieForNewToken 覆盖一个很容易踩的坑：
// 用户刚在页面上设置访问口令，紧接着的 303 跳转会被这个新口令拦住，
// 表现为"改完就进不去了"。必须在响应里同步下发 Cookie。
func TestSettingsSaveSetsCookieForNewToken(t *testing.T) {
	const token = "brand-new-token"

	app := newTestApp(t, upstreamWith(upstreamSample))
	rec := doForm(app, "/settings", url.Values{
		"api_base_url": {"http://host:1"},
		"api_key":      {"k"},
		"access_token": {token},
	}, "http://example.com")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303", rec.Code)
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, authCookieName) {
		t.Fatalf("设置口令后应下发 Cookie，实际 %q", setCookie)
	}
	if !strings.Contains(setCookie, token) {
		t.Errorf("Cookie 内容不正确: %q", setCookie)
	}

	// 用这个 Cookie 应当可以直接访问设置页。
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: token})
	rr := httptest.NewRecorder()
	app.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("用新 Cookie 访问状态码 = %d, 期望 200", rr.Code)
	}
}

// TestSettingsPageSizeWhitelist 守住每页条数的白名单与"0 = 全部"这条特殊取值。
//
// 三个容易写错的点，逐个钉住：
//   - 0 是合法值（全部显示），不能被"<=0 就回默认值"的写法吃掉；
//   - 白名单外的值（99999 / 负数）必须被忽略，否则 ?page_size=99999
//     会让结果页一次渲染十万行；
//   - 非数字输入保留原值，而不是静默重置。
func TestSettingsPageSizeWhitelist(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.PageSize = 100 })

	cases := []struct {
		name  string
		input string
		want  int
	}{
		{"白名单内的值生效", "300", 300},
		{"0 表示全部并保留", "0", 0},
		{"超界值被忽略", "99999", 100},
		{"负数被忽略", "-3", 100},
		{"非数字保留原值", "abc", 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app.applyConfig(normalizeConfig(Config{
				APIBaseURL: "http://host:1",
				ConfigDir:  t.TempDir(),
				PageSize:   100,
			}))

			rec := doForm(app, "/settings", url.Values{
				"api_base_url": {"http://host:1"},
				"page_size":    {tc.input},
			}, "http://example.com")
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("状态码 = %d, 期望 303", rec.Code)
			}
			if got := app.currentConfig().PageSize; got != tc.want {
				t.Errorf("page_size=%q → PageSize = %d，期望 %d", tc.input, got, tc.want)
			}
		})
	}
}

// TestSettingsIgnoresRetiredFields 确认已下线的表单项无法再影响配置。
//
// 「请求超时」「结果上限」两个输入框已按用户要求移除。如果有人只删了 HTML
// 而没删处理器里的赋值，外部调用方仍能通过 POST 把它们改掉——
// 页面上看不到、行为却还在变，是最难查的一类偏差。
func TestSettingsIgnoresRetiredFields(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) {
		c.TimeoutSec = 20
		c.MaxResults = 100
	})

	rec := doForm(app, "/settings", url.Values{
		"api_base_url":    {"http://host:1"},
		"timeout_seconds": {"99999"},
		"max_results":     {"1e9"},
	}, "http://example.com")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303", rec.Code)
	}
	cfg := app.currentConfig()
	if cfg.TimeoutSec != 20 {
		t.Errorf("已下线的「请求超时」仍被表单改写，实际 %d", cfg.TimeoutSec)
	}
	if cfg.MaxResults != 100 {
		t.Errorf("已下线的「结果上限」仍被表单改写，实际 %d", cfg.MaxResults)
	}
}

func TestSettingsTestEndpoint(t *testing.T) {
	t.Run("地址可用", func(t *testing.T) {
		app := newTestApp(t, upstreamWith(`{"code":0,"message":"ok","data":[]}`))
		rec := doForm(app, "/api/settings/test", url.Values{
			"api_base_url": {app.currentConfig().APIBaseURL},
			"api_key":      {"k"},
		}, "http://example.com")

		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d, 期望 200", rec.Code)
		}
		var resp downloadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("响应不是合法 JSON: %v", err)
		}
		if !resp.Success {
			t.Errorf("期望测试成功，实际 message=%q", resp.Message)
		}
	})

	t.Run("鉴权失败", func(t *testing.T) {
		app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"invalid api key"}`)
		})
		rec := doForm(app, "/api/settings/test", url.Values{
			"api_base_url": {app.currentConfig().APIBaseURL},
			"api_key":      {"wrong"},
		}, "http://example.com")

		var resp downloadResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("响应不是合法 JSON: %v", err)
		}
		if resp.Success {
			t.Error("鉴权失败时应返回 success=false")
		}
		if !strings.Contains(resp.Message, "鉴权失败") {
			t.Errorf("提示不明确: %q", resp.Message)
		}
	})

	t.Run("地址非法", func(t *testing.T) {
		app := newTestApp(t, upstreamWith(upstreamSample))
		rec := doForm(app, "/api/settings/test", url.Values{
			"api_base_url": {"不是地址"},
		}, "http://example.com")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("状态码 = %d, 期望 400", rec.Code)
		}
	})

	t.Run("拒绝跨站", func(t *testing.T) {
		app := newTestApp(t, upstreamWith(upstreamSample))
		rec := doForm(app, "/api/settings/test", url.Values{
			"api_base_url": {"http://host:1"},
		}, "http://evil.example.com")
		if rec.Code != http.StatusForbidden {
			t.Errorf("状态码 = %d, 期望 403", rec.Code)
		}
	})

	t.Run("测试不改动已保存配置", func(t *testing.T) {
		app := newTestApp(t, upstreamWith(upstreamSample))
		before := app.currentConfig().APIBaseURL
		// 用 127.0.0.1:1 而不是不存在的域名：连不上会立刻返回 connection refused，
		// 不必白等 DNS 解析超时。
		doForm(app, "/api/settings/test", url.Values{
			"api_base_url": {"http://127.0.0.1:1"},
		}, "http://example.com")
		if after := app.currentConfig().APIBaseURL; after != before {
			t.Errorf("测试连接不应修改生效配置：%q -> %q", before, after)
		}
	})
}

func TestSettingsBehindAuth(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) {
		c.AccessToken = "s3cret"
	})

	if rec := do(app, http.MethodGet, "/settings"); rec.Code != http.StatusUnauthorized {
		t.Errorf("未带口令访问设置页状态码 = %d, 期望 401", rec.Code)
	}

	rec := doForm(app, "/settings", url.Values{"api_base_url": {"http://x:1"}}, "http://example.com")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("未带口令提交设置状态码 = %d, 期望 401", rec.Code)
	}
}

func TestUnknownRouteReturns404(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	if rec := do(app, http.MethodGet, "/definitely-not-here"); rec.Code != http.StatusNotFound {
		t.Errorf("未知路径状态码 = %d, 期望 404", rec.Code)
	}
}
