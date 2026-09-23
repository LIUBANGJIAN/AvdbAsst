package main

// 上游「配置类」只读接口的访问层。
//
// 与 avdb.go 分开的原因：avdb.go 已接近 700 行，继续堆会变成难以维护的巨石文件。
//
// 本站**只用访问令牌（X-API-Key）**，不碰任何只认登录 JWT 的接口
// （例如 /api/v1/config/{key}）——那类接口要求用户提供上游账号密码，
// 而实际需要的"默认下载目标"在 default-rule 里就有，且那个接口接受访问令牌。
//
// 解析一律**防御式**：上游没有承诺过响应结构，解析不出来就返回空值并附上原文，
// 交给页面显示真实内容，而不是假装成功。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const (
	// defaultRulePath 读取「订阅默认资源规则」，即上游的全局默认下载目标。
	//
	// 官方文档标注鉴权为 `API Key/JWT`，同时"读取订阅默认资源规则"。
	// 它的字段与下载接口需要的东西直接对应：
	//
	//	downloader / save_path / save_path_label
	//
	// 这是**唯一**一条能用访问令牌拿到"上游到底配了哪个下载器"的路。
	// 上游那 4 条"下载器开关"配置（downloader / downloaders / download /
	// default_downloader）走的是 /api/v1/config/{key}，只认 JWT，用不了。
	defaultRulePath = "/api/v1/javdb/subscriptions/default-rule"

	// downloaderDirectoriesPath 列出某个下载器下的可用目录。
	// 鉴权同样是 `API Key/JWT`，因此"校验下载器标识"这一步不需要登录。
	downloaderDirectoriesPath = "/api/v1/config/downloader/directories"
)

// DefaultRule 是上游的全局默认下载目标。
//
// SavePathLabel 是上游给该目录起的显示名（可能为空），仅用于界面提示；
// 真正要提交给下载接口的是 SavePath。
//
// 三个字段都可以为空，且**必须分别判断**：上游完全可能只配了目录、没配下载器，
// 这时 SavePath 有值不代表"读到了可用配置"——下载依然会因为缺标识而失败。
type DefaultRule struct {
	Downloader    string `json:"downloader"`
	SavePath      string `json:"save_path"`
	SavePathLabel string `json:"save_path_label"`
}

// FetchDefaultRule 读取上游的全局默认下载目标。
//
// 这条路刻意不缓存：它只在用户点"读取上游默认下载器"时按需调用，
// 加了缓存反而会让人看到过期值而怀疑自己改错了地方。
func (c *AvdbClient) FetchDefaultRule(ctx context.Context) (DefaultRule, error) {
	body, status, err := c.request(ctx, http.MethodGet, defaultRulePath, nil)
	if err != nil {
		// 上游在错误响应体里给的说明比通用文案有用得多，优先透传。
		if msg := upstreamMessage(body); msg != "" {
			return DefaultRule{}, &UpstreamError{Status: status, Msg: msg}
		}
		return DefaultRule{}, err
	}

	payload := unwrapEnvelope(body)

	// 先确认响应里真的有我们认识的字段。若一个都没有，多半是上游换了结构，
	// 这时必须报错而不是安静地返回空规则——否则界面会显示"上游没配默认下载器"，
	// 把"我们没读懂"伪装成"上游没有"，是最难排查的一类误导。
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(payload, &probe); err != nil {
		return DefaultRule{}, &UpstreamError{
			Status: status,
			Msg:    "上游返回的不是 JSON 对象：" + truncateForDisplay(string(body), 160),
		}
	}
	if _, ok := probe["downloader"]; !ok {
		if _, ok := probe["save_path"]; !ok {
			return DefaultRule{}, &UpstreamError{
				Status: status,
				Msg:    "上游的默认规则里没有下载器字段，接口结构可能已变化：" + truncateForDisplay(string(body), 160),
			}
		}
	}

	var rule DefaultRule
	if err := json.Unmarshal(payload, &rule); err != nil {
		return DefaultRule{}, &UpstreamError{
			Status: status,
			Msg:    "解析上游默认规则失败：" + truncateForDisplay(err.Error(), 120),
		}
	}
	rule.Downloader = strings.TrimSpace(rule.Downloader)
	rule.SavePath = strings.TrimSpace(rule.SavePath)
	rule.SavePathLabel = strings.TrimSpace(rule.SavePathLabel)
	return rule, nil
}

// DownloaderDirectories 列出某个下载器下的目录，用于校验标识并给出保存路径候选。
//
// 该接口访问令牌即可调用，所以"校验下载器"这一步不需要用户登录。
func (c *AvdbClient) DownloaderDirectories(ctx context.Context, downloaderID string) ([]string, error) {
	downloaderID = strings.TrimSpace(downloaderID)
	if downloaderID == "" {
		return nil, &UpstreamError{Status: http.StatusBadRequest, Msg: "下载器标识不能为空"}
	}

	q := url.Values{"downloader_id": {downloaderID}}
	body, status, err := c.request(ctx, http.MethodGet, downloaderDirectoriesPath, q)
	if err != nil {
		// 失败时上游往往在响应体里给出更具体的原因（例如 404 +
		// `未找到下载器: xxx`），这比通用的"接口不存在"有用得多。
		if msg := upstreamMessage(body); msg != "" {
			return nil, &UpstreamError{Status: status, Msg: msg}
		}
		return nil, err
	}

	dirs := harvestStrings(body)
	if len(dirs) > 0 {
		return dirs, nil
	}
	// 没拿到目录时，把上游的原话带回去，便于判断是"标识不存在"还是"该下载器无目录"。
	if msg := upstreamMessage(body); msg != "" {
		return nil, &UpstreamError{Status: http.StatusOK, Msg: msg}
	}
	return nil, nil
}

// unwrapEnvelope 剥掉 {code,message,data} 外壳，返回内层 data。
// 没有外壳、或 data 为空时原样返回，由调用方按自己的结构解析。
func unwrapEnvelope(body []byte) json.RawMessage {
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return json.RawMessage(body)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return json.RawMessage(body)
	}
	// code 非 0 表示业务失败，此时 data 通常为空，仍交给调用方看 message。
	return env.Data
}

// ---------------------------------------------------------- 下载器类型探测

// 上游对 /api/v1/config/downloader/directories 的三种回答。
// 前两种是固定文案，用它来反推"这个类型到底存不存在、配没配"。
const (
	probeStatusConfigured  = "configured"  // 类型存在且已配置（能读目录，或读目录时报的是它自己的错）
	probeStatusKnown       = "known"       // 类型存在，但上游没配置
	probeStatusUnsupported = "unsupported" // 上游压根不认识这个标识
	probeStatusError       = "error"       // 连不上/响应异常，结论未知
)

var (
	// unsupportedDownloaderHints 命中它说明这个标识不在上游的类型枚举里。
	unsupportedDownloaderHints = []string{"不支持的下载工具", "unsupported downloader", "unknown downloader type"}
	// unconfiguredDownloaderHints 命中它说明类型合法、但上游没有对应配置。
	unconfiguredDownloaderHints = []string{"未找到该下载工具配置", "未找到该下载器配置", "not configured"}
)

// DownloaderProbe 是单个下载器类型的探测结果。
type DownloaderProbe struct {
	ID          string   `json:"id"`
	Status      string   `json:"status"`
	Message     string   `json:"message,omitempty"`
	Directories []string `json:"directories,omitempty"`
}

// Configured 表示这个类型确实在上游配置过，可以用它提交下载。
func (p DownloaderProbe) Configured() bool { return p.Status == probeStatusConfigured }

// ProbeDownloader 探测单个下载器类型。
//
// 判断依据来自实测的三种回答：
//
//	未找到该下载工具配置  → 类型存在，但没配
//	不支持的下载工具      → 类型不存在
//	其余（含 CloudDrive 报的目录读取失败）→ 类型存在且已配置
//
// 最后一条是关键：CloudDrive 的目录接口会因为"网盘上没这个目录"而失败，
// 但那恰恰证明它**配过了**。所以不能用 code==0 当作"已配置"的判据。
func (c *AvdbClient) ProbeDownloader(ctx context.Context, id string) DownloaderProbe {
	id = strings.TrimSpace(id)
	if id == "" {
		return DownloaderProbe{Status: probeStatusError, Message: "下载器标识不能为空"}
	}

	dirs, err := c.DownloaderDirectories(ctx, id)
	if err == nil {
		return DownloaderProbe{ID: id, Status: probeStatusConfigured, Directories: dirs}
	}

	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		// 传输层错误（连不上、超时）：这与"下载器是否存在"无关，不能下结论。
		return DownloaderProbe{ID: id, Status: probeStatusError, Message: shortError(err)}
	}

	msg := shortError(upErr)
	switch {
	case containsAnyFold(msg, unsupportedDownloaderHints):
		// 前两种情况**不带** Message：状态标签本身（"上游未配置"/"上游不支持此类型"）
		// 已经把结论说完了，再贴一句上游原话（"未找到该下载工具配置"）纯属重复，
		// 而且上游用"工具"、我们用"下载器"，并排放着反而让人以为说的是两件事。
		return DownloaderProbe{ID: id, Status: probeStatusUnsupported}
	case containsAnyFold(msg, unconfiguredDownloaderHints):
		return DownloaderProbe{ID: id, Status: probeStatusKnown}
	default:
		// 已配置。到这里说明目录没读成功，Message 的作用只剩"解释为什么列不出目录"，
		// 所以翻译成人话再回给界面。
		return DownloaderProbe{ID: id, Status: probeStatusConfigured, Message: friendlyProbeMessage(msg)}
	}
}

// ProbeDownloaders 依次探测候选标识，返回全部结果与"第一个已配置的标识"。
//
// 串行而非并发：上游是本地服务，5 次探测耗时可以忽略，串行还能让报错顺序可复现。
func (c *AvdbClient) ProbeDownloaders(ctx context.Context, ids []string) ([]DownloaderProbe, string) {
	out := make([]DownloaderProbe, 0, len(ids))
	best := ""
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		p := c.ProbeDownloader(ctx, id)
		out = append(out, p)
		if best == "" && p.Configured() {
			best = p.ID
		}
	}
	return out, best
}

// containsAnyFold 大小写不敏感地判断文本是否包含任一特征片段。
func containsAnyFold(text string, needles []string) bool {
	lower := strings.ToLower(text)
	for _, n := range needles {
		if strings.Contains(lower, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

// ------------------------------------------------------ 探测报错的友好化

// cloudDriveMissingPathPattern 从 CloudDrive 的 gRPC 异常里抠出"哪一级目录不存在"。
//
// 上游原文形如：
//
//	get_subfiles of "/a/b/c" error: not found "c" under "/a/b"
//
// 两段分别是缺失的那一级与它的父目录，拼起来才是用户真正需要的完整路径。
var cloudDriveMissingPathPattern = regexp.MustCompile(`not found "([^"]+)" under "([^"]+)"`)

// dirMissingHints 是"目录不存在"类异常的兜底识别片段。
var dirMissingHints = []string{"StatusCode.NOT_FOUND", "not found"}

// friendlyProbeMessage 把"已配置但读不出目录"的上游报错压成一句人能读懂的话。
//
// 为什么必须翻译：CloudDrive 读目录失败时，上游会把整段 gRPC 异常回给我们——
// StatusCode、details、debug_error_string、created_time，外加一批 RPC 内部字段。
// 原样贴到页面上，用户只会得出"我的配置坏了"这个**错误**结论；
// 事实恰恰相反：那个下载器是好的，只是它记录的目标目录在网盘上不存在。
//
// 翻译后只保留两条对用户真正有用的信息：下载器可用；目录不存在，且不影响提交下载。
func friendlyProbeMessage(msg string) string {
	trimmed := strings.TrimSpace(msg)
	if trimmed == "" {
		return ""
	}

	if m := cloudDriveMissingPathPattern.FindStringSubmatch(trimmed); m != nil {
		return fmt.Sprintf("下载器可用；它记录的目录 %q 在网盘上不存在，因此列不出目录（不影响提交下载）。",
			joinDirPath(m[2], m[1]))
	}
	if containsAnyFold(trimmed, dirMissingHints) {
		return "下载器可用；读取目录时上游报「目录不存在」，因此列不出目录（不影响提交下载）。"
	}
	// 认不出来的情况宁可少给信息，也不要让一段 RPC 堆栈糊满页面。
	return truncateForDisplay(trimmed, 160)
}

// joinDirPath 拼接父目录与其中的一级名字，并避免出现双斜杠。
func joinDirPath(dir, name string) string {
	dir = strings.TrimRight(strings.TrimSpace(dir), "/")
	name = strings.TrimLeft(strings.TrimSpace(name), "/")
	switch {
	case dir == "" && name == "":
		return ""
	case dir == "":
		return "/" + name
	case name == "":
		return dir
	default:
		return dir + "/" + name
	}
}

// upstreamMessage 尽力从上游响应里抠出一句人能读懂的错误。
// 覆盖三种常见形态：{message}、{detail: "..."}、FastAPI 的 {detail: [...]}。
func upstreamMessage(body []byte) string {
	var probe struct {
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	if msg := strings.TrimSpace(probe.Message); msg != "" {
		return msg
	}
	if len(probe.Detail) > 0 && string(probe.Detail) != "null" {
		var s string
		if json.Unmarshal(probe.Detail, &s) == nil {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
		if translated := fastAPIValidationMessage(body); translated != "" {
			return translated
		}
	}
	return ""
}

// shortError 把错误压成一句短语，供界面展示。
// 上游错误文本会进界面，因此统一截断，避免异常上游用超长文本糊满页面。
func shortError(err error) string {
	if err == nil {
		return ""
	}
	var upErr *UpstreamError
	if errors.As(err, &upErr) {
		return truncateForDisplay(upErr.Msg, 160)
	}
	return truncateForDisplay(err.Error(), 160)
}
