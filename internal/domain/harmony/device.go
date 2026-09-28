package harmony

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fan-weibo/ArkPerf/internal/execx"
)

// 各类设备操作的默认超时。装机明显慢于查询，不能共用一个值。
const (
	timeoutList    = 30 * time.Second
	timeoutShell   = 60 * time.Second
	timeoutInstall = 300 * time.Second
	timeoutLaunch  = 60 * time.Second
)

// Device 是 hdc 报告的一个设备。
type Device struct {
	// Serial 是 hdc 的 connect key，如 127.0.0.1:5555。
	Serial string
}

// ListDevices 返回当前连接的设备。
//
// "没有设备"是合法结果而不是错误：把空列表报成失败，上层就没法区分
// "模拟器没开"和"hdc 坏了"——而这两件事的处置完全不同。
func (tc *Toolchain) ListDevices(ctx context.Context) ([]Device, error) {
	hdc := tc.Required(ToolHDC)
	if !hdc.Found() {
		return nil, fmt.Errorf("找不到 hdc，无法查询设备。%s", hdc.Hint)
	}

	res, err := execx.Run(ctx, hdc.Path, []string{"list", "targets"}, execx.Options{Timeout: timeoutList})
	if err != nil {
		return nil, err
	}
	return parseTargets(res.Output), nil
}

// parseTargets 解析 `hdc list targets` 的输出。
//
// 实测：无设备时输出恰好是一行 "[Empty]"。把方括号开头的行当作状态提示
// 而不是设备名——否则会凭空多出一个叫 "[Empty]" 的设备，然后所有后续
// 操作都对着它失败。
func parseTargets(out string) []Device {
	var devices []Device
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}
		devices = append(devices, Device{Serial: line})
	}
	return devices
}

// Shell 在设备上执行命令。
func (tc *Toolchain) Shell(ctx context.Context, serial string, command ...string) (execx.Result, error) {
	return tc.run(ctx, timeoutShell, serial, append([]string{"shell"}, command...)...)
}

// Install 安装一个包（.hap / .hsp / .app）。
//
// -r 允许覆盖安装：复测时同一个包名必须能装第二次，否则每轮优化前
// 都得先手工卸载一次。
func (tc *Toolchain) Install(ctx context.Context, serial, packagePath string) (execx.Result, error) {
	return tc.run(ctx, timeoutInstall, serial, "install", "-r", packagePath)
}

// Uninstall 按包名卸载应用。
func (tc *Toolchain) Uninstall(ctx context.Context, serial, bundle string) (execx.Result, error) {
	return tc.run(ctx, timeoutShell, serial, "uninstall", bundle)
}

// StartAbility 启动应用的指定 Ability。
//
// **必须带 -a**。实测只给 -b 会报
//
//	Error Code:10103101 Failed to find a matching application for implicit launch
//
// 即使该包确实装着、也确实出现在 bm dump 里。ability 留空时先解析再启动。
//
// wait=true 会附加 -W，让 aa 等启动完成并回报 WaitTime / TotalTime——
// 对性能产品这是白拿的启动耗时数据，而且这正是已跑通的实现用的形式。
func (tc *Toolchain) StartAbility(ctx context.Context, serial, bundle, ability string, wait bool) (execx.Result, error) {
	bundle = strings.TrimSpace(bundle)
	if bundle == "" {
		return execx.Result{}, errors.New("bundle（应用包名）不能为空")
	}
	ability = strings.TrimSpace(ability)
	if ability == "" {
		ability = tc.ResolveAbility(ctx, serial, bundle)
	}

	args := []string{"shell", "aa", "start", "-a", ability, "-b", bundle}
	if wait {
		args = append(args, "-W")
	}
	return tc.run(ctx, timeoutLaunch, serial, args...)
}

// DefaultAbility 是解析不出入口 Ability 时使用的名字。
//
// 这是生态惯例（DevEco 新建工程的入口 Ability 就叫这个），
// 已跑通的实现也是这么兜底的。
const DefaultAbility = "EntryAbility"

var (
	mainAbilityPattern = regexp.MustCompile(`"mainAbility"\s*:\s*"([^"]+)"`)
	waitTimePattern    = regexp.MustCompile(`WaitTime:\s*(\d+)`)
	totalTimePattern   = regexp.MustCompile(`TotalTime:\s*(\d+)`)
)

// ResolveAbility 从设备上的包信息里取入口 Ability 名。
//
// 取不到就退回 DefaultAbility —— 不去猜别的：猜错的名字会让启动失败，
// 而失败信息（10103101）根本不会告诉你是名字错了。
func (tc *Toolchain) ResolveAbility(ctx context.Context, serial, bundle string) string {
	if strings.TrimSpace(bundle) == "" {
		return DefaultAbility
	}
	res, err := tc.run(ctx, timeoutShell, serial, "shell", "bm", "dump", "-n", bundle)
	if err == nil {
		if a := parseMainAbility(res.Output); a != "" {
			return a
		}
	}
	return DefaultAbility
}

func parseMainAbility(out string) string {
	if m := mainAbilityPattern.FindStringSubmatch(out); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// StartTimings 是 aa start -W 回报的启动耗时（毫秒）。
type StartTimings struct {
	WaitMs  int
	TotalMs int
	// Mode 是 StartMode（Cold / Hot），无法解析时为空。
	Mode string
}

// ParseStartTimings 解析 `aa start -W` 的输出。
//
// 实测输出形如：
//
//	StartMode: Cold
//	BundleName: com.example.perflab
//	AbilityName: EntryAbility
//	TotalTime: 1163
//	WaitTime: 1175
//	start ability successfully.
func ParseStartTimings(out string) (StartTimings, bool) {
	var t StartTimings
	if m := waitTimePattern.FindStringSubmatch(out); len(m) == 2 {
		t.WaitMs, _ = strconv.Atoi(m[1])
	}
	if m := totalTimePattern.FindStringSubmatch(out); len(m) == 2 {
		t.TotalMs, _ = strconv.Atoi(m[1])
	}
	if m := startModePattern.FindStringSubmatch(out); len(m) == 2 {
		t.Mode = m[1]
	}
	return t, t.WaitMs > 0 || t.TotalMs > 0
}

var startModePattern = regexp.MustCompile(`StartMode:\s*(\w+)`)

// Logs 读取 hilog。
//
// 固定带 -x：那是"读完缓冲区就退出"。不加的话 hilog 会一直挂着刷日志，
// 命令永远不返回，只能等超时被杀死——拿到的还是空输出。
func (tc *Toolchain) Logs(ctx context.Context, serial string, extra ...string) (execx.Result, error) {
	args := append([]string{"shell", "hilog", "-x"}, extra...)
	return tc.run(ctx, timeoutShell, serial, args...)
}

// run 是 hdc 调用的统一出口。
func (tc *Toolchain) run(ctx context.Context, timeout time.Duration, serial string, args ...string) (execx.Result, error) {
	hdc := tc.Required(ToolHDC)
	if !hdc.Found() {
		return execx.Result{}, fmt.Errorf("找不到 hdc，无法操作设备。%s", hdc.Hint)
	}
	return execx.Run(ctx, hdc.Path, tc.hdcArgs(serial, args...), execx.Options{Timeout: timeout})
}

// hdcArgs 在命令前插入 -t 以选定设备。
//
// 不指定 serial 时交给 hdc 自己决定（单设备时它工作正常）；多设备下
// hdc 会直接报错——那正是我们想要的行为，总好过随机挑一个测错对象。
func (tc *Toolchain) hdcArgs(serial string, args ...string) []string {
	if strings.TrimSpace(serial) == "" {
		return args
	}
	return append([]string{"-t", serial}, args...)
}
