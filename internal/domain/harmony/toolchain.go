// Package harmony 封装 OpenHarmony 工具链与设备的实际调用。
//
// 这一层不做决策、不调模型：只负责"把命令行拼对、把输出读回来、把失败如实说出来"。
// 所有判断（要不要构建、要不要装机）都在上层。
package harmony

import (
	"cmp"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/fan-weibo/ArkPerf/internal/execx"
)

// 工具名（同时是配置与展示用的键）。
const (
	ToolHDC     = "hdc"
	ToolHvigorw = "hvigorw"
	ToolOhpm    = "ohpm"
	ToolJava    = "java"
	ToolNode    = "node"
)

// 环境变量：显式指定 DevEco 安装根目录。
const EnvDevEcoRoot = "ARKPERF_DEVECO_HOME"

// EnvDevEcoSDKHome 是 hvigor 定位 SDK 用的环境变量。
//
// 这是实测踩出来的：命令行直接跑 hvigorw 会以
// "Invalid value of 'DEVECO_SDK_HOME'" 失败——DevEco 的内置终端会设这个变量，
// 裸 shell 不会。不补这一项，命令行构建在每台机器上都必失败。
const EnvDevEcoSDKHome = "DEVECO_SDK_HOME"

// Tool 是一个工具的探测结果。
type Tool struct {
	Name string
	// Path 为空表示没找到。
	Path string
	// Source 说明是怎么找到的，未找到时为空。
	Source string
	// Version 为"（未知）"表示没探测到（不是错误）。
	Version string
	// Searched 列出尝试过但没命中的候选位置；未找到时用于如实报告"我找过哪里"。
	Searched []string
	// Hint 是未找到时给出的下一步建议。
	Hint string
}

// Found 表示工具可用。
func (t Tool) Found() bool { return t.Path != "" }

// Describe 生成一行人类可读的状态。
func (t Tool) Describe() string {
	if !t.Found() {
		return t.Name + ": 未找到"
	}
	s := t.Name + ": " + t.Path
	if t.Source != "" {
		s += "（" + t.Source + "）"
	}
	if t.Version != "" {
		s += " · " + t.Version
	}
	return s
}

// Toolchain 是本机 OpenHarmony 工具链的探测结果。
type Toolchain struct {
	// DevEcoRoot 是 DevEco Studio 安装根目录，空表示没定位到。
	DevEcoRoot string
	// DevEcoSource 说明根目录是怎么确定的。
	DevEcoSource string
	// SDKDir 是 SDK 根目录（<DevEcoRoot>/sdk），供命令行构建注入 DEVECO_SDK_HOME。
	SDKDir string
	// EmulatorDir 是模拟器工具目录，空表示不存在。
	EmulatorDir string
	// Tools 按固定顺序排列（hdc / hvigorw / ohpm / node / java）。
	Tools []Tool
}

// Get 按名取工具。
func (tc *Toolchain) Get(name string) (Tool, bool) {
	for _, t := range tc.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Required 返回必填工具；Get 不到返回零值 Tool（Found() 为 false）。
func (tc *Toolchain) Required(name string) Tool {
	t, _ := tc.Get(name)
	return t
}

// Counts 返回可用工具数、总数与缺失的工具名。
//
// Summary 是"给人读的整句"，但界面上的状态点需要能直接判断"齐不齐"——
// 让前端去解析那句话里的 "5/5" 太脆（措辞一改就断），所以把数字单独给出。
func (tc *Toolchain) Counts() (found, total int, missing []string) {
	total = len(tc.Tools)
	for _, t := range tc.Tools {
		if t.Found() {
			found++
			continue
		}
		missing = append(missing, t.Name)
	}
	return found, total, missing
}

// Summary 生成一行摘要，用于构建提示词与状态展示。
func (tc *Toolchain) Summary() string {
	found, total, missing := tc.Counts()
	s := strconv.Itoa(found) + "/" + strconv.Itoa(total) + " 个工具可用"
	if len(missing) > 0 {
		s += "，缺：" + strings.Join(missing, ", ")
	}
	if tc.DevEcoRoot != "" {
		s += " · DevEco=" + tc.DevEcoRoot
	}
	return s
}

// DiscoverOptions 允许注入探测手段，使探测逻辑可以离线测试。
type DiscoverOptions struct {
	// DevEcoRoot 显式指定根目录，优先级最高。
	DevEcoRoot string
	// LookPath 默认 exec.LookPath。
	LookPath func(string) (string, error)
	// Stat 默认 os.Stat。
	Stat func(string) (os.FileInfo, error)
	// ProbeVersion 为 true 时真正执行各工具探测版本号（会启动子进程）。
	ProbeVersion bool
	// VersionTimeout 是版本探测的超时，默认 8s。
	VersionTimeout time.Duration
}

// Discover 探测本机工具链。
//
// DevEco 根目录的确定顺序：显式参数 → 环境变量 → 从 PATH 上的 hdc 反推。
// "反推"这一条很关键：hdc 若在 <root>/sdk/default/openharmony/toolchains/hdc.exe，
// 那么 <root> 就是它上溯若干层中同时含有 sdk 与 tools 的那一级。
// 这样不用把任何人的安装路径写死在代码里，换机器也能自适应。
func Discover(ctx context.Context, opts DiscoverOptions) *Toolchain {
	// 注意：不能用 cmp.Or —— 函数类型不可比较，编译期就会报
	// "does not satisfy comparable"。
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	stat := opts.Stat
	if stat == nil {
		stat = os.Stat
	}

	tc := &Toolchain{}

	// 1. 环境变量
	if tc.DevEcoRoot == "" && opts.DevEcoRoot != "" {
		if isDir(opts.DevEcoRoot, stat) {
			tc.DevEcoRoot, tc.DevEcoSource = opts.DevEcoRoot, "命令行/调用方指定"
		}
	}
	if tc.DevEcoRoot == "" {
		if v := strings.TrimSpace(os.Getenv(EnvDevEcoRoot)); v != "" && isDir(v, stat) {
			tc.DevEcoRoot, tc.DevEcoSource = v, "环境变量 "+EnvDevEcoRoot
		}
	}

	// 2. 先找 hdc（它同时在 PATH 上时能反推根目录）
	hdc := probeTool(ToolHDC, []candidate{
		{lookupOnly: true, base: "hdc", hint: "把 OpenHarmony SDK 的 toolchains 目录加入 PATH"},
		{rel: "sdk/default/openharmony/toolchains", base: "hdc", needRoot: true, source: "DevEco SDK"},
		{rel: "sdk/default/openharmony/toolchains", base: "hdc_std", needRoot: true, source: "DevEco SDK(旧名)"},
	}, tc.DevEcoRoot, stat, lookPath)
	tc.Tools = append(tc.Tools, hdc)

	// 3. 反推根目录
	if tc.DevEcoRoot == "" && hdc.Found() {
		if root := deriveDevEcoRoot(filepath.Dir(hdc.Path), stat); root != "" {
			tc.DevEcoRoot, tc.DevEcoSource = root, "由 hdc 路径反推"
		}
	}

	// 4. 其余工具
	tc.Tools = append(tc.Tools,
		probeTool(ToolHvigorw, []candidate{
			{lookupOnly: true, base: "hvigorw", hint: "hvigorw 通常由工程自带；也可用 DevEco 的 tools/hvigor/bin"},
			{rel: "tools/hvigor/bin", base: "hvigorw", needRoot: true, source: "DevEco tools/hvigor"},
		}, tc.DevEcoRoot, stat, lookPath),
		probeTool(ToolOhpm, []candidate{
			{lookupOnly: true, base: "ohpm", hint: "把 DevEco 的 tools/ohpm/bin 加入 PATH"},
			{rel: "tools/ohpm/bin", base: "ohpm", needRoot: true, source: "DevEco tools/ohpm"},
		}, tc.DevEcoRoot, stat, lookPath),
		probeTool(ToolNode, []candidate{
			{lookupOnly: true, base: "node", hint: "hvigor 依赖 Node，请安装 Node 或使用 DevEco 自带 node"},
			{rel: "tools/node", base: "node", needRoot: true, source: "DevEco tools/node"},
		}, tc.DevEcoRoot, stat, lookPath),
		probeTool(ToolJava, []candidate{
			{lookupOnly: true, base: "java", hint: "hvigor 依赖 JDK；DevEco 通常自带 jbr"},
			{rel: "jbr/bin", base: "java", needRoot: true, source: "DevEco jbr"},
		}, tc.DevEcoRoot, stat, lookPath),
	)

	if tc.DevEcoRoot != "" {
		if dir := filepath.Join(tc.DevEcoRoot, "sdk"); isDir(dir, stat) {
			tc.SDKDir = dir
		}
		if dir := filepath.Join(tc.DevEcoRoot, "tools", "emulator"); isDir(dir, stat) {
			tc.EmulatorDir = dir
		}
	}
	// 环境里已有有效的 SDK 路径时采纳它：用户可能把 SDK 单独装在别处
	if v := strings.TrimSpace(os.Getenv(EnvDevEcoSDKHome)); v != "" && isDir(v, stat) {
		tc.SDKDir = v
	}

	if opts.ProbeVersion {
		// 20s 而不是 8s：ohpm 这类 Node 写的批处理在 Windows 上光是启动
		// 就可能好几秒，超时设紧了会得到"版本未知"这种假缺失。
		timeout := cmp.Or(opts.VersionTimeout, 20*time.Second)
		for i := range tc.Tools {
			if tc.Tools[i].Found() {
				tc.Tools[i].Version = probeVersion(ctx, tc.Tools[i], timeout)
			}
		}
	}
	return tc
}

// candidate 是一个候选位置。
type candidate struct {
	// lookupOnly 表示只在 PATH 上找。
	lookupOnly bool
	// rel 是相对 DevEco 根目录的目录。
	rel string
	// base 是不带扩展名的可执行文件名。
	base string
	// needRoot 表示必须已知 DevEco 根目录才能尝试。
	needRoot bool
	// source 是命中后的来源说明。
	source string
	// hint 是未命中时的建议。
	hint string
}

func probeTool(name string, cands []candidate, devEcoRoot string, stat func(string) (os.FileInfo, error), lookPath func(string) (string, error)) Tool {
	t := Tool{Name: name}

	for _, c := range cands {
		if c.lookupOnly {
			// PATH 查找：把尝试过的可执行名记下来，未找到时能说清找的是什么
			for _, exe := range executableNames(c.base) {
				if p, err := lookPath(exe); err == nil && p != "" {
					t.Path, t.Source, t.Hint = p, "PATH", ""
					return t
				}
				t.Searched = append(t.Searched, "PATH:"+exe)
			}
			if t.Hint == "" && c.hint != "" {
				t.Hint = c.hint
			}
			continue
		}
		if c.needRoot && devEcoRoot == "" {
			continue
		}
		for _, exe := range executableNames(c.base) {
			p := filepath.Join(devEcoRoot, c.rel, exe)
			t.Searched = append(t.Searched, p)
			if isFile(p, stat) {
				t.Path, t.Source, t.Hint = p, c.source, ""
				return t
			}
		}
	}
	return t
}

// executableNames 返回某平台上该名字可能出现的文件名。
func executableNames(base string) []string {
	if runtime.GOOS != "windows" {
		return []string{base}
	}
	return []string{base + ".exe", base + ".bat", base + ".cmd", base}
}

// deriveDevEcoRoot 从某个目录向上找 DevEco 根目录：
// 同时含有 sdk 与 tools 两个子目录的那一级。
func deriveDevEcoRoot(from string, stat func(string) (os.FileInfo, error)) string {
	dir := from
	for range 8 {
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
		if isDir(filepath.Join(dir, "sdk"), stat) && isDir(filepath.Join(dir, "tools"), stat) {
			return dir
		}
	}
	return ""
}

func probeVersion(ctx context.Context, t Tool, timeout time.Duration) string {
	// 在临时目录里探测：hvigorw 光是启动就会在**当前工作目录**下创建
	// .hvigor/outputs/build-logs/build.log。不加这个 Dir，跑一次
	// `arkperf check` 就会在用户所在目录里留下一坨构建残留。
	res, err := execx.Run(ctx, t.Path, versionArgs(t.Name), execx.Options{
		Dir:       os.TempDir(),
		Timeout:   timeout,
		MaxOutput: 8 * 1024,
	})
	if err != nil && res.Output == "" {
		return ""
	}
	line := firstLine(res.Output)
	// hdc 输出 "Ver: 3.2.0d"，只留版本号本身
	if _, after, ok := strings.Cut(line, "Ver:"); ok {
		line = strings.TrimSpace(after)
	}
	return line
}

func versionArgs(name string) []string {
	switch name {
	case ToolHDC:
		return []string{"-v"}
	case ToolHvigorw:
		return []string{"--version"}
	case ToolOhpm, ToolNode:
		return []string{"-v"}
	case ToolJava:
		return []string{"-version"} // 输出走 stderr，execx 会合并回来
	default:
		return []string{"--version"}
	}
}

func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func isDir(p string, stat func(string) (os.FileInfo, error)) bool {
	fi, err := stat(p)
	return err == nil && fi.IsDir()
}

func isFile(p string, stat func(string) (os.FileInfo, error)) bool {
	fi, err := stat(p)
	return err == nil && !fi.IsDir()
}
