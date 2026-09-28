// Package kernel 是 ArkPerf 的内核：配置、模型客户端与最小执行闭环。
package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ProviderConfig 描述一个 OpenAI 兼容端点。
type ProviderConfig struct {
	BaseURL string `json:"baseUrl"` // 形如 https://api.example.com/v1
	APIKey  string `json:"apiKey"`
	Model   string `json:"model"`
}

// MCPServerConfig 描述一个以子进程方式启动的 stdio MCP 服务器。
//
// 测量能力全部来自 MCP，所以这里是 ArkPerf 最重要的一段配置。
type MCPServerConfig struct {
	// Command 是解释器或可执行文件的绝对路径。
	// 不要依赖 PATH：同一个 `python` 在不同环境里可能装着不同的包集，
	// 而"python 找到了但 import mcp 失败"是最难查的一类故障。
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// Trusted 为 true 时该服务器的工具免审批。
	//
	// 缺省必须是 false：远端能力无法静态审计，而且服务端自己声明的
	// readOnlyHint 也不能作为放行依据——那是被审计方给自己出的担保。
	Trusted bool `json:"trusted,omitzero"`
	// Enabled 用指针表示三态：未写 = 启用，显式 false = 停用。
	Enabled *bool `json:"enabled,omitempty"`
	// TimeoutSeconds 覆盖单次工具调用超时（秒）。零值用内置默认。
	TimeoutSeconds int `json:"timeoutSeconds,omitzero"`
}

// IsEnabled 缺省为启用。
func (c MCPServerConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// Config 是 ~/.arkperf/config.json 的形状。
type Config struct {
	Provider ProviderConfig `json:"provider"`
	MaxTurns int            `json:"maxTurns"`
	Approval string         `json:"approval"` // ask | auto
	// Locale 目前未接线（i18n 未实现）。
	Locale string `json:"locale"`
	// Workspace 是默认工作目录（要分析的 OpenHarmony 工程根或其上级）。
	//
	// 桌面版尤其需要：双击 exe 启动时进程 CWD 是 exe 所在目录，
	// 不指定的话"最近会话""工作区面板"就全对不上号（实测踩过）。
	// 空值时回退：最近一次会话的目录 → 进程 CWD。
	Workspace string `json:"workspace,omitempty"`
	// MCPServers 是服务器名到定义的映射。名字会出现在工具名前缀里，宜短。
	MCPServers map[string]MCPServerConfig `json:"mcpServers,omitempty"`
}

// Home 返回状态根目录：ARKPERF_HOME 优先，其次 ~/.arkperf。
func Home() string {
	if v := os.Getenv("ARKPERF_HOME"); v != "" {
		return v
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".arkperf"
	}
	return filepath.Join(h, ".arkperf")
}

// ConfigPath 返回配置文件路径。
func ConfigPath() string { return filepath.Join(Home(), "config.json") }

// DefaultConfig 返回带默认值的配置：不含 MCP 服务器。
//
// 刻意不预置服务器——默认值会在用户没写 mcpServers 时被继承，
// 让一份空配置悄悄拉起一堆子进程是很糟的默认行为。
func DefaultConfig() *Config {
	return &Config{MaxTurns: 200, Approval: "ask", Locale: "zh"}
}

// Load 读取配置。文件不存在时返回默认配置而非报错，随后应用环境变量覆盖。
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// 首次运行：允许只靠环境变量启动
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// 环境变量优先级高于文件
	if v := os.Getenv("ARKPERF_BASE_URL"); v != "" {
		cfg.Provider.BaseURL = v
	}
	if v := os.Getenv("ARKPERF_API_KEY"); v != "" {
		cfg.Provider.APIKey = v
	}
	if v := os.Getenv("ARKPERF_MODEL"); v != "" {
		cfg.Provider.Model = v
	}
	return cfg, nil
}

// Validate 校验必填项，缺失即明确报错（不要带着空值去发请求）。
func (c *Config) Validate() error {
	if c.Provider.BaseURL == "" {
		return errors.New("provider.baseUrl is empty (edit ~/.arkperf/config.json or set ARKPERF_BASE_URL)")
	}
	if c.Provider.APIKey == "" {
		return errors.New("provider.apiKey is empty (edit ~/.arkperf/config.json or set ARKPERF_API_KEY)")
	}
	if c.Provider.Model == "" {
		return errors.New("provider.model is empty (edit ~/.arkperf/config.json or set ARKPERF_MODEL)")
	}
	for name, srv := range c.MCPServers {
		if srv.IsEnabled() && strings.TrimSpace(srv.Command) == "" {
			return fmt.Errorf("mcpServers.%s.command is empty", name)
		}
	}
	return nil
}

// Redact 脱敏密钥，只保留头尾各 4 位。
func Redact(key string) string {
	if key == "" {
		return "(unset)"
	}
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

// 已知字段表，用于抓住拼错的键名。
var (
	knownTopFields      = fieldSet("provider", "maxTurns", "approval", "locale", "mcpServers")
	knownProviderFields = fieldSet("baseUrl", "apiKey", "model")
	knownMCPSrvFields   = fieldSet("command", "args", "env", "trusted", "enabled", "timeoutSeconds")
)

func fieldSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// UnknownFields 收集配置文件里拼错的字段名。
//
// Go 的 json.Unmarshal 默认静默忽略未知字段：把 baseUrl 写成 baseURL，
// 程序不报错、直接拿空 URL 去请求，报错却是 404——这类"拼写错误伪装成
// 其他故障"的坑，只能用显式比对来堵。
func UnknownFields(data []byte) []string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil
	}

	var out []string
	for k := range top {
		if !knownTopFields[k] {
			out = append(out, k)
		}
	}
	if raw, ok := top["provider"]; ok {
		out = append(out, unknownIn(raw, knownProviderFields, "provider.")...)
	}
	if raw, ok := top["mcpServers"]; ok {
		var servers map[string]json.RawMessage
		if json.Unmarshal(raw, &servers) == nil {
			for name, srv := range servers {
				out = append(out, unknownIn(srv, knownMCPSrvFields, "mcpServers."+name+".")...)
			}
		}
	}
	slices.Sort(out)
	return out
}

func unknownIn(raw json.RawMessage, known map[string]bool, prefix string) []string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	var out []string
	for k := range obj {
		if !known[k] {
			out = append(out, prefix+k)
		}
	}
	return out
}

// SaveDefault 写入配置模板（路径按当前环境解析）。
func SaveDefault(path string) error {
	return SaveDefaultFor(path, ResolveTemplatePaths())
}

// SaveDefaultFor 用指定的 MCP 目录 / python 写入配置模板。
//
// 单独留一个入口是给 `arkperf init` 用的：它先把解析结果打印给用户看，
// 再把同一份结果写进配置——两次解析可能不一致（比如中途被环境变量影响），
// 打印的和写下去的必须是同一份。
func SaveDefaultFor(path string, p TemplatePaths) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(TemplateConfigFor(p), "", "  ")
	if err != nil {
		return err
	}
	// Windows 忽略 mode（访问控制走 ACL）；保留 0600 是为了类 Unix 正确。
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// String 返回可安全打印的配置摘要。
//
// 未接线的字段显式标注：配置项存在但不起作用，比没有这个配置项更坑，
// 必须让人一眼看见。
func (c *Config) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "config   : %s\n", ConfigPath())
	fmt.Fprintf(&sb, "baseUrl  : %s\n", c.Provider.BaseURL)
	fmt.Fprintf(&sb, "apiKey   : %s\n", Redact(c.Provider.APIKey))
	fmt.Fprintf(&sb, "model    : %s\n", c.Provider.Model)
	fmt.Fprintf(&sb, "maxTurns : %d\n", c.MaxTurns)
	fmt.Fprintf(&sb, "approval : %s\n", c.Approval)
	fmt.Fprintf(&sb, "locale   : %s   ← 未接线\n", c.Locale)

	if len(c.MCPServers) == 0 {
		sb.WriteString("mcp      : (未配置)\n")
		return sb.String()
	}
	fmt.Fprintf(&sb, "mcp      : %d 个服务器\n", len(c.MCPServers))
	for _, name := range slices.Sorted(maps.Keys(c.MCPServers)) {
		srv := c.MCPServers[name]
		state, trust := "启用", "需审批"
		if !srv.IsEnabled() {
			state = "停用"
		}
		if srv.Trusted {
			trust = "免审批"
		}
		fmt.Fprintf(&sb, "  - %-20s %s · %s\n", name, state, trust)
	}
	return sb.String()
}
