package main

// 批量下载与"探测可用下载器"的回归测试。
//
// 这两块都是为"上游把 downloader 改成必填"这件事补的能力：
// 前者让一页最多 1000 条能一次提交，后者负责回答"到底该填哪个标识"。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// batchUpstream 是一个按 tid 决定成败的假上游，用来验证
// "单条失败不影响其余条目"这条批量场景下最重要的性质。
func batchUpstream(failFor map[string]string) (http.HandlerFunc, *int) {
	var mu sync.Mutex
	var hits int
	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		tid := r.URL.Query().Get("tid")
		if msg, bad := failFor[tid]; bad {
			_, _ = io.WriteString(w, `{"code":1,"message":"`+msg+`"}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"message":"已提交到下载器"}`)
	}
	return handler, &hits
}

func decodeBatch(t *testing.T, body []byte) batchDownloadResponse {
	t.Helper()
	var resp batchDownloadResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（原文 %s）", err, truncateForDisplay(string(body), 200))
	}
	return resp
}

// TestDownloadBatchSubmitsEveryTID 验证一次请求能把整批都提交出去。
func TestDownloadBatchSubmitsEveryTID(t *testing.T) {
	handler, hits := batchUpstream(nil)
	app := newTestApp(t, handler)

	rec := doForm(app, "/download/batch", url.Values{"tids": {"1,2,3,4,5"}}, "http://example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200；body=%s", rec.Code, rec.Body.String())
	}

	resp := decodeBatch(t, rec.Body.Bytes())
	if resp.Total != 5 || resp.OKCount != 5 || resp.FailCount != 0 {
		t.Errorf("统计不对: %+v", resp)
	}
	if !resp.Success {
		t.Error("全部成功时 success 应为 true")
	}
	if len(resp.Results) != 5 {
		t.Fatalf("逐条结果数 = %d，期望 5", len(resp.Results))
	}
	// 结果必须与入参**同序**：前端按顺序标状态，错位会把 A 的失败显示到 B 上。
	for i, item := range resp.Results {
		want := fmt.Sprintf("%d", i+1)
		if item.TID != want {
			t.Errorf("第 %d 条 tid = %q，期望 %q（结果顺序被打乱）", i, item.TID, want)
		}
		if !item.Success {
			t.Errorf("第 %d 条应成功，实际 %q", i, item.Message)
		}
	}
	if *hits != 5 {
		t.Errorf("上游被调用 %d 次，期望 5 次（去重或合并会丢条目）", *hits)
	}
}

// TestDownloadBatchIsolatesFailures 是批量场景的核心性质：
// 一条坏数据不能让整批停摆，也不能把失败悄悄记成成功。
func TestDownloadBatchIsolatesFailures(t *testing.T) {
	handler, hits := batchUpstream(map[string]string{
		"2": "未找到下载器: Downloader.bogus",
	})
	app := newTestApp(t, handler)

	rec := doForm(app, "/download/batch", url.Values{"tids": {"1,2,3"}}, "http://example.com")
	resp := decodeBatch(t, rec.Body.Bytes())

	if resp.OKCount != 2 || resp.FailCount != 1 {
		t.Errorf("统计不对: ok=%d fail=%d，期望 2/1", resp.OKCount, resp.FailCount)
	}
	if *hits != 3 {
		t.Errorf("失败条目不应导致其余条目被跳过，上游调用 %d 次，期望 3 次", *hits)
	}
	if resp.Results[1].Success {
		t.Error("第 2 条应标记为失败")
	}
	// 失败原因要经过人话化，而不是把上游原文直接丢给用户。
	if !strings.Contains(resp.Results[1].Message, "设置") {
		t.Errorf("失败原因应给出可操作指引，实际 %q", resp.Results[1].Message)
	}
	if !resp.Success {
		t.Error("部分成功时 success 应为 true（前端据此判断有没有必要整批重试）")
	}
	// 全失败时 success 必须为 false。
	handler2, _ := batchUpstream(map[string]string{"1": "boom", "2": "boom"})
	app2 := newTestApp(t, handler2)
	resp2 := decodeBatch(t, doForm(app2, "/download/batch", url.Values{"tids": {"1,2"}}, "http://example.com").Body.Bytes())
	if resp2.Success {
		t.Error("全部失败时 success 应为 false")
	}
}

func TestDownloadBatchInputValidation(t *testing.T) {
	handler, hits := batchUpstream(nil)
	app := newTestApp(t, handler)

	t.Run("空列表", func(t *testing.T) {
		rec := doForm(app, "/download/batch", url.Values{"tids": {""}}, "http://example.com")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("状态码 = %d, 期望 400", rec.Code)
		}
	})

	t.Run("非法 tid 被丢弃而不是整批报错", func(t *testing.T) {
		rec := doForm(app, "/download/batch", url.Values{
			"tids": {"1, ../etc/passwd ,2,,3"},
		}, "http://example.com")
		resp := decodeBatch(t, rec.Body.Bytes())
		if resp.Total != 3 {
			t.Errorf("应只保留 3 个合法 tid，实际 %d：%+v", resp.Total, resp.Results)
		}
	})

	t.Run("去重", func(t *testing.T) {
		rec := doForm(app, "/download/batch", url.Values{"tids": {"7,7,7,8"}}, "http://example.com")
		resp := decodeBatch(t, rec.Body.Bytes())
		if resp.Total != 2 {
			t.Errorf("重复 tid 应被去掉，实际 %d", resp.Total)
		}
	})

	t.Run("超过单批上限", func(t *testing.T) {
		ids := make([]string, 0, batchDownloadLimit+1)
		for i := 0; i <= batchDownloadLimit; i++ {
			ids = append(ids, fmt.Sprintf("%d", i+1))
		}
		before := *hits
		rec := doForm(app, "/download/batch", url.Values{"tids": {strings.Join(ids, ",")}}, "http://example.com")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("状态码 = %d, 期望 400（超上限应在发请求前拒绝）", rec.Code)
		}
		if *hits != before {
			t.Error("超上限的请求不应触达上游")
		}
	})
}

// TestDownloadBatchRejectsCrossSite 沿用单条下载的两道收口：
// 只接受 POST + 同源校验。批量接口一次能改上百条状态，收口只会更严。
func TestDownloadBatchRejectsCrossSite(t *testing.T) {
	handler, hits := batchUpstream(nil)
	app := newTestApp(t, handler)

	rec := doForm(app, "/download/batch", url.Values{"tids": {"1,2"}}, "http://evil.example.com")
	if rec.Code != http.StatusForbidden {
		t.Errorf("状态码 = %d, 期望 403", rec.Code)
	}
	if *hits != 0 {
		t.Error("跨站请求不应触达上游")
	}

	// GET 一律 405：批量和单条一样不能让 <img src> 触发。
	if rec := do(app, http.MethodGet, "/download/batch?tids=1,2"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET 状态码 = %d, 期望 405", rec.Code)
	}
	if *hits != 0 {
		t.Error("GET 请求不应触达上游")
	}
}

func TestDownloadBatchRequiresConfiguredSavePath(t *testing.T) {
	handler, hits := batchUpstream(nil)
	app := newTestApp(t, handler, func(c *Config) { c.SavePath = "" })

	rec := doForm(app, "/download/batch", url.Values{"tids": {"1,2"}}, "http://example.com")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400", rec.Code)
	}
	resp := decodeBatch(t, rec.Body.Bytes())
	if !strings.Contains(resp.Message, "保存路径") {
		t.Errorf("应指出要填保存路径，实际 %q", resp.Message)
	}
	if *hits != 0 {
		t.Error("保存路径没配时不应触达上游：注定全失败，只是白等")
	}
}

// ------------------------------------------------------ 探测可用下载器

// probeUpstream 复刻上游对 /config/downloader/directories 的三种回答。
func probeUpstream(status map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := r.URL.Query().Get("downloader_id")
		switch status[id] {
		case "unsupported":
			_, _ = io.WriteString(w, `{"code":1,"message":"不支持的下载工具","data":null}`)
		case "known":
			_, _ = io.WriteString(w, `{"code":1,"message":"未找到该下载工具配置","data":null}`)
		case "configured_with_dirs":
			_, _ = io.WriteString(w, `{"code":0,"message":"操作成功","data":{"directories":[{"path":"/media/movies"},{"name":"/media/tv"}]}}`)
		default:
			// 配置过、但读目录时下游出错（CloudDrive 的典型形态）。
			_, _ = io.WriteString(w, `{"code":1,"message":"CloudDrive目录读取失败：NOT_FOUND","data":null}`)
		}
	}
}

func TestProbeDownloadersPicksConfiguredOne(t *testing.T) {
	app := newTestApp(t, probeUpstream(map[string]string{
		"clouddrive":   "configured_with_dirs",
		"115":          "known",
		"qbittorrent":  "unsupported",
		"transmission": "known",
		"thunder":      "unsupported",
	}))

	rec := doForm(app, "/api/settings/downloaders", url.Values{}, "http://example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", rec.Code)
	}

	var resp settingsProbeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !resp.Success || resp.Best != "clouddrive" {
		t.Fatalf("应挑出已配置的 clouddrive，实际 success=%v best=%q", resp.Success, resp.Best)
	}
	if len(resp.Downloaders) != len(supportedDownloaderIDs) {
		t.Fatalf("应逐个报告 %d 个候选，实际 %d", len(supportedDownloaderIDs), len(resp.Downloaders))
	}
	statusOf := func(id string) string {
		for _, p := range resp.Downloaders {
			if p.ID == id {
				return p.Status
			}
		}
		return "(缺失)"
	}
	if got := statusOf("115"); got != probeStatusKnown {
		t.Errorf("115 应有『类型存在但未配置』的结论，实际 %q", got)
	}
	if got := statusOf("qbittorrent"); got != probeStatusUnsupported {
		t.Errorf("qbittorrent 应为『类型不存在』，实际 %q", got)
	}
	if len(resp.Directories) == 0 {
		t.Error("探测到已配置的下载器时，应顺手带回目录候选")
	}

	// 目录候选要落盘，这样设置页下次打开就有下拉提示。
	if got := app.currentConfig().SavePathOptions; len(got) == 0 {
		t.Error("目录候选应被缓存进配置")
	}
}

// TestProbeDownloadersReportsNothingConfigured 覆盖"上游一个下载器都没配"。
//
// 这是必须能把话说清楚的情形：用户点完按钮如果只看到"探测完成"，
// 他会以为已经可以下载了，然后继续撞上游的报错。
func TestProbeDownloadersReportsNothingConfigured(t *testing.T) {
	app := newTestApp(t, probeUpstream(map[string]string{
		"clouddrive": "known", "115": "known",
		"qbittorrent": "unsupported", "transmission": "known", "thunder": "unsupported",
	}))

	var resp settingsProbeResponse
	rec := doForm(app, "/api/settings/downloaders", url.Values{}, "http://example.com")
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}

	if resp.Success {
		t.Error("没有任何已配置的下载器时 success 必须为 false")
	}
	if resp.Best != "" {
		t.Errorf("没有可用标识时不应给出 best，实际 %q", resp.Best)
	}
	if !strings.Contains(resp.Message, "下载工具") {
		t.Errorf("应引导用户去上游配置下载工具，实际 %q", resp.Message)
	}
}

// TestProbeDownloaderTreatsDownstreamFailureAsConfigured 是一条容易写反的判据。
//
// CloudDrive 的目录接口会因"网盘上没那个目录"而失败，但那恰恰证明它配过了。
// 若把 code!=0 一律当成"没配"，用户会被告知"没有可用的下载器"，
// 于是去上游重新配一遍——而他其实已经配好了。
func TestProbeDownloaderTreatsDownstreamFailureAsConfigured(t *testing.T) {
	server := httptest.NewServer(probeUpstream(map[string]string{"clouddrive": "downstream_error"}))
	defer server.Close()

	client := NewAvdbClient(Config{APIBaseURL: server.URL, Timeout: 0})
	probe := client.ProbeDownloader(t.Context(), "clouddrive")

	if !probe.Configured() {
		t.Errorf("下游读目录失败仍应判定为『已配置』，实际 %+v", probe)
	}
	if probe.Message == "" {
		t.Error("应保留下游的原始报错，便于用户判断是路径问题还是配置问题")
	}
}

// TestProbeDownloaderReportsTransportErrorAsUnknown 确认连不上时不下结论。
//
// 传输层错误与"下载器是否存在"无关，把它判成"未配置"会误导用户去改上游配置。
func TestProbeDownloaderReportsTransportErrorAsUnknown(t *testing.T) {
	client := NewAvdbClient(Config{APIBaseURL: "http://127.0.0.1:1", Timeout: 0})
	probe := client.ProbeDownloader(t.Context(), "clouddrive")

	if probe.Status != probeStatusError {
		t.Errorf("连不上上游时应返回 %q，实际 %+v", probeStatusError, probe)
	}
	if probe.Configured() {
		t.Error("结论未知时不能声称已配置")
	}
}
