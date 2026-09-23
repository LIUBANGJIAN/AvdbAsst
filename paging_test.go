package main

// 分页、每页条数与结果页展示的回归测试。
//
// 这一组守的是"用户能看到什么"：翻页正确、序号连续、下拉能改且改完不走失，
// 以及**不该出现的东西真的不在页面上**（免费 / 做种 / 上游产品名）。

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readWebFile 读取内嵌的前端资源，用于断言脚本内容。
func readWebFile(name string) (string, error) {
	data, err := fs.ReadFile(webRoot(), name)
	return string(data), err
}

// ------------------------------------------------------------ 分页计算

func TestSlicePage(t *testing.T) {
	items := make([]Torrent, 250)
	for i := range items {
		items[i] = Torrent{ID: int64(i + 1)}
	}

	cases := []struct {
		name                    string
		page, size              int
		wantPage, wantPages     int
		wantStart, wantLength   int
		wantFirstID, wantLastID int64
	}{
		{"第一页", 1, 100, 1, 3, 1, 100, 1, 100},
		{"中间页", 2, 100, 2, 3, 101, 100, 101, 200},
		{"末页取余数", 3, 100, 3, 3, 201, 50, 201, 250},
		{"页码越界夹到末页", 999, 100, 3, 3, 201, 50, 201, 250},
		{"页码小于 1 夹到首页", 0, 100, 1, 3, 1, 100, 1, 100},
		{"每页全部", 1, 0, 1, 1, 1, 250, 1, 250},
		{"每页全部时页码也被忽略", 7, 0, 1, 1, 1, 250, 1, 250},
		{"条数正好整除", 2, 50, 2, 5, 51, 50, 51, 100},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			win := slicePage(items, tc.page, tc.size)
			if win.Page != tc.wantPage || win.TotalPages != tc.wantPages {
				t.Errorf("Page/TotalPages = %d/%d，期望 %d/%d",
					win.Page, win.TotalPages, tc.wantPage, tc.wantPages)
			}
			if win.Total != len(items) {
				t.Errorf("Total = %d，期望 %d", win.Total, len(items))
			}
			if win.StartedAt != tc.wantStart {
				t.Errorf("StartedAt = %d，期望 %d", win.StartedAt, tc.wantStart)
			}
			if len(win.Items) != tc.wantLength {
				t.Fatalf("本页条数 = %d，期望 %d", len(win.Items), tc.wantLength)
			}
			if win.Items[0].ID != tc.wantFirstID || win.Items[len(win.Items)-1].ID != tc.wantLastID {
				t.Errorf("本页 ID 区间 = %d..%d，期望 %d..%d",
					win.Items[0].ID, win.Items[len(win.Items)-1].ID, tc.wantFirstID, tc.wantLastID)
			}
		})
	}
}

// TestSlicePageEmptyResult 确认空结果不会被算成 0 页——那会让分页条显示"第 1 / 0 页"。
func TestSlicePageEmptyResult(t *testing.T) {
	win := slicePage(nil, 1, 100)
	if win.TotalPages != 1 {
		t.Errorf("空结果的 TotalPages = %d，期望 1", win.TotalPages)
	}
	if len(win.Items) != 0 {
		t.Errorf("空结果不应有条目，实际 %d", len(win.Items))
	}
}

func TestPagerURLKeepsKeywordOnlyWhenNeeded(t *testing.T) {
	cases := []struct {
		keyword string
		page    int
		want    string
	}{
		{"abc", 1, "/s?q=abc"},
		// 参数顺序由 url.Values.Encode() 决定（按键名排序），无需人工干预。
		{"abc", 3, "/s?page=3&q=abc"},
		{"", 1, "/s"},
		{"中文 关键词", 2, "/s?page=2&q=%E4%B8%AD%E6%96%87+%E5%85%B3%E9%94%AE%E8%AF%8D"},
	}
	for _, tc := range cases {
		if got := pagerURL(tc.keyword, tc.page); got != tc.want {
			t.Errorf("pagerURL(%q, %d) = %q，期望 %q", tc.keyword, tc.page, got, tc.want)
		}
	}
	// 关键属性：翻页链接里**不能**出现 page_size。
	// 每页条数是已落盘的偏好，靠链接反复重发只会让它看起来像一次性参数。
	if got := pagerURL("abc", 2); strings.Contains(got, "page_size") {
		t.Errorf("翻页链接不应携带 page_size: %s", got)
	}
}

func TestPageLinksWindow(t *testing.T) {
	links := pageLinks("q", 1, 20)
	if len(links) != 7 {
		t.Fatalf("页码窗口长度 = %d，期望 7", len(links))
	}
	if links[0].Number != 1 || !links[0].Current {
		t.Errorf("首页窗口应从 1 开始且标记当前页，实际首项 %+v", links[0])
	}

	links = pageLinks("q", 20, 20)
	if links[len(links)-1].Number != 20 || !links[len(links)-1].Current {
		t.Errorf("末页窗口应以 20 结束且标记当前页，实际末项 %+v", links[len(links)-1])
	}

	// 页数少于窗口长度时全部列出，不出现重复或越界页码。
	links = pageLinks("q", 2, 3)
	if len(links) != 3 {
		t.Fatalf("总页数 3 时应列出 3 个页码，实际 %d", len(links))
	}
	for i, l := range links {
		if l.Number != i+1 {
			t.Errorf("第 %d 项页码 = %d，期望 %d", i, l.Number, i+1)
		}
	}
}

// ------------------------------------------------------- 结果页的分页渲染

// manyTorrents 生成 n 条测试数据，用于验证跨页序号。
func manyTorrents(n int) string {
	var b strings.Builder
	b.WriteString(`{"code":0,"message":"操作成功","data":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%d,"number":"N-%03d","site":"Site","section":"Sec","category":"",
			"size_mb":100,"post_time":"2026-01-02 03:04:05","seeders":5,
			"title":"条目 %03d","download_url":"magnet:?xt=urn:btih:%040d"}`,
			i+1, i+1, i+1, i+1)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestSearchPagePaginatesAndNumbersRows(t *testing.T) {
	app := newTestApp(t, upstreamWith(manyTorrents(250)), func(c *Config) { c.PageSize = 100 })

	// 第 1 页：序号从 1 开始。
	body := do(app, http.MethodGet, "/s?q=x").Body.String()
	if !strings.Contains(body, `class="row-index" aria-hidden="true">1<`) {
		t.Errorf("第 1 页首条序号应为 1；片段: %s", excerpt(body, "row-index"))
	}
	if n := strings.Count(body, `class="row"`); n != 100 {
		t.Errorf("第 1 页渲染 %d 条，期望 100", n)
	}
	if !strings.Contains(body, "第 1 / 3 页") {
		t.Errorf("统计行应显示页码；片段: %s", excerpt(body, "class=\"stats\""))
	}

	// 第 2 页：序号必须接着走（101 起），而不是从 1 重新数——
	// 否则"第 137 条"这种指代在两页之间就失效了。
	body = do(app, http.MethodGet, "/s?q=x&page=2").Body.String()
	if !strings.Contains(body, `class="row-index" aria-hidden="true">101<`) {
		t.Errorf("第 2 页首条序号应为 101；片段: %s", excerpt(body, "row-index"))
	}
	if n := strings.Count(body, `class="row"`); n != 100 {
		t.Errorf("第 2 页渲染 %d 条，期望 100", n)
	}
	if strings.Contains(body, `class="row-index" aria-hidden="true">1<`) {
		t.Error("第 2 页不应重新从 1 开始编号")
	}

	// 第 3 页：余数 50 条。
	body = do(app, http.MethodGet, "/s?q=x&page=3").Body.String()
	if n := strings.Count(body, `class="row"`); n != 50 {
		t.Errorf("第 3 页渲染 %d 条，期望 50", n)
	}
}

// TestSearchPageSizeQueryPersists 是"永久设置"这条需求的回归测试。
//
// 用户的要求是：在结果页改了每页条数，之后一直生效，直到下次再改。
// 所以这个参数不能只影响当前响应，必须落盘。
func TestSearchPageSizeQueryPersists(t *testing.T) {
	dir := t.TempDir()
	app := newTestApp(t, upstreamWith(manyTorrents(250)), func(c *Config) {
		c.PageSize = 100
		c.ConfigDir = dir
	})

	rec := do(app, http.MethodGet, "/s?q=x&page_size=300")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}
	if n := strings.Count(rec.Body.String(), `class="row"`); n != 250 {
		t.Errorf("改成 300 后本页应渲染全部 250 条，实际 %d", n)
	}
	if got := app.currentConfig().PageSize; got != 300 {
		t.Errorf("内存中的每页条数 = %d，期望 300", got)
	}

	// 落盘：换一个不带参数的新请求，设置必须还在。
	body := do(app, http.MethodGet, "/s?q=x").Body.String()
	if n := strings.Count(body, `class="row"`); n != 250 {
		t.Errorf("下一次访问应沿用已保存的 300，实际渲染 %d 条", n)
	}

	// 白名单外的取值一律忽略，不能成为"每页十万行"的入口。
	do(app, http.MethodGet, "/s?q=x&page_size=99999")
	if got := app.currentConfig().PageSize; got != 300 {
		t.Errorf("非法取值不应改变设置，实际 %d", got)
	}
}

// TestSearchPageOmitsRemovedLabels 守住用户明确要求删掉的几类字样。
//
// 这些内容不是"换个地方展示"，而是**不该存在**：
//   - 免费：上游返回的资源全部免费，逐条标注毫无区分度；
//   - 做种：用户要求不要出现做种信息与排序项；
//   - 上游产品名：会出现在浏览器标签、截图和结果快照里。
func TestSearchPageOmitsRemovedLabels(t *testing.T) {
	// 样例数据里两条都是 free=true、seeders=66，正好用来验证"存在但不再渲染"。
	app := newTestApp(t, upstreamWith(upstreamSample))
	body := do(app, http.MethodGet, "/s?q=abc").Body.String()

	for _, gone := range []string{"免费", "做种", "Avdb", "种子数"} {
		if strings.Contains(body, gone) {
			t.Errorf("搜索结果页不应再出现「%s」；片段: %s", gone, excerpt(body, gone))
		}
	}
	// 做种排序项也必须一并消失，否则等于换了个地方露出做种数。
	if strings.Contains(body, `value="seeders"`) {
		t.Error("排序下拉不应再有「做种最多」选项")
	}
	// data-seeders 是给该排序用的，排序没了它也没有存在意义。
	if strings.Contains(body, "data-seeders") {
		t.Error("结果行不应再带 data-seeders 属性")
	}
}

func TestSearchPageHasSelectionControls(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))
	body := do(app, http.MethodGet, "/s?q=abc").Body.String()

	for _, want := range []string{
		`id="batchbar"`, `id="check-all"`, `id="batch-download"`, `id="batch-copy"`,
		`class="row-check"`, `class="row-pick"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("结果页缺少多选所需元素 %s", want)
		}
	}
	if n := strings.Count(body, `class="row-check"`); n != 2 {
		t.Errorf("每行都应有选择框，实际 %d 个", n)
	}
	// 批量复制要靠行上的磁链，缺了它"复制磁链"只能复制到空串。
	if !strings.Contains(body, `data-magnet="magnet:?xt=urn:btih:`) {
		t.Error("选择框应带上磁力链接，供批量复制使用")
	}
}

// TestPagesShowVersionNumberNotBuildID 守住"右上角显示的是版本号"。
//
// 用户明确说"版本显示的是版本序号"，意思是他要的是能横向比较的版本号，
// 而不是 branch@sha 那种构建标识。构建标识仍然有用，但应该退到 title 提示与
// 设置页的运行信息里。
func TestPagesShowVersionNumberNotBuildID(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))

	for _, target := range []string{"/", "/s?q=abc", "/settings"} {
		body := do(app, http.MethodGet, target).Body.String()
		if !strings.Contains(body, version) {
			t.Errorf("%s 应显示版本号 %q", target, version)
		}
		if !strings.HasPrefix(version, "v") {
			t.Errorf("版本号应当是形如 v1.2.3 的语义化版本，实际 %q", version)
		}
	}
}

// TestSettingsPageExposesPageSize 确认设置页也能改每页条数，且带上"永久生效"的说明。
func TestSettingsPageExposesPageSize(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.PageSize = 200 })
	body := do(app, http.MethodGet, "/settings").Body.String()

	if !strings.Contains(body, `name="page_size"`) {
		t.Fatal("设置页缺少每页条数选择框")
	}
	if !strings.Contains(body, `<option value="200" selected>`) {
		t.Errorf("当前值应被选中；片段: %s", excerpt(body, `name="page_size"`))
	}
	// 「全部」必须是可选值，且用 0 表示——"搜索结果没上限"这条需求的落点。
	if !strings.Contains(body, `<option value="0"`) {
		t.Error("设置页应提供「全部」选项（value=0）")
	}
	for _, size := range []string{"10", "50", "100", "300", "1000"} {
		if !strings.Contains(body, `<option value="`+size+`"`) {
			t.Errorf("设置页应提供 %s 条选项", size)
		}
	}
}

// TestSettingsProbeFormDoesNotSendRetiredFields 确认设置页脚本不再提交已下线的字段。
func TestSettingsProbeFormDoesNotSendRetiredFields(t *testing.T) {
	data, err := readWebFile("static/settings.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"timeout_seconds", "max_results"} {
		if strings.Contains(data, gone) {
			t.Errorf("settings.js 不应再提交已下线的字段 %s", gone)
		}
	}
	if !strings.Contains(data, "/api/settings/downloaders") {
		t.Error("settings.js 应调用「探测可用下载器」接口")
	}
}

// TestSearchPageSizeFormIsUsableWithoutJS 确认每页条数的表单在禁用 JS 时仍有提交手段。
func TestSearchPageSizeFormIsUsableWithoutJS(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.PageSize = 50 })
	body := do(app, http.MethodGet, "/s?q=abc").Body.String()

	if !strings.Contains(body, `<noscript>`) {
		t.Error("每页条数表单应提供 noscript 兜底提交按钮")
	}
	// 表单必须把关键词带上，否则改条数会把搜索结果清空。
	if !strings.Contains(body, `name="q" value="abc"`) {
		t.Errorf("每页条数表单应保留关键词；片段: %s", excerpt(body, "page-size-form"))
	}
}

// TestPageSizeEnvAllowsZero 确认"全部"能通过环境变量预置。
func TestPageSizeEnvAllowsZero(t *testing.T) {
	clearConfigEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv("AVDB_PAGE_SIZE", "0")

	cfg, notes := ResolveConfig()
	if cfg.PageSize != 0 {
		t.Errorf("AVDB_PAGE_SIZE=0 应表示「全部」，实际 PageSize = %d（notes: %v）", cfg.PageSize, notes)
	}

	clearConfigEnv(t)
	t.Setenv("AVDB_PAGE_SIZE", "abc")
	cfg, _ = ResolveConfig()
	if cfg.PageSize != defaultPageSize {
		t.Errorf("非法环境变量应回退默认值 %d，实际 %d", defaultPageSize, cfg.PageSize)
	}
}

// TestResultsPageSizeFormTargetsSearchRoute 确认表单指向的是搜索路由而不是当前路径。
func TestResultsPageSizeFormTargetsSearchRoute(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample), func(c *Config) { c.PageSize = 50 })
	body := do(app, http.MethodGet, "/s/abc").Body.String()

	if !strings.Contains(body, `<form class="field page-size-form" method="get" action="/s">`) {
		t.Errorf("表单应显式指向 /s；片段: %s", excerpt(body, "page-size-form"))
	}
}

// TestSearchPageHasNoResultHead 守住"不要再显示『搜索结果 xxx』"。
//
// 那行标题只是把用户刚输入的关键词又复述一遍（搜索框里已经有了），
// 却占掉页面上最显眼的位置。统计行（第 x / y 页 · 共 N 条）保留，它才是有效信息。
func TestSearchPageHasNoResultHead(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))
	body := do(app, http.MethodGet, "/s?q=abc").Body.String()

	for _, gone := range []string{"result-head", "搜索结果"} {
		if strings.Contains(body, gone) {
			t.Errorf("结果页不应再出现 %q；片段: %s", gone, excerpt(body, gone))
		}
	}
	// 关键词仍要留在搜索框里，否则用户看不出自己在搜什么。
	if !strings.Contains(body, `value="abc"`) {
		t.Error("搜索框应保留当前关键词")
	}
}

// TestBatchBarIsAlwaysVisible 守住"按钮不用隐藏"。
//
// 早先批量操作条是"选中任意一项后才出现"。用户明确要求它常驻：
// 按钮忽隐忽现会让人以为功能不稳定，而"没选中时它去哪了"本身就是个多余的问题。
func TestBatchBarIsAlwaysVisible(t *testing.T) {
	app := newTestApp(t, upstreamWith(upstreamSample))
	body := do(app, http.MethodGet, "/s?q=abc").Body.String()

	// 用完整开标签做断言：模板一旦退回带 hidden 的写法，这里立刻失败。
	if !strings.Contains(body, `<div class="batchbar" id="batchbar">`) {
		t.Errorf("批量操作条应当常驻（不带 hidden）；片段: %s", excerpt(body, "batchbar"))
	}
	// 「全选」是**带文字的按钮**，不是一个没有文案的复选框。
	if !strings.Contains(body, `id="check-all">全选本页<`) {
		t.Errorf("「全选」应是带文字的按钮；片段: %s", excerpt(body, "check-all"))
	}
	for _, want := range []string{"批量下载", "复制磁链", "清空选择"} {
		if !strings.Contains(body, want) {
			t.Errorf("批量操作条缺少「%s」", want)
		}
	}
}

// TestSearchIsNotTruncatedByDefault 守住"搜索没有条数上限"。
//
// 默认 MaxResults 为 0（不限制），所以哪怕上游一次回 5200 条（超过早期默认的 5000），
// 也必须一条不少地全部收下。早期实现会在页面上打出
// 「结果过多，只展示前 5000 条」，与"没有上限"的承诺自相矛盾。
func TestSearchIsNotTruncatedByDefault(t *testing.T) {
	const total = 5200

	app := newTestApp(t, upstreamWith(manyTorrents(total)), func(c *Config) {
		c.PageSize = 0 // 每页「全部」，否则分页会让断言变复杂
	})
	if got := app.currentConfig().MaxResults; got != 0 {
		t.Fatalf("默认 MaxResults = %d，期望 0（不限制）", got)
	}

	body := do(app, http.MethodGet, "/s?q=x").Body.String()
	if n := strings.Count(body, `class="row"`); n != total {
		t.Errorf("应渲染全部 %d 条，实际 %d", total, n)
	}
	if strings.Contains(body, "结果过多") {
		t.Error("默认配置下不应出现截断提示")
	}

	// 显式设置安全阀时它仍要生效——这是"可选"，不是"删除"。
	app = newTestApp(t, upstreamWith(manyTorrents(total)), func(c *Config) {
		c.MaxResults = 100
		c.PageSize = 0
	})
	body = do(app, http.MethodGet, "/s?q=x").Body.String()
	if n := strings.Count(body, `class="row"`); n != 100 {
		t.Errorf("显式设置安全阀 100 后应只渲染 100 条，实际 %d", n)
	}
}

// TestLegacyConfigFileDoesNotTruncate 是用户投诉「结果过多，只展示前 5000 条」的回归锁。
//
// 实况：搜「HD」时上游真实返回 5954 条，页面却只肯收 5000 条并打出截断提示，
// 954 条被无声丢掉。
//
// 根因不在默认值，而在**存量配置**：老版本的 Save() 会把整个 Config 落盘，
// max_results 字段没有 omitempty，于是每一个存量的 config.json 里都躺着一个 5000。
// 因此只把默认值从 5000 改成 0 是治不好的——升级二进制后那个 5000 照样被读回来。
// 这条用例把"文件里写着 5000 也不能生效"钉死：数据源必须是真实的配置文件，
// 而不是一个手工构造的 Config 字面量，否则测的就不是出问题的那条路径。
func TestLegacyConfigFileDoesNotTruncate(t *testing.T) {
	const total = 5954 // 真实上游在「HD」下的条数

	upstream := httptest.NewServer(upstreamWith(manyTorrents(total)))
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	// 这就是老版本写出来的文件长什么样：max_results 明晃晃地写着 5000。
	legacy := `{"api_key":"k","default_downloader":"clouddrive",` +
		`"default_save_path":"/x/y","max_results":5000,"page_size":0}`
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte(legacy), 0o600); err != nil {
		t.Fatalf("写入存量 config.json 失败: %v", err)
	}

	// 切到空目录，确保除了 dir 之外没有别的 config.json 干扰。
	t.Chdir(t.TempDir())
	clearConfigEnv(t)
	t.Setenv("AVDB_CONFIG_DIR", dir)
	t.Setenv("AVDB_API_BASE_URL", upstream.URL)

	cfg, notes := ResolveConfig()
	if cfg.MaxResults != 0 {
		t.Fatalf("存量配置里的 max_results=5000 应被忽略，实际 MaxResults = %d", cfg.MaxResults)
	}

	// 退役字段必须报一声，不能静默改变行为。
	warned := false
	for _, n := range notes {
		if strings.Contains(n, "max_results") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("退役字段应产生提示，实际 notes = %v", notes)
	}

	app := NewApp(cfg, discardLogger())
	body := do(app, http.MethodGet, "/s?q=x").Body.String()
	if n := strings.Count(body, `class="row"`); n != total {
		t.Errorf("应渲染全部 %d 条，实际 %d（存量 max_results 仍在截断）", total, n)
	}
	if strings.Contains(body, "结果过多") {
		t.Error("存量配置下也不应出现截断提示")
	}
}
