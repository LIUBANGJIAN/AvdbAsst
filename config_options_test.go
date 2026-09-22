package main

import "testing"

// TestNormalizeConfigFillsEmptyDownloader 守住"下载器永不为空"这个不变量。
//
// 这条不变量是踩出来的：上游的 download/manul 把 downloader 当作必填标识来查，
// 传空串会回一句 `未找到下载器: ...`。所以空值必须在归一化阶段就被补掉，
// 绝不能一路透传到上游。
func TestNormalizeConfigFillsEmptyDownloader(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"空串补默认值", "", defaultDownloader},
		{"全空白也补默认值", "   ", defaultDownloader},
		{"已配置的值原样保留", "qb", "qb"},
		{"已配置的值去首尾空白", " 115 ", "115"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeConfig(Config{Downloader: tc.input})
			if got.Downloader != tc.want {
				t.Errorf("Downloader(%q) = %q，期望 %q", tc.input, got.Downloader, tc.want)
			}
		})
	}
}

// TestNormalizeDownloaderOptionsDedupesAndKeepsOrder 确认候选清单清洗后保序。
//
// 保序不是洁癖：上游清单的顺序通常把默认项排在前面，用 map 去重再输出
// 会打乱它，用户每次打开设置页看到的顺序都不一样。
func TestNormalizeDownloaderOptionsDedupesAndKeepsOrder(t *testing.T) {
	in := []DownloaderOption{
		{ID: "115", Label: "115网盘"},
		{ID: " qb ", Label: " qBittorrent "},
		{ID: "115", Label: "重复项"},
		{ID: "   ", Label: "空标识应被丢弃"},
		{ID: "aria2"},
	}

	got := normalizeDownloaderOptions(in)

	if len(got) != 3 {
		t.Fatalf("应剩 3 项，实际 %d: %+v", len(got), got)
	}
	wantIDs := []string{"115", "qb", "aria2"}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("第 %d 项 ID = %q，期望 %q（顺序必须保持）", i, got[i].ID, want)
		}
	}
	if got[1].Label != "qBittorrent" {
		t.Errorf("标签未去空白: %q", got[1].Label)
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
