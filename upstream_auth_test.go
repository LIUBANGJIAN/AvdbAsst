package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 设置类接口的单元测试：登录换 JWT、探测下载器、列出目录、防御式解析。
//
// 这一组测试守护的是本项目最敏感的两条红线：
//   1) 密码与 JWT 绝不落盘（TestSettingsLoginNeverPersistsPassword 会直接读配置文件核查）；
//   2) 上游配置里的凭据（网盘 Cookie 等）绝不被解析进来、更不会回传前端
//      （TestHarvestStringsOnlyTakesWhitelistedKeys）。
// ---------------------------------------------------------------------------

// authUpstream 是"路由表驱动"的假上游，用来复刻设置类接口。
// 与 e2e_test.go 的 strictUpstream 区别：那个只管下载契约，这个管认证与配置。
type authUpstream struct {
	routes   map[string]http.HandlerFunc
	lastForm url.Values
	lastAuth string
	lastPath string
}

func (a *authUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	a.lastForm = r.PostForm
	a.lastAuth = r.Header.Get("Authorization")
	a.lastPath = r.URL.Path

	if handler, ok := a.routes[r.Method+" "+r.URL.Path]; ok {
		handler(w, r)
		return
	}
	upstreamWriteJSON(w, http.StatusNotFound, map[string]any{"detail": "Not Found"})
}

// newAuthUpstream 起一个假上游并返回与之绑定的客户端。
func newAuthUpstream(t *testing.T, routes map[string]http.HandlerFunc) (*authUpstream, *AvdbClient) {
	t.Helper()
	up := &authUpstream{routes: routes}
	server := httptest.NewServer(up)
	t.Cleanup(server.Close)

	client := NewAvdbClient(Config{APIBaseURL: server.URL, APIKey: "test-key", Timeout: 5 * time.Second})
	return up, client
}

// --------------------------------------------------------------- 登录

func TestLoginExtractsTokenFromData(t *testing.T) {
	up, client := newAuthUpstream(t, map[string]http.HandlerFunc{
		"POST /api/v1/users/login": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0, "message": "登录成功",
				"data": map[string]any{"access_token": "jwt-in-data"},
			})
		},
	})

	result, err := client.Login(context.Background(), "alice", "pw")
	if err != nil {
		t.Fatalf("登录应成功，实际 %v", err)
	}
	if result.JWT != "jwt-in-data" {
		t.Errorf("JWT = %q，期望 jwt-in-data", result.JWT)
	}
	if result.Needs2FA() {
		t.Error("不应被判为需要二次验证")
	}
	// 上游要求表单编码，字段名是 username / password。
	if up.lastForm.Get("username") != "alice" || up.lastForm.Get("password") != "pw" {
		t.Errorf("登录表单字段不正确: %v", up.lastForm)
	}
}

func TestLoginExtractsTopLevelToken(t *testing.T) {
	// 有些版本把 access_token 直接放在顶层，没有 data 外壳。
	_, client := newAuthUpstream(t, map[string]http.HandlerFunc{
		"POST /api/v1/users/login": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusOK, map[string]any{"access_token": "jwt-top"})
		},
	})

	result, err := client.Login(context.Background(), "alice", "pw")
	if err != nil {
		t.Fatalf("登录应成功，实际 %v", err)
	}
	if result.JWT != "jwt-top" {
		t.Errorf("JWT = %q，期望 jwt-top", result.JWT)
	}
}

func TestLoginRequires2FA(t *testing.T) {
	_, client := newAuthUpstream(t, map[string]http.HandlerFunc{
		"POST /api/v1/users/login": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0,
				"data": map[string]any{"otp_token": "otp-abc"},
			})
		},
	})

	result, err := client.Login(context.Background(), "alice", "pw")
	if err != nil {
		t.Fatalf("登录应成功（待二次验证），实际 %v", err)
	}
	if !result.Needs2FA() {
		t.Fatalf("应判定为需要二次验证，实际 JWT=%q OTP=%q", result.JWT, result.OTPToken)
	}
	if result.OTPToken != "otp-abc" {
		t.Errorf("OTPToken = %q", result.OTPToken)
	}
}

func TestLogin2FACompletes(t *testing.T) {
	up, client := newAuthUpstream(t, map[string]http.HandlerFunc{
		"POST /api/v1/users/login/2fa": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": map[string]any{"access_token": "jwt-after-2fa"},
			})
		},
	})

	jwt, err := client.Login2FA(context.Background(), "otp-abc", "123456")
	if err != nil {
		t.Fatalf("二次验证应成功，实际 %v", err)
	}
	if jwt != "jwt-after-2fa" {
		t.Errorf("JWT = %q", jwt)
	}
	if up.lastForm.Get("otp_token") != "otp-abc" || up.lastForm.Get("otp_code") != "123456" {
		t.Errorf("二次验证表单字段不正确: %v", up.lastForm)
	}
}

func TestLoginSurfacesUpstreamMessage(t *testing.T) {
	// 密码错误时上游返回业务错误码 + 中文说明，必须原样带给用户。
	_, client := newAuthUpstream(t, map[string]http.HandlerFunc{
		"POST /api/v1/users/login": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 1, "message": "用户名或密码错误",
			})
		},
	})

	_, err := client.Login(context.Background(), "alice", "wrong")
	if err == nil {
		t.Fatal("密码错误时应返回 error")
	}
	if !strings.Contains(err.Error(), "用户名或密码错误") {
		t.Errorf("错误信息应带上上游原话，实际 %q", err.Error())
	}
}

func TestLoginRejectsEmptyCredentialsWithoutCallingUpstream(t *testing.T) {
	called := false
	_, client := newAuthUpstream(t, map[string]http.HandlerFunc{
		"POST /api/v1/users/login": func(w http.ResponseWriter, r *http.Request) {
			called = true
			upstreamWriteJSON(w, http.StatusOK, map[string]any{"access_token": "x"})
		},
	})

	if _, err := client.Login(context.Background(), "", "pw"); err == nil {
		t.Error("空用户名应报错")
	}
	if _, err := client.Login(context.Background(), "alice", ""); err == nil {
		t.Error("空密码应报错")
	}
	if called {
		t.Error("凭据不全时不应发起上游请求")
	}
}

// ------------------------------------------------------- 下载器清单探测

func TestProbeDownloadersFindsList(t *testing.T) {
	_, client := newAuthUpstream(t, map[string]http.HandlerFunc{
		"GET /api/v1/config/downloader": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0,
				"data": []map[string]any{
					{"id": "115", "name": "115网盘"},
					{"id": "qb", "name": "qBittorrent"},
					{"id": "qb", "name": "重复项应被去重"},
				},
			})
		},
	})

	opts, trace, err := client.ProbeDownloaders(context.Background(), "jwt")
	if err != nil {
		t.Fatalf("探测不应报错，实际 %v", err)
	}
	if len(opts) != 2 {
		t.Fatalf("应得到 2 个下载器（去重后），实际 %d: %+v", len(opts), opts)
	}
	if opts[0].ID != "115" || opts[0].Label != "115网盘" {
		t.Errorf("第一个下载器 = %+v", opts[0])
	}
	if len(trace) == 0 {
		t.Error("应返回探测轨迹，便于界面上说明过程")
	}
}

func TestProbeDownloadersFallsBackThroughCandidateKeys(t *testing.T) {
	// 第一个候选键不存在时，应继续尝试后面的键，而不是直接失败。
	_, client := newAuthUpstream(t, map[string]http.HandlerFunc{
		"GET /api/v1/config/downloaders": func(w http.ResponseWriter, r *http.Request) {
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": []string{"115", "aria2"},
			})
		},
	})

	opts, _, err := client.ProbeDownloaders(context.Background(), "jwt")
	if err != nil {
		t.Fatalf("探测不应报错，实际 %v", err)
	}
	if len(opts) != 2 || opts[0].ID != "115" || opts[1].ID != "aria2" {
		t.Errorf("期望从后备键取到 [115 aria2]，实际 %+v", opts)
	}
}

func TestProbeDownloadersReportsNilWhenNothingFound(t *testing.T) {
	// 全部候选键都读不到时，不算错误——页面会退回到"手工填写 + 校验"路径。
	_, client := newAuthUpstream(t, nil)

	opts, trace, err := client.ProbeDownloaders(context.Background(), "jwt")
	if err != nil {
		t.Fatalf("没找到不应算错误，实际 %v", err)
	}
	if len(opts) != 0 {
		t.Errorf("不应解析出下载器，实际 %+v", opts)
	}
	if len(trace) != len(downloaderConfigKeys) {
		t.Errorf("轨迹应覆盖每个候选键，实际 %d 条", len(trace))
	}
}

// ------------------------------------------------------------ 目录列举

func TestDownloaderDirectoriesParsesStringArray(t *testing.T) {
	up, client := newAuthUpstream(t, map[string]http.HandlerFunc{
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
}

func TestDownloaderDirectoriesParsesObjectArray(t *testing.T) {
	_, client := newAuthUpstream(t, map[string]http.HandlerFunc{
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
	_, client := newAuthUpstream(t, map[string]http.HandlerFunc{
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
	_, client := newAuthUpstream(t, map[string]http.HandlerFunc{
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
	_, client := newAuthUpstream(t, nil)

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

func TestHarvestDownloaderOptionsIgnoresNonListPayload(t *testing.T) {
	// 单个对象（不是数组）不算"清单"，否则会把无关设置里的 name/id 也当成下载器。
	payload := []byte(`{"enabled": true, "name": "some-unrelated-setting"}`)
	if opts := harvestDownloaderOptions(payload); len(opts) != 0 {
		t.Errorf("非数组载荷不应产出下载器，实际 %+v", opts)
	}
}

func TestHarvestDownloaderOptionsFromScalarArray(t *testing.T) {
	payload := []byte(`["115", "aria2", "115"]`)
	opts := harvestDownloaderOptions(payload)
	if len(opts) != 2 || opts[0].ID != "115" || opts[1].ID != "aria2" {
		t.Errorf("期望 [115 aria2]，实际 %+v", opts)
	}
}

// ------------------------------------------------------- 设置页处理器

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

func TestSettingsLoginDiscoversDownloaders(t *testing.T) {
	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/users/login":
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": map[string]any{"access_token": "jwt-1"},
			})
		case "/api/v1/config/downloader":
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0,
				"data": []map[string]any{
					{"id": "115", "name": "115网盘"},
					{"id": "qb", "name": "qBittorrent"},
				},
			})
		default:
			upstreamWriteJSON(w, http.StatusNotFound, map[string]any{"detail": "Not Found"})
		}
	})

	rec := doForm(app, "/api/settings/login", url.Values{
		"api_base_url": {app.currentConfig().APIBaseURL},
		"username":     {"alice"},
		"password":     {"s3cret-password"},
	}, "http://example.com")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 %s", rec.Code, rec.Body.String())
	}

	var resp settingsProbeResponse
	if err := decodeJSON(t, rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Success || len(resp.Downloaders) != 2 {
		t.Fatalf("响应 = %+v", resp)
	}
	if cached := app.currentConfig().DownloaderOptions; len(cached) != 2 {
		t.Errorf("下载器候选未缓存，实际 %v", cached)
	}
}

// TestSettingsLoginNeverPersistsPassword 是本文件最重要的一条断言。
//
// 做法很土但很硬：登录成功后直接把落盘的 config.json 读出来，
// 逐字节确认里面**没有**刚才那个密码。任何"顺手把密码存下来方便下次自动登录"
// 的改动都会在这里当场翻车。
func TestSettingsLoginNeverPersistsPassword(t *testing.T) {
	const password = "s3cret-password"

	app := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/users/login":
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": map[string]any{"access_token": "jwt-1"},
			})
		case "/api/v1/config/downloader":
			upstreamWriteJSON(w, http.StatusOK, map[string]any{
				"code": 0, "data": []map[string]any{{"id": "115", "name": "115网盘"}},
			})
		default:
			upstreamWriteJSON(w, http.StatusNotFound, map[string]any{"detail": "Not Found"})
		}
	})

	rec := doForm(app, "/api/settings/login", url.Values{
		"api_base_url": {app.currentConfig().APIBaseURL},
		"username":     {"alice"},
		"password":     {password},
	}, "http://example.com")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}

	raw, err := os.ReadFile(app.currentConfig().ConfigPath())
	if err != nil {
		t.Fatalf("读取配置文件失败: %v", err)
	}
	for _, forbidden := range []string{password, "jwt-1"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("配置文件里出现了 %q —— 密码/JWT 绝不能落盘", forbidden)
		}
	}
	// 顺带确认响应体也没有把 JWT 回显给浏览器。
	if bytes.Contains(rec.Body.Bytes(), []byte("jwt-1")) {
		t.Error("响应体回显了 JWT")
	}
}

// ------------------------------------------------------------ 错误人话化

func TestHumanizeDownloadFailureExplainsDownloaderNotFound(t *testing.T) {
	cfg := Config{Downloader: "115"}

	for _, raw := range []string{
		"未找到下载器: 115",
		"Downloader not found: 115",
		"unknown downloader",
	} {
		got := humanizeDownloadFailure(raw, cfg)
		if !strings.Contains(got, "设置") {
			t.Errorf("对 %q 应给出指向设置页的指引，实际 %q", raw, got)
		}
		if !strings.Contains(got, raw) {
			t.Errorf("应保留上游原文便于排查，实际 %q", got)
		}
	}
}

func TestHumanizeDownloadFailureLeavesOtherErrorsAlone(t *testing.T) {
	raw := "磁盘空间不足"
	if got := humanizeDownloadFailure(raw, Config{Downloader: "115"}); got != raw {
		t.Errorf("无关错误不应被改写，实际 %q", got)
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
