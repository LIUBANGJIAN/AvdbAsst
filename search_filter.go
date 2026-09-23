package main

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ===========================================================================
// 结果页筛选
//
// 这里是"筛选"唯一的一份实现，页面与 /api/search 共用。
//
// 为什么必须放在服务端：
// 早先这些条件只在前端对**当前页的 DOM** 过滤，于是"筛选"和"分页"互相拆台——
// 筛完翻到第 2 页，条件没了，得重选一遍；就算勉强留住，第 2 页也只是
// "全量第 101~200 条里恰好符合条件的那几条"，条数忽多忽少、序号还会跳。
// 现在条件写进 URL（可收藏、可分享、可后退），在**全量结果**上生效后再分页，
// 翻页链接里带着同一份条件，翻到哪一页都是同一组筛选。
// ===========================================================================

// flagOrder 固定属性筛选的顺序，同时充当白名单：
// URL 里冒出别的值一律丢弃，不让任意字符串顺着链接流进页面。
var flagOrder = []string{"cn", "unc", "uhd", "hd"}

// flagLabels 是属性筛选的展示名，键与 flagOrder 对应。
var flagLabels = map[string]string{
	"cn":  "中文字幕",
	"unc": "无码",
	"uhd": "4K",
	"hd":  "高清",
}

// defaultSort 是排序的默认值，也是排序白名单的兜底项。
const defaultSort = "default"

// sortModes 是排序白名单，顺序即下拉框顺序。
var sortModes = []struct{ Value, Label string }{
	{"default", "默认"},
	{"size-desc", "体积从大到小"},
	{"size-asc", "体积从小到大"},
	{"time-desc", "最新发布"},
	{"time-asc", "最早发布"},
}

// maxFacetLen 限制单个筛选项（站点/板块/分类）的长度。
//
// 这些值来自上游，会被写进 URL，再原样回显在下拉框里。万一上游塞进一段超长
// 文本，翻页链接会被撑到几 KB。超长的值直接视作"没选"，而不是截断后拿去比较
// ——截断的值永远匹配不上任何条目，页面会变成一个空列表配一个看不出问题的
// 下拉框，属于最难排查的那种。
const maxFacetLen = 64

// maxFilterTextLen 限制标题过滤表达式的长度。200 个字符做过滤已经很宽裕，
// 主要是别让 URL 无限膨胀。
const maxFilterTextLen = 200

// formParam 是模板里一个 <input type="hidden"> 的键值对。
//
// 存在的理由：翻页、跳页、改每页条数都要把筛选条件原样带上，
// 分散在模板里手写 hidden 迟早会漏掉一个——那处的筛选就又"失效"了。
type formParam struct {
	Name  string
	Value string
}

// selectOption 是下拉框里的一项。Selected 由服务端算好，
// 省得模板里写一串 {{if eq ...}}：那种写法一多，漏一个的表现就是
// "选了却不显示选中"，还很难一眼看出。
type selectOption struct {
	Value    string
	Label    string
	Selected bool
}

// chipView 是一个属性筛选按钮。用链接而不是按钮，
// 是为了让它在禁用 JS 时照样能用（点一下就带条件重新加载）。
type chipView struct {
	Flag  string
	Label string
	URL   string
	On    bool
}

// searchFilter 是一次结果页筛选的全部条件，字段与 URL 参数一一对应。
type searchFilter struct {
	Site     string
	Section  string
	Category string
	// Flags 是属性筛选，多个之间是"与"：选了中文字幕又选 4K，要同时满足。
	Flags []string
	// Sort 取值见 sortModes，恒非空（非法值归默认）。
	Sort string
	// Text 是标题过滤表达式：分词后前缀带 - 或 ! 的词表示排除。
	Text string
}

// parseSearchFilter 从 query 解析筛选条件。所有取值都过白名单或长度上限，
// 因为它们是能被随手改的 URL 参数。
func parseSearchFilter(r *http.Request) searchFilter {
	q := r.URL.Query()
	return searchFilter{
		Site:     sanitizeFacet(q.Get("site")),
		Section:  sanitizeFacet(q.Get("section")),
		Category: sanitizeFacet(q.Get("category")),
		Flags:    normalizeFlags(q.Get("flags")),
		Sort:     normalizeSort(q.Get("sort")),
		Text:     sanitizeFilterText(q.Get("filter")),
	}
}

// active 表示当前是否施加了任何筛选（排序不算——它不改变结果集大小）。
// 页面上据此决定要不要显示「重置」，以及统计行怎么说。
func (f searchFilter) active() bool {
	return f.Site != "" || f.Section != "" || f.Category != "" ||
		len(f.Flags) > 0 || f.Text != ""
}

// apply 在**全量**结果上过滤，并返回一份调用方可以自由修改的新切片。
//
// 为什么没有筛选时也要拷贝：紧接着要按排序模式**就地重排**，而入参很可能是
// 搜索缓存持有的那一份切片。就地排序会把缓存里的顺序改掉，等用户切回「默认」
// 排序时就再也拿不到上游原本的次序了——这种错看起来只是"顺序有点怪"，
// 极难联想到是几屏之前的某次排序造成的。
// 几千条的拷贝是微秒级开销，不值得为省它冒这个险。
func (f searchFilter) apply(items []Torrent) []Torrent {
	if !f.active() {
		out := make([]Torrent, len(items))
		copy(out, items)
		return out
	}
	include, exclude := f.terms()
	out := make([]Torrent, 0, len(items))
	for _, t := range items {
		if f.matches(t, include, exclude) {
			out = append(out, t)
		}
	}
	return out
}

// matches 判断单条资源是否命中筛选。include / exclude 由 terms() 预解析，
// 避免在几千条的循环里反复解析同一个表达式。
func (f searchFilter) matches(t Torrent, include, exclude []string) bool {
	if f.Site != "" && t.Site != f.Site {
		return false
	}
	if f.Section != "" && t.Section != f.Section {
		return false
	}
	if f.Category != "" && t.Category != f.Category {
		return false
	}
	if len(f.Flags) > 0 {
		hit := make(map[string]bool, 4)
		for _, flag := range torrentFlags(t) {
			hit[flag] = true
		}
		for _, flag := range f.Flags {
			if !hit[flag] {
				return false
			}
		}
	}
	if len(include) == 0 && len(exclude) == 0 {
		return true
	}

	title := strings.ToLower(t.Title)
	for _, term := range include {
		if !strings.Contains(title, term) {
			return false
		}
	}
	for _, term := range exclude {
		if strings.Contains(title, term) {
			return false
		}
	}
	return true
}

// filterSeparators 定义标题过滤表达式的分词规则。
//
// 除了空白，还认中英文逗号与顿号：中文用户输入多个词时很自然会打「，」，
// 若只认空格，整串"词1，词2"会被当成一个词，什么都筛不出来。
var filterSeparators = func(r rune) bool {
	return unicode.IsSpace(r) || r == ',' || r == '，' || r == '、' || r == ';' || r == '；'
}

// terms 把「标题过滤」输入框的内容拆成"必须包含"与"必须不包含"两组词。
//
// 语法刻意做得极简：分词后，前缀带 - 或 ! 的词表示排除，其余表示必须包含。
// 不加"包含/排除"切换按钮的理由和下载器那处一样——状态型按钮一多，
// 用户得先看清当前处在哪个模式才敢输入；写在输入框里则所见即所得。
func (f searchFilter) terms() (include, exclude []string) {
	seenIn := make(map[string]bool, 4)
	seenEx := make(map[string]bool, 4)
	for _, raw := range strings.FieldsFunc(f.Text, filterSeparators) {
		term := strings.ToLower(raw)
		excluded := false
		if strings.HasPrefix(term, "-") || strings.HasPrefix(term, "!") {
			excluded = true
			term = strings.TrimLeft(term, "-!")
		}
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		if excluded {
			if !seenEx[term] {
				seenEx[term] = true
				exclude = append(exclude, term)
			}
			continue
		}
		if !seenIn[term] {
			seenIn[term] = true
			include = append(include, term)
		}
	}
	return include, exclude
}

// torrentFlags 返回一条资源命中的属性标记。
//
// 属性判定只此一份：列表上的标签、data-flags、服务端筛选共用它，
// 否则"标签明明显示有、筛选却筛不出来"这种事迟早会发生。
func torrentFlags(t Torrent) []string {
	out := make([]string, 0, 4)
	if t.Chinese {
		out = append(out, "cn")
	}
	if t.Uncensored || t.UC {
		out = append(out, "unc")
	}
	if t.UHD {
		out = append(out, "uhd")
	}
	if t.HD {
		out = append(out, "hd")
	}
	return out
}

// sortTorrents 按排序模式就地重排。
//
// 用稳定排序：同分条目保持上游给出的顺序，翻页时才不会出现
// "两条同样大小、每次刷新谁在前还不一样"的抖动，也不会把分页切碎。
func sortTorrents(items []Torrent, mode string) {
	switch mode {
	case "size-desc":
		sort.SliceStable(items, func(i, j int) bool { return items[i].SizeMB > items[j].SizeMB })
	case "size-asc":
		sort.SliceStable(items, func(i, j int) bool { return items[i].SizeMB < items[j].SizeMB })
	case "time-desc":
		sort.SliceStable(items, func(i, j int) bool { return tsGreater(items[i].SortTS, items[j].SortTS) })
	case "time-asc":
		sort.SliceStable(items, func(i, j int) bool { return tsLess(items[i].SortTS, items[j].SortTS) })
	}
}

// tsLess / tsGreater 处理发布时间缺失（SortTS == 0）的比较。
//
// 0 表示上游没给时间或解析失败，不能当成"1970 年"参与排序：升序时它们会
// 齐刷刷抢到最前面，看起来像一堆莫名其妙的老资源。这类条目一律沉底。
func tsLess(a, b int64) bool {
	if a == 0 {
		return false
	}
	if b == 0 {
		return true
	}
	return a < b
}

func tsGreater(a, b int64) bool {
	if a == 0 {
		return false
	}
	if b == 0 {
		return true
	}
	return a > b
}

// pageURL 生成"带着当前筛选条件跳到第 page 页"的链接。
//
// 第 1 页不写 page 参数：筛选后的首页该有一个干净、稳定的地址，
// 而不是 /s?q=x&page=1（那会让人以为是两个不同的结果集）。
func (f searchFilter) pageURL(keyword string, page int) string {
	return buildSearchURL(f.values(keyword, page))
}

// values 汇总一次搜索请求的全部参数（关键词 + 页码 + 筛选）。
func (f searchFilter) values(keyword string, page int) url.Values {
	q := url.Values{}
	if keyword != "" {
		q.Set("q", keyword)
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	f.addTo(q)
	return q
}

// addTo 把筛选条件写进 query，空值一律不写——
// 干净的 URL 才说明"这一项确实没筛"。
func (f searchFilter) addTo(q url.Values) {
	if f.Site != "" {
		q.Set("site", f.Site)
	}
	if f.Section != "" {
		q.Set("section", f.Section)
	}
	if f.Category != "" {
		q.Set("category", f.Category)
	}
	if len(f.Flags) > 0 {
		q.Set("flags", strings.Join(f.Flags, ","))
	}
	if f.Sort != "" && f.Sort != defaultSort {
		q.Set("sort", f.Sort)
	}
	if f.Text != "" {
		q.Set("filter", f.Text)
	}
}

// hiddenParams 生成必须随表单一起提交、但用户看不见的字段。
//
// 跳页表单如果只带关键词，一跳就等于把筛选条件全清了——"跳个页条件就没了"
// 正是本次要修的病，不能在新表单里复发。
//
// 刻意不含 page：改筛选条件后应该回到第 1 页，而不是停在原来的页码上
// 面对一个可能不存在的页。
func (f searchFilter) hiddenParams(keyword string) []formParam {
	q := url.Values{}
	if keyword != "" {
		q.Set("q", keyword)
	}
	f.addTo(q)

	names := make([]string, 0, len(q))
	for name := range q {
		names = append(names, name)
	}
	sort.Strings(names) // 输出顺序稳定，页面 diff 与测试断言都省心

	out := make([]formParam, 0, len(names))
	for _, name := range names {
		out = append(out, formParam{Name: name, Value: q.Get(name)})
	}
	return out
}

// chips 生成属性筛选按钮，每个按钮指向"把这一项取反"的地址。
// 行内其他条件原样保留——点「4K」不该把已经选好的站点清掉。
func (f searchFilter) chips(keyword string) []chipView {
	out := make([]chipView, 0, len(flagOrder))
	for _, flag := range flagOrder {
		next := f
		on := f.hasFlag(flag)
		if on {
			kept := make([]string, 0, len(f.Flags))
			for _, active := range f.Flags {
				if active != flag {
					kept = append(kept, active)
				}
			}
			next.Flags = kept
		} else {
			next.Flags = sortFlags(append(append([]string{}, f.Flags...), flag))
		}
		out = append(out, chipView{
			Flag:  flag,
			Label: flagLabels[flag],
			URL:   next.pageURL(keyword, 1),
			On:    on,
		})
	}
	return out
}

func (f searchFilter) hasFlag(flag string) bool {
	for _, active := range f.Flags {
		if active == flag {
			return true
		}
	}
	return false
}

// resetURL 清掉全部筛选，但保留关键词。
// 用户点「重置」想重来的是筛选条件，不是把搜索结果清空。
func (f searchFilter) resetURL(keyword string) string {
	q := url.Values{}
	if keyword != "" {
		q.Set("q", keyword)
	}
	return buildSearchURL(q)
}

// facetOptions 之类的辅助：把 facet 列表转成带选中态的下拉项。
//
// current 不在候选里时也要把它列出来（手改的 URL、或上游数据变了都可能造成），
// 否则下拉会显示「全部」而实际正在筛一个看不见的值——结果为空时用户
// 完全找不到线索，只能靠手工编辑地址栏。
func facetOptions(values []string, current string) []selectOption {
	out := make([]selectOption, 0, len(values)+2)
	out = append(out, selectOption{Value: "", Label: "全部", Selected: current == ""})

	found := current == ""
	for _, v := range values {
		if v == current {
			found = true
		}
		out = append(out, selectOption{Value: v, Label: v, Selected: v == current})
	}
	if !found {
		out = append(out, selectOption{Value: current, Label: current + "（无匹配）", Selected: true})
	}
	return out
}

// sortOptions 生成排序下拉项。
func sortOptions(current string) []selectOption {
	out := make([]selectOption, 0, len(sortModes))
	for _, mode := range sortModes {
		out = append(out, selectOption{Value: mode.Value, Label: mode.Label, Selected: mode.Value == current})
	}
	return out
}

// buildSearchURL 把参数拼成 /s?... 形式的地址。
func buildSearchURL(q url.Values) string {
	if len(q) == 0 {
		return "/s"
	}
	return "/s?" + q.Encode()
}

// appliedFilter 是当前生效筛选条件的可读摘要，给 /api/search 的调用方看。
// 页面不需要它：条件本来就显示在工具条里。
type appliedFilter struct {
	Site     string   `json:"site,omitempty"`
	Section  string   `json:"section,omitempty"`
	Category string   `json:"category,omitempty"`
	Flags    []string `json:"flags,omitempty"`
	Sort     string   `json:"sort,omitempty"`
	Filter   string   `json:"filter,omitempty"`
}

// applied 在"确实做了什么"时才返回非 nil——一个全是空字段的对象
// 只会让调用方误以为收到了筛选条件。
func (f searchFilter) applied() *appliedFilter {
	if !f.active() && f.Sort == defaultSort {
		return nil
	}
	out := &appliedFilter{
		Site:     f.Site,
		Section:  f.Section,
		Category: f.Category,
		Flags:    f.Flags,
		Filter:   f.Text,
	}
	if f.Sort != defaultSort {
		out.Sort = f.Sort
	}
	return out
}

// facetList 汇总某个字段的去重取值，顺序稳定（排序后）。
//
// 刻意**不做 TrimSpace**：筛选项的值要拿去和 t.Site 这类字段做等值比较，
// 一旦在这里归一化，下拉框里显示的"abc"就永远匹配不上实际存着" abc"的那条，
// 表现是一个永远筛不出结果的选项——最费解的一种空列表。
func facetList(items []Torrent, pick func(Torrent) string) []string {
	set := make(map[string]struct{})
	for _, t := range items {
		if v := pick(t); v != "" {
			set[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// sanitizeFacet 归一化来自 URL 的筛选项取值：去首尾空白、限制长度。
func sanitizeFacet(raw string) string {
	v := strings.TrimSpace(raw)
	if len([]rune(v)) > maxFacetLen {
		return ""
	}
	return v
}

// sanitizeFilterText 归一化标题过滤表达式，超长部分直接截掉
// （它只是过滤词，截断不影响语义，也不会像 facet 那样"截断后永远匹配不上"）。
func sanitizeFilterText(raw string) string {
	v := strings.TrimSpace(raw)
	if r := []rune(v); len(r) > maxFilterTextLen {
		v = strings.TrimSpace(string(r[:maxFilterTextLen]))
	}
	return v
}

// normalizeSort 把排序值收敛到白名单，非法值一律当默认。
func normalizeSort(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	for _, mode := range sortModes {
		if v == mode.Value {
			return mode.Value
		}
	}
	return defaultSort
}

// normalizeFlags 解析 flags=cn,hd 形式的属性筛选，丢弃未知项与重复项。
func normalizeFlags(raw string) []string {
	seen := make(map[string]bool, len(flagOrder))
	out := make([]string, 0, len(flagOrder))
	for _, part := range strings.Split(raw, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p == "" || seen[p] || flagIndex(p) < 0 {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return sortFlags(out)
}

// sortFlags 按 flagOrder 归一顺序，保证同一组筛选永远生成同一个 URL。
// 否则 flags=hd,cn 和 flags=cn,hd 会被当成两个不同的地址。
func sortFlags(flags []string) []string {
	out := append([]string{}, flags...)
	sort.Slice(out, func(i, j int) bool { return flagIndex(out[i]) < flagIndex(out[j]) })
	return out
}

// flagIndex 返回属性在白名单中的位置，-1 表示不在白名单里。
func flagIndex(flag string) int {
	for i, allowed := range flagOrder {
		if allowed == flag {
			return i
		}
	}
	return -1
}
