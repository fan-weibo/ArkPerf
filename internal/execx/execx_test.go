package execx

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess 是一个"子进程替身"：它被本文件的其它测试以子进程方式启动，
// 用来产生可预期的输出/耗时/退出码。
//
// 用测试二进制自身当替身，是为了不依赖 sleep / ping / timeout 这些
// 各平台写法不同的工具——那些差异会把测试变成"平台赌博"。
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("ARKPERF_EXECX_HELPER")
	if mode == "" {
		return // 正常跑测试：这个用例本身什么都不做
	}
	switch mode {
	case "sleep":
		time.Sleep(10 * time.Second)
	case "spam":
		// 写 8KB 可辨识内容：头尾用不同字符，便于断言"掐中间"
		os.Stdout.WriteString(strings.Repeat("H", 4000))
		os.Stdout.WriteString(strings.Repeat("T", 4000))
	case "noisy":
		os.Stdout.WriteString("Picked up JAVA_TOOL_OPTIONS: -Dfile.encoding=UTF-8\nreal output\n")
	case "exit3":
		os.Exit(3)
	}
	os.Exit(0)
}

// helperCmd 返回"以子进程方式重新运行本测试二进制"的命令行。
// 具体行为由 ARKPERF_EXECX_HELPER 环境变量决定（见 helperOpts）。
// 参数刻意留空占位，避免调用点看起来像在传行为。
func helperCmd(_ string) (string, []string) {
	return os.Args[0], []string{"-test.run=TestHelperProcess"}
}

func helperOpts(mode string, opts Options) Options {
	if opts.Env == nil {
		opts.Env = map[string]string{}
	}
	opts.Env["ARKPERF_EXECX_HELPER"] = mode
	return opts
}

func TestRunCapturesOutput(t *testing.T) {
	name, args := helperCmd("")
	res, err := Run(t.Context(), name, args, helperOpts("", Options{}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.OK() {
		t.Fatalf("expected success: %+v", res)
	}
	if res.Duration <= 0 {
		t.Fatal("duration must be measured")
	}
	if !strings.Contains(res.Command, "TestHelperProcess") {
		t.Fatalf("command should be recorded: %q", res.Command)
	}
}

// 非零退出必须算失败。把 exit 0 之外的返回当成功，是这类封装最典型的坑。
func TestRunNonZeroExitIsAnError(t *testing.T) {
	name, args := helperCmd("exit3")
	res, err := Run(t.Context(), name, args, helperOpts("exit3", Options{}))
	if err == nil {
		t.Fatal("non-zero exit must return an error")
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit code: %d", res.ExitCode)
	}
	if !strings.Contains(err.Error(), "退出码 3") {
		t.Fatalf("error must state the exit code: %v", err)
	}
	if res.OK() {
		t.Fatal("OK() must be false for a failed command")
	}
}

func TestRunTimeout(t *testing.T) {
	name, args := helperCmd("sleep")
	start := time.Now()
	res, err := Run(t.Context(), name, args, helperOpts("sleep", Options{Timeout: 300 * time.Millisecond}))
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
	if !res.TimedOut {
		t.Fatal("TimedOut flag must be set")
	}
	if !strings.Contains(err.Error(), "命令超时") {
		t.Fatalf("error must explain the timeout: %v", err)
	}
	// 不能傻等子进程睡完
	if elapsed > 5*time.Second {
		t.Fatalf("timeout did not interrupt promptly: %v", elapsed)
	}
}

func TestRunStartFailureIsDistinguishable(t *testing.T) {
	res, err := Run(t.Context(), "definitely-not-a-real-binary-arkperf", nil, Options{})
	if !errors.Is(err, ErrStart) {
		t.Fatalf("expected ErrStart, got %v", err)
	}
	if res.OK() {
		t.Fatal("a command that never ran cannot be OK")
	}
}

func TestRunEmptyCommandName(t *testing.T) {
	if _, err := Run(t.Context(), "  ", nil, Options{}); !errors.Is(err, ErrStart) {
		t.Fatalf("empty name must be rejected: %v", err)
	}
}

// 长输出必须掐中间而不是砍尾巴：构建日志的失败原因在末尾。
func TestRunTruncatesMiddleKeepingBothEnds(t *testing.T) {
	name, args := helperCmd("spam")
	res, err := Run(t.Context(), name, args, helperOpts("spam", Options{MaxOutput: 1000}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Truncated {
		t.Fatal("Truncated flag must be set")
	}
	if !strings.Contains(res.Output, "中间部分已省略") {
		t.Fatalf("truncation must be labelled: %q", first(res.Output, 80))
	}
	if !strings.Contains(res.Output, "HHHH") {
		t.Fatal("head must be kept")
	}
	if !strings.Contains(res.Output, "TTTT") {
		t.Fatal("tail must be kept")
	}
	if len(res.Output) > 1000+len(TruncationMarker)+50 {
		t.Fatalf("output not bounded: %d bytes", len(res.Output))
	}
}

// Java 工具链被钉了 JAVA_TOOL_OPTIONS 后会固定打一行噪声，必须过滤掉，
// 否则每次构建输出都会带着它，混进给模型的上下文里。
func TestCleanStripsJavaToolOptionsNoise(t *testing.T) {
	got := clean("Picked up JAVA_TOOL_OPTIONS: -Dfile.encoding=UTF-8\nreal output\n")
	if strings.Contains(got, "Picked up") {
		t.Fatalf("noise not stripped: %q", got)
	}
	if got != "real output" {
		t.Fatalf("real output lost: %q", got)
	}
}

func TestCleanNormalizesNewlinesAndHandlesNonUTF8(t *testing.T) {
	if got := clean("a\r\nb\r\n"); got != "a\nb" {
		t.Fatalf("CRLF not normalized: %q", got)
	}
	// 0xFF 不是合法 UTF-8 起点
	got := clean("正常" + string([]byte{0xFF, 0xFE}) + "尾")
	if !strings.Contains(got, "不是合法 UTF-8") {
		t.Fatalf("non-UTF8 output must be labelled, not silently passed on: %q", got)
	}
}

// hvigor 即使输出被管道接走也照样打颜色码（实测），必须清掉：
// 这些字节进入模型上下文既占 token 又让日志难读。
func TestCleanStripsANSIColors(t *testing.T) {
	raw := "> hvigor \x1b[32mFinished :entry:assembleHap\x1b[39m\n" +
		"> hvigor \x1b[31mERROR: 找不到模块\x1b[39m"

	got := clean(raw)
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("escape sequences survived: %q", got)
	}
	for _, want := range []string{"Finished :entry:assembleHap", "ERROR: 找不到模块"} {
		if !strings.Contains(got, want) {
			t.Fatalf("text lost (%q): %q", want, got)
		}
	}
}

func TestMergeAttachesStderrOnlyWhenPresent(t *testing.T) {
	if got := merge("out", ""); got != "out" {
		t.Fatalf("merge: %q", got)
	}
	if got := merge("", "err"); got != "err" {
		t.Fatalf("merge: %q", got)
	}
	if got := merge("out", "err"); !strings.Contains(got, "--- stderr ---") {
		t.Fatalf("stderr must be labelled: %q", got)
	}
}

func TestDisplayQuotesArgumentsWithSpaces(t *testing.T) {
	got := Display("hdc", []string{"-t", `D:\My Apps\a.hap`})
	if !strings.HasSuffix(got, `"D:\My Apps\a.hap"`) {
		t.Fatalf("display: %q", got)
	}
}

// Windows 不能直接执行 .bat，必须包 cmd.exe；DevEco 的 hvigorw/ohpm 都是 .bat。
func TestCommandLineWrapsBatchFiles(t *testing.T) {
	name, args := commandLine(`C:\deveco\tools\hvigor\bin\hvigorw.bat`, []string{"assembleHap"})
	if runtime.GOOS != "windows" {
		if name != `C:\deveco\tools\hvigor\bin\hvigorw.bat` {
			t.Fatalf("non-windows must pass through: %q", name)
		}
		return
	}
	if !strings.EqualFold(name, "cmd.exe") {
		t.Fatalf("batch file must be wrapped in cmd.exe, got %q", name)
	}
	if len(args) != 3 || args[0] != "/C" || args[2] != "assembleHap" {
		t.Fatalf("wrapped args wrong: %v", args)
	}
}

func TestCapBufferKeepsHeadAndTail(t *testing.T) {
	var b capBuffer
	b.max = 10
	if _, err := b.Write([]byte("0123456789ABCDEF")); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if !strings.HasPrefix(got, "01234") {
		t.Fatalf("head lost: %q", got)
	}
	if !strings.HasSuffix(got, "BCDEF") {
		t.Fatalf("tail lost: %q", got)
	}
	if !b.truncated {
		t.Fatal("truncated flag not set")
	}
}

func TestCapBufferUnderLimitIsVerbatim(t *testing.T) {
	var b capBuffer
	b.max = 100
	b.Write([]byte("hello"))
	if got := b.String(); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if b.truncated {
		t.Fatal("should not be marked truncated")
	}
}

func TestRunHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	name, args := helperCmd("sleep")
	res, err := Run(ctx, name, args, helperOpts("sleep", Options{Timeout: 30 * time.Second}))
	if err == nil {
		t.Fatal("cancelled context must abort")
	}
	if !res.TimedOut {
		// 上游取消与自身超时都走同一条终止路径，行为一致即可
		t.Logf("cancelled without TimedOut flag: %v", err)
	}
}

func first(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
