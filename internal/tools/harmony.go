package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fan-weibo/ArkPerf/internal/domain/harmony"
	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// Harmony 返回鸿蒙域工具：工具链探测、设备、构建、装机、启动、日志。
//
// tc 允许为 nil：那时工具照常注册，但每次调用都会如实报告"工具链未探测"，
// 而不是抛异常或假装成功——模型看到原因才能自己决定换策略。
//
// 审批规则（贯穿本文件）：
//   - 只读探测（toolchain_check / devices / logs）→ 免审批
//   - 会执行工程代码或改变设备状态（build / install / launch / shell）→ 需要审批
//
// build 之所以也要审批：hvigor 会执行工程自带的 hvigorfile.ts，
// 那等同于运行工程提供的代码。分析第三方工程时这一点尤其重要。
func Harmony(tc *harmony.Toolchain) []kernel.Tool {
	tools := []kernel.Tool{
		toolchainCheckTool{tc},
		devicesTool{tc},
		buildTool{tc},
		installTool{tc},
		launchTool{tc},
		logsTool{tc},
		shellTool{tc},
	}
	// 模拟器组（解决"没有真机时怎么测"）与工程组（补上从代码到能跑起来的环节）
	// 各自成文件：这一组的工具数量已经多到塞在一个文件里没法读了。
	tools = append(tools, HarmonyEmulator(tc)...)
	tools = append(tools, HarmonyProject(tc)...)
	return tools
}

// ---------------------------------------------------------------- 工具链探测

type toolchainCheckTool struct{ tc *harmony.Toolchain }

func (toolchainCheckTool) Name() string { return "harmony_toolchain_check" }

func (toolchainCheckTool) Description() string {
	return "探测本机 OpenHarmony 工具链（hdc / hvigorw / ohpm / node / java 与 DevEco 安装位置），报告每个工具的路径、版本与缺失项。只读，免审批。"
}

func (toolchainCheckTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

func (toolchainCheckTool) NeedsApproval(map[string]any) bool { return false }

func (t toolchainCheckTool) Execute(ctx context.Context, _ map[string]any, _ kernel.ToolCtx) (kernel.ToolResult, error) {
	if t.tc == nil {
		return kernel.ToolResult{Output: "工具链未探测（本次启动跳过了探测）"}, nil
	}
	// 显式检查才现探版本号：探测要启动子进程，日常调用没必要付这个成本
	tc := harmony.Discover(ctx, harmony.DiscoverOptions{
		DevEcoRoot:   t.tc.DevEcoRoot,
		ProbeVersion: true,
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "OpenHarmony 工具链：%s\n", tc.Summary())
	if tc.DevEcoSource != "" {
		fmt.Fprintf(&sb, "DevEco 根目录：%s（%s）\n", tc.DevEcoRoot, tc.DevEcoSource)
	}
	sb.WriteString("\n")
	for _, tool := range tc.Tools {
		fmt.Fprintf(&sb, "- %s\n", tool.Describe())
		if !tool.Found() && tool.Hint != "" {
			fmt.Fprintf(&sb, "    建议：%s\n", tool.Hint)
		}
	}
	if tc.EmulatorDir != "" {
		fmt.Fprintf(&sb, "- 模拟器工具目录：%s\n", tc.EmulatorDir)
	}
	return kernel.ToolResult{Output: strings.TrimRight(sb.String(), "\n")}, nil
}

// ---------------------------------------------------------------- 设备

type devicesTool struct{ tc *harmony.Toolchain }

func (devicesTool) Name() string { return "harmony_devices" }

func (devicesTool) Description() string {
	return "列出当前已连接的 OpenHarmony 设备/模拟器（hdc list targets）。只读，免审批。"
}

func (devicesTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

func (devicesTool) NeedsApproval(map[string]any) bool { return false }

func (t devicesTool) Execute(ctx context.Context, _ map[string]any, _ kernel.ToolCtx) (kernel.ToolResult, error) {
	if t.tc == nil {
		return kernel.ToolResult{}, errors.New("工具链未探测，无法查询设备")
	}
	devices, err := t.tc.ListDevices(ctx)
	if err != nil {
		return kernel.ToolResult{}, err
	}
	if len(devices) == 0 {
		// 这是"没有设备"，不是"查询失败"——不能报成错误，否则调用方会
		// 误以为 hdc 坏了而去修 hdc
		return kernel.ToolResult{Output: strings.Join([]string{
			"当前没有已连接的设备（hdc list targets 返回空）。",
			"可能原因：模拟器未启动；真机未开启调试 / 未授权；网络设备未连接。",
			"网络设备可用 `hdc tconn <ip:port>` 连接；模拟器通常为 127.0.0.1:5555。",
		}, "\n")}, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "已连接 %d 个设备：\n", len(devices))
	for _, d := range devices {
		fmt.Fprintf(&sb, "- %s\n", d.Serial)
	}
	return kernel.ToolResult{Output: strings.TrimRight(sb.String(), "\n")}, nil
}

// ---------------------------------------------------------------- 构建

type buildTool struct{ tc *harmony.Toolchain }

func (buildTool) Name() string { return "harmony_build" }

func (buildTool) Description() string {
	return "在 OpenHarmony 工程里执行 hvigorw 构建（默认任务 assembleHap）。会自动定位工程根目录与可用的 hvigorw。会执行工程自带的 hvigorfile.ts，因此需要审批。"
}

func (buildTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "task": {"type": "string", "description": "hvigor 任务名，默认 assembleHap；其他常用：assembleHsp、assembleApp、clean"},
    "projectDir": {"type": "string", "description": "工程内任意目录；留空用当前工作目录向上查找"},
    "extra": {"type": "array", "items": {"type": "string"}, "description": "追加到 hvigorw 之后的额外参数，如 --mode module"}
  },
  "additionalProperties": false
}`)
}

// 会执行工程提供的构建脚本，等同于运行工程代码 → 必须审批
func (buildTool) NeedsApproval(map[string]any) bool { return true }

func (t buildTool) Execute(ctx context.Context, args map[string]any, tc kernel.ToolCtx) (kernel.ToolResult, error) {
	proj, err := harmony.FindProject(startDir(args, tc))
	if err != nil {
		return kernel.ToolResult{}, err
	}
	if !proj.UseToolchain(t.tc) {
		return kernel.ToolResult{}, errors.New("找不到 hvigorw：工程内没有构建脚本，DevEco 的 tools/hvigor/bin 也不可用（先跑 harmony_toolchain_check 看缺什么）")
	}

	res, err := proj.Build(ctx, strArg(args, "task"), strSliceArg(args, "extra"))
	header := fmt.Sprintf("工程：%s\n构建入口：%s（%s）\n命令：%s\n耗时：%s\n退出码：%d\n",
		proj.Root, proj.Hvigorw, proj.HvigorwSource, res.Command, res.Duration.Round(time.Millisecond), res.ExitCode)

	// 命令跑起来了但失败：把日志交回模型（它要据此判断怎么改），标成 IsError
	if err != nil {
		return kernel.ToolResult{Output: header + "\n" + res.Output, IsError: true}, nil
	}

	modules := "（无）"
	if len(proj.Modules) > 0 {
		modules = strings.Join(proj.Modules, ", ")
	}
	out := header + fmt.Sprintf("模块：%s\n\n%s", modules, tail(res.Output, 4000))
	if hap, err := proj.FindHap(); err == nil {
		out += "\n\n产物：" + hap
	}
	return kernel.ToolResult{Output: out}, nil
}

// ---------------------------------------------------------------- 装机

type installTool struct{ tc *harmony.Toolchain }

func (installTool) Name() string { return "harmony_install" }

func (installTool) Description() string {
	return "把 .hap 安装到设备（hdc install -r，覆盖安装）。hap 留空时自动取工程构建产物中最新的一个。会改变设备状态，需要审批。"
}

func (installTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "hap": {"type": "string", "description": ".hap 文件路径；留空则自动从工程构建产物里找最新的"},
    "projectDir": {"type": "string", "description": "用于自动定位产物的工程目录，留空用当前工作目录"},
    "device": {"type": "string", "description": "设备序列号；多设备时必须指定"}
  },
  "additionalProperties": false
}`)
}

func (installTool) NeedsApproval(map[string]any) bool { return true }

func (t installTool) Execute(ctx context.Context, args map[string]any, tc kernel.ToolCtx) (kernel.ToolResult, error) {
	hap := strArg(args, "hap")
	if hap == "" {
		proj, err := harmony.FindProject(startDir(args, tc))
		if err != nil {
			return kernel.ToolResult{}, fmt.Errorf("未指定 hap 且无法定位工程产物：%w", err)
		}
		if hap, err = proj.FindHap(); err != nil {
			return kernel.ToolResult{}, err
		}
	}

	res, err := t.tc.Install(ctx, strArg(args, "device"), hap)
	out := fmt.Sprintf("命令：%s\n耗时：%s\n退出码：%d\n\n%s", res.Command, res.Duration.Round(time.Millisecond), res.ExitCode, res.Output)
	if err != nil {
		return kernel.ToolResult{Output: out, IsError: true}, nil
	}
	return kernel.ToolResult{Output: "安装成功。\n" + out}, nil
}

// ---------------------------------------------------------------- 启动

type launchTool struct{ tc *harmony.Toolchain }

func (launchTool) Name() string { return "harmony_launch" }

func (launchTool) Description() string {
	return "启动设备上已安装应用（aa start）。ability 留空时自动从设备包信息解析——" +
		"实测只给 -b 会报 10103101，必须带 -a。默认附加 -W，会回报启动耗时 WaitTime/TotalTime。" +
		"会改变设备状态，需要审批。"
}

func (launchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "bundle": {"type": "string", "description": "应用包名，如 com.example.app"},
    "ability": {"type": "string", "description": "Ability 名，如 EntryAbility；留空则从设备包信息自动解析"},
    "device": {"type": "string", "description": "设备序列号；多设备时必须指定"},
    "wait": {"type": "boolean", "description": "是否等待启动完成并回报耗时（-W），默认 true"}
  },
  "required": ["bundle"],
  "additionalProperties": false
}`)
}

func (launchTool) NeedsApproval(map[string]any) bool { return true }

func (t launchTool) Execute(ctx context.Context, args map[string]any, tc kernel.ToolCtx) (kernel.ToolResult, error) {
	bundle := strArg(args, "bundle")
	if bundle == "" {
		return kernel.ToolResult{}, errors.New("bundle（应用包名）不能为空")
	}
	wait := boolArg(args, "wait", true)

	res, err := t.tc.StartAbility(ctx, strArg(args, "device"), bundle, strArg(args, "ability"), wait)
	out := fmt.Sprintf("命令：%s\n耗时：%s\n退出码：%d\n\n%s",
		res.Command, res.Duration.Round(time.Millisecond), res.ExitCode, res.Output)

	// 启动耗时是 ArkPerf 的核心指标之一，解析出来直接摆在明面上，
	// 别让模型去正则里捞。
	if timings, ok := harmony.ParseStartTimings(res.Output); ok {
		out += fmt.Sprintf("\n\n启动耗时：TotalTime=%dms  WaitTime=%dms", timings.TotalMs, timings.WaitMs)
		if timings.Mode != "" {
			out += "  StartMode=" + timings.Mode
		}
	}
	if err != nil {
		return kernel.ToolResult{Output: out, IsError: true}, nil
	}
	return kernel.ToolResult{Output: out}, nil
}

// ---------------------------------------------------------------- 日志

type logsTool struct{ tc *harmony.Toolchain }

func (logsTool) Name() string { return "harmony_logs" }

func (logsTool) Description() string {
	return "读取设备 hilog 缓冲区（hilog -x，读完即退出，不会挂着刷）。可传 args 追加过滤参数，如 -T <tag>、-L E。只读，免审批。"
}

func (logsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "args": {"type": "array", "items": {"type": "string"}, "description": "追加给 hilog 的参数，例如 [\"-T\",\"MyTag\"] 或 [\"-L\",\"E\"]"},
    "device": {"type": "string", "description": "设备序列号；多设备时必须指定"}
  },
  "additionalProperties": false
}`)
}

func (logsTool) NeedsApproval(map[string]any) bool { return false }

func (t logsTool) Execute(ctx context.Context, args map[string]any, tc kernel.ToolCtx) (kernel.ToolResult, error) {
	res, err := t.tc.Logs(ctx, strArg(args, "device"), strSliceArg(args, "args")...)
	out := fmt.Sprintf("命令：%s\n耗时：%s\n退出码：%d\n\n%s", res.Command, res.Duration.Round(time.Millisecond), res.ExitCode, tail(res.Output, 8000))
	if err != nil {
		return kernel.ToolResult{Output: out, IsError: true}, nil
	}
	return kernel.ToolResult{Output: out}, nil
}

// ---------------------------------------------------------------- 任意设备命令

type shellTool struct{ tc *harmony.Toolchain }

func (shellTool) Name() string { return "harmony_shell" }

func (shellTool) Description() string {
	return "在设备上执行任意 shell 命令（hdc shell）。自由度大、风险也大（可改设备状态、删应用数据），需要审批。能确定用途时优先用更具体的工具。"
}

func (shellTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "array", "items": {"type": "string"}, "description": "命令与参数，逐个元素传入，如 [\"hidumper\",\"--mem\"]"},
    "device": {"type": "string", "description": "设备序列号；多设备时必须指定"}
  },
  "required": ["command"],
  "additionalProperties": false
}`)
}

// 硬拒的命令不需要审批——护栏已经替用户决定了。
// 若这里返回 true，会出现"先弹审批框、用户点了同意、然后才被告知不许做"。
func (shellTool) NeedsApproval(args map[string]any) bool {
	if kernel.CheckCommand(strings.Join(strSliceArg(args, "command"), " ")) != nil {
		return false
	}
	return true
}

func (t shellTool) Execute(ctx context.Context, args map[string]any, tc kernel.ToolCtx) (kernel.ToolResult, error) {
	command := strSliceArg(args, "command")
	if len(command) == 0 {
		return kernel.ToolResult{}, errors.New("command 不能为空")
	}

	// 设备同样是真实系统：抹掉 /data 或 /system 与抹掉宿主一样不可逆。
	// 护栏必须与"命令发到哪里"无关——不能因为目标是设备就跳过。
	if err := kernel.CheckCommand(strings.Join(command, " ")); err != nil {
		return kernel.ToolResult{}, err
	}

	res, err := t.tc.Shell(ctx, strArg(args, "device"), command...)
	out := fmt.Sprintf("命令：%s\n耗时：%s\n退出码：%d\n\n%s", res.Command, res.Duration.Round(time.Millisecond), res.ExitCode, tail(res.Output, 8000))
	if err != nil {
		return kernel.ToolResult{Output: out, IsError: true}, nil
	}
	return kernel.ToolResult{Output: out}, nil
}

// ---------------------------------------------------------------- 参数小工具

func strArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}

// boolArg 读布尔参数，容忍模型写成字符串（"true" / "false"）。
func boolArg(args map[string]any, key string, def bool) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "1":
			return true
		case "false", "no", "0":
			return false
		}
	}
	return def
}

func startDir(args map[string]any, tc kernel.ToolCtx) string {
	if dir := strArg(args, "projectDir"); dir != "" {
		return dir
	}
	return tc.CWD
}

// strSliceArg 读字符串数组参数，容忍单个字符串（模型偶尔会这么写）。
func strSliceArg(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return strings.Fields(v)
	default:
		return nil
	}
}

// tail 取输出末尾若干字节。构建失败的原因几乎总在最后。
func tail(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	cut := s[len(s)-limit:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[i+1:] // 从整行开始，避免半行
	}
	return "…（前文省略）\n" + cut
}
