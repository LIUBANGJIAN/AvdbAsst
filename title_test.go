package main

// 「网站名称带不带搜索关键词」开关的回归测试。
//
// 需求原话：设置里加个网站名称的开关，关闭是没有关键字的，开启后就带关键字，
// 默认状态是关闭。
//
// 这一条落在三处彼此不相邻的地方，所以三处都要钉住：
//  1. 模板渲染——title 的内容与形状（空关键词不能渲染成「 · 资源搜索」）；
//  2. 设置页——复选框当前状态要如实回显，提交后要能改；
//  3. 持久化——它是偏好，重启后必须还在；同时默认值不能把"以后想改默认值"
//     这条路堵死（对照 max_results 那个坑）。

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// emptyUpstream 是"搜什么都 0 条"的假上游。
func emptyUpstream() http.HandlerFunc {
	return upstreamWith(`{"code":0,"message":"操作成功","data":[]}`)
}

// titleOf 取出响应里 <title> 的内容。
//
// 刻意"取标签内容"而不是直接在整页上搜关键词：页头 brand 与首页 h1 里
// 也有站名，只在整页上做 Contains 分不清"标题里带了关键词"
// 和"别的什么地方出现了关键词"——那样这条测试就成了永远绿的摆设。
func titleOf(t *testing.T, body string) string {
	t.Helper()
	const open = "<title>"
	start := strings.Index(body, open)
	if start < 0 {
		t.Fatalf("页面里没有 <title>；片段: %s", excerpt(body, "<head>"))
	}
	rest := body[start+len(open):]
	end := strings.Index(rest, "</title>")
	if end < 0 {
		t.Fatal("<title> 没有闭合")
	}
	return rest[:end]
}

// TestPageTitleHidesKeywordByDefault 守住"默认关闭"。
//
// 这里覆盖了四条会各自独立渲染的路径（首页、结果页、无结果、上游报错），
// 它们共用同一份视图数据但不共用同一段模板分支——将来只改一处，
// 另外几处就会漏。
func TestPageTitleHidesKeywordByDefault(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))
	if app.currentConfig().TitleWithKeyword {
		t.Fatal("网站名称开关的默认值应为 false（关闭）")
	}

	for _, target := range []string{"/", "/s?q=abc", "/s/abc", "/s?q=abc&page=2"} {
		got := titleOf(t, do(app, http.MethodGet, target).Body.String())
		if got != siteName {
			t.Errorf("%s 的标题 = %q，默认应恒为 %q（不带关键词）", target, got, siteName)
		}
	}

	// 无结果页。
	empty := newTestApp(t, emptyUpstream())
	if got := titleOf(t, do(empty, http.MethodGet, "/s?q=zzz").Body.String()); got != siteName {
		t.Errorf("无结果页的标题 = %q，期望 %q", got, siteName)
	}

	// 上游报错页。刻意用 401 而不是 500：5xx 属于可重试状态，
	// 会带上退避重试，白白拖长这条用例。
	failing := newTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid api key"}`))
	})
	if got := titleOf(t, do(failing, http.MethodGet, "/s?q=abc").Body.String()); got != siteName {
		t.Errorf("错误页的标题 = %q，期望 %q", got, siteName)
	}

	// 设置页本来就不带关键词，顺手确认它没有被这次改动波及。
	if got := titleOf(t, do(app, http.MethodGet, "/settings").Body.String()); got != "设置 · "+siteName {
		t.Errorf("设置页的标题 = %q，期望 %q", got, "设置 · "+siteName)
	}
}

// TestPageTitleShowsKeywordWhenEnabled 守住"开启后带关键词"。
func TestPageTitleShowsKeywordWhenEnabled(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) {
		c.TitleWithKeyword = true
	})

	want := "abc · " + siteName
	// 同一份配置要在所有页面形态上生效：路径式地址、翻页、筛选后都是。
	for _, target := range []string{"/s?q=abc", "/s/abc", "/s?q=abc&page=2", "/s?q=abc&site=%E8%89%B2%E8%8A%B1%E5%A0%82"} {
		got := titleOf(t, do(app, http.MethodGet, target).Body.String())
		if got != want {
			t.Errorf("%s 的标题 = %q，期望 %q", target, got, want)
		}
	}

	// 开关打开但关键词为空时**不能**留下一个孤零零的分隔符。
	for _, target := range []string{"/", "/s", "/s?q=%20%20"} {
		if got := titleOf(t, do(app, http.MethodGet, target).Body.String()); got != siteName {
			t.Errorf("%s 在开关打开时标题 = %q，期望 %q（无关键词时不带分隔符）", target, got, siteName)
		}
	}
}

// TestPageTitleEscapesKeyword 确认关键词是**不可信输入**。
//
// 关键词直接来自地址栏，会被拼进 <title>。html/template 会按上下文转义，
// 这条测试把它钉住——哪天有人图省事改成 text/template 或手工拼字符串，
// 这里会立刻红。
func TestPageTitleEscapesKeyword(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) {
		c.TitleWithKeyword = true
	})

	body := do(app, http.MethodGet, "/s?q=%3Cb%3Ex").Body.String()
	got := titleOf(t, body)

	if strings.Contains(got, "<b>") {
		t.Errorf("标题里出现了未转义的标签：%q", got)
	}
	if !strings.Contains(got, "&lt;b&gt;x") {
		t.Errorf("标题应包含转义后的关键词，实际 %q", got)
	}
	if !strings.HasSuffix(got, "· "+siteName) {
		t.Errorf("转义不应吃掉站名，实际 %q", got)
	}
}

// TestSettingsPageShowsTitleSwitch 确认设置页有这个开关，且状态如实回显。
func TestSettingsPageShowsTitleSwitch(t *testing.T) {
	// 关（默认）
	off := newTestApp(t, upstreamWith(upstreamSample))
	body := do(off, http.MethodGet, "/settings").Body.String()
	if !strings.Contains(body, `name="title_with_keyword"`) {
		t.Fatalf("设置页缺少网站名称开关；片段: %s", excerpt(body, "结果展示"))
	}
	if !strings.Contains(body, `name="title_with_keyword" value="1">`) {
		t.Errorf("默认应是未勾选状态；片段: %s", excerpt(body, `name="title_with_keyword"`))
	}
	if strings.Contains(body, `name="title_with_keyword" value="1" checked`) {
		t.Errorf("默认不应是勾选状态；片段: %s", excerpt(body, `name="title_with_keyword"`))
	}

	// 开
	on := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.TitleWithKeyword = true })
	body = do(on, http.MethodGet, "/settings").Body.String()
	if !strings.Contains(body, `name="title_with_keyword" value="1" checked>`) {
		t.Errorf("已开启时复选框应勾上；片段: %s", excerpt(body, `name="title_with_keyword"`))
	}
}

// TestSettingsSavePersistsTitleSwitch 是本需求的主用例：关 → 开 → 关 的完整往返。
func TestSettingsSavePersistsTitleSwitch(t *testing.T) {
	dir := t.TempDir()
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.ConfigDir = dir })
	upstream := app.currentConfig().APIBaseURL
	configPath := filepath.Join(dir, configFileName)

	// 表单必须带上合法的上游地址，否则会先被 Validate 拦下（与本用例无关）。
	post := func(withSwitch bool) {
		t.Helper()
		form := url.Values{"api_base_url": {upstream}, "page_size": {"100"}}
		if withSwitch {
			form.Set("title_with_keyword", "1")
		}
		if rec := doForm(app, "/settings", form, "http://example.com"); rec.Code != http.StatusSeeOther {
			t.Fatalf("保存失败，状态码 = %d；body=%s", rec.Code, excerpt(rec.Body.String(), ""))
		}
	}

	readFile := func() string {
		t.Helper()
		data, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatalf("读取配置文件失败: %v", err)
		}
		return string(data)
	}

	// 1) 勾上并保存：内存、磁盘、页面三处都要跟上。
	post(true)
	if !app.currentConfig().TitleWithKeyword {
		t.Error("勾选保存后内存配置未更新")
	}
	if !strings.Contains(readFile(), `"title_with_keyword": true`) {
		t.Errorf("勾选保存后磁盘上应记录该开关：\n%s", readFile())
	}
	if !strings.Contains(do(app, http.MethodGet, "/settings").Body.String(), `name="title_with_keyword" value="1" checked>`) {
		t.Error("保存后设置页的复选框未回显为已勾选")
	}
	if got := titleOf(t, do(app, http.MethodGet, "/s?q=abc").Body.String()); got != "abc · "+siteName {
		t.Errorf("开启后结果页标题 = %q，期望 %q", got, "abc · "+siteName)
	}

	// 2) 取消勾选（浏览器不再提交该字段）并保存：立刻回到不带关键词。
	post(false)
	if app.currentConfig().TitleWithKeyword {
		t.Error("取消勾选保存后内存配置未复原")
	}
	if got := titleOf(t, do(app, http.MethodGet, "/s?q=abc").Body.String()); got != siteName {
		t.Errorf("关闭后结果页标题 = %q，期望 %q", got, siteName)
	}

	// 3) 重启（重新读盘）后开关仍在——它是偏好，不是一次性的 URL 参数。
	post(true)
	t.Chdir(t.TempDir()) // 切到空目录，确保只可能从 dir 读到配置
	clearConfigEnv(t)
	t.Setenv("AVDB_CONFIG_DIR", dir)

	reloaded, _ := ResolveConfig()
	if !reloaded.TitleWithKeyword {
		t.Error("重启后开关丢失，应保持开启")
	}
	restarted := NewApp(reloaded, discardLogger())
	if got := titleOf(t, do(restarted, http.MethodGet, "/s?q=abc").Body.String()); got != "abc · "+siteName {
		t.Errorf("重启后结果页标题 = %q，期望 %q", got, "abc · "+siteName)
	}
}

// TestTitleSwitchIsPreferenceNotURLParam 确认这个开关**只能**从设置页改。
//
// 它一旦做成 URL 参数，任何一条链到 /s?title_with_keyword=1 的地址
// 都能顺手改掉别人的偏好，而且翻页链接还会把它一路带着走。
func TestTitleSwitchIsPreferenceNotURLParam(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	do(app, http.MethodGet, "/s?q=abc&title_with_keyword=1")
	if app.currentConfig().TitleWithKeyword {
		t.Error("URL 参数不应能改这个开关")
	}
	body := do(app, http.MethodGet, "/s?q=abc").Body.String()
	if strings.Contains(body, "title_with_keyword") {
		t.Errorf("结果页不应出现该字段名；片段: %s", excerpt(body, "title_with_keyword"))
	}
	if got := pagerURL("abc", searchFilter{}, 2); strings.Contains(got, "title_with_keyword") {
		t.Errorf("翻页链接不应携带该字段：%s", got)
	}
}

// TestTitleSwitchDefaultStaysOutOfConfigFile 确认默认值不会被写进配置文件。
//
// 这是一道防未来的保险，不是洁癖：max_results 就是被"把默认值照写成文件"
// 这件事坑过的——默认值一旦落进存量 config.json，以后想改默认值就再也改不动。
// 所以这里钉住"没偏离默认值就不落盘"，哪天有人把 json tag 的 omitempty 去掉，
// 这条会红并指着上面那段解释。
func TestTitleSwitchDefaultStaysOutOfConfigFile(t *testing.T) {
	dir := t.TempDir()

	off := testConfig(dir)
	off.TitleWithKeyword = false
	if err := off.Save(); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}
	data, err := os.ReadFile(off.ConfigPath())
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	if strings.Contains(string(data), "title_with_keyword") {
		t.Errorf("默认关闭时不应把这个字段写进配置文件：\n%s", data)
	}

	// 显式开启时必须落盘，否则"开了等于没开"。
	on := testConfig(dir)
	on.TitleWithKeyword = true
	if err := on.Save(); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}
	data, err = os.ReadFile(on.ConfigPath())
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	if !strings.Contains(string(data), `"title_with_keyword": true`) {
		t.Errorf("开启时必须落盘：\n%s", data)
	}
}
