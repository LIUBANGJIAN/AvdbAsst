package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearConfigEnv 清空所有与本项目相关的环境变量，保证测试不受宿主环境影响。
// 注意 Setenv("") 会让 LookupEnv 返回空串，而 applyEnv 把空串视为"未设置"。
func clearConfigEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"AVDB_API_BASE_URL", "AVDB_API_URL", "AVDB_API_KEY",
		"AVDB_DOWNLOADER", "AVDB_SAVE_PATH", "AVDB_ACCESS_TOKEN",
		"AVDB_ADDR", "AVDB_LISTEN", "AVDB_TIMEOUT_SECONDS", "AVDB_MAX_RESULTS",
		"AVDB_CONFIG_DIR", "PORT",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}
}

func TestResolveConfigDefaults(t *testing.T) {
	t.Chdir(t.TempDir()) // 空目录，确保读不到 config.json
	clearConfigEnv(t)

	cfg, notes := ResolveConfig()

	if cfg.Addr != defaultAddr {
		t.Errorf("Addr = %q, 期望 %q", cfg.Addr, defaultAddr)
	}
	if cfg.TimeoutSec != defaultTimeoutSec {
		t.Errorf("TimeoutSec = %d, 期望 %d", cfg.TimeoutSec, defaultTimeoutSec)
	}
	if cfg.MaxResults != defaultMaxResults {
		t.Errorf("MaxResults = %d, 期望 %d", cfg.MaxResults, defaultMaxResults)
	}
	if want := time.Duration(defaultTimeoutSec) * time.Second; cfg.Timeout != want {
		t.Errorf("Timeout = %v, 期望 %v", cfg.Timeout, want)
	}
	// 没有 config.json 不应产生任何提示（这是合法的启动方式）。
	for _, n := range notes {
		if strings.Contains(n, "已加载本地配置") {
			t.Errorf("空目录不应报告加载了 config.json，实际提示: %v", notes)
		}
	}
}

func TestResolveConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, configFileName), []byte(`{
		"api_base_url": "http://192.168.1.20:8000/",
		"api_key": "file-key",
		"default_downloader": "115",
		"listen_addr": ":9000",
		"max_results": 42
	}`), 0o600)
	if err != nil {
		t.Fatalf("写入 config.json 失败: %v", err)
	}
	t.Chdir(dir)
	clearConfigEnv(t)

	cfg, notes := ResolveConfig()

	if cfg.APIKey != "file-key" {
		t.Errorf("APIKey = %q, 期望 file-key", cfg.APIKey)
	}
	// 结尾斜杠必须被裁掉，否则拼接出来会是 //api/v1/...
	if cfg.APIBaseURL != "http://192.168.1.20:8000" {
		t.Errorf("APIBaseURL = %q, 期望去掉结尾斜杠", cfg.APIBaseURL)
	}
	if cfg.Addr != ":9000" {
		t.Errorf("Addr = %q, 期望 :9000", cfg.Addr)
	}
	if cfg.MaxResults != defaultMaxResults {
		t.Errorf("配置文件里的 max_results 应被忽略（已退役，只认环境变量），实际 MaxResults = %d", cfg.MaxResults)
	}
	// 退役字段必须给一声提示，否则用户会遇到"改了没反应"的哑谜。
	foundRetired := false
	for _, n := range notes {
		if strings.Contains(n, "max_results") && strings.Contains(n, "已不再生效") {
			foundRetired = true
		}
	}
	if !foundRetired {
		t.Errorf("配置里有退役字段时应给出提示，实际 notes = %v", notes)
	}
	// 文件中没有出现的字段应保留默认值（叠加语义，而非整体替换）。
	if cfg.TimeoutSec != defaultTimeoutSec {
		t.Errorf("TimeoutSec = %d, 期望保留默认值 %d", cfg.TimeoutSec, defaultTimeoutSec)
	}
	if len(notes) == 0 {
		t.Error("成功加载 config.json 时应产生提示")
	}
}

// TestResolveConfigFileOverridesEnvSeed 锁死新的优先级语义。
//
// 环境变量在这里是"首次启动的初值"，配置文件才是最终裁决者。
// 理由：配置文件由设置页面写入，用户在页面上显式改过的东西必须生效，
// 否则会出现"改完刷新又变回去了"这种最难排查的问题。
func TestResolveConfigFileOverridesEnvSeed(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, configFileName),
		[]byte(`{"api_key":"from-file","api_base_url":"http://file-host:1","max_results":7}`), 0o600)
	if err != nil {
		t.Fatalf("写入 config.json 失败: %v", err)
	}
	t.Chdir(dir)
	clearConfigEnv(t)

	// 这些环境变量只是"种子"，应当被文件覆盖。
	t.Setenv("AVDB_API_KEY", "from-env-seed")
	t.Setenv("AVDB_API_BASE_URL", "http://env-host:2")
	t.Setenv("AVDB_MAX_RESULTS", "999")

	cfg, _ := ResolveConfig()

	if cfg.APIKey != "from-file" {
		t.Errorf("APIKey = %q, 配置文件应优先于环境变量", cfg.APIKey)
	}
	if cfg.APIBaseURL != "http://file-host:1" {
		t.Errorf("APIBaseURL = %q, 配置文件应优先于环境变量", cfg.APIBaseURL)
	}
	// 例外：MaxResults 是部署参数，配置文件的 7 不再作数，环境变量的 999 说了算。
	if cfg.MaxResults != 999 {
		t.Errorf("MaxResults = %d, 期望 999（部署参数只认环境变量）", cfg.MaxResults)
	}
}

// TestResolveConfigDeploymentEnvWinsOverFile 是上面规则的例外：
// 监听地址与配置目录属于部署参数，改它们需要重启，
// 所以永远由环境变量说了算，配置文件里写了也不作数。
func TestResolveConfigDeploymentEnvWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, configFileName),
		[]byte(`{"listen_addr":":1111","api_key":"k"}`), 0o600)
	if err != nil {
		t.Fatalf("写入 config.json 失败: %v", err)
	}
	t.Chdir(dir)
	clearConfigEnv(t)

	t.Setenv("AVDB_ADDR", ":2222")
	cfg, _ := ResolveConfig()
	if cfg.Addr != ":2222" {
		t.Errorf("Addr = %q, 部署参数应始终由环境变量决定", cfg.Addr)
	}

	// 没有环境变量时，文件里的值作为启动默认值仍然有效。
	t.Setenv("AVDB_ADDR", "")
	cfg, _ = ResolveConfig()
	if cfg.Addr != ":1111" {
		t.Errorf("Addr = %q, 无环境变量时应采用文件里的启动默认值", cfg.Addr)
	}
}

func TestResolveConfigUsesCustomConfigDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configFileName),
		[]byte(`{"api_key":"in-dir","api_base_url":"http://dir-host:9"}`), 0o600); err != nil {
		t.Fatalf("写入 config.json 失败: %v", err)
	}
	// 故意切换到一个不含配置文件的目录，验证读取的是 AVDB_CONFIG_DIR 指定的位置。
	t.Chdir(t.TempDir())
	clearConfigEnv(t)
	t.Setenv("AVDB_CONFIG_DIR", dir)

	cfg, _ := ResolveConfig()

	if cfg.ConfigDir != dir {
		t.Errorf("ConfigDir = %q, 期望 %q", cfg.ConfigDir, dir)
	}
	if cfg.ConfigPath() != filepath.Join(dir, configFileName) {
		t.Errorf("ConfigPath = %q", cfg.ConfigPath())
	}
	if cfg.APIKey != "in-dir" {
		t.Errorf("APIKey = %q, 应从 AVDB_CONFIG_DIR 指向的目录加载", cfg.APIKey)
	}
}

func TestResolveConfigConfigDirAlwaysFromEnv(t *testing.T) {
	dir := t.TempDir()
	// 恶意/误写的配置文件试图把目录改到别处；必须被忽略。
	if err := os.WriteFile(filepath.Join(dir, configFileName),
		[]byte(`{"config_dir":"/etc","api_key":"k"}`), 0o600); err != nil {
		t.Fatalf("写入 config.json 失败: %v", err)
	}
	t.Chdir(t.TempDir())
	clearConfigEnv(t)
	t.Setenv("AVDB_CONFIG_DIR", dir)

	cfg, _ := ResolveConfig()
	if cfg.ConfigDir != dir {
		t.Errorf("ConfigDir 被配置文件篡改: %q", cfg.ConfigDir)
	}
}

func TestResolveConfigBrokenFileFallsBack(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("写入 config.json 失败: %v", err)
	}
	t.Chdir(dir)
	clearConfigEnv(t)

	cfg, notes := ResolveConfig()

	// 关键行为：配置文件损坏不能让服务起不来，必须带着默认值继续。
	if cfg.MaxResults != defaultMaxResults {
		t.Errorf("损坏配置后应回退默认值，实际 MaxResults = %d", cfg.MaxResults)
	}
	found := false
	for _, n := range notes {
		if strings.Contains(n, "解析失败") {
			found = true
		}
	}
	if !found {
		t.Errorf("配置损坏时应给出提示，实际 notes = %v", notes)
	}
}

func TestResolveConfigInvalidNumbersIgnored(t *testing.T) {
	t.Chdir(t.TempDir())
	clearConfigEnv(t)

	t.Setenv("AVDB_MAX_RESULTS", "abc")
	t.Setenv("AVDB_TIMEOUT_SECONDS", "-5")

	cfg, notes := ResolveConfig()

	if cfg.MaxResults != defaultMaxResults {
		t.Errorf("非法 MaxResults 应回退默认值，实际 %d", cfg.MaxResults)
	}
	if cfg.TimeoutSec != defaultTimeoutSec {
		t.Errorf("非法 TimeoutSec 应回退默认值，实际 %d", cfg.TimeoutSec)
	}
	if len(notes) < 2 {
		t.Errorf("两个非法值都应产生提示，实际 notes = %v", notes)
	}
}

func TestResolveConfigPortFallback(t *testing.T) {
	t.Chdir(t.TempDir())
	clearConfigEnv(t)

	t.Setenv("PORT", "3000")
	cfg, _ := ResolveConfig()
	if cfg.Addr != ":3000" {
		t.Errorf("裸端口 PORT 应补成 :3000，实际 %q", cfg.Addr)
	}

	// AVDB_ADDR 的优先级要高于 PORT。
	t.Setenv("AVDB_ADDR", "127.0.0.1:4000")
	cfg, _ = ResolveConfig()
	if cfg.Addr != "127.0.0.1:4000" {
		t.Errorf("AVDB_ADDR 应优先于 PORT，实际 %q", cfg.Addr)
	}
}

func TestNormalizeAddr(t *testing.T) {
	cases := map[string]string{
		"":            defaultAddr,
		"8080":        ":8080",
		":8080":       ":8080",
		"0.0.0.0:81":  "0.0.0.0:81",
		"127.0.0.1:1": "127.0.0.1:1",
	}
	for in, want := range cases {
		if got := normalizeAddr(in); got != want {
			t.Errorf("normalizeAddr(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		base    string
		wantErr bool
	}{
		{"正常的 http 地址", "http://192.168.1.20:8000", false},
		{"正常的 https 地址", "https://avdb.example.com", false},
		{"空地址", "", true},
		{"缺少协议", "192.168.1.20:8000", true},
		{"协议不支持", "ftp://host", true},
		{"缺少主机名", "http://", true},
		{"完全是垃圾", "://:://", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{APIBaseURL: tc.base}
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%q) 期望报错，实际通过", tc.base)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%q) 期望通过，实际报错: %v", tc.base, err)
			}
		})
	}
}

// ---------------------------------------------------------------- 持久化

func testConfig(dir string) Config {
	return normalizeConfig(Config{
		APIBaseURL:  "http://192.168.1.20:8000",
		APIKey:      "secret-token-value",
		Downloader:  "115",
		SavePath:    "/影视/AV视频",
		Addr:        ":8080",
		TimeoutSec:  25,
		MaxResults:  321,
		AccessToken: "page-token",
		ConfigDir:   dir,
	})
}

func TestConfigSaveThenReload(t *testing.T) {
	dir := t.TempDir()
	// 切到一个空目录，确保后续只可能从 dir 里读到配置。
	t.Chdir(t.TempDir())
	clearConfigEnv(t)
	t.Setenv("AVDB_CONFIG_DIR", dir)

	want := testConfig(dir)
	if err := want.Save(); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}

	// 落盘文件里不能再出现 max_results——老版本正是把 5000 写进了这里，
	// 让"不限制条数"形同虚设。
	if raw, err := os.ReadFile(want.ConfigPath()); err != nil {
		t.Fatalf("读取配置文件失败: %v", err)
	} else if bytes.Contains(raw, []byte("max_results")) {
		t.Errorf("配置文件仍写入了 max_results：\n%s", raw)
	}

	got, _ := ResolveConfig()

	// 核心断言：写进去什么，重启后就读回什么。
	if got.APIBaseURL != want.APIBaseURL {
		t.Errorf("APIBaseURL = %q, 期望 %q", got.APIBaseURL, want.APIBaseURL)
	}
	if got.APIKey != want.APIKey {
		t.Errorf("APIKey = %q, 期望 %q", got.APIKey, want.APIKey)
	}
	if got.SavePath != want.SavePath {
		t.Errorf("SavePath = %q, 期望 %q", got.SavePath, want.SavePath)
	}
	if got.Downloader != want.Downloader {
		t.Errorf("Downloader = %q, 期望 %q", got.Downloader, want.Downloader)
	}
	if got.TimeoutSec != want.TimeoutSec {
		t.Errorf("TimeoutSec = %d, 期望 %d", got.TimeoutSec, want.TimeoutSec)
	}
	// 这里刻意给一个非零值：它是部署参数，**不该**被写进文件，
	// 重载后必须回到默认值（不限制）。若哪天有人把它改回可落盘，
	// 老配置里那个 5000 就会复活，本条断言会立刻拦住。
	if got.MaxResults != defaultMaxResults {
		t.Errorf("MaxResults = %d, 期望 %d（部署参数不应落盘）", got.MaxResults, defaultMaxResults)
	}
	if got.AccessToken != want.AccessToken {
		t.Errorf("AccessToken = %q, 期望 %q", got.AccessToken, want.AccessToken)
	}
	// 非 ASCII 的保存路径必须原样往返，不能被编码弄坏。
	if !strings.Contains(got.SavePath, "影视") {
		t.Errorf("中文保存路径被破坏: %q", got.SavePath)
	}
}

func TestConfigSaveCreatesDirAndWritesFile(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "data")

	cfg := testConfig(dir)
	if err := cfg.Save(); err != nil {
		t.Fatalf("保存到不存在的目录应自动创建，实际失败: %v", err)
	}

	info, err := os.Stat(cfg.ConfigPath())
	if err != nil {
		t.Fatalf("配置文件未生成: %v", err)
	}
	if info.IsDir() {
		t.Fatal("配置路径指向了目录")
	}
	if info.Size() == 0 {
		t.Error("配置文件为空")
	}
}

func TestConfigSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)

	for i := 0; i < 5; i++ {
		if err := cfg.Save(); err != nil {
			t.Fatalf("第 %d 次保存失败: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	// 原子写用的临时文件必须被清理干净，否则持久化目录会越攒越乱。
	for _, e := range entries {
		if e.Name() != configFileName {
			t.Errorf("目录里残留了非预期文件: %q", e.Name())
		}
	}
}

func TestConfigSaveDoesNotPersistDeploymentFields(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	if err := cfg.Save(); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	data, err := os.ReadFile(cfg.ConfigPath())
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "api_key") {
		t.Error("配置文件应包含 api_key")
	}
	// ConfigDir 的 json tag 是 "-"，属于部署参数，不该写进文件。
	if strings.Contains(content, "config_dir") {
		t.Errorf("配置文件不应包含 config_dir：%s", content)
	}

	// 内容必须是合法 JSON，且能解析回结构体。
	var back Config
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("写出的不是合法 JSON: %v", err)
	}
	if back.APIKey != cfg.APIKey {
		t.Errorf("往返后 APIKey 不一致: %q", back.APIKey)
	}
}

func TestConfigSaveFailsWhenDirCannotBeCreated(t *testing.T) {
	// 用一个"已存在的普通文件"作为父目录，MkdirAll 在任何平台都会失败。
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备阻塞文件失败: %v", err)
	}

	cfg := testConfig(filepath.Join(blocker, "data"))
	err := cfg.Save()
	if err == nil {
		t.Fatal("目录无法创建时保存应当报错")
	}
	// 错误信息要能指导用户（尤其是容器里没挂卷的场景）。
	if !strings.Contains(err.Error(), "挂载") && !strings.Contains(err.Error(), "配置目录") {
		t.Errorf("错误信息缺少可操作提示: %v", err)
	}
}

func TestConfigDirWritable(t *testing.T) {
	if !testConfig(t.TempDir()).ConfigDirWritable() {
		t.Error("临时目录应当可写")
	}

	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备阻塞文件失败: %v", err)
	}
	if testConfig(filepath.Join(blocker, "data")).ConfigDirWritable() {
		t.Error("不可创建的目录不应被判为可写")
	}
}

func TestMaskSecret(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空串", "", ""},
		{"纯空白", "   ", ""},
		{"1 位", "a", "•"},
		{"4 位", "abcd", "••••"},
		{"6 位", "abcdef", "ab••••••"},
		{"8 位", "abcdefgh", "ab••••••"},
		{"16 位", "AAAABBBBCCCCDDDD", "AAAA••••••••DDDD"},
		{"32 位", "AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH", "AAAA••••••••HHHH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaskSecret(tc.in); got != tc.want {
				t.Errorf("MaskSecret(%q) = %q, 期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMaskSecretNeverLeaksMiddle 校验真正重要的性质：
// 对足够长的密钥，中段必须被抹掉，只保留首尾各 4 位。
func TestMaskSecretNeverLeaksMiddle(t *testing.T) {
	const token = "AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH"
	masked := MaskSecret(token)

	if !strings.HasPrefix(masked, "AAAA") || !strings.HasSuffix(masked, "HHHH") {
		t.Errorf("掩码应保留首尾各 4 位: %q", masked)
	}
	// 中段（第 5~28 位）一个字符都不能出现。
	if strings.Contains(masked, token[4:len(token)-4]) {
		t.Errorf("掩码泄露了密钥中段: %q", masked)
	}
	for _, chunk := range []string{"BBBB", "CCCC", "DDDD", "EEEE", "FFFF", "GGGG"} {
		if strings.Contains(masked, chunk) {
			t.Errorf("掩码泄露了中段片段 %q: %q", chunk, masked)
		}
	}
}

func TestConfigWarnings(t *testing.T) {
	cfg := Config{}
	warnings := cfg.Warnings()
	if len(warnings) != 2 {
		t.Fatalf("空配置应产生 2 条提醒，实际 %d 条: %v", len(warnings), warnings)
	}

	full := Config{APIKey: "k", AccessToken: "t"}
	if got := full.Warnings(); len(got) != 0 {
		t.Errorf("配置完整时不应有提醒，实际: %v", got)
	}
}
