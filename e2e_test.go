package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 严格上游：复刻真实 Avdb 的下载接口契约
//
// 线上报过的错：
//
//	上游参数校验失败（422）（HTTP 422）：
//	{"detail":[{"type":"missing","loc":["query","save_path"],"msg":"Field required","input":null}]}
//
// 也就是说官方文档写"选填"的 downloader / save_path，实际是**必填** query 参数。
// 这个假上游故意照抄这份严格契约，任何参数缺失都返回 422 —— 只要客户端退回
// "非空才发送"的旧写法，这一组测试就会立刻失败。
// ---------------------------------------------------------------------------

// downloadRequiredParams 是下载接口**必填**的 query 参数。
//
// downloader 不在其列：官方文档标它是"选填"，实测也确实如此——缺了它不会 422。
// 它的坑在别处：传空串会被上游当成一个名叫"空"的下载器，回 `未找到下载器`。
// 所以正确姿势是"有值才带"，见 avdb.go 的 SubmitDownload。
var downloadRequiredParams = []string{"tid", "save_path"}

type recordedRequest struct {
	Method string
	Path   string
	Query  url.Values
	APIKey string
}

type strictUpstream struct {
	mu       sync.Mutex
	recorded []recordedRequest
}

func (s *strictUpstream) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ParseForm()
	s.recorded = append(s.recorded, recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.Query(),
		APIKey: r.Header.Get("X-API-Key"),
	})
}

func (s *strictUpstream) last(t *testing.T) recordedRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.recorded) == 0 {
		t.Fatal("上游没有收到任何请求")
	}
	return s.recorded[len(s.recorded)-1]
}

func upstreamWriteJSON(w http.ResponseWriter, status int, payload any) {
	body, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *strictUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.record(r)

	if r.Header.Get("X-API-Key") != "e2e-key" {
		upstreamWriteJSON(w, http.StatusUnauthorized, map[string]any{"message": "invalid api key"})
		return
	}

	switch r.URL.Path {
	case "/api/v1/articles/torrents":
		upstreamWriteJSON(w, http.StatusOK, map[string]any{
			"code": 0, "message": "操作成功", "data": e2eTorrents,
		})

	case "/api/v1/articles/download/manul":
		// 严格校验：必填参数一个都不能少（这正是线上 422 的来源）。
		var missing []map[string]any
		for _, param := range downloadRequiredParams {
			if _, ok := r.URL.Query()[param]; !ok {
				missing = append(missing, map[string]any{
					"type": "missing", "loc": []any{"query", param},
					"msg": "Field required", "input": nil,
				})
			}
		}
		if len(missing) > 0 {
			upstreamWriteJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": missing})
			return
		}
		// 复刻真实上游对"空标识"的处理：传了就要能查到，传空串一律视为
		// "名为空的下载器"而拒绝。这条断言是本次修复的核心——
		// 若客户端把"留空"实现成"发空串"，这里必须炸。
		if v, present := r.URL.Query()["downloader"]; present {
			if strings.TrimSpace(v[0]) == "" {
				upstreamWriteJSON(w, http.StatusOK, map[string]any{
					"code": 1, "message": "未找到下载器: ",
				})
				return
			}
		}
		upstreamWriteJSON(w, http.StatusOK, map[string]any{
			"code": 0, "message": "已提交到下载器", "data": map[string]any{},
		})

	default:
		upstreamWriteJSON(w, http.StatusNotFound, map[string]any{"detail": "Not Found"})
	}
}

// e2eTorrents 刻意带上两个已知的真实边界：id 超出 int32、category 为 null。
var e2eTorrents = []map[string]any{
	{
		"id": 10080460000, "number": "TEST-001", "site": "SampleSiteA",
		"section": "Section-One", "category": nil, "size_mb": 2048.5,
		"post_time": "2026-08-14 20:13:14", "seeders": 37,
		"title": "Sample Item Alpha", "download_url": "magnet:?xt=urn:btih:0123456789abcdef",
		"preview_image": "https://cdn.example.com/alpha.jpg",
		"free":          true, "chinese": true, "hd": true, "uhd": true,
	},
	{
		"id": 3691410, "number": "TEST-002", "site": "SampleSiteB",
		"section": "Section-Two", "category": "General", "size_mb": 512.0,
		"post_time": "2026-08-13 09:00:00", "seeders": 0,
		"title": "Sample Item Beta", "download_url": "magnet:?xt=urn:btih:fedcba9876543210",
		"preview_image": "", "uncensored": true, "mosaic": true,
	},
}

func newStrictUpstream(t *testing.T) (*httptest.Server, *strictUpstream) {
	t.Helper()
	up := &strictUpstream{}
	server := httptest.NewServer(up)
	t.Cleanup(server.Close)
	return server, up
}

// TestStrictUpstreamReproduces422 先证明这个假上游是"忠实的"：
// 缺 save_path 真的会 422。否则后面的测试可能因为假上游太宽松而形同虚设。
func TestStrictUpstreamReproduces422(t *testing.T) {
	server, _ := newStrictUpstream(t)

	get := func(query string) int {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/articles/download/manul?"+query, nil)
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		req.Header.Set("X-API-Key", "e2e-key")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("请求假上游失败: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if got := get("tid=3691410"); got != http.StatusUnprocessableEntity {
		t.Fatalf("只传 tid 应复现 422，实际 %d（假上游不够严格，本组测试失去意义）", got)
	}
	if got := get("tid=3691410&downloader=115"); got != http.StatusUnprocessableEntity {
		t.Fatalf("缺 save_path 应复现 422，实际 %d", got)
	}
	if got := get("tid=3691410&downloader=&save_path="); got != http.StatusOK {
		t.Fatalf("参数齐全（值为空）应 200，实际 %d", got)
	}
}

// TestEndToEndDownloadAgainstStrictUpstream 是本次修复的端到端验收：
// 用"什么都没填"的配置（最常见的部署形态）走完整链路，
// 必须成功，且**不能**把 downloader 以空值形式发给上游。
func TestEndToEndDownloadAgainstStrictUpstream(t *testing.T) {
	server, up := newStrictUpstream(t)

	// 刻意不配置 Downloader / SavePath —— 这正是触发线上 422 的场景。
	app := NewApp(Config{
		APIBaseURL: server.URL,
		APIKey:     "e2e-key",
		Addr:       ":0",
		Timeout:    5 * time.Second,
		TimeoutSec: 5,
		MaxResults: 100,
		ConfigDir:  t.TempDir(),
	}, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/download", strings.NewReader("tid=3691410"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com") // 与 httptest 默认 Host 一致
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200；响应: %s", rec.Code, rec.Body.String())
	}

	var resp downloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !resp.Success {
		t.Fatalf("提交下载应成功，实际 message=%q", resp.Message)
	}

	// 上游侧必须收到必填参数——这是 422 不再复发的直接证据。
	got := up.last(t)
	if got.Path != "/api/v1/articles/download/manul" {
		t.Errorf("上游路径 = %q", got.Path)
	}
	if got.Method != http.MethodGet {
		t.Errorf("调用上游应使用 GET，实际 %s", got.Method)
	}
	for _, param := range downloadRequiredParams {
		if _, ok := got.Query[param]; !ok {
			t.Errorf("上游未收到参数 %s（会返回 422）", param)
		}
	}
	if got.Query.Get("tid") != "3691410" {
		t.Errorf("tid = %q", got.Query.Get("tid"))
	}

	// 本次修复的核心断言：没配下载器时，这个参数必须**整个不存在**。
	//
	// 不能用 Get() == "" 来判断——那对"没有该键"和"键存在但值为空"是一样的，
	// 而这两种情形在上游眼里天差地别：前者继承全局下载器，后者会去查一个
	// 名叫"空"的下载器然后报 `未找到下载器`。假上游已经复刻了这个行为，
	// 所以只要客户端退化成发空串，这里就会失败。
	if values, present := got.Query["downloader"]; present {
		t.Errorf("未配置下载器时不应发送 downloader 参数，实际发了 %q（上游会回『未找到下载器』）", values)
	}
	// save_path 不同：留空是合法的，但必须**发送**（上游判它必填，空值也发）。
	if values, present := got.Query["save_path"]; !present {
		t.Error("save_path 必须发送（上游判为必填），即使值为空")
	} else if values[0] != "" {
		t.Errorf("未配置保存路径时应传空值，实际 %q", values[0])
	}
	if got.APIKey != "e2e-key" {
		t.Errorf("上游未收到 X-API-Key，实际 %q", got.APIKey)
	}
}

// TestEndToEndDownloadPassesConfiguredValues 确认"配了就用配的"，
// 不能因为修 422 就把用户填的值丢掉。
func TestEndToEndDownloadPassesConfiguredValues(t *testing.T) {
	server, up := newStrictUpstream(t)

	app := NewApp(Config{
		APIBaseURL: server.URL,
		APIKey:     "e2e-key",
		Downloader: "my-downloader",
		SavePath:   "/media/movies",
		Addr:       ":0",
		Timeout:    5 * time.Second,
		TimeoutSec: 5,
		MaxResults: 100,
		ConfigDir:  t.TempDir(),
	}, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/download", strings.NewReader("tid=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200；响应: %s", rec.Code, rec.Body.String())
	}

	got := up.last(t)
	if got.Query.Get("downloader") != "my-downloader" {
		t.Errorf("downloader 未透传: %q", got.Query.Get("downloader"))
	}
	if got.Query.Get("save_path") != "/media/movies" {
		t.Errorf("save_path 未透传: %q", got.Query.Get("save_path"))
	}
}

// TestEndToEndSearchShowsUpstream422AsReadableMessage 验证上游报错会被翻译成人话，
// 而不是把原始 JSON 甩到界面上。
func TestEndToEndSearchShowsUpstream422AsReadableMessage(t *testing.T) {
	// 让下载接口在"参数齐全"的情况下依然报 422，模拟上游契约再次漂移。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamWriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"detail": []any{map[string]any{
				"type": "missing", "loc": []any{"query", "save_path"},
				"msg": "Field required", "input": nil,
			}},
		})
	}))
	t.Cleanup(server.Close)

	client := NewAvdbClient(Config{APIBaseURL: server.URL, APIKey: "k", Timeout: 5 * time.Second})
	_, _, err := client.SubmitDownload(context.Background(), "1", "", "")
	if err == nil {
		t.Fatal("上游 422 时应返回错误")
	}

	msg := err.Error()
	if !strings.Contains(msg, "参数校验失败") {
		t.Errorf("应保留状态码语义，实际 %q", msg)
	}
	if !strings.Contains(msg, "save_path") {
		t.Errorf("应指明缺失的参数，实际 %q", msg)
	}
	if strings.Contains(msg, `"detail"`) || strings.Contains(msg, `"loc"`) {
		t.Errorf("不应把原始 JSON 甩给用户，实际 %q", msg)
	}
}

// TestEndToEndSearchRendersList 走一遍完整的搜索链路，确认列表形态与数据正确。
func TestEndToEndSearchRendersList(t *testing.T) {
	server, _ := newStrictUpstream(t)

	app := NewApp(Config{
		APIBaseURL: server.URL,
		APIKey:     "e2e-key",
		Addr:       ":0",
		Timeout:    5 * time.Second,
		TimeoutSec: 5,
		MaxResults: 100,
		ConfigDir:  t.TempDir(),
	}, discardLogger())

	rec := do(app, http.MethodGet, "/s?q=TEST")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, `id="result-count">2<`) {
		t.Errorf("应渲染 2 条结果；片段: %s", excerpt(body, `id="result-count"`))
	}
	if n := strings.Count(body, `class="row"`); n != 2 {
		t.Errorf("列表项数量 = %d, 期望 2", n)
	}
	if strings.Contains(body, "<img") {
		t.Error("列表不应加载任何图片")
	}
	// 站点名归一化后应出现在筛选项里（两条数据来自不同站点）。
	for _, site := range []string{"SampleSiteA", "SampleSiteB"} {
		if !strings.Contains(body, `<option value="`+site+`">`) {
			t.Errorf("筛选下拉缺少站点 %s", site)
		}
	}
	// 不可信输入必须转义：标题里没有 HTML，但容器结构必须由模板给出而非拼接。
	if strings.Contains(body, "<script>alert") {
		t.Error("页面出现可疑脚本注入痕迹")
	}
}
