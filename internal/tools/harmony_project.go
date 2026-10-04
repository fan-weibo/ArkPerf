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

// HarmonyProject 返回工程与设备侧的补充工具。
//
// 这一组补的是"从代码到能跑起来"之间的环节：工程画像、配置预检、
// 构建失败诊断、签名、静态检查、设备冒烟、UI 截图、卸载。
// 它们都不采集性能数据——采集由 mcp-servers 的测量服务负责——
// 但没有这一组，采集前的准备工作得全部手工做。
func HarmonyProject(tc *harmony.Toolchain) []kernel.Tool {
	return []kernel.Tool{
		uninstallTool{tc},
		projectProfileTool{},
		schemaCheckTool{},
		buildDoctorTool{tc},
		apiLookupTool{tc},
		signTool{tc},
		lintTool{tc},
		deviceTestTool{tc},
		uiRegressionTool{tc},
	}
}

// ---------------------------------------------------------------- 卸载

type uninstallTool struct{ tc *harmony.Toolchain }

func (uninstallTool) Name() string { return "harmony_uninstall" }

func (uninstallTool) Description() string {
	return "按包名从设备卸载应用（hdc uninstall）。" +
		"冷启动测量前必须卸载重装——否则测到的是热启动，数据会偏乐观。会改变设备状态，需要审批。"
}

func (uninstallTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "bundle": {"type": "string", "description": "应用包名；留空则从工程配置（AppScope/app.json5）读取"},
    "projectDir": {"type": "string", "description": "用于读取包名的工程目录，留空用当前工作目录"},
    "device": {"type": "string", "description": "设备序列号；多设备时必须指定"}
  },
  "additionalProperties": false
}`)
}

func (uninstallTool) NeedsApproval(map[string]any) bool { return true }

func (t uninstallTool) Execute(ctx context.Context, args map[string]any, tcx kernel.ToolCtx) (kernel.ToolResult, error) {
	bundle := strArg(args, "bundle")
	if bundle == "" {
		proj, err := harmony.FindProject(startDir(args, tcx))
		if err != nil {
			return kernel.ToolResult{}, fmt.Errorf("未指定 bundle 且无法定位工程：%w", err)
		}
		if bundle, err = harmony.ReadBundleName(proj.Root); err != nil {
			return kernel.ToolResult{}, fmt.Errorf("读不到应用包名：%w", err)
		}
	}
	res, err := t.tc.Uninstall(ctx, strArg(args, "device"), bundle)
	out := fmt.Sprintf("包名：%s\n命令：%s\n退出码：%d\n\n%s", bundle, res.Command, res.ExitCode, res.Output)
	if err != nil {
		return kernel.ToolResult{Output: out, IsError: true}, nil
	}
	return kernel.ToolResult{Output: "已卸载 " + bundle + "。\n" + out}, nil
}

// ---------------------------------------------------------------- 工程画像

type projectProfileTool struct{}

func (projectProfileTool) Name() string { return "harmony_project_profile" }

func (projectProfileTool) Description() string {
	return "一次性给出 OpenHarmony 工程的全貌：包名、目标 SDK、各模块的类型/设备类型/ability/页面、依赖、源码与资源规模，并顺带报告配置问题。" +
		"分析一个陌生工程时先跑这个，比逐个文件去读省事得多。只读，免审批。"
}

func (projectProfileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "projectDir": {"type": "string", "description": "工程内任意目录；留空用当前工作目录向上查找"}
  },
  "additionalProperties": false
}`)
}

func (projectProfileTool) NeedsApproval(map[string]any) bool { return false }

func (projectProfileTool) Execute(_ context.Context, args map[string]any, tcx kernel.ToolCtx) (kernel.ToolResult, error) {
	proj, err := harmony.FindProject(startDir(args, tcx))
	if err != nil {
		return kernel.ToolResult{}, err
	}
	profile, err := harmony.Profile(proj.Root)
	if err != nil {
		return kernel.ToolResult{}, err
	}
	return kernel.ToolResult{Output: profile.Format()}, nil
}

// ---------------------------------------------------------------- 配置校验

type schemaCheckTool struct{}

func (schemaCheckTool) Name() string { return "harmony_schema_check" }

func (schemaCheckTool) Description() string {
	return "在构建之前校验工程配置结构（根 build-profile、模块 build-profile、module.json5），" +
		"指出具体是哪个文件的哪个字段有问题。只读，免审批。" +
		"构建时配置错误要等几十秒才暴露，而且报错往往不含文件名——先跑这个能省掉一轮无效构建。"
}

func (schemaCheckTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "projectDir": {"type": "string", "description": "工程内任意目录；留空用当前工作目录向上查找"}
  },
  "additionalProperties": false
}`)
}

func (schemaCheckTool) NeedsApproval(map[string]any) bool { return false }

func (schemaCheckTool) Execute(_ context.Context, args map[string]any, tcx kernel.ToolCtx) (kernel.ToolResult, error) {
	proj, err := harmony.FindProject(startDir(args, tcx))
	if err != nil {
		return kernel.ToolResult{}, err
	}
	issues, checked, err := harmony.CheckProjectSchema(proj.Root)
	if err != nil {
		return kernel.ToolResult{}, err
	}
	if checked == 0 {
		return kernel.ToolResult{
			Output:  "没有读到任何配置文件，因此也没有可校验的内容（工程根：" + proj.Root + "）",
			IsError: true,
		}, nil
	}
	if len(issues) == 0 {
		return kernel.ToolResult{Output: fmt.Sprintf("配置检查通过：%d 个文件，未发现问题。\n工程：%s", checked, proj.Root)}, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "发现 %d 处配置问题（检查了 %d 个文件）：\n", len(issues), checked)
	for _, i := range issues {
		fmt.Fprintf(&sb, "- %s\n", i.String())
	}
	return kernel.ToolResult{Output: strings.TrimRight(sb.String(), "\n"), IsError: true}, nil
}

// ---------------------------------------------------------------- 构建诊断

type buildDoctorTool struct{ tc *harmony.Toolchain }

func (buildDoctorTool) Name() string { return "harmony_build_doctor" }

func (buildDoctorTool) Description() string {
	return "诊断一次失败的构建：把日志归入已知原因（SDK 路径 / SDK 版本 / 签名 / 依赖 / 构建环境 / 源码 / 配置 / 网络），并给出具体的下一步。" +
		"可以直接给 log，也可以给 projectDir 让它自己跑一次构建再诊断（后者会执行构建，需要审批）。"
}

func (buildDoctorTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "log": {"type": "string", "description": "构建失败的日志文本"},
    "projectDir": {"type": "string", "description": "不给 log 时，用这个工程跑一次构建再诊断"}
  },
  "additionalProperties": false
}`)
}

// 只在"要跑一次构建"时才需要审批；纯粹分析日志是只读的
func (buildDoctorTool) NeedsApproval(args map[string]any) bool {
	return strings.TrimSpace(strArg(args, "log")) == ""
}

func (t buildDoctorTool) Execute(ctx context.Context, args map[string]any, tcx kernel.ToolCtx) (kernel.ToolResult, error) {
	logText := strArg(args, "log")
	if logText == "" {
		proj, err := harmony.FindProject(startDir(args, tcx))
		if err != nil {
			return kernel.ToolResult{}, err
		}
		if !proj.UseToolchain(t.tc) {
			return kernel.ToolResult{}, errors.New("找不到 hvigorw，无法自己跑构建；请直接把日志用 log 参数传进来")
		}
		res, err := proj.Build(ctx, "", nil)
		logText = res.Output
		if err == nil {
			return kernel.ToolResult{Output: "构建成功，无需诊断。\n" + tail(logText, 2000)}, nil
		}
	}

	d := harmony.DiagnoseBuildLog(logText)
	var sb strings.Builder
	if d.Confident {
		fmt.Fprintf(&sb, "判断：%s（%s）\n", d.Title, d.Cause)
	} else {
		fmt.Fprintf(&sb, "未能归入已知类别。\n")
	}
	if d.Evidence != "" {
		sb.WriteString("\n证据：\n" + d.Evidence + "\n")
	}
	if len(d.Advice) > 0 {
		sb.WriteString("\n建议：\n")
		for _, a := range d.Advice {
			sb.WriteString("- " + a + "\n")
		}
	}
	return kernel.ToolResult{Output: strings.TrimRight(sb.String(), "\n"), IsError: !d.Confident}, nil
}

// ---------------------------------------------------------------- API 查询

type apiLookupTool struct{ tc *harmony.Toolchain }

func (apiLookupTool) Name() string { return "harmony_api_lookup" }

func (apiLookupTool) Description() string {
	return "在本机 SDK 的 ArkTS 声明文件（d.ts）里查一个 API 是否存在、在哪个文件、属于哪个类。" +
		"查的是**真实安装的 SDK**，不是网上查来的记忆——写优化建议前用它确认 API 确实存在。只读，免审批（首次调用会建索引，稍慢）。"
}

func (apiLookupTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "symbol": {"type": "string", "description": "要查的符号，如 UIAbility、hilog.info、Want"},
    "limit": {"type": "integer", "description": "最多返回几条，默认 10"}
  },
  "required": ["symbol"],
  "additionalProperties": false
}`)
}

func (apiLookupTool) NeedsApproval(map[string]any) bool { return false }

func (t apiLookupTool) Execute(_ context.Context, args map[string]any, _ kernel.ToolCtx) (kernel.ToolResult, error) {
	symbol := strArg(args, "symbol")
	if symbol == "" {
		return kernel.ToolResult{}, errors.New("symbol 不能为空")
	}
	if t.tc == nil {
		return kernel.ToolResult{}, errors.New("工具链未探测，无法定位 SDK 声明目录（先跑 harmony_toolchain_check）")
	}
	idx, err := harmony.APIIndexLoad(t.tc.DevEcoRoot)
	if err != nil {
		return kernel.ToolResult{}, err
	}
	hits, total := idx.LookupSymbol(symbol, intArg(args, "limit", 10))
	if len(hits) == 0 {
		return kernel.ToolResult{
			Output: fmt.Sprintf("在 SDK 声明里没有找到 %q（索引共 %d 个符号）。\n"+
				"可能原因：该 API 不在本机装的 SDK 版本里；或名字写法不同（可试试更短的关键字做模糊查询）。", symbol, total),
		}, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "SDK 声明索引：%d 个符号（目录 %s）\n\n", total, idx.Dir)
	for _, h := range hits {
		tag := "近似"
		if h.Exact {
			tag = "精确"
		}
		fmt.Fprintf(&sb, "- [%s] %s（%s）%s:%d\n    %s\n", tag, h.Symbol.Name, h.Symbol.Kind, h.Symbol.File, h.Symbol.Line, h.Symbol.Snippet)
	}
	return kernel.ToolResult{Output: strings.TrimRight(sb.String(), "\n")}, nil
}

// ---------------------------------------------------------------- 签名

type signTool struct{ tc *harmony.Toolchain }

func (signTool) Name() string { return "harmony_sign" }

func (signTool) Description() string {
	return "用 SDK 自带的调试身份给 .hap 签名（hap-sign-tool：先签 profile 再签应用）。" +
		"未签名的 hap 装不上设备。hap 留空时取工程里最新的构建产物。会生成新文件，需要审批。"
}

func (signTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "hap": {"type": "string", "description": ".hap 文件路径；留空则取工程构建产物里最新的一个"},
    "projectDir": {"type": "string", "description": "用于定位产物的工程目录，留空用当前工作目录"}
  },
  "additionalProperties": false
}`)
}

func (signTool) NeedsApproval(map[string]any) bool { return true }

func (t signTool) Execute(ctx context.Context, args map[string]any, tcx kernel.ToolCtx) (kernel.ToolResult, error) {
	hap := strArg(args, "hap")
	if hap == "" {
		proj, err := harmony.FindProject(startDir(args, tcx))
		if err != nil {
			return kernel.ToolResult{}, fmt.Errorf("未指定 hap 且无法定位工程：%w", err)
		}
		if hap, err = proj.FindHap(); err != nil {
			return kernel.ToolResult{}, err
		}
	}
	signed, out, err := harmony.SignHap(ctx, t.tc, hap)
	if err != nil {
		return kernel.ToolResult{Output: err.Error() + "\n" + tail(out, 2000), IsError: true}, nil
	}
	return kernel.ToolResult{Output: fmt.Sprintf("签名完成。\n产物：%s\n源：%s", signed, hap)}, nil
}

// ---------------------------------------------------------------- 静态检查

type lintTool struct{ tc *harmony.Toolchain }

func (lintTool) Name() string { return "harmony_lint" }

func (lintTool) Description() string {
	return "用 DevEco 官方的 codelinter 对工程做静态检查（ArkTS 规范与常见错误）。" +
		"改完代码、构建之前跑一遍能提前发现低级错误。只读（不修改任何文件），免审批，但耗时较长。"
}

func (lintTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "projectDir": {"type": "string", "description": "工程目录；留空用当前工作目录向上定位工程根"}
  },
  "additionalProperties": false
}`)
}

func (lintTool) NeedsApproval(map[string]any) bool { return false }

func (t lintTool) Execute(ctx context.Context, args map[string]any, tcx kernel.ToolCtx) (kernel.ToolResult, error) {
	proj, err := harmony.FindProject(startDir(args, tcx))
	if err != nil {
		return kernel.ToolResult{}, err
	}
	res, err := harmony.Lint(ctx, t.tc, proj.Root)
	out := fmt.Sprintf("工程：%s\n退出码：%d\n\n%s", proj.Root, res.ExitCode, tail(res.Output, 8000))
	if err != nil {
		return kernel.ToolResult{Output: out + "\n" + err.Error(), IsError: true}, nil
	}
	if res.ExitCode != 0 {
		return kernel.ToolResult{Output: out, IsError: true}, nil
	}
	return kernel.ToolResult{Output: out}, nil
}

// ---------------------------------------------------------------- 设备冒烟

type deviceTestTool struct{ tc *harmony.Toolchain }

func (deviceTestTool) Name() string { return "harmony_device_test" }

func (deviceTestTool) Description() string {
	return "在设备上跑一遍最小可用验证：装机 → 启动 → 检查日志有无崩溃 → 卸载清理。" +
		"用来确认「这个 hap 能装上、能起来」，是做性能采集前的门禁。会改变设备状态，需要审批。"
}

func (deviceTestTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "hap": {"type": "string", "description": ".hap 路径；留空取工程最新产物（建议先 harmony_sign 签名）"},
    "bundle": {"type": "string", "description": "应用包名；留空从工程配置读取"},
    "ability": {"type": "string", "description": "入口 ability；留空自动解析"},
    "device": {"type": "string", "description": "设备序列号；多设备时必须指定"},
    "projectDir": {"type": "string", "description": "用于定位产物与包名的工程目录"}
  },
  "additionalProperties": false
}`)
}

func (deviceTestTool) NeedsApproval(map[string]any) bool { return true }

func (t deviceTestTool) Execute(ctx context.Context, args map[string]any, tcx kernel.ToolCtx) (kernel.ToolResult, error) {
	hap := strArg(args, "hap")
	bundle := strArg(args, "bundle")
	if hap == "" || bundle == "" {
		proj, err := harmony.FindProject(startDir(args, tcx))
		if err != nil {
			return kernel.ToolResult{}, err
		}
		if hap == "" {
			if hap, err = proj.FindHap(); err != nil {
				return kernel.ToolResult{}, err
			}
		}
		if bundle == "" {
			if bundle, err = harmony.ReadBundleName(proj.Root); err != nil {
				return kernel.ToolResult{}, err
			}
		}
	}

	steps, err := harmony.DeviceSmoke(ctx, t.tc, hap, bundle, strArg(args, "ability"), strArg(args, "device"))
	if err != nil {
		return kernel.ToolResult{Output: err.Error(), IsError: true}, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "冒烟测试：%s\n", hap)
	allPass := true
	for _, s := range steps {
		if !s.Pass {
			allPass = false
		}
		mark := "通过"
		if !s.Pass {
			mark = "失败"
		}
		fmt.Fprintf(&sb, "- %s：%s", s.Step, mark)
		if s.Detail != "" {
			fmt.Fprintf(&sb, "\n    %s", strings.ReplaceAll(s.Detail, "\n", "\n    "))
		}
		sb.WriteString("\n")
	}
	if allPass {
		sb.WriteString("\n结论：可以装机并启动，未发现崩溃迹象（已自动卸载清理）。")
	} else {
		sb.WriteString("\n结论：有步骤未通过，见上。")
	}
	return kernel.ToolResult{Output: strings.TrimRight(sb.String(), "\n"), IsError: !allPass}, nil
}

// ---------------------------------------------------------------- UI 截图

type uiRegressionTool struct{ tc *harmony.Toolchain }

func (uiRegressionTool) Name() string { return "harmony_ui_regression" }

func (uiRegressionTool) Description() string {
	return "在设备上启动应用并截一张屏，保存到本地并返回文件路径。" +
		"注意：本工具**只负责截图**，不做图像判断——要看图里有没有异常，请把返回的图片路径交给支持读图的模型，或自己打开看。" +
		"会启动应用（改变设备状态），需要审批。"
}

func (uiRegressionTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "bundle": {"type": "string", "description": "应用包名；留空从工程配置读取"},
    "ability": {"type": "string", "description": "入口 ability；留空自动解析"},
    "device": {"type": "string", "description": "设备序列号；多设备时必须指定"},
    "outDir": {"type": "string", "description": "截图保存目录；默认 ~/.arkperf/tmp"},
    "projectDir": {"type": "string", "description": "用于读取包名的工程目录"}
  },
  "additionalProperties": false
}`)
}

func (uiRegressionTool) NeedsApproval(map[string]any) bool { return true }

func (t uiRegressionTool) Execute(ctx context.Context, args map[string]any, tcx kernel.ToolCtx) (kernel.ToolResult, error) {
	bundle := strArg(args, "bundle")
	if bundle == "" {
		proj, err := harmony.FindProject(startDir(args, tcx))
		if err != nil {
			return kernel.ToolResult{}, err
		}
		if bundle, err = harmony.ReadBundleName(proj.Root); err != nil {
			return kernel.ToolResult{}, err
		}
	}
	device := strArg(args, "device")

	// 先启动再截：直接截屏截到的是桌面
	res, err := t.tc.StartAbility(ctx, device, bundle, strArg(args, "ability"), true)
	launchOut := tail(res.Output, 800)
	if err != nil {
		return kernel.ToolResult{Output: "启动失败，未截图。\n" + launchOut, IsError: true}, nil
	}

	// 给界面一点时间画出来——立刻截图会拿到启动中的白屏
	waitForUI(ctx)

	local, err := harmony.SnapshotDisplay(ctx, t.tc, device, strArg(args, "outDir"))
	if err != nil {
		return kernel.ToolResult{Output: "已启动 " + bundle + "，但截图失败：" + err.Error() + "\n" + launchOut, IsError: true}, nil
	}
	return kernel.ToolResult{Output: fmt.Sprintf(
		"已启动 %s 并截图。\n图片：%s\n\n（本工具不做图像判断；要看界面是否正常，请打开该图片，或把它交给支持读图的模型。）\n\n启动输出：\n%s",
		bundle, local, launchOut)}, nil
}

// waitForUI 等待界面绘制完成。
//
// 固定等待而不是可配置：截图的目的是"看到应用界面"，
// 等 1.5 秒足以跨过启动白屏，再长也只是拖慢工具。
func waitForUI(ctx context.Context) {
	timer := time.NewTimer(1500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
