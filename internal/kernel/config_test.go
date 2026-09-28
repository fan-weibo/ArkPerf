package kernel

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestLoadEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	write := `{"provider":{"baseUrl":"https://file.example/v1","apiKey":"sk-file","model":"m-file"}}`
	if err := os.WriteFile(path, []byte(write), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARKPERF_MODEL", "m-env")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.Model != "m-env" {
		t.Fatalf("env must win: got %q", cfg.Provider.Model)
	}
	if cfg.Provider.BaseURL != "https://file.example/v1" {
		t.Fatalf("file value must survive: %q", cfg.Provider.BaseURL)
	}
	if cfg.MaxTurns != 200 {
		t.Fatalf("default MaxTurns: %d", cfg.MaxTurns)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file must not error (env-only bootstrap): %v", err)
	}
	if cfg.Approval != "ask" || cfg.Locale != "zh" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestValidateRequiresAllProviderFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"no baseUrl", Config{}, "baseUrl"},
		{"no apiKey", Config{Provider: ProviderConfig{BaseURL: "https://x/v1"}}, "apiKey"},
		{"no model", Config{Provider: ProviderConfig{BaseURL: "https://x/v1", APIKey: "sk"}}, "model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error should name the missing field: %v", err)
			}
		})
	}

	ok := Config{Provider: ProviderConfig{BaseURL: "https://x/v1", APIKey: "sk", Model: "m"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("complete config must pass: %v", err)
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"":                "(unset)",
		"sk-abcdefgh1234": "sk-a...1234",
		"short":           "****",
		"exactly8":        "****",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Fatalf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

// 密钥绝不能出现在可打印的配置摘要里。
func TestStringRedactsKey(t *testing.T) {
	cfg := &Config{
		Provider: ProviderConfig{BaseURL: "https://x/v1", APIKey: "sk-secret-12345678", Model: "m"},
		Locale:   "zh",
	}
	out := cfg.String()
	if strings.Contains(out, "sk-secret-12345678") {
		t.Fatalf("raw key leaked into output:\n%s", out)
	}
	if !strings.Contains(out, "sk-s...5678") {
		t.Fatalf("expected redacted key in output:\n%s", out)
	}
	// 未接线的字段必须显式标注，否则用户改了不生效还不知道为什么
	if !strings.Contains(out, "未接线") {
		t.Fatalf("unwired fields must be labelled:\n%s", out)
	}
}

func TestUnknownFields(t *testing.T) {
	cases := []struct {
		name string
		json string
		want []string
	}{
		{"clean", `{"provider":{"baseUrl":"x","apiKey":"y","model":"z"},"maxTurns":5}`, nil},
		{"typo at top level", `{"maxTurn":5}`, []string{"maxTurn"}},
		{"typo nested", `{"provider":{"baseURL":"x"}}`, []string{"provider.baseURL"}},
		{"both", `{"locale":"zh","provider":{"api_key":"x"}}`, []string{"provider.api_key"}},
		{"not an object", `not json`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UnknownFields([]byte(tc.json))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// 配置里迟早会有密钥，别让它落到世界可读。
//
// Windows 忽略 os.WriteFile 的 mode 参数（访问控制走 ACL），所以这条断言
// 只在类 Unix 上有意义；Windows 上状态根位于用户配置目录，已由 ACL 限定到当前用户。
func TestSaveDefaultUsesRestrictivePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不使用 Unix 权限位，访问控制由 ACL 承担")
	}
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	if err := SaveDefault(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions: %v", perm)
	}
}

func TestUnknownFieldsFindsMCPTypos(t *testing.T) {
	// command 拼成 comand、timeoutSeconds 拼成 timeoutSec —— 这类错误会让
	// 服务器"静默不生效"，是最需要被抓住的一种
	data := []byte(`{"mcpServers":{"cpu":{"comand":"python.exe","timeoutSec":360}}}`)
	got := UnknownFields(data)
	want := []string{"mcpServers.cpu.comand", "mcpServers.cpu.timeoutSec"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	clean := []byte(`{"mcpServers":{"cpu":{"command":"python.exe","args":["-u","x.py"],"trusted":true,"timeoutSeconds":360}}}`)
	if got := UnknownFields(clean); len(got) != 0 {
		t.Fatalf("clean config flagged as unknown: %v", got)
	}
}

// Enabled 是三态：未写 = 启用，显式 false = 停用。
func TestMCPServerEnabledIsTriState(t *testing.T) {
	no, yes := false, true
	cases := []struct {
		name string
		srv  MCPServerConfig
		want bool
	}{
		{"unset means enabled", MCPServerConfig{}, true},
		{"explicit true", MCPServerConfig{Enabled: &yes}, true},
		{"explicit false", MCPServerConfig{Enabled: &no}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.srv.IsEnabled(); got != tc.want {
				t.Fatalf("IsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// 停用的服务器不该因为 command 为空而被判为无效配置——
// 用户停用它时本来就会把 command 留空或删掉。
func TestValidateSkipsDisabledServers(t *testing.T) {
	no := false
	base := func() *Config {
		cfg := DefaultConfig()
		cfg.Provider = ProviderConfig{BaseURL: "https://x/v1", APIKey: "sk", Model: "m"}
		return cfg
	}

	ok := base()
	ok.MCPServers = map[string]MCPServerConfig{"off": {Enabled: &no}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("disabled server must not fail validation: %v", err)
	}

	bad := base()
	bad.MCPServers = map[string]MCPServerConfig{"cpu": {Command: "   "}}
	if err := bad.Validate(); err == nil {
		t.Fatal("enabled server without command must fail validation")
	}
}

func TestTemplateConfigIsUsable(t *testing.T) {
	cfg := TemplateConfig()
	if len(cfg.MCPServers) != 5 {
		t.Fatalf("template should ship the 5 measurement servers, got %d", len(cfg.MCPServers))
	}
	for name, srv := range cfg.MCPServers {
		if srv.Command == "" {
			t.Fatalf("%s: template server has no command", name)
		}
		if len(srv.Args) < 2 || srv.Args[0] != "-u" {
			// -u 关掉 Python 的 stdout 缓冲，stdio JSON-RPC 靠它才不会卡死
			t.Fatalf("%s: args must start with -u, got %v", name, srv.Args)
		}
		if !srv.Trusted {
			t.Fatalf("%s: 本机脚本按模板应免审批", name)
		}
		if srv.TimeoutSeconds < 300 {
			// 全量体验分析约 4 分钟、标准模式约 6 分钟
			t.Fatalf("%s: timeout too small for measurement tools: %ds", name, srv.TimeoutSeconds)
		}
	}

	cfg.Provider = ProviderConfig{BaseURL: "https://x/v1", APIKey: "sk", Model: "m"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("template must validate: %v", err)
	}
}

// 模板里的两条路径必须"跟着仓库走"，不能写死作者机器上的绝对路径：
// 写死会让队友拉下来一个 MCP 服务都连不上，而现象是"Agent 好像什么都不会"，
// 比一条明确的报错难查得多。
func TestTemplatePathsAreLocationIndependent(t *testing.T) {
	paths := ResolveTemplatePaths()

	if !filepath.IsAbs(paths.MCPDir) {
		t.Fatalf("MCP 目录应是绝对路径，得到 %q", paths.MCPDir)
	}
	if filepath.Base(paths.MCPDir) != templateMCPDirName {
		t.Fatalf("MCP 目录应以 %q 结尾，得到 %q", templateMCPDirName, paths.MCPDir)
	}
	if paths.Python == "" {
		t.Fatal("python 不应为空")
	}

	// 写进配置的脚本路径必须落在解析出的 MCP 目录下（不能是别的机器上的位置）
	cfg := TemplateConfigFor(paths)
	for name, srv := range cfg.MCPServers {
		if len(srv.Args) < 2 {
			t.Fatalf("%s: args 结构不对：%v", name, srv.Args)
		}
		if got := filepath.Dir(srv.Args[1]); got != paths.MCPDir {
			t.Fatalf("%s: 脚本目录 %q 不等于解析出的 MCP 目录 %q", name, got, paths.MCPDir)
		}
		if srv.Command != paths.Python {
			t.Fatalf("%s: command 应等于解析出的 python %q，得到 %q", name, paths.Python, srv.Command)
		}
	}
}

// 环境变量优先于一切推断：换机器、换虚拟环境、目录被挪走时的逃生口。
func TestTemplatePathsEnvOverride(t *testing.T) {
	custom := t.TempDir()
	t.Setenv("ARKPERF_MCP_DIR", custom)
	if got := ResolveMCPDir(); got != custom {
		t.Fatalf("ARKPERF_MCP_DIR 应优先，得到 %q", got)
	}

	py := filepath.Join(t.TempDir(), "python.exe")
	t.Setenv("ARKPERF_PYTHON", py)
	if got := ResolvePython(custom); got != py {
		t.Fatalf("ARKPERF_PYTHON 应优先，得到 %q", got)
	}
}

func TestLoadParsesMCPServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{
	  "provider": {"baseUrl":"https://x/v1","apiKey":"sk-a","model":"m"},
	  "mcpServers": {
	    "cpu": {"command":"python.exe","args":["-u","cpu_server.py"],"trusted":true},
	    "jank": {"command":"python.exe","enabled":false}
	  }
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 2 {
		t.Fatalf("servers: %d", len(cfg.MCPServers))
	}
	if !cfg.MCPServers["cpu"].Trusted {
		t.Fatal("trusted flag lost")
	}
	if cfg.MCPServers["jank"].IsEnabled() {
		t.Fatal("enabled=false lost")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
