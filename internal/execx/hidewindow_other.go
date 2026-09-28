//go:build !windows

package execx

import "os/exec"

// HideWindow 在非 Windows 平台上是空操作。
//
// 保留这个空实现是为了让整个模块在 Linux/macOS 上照样能编译——
// 本项目的桌面外壳只在 Windows 上出了"子进程弹控制台"这个问题。
func HideWindow(_ *exec.Cmd) {}
