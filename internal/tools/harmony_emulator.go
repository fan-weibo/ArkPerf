package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/fan-weibo/ArkPerf/internal/domain/harmony"
	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// HarmonyEmulator 返回模拟器相关的工具。
//
// 这一组解决的是"没有真机时怎么测"：ArkPerf 要在 PC / 手机 / 折叠屏 / 平板
// 四种形态上采集数据，而手工在 DevEco 里点开模拟器、再回来跑命令，
// 既无法自动化，也没法在脚本里复现。
//
// 数据来源与 DevEco 安装目录**无关**（实例与镜像都在 %LOCALAPPDATA%\Huawei 下），
// 只有启动要用的 Emulator.exe 来自 DevEco。
func HarmonyEmulator(tc *harmony.Toolchain) []kernel.Tool {
	return []kernel.Tool{
		emulatorListTool{tc},
		emulatorCatalogTool{},
		emulatorImageCheckTool{tc},
		emulatorStartTool{tc},
		emulatorStopTool{tc},
		emulatorCreateTool{tc},
		emulatorDeleteTool{tc},
	}
}

// ---------------------------------------------------------------- 列表

type emulatorListTool struct{ tc *harmony.Toolchain }

func (emulatorListTool) Name() string { return "harmony_emulator_list" }

func (emulatorListTool) Description() string {
	return "列出本机已部署的 OpenHarmony 模拟器（名称/形态/分辨率/系统版本）以及正在运行的是哪些。只读，免审批。" +
		"这些信息在 %LOCALAPPDATA%\\Huawei\\Emulator\\deployed 下，与 DevEco 安装位置无关。"
}

func (emulatorListTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

func (emulatorListTool) NeedsApproval(map[string]any) bool { return false }

func (t emulatorListTool) Execute(ctx context.Context, _ map[string]any, _ kernel.ToolCtx) (kernel.ToolResult, error) {
	list, err := harmony.ListEmulators()
	if err != nil {
		return kernel.ToolResult{}, err
	}
	if len(list) == 0 {
		// 没有模拟器不是错误：全新机器上就是这个状态，
		// 报成错误会让模型以为是环境坏了而去修环境。
		return kernel.ToolResult{Output: strings.Join([]string{
			"本机没有已部署的模拟器。",
			"可用 harmony_emulator_catalog 看有哪些型号能建，再用 harmony_emulator_create 或 DevEco 创建。",
		}, "\n")}, nil
	}

	running, rerr := harmony.RunningEmulators(ctx)
	if rerr != nil {
		running = nil // 查不到运行状态也要把清单给出来，只是少一列
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "已部署 %d 个模拟器：\n", len(list))
	for _, e := range list {
		state := "未运行"
		for _, r := range running {
			if strings.EqualFold(r, e.Name) {
				state = "运行中"
				break
			}
		}
		fmt.Fprintf(&sb, "- %s｜%s｜%s｜%s｜%s｜%s\n", e.Name, orUnknown(e.Type), e.Resolution(), orUnknown(e.ShowVer), orUnknown(e.ImageDir), state)
	}
	if rerr != nil {
		fmt.Fprintf(&sb, "\n（运行状态查询失败：%s）", rerr.Error())
	}
	return kernel.ToolResult{Output: strings.TrimRight(sb.String(), "\n")}, nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "（未知）"
	}
	return s
}

// ---------------------------------------------------------------- 设备目录

type emulatorCatalogTool struct{}

func (emulatorCatalogTool) Name() string { return "harmony_emulator_catalog" }

func (emulatorCatalogTool) Description() string {
	return "列出本机 SDK 里可创建的官方设备型号目录（名称/形态/分辨率/尺寸），来自 productConfig.json。只读，免审批。" +
		"想知道「能建哪些形态的模拟器」时用这个，而不是猜型号名。"
}

func (emulatorCatalogTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "type": {"type": "string", "description": "只看某种形态：phone / tablet / 2in1 / wearable / foldable（留空看全部）"}
  },
  "additionalProperties": false
}`)
}

func (emulatorCatalogTool) NeedsApproval(map[string]any) bool { return false }

func (emulatorCatalogTool) Execute(_ context.Context, args map[string]any, _ kernel.ToolCtx) (kernel.ToolResult, error) {
	catalog, err := harmony.Catalog()
	if err != nil {
		return kernel.ToolResult{}, err
	}
	filter := strings.ToLower(strArg(args, "type"))

	var kept []harmony.DeviceCatalog
	for _, c := range catalog {
		if filter == "" || strings.Contains(strings.ToLower(c.DeviceType), filter) ||
			strings.Contains(strings.ToLower(c.Name), filter) {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		if len(catalog) == 0 {
			return kernel.ToolResult{}, errors.New("没读到设备目录（productConfig.json 不存在或解析失败）")
		}
		return kernel.ToolResult{Output: fmt.Sprintf("设备目录里没有匹配 %q 的条目（共 %d 条）", strArg(args, "type"), len(catalog))}, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "设备目录（%d/%d）：\n", len(kept), len(catalog))
	for _, c := range kept {
		fmt.Fprintf(&sb, "- %s｜%s｜%dx%d｜%s 英寸｜dpi=%d\n",
			c.Name, c.DeviceType, c.Width, c.Height, c.Diagonal, c.Density)
	}
	return kernel.ToolResult{Output: strings.TrimRight(sb.String(), "\n")}, nil
}

// ---------------------------------------------------------------- 镜像检查

type emulatorImageCheckTool struct{ tc *harmony.Toolchain }

func (emulatorImageCheckTool) Name() string { return "harmony_image_check" }

func (emulatorImageCheckTool) Description() string {
	return "检查本机已安装的模拟器系统镜像（API 版本 × 设备形态）。只读，免审批。" +
		"没有镜像就无法新建模拟器——这个工具用来确认「缺的是镜像还是实例」。"
}

func (emulatorImageCheckTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "type": {"type": "string", "description": "只看某种形态的镜像，如 phone / tablet / pc（留空看全部）"}
  },
  "additionalProperties": false
}`)
}

func (emulatorImageCheckTool) NeedsApproval(map[string]any) bool { return false }

func (t emulatorImageCheckTool) Execute(ctx context.Context, args map[string]any, _ kernel.ToolCtx) (kernel.ToolResult, error) {
	images, err := t.tc.InstalledImages(ctx)
	if err != nil {
		return kernel.ToolResult{}, err
	}
	filter := strings.ToLower(strArg(args, "type"))
	var kept []harmony.SystemImage
	for _, img := range images {
		if filter == "" || strings.Contains(strings.ToLower(img.DeviceType), filter) {
			kept = append(kept, img)
		}
	}
	if len(images) == 0 {
		return kernel.ToolResult{
			Output: "本机没有安装任何模拟器系统镜像。\n" +
				"镜像只能通过 DevEco 的组件管理器（Settings → SDK → HarmonyOS 镜像）下载；" +
				"装好镜像后再用 harmony_emulator_create 创建实例。",
			IsError: true,
		}, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "已下载镜像（%d/%d）：\n", len(kept), len(images))
	for _, img := range kept {
		if img.SoftVersion != "" {
			fmt.Fprintf(&sb, "- %s｜%s｜%s\n", img.OSVersion, img.DeviceType, img.SoftVersion)
			continue
		}
		fmt.Fprintf(&sb, "- %s｜%s\n", img.OSVersion, img.DeviceType)
	}
	if len(kept) == 0 {
		fmt.Fprintf(&sb, "（形态筛选 %q 没有匹配项）", filter)
	}
	return kernel.ToolResult{Output: strings.TrimRight(sb.String(), "\n")}, nil
}

// ---------------------------------------------------------------- 启动

type emulatorStartTool struct{ tc *harmony.Toolchain }

func (emulatorStartTool) Name() string { return "harmony_emulator_start" }

func (emulatorStartTool) Description() string {
	return "无头启动一个已部署的模拟器（直接拉起 Emulator.exe，不打开 DevEco 界面），并等待它出现在 hdc 设备列表里。" +
		"首次启动通常要 30-90 秒。会启动一个长期运行的进程，需要审批。"
}

func (emulatorStartTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "模拟器名称；留空且本机只有一个时用它"}
  },
  "additionalProperties": false
}`)
}

func (emulatorStartTool) NeedsApproval(map[string]any) bool { return true }

func (t emulatorStartTool) Execute(ctx context.Context, args map[string]any, _ kernel.ToolCtx) (kernel.ToolResult, error) {
	_, out, err := t.tc.StartEmulator(ctx, strArg(args, "name"))
	if err != nil {
		// 启动失败时 out 常是空的（Emulator.exe 的输出被它自己吞了），
		// 只回 out 等于什么都没说。错误本身必须带回去，超时还要补日志尾部。
		msg := err.Error()
		var to *harmony.EmulatorStartTimeout
		if errors.As(err, &to) {
			if tail := logTail(to.LogPath(), 25); tail != "" {
				msg += "\n\n问题定位：" + to.Hint() + "\n" + to.LogPath() + " 尾部：\n" + tail
			} else {
				msg += "\n\n问题定位：" + to.Hint() + "\n（未找到日志 " + to.LogPath() + "，可能实例目录还未生成）"
			}
		}
		if out != "" {
			msg += "\n\n启动命令输出：\n" + out
		}
		return kernel.ToolResult{Output: msg, IsError: true}, nil
	}
	return kernel.ToolResult{Output: out}, nil
}

// logTail 读文件最后 n 行。读不到就返回空串——日志缺失不该让报错本身失败。
func logTail(path string, n int) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// ---------------------------------------------------------------- 停止

type emulatorStopTool struct{ tc *harmony.Toolchain }

func (emulatorStopTool) Name() string { return "harmony_emulator_stop" }

func (emulatorStopTool) Description() string {
	return "停止正在运行的模拟器（按进程精确匹配实例名）。name 留空表示停止全部。会结束进程，需要审批。"
}

func (emulatorStopTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "模拟器名称；留空停止所有运行中的模拟器"}
  },
  "additionalProperties": false
}`)
}

func (emulatorStopTool) NeedsApproval(map[string]any) bool { return true }

func (t emulatorStopTool) Execute(ctx context.Context, args map[string]any, _ kernel.ToolCtx) (kernel.ToolResult, error) {
	stopped, err := t.tc.StopEmulator(ctx, strArg(args, "name"))
	if err != nil {
		return kernel.ToolResult{Output: err.Error(), IsError: true}, nil
	}
	return kernel.ToolResult{Output: "已停止：" + strings.Join(stopped, "、")}, nil
}

// ---------------------------------------------------------------- 创建

type emulatorCreateTool struct{ tc *harmony.Toolchain }

func (emulatorCreateTool) Name() string { return "harmony_emulator_create" }

func (emulatorCreateTool) Description() string {
	return "新建一个模拟器实例（走官方 Emulator.exe -create，会正确维护 DevEco 的实例清单）。" +
		"device_type 取 phone / tablet / 2in1 / foldable / widefold / triplefold / wearable / tv；" +
		"os_version 留空会自动选一个该形态已下载的镜像，screen_profile 留空则取实例名本身（本机实例名就是型号名，如 \"Pura 90\"）。" +
		"形态必须先有镜像（先用 harmony_image_check 确认）。会写入模拟器目录，需要审批。"
}

func (emulatorCreateTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "新实例名称：字母数字开头，可含空格与短横线，1-31 字符"},
    "device_type": {"type": "string", "description": "形态：phone / tablet / 2in1 / foldable / widefold / triplefold / wearable / tv；留空为 phone"},
    "os_version": {"type": "string", "description": "镜像版本，形如 \"HarmonyOS 6.1.1(24)\"；留空自动选已下载的"},
    "screen_profile": {"type": "string", "description": "屏幕型号，如 \"Pura 90\"；留空取 name"}
  },
  "required": ["name"],
  "additionalProperties": false
}`)
}

func (emulatorCreateTool) NeedsApproval(map[string]any) bool { return true }

func (t emulatorCreateTool) Execute(ctx context.Context, args map[string]any, _ kernel.ToolCtx) (kernel.ToolResult, error) {
	name := strArg(args, "name")
	if name == "" {
		return kernel.ToolResult{}, errors.New("name 不能为空")
	}
	created, err := t.tc.CreateEmulator(ctx, name,
		strArg(args, "device_type"), strArg(args, "os_version"), strArg(args, "screen_profile"))
	if err != nil {
		return kernel.ToolResult{Output: err.Error(), IsError: true}, nil
	}
	return kernel.ToolResult{Output: fmt.Sprintf(
		"已创建模拟器 %s（形态 %s，型号 %s，系统 %s，分辨率 %s）\n目录：%s\n"+
			"注意：新建实例要**首次启动成功后**才会写进 lists.json，本工具已能直接识别它。\n下一步：harmony_emulator_start 启动它。",
		created.Name, orUnknown(created.Type), orUnknown(created.Model),
		orUnknown(created.ShowVer), created.Resolution(), created.Path)}, nil
}

// ---------------------------------------------------------------- 删除

type emulatorDeleteTool struct{ tc *harmony.Toolchain }

func (emulatorDeleteTool) Name() string { return "harmony_emulator_delete" }

func (emulatorDeleteTool) Description() string {
	return "删除一个模拟器实例（走官方 Emulator.exe -delete，实例目录与清单一起清理）。" +
		"运行中会拒绝删除。这是破坏性操作，需要审批。"
}

func (emulatorDeleteTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "要删除的模拟器名称"}
  },
  "required": ["name"],
  "additionalProperties": false
}`)
}

func (emulatorDeleteTool) NeedsApproval(map[string]any) bool { return true }

func (t emulatorDeleteTool) Execute(ctx context.Context, args map[string]any, _ kernel.ToolCtx) (kernel.ToolResult, error) {
	name := strArg(args, "name")
	if name == "" {
		return kernel.ToolResult{}, errors.New("name 不能为空")
	}
	if err := t.tc.DeleteEmulator(ctx, name); err != nil {
		return kernel.ToolResult{Output: err.Error(), IsError: true}, nil
	}
	return kernel.ToolResult{Output: "已删除模拟器：" + name}, nil
}
