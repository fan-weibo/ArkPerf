//go:build windows

package execx

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// HideWindow 让子进程不弹出控制台窗口。
//
// 桌面版是用 `-H windowsgui` 构建的——它**自己没有控制台**。这种进程再启动任何
// 控制台程序（python / hdc / hvigorw / java …）时，Windows 默认会给**每个子进程
// 新建一个控制台窗口**，表现是"每跑一次就弹出一堆终端窗口"（启动时连 5 个 MCP
// 服务器 → 就弹 5 个）。
//
// TUI 模式下看不到这个问题：那时父进程自己就在控制台里，子进程直接复用父控制台。
//
// 两个标记一起用是有意的：
//   - CREATE_NO_WINDOW：根本不为子进程创建控制台（连一闪而过的黑框都没有）
//   - HideWindow：兜底隐藏窗口（万一某条路径仍创建了窗口）
//
// 对 MCP 服务器（stdio 管道通信）与 hdc/hvigorw（输出走管道捕获）都没有副作用：
// 它们本来就不需要真正的控制台。
func HideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
