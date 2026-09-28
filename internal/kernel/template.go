package kernel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fan-weibo/ArkPerf/internal/execx"
)

// 测量服务的位置与解释器**按克隆位置动态解析，不写死绝对路径**。
//
// 为什么不能写死：mcp-servers 就在本仓库根目录里，而每个人把它克隆到哪里、
// python 装在哪都不同。写死成作者机器上的 `E:\ArkPerf\mcp-servers` 与
// `C:\Users\xxx\...\python.exe`，队友拉下来会一个服务都连不上——
// 而现象是"Agent 好像什么都不会"，比报错更难查。
//
// 解析顺序（每个候选都要求真实存在，先命中先用）：
//
//	MCP 目录：ARKPERF_MCP_DIR
//	          → 可执行文件所在目录及其上两级下的 mcp-servers（覆盖仓库根 / desktop/bin 两种启动位置）
//	          → 当前工作目录下的 mcp-servers
//	Python  ：ARKPERF_PYTHON
//	          → 仓库根或 mcp-servers 下的 .venv（README 让队友建的虚拟环境就是它）
//	          → PATH 上的 python / python3 / py
//
// 为什么 python 优先找 venv：`mcp` 包通常只装在虚拟环境里，
// 系统 PATH 上的 python 往往没有它——连不上时的报错是 ImportError，
// 很容易被误读成"服务写错了"。
//
// 想覆盖就设环境变量。`arkperf init` 会把解析结果打印出来：连不上先看那两行。
const templateMCPDirName = "mcp-servers"

// ResolveMCPDir 解析 mcp-servers 目录。
//
// 都找不到时返回当前目录下的 mcp-servers 作为最合理的猜测：
// 让下游的报错（"连接失败 / 文件不存在"）来说明问题，而不是在这里 panic——
// 这个函数会被 `arkperf init` 与配置模板调用，崩了就没法生成配置。
func ResolveMCPDir() string {
	// 显式指定就照用，不做存在性检查：用户说了算，指错了应由
	// `arkperf mcp` 的报错来说，而不是悄悄换到别的目录（那更难看懂）。
	if v := os.Getenv("ARKPERF_MCP_DIR"); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
		return v
	}
	var cands []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		// 往上找三级：仓库根的 arkperf.exe、desktop/bin 的桌面端、再深一层也兜住
		for range 3 {
			cands = append(cands, filepath.Join(dir, templateMCPDirName))
			dir = filepath.Dir(dir)
		}
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(wd, templateMCPDirName))
	}
	for _, c := range cands {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			if abs, err := filepath.Abs(c); err == nil {
				return abs
			}
			return c
		}
	}
	wd, _ := os.Getwd()
	return filepath.Join(wd, templateMCPDirName)
}

// ResolvePython 解析跑 MCP 服务要用的 python 解释器路径。
func ResolvePython(mcpDir string) string {
	if v := os.Getenv("ARKPERF_PYTHON"); v != "" {
		return v
	}
	// 仓库根与 mcp-servers 下的 .venv，Windows 与类 Unix 两种布局都试
	bases := []string{filepath.Dir(mcpDir), mcpDir}
	for _, base := range bases {
		for _, sub := range []string{"Scripts", "bin"} {
			for _, name := range []string{"python.exe", "python"} {
				cand := filepath.Join(base, ".venv", sub, name)
				if info, err := os.Stat(cand); err == nil && !info.IsDir() {
					return cand
				}
			}
		}
	}
	for _, name := range []string{"python", "python3", "py"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	// 交给连接时的报错去说明（不去 panic，理由同 ResolveMCPDir）
	return "python"
}

// TemplatePaths 是模板解析出来的两条路径，供调用方打印 / 自检。
type TemplatePaths struct {
	MCPDir string
	Python string
}

// ResolveTemplatePaths 一次解析出 MCP 目录与 python。
func ResolveTemplatePaths() TemplatePaths {
	dir := ResolveMCPDir()
	return TemplatePaths{MCPDir: dir, Python: ResolvePython(dir)}
}

// TemplateConfig 是 `arkperf init` 写出的完整模板：默认值 + 本项目的 5 个测量服务器。
//
// 5 个服务器全部标记 trusted：它们是本机脚本、内容已知，逐次点击确认只会
// 让人麻木地乱点（"审批疲劳"本身就是安全风险）。真正不确定的第三方服务器
// 不要这样标。
//
// 路径来自 ResolveTemplatePaths（按克隆位置解析），所以队友拉下来直接 init 就能用。
// `arkperf mcp` 会逐个显示连接结果——某一个连不上不影响其他几个。
func TemplateConfig() *Config {
	return TemplateConfigFor(ResolveTemplatePaths())
}

// TemplateConfigFor 用指定的路径生成模板（`arkperf init` 用它把解析结果同时打印出来，
// 避免"打印的路径"和"写进配置的路径"两次解析出现分歧）。
func TemplateConfigFor(p TemplatePaths) *Config {
	cfg := DefaultConfig()

	server := func(script string) MCPServerConfig {
		return MCPServerConfig{
			Command: p.Python,
			Args:    []string{"-u", filepath.Join(p.MCPDir, script)},
			Trusted: true,
			// 显式写出超时而不是靠内置默认：这个值是最容易被低估的一项，
			// 写在配置里能让"工具为什么被掐断"一眼可查。
			TimeoutSeconds: int(TemplateMCPTimeout / time.Second),
		}
	}

	cfg.MCPServers = map[string]MCPServerConfig{
		"experience": server("agent_server.py"),      // 一键全量分析 + 报告落盘
		"cold-start": server("cold_start_server.py"), // 冷启动测量 / 基线对比
		"memory":     server("memory_server.py"),     // 内存快照 / 泄漏判定
		"cpu":        server("cpu_server.py"),        // CPU 占用 / 趋势 / 评估
		"jank":       server("jank_server.py"),       // 主线程卡顿 / 阻塞
	}
	return cfg
}

// TemplateMCPTimeout 是模板里显式写出的调用超时。
//
// 单独提出来是因为它容易被低估：全量体验分析要驱动模拟器跑完整采集，
// 实测约 4 分钟，标准模式约 6 分钟。默认值给 360s，宁可等待也不要
// 在服务端还在工作时把连接掐掉——那会浪费掉一整次测量。
const TemplateMCPTimeout = 360 * time.Second

// CheckTemplatePaths 检查解析出来的路径是否真的可用，返回人话描述的问题列表。
// `arkperf init` 用它给出"连不上之前就能看到"的提示。
//
// 注意：这一步会**起一次 python 子进程**去试 `import mcp`（约百毫秒）。
// 值得这么做——没有 mcp 包时报错是 ImportError，极容易被误读成"服务脚本写错了"，
// 队友会在错的方向上查半天。
func CheckTemplatePaths(p TemplatePaths) []string {
	var problems []string

	if info, err := os.Stat(p.MCPDir); err != nil || !info.IsDir() {
		problems = append(problems, fmt.Sprintf("MCP 目录不存在：%s（可用环境变量 ARKPERF_MCP_DIR 指定）", p.MCPDir))
	} else {
		for _, script := range []string{"agent_server.py", "cold_start_server.py", "memory_server.py", "cpu_server.py", "jank_server.py"} {
			if _, err := os.Stat(filepath.Join(p.MCPDir, script)); err != nil {
				problems = append(problems, fmt.Sprintf("缺少服务脚本：%s", filepath.Join(p.MCPDir, script)))
			}
		}
	}

	// python 本身能不能跑，先单独判：否则"找不到 python"会被误报成"没装 mcp 包"
	pyPath, err := resolveForCheck(p.Python)
	if err != nil {
		problems = append(problems, fmt.Sprintf("Python 解释器不可用：%s（可用环境变量 ARKPERF_PYTHON 指定）", p.Python))
		return problems
	}
	if err := checkMCPSDK(pyPath); err != nil {
		problems = append(problems, fmt.Sprintf("这个 Python 里没有 mcp 包（%s）——%v", pyPath, err))
	}
	return problems
}

// resolveForCheck 把配置里的 python 变成"确实存在可执行文件"的路径。
// 绝对路径直接 stat；相对名字（python / py）走 PATH。
func resolveForCheck(python string) (string, error) {
	if filepath.IsAbs(python) {
		info, err := os.Stat(python)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			return "", fmt.Errorf("%s 是目录", python)
		}
		return python, nil
	}
	return exec.LookPath(python)
}

// checkMCPSDK 试跑 `python -c "import mcp"`。
func checkMCPSDK(python string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, python, "-c", "import mcp")
	// 无控制台环境（桌面版 / 从 GUI 启动）下不加这个会闪黑框
	execx.HideWindow(cmd)

	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	return errors.New(msg)
}
