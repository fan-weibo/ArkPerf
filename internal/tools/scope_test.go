package tools

import (
	"path/filepath"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

func scopeOf(t *testing.T, tool kernel.Tool, args map[string]any, cwd string) string {
	t.Helper()
	return kernel.ApprovalScope(tool, args, cwd)
}

// ---------------------------------------------------------------- 路径类

// 归到**目录**而不是整文件：记一条"允许改 a.go"，下次模型改 b.go 还得再问；
// 而"允许改这个目录"才是用户点那一项时真正的意思。
func TestPathScopeIsTheDirectory(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(cwd, "src", "main")
	want := filepath.Dir(filepath.Join(cwd, "src", "main", "a.ets"))

	got := scopeOf(t, editFileTool{}, map[string]any{"path": filepath.Join(dir, "a.ets")}, cwd)
	if got != want {
		t.Fatalf("类别 = %q，期望 %q", got, want)
	}

	// 同一目录下的另一个文件应当得到同一个类别
	other := scopeOf(t, editFileTool{}, map[string]any{"path": filepath.Join(dir, "b.ets")}, cwd)
	if other != got {
		t.Fatalf("同目录应当同类：%q vs %q", other, got)
	}

	// 换目录就必须换类别
	elsewhere := scopeOf(t, editFileTool{}, map[string]any{"path": filepath.Join(cwd, "other", "c.ets")}, cwd)
	if elsewhere == got {
		t.Fatalf("不同目录不该同类：%q", elsewhere)
	}
}

// 相对路径必须按**工作目录**解析。用进程 CWD 的话，同一份规则在
// 换个目录启动后会算出另一个类别，表现为"规则时灵时不灵"。
func TestPathScopeResolvesRelativeAgainstCWD(t *testing.T) {
	cwd := t.TempDir()

	got := scopeOf(t, writeFileTool{}, map[string]any{"path": filepath.Join("sub", "a.txt")}, cwd)
	if got != filepath.Join(cwd, "sub") {
		t.Fatalf("相对路径应当按 cwd 解析：%q", got)
	}
}

func TestPathScopeEmptyWhenPathMissing(t *testing.T) {
	if got := scopeOf(t, editFileTool{}, map[string]any{}, t.TempDir()); got != "" {
		t.Fatalf("没给路径时不该有类别：%q", got)
	}
}

// ---------------------------------------------------------------- 命令类

// 带上第一个参数是必要的：`git status` 只读可以记，
// 而 `git push` / `git reset` 绝不该被同一条规则放过。
func TestCommandScopeDistinguishesSubcommands(t *testing.T) {
	status := scopeOf(t, runCommandTool{}, map[string]any{"command": []any{"git", "status"}}, "")
	push := scopeOf(t, runCommandTool{}, map[string]any{"command": []any{"git", "push"}}, "")

	if status != "git status" {
		t.Fatalf("类别 = %q，期望 git status", status)
	}
	if push != "git push" {
		t.Fatalf("类别 = %q，期望 git push", push)
	}
	if status == push {
		t.Fatal("status 与 push 必须是两个类别")
	}
}

func TestCommandScopeIgnoresLeadingFlags(t *testing.T) {
	// 第一个参数是选项时，类别只到程序名
	if got := scopeOf(t, runCommandTool{}, map[string]any{"command": []any{"rg", "-n", "TODO"}}, ""); got != "rg" {
		t.Fatalf("类别 = %q，期望 rg", got)
	}
	// 单元素命令也要有类别
	if got := scopeOf(t, runCommandTool{}, map[string]any{"command": []any{"git", "status", "--short"}}, ""); got != "git status" {
		t.Fatalf("类别 = %q，期望 git status", got)
	}
}

func TestCommandScopeUsesBaseName(t *testing.T) {
	// 同一程序的不同写法应当归成同一类，否则规则会因为路径写法不同而失效
	a := scopeOf(t, runCommandTool{}, map[string]any{"command": []any{"git", "status"}}, "")
	b := scopeOf(t, runCommandTool{}, map[string]any{"command": []any{`C:\Program Files\Git\cmd\git.exe`, "status"}}, "")
	if a != b {
		t.Fatalf("应当归成同一类：%q vs %q", a, b)
	}
}

func TestShellToolScopeFromDeviceCommand(t *testing.T) {
	got := scopeOf(t, shellTool{}, map[string]any{"command": []any{"hidumper", "--mem"}}, "")
	if got != "hidumper" {
		t.Fatalf("类别 = %q，期望 hidumper", got)
	}
}

// ---------------------------------------------------------------- 不该有类别的

// 静态审批的域工具刻意不给类别：它们每次的差别在工程状态而不在参数，
// 划不出一个安全的类别。用户只能一次次点同意——这是有意的取舍。
func TestStaticApprovalToolsHaveNoScope(t *testing.T) {
	for _, tool := range Harmony(nil) {
		if got := kernel.ApprovalScope(tool, map[string]any{}, t.TempDir()); got != "" {
			t.Fatalf("域工具 %s 不该有类别：%q", tool.Name(), got)
		}
	}
}

// ---------------------------------------------------------------- 红线

// 被硬拒的命令**不进入审批路径**（NeedsApproval 恒为 false），
// 所以审批规则也放行不了它们——规则只减少询问，不放宽任何红线。
func TestHardDeniedCommandsNeverReachApproval(t *testing.T) {
	cases := []struct {
		argv   []any
		joined string
	}{
		{[]any{"rm", "-rf", "/"}, "rm -rf /"},
		{[]any{"format", "C:"}, "format C:"},
	}
	for _, tc := range cases {
		if (runCommandTool{}).NeedsApproval(map[string]any{"command": tc.argv}) {
			t.Fatalf("%v 被硬拒，不该进入审批", tc.argv)
		}
		if err := kernel.CheckCommand(tc.joined); err == nil {
			t.Fatalf("%q 应当被硬拒", tc.joined)
		}
	}
	// 普通写类命令仍然要审批——否则上面两条的断言可能只是"恒为 false"的假象
	if !(runCommandTool{}).NeedsApproval(map[string]any{"command": []any{"git", "push"}}) {
		t.Fatal("普通写类命令应当需要审批")
	}
}

// 设备命令同样受红线约束：抹掉设备系统分区与抹掉宿主一样不可逆。
func TestHardDeniedDeviceCommandNeverReachesApproval(t *testing.T) {
	if (shellTool{}).NeedsApproval(map[string]any{"command": []any{"rm", "-rf", "/data"}}) {
		t.Fatal("被硬拒的设备命令不该进入审批")
	}
	if err := kernel.CheckCommand("rm -rf /data"); err == nil {
		t.Fatal("抹除设备系统分区应当被硬拒")
	}
}
