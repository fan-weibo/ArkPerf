package tools

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// echoCmd 返回一个跨平台可用的"回显"命令。
// 命令本身以数组传入，所以不受 shell 差异影响，只有解释器名字按平台变。
func echoCmd(msg string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd.exe", "/C", "echo " + msg}
	}
	return []string{"sh", "-c", "echo " + msg}
}

func exitCmd(code int) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd.exe", "/C", "exit " + string(rune('0'+code))}
	}
	return []string{"sh", "-c", "exit " + string(rune('0'+code))}
}

func TestRunCommandCapturesOutput(t *testing.T) {
	res, err := runCommandTool{}.Execute(t.Context(), map[string]any{
		"command": echoCmd("hello-arkperf"),
	}, kernel.ToolCtx{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "hello-arkperf") {
		t.Fatalf("输出不对：%q", res.Output)
	}
	if !strings.Contains(res.Output, "退出码：0") {
		t.Fatalf("应报出退出码：%q", res.Output)
	}
}

func TestRunCommandNonZeroExitIsErrorResult(t *testing.T) {
	res, err := runCommandTool{}.Execute(t.Context(), map[string]any{
		"command": exitCmd(3),
	}, kernel.ToolCtx{CWD: t.TempDir()})
	// 命令跑了但失败：交给模型的是 IsError 结果（带输出），而不是 Go 错误
	if err != nil {
		t.Fatalf("失败的命令应折叠成 IsError 结果而不是 Go 错误：%v", err)
	}
	if !res.IsError {
		t.Fatal("非零退出应标记为 IsError")
	}
	if !strings.Contains(res.Output, "退出码：3") {
		t.Fatalf("应报出真实退出码：%q", res.Output)
	}
}

// ArkPerf 不经过 shell：没有管道、重定向、串联。
// 这个限制必须明确告知，而不是让命令莫名其妙地跑错。
func TestRunCommandRejectsShellSyntax(t *testing.T) {
	for _, tok := range []string{"&&", "|", ">", ";"} {
		_, err := runCommandTool{}.Execute(t.Context(), map[string]any{
			"command": []string{"go", "build", "./...", tok, "rm", "-rf", "/"},
		}, kernel.ToolCtx{CWD: t.TempDir()})
		if err == nil {
			t.Fatalf("含有 %q 的命令应被拒绝", tok)
		}
		if !strings.Contains(err.Error(), "不支持 shell 语法") {
			t.Fatalf("应说明原因：%v", err)
		}
	}
}

// 这是本次补齐的核心：破坏性命令**在任何审批状态下都不放行**。
func TestRunCommandDeniesDestructiveEvenWithApproval(t *testing.T) {
	cmd := []string{"rm", "-rf", "/"}

	// 护栏已决定，因此根本不该再弹审批
	if (runCommandTool{}).NeedsApproval(map[string]any{"command": cmd}) {
		t.Fatal("硬拒的命令不该再征求审批")
	}
	_, err := runCommandTool{}.Execute(t.Context(), map[string]any{"command": cmd}, kernel.ToolCtx{CWD: t.TempDir()})
	if err == nil {
		t.Fatal("破坏性命令必须被拒")
	}
	if !errors.Is(err, kernel.ErrDenied) {
		t.Fatalf("应返回 ErrDenied：%v", err)
	}
}

func TestRunCommandApprovalDependsOnCommand(t *testing.T) {
	if (runCommandTool{}).NeedsApproval(map[string]any{"command": []string{"git", "status"}}) {
		t.Fatal("只读裸探测应免审批")
	}
	if !(runCommandTool{}).NeedsApproval(map[string]any{"command": []string{"go", "build", "./..."}}) {
		t.Fatal("非只读命令应审批")
	}
	// 参数有问题的命令不需要审批：直接让 Execute 报错
	if (runCommandTool{}).NeedsApproval(map[string]any{"command": 42}) {
		t.Fatal("畸形参数不该弹审批")
	}
}

func TestRunCommandEmptyIsRejected(t *testing.T) {
	if _, err := (runCommandTool{}).Execute(t.Context(), map[string]any{
		"command": []string{},
	}, kernel.ToolCtx{CWD: t.TempDir()}); err == nil {
		t.Fatal("空命令应报错")
	}
}

func TestRunCommandRunsInGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	res, err := runCommandTool{}.Execute(t.Context(), map[string]any{
		"command": echoCmd("x"),
		"cwd":     dir,
	}, kernel.ToolCtx{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, dir) {
		t.Fatalf("应报出实际工作目录：%q", res.Output)
	}
}

func TestRunCommandTimeoutIsClamped(t *testing.T) {
	// 超过上限的超时会被夹到 20 分钟而不是真的等那么久；
	// 这里只验证"给了超大值也不会立刻报错"，真正的等待逻辑由 execx 覆盖。
	res, err := runCommandTool{}.Execute(t.Context(), map[string]any{
		"command":        echoCmd("fast"),
		"timeoutSeconds": 99999,
	}, kernel.ToolCtx{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "fast") {
		t.Fatalf("命令应正常执行：%q", res.Output)
	}
}

// ---------------------------------------------------------------- 命令切分

func TestSplitCommandArray(t *testing.T) {
	got, err := splitCommand([]any{"git", "commit", "-m", "msg"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "git|commit|-m|msg" {
		t.Fatalf("切分结果：%v", got)
	}

	// 数组里的空项要丢掉：模型有时会塞空字符串
	got, _ = splitCommand([]string{"git", "", "status"})
	if len(got) != 2 {
		t.Fatalf("空项应被丢弃：%v", got)
	}
}

// 路径含空格是 Windows 上的常态（DevEco Studio 就装在带空格的目录里），
// 字符串形式必须能正确处理引号。
func TestTokenizeHandlesQuotedPaths(t *testing.T) {
	got, err := tokenize(`"C:\Program Files\x\y.exe" --flag value`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`C:\Program Files\x\y.exe`, "--flag", "value"}
	if len(got) != len(want) {
		t.Fatalf("切分结果：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项 %q，期望 %q", i, got[i], want[i])
		}
	}

	if _, err := tokenize(`"未闭合`); err == nil {
		t.Fatal("引号不闭合应报错")
	}
}

// 模型经常把数组整段序列化成字符串塞进来（实测撞到过）：
// `["rm","-rf","/"]`。不兜住的话，整串会被当成一个"程序名字"去执行，
// 既跑不出结果，也让安全护栏失效（它认不出这是个删除命令）。
func TestSplitCommandAcceptsJSONArrayAsString(t *testing.T) {
	got, err := splitCommand(`["rm","-rf","/"]`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "rm -rf /" {
		t.Fatalf("应还原成 argv：%v", got)
	}

	// 还原之后，护栏必须认出它是破坏性命令
	res, err := (runCommandTool{}).Execute(t.Context(), map[string]any{
		"command": `["rm","-rf","/"]`,
	}, kernel.ToolCtx{CWD: t.TempDir()})
	if err == nil {
		t.Fatal("字符串化数组也该被护栏拦住")
	}
	if !errors.Is(err, kernel.ErrDenied) {
		t.Fatalf("应返回 ErrDenied：%v", err)
	}
	if res.IsError {
		t.Fatal("护栏拒绝应作为 Go 错误返回，而不是 IsError 结果")
	}
}

// 进程没启动时不能报"退出码：0"——那是零值假象，
// 实测把模型都搞糊涂了（ERROR 标签 + 退出码 0 自相矛盾）。
func TestRunCommandDistinguishesNeverStarted(t *testing.T) {
	// 一个绝对不存在的程序
	res, err := (runCommandTool{}).Execute(t.Context(), map[string]any{
		"command": []string{"arkperf-no-such-program-xyz", "--flag"},
	}, kernel.ToolCtx{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("启动失败应标记 IsError")
	}
	if strings.Contains(res.Output, "退出码：0") {
		t.Fatalf("不该出现假的退出码 0：%q", res.Output)
	}
	if !strings.Contains(res.Output, "进程未启动") {
		t.Fatalf("应说明进程没起来：%q", res.Output)
	}
	// 失败原因必须给出来，否则模型只能瞎猜
	if !strings.Contains(res.Output, "失败原因") {
		t.Fatalf("应给出失败原因：%q", res.Output)
	}
}

func TestSplitCommandRejectsBadTypes(t *testing.T) {
	if _, err := splitCommand([]any{"git", 42}); err == nil {
		t.Fatal("非字符串元素应报错")
	}
	if _, err := splitCommand(42); err == nil {
		t.Fatal("数字应报错")
	}
	if _, err := splitCommand(nil); err == nil {
		t.Fatal("nil 应报错")
	}
}

func TestIntArgToleratesShapes(t *testing.T) {
	if intArg(map[string]any{"n": 5}, "n", 0) != 5 {
		t.Fatal("int 失败")
	}
	if intArg(map[string]any{"n": int64(7)}, "n", 0) != 7 {
		t.Fatal("int64 失败")
	}
	if intArg(map[string]any{"n": float64(9)}, "n", 0) != 9 {
		t.Fatal("float64 失败")
	}
	if intArg(map[string]any{"n": " 12 "}, "n", 0) != 12 {
		t.Fatal("字符串失败")
	}
	if intArg(map[string]any{}, "n", 3) != 3 {
		t.Fatal("默认值失败")
	}
}

func TestRunTimeoutConstantsAreSane(t *testing.T) {
	if defaultRunTimeout <= 0 || defaultRunTimeout > maxRunTimeout {
		t.Fatal("默认超时应在上限之内")
	}
	if maxRunTimeout != 20*time.Minute {
		t.Fatalf("上限应为 20 分钟：%v", maxRunTimeout)
	}
}

// 编译期确认：不经过 shell 的入口只有一个（execx.Run），
// 若以后有人加入 shell 路径，这里会提醒先想清楚安全后果。
func TestNoShellInterpreterInToolPath(t *testing.T) {
	src, err := os.ReadFile("exec.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"sh -c", "cmd.exe /C", "/bin/sh"} {
		if strings.Contains(string(src), banned) {
			t.Fatalf("exec.go 不应硬编码 shell 调用：%q", banned)
		}
	}
}
