package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 上游「配置类」只读接口的单元测试：读取默认下载目标、列出目录、防御式解析。
//
// 这一组测试守护的是一条安全红线：
//   上游配置响应里可能有网盘 Cookie、账号令牌等凭据，收集目录时
//   必须**只按白名单字段名取值**（TestHarvestStringsOnlyTakesWhitelistedKeys）。
//   一旦有人图省事改成"取遍所有字符串"，那条测试会立刻失败。
//
// 本站**不需要登录**：所有用到的接口在文档里都标注为 `API Key/JWT`，
// 访问令牌即可调用。这是与旧版脚本最大的区别——旧脚本只能手改 config.json。
// ---------------------------------------------------------------------------

// configUpstream 是"路由表驱动"的假上游，用来复刻配置类接口。
// 与 e2e_test.go 的 strictUpstream 区别：那个只管下载契约，这个管配置读取。
type configUpstream struct {
	routes    map[string]http.HandlerFunc
	lastForm  url.Values
	lastAuth  string
	lastPath  string
	lastQuery url.Values
}

func (c *configUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	c.lastForm = r.PostForm
	c.lastAuth = r.Header.Get("Authorization")
	c.lastPath = r.URL.Path
	c.lastQuery = r.URL.Query()

	if handler, ok := c.routes[r.Method+" "+r.URL.Path]; ok {
		handler(w, r)
		return
	}
	upstreamWriteJSON(w, http.StatusNotFound, map[string]any{"detail": "Not Found"})
}

// newConfigUpstream 起一个假上游并返回与之绑定的客户端。
func newConfigUpstream(t *testing.T, routes map[string]http.HandlerFunc) (*configUpstream, *AvdbClient) {
	t.Helper()
	up := &configUpstream{routes: routes}
	server := httptest.NewServer(up)
	t.Cleanup(server.Close)

	client := NewAvdbClient(Config{APIBaseURL: server.URL, APIKey: "test-key", Timeout: 5 * time.Second})
	return up, client
}

// ------------------------------------------------------- 读取默认下载目标

func TestFetchDefaultRuleReadsDownloaderAndSavePath(t *testing.T) {
	up, client := newConfigUpstream(t, map[string]http.HandlerFunc{
		"GET /api/v1/javdb/subscriptions/default-rule": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0, "message": "操作成功",
				"data": map[string]any{
					"downloader":      "115",
					"save_path":       "/media/115/movies",
					"save_path_label": "电影",
				},
			})
		},
	})

	rule, err := client.FetchDefaultRule(context.Background())
	if err != nil {
		t.Fatalf("读取默认规则应成功，实际 %v", err)
	}
	if rule.Downloader != "115" {
		t.Errorf("Downloader = %q，期望 115", rule.Downloader)
	}
	if rule.SavePath != "/media/115/movies" {
		t.Errorf("SavePath = %q", rule.SavePath)
	}
	if rule.SavePathLabel != "电影" {
		t.Errorf("SavePathLabel = %q", rule.SavePathLabel)
	}
	// 这条最关键：必须只靠访问令牌，不能带 Bearer。
	if up.lastAuth != "" {
		t.Errorf("不应发送 Authorization 头（本站不使用登录 JWT），实际 %q", up.lastAuth)
	}
	if up.lastPath != defaultRulePath {
		t.Errorf("请求路径 = %q，期望 %q", up.lastPath, defaultRulePath)
	}
}

// TestFetchDefaultRuleReportsStructureChangeInsteadOfSilentEmpty 是本文件里
// 最值得保留的一条断言。
//
// 当上游把响应结构改掉、我们一个认识的字段都解不出来时，有两种写法：
//   - 静默返回零值 → 界面显示"上游没有配置默认下载目标"；
//   - 报错并附上响应原文 → 界面显示"接口结构可能已变化：{...}"。
//
// 两者对用户的信息量天差地别：前者的结论是**错的**，会让人去上游翻配置、
// 怀疑自己没配默认下载器，而真实原因是本程序读不懂新格式。
// 把"我们没读懂"伪装成"上游没有"是最难排查的一类误导，因此必须报错。
func TestFetchDefaultRuleReportsStructureChangeInsteadOfSilentEmpty(t *testing.T) {
	_, client := newConfigUpstream(t, map[string]http.HandlerFunc{
		"GET /api/v1/javdb/subscriptions/default-rule": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": map[string]any{"target": map[string]any{"id": "115"}},
			})
		},
	})

	rule, err := client.FetchDefaultRule(context.Background())
	if err == nil {
		t.Fatalf("结构不认识时必须报错，不能静默返回空规则（实际返回 %+v）", rule)
	}
	if !strings.Contains(err.Error(), "结构可能已变化") {
		t.Errorf("错误信息应点明是结构问题，实际 %q", err.Error())
	}
}

func TestFetchDefaultRuleSurfacesUpstreamMessage(t *testing.T) {
	// 上游用业务错误说明拒绝时，原话比通用文案有用。
	_, client := newConfigUpstream(t, map[string]http.HandlerFunc{
		"GET /api/v1/javdb/subscriptions/default-rule": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusForbidden, map[string]any{
				"code": 1, "message": "访问令牌无权读取该配置",
			})
		},
	})

	_, err := client.FetchDefaultRule(context.Background())
	if err == nil {
		t.Fatal("上游拒绝时应返回错误")
	}
	if !strings.Contains(err.Error(), "访问令牌无权读取该配置") {
		t.Errorf("应透传上游原话，实际 %q", err.Error())
	}
}

func TestFetchDefaultRuleRejectsNonJSON(t *testing.T) {
	// 反向代理返回 HTML 错误页是常见故障，必须给出可读提示而不是解析报错原文。
	_, client := newConfigUpstream(t, map[string]http.HandlerFunc{
		"GET /api/v1/javdb/subscriptions/default-rule": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
		},
	})

	_, err := client.FetchDefaultRule(context.Background())
	if err == nil {
		t.Fatal("非 JSON 响应应返回错误")
	}
	if !strings.Contains(err.Error(), "不是 JSON") {
		t.Errorf("应指出响应不是 JSON，实际 %q", err.Error())
	}
}

// TestFetchDefaultRuleUnwrapsDataEnvelope 确认 {code,message,data} 外壳被剥掉。
// 上游有的接口带外壳、有的不带，两种都要能解。
func TestFetchDefaultRuleUnwrapsDataEnvelope(t *testing.T) {
	_, client := newConfigUpstream(t, map[string]http.HandlerFunc{
		"GET /api/v1/javdb/subscriptions/default-rule": func(w http.ResponseWriter, r *http.Request) {
			// 注意 data 里还嵌了一层，且顶层没有 downloader 字段。
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0,
				"data": map[string]any{"downloader": "qb", "save_path": "/downloads"},
			})
		},
	})

	rule, err := client.FetchDefaultRule(context.Background())
	if err != nil {
		t.Fatalf("应剥掉外壳后成功，实际 %v", err)
	}
	if rule.Downloader != "qb" || rule.SavePath != "/downloads" {
		t.Errorf("规则 = %+v", rule)
	}
}

// ------------------------------------------------------------ 目录列举

func TestDownloaderDirectoriesParsesStringArray(t *testing.T) {
	up, client := newConfigUpstream(t, map[string]http.HandlerFunc{
		"GET /api/v1/config/downloader/directories": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": []string{"/media/a", "/media/b"},
			})
		},
	})

	dirs, err := client.DownloaderDirectories(context.Background(), "115")
	if err != nil {
		t.Fatalf("应成功，实际 %v", err)
	}
	if len(dirs) != 2 || dirs[0] != "/media/a" || dirs[1] != "/media/b" {
		t.Errorf("目录 = %v", dirs)
	}
	if got := up.lastPath; got != "/api/v1/config/downloader/directories" {
		t.Errorf("请求路径 = %q", got)
	}
	if got := up.lastQuery.Get("downloader_id"); got != "115" {
		t.Errorf("downloader_id = %q，期望 115", got)
	}
}

func TestDownloaderDirectoriesParsesObjectArray(t *testing.T) {
	_, client := newConfigUpstream(t, map[string]http.HandlerFunc{
		"GET /api/v1/config/downloader/directories": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0,
				"data": []map[string]any{
					{"path": "/media/movies"},
					{"path": "/media/tv", "name": "剧集"},
				},
			})
		},
	})

	dirs, err := client.DownloaderDirectories(context.Background(), "115")
	if err != nil {
		t.Fatalf("应成功，实际 %v", err)
	}
	if len(dirs) != 2 || dirs[0] != "/media/movies" || dirs[1] != "/media/tv" {
		t.Errorf("目录 = %v", dirs)
	}
}

func TestDownloaderDirectoriesFallsBackToNameWhenNoPathField(t *testing.T) {
	// 兼容性兜底：有些上游的目录条目只给显示名，没有 path 字段。
	// 注意同一批数据里若**同时**有 path 与 name，name 不能被当成路径
	// （见上一个用例），否则会凭空多出一条假目录。
	_, client := newConfigUpstream(t, map[string]http.HandlerFunc{
		"GET /api/v1/config/downloader/directories": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0,
				"data": []map[string]any{{"name": "/media/only-name"}},
			})
		},
	})

	dirs, err := client.DownloaderDirectories(context.Background(), "115")
	if err != nil {
		t.Fatalf("应成功，实际 %v", err)
	}
	if len(dirs) != 1 || dirs[0] != "/media/only-name" {
		t.Errorf("目录 = %v", dirs)
	}
}

// TestDownloaderDirectoriesPrefersUpstreamBusinessMessage 确认失败时优先用上游的业务说明。
//
// 上游对不存在的标识返回 404 + `未找到下载器: xxx`。若照搬状态码含义，
// 用户会看到"上游接口不存在，请确认 Avdb 版本是否匹配"——把他引到完全错误的方向。
func TestDownloaderDirectoriesPrefersUpstreamBusinessMessage(t *testing.T) {
	_, client := newConfigUpstream(t, map[string]http.HandlerFunc{
		"GET /api/v1/config/downloader/directories": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusNotFound, map[string]any{
				"code": 1, "message": "未找到下载器: bogus",
			})
		},
	})

	_, err := client.DownloaderDirectories(context.Background(), "bogus")
	if err == nil {
		t.Fatal("标识不存在时应返回错误")
	}
	if !strings.Contains(err.Error(), "未找到下载器") {
		t.Errorf("应采用上游的业务说明，实际 %q", err.Error())
	}
	if strings.Contains(err.Error(), "接口不存在") {
		t.Errorf("不应退回通用的状态码描述，实际 %q", err.Error())
	}
}

func TestDownloaderDirectoriesRejectsEmptyID(t *testing.T) {
	_, client := newConfigUpstream(t, nil)

	if _, err := client.DownloaderDirectories(context.Background(), "  "); err == nil {
		t.Error("空下载器标识应报错，而不是发起请求")
	}
}

// --------------------------------------------------- 防御式解析（安全红线）

// TestHarvestStringsOnlyTakesWhitelistedKeys 是安全回归测试。
//
// 上游的配置响应里可能混有网盘 Cookie、账号令牌等凭据。收集目录时
// 必须**只按白名单字段名取值**；一旦有人图省事改成"取遍所有字符串"，
// 这条测试会立刻失败，从而挡住凭据泄露到前端。
func TestHarvestStringsOnlyTakesWhitelistedKeys(t *testing.T) {
	payload := []byte(`{
		"code": 0,
		"data": {
			"cookie": "SECRET-COOKIE-VALUE",
			"account": "SECRET-ACCOUNT",
			"directories": ["/media/a", "/media/b"]
		}
	}`)

	dirs := harvestStrings(payload)
	joined := strings.Join(dirs, ",")
	if !strings.Contains(joined, "/media/a") {
		t.Errorf("应收集到目录，实际 %v", dirs)
	}
	for _, secret := range []string{"SECRET-COOKIE-VALUE", "SECRET-ACCOUNT"} {
		if strings.Contains(joined, secret) {
			t.Errorf("把凭据 %q 收集进来了——这是严重的信息泄露", secret)
		}
	}
}

// TestHarvestStringsIgnoresErrorMessagePayload 挡住"把报错文案当成目录"。
//
// harvestStrings 会扫过整个响应体（不只 data 段），所以上游的 code / message /
// detail 这些字段也在它的视野里。它们必须在白名单之外——否则
// `{"code":1,"message":"未找到下载器: bogus"}` 会被解析出一条假目录，
// 界面上就变成"下载器 bogus 可用，读到 1 个目录"，把失败伪装成成功。
func TestHarvestStringsIgnoresErrorMessagePayload(t *testing.T) {
	for _, body := range []string{
		`{"code":1,"message":"未找到下载器: bogus"}`,
		`{"detail":"Not Found"}`,
		`{"code":0,"message":"操作成功"}`,
	} {
		if dirs := harvestStrings([]byte(body)); len(dirs) != 0 {
			t.Errorf("报错/提示载荷不应产出目录，实际 %+v（来源 %s）", dirs, body)
		}
	}
}

// TestHarvestStringsTreatsNameAsDirectoryOnlyWithoutPathField 固定一条
// "看起来像 bug、其实是刻意设计"的行为，免得后人误改成 bug。
//
// 上游的目录条目有两种形态：{"path":"/media/tv","name":"剧集"} 和
// {"name":"/media/tv"}。前者必须只取 path，否则"剧集"会变成一条假路径；
// 后者没有任何 path 字段，此时 name 就是唯一可用的值，必须兜住。
// 因此单独一个 {"name": "..."} 对象会被收下——这是兼容性，不是漏洞。
func TestHarvestStringsTreatsNameAsDirectoryOnlyWithoutPathField(t *testing.T) {
	if dirs := harvestStrings([]byte(`{"name":"/media/only-name"}`)); len(dirs) != 1 || dirs[0] != "/media/only-name" {
		t.Errorf("无 path 字段时应退回 name，实际 %+v", dirs)
	}
	// 同时存在时，name 不得混进来。
	dirs := harvestStrings([]byte(`{"path":"/media/tv","name":"剧集"}`))
	if len(dirs) != 1 || dirs[0] != "/media/tv" {
		t.Errorf("有 path 时不应把 name 当成路径，实际 %+v", dirs)
	}
}

func TestHarvestStringsDedupesAndCapsLength(t *testing.T) {
	// 上限的意义：异常上游返回几万条目录时不能把页面撑爆。
	var sb strings.Builder
	sb.WriteString(`[`)
	for i := 0; i < maxHarvested*2; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "%q", fmt.Sprintf("/media/dir-%d", i))
	}
	sb.WriteString(`, "/media/dir-0"]`) // 末尾重复一条，验证去重

	dirs := harvestStrings([]byte(sb.String()))
	if len(dirs) != maxHarvested {
		t.Errorf("应收敛到上限 %d 条，实际 %d", maxHarvested, len(dirs))
	}
	seen := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		if seen[d] {
			t.Errorf("出现重复目录 %q", d)
			break
		}
		seen[d] = true
	}
}

// ------------------------------------------------------- 设置页处理器

func TestSettingsDefaultRuleRejectsCrossSite(t *testing.T) {
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("跨站请求不应到达上游")
	})

	rec := doForm(app, "/api/settings/default-rule", url.Values{}, "http://evil.example.com")
	if rec.Code != http.StatusForbidden {
		t.Errorf("状态码 = %d，期望 403", rec.Code)
	}
}

func TestSettingsDefaultRuleReturnsFieldsForForm(t *testing.T) {
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != defaultRulePath {
			upstreamWriteJSON(w, http.StatusNotFound, map[string]any{"detail": "Not Found"})
			return
		}
		upstreamWriteJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"downloader":      "qb",
				"save_path":       "/downloads/tv",
				"save_path_label": "剧集",
			},
		})
	})

	rec := doForm(app, "/api/settings/default-rule",
		url.Values{"api_base_url": {app.currentConfig().APIBaseURL}}, "http://example.com")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 %s", rec.Code, rec.Body.String())
	}
	var resp settingsProbeResponse
	if err := decodeJSON(t, rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Success {
		t.Fatalf("应成功，实际 %+v", resp)
	}
	// 页面的 js 就是靠这三个字段回填输入框的，字段名不能改。
	if resp.Downloader != "qb" || resp.SavePath != "/downloads/tv" || resp.SavePathLabel != "剧集" {
		t.Errorf("响应字段不完整: %+v", resp)
	}
	if !strings.Contains(resp.Message, "已填入") {
		t.Errorf("应提示用户已回填，实际 %q", resp.Message)
	}
	// 读到的值**不落盘**：它只是给用户看一眼，要不要固化由用户点保存决定。
	if got := app.currentConfig().Downloader; got == "qb" {
		t.Error("读取默认下载器时不应直接改写已保存的配置")
	}
}

func TestSettingsDefaultRuleReportsEmptyUpstreamConfig(t *testing.T) {
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamWriteJSON(w, http.StatusOK, map[string]any{
			"code": 0, "data": map[string]any{"downloader": "", "save_path": ""},
		})
	})

	rec := doForm(app, "/api/settings/default-rule",
		url.Values{"api_base_url": {app.currentConfig().APIBaseURL}}, "http://example.com")

	var resp settingsProbeResponse
	if err := decodeJSON(t, rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Success {
		t.Errorf("上游没配默认目标时不应报成功（会误导用户以为读到了值）: %+v", resp)
	}
	if !strings.Contains(resp.Message, "没有配置默认下载器") {
		t.Errorf("应说明上游没有配置默认下载器，实际 %q", resp.Message)
	}
}

// TestSettingsDefaultRuleReportsMissingDownloader 守住一个曾经真实存在的缺陷。
//
// 上游完全可能只配了默认目录、没配默认下载器。早先这里用"三个字段全空"判空，
// 于是这种情况下会回一句「上游默认下载器：，默认目录：/xxx」并标 success ——
// 前半句是空的，用户以为下载器已经填好了，结果下载还是失败。
// 本接口存在的**唯一目的**就是拿到那个标识，所以必须以 Downloader 是否为空分岔。
func TestSettingsDefaultRuleReportsMissingDownloader(t *testing.T) {
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamWriteJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{
				"downloader": "", "save_path": "/downloads/only-dir",
			},
		})
	})

	rec := doForm(app, "/api/settings/default-rule",
		url.Values{"api_base_url": {app.currentConfig().APIBaseURL}}, "http://example.com")

	var resp settingsProbeResponse
	if err := decodeJSON(t, rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Success {
		t.Errorf("没有下载器标识时不能报成功: %+v", resp)
	}
	if strings.Contains(resp.Message, "上游默认下载器：") {
		t.Errorf("不得渲染出'上游默认下载器：'后面空无一物的句子，实际 %q", resp.Message)
	}
	if !strings.Contains(resp.Message, "没有配置默认下载器") {
		t.Errorf("应点明缺的是下载器，实际 %q", resp.Message)
	}
	// 目录是有用的信息，不该因为下载器缺失就丢掉。
	if resp.SavePath != "/downloads/only-dir" {
		t.Errorf("应仍带回默认目录供用户使用，实际 %q", resp.SavePath)
	}
}

func TestSettingsDirectoriesRejectsCrossSite(t *testing.T) {
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("跨站请求不应到达上游")
	})

	rec := doForm(app, "/api/settings/directories",
		url.Values{"downloader_id": {"115"}}, "http://evil.example.com")

	if rec.Code != http.StatusForbidden {
		t.Errorf("状态码 = %d，期望 403", rec.Code)
	}
}

func TestSettingsDirectoriesListsAndCaches(t *testing.T) {
	var gotQuery url.Values
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		upstreamWriteJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{"directories": []string{"/media/a", "/media/b"}},
		})
	})

	rec := doForm(app, "/api/settings/directories", url.Values{
		"api_base_url":  {app.currentConfig().APIBaseURL},
		"downloader_id": {"115"},
	}, "http://example.com")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 %s", rec.Code, rec.Body.String())
	}
	if gotQuery.Get("downloader_id") != "115" {
		t.Errorf("上游未收到 downloader_id=115，实际 %q", gotQuery.Get("downloader_id"))
	}

	var resp settingsProbeResponse
	if err := decodeJSON(t, rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Success || len(resp.Directories) != 2 {
		t.Errorf("响应 = %+v", resp)
	}
	// 目录候选应被缓存，下次打开设置页就能直接给建议。
	if cached := app.currentConfig().SavePathOptions; len(cached) != 2 {
		t.Errorf("目录候选未缓存，实际 %v", cached)
	}
}

// TestSettingsDirectoriesFallsBackToSavedDownloader 确认下载器标识留空时沿用已保存的值。
// 页面上「校验」按钮可能只带目标字段不带标识（用户刚保存过），
// 此时应当用配置里的下载器去校验，而不是立刻报"标识不能为空"。
func TestSettingsDirectoriesFallsBackToSavedDownloader(t *testing.T) {
	var gotQuery url.Values
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		upstreamWriteJSON(w, http.StatusOK, map[string]any{
			"code": 0, "data": []string{"/media/only"},
		})
	})

	rec := doForm(app, "/api/settings/directories",
		url.Values{"api_base_url": {app.currentConfig().APIBaseURL}}, "http://example.com")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 %s", rec.Code, rec.Body.String())
	}
	// 表单没带 downloader_id 时，应沿用已保存的下载器。
	if got := gotQuery.Get("downloader_id"); got != testDownloaderID {
		t.Errorf("应沿用已保存的下载器 %q，实际 %q", testDownloaderID, got)
	}
}

func TestSettingsDirectoriesInheritsSavedTargetWhenFormOmitsIt(t *testing.T) {
	// 探测接口的两个目标字段都必须是"留空即沿用已保存"。
	// 设置页总会把值带上，所以只靠页面测不出这个口径；
	// 一旦退化成"留空即空地址"，直接调接口的调用方会收到
	// 「上游 API 地址为空」这种与事实不符的报错。
	var hits int
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if got := r.Header.Get("X-API-Key"); got == "" {
			t.Errorf("沿用已保存的 API Key 时未带上令牌")
		}
		upstreamWriteJSON(w, http.StatusOK, map[string]any{
			"code": 0,
			"data": map[string]any{"directories": []string{"/media/only"}},
		})
	})

	for name, form := range map[string]url.Values{
		"完全不传目标字段": {"downloader_id": {"115"}},
		"显式传空字符串":  {"api_base_url": {""}, "api_key": {""}, "downloader_id": {"115"}},
	} {
		t.Run(name, func(t *testing.T) {
			hits = 0
			rec := doForm(app, "/api/settings/directories", form, "http://example.com")

			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，响应 %s", rec.Code, rec.Body.String())
			}
			var resp settingsProbeResponse
			if err := decodeJSON(t, rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if !resp.Success {
				t.Fatalf("应沿用已保存的上游地址，实际 %+v", resp)
			}
			if hits != 1 {
				t.Errorf("上游被调用 %d 次，期望 1 次", hits)
			}
		})
	}
}

// ------------------------------------------------------------ 错误人话化

func TestHumanizeDownloadFailureExplainsDownloaderNotFound(t *testing.T) {
	const sent = "clouddrive"

	for _, raw := range []string{
		"未找到下载器: 115",
		"Downloader not found: 115",
		"unknown downloader",
	} {
		got := humanizeDownloadFailure(raw, sent)
		if !strings.Contains(got, "设置") {
			t.Errorf("对 %q 应给出指向设置页的指引，实际 %q", raw, got)
		}
		if !strings.Contains(got, raw) {
			t.Errorf("应保留上游原文便于排查，实际 %q", got)
		}
		// 提示里必须出现**本次真正发出去**的标识，而不是配置里可能为空的原始值——
		// 否则用户会看到"请检查你填的 xxx"，而那句 xxx 他从来没填过。
		if !strings.Contains(got, sent) {
			t.Errorf("应说明实际发送的标识 %q，实际 %q", sent, got)
		}
	}
}

// TestHumanizeDownloadFailureExplainsEmptySavePath 覆盖上游拒绝保存目录的情形。
//
// 上游把 save_path 判为必填且不接受空值，返回的原文只有一句
// 「保存目录不能为空」。这时要告诉用户去哪个页面把路径补上，
// 以及怎么把上游已配置的目录列出来。
func TestHumanizeDownloadFailureExplainsEmptySavePath(t *testing.T) {
	for _, raw := range []string{
		"保存目录不能为空",
		"上游参数校验失败（422）：查询参数 → save_path：Field required",
	} {
		got := humanizeDownloadFailure(raw, "clouddrive")
		if !strings.Contains(got, "保存路径") {
			t.Errorf("对 %q 应指出要填「保存路径」，实际 %q", raw, got)
		}
		if !strings.Contains(got, "目录") {
			t.Errorf("对 %q 应提示可以列出上游目录，实际 %q", raw, got)
		}
		if !strings.Contains(got, raw) {
			t.Errorf("应保留上游原文便于排查，实际 %q", got)
		}
	}
}

func TestHumanizeDownloadFailureLeavesOtherErrorsAlone(t *testing.T) {
	raw := "磁盘空间不足"
	if got := humanizeDownloadFailure(raw, "clouddrive"); got != raw {
		t.Errorf("无关错误不应被改写，实际 %q", got)
	}
}

// TestFriendlyProbeMessageTranslatesCloudDriveError 是本轮的核心回归：
// CloudDrive 读目录失败时，上游会把整段 gRPC 异常回给我们。
// 界面必须只显示一句人话，绝不能把 RPC 堆栈糊到页面上——
// 那样用户会以为"我的配置坏了"，而事实恰恰相反（下载器是好的，只是目录不存在）。
func TestFriendlyProbeMessageTranslatesCloudDriveError(t *testing.T) {
	raw := `CloudDrive目录读取失败：<_MultiThreadedRendezvous of RPC that terminated with:
	status = StatusCode.NOT_FOUND
	details = "get_subfiles of "/BON_115网盘/私存入库/AVdb" error: not found "AVdb" under "/BON_115网盘/私存入库""
	debug_error_string = "UNKNOWN:get_subfiles of "/BON_115网盘/私存入库/AVdb" error: ... {created_time:...}"
>`

	got := friendlyProbeMessage(raw)

	// 缺失的那一级要与父目录拼成完整路径——这是用户唯一需要的具体信息。
	if !strings.Contains(got, "/BON_115网盘/私存入库/AVdb") {
		t.Errorf("未拼出完整路径，实际 %q", got)
	}
	if !strings.Contains(got, "可用") {
		t.Errorf("应说明下载器本身可用，实际 %q", got)
	}
	// 原始异常里的噪音一个都不许漏到界面上。
	for _, noise := range []string{"Rendezvous", "RPC", "StatusCode", "debug_error_string", "created_time"} {
		if strings.Contains(got, noise) {
			t.Errorf("上游原文泄露到界面（含 %q）：%s", noise, got)
		}
	}
	if n := len([]rune(got)); n > 120 {
		t.Errorf("友好文案过长（%d 字），应当只有一句：%s", n, got)
	}
}

// TestFriendlyProbeMessageTruncatesUnknownErrors 保证"认不出来的报错"也不会糊满页面。
func TestFriendlyProbeMessageTruncatesUnknownErrors(t *testing.T) {
	got := friendlyProbeMessage(strings.Repeat("异常", 300))
	if n := len([]rune(got)); n > 200 {
		t.Errorf("未知报错应被截断，实际 %d 字", n)
	}
}

// TestFriendlyProbeMessageKeepsEmptyEmpty 确认空输入不产生任何提示文案。
func TestFriendlyProbeMessageKeepsEmptyEmpty(t *testing.T) {
	if got := friendlyProbeMessage("   "); got != "" {
		t.Errorf("空白输入应返回空串，实际 %q", got)
	}
}

// TestJoinDirPath 覆盖拼路径的边界：多斜杠、缺一侧、两侧都空。
func TestJoinDirPath(t *testing.T) {
	cases := []struct{ dir, name, want string }{
		{"/a/b", "c", "/a/b/c"},
		{"/a/b/", "c", "/a/b/c"},
		{"/a/b", "/c", "/a/b/c"},
		{"", "c", "/c"},
		{"/a/b", "", "/a/b"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := joinDirPath(c.dir, c.name); got != c.want {
			t.Errorf("joinDirPath(%q, %q) = %q, 期望 %q", c.dir, c.name, got, c.want)
		}
	}
}

// decodeJSON 是测试里解 JSON 的小包装，失败时给出原始响应体便于定位。
func decodeJSON(t *testing.T, body []byte, target any) error {
	t.Helper()
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("%w（原始响应：%s）", err, truncateForDisplay(string(body), 200))
	}
	return nil
}
