package main

import "testing"

// TestNormalizeConfigKeepsDownloaderEmpty 守住"不替用户猜下载器"这条规则。
//
// 项目历史上走了两次弯路，都以"给下载器补个值"告终：
//  1. 传空串 → 上游把它当成名叫"空"的下载器，回 `未找到下载器`；
//  2. 硬编码 "115" → 用户上游没有这个标识，回 `未找到下载器: Downloader.115`。
//
// 两次都让用户拿到一句看不懂的报错。正确的做法是：留空就保持留空，
// 由 SubmitDownload 省略该 query 参数，让上游用自己的默认下载器。
// 所以这里断言的是"原样保留"，一旦有人再加回补值逻辑，这个测试会红。
func TestNormalizeConfigKeepsDownloaderEmpty(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"空串保持空串", "", ""},
		{"全空白归一成空串", "   ", ""},
		{"已配置的值原样保留", "qb", "qb"},
		{"已配置的值去首尾空白", " darkcount ", "darkcount"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeConfig(Config{Downloader: tc.input})
			if got.Downloader != tc.want {
				t.Errorf("Downloader(%q) = %q，期望 %q（不得注入任何猜测的默认值）",
					tc.input, got.Downloader, tc.want)
			}
		})
	}
}

func TestNormalizeStringListDedupesAndKeepsOrder(t *testing.T) {
	got := normalizeStringList([]string{"/media/a", " /media/b ", "/media/a", "", "   "}, 10)
	if len(got) != 2 || got[0] != "/media/a" || got[1] != "/media/b" {
		t.Errorf("结果 = %v", got)
	}
}

func TestNormalizeStringListRespectsLimit(t *testing.T) {
	got := normalizeStringList([]string{"/a", "/b", "/c", "/d"}, 2)
	if len(got) != 2 {
		t.Errorf("应被截到 2 项，实际 %v", got)
	}
}
