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

	// 上游请求默认超时（秒）。
	defaultTimeoutSec = 20

	// defaultMaxResults 是安全阀的**默认值**：0 = 不限制。
	//
	// 它曾经默认 5000，于是用户在宽泛关键词下会看到「结果过多，只展示前 5000 条」——
	// 而本站的定位是"搜索没有上限、靠分页展示全部"，这个默认值与承诺自相矛盾。
	// 现在默认完全不截断，只有显式配置了正整数（环境变量或配置文件）才生效。
	// 真正决定"一屏显示多少"的始终是 PageSize。
	defaultMaxResults = 0
	// maxAllowedResults 是显式配置时的上界，防止有人写一个离谱的值把内存打满。
	maxAllowedResults = 50000

	// defaultPageSize 是每页展示条数，可在结果页与设置页调整并持久化。
	defaultPageSize = 100

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
	// Downloader 提交离线下载时使用的下载器标识。
	//
	// 上游改版后该参数**已从选填变为必填**：实测 `GET /api/v1/articles/download/manul`
	// 缺这个参数会直接返回 422（`loc:["query","downloader"]`），
	// 上游自己的 openapi.json 里它也标着 `required: true`。
	// 所以"留空 = 让上游用自己的默认下载器"这条老路已经走不通了——
	// 上游宁可报错，也不会替你选。
	//
	// 留空时的兜底值由 EffectiveDownloader 给出，见那里的说明。
	// 想要真实值，用 GET /api/v1/javdb/subscriptions/default-rule 向上游要，
	// 或用设置页的「探测可用下载器」按钮问（两者都只需 API Key）。
	Downloader string `json:"default_downloader"`
	// SavePath 提交离线下载时的保存路径。
	//
	// 它同样是**必填且不能为空**：缺失返回 422，传空串返回「保存目录不能为空」。
	// 也就是说上游把"用哪个目录"的决定权完全交还给调用方。
	SavePath string `json:"default_save_path"`
	// SavePathOptions 是最近一次从上游列出的目录候选，仅供设置页做下拉提示。
	SavePathOptions []string `json:"save_path_options,omitempty"`
	// Addr HTTP 监听地址。部署参数，仅环境变量生效。
	Addr string `json:"listen_addr"`
	// TimeoutSec 单次上游请求超时（秒）。
	TimeoutSec int `json:"timeout_seconds"`
	// MaxResults 是可选安全阀：单次搜索最多接受的条目数。**0（默认）表示不限制**。
	//
	// 它属于**部署参数**（与 Addr、ConfigDir 同级），只认环境变量 AVDB_MAX_RESULTS，
	// 配置文件里写了也不作数——所以 json tag 是 "-"，既不读出也不写入。
	//
	// 为什么必须从配置文件里退役：
	//   老版本的默认值是 5000，而 Save() 会把整个结构体落盘（该字段没有 omitempty），
	//   于是任何存量的 config.json 里都静静躺着一个 "max_results": 5000。
	//   只把默认值改成 0 是**治不好**这个病的——升级二进制后，那个 5000 依然会被读回来，
	//   用户照样看到「结果过多，只展示前 5000 条」，也就是他报上来的那个问题。
	//   把这个字段从文件里彻底摘掉，存量配置才会自动恢复成"不限制"。
	MaxResults int `json:"-"`
	// PageSize 是结果页每页展示的条数，取值见 AllowedPageSizes()。
	// 0 表示"全部显示"（不分页）。该值由用户在结果页或设置页选择后**持久化**，
	// 下次打开仍然是这个值——它是偏好，不是单次请求参数。
	PageSize int `json:"page_size"`
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
		// PageSize 必须在这里显式给初值：它的零值 0 是一个**合法取值**
		// （表示"全部显示"），不能靠"零值即默认"来自动兜底，
		// 否则不设任何配置时页面会一次列完全部结果。
		PageSize:  defaultPageSize,
		ConfigDir: dir,
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
			notes = append(notes, retiredConfigKeyNotes(data)...)
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

// retiredConfigKey 描述一个"已从配置文件退役、只认环境变量"的字段。
type retiredConfigKey struct {
	// Key 是配置文件里不再生效的 JSON 键。
	Key string
	// Env 是替代它、且唯一有效的环境变量名。
	Env string
}

// retiredConfigKeys 列出所有退役字段。
//
// 退役了还要报一声，是为了避免哑谜：用户在文件里改了却不生效、
// 又查不出原因，是最难排查的一类问题。
var retiredConfigKeys = []retiredConfigKey{
	{Key: "max_results", Env: "AVDB_MAX_RESULTS"},
}

// retiredConfigKeyNotes 检查配置文件里是否还残留退役字段，逐条给出替代做法。
//
// 解析失败时返回 nil——那种情况 ResolveConfig 已经另有"解析失败"的提示，不必重复报。
func retiredConfigKeyNotes(data []byte) []string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}

	var notes []string
	for _, retired := range retiredConfigKeys {
		if _, present := raw[retired.Key]; present {
			notes = append(notes, fmt.Sprintf(
				"配置文件里的 %q 已不再生效（搜索结果现在默认不限制条数）；如需限制请改用环境变量 %s",
				retired.Key, retired.Env))
		}
	}
	return notes
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
	// 注意 AVDB_MAX_RESULTS 不在这里读：它已划归部署参数，
	// 统一放到 applyDeploymentEnv（配置文件合并之后再定），见那里的说明。

	// 每页条数允许 0（= 全部），所以不能复用 setInt（那个只收正整数）。
	if v, ok := os.LookupEnv("AVDB_PAGE_SIZE"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.PageSize = n
		} else {
			notes = append(notes, fmt.Sprintf("环境变量 AVDB_PAGE_SIZE=%q 不是非负整数，已忽略", v))
		}
	}

	return notes
}

// applyDeploymentEnv 只处理"属于部署环境"的字段，在配置文件合并之后调用，
// 保证这些字段永远由环境说了算。
func applyDeploymentEnv(cfg *Config) []string {
	var notes []string

	// 监听地址三选一，按优先级取第一个有值的（AVDB_ADDR > AVDB_LISTEN > PORT）。
	for _, key := range []string{"AVDB_ADDR", "AVDB_LISTEN", "PORT"} {
		if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
			cfg.Addr = normalizeAddr(strings.TrimSpace(v))
			break
		}
	}

	// 结果上限：与监听地址同级，只认环境变量（配置文件里的 max_results 已退役）。
	//
	// 这里是"非负整数"而不是"正整数"——0 是合法取值，含义是**不限制**，也就是默认值。
	// 复用不了 applyEnv 里的 setInt：那个校验的是 n <= 0 就丢弃，会把"显式不限制"当成非法输入。
	if v, ok := os.LookupEnv("AVDB_MAX_RESULTS"); ok && strings.TrimSpace(v) != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.MaxResults = n
		} else {
			notes = append(notes, fmt.Sprintf("环境变量 AVDB_MAX_RESULTS=%q 不是非负整数，已忽略（按不限制处理）", v))
		}
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

	// MaxResults 为 0 或负数一律归为"不限制"。
	// 注意不能沿用"<=0 就回默认值"的写法：默认值本身就是 0（不限制），
	// 那样写会把"显式关掉限制"误当成"没配置"。
	if cfg.MaxResults < 0 {
		cfg.MaxResults = 0
	}
	if cfg.MaxResults > maxAllowedResults {
		cfg.MaxResults = maxAllowedResults
	}
	cfg.PageSize = normalizePageSize(cfg.PageSize)
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

// ------------------------------------------------- 每页条数（分页）与下载目标

// pageSizeChoices 是结果页/设置页允许选择的每页条数。
//
// 0 是一个**合法取值**，含义是"全部显示、不分页"。因此：
//   - 不能用 `<= 0 就回退默认值` 这种写法做归一化，否则"全部"会被悄悄改成 100；
//   - 白名单之外的取值（手工改 URL、脏配置）一律回退到默认值，
//     避免有人用 ?page_size=100000 让本站一次渲染十万行。
var pageSizeChoices = []int{10, 50, 100, 200, 300, 500, 1000, 0}

// AllowedPageSizes 返回可选的每页条数（含末尾的 0 = 全部）。
func AllowedPageSizes() []int {
	out := make([]int, len(pageSizeChoices))
	copy(out, pageSizeChoices)
	return out
}

// IsAllowedPageSize 判断取值是否在白名单内。
func IsAllowedPageSize(n int) bool {
	for _, v := range pageSizeChoices {
		if v == n {
			return true
		}
	}
	return false
}

// normalizePageSize 把任意整数收敛到白名单内的合法值。
func normalizePageSize(n int) int {
	if IsAllowedPageSize(n) {
		return n
	}
	return defaultPageSize
}

// PageSizeLabel 把每页条数转成界面文案。
func PageSizeLabel(n int) string {
	if n <= 0 {
		return "全部"
	}
	return strconv.Itoa(n)
}

// ---------------------------------------------------------- 下载目标的解析

// supportedDownloaderIDs 是上游已知的下载器**类型**标识，用于「探测可用下载器」。
//
// 这份清单不是抄来的，是用 GET /api/v1/config/downloader/directories 逐个试出来的：
// 上游对不认识的取值回「不支持的下载工具」，对认识但没配的回「未找到该下载工具配置」，
// 其余响应（哪怕是 CloudDrive 报的目录读取失败）都说明这个类型是配过的。
// 前四个来自实测的枚举，clouddrive 是本机上游当前唯一配置过的那一个。
var supportedDownloaderIDs = []string{"clouddrive", "115", "qbittorrent", "transmission", "thunder"}

// fallbackDownloaderID 是配置为空时的兜底下载器标识。
//
// 为什么可以兜底、而不再"什么都不填"：上游把 downloader 改成了必填，
// 不填的结果是 100% 失败（422），兜底至少给了一条能走通的路。
//
// 为什么是 clouddrive：本机上游实测只有一个下载器被真正配置过，就是它
// （其余 4 个类型都回「未找到该下载工具配置」）；而上游正是通过 CloudDrive2
// 把离线任务投给 115 网盘。这个值只是**兜底**，设置页里填了任何值都会覆盖它。
const fallbackDownloaderID = "clouddrive"

// downloaderChoices 返回设置页「下载器」下拉的选项。
//
// 除了内置候选，还要把**当前配置值**并进去：用户的上游可能用了我们还不认识的
// 新类型，若直接丢弃，下拉框会显示成另一个值，用户点一次保存就把真实配置改掉了。
func downloaderChoices(current string) []string {
	out := make([]string, 0, len(supportedDownloaderIDs)+1)
	out = append(out, supportedDownloaderIDs...)

	current = strings.TrimSpace(current)
	if current == "" {
		return out
	}
	for _, id := range out {
		if id == current {
			return out
		}
	}
	return append(out, current)
}

// EffectiveDownloader 返回本次提交真正要发给上游的下载器标识。
// 优先用户显式配置的值，为空时用兜底值——绝不会返回空串。
func (c Config) EffectiveDownloader() string {
	if v := strings.TrimSpace(c.Downloader); v != "" {
		return v
	}
	return fallbackDownloaderID
}

// resolveDownloadTarget 解析出一次下载提交所需的两个参数，并给出"为什么不行"。
//
// 两个参数的规则不一致，这是上游定的，不是我们的选择：
//   - downloader 必有值（配置为空时走兜底），所以它总能拿到；
//   - save_path 只能由用户提供——上游不接受空值，我们也不能替他猜一个目录
//     （猜错的代价是文件被丢到一个不存在的路径，而调用方毫不知情）。
//
// 返回的 ok 为 false 时，message 是给用户看的、可操作的说明。
func (c Config) resolveDownloadTarget() (downloader, savePath, message string, ok bool) {
	if path := strings.TrimSpace(c.SavePath); path != "" {
		return c.EffectiveDownloader(), path, "", true
	}
	return c.EffectiveDownloader(), "", "上游要求保存目录不能为空。" +
		"请到「设置」页填写「保存路径」——可用「校验下载器并列出目录」把上游已配置的目录列出来直接选；" +
		"也可以点「读取上游默认下载器」，把上游自己配的目录继承过来。", false
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
