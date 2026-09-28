//go:build windows

package execx

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

// 桌面版（-H windowsgui）自己没有控制台，子进程不加这两个标记就会各自弹一个
// 终端窗口——实测启动时连 5 个 MCP 服务器就弹 5 个终端，用户一眼就看见了。
// 这个行为一旦被改回去，问题会原样复现，所以钉在测试里。
func TestHideWindowSuppressesChildConsole(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "echo")
	HideWindow(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr 不应为空")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("应设置 HideWindow（隐藏子进程窗口）")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatal("应设置 CREATE_NO_WINDOW（根本不给子进程创建控制台）")
	}
}

// 已经有 SysProcAttr 时不能把它覆盖掉（其它标记必须保留）。
func TestHideWindowPreservesExistingFlags(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "echo")
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	HideWindow(cmd)

	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatal("既有的 CREATE_NEW_PROCESS_GROUP 被覆盖丢了")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatal("应追加 CREATE_NO_WINDOW")
	}
}
