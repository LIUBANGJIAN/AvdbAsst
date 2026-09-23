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
	"net/http"
	"net/url"
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
