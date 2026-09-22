package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// upstreamSample 是从真实上游抓下来的响应片段，包含两个已知的边界情况：
//   - id = 10080460000，超出 int32；
//   - category = null。
//
// 用真实数据做回归，避免以后有人把 int64 改回 int。
const upstreamSample = `{
  "code": 0,
  "message": "操作成功",
  "data": [
    {
      "id": 3691410,
      "number": "",
      "site": "🌸色花堂🍑欧美无码",
      "website": "sehuatang",
      "section": "欧美无码",
      "category": "",
      "size_mb": 6297.0,
      "size_bytes": 6602883072,
      "post_time": "2026-08-14 20:13:14",
      "seeders": 66,
      "title": "pornplus.26.07.10.aubry.babcock.marketplace.meetup.4k",
      "download_url": "magnet:?xt=urn:btih:152C64EA8C4F3D105B5E191CB988F5E905AD71D3",
      "preview_image": "https://tu.ewrewej.la/tupian/forum/202608/14/084858ckj03dcscdrarajk.jpg",
      "free": true, "chinese": false, "uncensored": true, "uc": false,
      "hd": true, "uhd": true, "mosaic": false
    },
    {
      "id": 10080460000,
      "number": "BOBB-456,BOBB-00456",
      "site": "🌸x1080x🍑亚洲有码",
      "website": "x1080x",
      "section": "亚洲有码",
      "category": null,
      "size_mb": 5244.0,
      "size_bytes": 5498732544,
      "post_time": "2026-02-22 20:13:14",
      "seeders": 66,
      "title": "BOBB-456 [BT](ABC)(bobb00456) 超ド級Jカップ！",
      "download_url": "magnet:?xt=urn:btih:c56bcdbc8066c250493a9fe2ca98bdae55f285c0&dn=BOBB-456",
      "preview_image": "https://www.hxmmdd.com/pics/off/2026/202602/20260222/off_BOBB-456.jpg",
      "free": true, "chinese": false, "uncensored": false, "uc": false,
      "hd": true, "uhd": false, "mosaic": true
    }
  ]
}`

func TestDecodeUpstreamSample(t *testing.T) {
	var env apiEnvelope
	if err := json.Unmarshal([]byte(upstreamSample), &env); err != nil {
		t.Fatalf("解析上游外壳失败: %v", err)
	}
	if env.Code != 0 {
		t.Fatalf("code = %d, 期望 0", env.Code)
	}

	var list []Torrent
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatalf("解析 data 失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("解析出 %d 条，期望 2 条", len(list))
	}

	// 大整数 ID 必须原样保留。
	if list[1].ID != 10080460000 {
		t.Errorf("大 ID 解析错误: %d", list[1].ID)
	}
	// category 为 null 时必须安全落成空串，而不是解析失败或变成 "<nil>"。
	if list[1].Category != "" {
		t.Errorf("null category 应解析为空串，实际 %q", list[1].Category)
	}
	if list[0].Category != "" {
		t.Errorf("空字符串 category 应保持为空，实际 %q", list[0].Category)
	}
}

func TestNormalizeTorrent(t *testing.T) {
	var env apiEnvelope
	if err := json.Unmarshal([]byte(upstreamSample), &env); err != nil {
		t.Fatalf("解析上游外壳失败: %v", err)
	}
	var list []Torrent
	if err := json.Unmarshal(env.Data, &list); err != nil {
		t.Fatalf("解析 data 失败: %v", err)
	}
	for i := range list {
		normalizeTorrent(&list[i])
	}

	first := list[0]
	if first.Site != "色花堂" {
		t.Errorf("站点名归一 = %q, 期望 色花堂", first.Site)
	}
	if first.Section != "欧美无码" {
		t.Errorf("板块 = %q, 期望 欧美无码", first.Section)
	}
	if first.SortTS <= 0 {
		t.Errorf("SortTS 应被解析出来，实际 %d", first.SortTS)
	}
	if !strings.HasPrefix(first.DownloadURL, "magnet:") {
		t.Errorf("磁力链接被误删: %q", first.DownloadURL)
	}
	if !strings.HasPrefix(first.PreviewImage, "https://") {
		t.Errorf("预览图被误删: %q", first.PreviewImage)
	}
	// 标题里的 / \ 等字符必须原样保留，不能被过度清洗。
	if !strings.Contains(first.Title, "pornplus.26.07.10") {
		t.Errorf("标题被破坏: %q", first.Title)
	}
}

func TestNormalizeTorrentSizeFallback(t *testing.T) {
	tr := Torrent{SizeMB: 0, SizeBytes: 2 * 1024 * 1024}
	normalizeTorrent(&tr)
	if tr.SizeMB < 1.99 || tr.SizeMB > 2.01 {
		t.Errorf("应据 size_bytes 补算出 2MB，实际 %v", tr.SizeMB)
	}

	neg := Torrent{SizeMB: -5, SizeBytes: 0, Seeders: -3}
	normalizeTorrent(&neg)
	if neg.SizeMB != 0 {
		t.Errorf("负体积应归零，实际 %v", neg.SizeMB)
	}
	if neg.Seeders != 0 {
		t.Errorf("负做种数应归零，实际 %d", neg.Seeders)
	}
}

func TestSafeURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		allowed []string
		want    string
	}{
		{"磁力链放行", "magnet:?xt=urn:btih:ABC", []string{"magnet", "http", "https"}, "magnet:?xt=urn:btih:ABC"},
		{"https 放行", "https://a.b/c.jpg", []string{"http", "https"}, "https://a.b/c.jpg"},
		{"javascript 拒绝", "javascript:alert(1)", []string{"http", "https"}, ""},
		{"大小写混写的 javascript 拒绝", "JaVaScRiPt:alert(1)", []string{"http", "https"}, ""},
		{"中间插入控制字符的 javascript 拒绝", "java\nscript:alert(1)", []string{"http", "https"}, ""},
		{"data 协议拒绝", "data:text/html;base64,PHNjcmlwdD4=", []string{"http", "https"}, ""},
		{"相对路径拒绝", "/pics/a.jpg", []string{"http", "https"}, ""},
		{"空串", "", []string{"http", "https"}, ""},
		{"纯空白", "   ", []string{"http", "https"}, ""},
		{"前后空白被裁掉", "  https://a.b  ", []string{"http", "https"}, "https://a.b"},
		{"协议白名单未包含则拒绝", "ftp://a.b", []string{"http", "https"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeURL(tc.raw, tc.allowed...); got != tc.want {
				t.Errorf("safeURL(%q) = %q, 期望 %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestSanitizeText(t *testing.T) {
	in := "a\nb\tc\r\nd   e\x00f\x07"
	got := sanitizeText(in)
	if got != "a b c d e f" {
		t.Errorf("sanitizeText = %q, 期望 %q", got, "a b c d e f")
	}
	// 零宽字符要清掉，否则会破坏前端的整词匹配。
	if got := sanitizeText("磁力\u200b链接\ufeff"); got != "磁力链接" {
		t.Errorf("零宽字符未被清除: %q", got)
	}
}

func TestCleanSiteName(t *testing.T) {
	cases := map[string]string{
		"🌸色花堂🍑欧美无码": "色花堂欧美无码",
		"纯中文站点":     "纯中文站点",
		"A+B":       "A+B", // 旧实现用 IsSymbol 会误删 "+"，这里必须保留
		"  x  ":     "x",
		"":          "",
	}
	for in, want := range cases {
		if got := cleanSiteName(in); got != want {
			t.Errorf("cleanSiteName(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestExtractSiteName(t *testing.T) {
	cases := []struct{ site, section, want string }{
		{"🌸色花堂🍑欧美无码", "欧美无码", "色花堂"},
		{"🌸x1080x🍑亚洲有码", "亚洲有码", "x1080x"},
		{"站点带\n换行", "板块", "站点带"},
		{"没有表情的站点", "", "没有表情的站点"},
		{"", "板块", ""},
	}
	for _, tc := range cases {
		if got := extractSiteName(tc.site, tc.section); got != tc.want {
			t.Errorf("extractSiteName(%q, %q) = %q, 期望 %q", tc.site, tc.section, got, tc.want)
		}
	}
}

func TestParseTimeToUnix(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1970-01-01 00:00:00", 0}, // 解析成功但落在 epoch，按"失败"处理不影响排序语义
		{"2026-08-14 20:13:14", time.Date(2026, 8, 14, 20, 13, 14, 0, time.UTC).Unix()},
		{"2026-08-14", time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC).Unix()},
		{"2026/08/14", time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC).Unix()},
		{"", 0},
		{"不是时间", 0},
	}
	for _, tc := range cases {
		if got := parseTimeToUnix(tc.in); got != tc.want {
			t.Errorf("parseTimeToUnix(%q) = %d, 期望 %d", tc.in, got, tc.want)
		}
	}
}

func TestFormatSize(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.00 MB"},
		{512, "512.00 MB"},
		{1024, "1.00 GB"},
		{6297, "6.15 GB"},
		{1024 * 1024, "1.00 TB"},
		{-1, "0.00 MB"},
	}
	for _, tc := range cases {
		if got := formatSize(tc.in); got != tc.want {
			t.Errorf("formatSize(%v) = %q, 期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatDate(t *testing.T) {
	cases := map[string]string{
		"2026-08-14 20:13:14": "2026-08-14",
		"2026-08-14T20:13:14": "2026-08-14",
		"2026-08-14":          "2026-08-14",
		"2026/08/14 10:00:00": "2026-08-14",
		"2026.08.14":          "2026-08-14",
		"2026-8-4 09:00:00":   "2026-08-04", // 补零
		"":                    "",
		"   ":                 "",
		"2026-13-45":          "2026-13-45", // 非法月日：不猜，按 rune 截断兜底
		"垃圾输入但够长啊啊啊啊啊":        "垃圾输入但够长啊啊啊",
	}
	for in, want := range cases {
		if got := formatDate(in); got != want {
			t.Errorf("formatDate(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

// TestFormatDateKeepsUpstreamDay 锁死"日期用上游的"这条要求。
//
// 曾经的实现是把上游时间解析成时间戳、再用本地时区格式化，
// 在 UTC+8 下会把当天的 20:13 显示成次日，日期整体偏移一天。
// 这里用一整天里最靠近时区边界的几个时刻做回归。
func TestFormatDateKeepsUpstreamDay(t *testing.T) {
	// 这些时刻在 UTC+8 或 UTC-8 下都会被推过午夜。
	for _, upstream := range []string{
		"2026-01-01 00:00:01",
		"2026-06-30 23:59:59",
		"2026-12-31 16:00:00",
		"2026-03-15 08:30:00",
	} {
		day := upstream[:10]
		if got := formatDate(upstream); got != day {
			t.Errorf("formatDate(%q) = %q, 必须原样保留上游日期 %q", upstream, got, day)
		}
	}
}

func TestNormalizeKeyword(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"中文", "中文关键词", "中文关键词"},
		{"纯英文", "BOBB-456", "BOBB-456"},
		{"纯数字", "123456", "123456"},
		{"中英数混排", "少林足球 Shaolin Soccer 2001", "少林足球 Shaolin Soccer 2001"},
		{"日文假名", "あいうえお カタカナ", "あいうえお カタカナ"},
		{"全角英文数字转半角", "ＢＯＢＢ－４５６", "BOBB-456"},
		{"全角大写字母", "ＡＢＣ１２３", "ABC123"},
		{"全角空格折叠", "全角　空格", "全角 空格"},
		{"连续空格折叠", "a   b", "a b"},
		{"制表换行折叠", "a\tb\nc", "a b c"},
		{"首尾空白裁掉", "  abc  ", "abc"},
		{"空串", "", ""},
		{"纯空白", "   \t\n", ""},
		{"控制字符替换为空格", "a\x00b", "a b"},
		{"保留中文书名号等标点", "《少林足球》", "《少林足球》"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeKeyword(tc.in); got != tc.want {
				t.Errorf("normalizeKeyword(%q) = %q, 期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeKeywordLengthCap(t *testing.T) {
	long := strings.Repeat("中", 500)
	got := normalizeKeyword(long)
	if n := len([]rune(got)); n != maxKeywordRunes {
		t.Errorf("超长关键词应截断到 %d 个字符，实际 %d", maxKeywordRunes, n)
	}
}

func TestNormalizeKeywordHandlesInvalidUTF8(t *testing.T) {
	// 伪造非法 UTF-8 字节（等价于攻击者直接发原始二进制进 query）。
	got := normalizeKeyword("ab\xff\xfecd")
	if got == "" {
		t.Fatal("非法字节不应导致整体被丢弃")
	}
	if strings.ContainsRune(got, 0xff) {
		t.Errorf("非法字节未被替换: %q", got)
	}
	if !strings.HasPrefix(got, "ab") || !strings.HasSuffix(got, "cd") {
		t.Errorf("合法部分应被保留: %q", got)
	}
}

// TestCollectFacetsIsDeterministic 是本项目最重要的一条回归测试。
//
// 旧实现直接遍历 map 生成筛选项，导致同一份数据每次渲染顺序都不同，
// 属于"同一输入两次结果不一致"的安静型故障。这里连跑 200 次做校验。
func TestCollectFacetsIsDeterministic(t *testing.T) {
	items := []Torrent{
		{Site: "zeta", Section: "s2", Category: "c3"},
		{Site: "alpha", Section: "s1", Category: "c1"},
		{Site: "mike", Section: "s3", Category: "c2"},
		{Site: "alpha", Section: "s1", Category: "c1"},
		{Site: "beta", Section: "s2", Category: "c3"},
	}

	firstSites, firstSections, firstCategories := collectFacets(items)
	wantSites := "alpha,beta,mike,zeta"
	if got := strings.Join(firstSites, ","); got != wantSites {
		t.Fatalf("站点筛选未按字典序去重: %q, 期望 %q", got, wantSites)
	}
	if got := strings.Join(firstSections, ","); got != "s1,s2,s3" {
		t.Fatalf("板块筛选错误: %q", got)
	}
	if got := strings.Join(firstCategories, ","); got != "c1,c2,c3" {
		t.Fatalf("分类筛选错误: %q", got)
	}

	for i := 0; i < 200; i++ {
		sites, sections, categories := collectFacets(items)
		if strings.Join(sites, ",") != strings.Join(firstSites, ",") ||
			strings.Join(sections, ",") != strings.Join(firstSections, ",") ||
			strings.Join(categories, ",") != strings.Join(firstCategories, ",") {
			t.Fatalf("第 %d 次调用结果与首次不一致，说明存在 map 遍历顺序泄漏", i)
		}
	}
}

func TestCollectFacetsSkipsEmpty(t *testing.T) {
	items := []Torrent{
		{Site: "", Section: "", Category: ""},
		{Site: "a", Section: "", Category: ""},
	}
	sites, sections, categories := collectFacets(items)
	if len(sites) != 1 || sites[0] != "a" {
		t.Errorf("站点筛选应只含 a，实际 %v", sites)
	}
	if len(sections) != 0 || len(categories) != 0 {
		t.Errorf("空值不应进入筛选列表，实际 sections=%v categories=%v", sections, categories)
	}
}

func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 502, 503, 504}
	for _, s := range retryable {
		if !isRetryableStatus(s) {
			t.Errorf("状态码 %d 应被视为可重试", s)
		}
	}
	notRetryable := []int{200, 400, 401, 403, 404, 422}
	for _, s := range notRetryable {
		if isRetryableStatus(s) {
			t.Errorf("状态码 %d 不应被重试", s)
		}
	}
}

func TestDescribeStatus(t *testing.T) {
	// 覆盖 Avdb 文档中列出的全部常见状态码。
	cases := map[int]string{
		401: "鉴权失败",
		403: "拒绝该操作",
		404: "接口不存在",
		422: "参数校验失败",
		429: "限流",
		503: "暂时不可用",
	}
	for status, want := range cases {
		if got := describeStatus(status, []byte(`{}`)); !strings.Contains(got, want) {
			t.Errorf("状态码 %d 的提示应包含 %q，实际 %q", status, want, got)
		}
	}
	if got := describeStatus(401, []byte(`{"message":"unauthorized"}`)); !strings.Contains(got, "鉴权失败") {
		t.Errorf("401 应提示鉴权失败，实际 %q", got)
	}
	// 超长响应体必须被截断，避免把上游整页 HTML 灌进用户界面。
	long := describeStatus(500, []byte(strings.Repeat("x", 5000)))
	if len(long) > 300 {
		t.Errorf("错误信息未截断，长度 %d", len(long))
	}
	// 换行必须被压平，否则日志会被撑爆。
	if got := describeStatus(500, []byte("a\nb\nc")); strings.Contains(got, "\n") {
		t.Errorf("错误信息中的换行未被压平: %q", got)
	}
}

// TestFastAPIValidationMessage 覆盖线上 422 的报错可读性。
//
// 上游（FastAPI）参数校验失败时返回的是结构化报告，直接回显对用户毫无意义：
//
//	{"detail":[{"type":"missing","loc":["query","save_path"],"msg":"Field required"}]}
//
// 必须翻译成"查询参数 → save_path：Field required"这类能直接照做的提示。
func TestFastAPIValidationMessage(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantHas  []string
		wantNone bool // true 表示应当解析失败并返回空串
	}{
		{
			name:    "缺失 query 参数（线上原样）",
			body:    `{"detail":[{"type":"missing","loc":["query","save_path"],"msg":"Field required","input":null}]}`,
			wantHas: []string{"查询参数", "save_path", "Field required"},
		},
		{
			name:    "缺失 body 字段（带数组下标）",
			body:    `{"detail":[{"type":"missing","loc":["body",0,"magnet"],"msg":"Field required"}]}`,
			wantHas: []string{"请求体", "[0]", "magnet"},
		},
		{
			name:    "多个错误只列前三条并省略",
			body:    `{"detail":[{"loc":["query","a"],"msg":"x"},{"loc":["query","b"],"msg":"y"},{"loc":["query","c"],"msg":"z"},{"loc":["query","d"],"msg":"w"}]}`,
			wantHas: []string{"a", "b", "c", "…"},
		},
		{
			name:     "非 422 结构不做翻译",
			body:     `{"code":4001,"message":"关键词过短"}`,
			wantNone: true,
		},
		{
			name:     "HTML 错误页不做翻译",
			body:     `<html><body>502 Bad Gateway</body></html>`,
			wantNone: true,
		},
		{
			name:     "空的 detail 数组不做翻译",
			body:     `{"detail":[]}`,
			wantNone: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fastAPIValidationMessage([]byte(tc.body))
			if tc.wantNone {
				if got != "" {
					t.Errorf("应返回空串，实际 %q", got)
				}
				return
			}
			if got == "" {
				t.Fatal("不应返回空串")
			}
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("结果应包含 %q，实际 %q", want, got)
				}
			}
		})
	}
}

// TestDescribeStatusPrefersHumanReadable422 确认 422 走的是翻译后的文案，
// 而不是把原始 JSON 原样甩出来。
func TestDescribeStatusPrefersHumanReadable422(t *testing.T) {
	raw := `{"detail":[{"type":"missing","loc":["query","save_path"],"msg":"Field required","input":null}]}`
	got := describeStatus(http.StatusUnprocessableEntity, []byte(raw))

	if !strings.Contains(got, "参数校验失败") {
		t.Errorf("应保留状态码语义，实际 %q", got)
	}
	if !strings.Contains(got, "save_path") {
		t.Errorf("应指明缺失的参数名，实际 %q", got)
	}
	if strings.Contains(got, `"detail"`) || strings.Contains(got, `"type"`) {
		t.Errorf("不应回显原始 JSON 结构，实际 %q", got)
	}
	// 解析不出结构时必须退回原文，不能把信息丢掉。
	fallback := describeStatus(http.StatusUnprocessableEntity, []byte(`{"oops":1}`))
	if !strings.Contains(fallback, "oops") {
		t.Errorf("无法翻译时应保留原文线索，实际 %q", fallback)
	}
}

// TestSubmitDownloadAlwaysSendsAllQueryParams 是 422 的单元级回归：
// 无论是否配置下载器与保存路径，两个参数都必须出现（可为空）。
func TestSubmitDownloadAlwaysSendsAllQueryParams(t *testing.T) {
	var gotURI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
	}))
	defer upstream.Close()

	client := NewAvdbClient(Config{APIBaseURL: upstream.URL, APIKey: "k", Timeout: 5 * time.Second})

	cases := []struct {
		name                 string
		downloader, savePath string
	}{
		{"两者都空（最常见）", "", ""},
		{"只配下载器", "115", ""},
		{"只配保存路径", "", "/media/movies"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := client.SubmitDownload(context.Background(), "3691410", tc.downloader, tc.savePath); err != nil {
				t.Fatalf("提交下载失败: %v", err)
			}
			u, err := url.Parse(gotURI)
			if err != nil {
				t.Fatalf("上游 URI 无法解析: %v", err)
			}
			q := u.Query()
			if q.Get("tid") != "3691410" {
				t.Errorf("tid 不正确: %q", q.Get("tid"))
			}
			for _, key := range []string{"downloader", "save_path"} {
				if _, ok := q[key]; !ok {
					t.Fatalf("缺少参数 %s（上游会返回 422）: %s", key, gotURI)
				}
			}
			if q.Get("downloader") != tc.downloader || q.Get("save_path") != tc.savePath {
				t.Errorf("参数值未透传: %s", gotURI)
			}
		})
	}
}
