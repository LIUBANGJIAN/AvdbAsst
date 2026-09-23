package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultAddr 是容器内的默认监听地址；对外端口通过 -p 映射。
	defaultAddr = ":8080"

	// defaultAPIBaseURL 只是占位。真实地址请在部署时通过环境变量或页面配置。
	defaultAPIBaseURL = "http://127.0.0.1:8999"

	// 上游请求默认超时（秒）与结果条数上限。
	defaultTimeoutSec = 20
	defaultMaxResults = 500

	configFileName = "config.json"

	// envConfigDir 指定配置文件落盘目录。容器内固定为 /data，
	// 配合卷挂载即可实现重启不丢配置。
	envConfigDir = "AVDB_CONFIG_DIR"
)

// Config 汇总运行期配置。
//
// 优先级（后者覆盖前者）：
//
//	内置默认值  <  环境变量  <  配置文件
//
// 配置文件排在最后，是因为它由「设置」页面写入——用户在页面上显式改过的东西
// 必须能生效，否则就成了"改了没反应"。环境变量在这里的角色是**首次启动的初值**
// （容器部署时用来预置），而不是最终裁决者。
//
// 例外：监听地址与配置目录属于部署参数，只能由环境变量决定，文件里改了也不作数。
type Config struct {
	// APIBaseURL 上游 Avdb 服务地址，例如 http://192.168.1.20:8000
	APIBaseURL string `json:"api_base_url"`
	// APIKey 上游鉴权用的 X-API-Key。
	APIKey string `json:"api_key"`
	// Downloader 提交离线下载时使用的下载器标识，留空表示**交给上游决定**。
	//
	// 留空的正确实现是「不发送 downloader 这个 query 参数」——上游会继承它的
	// 全局订阅下载器。注意别写成"发送空串"：上游会把空串当成一个名叫"空"
	// 的下载器去查，然后回你一句 `未找到下载器`。这是本项目踩过的坑，
	// 见 avdb.go 的 SubmitDownload。
	//
	// 也不要在代码里猜一个默认标识：猜测的代价是用户拿到一句看不懂的
	// `未找到下载器: Downloader.<猜测值>`。想要真实值，用
	// GET /api/v1/javdb/subscriptions/default-rule 向上游要（只需 API Key）。
	Downloader string `json:"default_downloader"`
	// SavePath 提交离线下载时的保存路径，留空表示交给上游决定。
	// 与 downloader 不同，上游把这个参数判为**必填**（缺失直接 422），
	// 所以它始终会被发送，空值也发。
	SavePath string `json:"default_save_path"`
	// SavePathOptions 是最近一次从上游列出的目录候选，仅供设置页做下拉提示。
	SavePathOptions []string `json:"save_path_options,omitempty"`
	// Addr HTTP 监听地址。部署参数，仅环境变量生效。
	Addr string `json:"listen_addr"`
	// TimeoutSec 单次上游请求超时（秒）。
	TimeoutSec int `json:"timeout_seconds"`
	// MaxResults 单次搜索最多展示的条目数。
	MaxResults int `json:"max_results"`
	// AccessToken 可选访问口令。留空表示完全开放（默认）。
	// 设置后，请求需携带 ?token=xxx 或 X-Auth-Token 头；首次带 token
	// 访问会下发同值 Cookie，后续请求即可自动通过。
	AccessToken string `json:"access_token"`

	// ConfigDir 配置文件所在目录。部署参数，不写入配置文件本身。
	ConfigDir string `json:"-"`
	// Timeout 由 TimeoutSec 推导，不参与序列化。
	Timeout time.Duration `json:"-"`
}

// ResolveConfigDir 返回配置文件目录。
// 默认 "."（本地直接运行二进制），容器内由 Dockerfile 设为 /data。
func ResolveConfigDir() string {
	if v := strings.TrimSpace(os.Getenv(envConfigDir)); v != "" {
		return v
	}
	return "."
}

// ConfigPath 返回配置文件的完整路径。
func (c Config) ConfigPath() string {
	dir := c.ConfigDir
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, configFileName)
}

// ResolveConfig 按 默认值 → 环境变量 → 配置文件 的顺序合成配置。
// 返回的 notes 是需要提醒用户但不致命的信息。
func ResolveConfig() (Config, []string) {
	dir := ResolveConfigDir()

	cfg := Config{
		APIBaseURL: defaultAPIBaseURL,
		Addr:       defaultAddr,
		TimeoutSec: defaultTimeoutSec,
		MaxResults: defaultMaxResults,
		ConfigDir:  dir,
	}

	var notes []string

	// 第 1 层：环境变量，作为首次启动的初值。
	notes = append(notes, applyEnv(&cfg)...)

	// 第 2 层：配置文件（设置页面写入的内容）。
	path := filepath.Join(dir, configFileName)
	if data, err := os.ReadFile(path); err == nil {
		// Unmarshal 只覆盖 JSON 中出现的字段，未出现的保留上一层的结果，
		// 这正是我们要的"叠加"语义。
		if err := json.Unmarshal(data, &cfg); err != nil {
			notes = append(notes, fmt.Sprintf("%s 解析失败，该文件被忽略：%v", path, err))
		} else {
			notes = append(notes, fmt.Sprintf("已加载配置文件 %s", path))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		notes = append(notes, fmt.Sprintf("读取 %s 失败，已改用环境变量配置：%v", path, err))
	}

	// 部署参数不允许被配置文件改写（改监听地址和配置目录都需要重启，
	// 让它们在页面上"看起来能改"只会制造误解）。
	cfg.ConfigDir = dir
	notes = append(notes, applyDeploymentEnv(&cfg)...)

	return normalizeConfig(cfg), notes
}

// applyEnv 读取环境变量作为初值。
func applyEnv(cfg *Config) []string {
	var notes []string

	setStr := func(dst *string, keys ...string) {
		for _, k := range keys {
			if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
				*dst = strings.TrimSpace(v)
				return
			}
		}
	}
	setInt := func(dst *int, key string) {
		v, ok := os.LookupEnv(key)
		if !ok || strings.TrimSpace(v) == "" {
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n <= 0 {
			notes = append(notes, fmt.Sprintf("环境变量 %s=%q 不是正整数，已忽略", key, v))
			return
		}
		*dst = n
	}

	setStr(&cfg.APIBaseURL, "AVDB_API_BASE_URL", "AVDB_API_URL")
	setStr(&cfg.APIKey, "AVDB_API_KEY")
	setStr(&cfg.Downloader, "AVDB_DOWNLOADER")
	setStr(&cfg.SavePath, "AVDB_SAVE_PATH")
	setStr(&cfg.AccessToken, "AVDB_ACCESS_TOKEN")
	setInt(&cfg.TimeoutSec, "AVDB_TIMEOUT_SECONDS")
	setInt(&cfg.MaxResults, "AVDB_MAX_RESULTS")

	return notes
}

// applyDeploymentEnv 只处理"属于部署环境"的字段，在配置文件合并之后调用，
// 保证这些字段永远由环境说了算。
func applyDeploymentEnv(cfg *Config) []string {
	var notes []string
	if v, ok := os.LookupEnv("AVDB_ADDR"); ok && strings.TrimSpace(v) != "" {
		cfg.Addr = normalizeAddr(strings.TrimSpace(v))
		return notes
	}
	if v, ok := os.LookupEnv("AVDB_LISTEN"); ok && strings.TrimSpace(v) != "" {
		cfg.Addr = normalizeAddr(strings.TrimSpace(v))
		return notes
	}
	if v, ok := os.LookupEnv("PORT"); ok && strings.TrimSpace(v) != "" {
		// 兼容 PaaS / Docker 常见的 PORT 约定。
		cfg.Addr = normalizeAddr(strings.TrimSpace(v))
	}
	return notes
}

// normalizeConfig 做统一的后置整理，避免各处重复裁剪。
func normalizeConfig(cfg Config) Config {
	cfg.APIBaseURL = strings.TrimRight(strings.TrimSpace(cfg.APIBaseURL), "/")
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.AccessToken = strings.TrimSpace(cfg.AccessToken)
	cfg.Downloader = strings.TrimSpace(cfg.Downloader)
	cfg.SavePath = strings.TrimSpace(cfg.SavePath)
	cfg.Addr = strings.TrimSpace(cfg.Addr)

	// 下载器留空是合法状态，代表"让上游用自己的默认下载器"，因此这里不做补值。
	// 真实值只可能来自两处：用户显式填写，或上游的 default-rule 接口。
	cfg.SavePathOptions = normalizeStringList(cfg.SavePathOptions, 100)

	if cfg.MaxResults <= 0 {
		cfg.MaxResults = defaultMaxResults
	}
	if cfg.MaxResults > 5000 {
		cfg.MaxResults = 5000
	}
	if cfg.TimeoutSec <= 0 {
		cfg.TimeoutSec = defaultTimeoutSec
	}
	if cfg.TimeoutSec > 600 {
		cfg.TimeoutSec = 600
	}
	if cfg.Addr == "" {
		cfg.Addr = defaultAddr
	}
	cfg.Timeout = time.Duration(cfg.TimeoutSec) * time.Second
	return cfg
}

// normalizeAddr 把 "8080" 这类裸端口补成 ":8080"，避免 ListenAndServe 报错。
func normalizeAddr(v string) string {
	if v == "" {
		return defaultAddr
	}
	if strings.Contains(v, ":") {
		return v
	}
	return ":" + v
}

// normalizeStringList 清洗字符串清单：去空白、去重、保序、限量。
func normalizeStringList(in []string, limit int) []string {
	if limit <= 0 {
		limit = 100
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// Validate 校验配置的自洽性。缺失 API Key 不致命（上游可能未开启鉴权），
// 但地址不合法会让整个服务毫无意义，必须拦住。
func (c Config) Validate() error {
	if strings.TrimSpace(c.APIBaseURL) == "" {
		return errors.New("上游 API 地址为空：请在设置页面填写，或设置环境变量 AVDB_API_BASE_URL")
	}
	u, err := url.Parse(c.APIBaseURL)
	if err != nil {
		return fmt.Errorf("上游 API 地址无法解析: %q: %w", c.APIBaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("上游 API 地址必须以 http:// 或 https:// 开头，当前为 %q", c.APIBaseURL)
	}
	if u.Host == "" {
		return fmt.Errorf("上游 API 地址缺少主机名: %q", c.APIBaseURL)
	}
	return nil
}

// Warnings 返回非致命但值得提醒用户的配置问题。
func (c Config) Warnings() []string {
	var out []string
	if c.APIKey == "" {
		out = append(out, "未配置 API Key。若上游开启了鉴权，搜索会返回 401；可在设置页面填写")
	}
	if c.AccessToken == "" {
		out = append(out, "未设置访问口令，服务对本网段完全开放；暴露到公网前请在设置页面设置")
	}
	return out
}

// ---------------------------------------------------------------- 持久化

// Save 原子地把配置写入持久化目录。
//
// 实现要点：
//  1. 先写临时文件、fsync、再 rename。中途断电最多丢掉这一次修改，
//     不会留下一个半截的 JSON 让服务下次启动直接失败；
//  2. 权限 0600——文件里有 API Key 和访问口令。
func (c Config) Save() error {
	dir := c.ConfigDir
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建配置目录 %s 失败（容器内请确认已挂载卷）：%w", dir, err)
	}

	// ConfigDir 与 Timeout 的 json tag 是 "-"，不会被写进文件。
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败：%w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, configFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("在 %s 创建临时文件失败（目录可能只读）：%w", dir, err)
	}
	tmpName := tmp.Name()
	// rename 成功后这里会失败，忽略即可；失败时它就是清理动作。
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时配置失败：%w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("设置配置权限失败：%w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("落盘配置失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时配置失败：%w", err)
	}
	if err := os.Rename(tmpName, c.ConfigPath()); err != nil {
		return fmt.Errorf("替换配置文件失败：%w", err)
	}
	return nil
}

// ConfigDirWritable 探测配置目录是否可写，供设置页面提前给出提示，
// 而不是等用户点了保存才报错。
func (c Config) ConfigDirWritable() bool {
	dir := c.ConfigDir
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	probe, err := os.CreateTemp(dir, ".write-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return true
}

// MaskSecret 把密钥压成 "abcd…wxyz" 形式，用于在页面上表明"已配置"，
// 同时避免把完整凭证渲染进 HTML。
//
// 官方文档明确要求令牌不得进入前端，所以页面上永远只显示掩码；
// 输入框留空即表示"不修改"。
func MaskSecret(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	runes := []rune(secret)
	switch {
	case len(runes) <= 4:
		return strings.Repeat("•", len(runes))
	case len(runes) <= 8:
		return string(runes[:2]) + strings.Repeat("•", 6)
	default:
		return string(runes[:4]) + strings.Repeat("•", 8) + string(runes[len(runes)-4:])
	}
}
