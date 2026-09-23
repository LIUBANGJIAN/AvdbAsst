package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ===========================================================================
// 结果页筛选的测试
//
// 这里守的核心是一个曾经真实存在的缺陷：筛选只在前端对**当前页的 DOM** 生效，
// 于是筛完翻到第 2 页条件就没了。本文件同时测"筛选算得对"和"翻页带得住"。
// ===========================================================================

func filterFromQuery(t *testing.T, raw string) searchFilter {
	t.Helper()
	return parseSearchFilter(httptest.NewRequest(http.MethodGet, "/s?"+raw, nil))
}

// ------------------------------------------------------------- 条件解析

func TestParseSearchFilterDropsUnknownValues(t *testing.T) {
	f := filterFromQuery(t, "site=javdb&section=%E6%9C%89%E7%A0%81&flags=hd,cn,evil,hd&sort=size-desc&filter=%E4%B8%AD%E6%96%87")

	if f.Site != "javdb" {
		t.Errorf("Site = %q，期望 javdb", f.Site)
	}
	if f.Section != "有码" {
		t.Errorf("Section = %q，期望 有码", f.Section)
	}
	if f.Sort != "size-desc" {
		t.Errorf("Sort = %q，期望 size-desc", f.Sort)
	}
	// 白名单外的 evil 与重复的 hd 都要丢掉，并且顺序归一为 flagOrder。
	if got := strings.Join(f.Flags, ","); got != "cn,hd" {
		t.Errorf("Flags = %q，期望 cn,hd", got)
	}
	if f.Text != "中文" {
		t.Errorf("Text = %q，期望 中文", f.Text)
	}
}

func TestParseSearchFilterFallsBackToDefaults(t *testing.T) {
	f := filterFromQuery(t, "sort=drop+table+torrents&flags=../../etc/passwd")

	if f.Sort != defaultSort {
		t.Errorf("非法排序值应回落到 %q，实际 %q", defaultSort, f.Sort)
	}
	if len(f.Flags) != 0 {
		t.Errorf("非法属性值应被丢弃，实际 %v", f.Flags)
	}
	if f.active() {
		t.Error("没解析出任何条件时 active() 应为 false")
	}
}

// TestParseSearchFilterRejectsOverlongFacet 守住"超长值视作未选择"。
//
// 截断后拿去比较永远匹配不上，页面会变成一个空列表配一个看不出问题的下拉框，
// 属于最难排查的一种表现。
func TestParseSearchFilterRejectsOverlongFacet(t *testing.T) {
	f := filterFromQuery(t, "site="+strings.Repeat("x", maxFacetLen+1))
	if f.Site != "" {
		t.Errorf("超长筛选项应视作未选择，实际 %q", f.Site)
	}

	// 恰好等于上限的值应当保留，别把边界一起砍掉。
	ok := strings.Repeat("y", maxFacetLen)
	if got := filterFromQuery(t, "site="+ok).Site; got != ok {
		t.Errorf("长度等于上限的取值应保留，实际长度 %d", len(got))
	}
}

func TestParseSearchFilterTruncatesOverlongText(t *testing.T) {
	long := strings.Repeat("中", maxFilterTextLen+50)
	f := filterFromQuery(t, "filter="+url.QueryEscape(long))
	if n := len([]rune(f.Text)); n != maxFilterTextLen {
		t.Errorf("标题过滤表达式应截断到 %d 个字符，实际 %d", maxFilterTextLen, n)
	}
}

// ------------------------------------------------------------- 条件匹配

func TestSearchFilterMatchesFacets(t *testing.T) {
	items := []Torrent{
		{Site: "a", Section: "s1", Category: "c1", Chinese: true},
		{Site: "a", Section: "s2", Category: "c1", HD: true},
		{Site: "b", Section: "s1", Category: "c2", UHD: true},
	}

	cases := []struct {
		name string
		f    searchFilter
		want int
	}{
		{"站点", searchFilter{Site: "a"}, 2},
		{"板块", searchFilter{Section: "s1"}, 2},
		{"分类", searchFilter{Category: "c2"}, 1},
		{"单属性", searchFilter{Flags: []string{"hd"}}, 1},
		// 多个属性之间是"与"：同时要中文又要高清，只有都满足的才算。
		{"多属性取交集", searchFilter{Flags: []string{"cn", "hd"}}, 0},
		{"站点+分类", searchFilter{Site: "a", Category: "c1"}, 2},
		{"组合到只剩一条", searchFilter{Site: "a", Section: "s2"}, 1},
		{"无匹配", searchFilter{Site: "nope"}, 0},
	}

	for _, tc := range cases {
		if got := len(tc.f.apply(items)); got != tc.want {
			t.Errorf("%s：命中 %d 条，期望 %d", tc.name, got, tc.want)
		}
	}
}

// TestUncensoredAliasCountsAsFlag 确认属性筛选认 uc 这个别名。
// 上游同时用 uncensored 与 uc 两个字段表达同一件事，只认一个会漏筛。
func TestUncensoredAliasCountsAsFlag(t *testing.T) {
	items := []Torrent{
		{Title: "x", Uncensored: true},
		{Title: "y", UC: true},
		{Title: "z"},
	}
	if got := len(searchFilter{Flags: []string{"unc"}}.apply(items)); got != 2 {
		t.Errorf("uncensored 与 uc 都应算「无码」，命中 %d 条，期望 2", got)
	}
}

func TestSearchFilterTextTerms(t *testing.T) {
	items := []Torrent{
		{Title: "ABC-123 中文字幕 4K"},
		{Title: "abc-456 无码"},
		{Title: "XYZ-789"},
	}

	cases := []struct {
		expr string
		want int
	}{
		{"abc", 2},      // 不区分大小写
		{"ABC 中文字幕", 1}, // 多个词之间是"与"
		{"abc -无码", 1},  // - 前缀表示排除（反向过滤）
		{"abc !4K", 1},  // ! 也一样
		{"abc，无码", 1},   // 中文逗号也当分隔符
		{"abc、无码", 1},   // 顿号同理
		{"-abc", 1},     // 只给排除词也成立
		{"abc -", 2},    // 落单的减号忽略掉，不是"排除空串"
		{"  ", 3},       // 全是空白等于没过滤
		{"不存在的词", 0},
	}

	for _, tc := range cases {
		f := searchFilter{Text: tc.expr}
		if got := len(f.apply(items)); got != tc.want {
			t.Errorf("过滤 %q：命中 %d 条，期望 %d", tc.expr, got, tc.want)
		}
	}
}

// TestApplyReturnsPrivateSlice 守住 apply 的返回值不与入参共享底层数组。
//
// 返回的切片紧接着要被 sortTorrents 就地重排，而入参很可能是搜索缓存持有的
// 那一份——共享的话会把缓存里的顺序改掉，用户切回「默认」排序时
// 再也拿不到上游原本的次序。
func TestApplyReturnsPrivateSlice(t *testing.T) {
	items := []Torrent{{Site: "a"}, {Site: "b"}}

	// 两个分支都要覆盖：无筛选（走拷贝）与有筛选（走重建）。
	for name, f := range map[string]searchFilter{
		"无筛选": {},
		"有筛选": {Site: "a"},
	} {
		got := f.apply(items)
		if len(got) == 0 {
			t.Fatalf("%s：apply 不应返回空切片", name)
		}
		got[0] = Torrent{Site: "mutated"}
		if items[0].Site != "a" {
			t.Errorf("%s：apply 的返回值与入参共享底层数组，就地排序会污染搜索缓存", name)
		}
	}
}

// --------------------------------------------------------------- 排序

func sortTitles(items []Torrent, mode string) string {
	sortTorrents(items, mode)
	out := make([]string, 0, len(items))
	for _, t := range items {
		out = append(out, t.Title)
	}
	return strings.Join(out, ",")
}

func TestSortTorrents(t *testing.T) {
	base := func() []Torrent {
		return []Torrent{
			{Title: "mid", SizeMB: 5, SortTS: 200},
			{Title: "big", SizeMB: 9, SortTS: 100},
			{Title: "small", SizeMB: 1, SortTS: 300},
		}
	}

	cases := []struct {
		mode string
		want string
	}{
		{"default", "mid,big,small"}, // 默认保持上游顺序
		{"size-desc", "big,mid,small"},
		{"size-asc", "small,mid,big"},
		{"time-desc", "small,mid,big"},
		{"time-asc", "big,mid,small"},
		{"不认识的模式", "mid,big,small"}, // 非法值不能让结果乱序
	}

	for _, tc := range cases {
		if got := sortTitles(base(), tc.mode); got != tc.want {
			t.Errorf("排序 %q → %s，期望 %s", tc.mode, got, tc.want)
		}
	}
}

// TestSortTorrentsPutsUnknownTimeLast 确认解析不出时间的条目沉底。
//
// SortTS 为 0 表示上游没给时间或解析失败。若当成"1970 年"参与排序，
// 升序时它们会齐刷刷抢到最前面，看起来像一堆莫名其妙的老资源。
func TestSortTorrentsPutsUnknownTimeLast(t *testing.T) {
	items := []Torrent{
		{Title: "unknown", SortTS: 0},
		{Title: "old", SortTS: 100},
		{Title: "new", SortTS: 900},
	}

	if got := sortTitles(append([]Torrent{}, items...), "time-asc"); got != "old,new,unknown" {
		t.Errorf("time-asc → %s，期望 old,new,unknown", got)
	}
	if got := sortTitles(append([]Torrent{}, items...), "time-desc"); got != "new,old,unknown" {
		t.Errorf("time-desc → %s，期望 new,old,unknown", got)
	}
}

// TestSortTorrentsIsStable 确认同值条目保持原顺序。
// 不稳定的话，翻页边界上的条目会来回串位，用户会看到"这条我刚才见过"。
func TestSortTorrentsIsStable(t *testing.T) {
	items := []Torrent{
		{Title: "a", SizeMB: 5},
		{Title: "b", SizeMB: 5},
		{Title: "c", SizeMB: 5},
	}
	if got := sortTitles(items, "size-desc"); got != "a,b,c" {
		t.Errorf("同体积条目应保持原顺序，实际 %s", got)
	}
}

// ------------------------------------------------------------ URL 生成

func TestSearchFilterURLKeepsEveryCondition(t *testing.T) {
	f := searchFilter{
		Site:     "javdb",
		Section:  "有码",
		Category: "c1",
		Flags:    []string{"cn", "hd"},
		Sort:     "size-desc",
		Text:     "中文 -无码",
	}
	u, err := url.Parse(f.pageURL("abc", 3))
	if err != nil {
		t.Fatalf("生成的地址无法解析: %v", err)
	}
	q := u.Query()

	want := map[string]string{
		"q":        "abc",
		"page":     "3",
		"site":     "javdb",
		"section":  "有码",
		"category": "c1",
		"flags":    "cn,hd",
		"sort":     "size-desc",
		"filter":   "中文 -无码",
	}
	for key, value := range want {
		if got := q.Get(key); got != value {
			t.Errorf("翻页链接的 %s = %q，期望 %q（完整地址 %s）", key, got, value, u.String())
		}
	}
}

func TestSearchFilterURLOmitsNoise(t *testing.T) {
	// 第 1 页不写 page，默认排序不写 sort：筛选后的首页该有一个干净的地址。
	f := searchFilter{Site: "javdb", Sort: defaultSort}
	got := f.pageURL("abc", 1)
	if strings.Contains(got, "page=") {
		t.Errorf("第 1 页不应带 page 参数：%s", got)
	}
	if strings.Contains(got, "sort=") {
		t.Errorf("默认排序不应写进地址：%s", got)
	}
	if got != "/s?q=abc&site=javdb" {
		t.Errorf("pageURL = %q，期望 /s?q=abc&site=javdb", got)
	}
}

func TestChipURLTogglesOnlyThatFlag(t *testing.T) {
	f := searchFilter{Site: "javdb", Flags: []string{"cn"}}

	chips := f.chips("abc")
	if len(chips) != len(flagOrder) {
		t.Fatalf("应有 %d 个属性按钮，实际 %d", len(flagOrder), len(chips))
	}

	byFlag := make(map[string]chipView, len(chips))
	for _, chip := range chips {
		byFlag[chip.Flag] = chip
		if chip.Label == "" {
			t.Errorf("属性 %q 缺少展示名", chip.Flag)
		}
	}

	// 已选中的：点一下取消，但站点条件必须留着。
	cn := byFlag["cn"]
	if !cn.On {
		t.Error("已选的 cn 应标记为选中")
	}
	if cn.URL != "/s?q=abc&site=javdb" {
		t.Errorf("取消 cn 的地址 = %q，期望只剩站点条件", cn.URL)
	}

	// 未选中的：点一下加上，并且原有条件保留。
	hd := byFlag["hd"]
	if hd.On {
		t.Error("未选的 hd 不应标记为选中")
	}
	// 逐项比较而不是比对整串：参数顺序由 url.Values 决定，
	// 把顺序写进断言只会让测试变得脆弱。
	q := mustQuery(t, hd.URL)
	for key, want := range map[string]string{"flags": "cn,hd", "site": "javdb", "q": "abc"} {
		if got := q.Get(key); got != want {
			t.Errorf("选中 hd 的地址里 %s = %q，期望 %q（完整地址 %s）", key, got, want, hd.URL)
		}
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("地址无法解析 %q: %v", raw, err)
	}
	return u.Query()
}

// TestFacetListIsDeterministic 是本项目最重要的一条回归测试。
//
// 旧实现直接遍历 map 生成筛选项，导致同一份数据每次渲染顺序都不同，
// 属于"同一输入两次结果不一致"的安静型故障。这里连跑 200 次做校验。
func TestFacetListIsDeterministic(t *testing.T) {
	items := []Torrent{
		{Site: "zeta", Section: "s2", Category: "c3"},
		{Site: "alpha", Section: "s1", Category: "c1"},
		{Site: "mike", Section: "s3", Category: "c2"},
		{Site: "alpha", Section: "s1", Category: "c1"},
		{Site: "beta", Section: "s2", Category: "c3"},
	}
	sitesOf := func() string {
		return strings.Join(facetList(items, func(t Torrent) string { return t.Site }), ",")
	}
	sectionsOf := func() string {
		return strings.Join(facetList(items, func(t Torrent) string { return t.Section }), ",")
	}
	categoriesOf := func() string {
		return strings.Join(facetList(items, func(t Torrent) string { return t.Category }), ",")
	}

	if got := sitesOf(); got != "alpha,beta,mike,zeta" {
		t.Fatalf("站点筛选未按字典序去重: %q", got)
	}
	if got := sectionsOf(); got != "s1,s2,s3" {
		t.Fatalf("板块筛选错误: %q", got)
	}
	if got := categoriesOf(); got != "c1,c2,c3" {
		t.Fatalf("分类筛选错误: %q", got)
	}

	for i := 0; i < 200; i++ {
		if sitesOf() != "alpha,beta,mike,zeta" ||
			sectionsOf() != "s1,s2,s3" ||
			categoriesOf() != "c1,c2,c3" {
			t.Fatalf("第 %d 次调用结果与首次不一致，说明存在 map 遍历顺序泄漏", i)
		}
	}
}

func TestFacetListSkipsEmpty(t *testing.T) {
	items := []Torrent{
		{Site: "", Section: "", Category: ""},
		{Site: "a", Section: "", Category: ""},
	}
	sites := facetList(items, func(t Torrent) string { return t.Site })
	if len(sites) != 1 || sites[0] != "a" {
		t.Errorf("站点筛选应只含 a，实际 %v", sites)
	}
	if n := len(facetList(items, func(t Torrent) string { return t.Section })); n != 0 {
		t.Errorf("空值不应进入筛选列表，实际 %d 项", n)
	}
}

func TestResetURLClearsFiltersButKeepsKeyword(t *testing.T) {
	f := searchFilter{Site: "javdb", Flags: []string{"hd"}, Text: "x", Sort: "time-desc"}
	if got := f.resetURL("abc"); got != "/s?q=abc" {
		t.Errorf("resetURL = %q，期望 /s?q=abc", got)
	}
	if got := f.resetURL(""); got != "/s" {
		t.Errorf("无关键词时 resetURL = %q，期望 /s", got)
	}
}

// TestHiddenParamsCarryFiltersButNotPage 守住跳页表单不丢筛选、也不带页码。
//
// 改筛选条件后应该回到第 1 页；把 page 一起提交过去，用户会停在
// 一个可能根本不存在的页码上。
func TestHiddenParamsCarryFiltersButNotPage(t *testing.T) {
	f := searchFilter{Site: "javdb", Flags: []string{"hd"}, Sort: "time-desc", Text: "x"}

	first := f.hiddenParams("kw")
	got := make(map[string]string, len(first))
	for _, p := range first {
		got[p.Name] = p.Value
	}

	for _, name := range []string{"q", "site", "flags", "sort", "filter"} {
		if _, ok := got[name]; !ok {
			t.Errorf("跳页表单缺少 %s 字段，跳一次就会丢掉这项筛选", name)
		}
	}
	if _, ok := got["page"]; ok {
		t.Error("跳页表单不该带 page")
	}

	// 输出顺序必须稳定，否则页面 diff 与断言都没法看。
	second := f.hiddenParams("kw")
	if len(first) != len(second) {
		t.Fatalf("两次调用的字段数不一致：%d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("第 %d 个字段不稳定：%+v vs %+v", i, first[i], second[i])
		}
	}
}

func TestFacetOptionsMarkCurrentValue(t *testing.T) {
	options := facetOptions([]string{"a", "b"}, "b")
	if len(options) != 3 {
		t.Fatalf("应包含「全部」与两个取值，实际 %d 项", len(options))
	}
	if options[0].Value != "" || options[0].Selected {
		t.Errorf("首项应为未选中的「全部」，实际 %+v", options[0])
	}
	if options[2].Value != "b" || !options[2].Selected {
		t.Errorf("当前值 b 应被标记选中，实际 %+v", options[2])
	}

	// 没有筛选时「全部」必须是选中态，否则下拉框看起来像没加载完。
	if !facetOptions([]string{"a"}, "")[0].Selected {
		t.Error("未筛选时「全部」应选中")
	}
}

// TestFacetOptionsKeepsUnknownCurrent 守住"当前值不在候选里"的兜底。
//
// 手改 URL、或上游数据变了，都可能让筛选项指向一个候选里没有的值。
// 这时下拉如果只显示「全部」，用户看到的就是"筛着某个看不见的东西、
// 结果一个都没有"，而且完全找不到线索。
func TestFacetOptionsKeepsUnknownCurrent(t *testing.T) {
	options := facetOptions([]string{"a", "b"}, "已下线的站")

	if len(options) != 4 {
		t.Fatalf("应补出当前值，共 4 项，实际 %d 项", len(options))
	}
	last := options[3]
	if last.Value != "已下线的站" || !last.Selected {
		t.Errorf("补出的当前值应被标记选中，实际 %+v", last)
	}
	if !strings.Contains(last.Label, "无匹配") {
		t.Errorf("补出的项要说明它没有匹配，实际标签 %q", last.Label)
	}
	// 「全部」这时不能被标成选中，否则等于告诉用户"没在筛"。
	if options[0].Selected {
		t.Error("当前有筛选时「全部」不应选中")
	}
}

// ------------------------------------------------- 与页面/接口的端到端

// filterableTorrents 生成 n 条可筛选的数据：站点、分类、属性、标题都按序号交错，
// 偶数条属于 Alpha（中文+高清），奇数条属于 Beta，便于断言"筛出多少条"。
func filterableTorrents(n int) string {
	var b strings.Builder
	b.WriteString(`{"code":0,"message":"操作成功","data":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		site, category, chinese, hd := "Alpha", "Cat1", "true", "true"
		if i%2 == 1 {
			site, category, chinese, hd = "Beta", "Cat2", "false", "false"
		}
		uhd := "false"
		if i%4 == 0 {
			uhd = "true"
		}
		fmt.Fprintf(&b, `{"id":%d,"number":"N-%03d","site":"%s","section":"Sec","category":"%s",
			"size_mb":%d,"post_time":"2026-01-02 03:04:05","seeders":5,
			"title":"条目 %03d %s","chinese":%s,"hd":%s,"uhd":%s,
			"download_url":"magnet:?xt=urn:btih:%040d"}`,
			i+1, i+1, site, category, 1000-i, i+1, strings.ToUpper(site),
			chinese, hd, uhd, i+1)
	}
	b.WriteString(`]}`)
	return b.String()
}

// TestSearchFilterSurvivesPagination 是本轮投诉的回归测试：
// "选择后，翻页会失效了，需要重新选择"。
func TestSearchFilterSurvivesPagination(t *testing.T) {
	app := newTestApp(t, upstreamWith(filterableTorrents(250)), func(c *Config) { c.PageSize = 100 })

	// 只筛 Alpha：共 125 条，每页 100 → 2 页。
	body := do(app, http.MethodGet, "/s?q=x&site=Alpha").Body.String()

	if !strings.Contains(body, "筛选出 <strong>125</strong> 条（全部 250 条）") {
		t.Errorf("统计行应说明筛选后的条数；片段: %s", excerpt(body, "class=\"stats\""))
	}
	if n := strings.Count(body, `class="row"`); n != 100 {
		t.Errorf("第 1 页渲染 %d 条，期望 100", n)
	}
	if strings.Contains(body, `data-site="Beta"`) {
		t.Error("筛选之后不应再出现其它站点的条目")
	}
	// 翻页链接必须带着筛选条件——这正是"翻页就失效"的病根。
	// 注意模板会把 & 转义成 &amp;。
	if !strings.Contains(body, `href="/s?page=2&amp;q=x&amp;site=Alpha"`) {
		t.Errorf("翻页链接应保留筛选条件；片段: %s", excerpt(body, "pager-btn"))
	}
	// 下拉框要显示当前选中的值，否则用户会以为筛选丢了。
	if !strings.Contains(body, `<option value="Alpha" selected>`) {
		t.Errorf("站点下拉应回显当前筛选值；片段: %s", excerpt(body, "f-site"))
	}
	// 属性按钮的地址也要带上站点，点它不该把站点条件清掉。
	if !strings.Contains(body, `href="/s?flags=cn&amp;q=x&amp;site=Alpha"`) {
		t.Errorf("属性按钮应保留其它筛选条件；片段: %s", excerpt(body, "chips"))
	}

	// 第 2 页：条件同样有效，只剩 25 条，且序号接着走。
	page2 := do(app, http.MethodGet, "/s?q=x&page=2&site=Alpha").Body.String()
	if n := strings.Count(page2, `class="row"`); n != 25 {
		t.Errorf("第 2 页渲染 %d 条，期望 25（125 条筛出结果的余数）", n)
	}
	if strings.Contains(page2, `data-site="Beta"`) {
		t.Error("第 2 页也必须遵守筛选条件")
	}
	if !strings.Contains(page2, `class="row-index" aria-hidden="true">101<`) {
		t.Errorf("第 2 页首条序号应为 101（按筛选后的结果集编号）；片段: %s", excerpt(page2, "row-index"))
	}
}

// TestTitleFilterEndToEnd 确认标题过滤（含反向过滤）在服务端生效并可翻页。
func TestTitleFilterEndToEnd(t *testing.T) {
	app := newTestApp(t, upstreamWith(filterableTorrents(250)), func(c *Config) { c.PageSize = 300 })

	params := url.Values{"q": {"x"}, "filter": {"beta"}}
	body := do(app, http.MethodGet, "/s?"+params.Encode()).Body.String()
	if n := strings.Count(body, `class="row"`); n != 125 {
		t.Errorf("标题含 beta 的应有 125 条，实际 %d", n)
	}
	// 输入框要回显，否则用户看不到自己填了什么。
	if !strings.Contains(body, `value="beta"`) {
		t.Errorf("标题过滤框应回显当前值；片段: %s", excerpt(body, "f-filter"))
	}

	// 反向过滤：排除 beta，只剩 Alpha 那 125 条。
	params = url.Values{"q": {"x"}, "filter": {"-beta"}}
	body = do(app, http.MethodGet, "/s?"+params.Encode()).Body.String()
	if n := strings.Count(body, `class="row"`); n != 125 {
		t.Errorf("排除 beta 后应剩 125 条，实际 %d", n)
	}
	if strings.Contains(body, "BETA") {
		t.Error("反向过滤后不应出现被排除的条目")
	}
}

// TestFilterWithNoMatchKeepsToolbar 守住"筛剩下 0 条"时的页面。
//
// 工具条必须还在：否则用户没法把条件改回来，只能去手工编辑 URL。
// 空态文案也要区分"搜不到"与"筛没的"。
func TestFilterWithNoMatchKeepsToolbar(t *testing.T) {
	app := newTestApp(t, upstreamWith(filterableTorrents(50)))

	body := do(app, http.MethodGet, "/s?q=x&site=Gamma").Body.String()

	if !strings.Contains(body, "当前筛选条件下没有结果") {
		t.Errorf("应显示筛选无结果的空态；片段: %s", excerpt(body, "empty"))
	}
	if strings.Contains(body, "没有找到") {
		t.Error("这是筛没的，不是搜不到，不应套用「没有找到」的文案")
	}
	for _, want := range []string{`id="filter-form"`, `id="f-site"`, `id="f-filter"`, "重置筛选"} {
		if !strings.Contains(body, want) {
			t.Errorf("筛选无结果时工具条仍需可用，缺少 %s", want)
		}
	}

	// 换成标题过滤词筛空，结果应当一样。
	params := url.Values{"q": {"x"}, "filter": {"根本不存在的词"}}
	body = do(app, http.MethodGet, "/s?"+params.Encode()).Body.String()
	if !strings.Contains(body, "当前筛选条件下没有结果") {
		t.Error("过滤词无命中时也应给出筛选空态")
	}
	if !strings.Contains(body, `id="filter-form"`) {
		t.Error("过滤到空时工具条同样要留着")
	}

	// 上游一条都没返回时是另一回事：那是真"搜不到"，不该出现工具条。
	empty := newTestApp(t, upstreamWith(manyTorrents(0)))
	body = do(empty, http.MethodGet, "/s?q=x").Body.String()
	if !strings.Contains(body, "没有找到") {
		t.Error("上游无结果时应保留「没有找到」的文案")
	}
	if strings.Contains(body, `id="filter-form"`) {
		t.Error("上游无结果时不该渲染筛选工具条")
	}
}

// TestAPISearchAppliesSameFilter 确认接口与页面用同一份筛选，不给两套结果。
func TestAPISearchAppliesSameFilter(t *testing.T) {
	app := newTestApp(t, upstreamWith(filterableTorrents(250)))

	rec := do(app, http.MethodGet, "/api/search?q=x&site=Alpha&flags=cn")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}

	var payload struct {
		Total      int            `json:"total"`
		TotalAll   int            `json:"total_all"`
		Filtered   bool           `json:"filtered"`
		Count      int            `json:"count"`
		Sites      []string       `json:"sites"`
		Torrents   []Torrent      `json:"torrents"`
		Applied    *appliedFilter `json:"applied_filters"`
		TotalPages int            `json:"total_pages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}

	if !payload.Filtered {
		t.Error("施加了筛选时 filtered 应为 true")
	}
	if payload.TotalAll != 250 {
		t.Errorf("total_all = %d，期望筛选前的 250", payload.TotalAll)
	}
	if payload.Total != 125 {
		t.Errorf("total = %d，期望筛选后的 125", payload.Total)
	}
	// 筛选项列表仍基于全量：否则调用方拿到的下拉候选会随筛选而收缩。
	if len(payload.Sites) != 2 {
		t.Errorf("sites = %v，期望仍是全部 2 个站点", payload.Sites)
	}
	for _, item := range payload.Torrents {
		if item.Site != "Alpha" {
			t.Errorf("接口返回了未命中筛选的条目：%+v", item)
			break
		}
	}
	if payload.Applied == nil || payload.Applied.Site != "Alpha" {
		t.Errorf("applied_filters 应说明生效的条件，实际 %+v", payload.Applied)
	}
}

// TestAPISearchWithoutFilterReportsTotalAll 确认没筛选时前后一致，
// 免得调用方以为 total_all 永远比 total 大。
func TestAPISearchWithoutFilterReportsTotalAll(t *testing.T) {
	app := newTestApp(t, upstreamWith(filterableTorrents(30)))

	var payload struct {
		Total      int  `json:"total"`
		TotalAll   int  `json:"total_all"`
		Filtered   bool `json:"filtered"`
		TotalPages int  `json:"total_pages"`
	}
	if err := json.Unmarshal(do(app, http.MethodGet, "/api/search?q=x").Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if payload.Filtered {
		t.Error("没有筛选时 filtered 应为 false")
	}
	if payload.Total != payload.TotalAll || payload.Total != 30 {
		t.Errorf("未筛选时 total / total_all 都应为 30，实际 %d / %d", payload.Total, payload.TotalAll)
	}
}
