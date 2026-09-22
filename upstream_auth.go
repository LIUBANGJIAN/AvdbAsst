package main

// 上游"设置类"接口的访问层：登录换取 JWT、读取配置、枚举下载器与目录。
//
// 为什么单独一个文件：
//  1. avdb.go 已接近 700 行，继续堆会变成难以维护的巨石文件；
//  2. 这里的接口凭据模型与业务接口**不同**——/api/v1/config/{key} 只认登录 JWT，
//     不接受访问令牌（X-API-Key），混在业务客户端里容易误用。
//
// 设计原则：
//   - 密码只在上游请求里出现一次，绝不落盘、绝不写日志；
//   - JWT 同样不落盘，用完即弃（下载器清单是低频数据，缓存清单即可）；
//   - 上游响应结构未公开承诺，因此所有解析都做**防御式**处理：
//     解析不出来就返回空值并附上原文，交给页面显示真实内容，而不是假装成功。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	loginPath    = "/api/v1/users/login"
	login2FAPath = "/api/v1/users/login/2fa"

	configPathPrefix = "/api/v1/config/"

	// downloaderDirectoriesPath 列出某个下载器下的可用目录。
	// 官方文档标注鉴权为 `API Key/JWT`，即**不需要登录**也能用，
	// 这是本站校验"下载器标识是否有效"的主要手段。
	downloaderDirectoriesPath = "/api/v1/config/downloader/directories"
)

// LoginResult 是登录接口的归一化结果。
// JWT 与 OTPToken 二者必有其一：拿到 OTPToken 表示还需一步二次验证。
type LoginResult struct {
	JWT      string
	OTPToken string
}

// Needs2FA 表示账号开启了两步验证，需要用户补一个验证码。
func (r LoginResult) Needs2FA() bool { return r.JWT == "" && r.OTPToken != "" }

// Login 用用户名 + 密码换 JWT。
//
// 上游要求 application/x-www-form-urlencoded（不是 JSON）。
// 若账号开启两步验证，这里会返回 OTPToken，调用方需再用 Login2FA 补一次。
func (c *AvdbClient) Login(ctx context.Context, username, password string) (LoginResult, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return LoginResult{}, &UpstreamError{Status: http.StatusBadRequest, Msg: "用户名不能为空"}
	}
	if password == "" {
		return LoginResult{}, &UpstreamError{Status: http.StatusBadRequest, Msg: "密码不能为空"}
	}

	form := url.Values{"username": {username}, "password": {password}}
	body, status, err := c.do(ctx, http.MethodPost, loginPath, nil, form, "")
	if err != nil {
		return LoginResult{}, err
	}
	return normalizeLoginResult(body, status, "登录")
}

// Login2FA 完成两步验证，返回最终 JWT。
func (c *AvdbClient) Login2FA(ctx context.Context, otpToken, code string) (string, error) {
	otpToken = strings.TrimSpace(otpToken)
	code = strings.TrimSpace(code)
	if otpToken == "" {
		return "", &UpstreamError{Status: http.StatusBadRequest, Msg: "缺少 otp_token，请重新登录"}
	}
	if code == "" {
		return "", &UpstreamError{Status: http.StatusBadRequest, Msg: "验证码不能为空"}
	}

	form := url.Values{"otp_token": {otpToken}, "otp_code": {code}}
	body, status, err := c.do(ctx, http.MethodPost, login2FAPath, nil, form, "")
	if err != nil {
		return "", err
	}
	result, err := normalizeLoginResult(body, status, "二次验证")
	if err != nil {
		return "", err
	}
	if result.Needs2FA() {
		return "", &UpstreamError{Status: status, Msg: "二次验证未通过，请确认验证码是否正确或已过期"}
	}
	return result.JWT, nil
}

// normalizeLoginResult 从登录/二次验证响应里提取令牌，提取不到就给出可读原因。
func normalizeLoginResult(body []byte, status int, action string) (LoginResult, error) {
	access, otp := extractLoginTokens(body)
	switch {
	case access != "":
		return LoginResult{JWT: access}, nil
	case otp != "":
		return LoginResult{OTPToken: otp}, nil
	}

	msg := upstreamMessage(body)
	if msg == "" {
		msg = fmt.Sprintf("响应里没有 access_token（HTTP %d）", status)
	}
	return LoginResult{}, &UpstreamError{Status: status, Msg: action + "失败：" + msg}
}

// extractLoginTokens 兼容两种摆放位置：data.access_token 与顶层 access_token。
// 上游不同版本两种都出现过，且没有任何版本承诺过结构，所以两处都看。
func extractLoginTokens(body []byte) (access, otp string) {
	var raw struct {
		AccessToken string `json:"access_token"`
		OTPToken    string `json:"otp_token"`
		Data        struct {
			AccessToken string `json:"access_token"`
			OTPToken    string `json:"otp_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", ""
	}
	access = strings.TrimSpace(firstNonEmpty(raw.Data.AccessToken, raw.AccessToken))
	otp = strings.TrimSpace(firstNonEmpty(raw.Data.OTPToken, raw.OTPToken))
	return access, otp
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

// FetchConfig 读取一个上游配置项。**必须**用 JWT：该路由不接受访问令牌。
func (c *AvdbClient) FetchConfig(ctx context.Context, jwt, key string) (json.RawMessage, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, &UpstreamError{Status: http.StatusBadRequest, Msg: "配置键不能为空"}
	}

	body, _, err := c.do(ctx, http.MethodGet, configPathPrefix+url.PathEscape(key), nil, nil, jwt)
	if err != nil {
		return nil, err
	}

	// 有 {code,message,data} 外壳就剥掉 data；没有就原样返回。
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err == nil && len(env.Data) > 0 && string(env.Data) != "null" {
		if env.Code == 0 {
			return env.Data, nil
		}
		if msg := strings.TrimSpace(env.Message); msg != "" {
			return nil, &UpstreamError{Status: http.StatusOK, Msg: msg}
		}
	}
	return json.RawMessage(body), nil
}

// DownloaderOption 是一个可选的下载器。
type DownloaderOption struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// downloaderConfigKeys 是"下载器清单"可能所在的配置键，按可能性排序。
//
// 上游没有公开"列出下载器"的专用接口，清单藏在通用配置项里，而配置键名
// 未在文档中承诺。因此这里逐个探测，命中即止；全部落空也不算失败——
// 页面会退回到"手工填写 + 在线校验"的路径，用户依然能完成任务。
var downloaderConfigKeys = []string{
	"downloader",
	"downloaders",
	"download",
	"default_downloader",
}

// ProbeDownloaders 依次探测候选配置键，返回找到的下载器清单与探测轨迹。
//
// 返回值：(清单, 探测轨迹, error)。清单为空且 error 为 nil 表示"没找到"，
// 这不是错误，调用方应引导用户手工填写。探测轨迹用于把过程透明地展示给用户。
func (c *AvdbClient) ProbeDownloaders(ctx context.Context, jwt string) ([]DownloaderOption, []string, error) {
	tried := make([]string, 0, len(downloaderConfigKeys))

	for _, key := range downloaderConfigKeys {
		raw, err := c.FetchConfig(ctx, jwt, key)
		if err != nil {
			tried = append(tried, key+"：读取失败（"+shortError(err)+"）")
			continue
		}
		opts := harvestDownloaderOptions(raw)
		if len(opts) > 0 {
			tried = append(tried, fmt.Sprintf("%s：命中 %d 个", key, len(opts)))
			return opts, tried, nil
		}
		tried = append(tried, key+"：无下载器字段")
	}
	return nil, tried, nil
}

// DownloaderDirectories 列出某个下载器下的目录，用于校验标识并给出保存路径候选。
//
// 该接口 API Key 即可调用，所以"校验下载器"这一步不需要用户登录。
func (c *AvdbClient) DownloaderDirectories(ctx context.Context, downloaderID string) ([]string, error) {
	downloaderID = strings.TrimSpace(downloaderID)
	if downloaderID == "" {
		return nil, &UpstreamError{Status: http.StatusBadRequest, Msg: "下载器标识不能为空"}
	}

	q := url.Values{"downloader_id": {downloaderID}}
	body, status, err := c.do(ctx, http.MethodGet, downloaderDirectoriesPath, q, nil, "")
	if err != nil {
		// 失败时上游往往在响应体里给出更具体的原因（例如 404 +
		// `未找到下载器: xxx`），这比通用的"接口不存在"有用得多。
		// do() 在错误路径上同样会把 body 带回来，这里直接利用。
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

// shortError 把错误压成一句短语，供探测轨迹展示。
// 上游错误文本会进界面，因此统一截断，避免异常上游用超长文本糊满页面。
func shortError(err error) string {
	if err == nil {
		return ""
	}
	var upErr *UpstreamError
	if errors.As(err, &upErr) {
		return truncateForDisplay(upErr.Msg, 120)
	}
	return truncateForDisplay(err.Error(), 120)
}
