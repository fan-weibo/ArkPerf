package harmony

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 冒烟测试各步的超时。装机最慢（要把 hap 传进设备并安装）。
const (
	timeoutSmokeInstall   = 300 * time.Second
	timeoutSmokeLaunch    = 60 * time.Second
	timeoutSmokeLogWindow = 10 * time.Second
)

// SmokeStep 是冒烟测试的一个步骤结果。
type SmokeStep struct {
	Step   string
	Pass   bool
	Detail string
}

// DeviceSmoke 在设备上跑一遍最小可用验证：装机 → 启动 → 看日志 → 卸载。
//
// 为什么最后要卸载：这是"验证"而不是"部署"，留一个装上的应用会污染
// 后续的冷启动测量（冷启动必须是首次安装后的首次启动）。
//
// ability 留空时会解析入口 ability；bundle 留空时从工程配置读包名。
func DeviceSmoke(ctx context.Context, tc *Toolchain, hap, bundle, ability, device string) ([]SmokeStep, error) {
	if tc == nil {
		return nil, fmt.Errorf("工具链未探测，无法做设备冒烟测试")
	}
	if hap == "" {
		return nil, fmt.Errorf("没有指定 hap：先构建（harmony_build），或显式给 hap 路径")
	}
	hap, err := filepath.Abs(hap)
	if err != nil {
		return nil, err
	}
	if _, serr := os.Stat(hap); serr != nil {
		return nil, fmt.Errorf("hap 不存在：%s", hap)
	}

	var steps []SmokeStep

	// ---- 1. 装机 ----
	// 直接用 Install（-r 覆盖安装）：复测时不该要求先手工卸载
	res, err := tc.Install(ctx, device, hap)
	steps = append(steps, SmokeStep{
		Step:   "install",
		Pass:   err == nil && !looksFailed(res.Output),
		Detail: clip(strings.TrimSpace(res.Output), 600),
	})
	if err != nil || !steps[0].Pass {
		// 装机失败就别往下走了：启动一个没装上的应用只会得到一串无关错误
		return steps, nil
	}

	// ---- 2. 启动 ----
	res, err = tc.StartAbility(ctx, device, bundle, ability, true)
	launchOK := err == nil && strings.Contains(strings.ToLower(res.Output), "successfully")
	detail := clip(strings.TrimSpace(res.Output), 600)
	if timings, ok := ParseStartTimings(res.Output); ok {
		detail += fmt.Sprintf("\n启动耗时：TotalTime=%dms WaitTime=%dms", timings.TotalMs, timings.WaitMs)
	}
	steps = append(steps, SmokeStep{Step: "launch", Pass: launchOK, Detail: detail})

	// ---- 3. 日志里找崩溃迹象 ----
	logRes, logErr := tc.Logs(ctx, device)
	step := SmokeStep{Step: "logs", Pass: true, Detail: clip(strings.TrimSpace(logRes.Output), 600)}
	if logErr == nil {
		if fatal := firstFatalLine(logRes.Output); fatal != "" {
			step.Pass = false
			step.Detail = "发现崩溃/致命日志：" + fatal
		}
	} else {
		step.Detail = "读取日志失败：" + logErr.Error()
	}
	steps = append(steps, step)

	// ---- 4. 清理 ----
	if bundle != "" {
		if _, uerr := tc.Uninstall(ctx, device, bundle); uerr == nil {
			steps = append(steps, SmokeStep{Step: "uninstall", Pass: true, Detail: "已卸载 " + bundle})
		} else {
			steps = append(steps, SmokeStep{Step: "uninstall", Pass: false,
				Detail: "清理卸载失败（不影响结论，但下次装机前需手动卸载）：" + uerr.Error()})
		}
	}
	return steps, nil
}

// looksFailed 判断 hdc 输出里是否有失败标志。
//
// hdc 的 install 即使失败也可能返回 0，只能看输出里的关键字——
// 这是实测行为，不是猜测。
func looksFailed(out string) bool {
	lower := strings.ToLower(out)
	for _, marker := range []string{"fail", "error", "not found", "no such"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// firstFatalLine 从日志里找出第一条致命行。
func firstFatalLine(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		upper := strings.ToUpper(line)
		if strings.Contains(upper, "FATAL") || strings.Contains(upper, "CRASH") ||
			strings.Contains(upper, "JSAPP") && strings.Contains(upper, "ERROR") ||
			strings.Contains(upper, "ABORT") {
			return clip(line, 300)
		}
	}
	return ""
}

// ---------------------------------------------------------------- 截图

// SnapshotDisplay 从设备截一张屏，落到本地文件，返回本地路径。
//
// 用 snapshot_display 而不是 uitest 的录屏：前者一次调用就出图，
// 后者要启动录制再取输出，步骤多一倍、失败点也多一倍。
//
// 截图文件小于 1KB 视为失败（正常截图至少几十 KB）：hdc 在出错时
// 也会"成功"创建一个空文件，只看命令返回值会拿到一张假图。
func SnapshotDisplay(ctx context.Context, tc *Toolchain, device, outDir string) (string, error) {
	if tc == nil {
		return "", fmt.Errorf("工具链未探测，无法截图")
	}
	remote := "/data/local/tmp/arkperf-shot.jpeg"
	if _, err := tc.Shell(ctx, device, "snapshot_display", "-f", remote); err != nil {
		return "", fmt.Errorf("设备截图失败：%w", err)
	}
	if outDir == "" {
		outDir = filepath.Join(IndexDir(), "tmp")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	local := filepath.Join(outDir, "ui-"+time.Now().Format("20060102-150405")+".jpeg")

	res, err := tc.run(ctx, timeoutShell, device, "file", "recv", remote, local)
	if err != nil {
		return "", fmt.Errorf("从设备取回截图失败：%w", err)
	}
	_ = res

	info, serr := os.Stat(local)
	if serr != nil {
		return "", fmt.Errorf("截图已取回但本地文件不存在：%s", local)
	}
	if info.Size() <= 1000 {
		_ = os.Remove(local)
		return "", fmt.Errorf("截图只有 %d 字节，判定为失败（设备可能未解锁或屏幕未点亮）", info.Size())
	}
	return local, nil
}
