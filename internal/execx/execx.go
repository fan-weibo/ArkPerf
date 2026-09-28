// Package execx 是统一的子进程执行封装：超时、输出截断、Windows 批处理包装、错误分类。
//
// 所有对外部工具（hdc / hvigorw / ohpm）的调用都必须走这里——不是因为好看，
// 而是因为"命令跑了但输出被截断 / 编码坏了 / 超时被杀"这三件事，如果每个调用点
// 各自处理，迟早会漏一个，然后在很久之后以"数据莫名其妙不对"的形式复现。
package execx

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	// ErrTimeout 表示命令在超时时间内没有结束，已被终止。
	ErrTimeout = errors.New("命令超时")
	// ErrStart 表示命令根本没能启动（路径不对、不是可执行文件、权限不足）。
	ErrStart = errors.New("命令无法启动")
)

const (
	// DefaultTimeout 是默认超时。设备查询这类操作很快，但 hdc 首次连接
	// 可能需要拉起 server 进程，给宽一点。
	DefaultTimeout = 60 * time.Second
	// DefaultMaxOutput 是单条命令保留的输出上限（字节）。
	DefaultMaxOutput = 256 * 1024
)

// Options 控制单次执行。
type Options struct {
	// Timeout 覆盖默认超时。
	Timeout time.Duration
	// Dir 是子进程工作目录。
	Dir string
	// Env 是追加（而非替换）到继承环境上的变量。
	Env map[string]string
	// MaxOutput 覆盖输出上限。
	MaxOutput int
}

// Result 是一次执行的完整结果。即使返回了 error，Result 里也带着已经拿到的
// 部分输出——排查失败时那半截输出往往就是全部线索。
type Result struct {
	// Command 是可读的命令行（仅用于展示与报错）。
	Command string
	Stdout  string
	Stderr  string
	// Output 是合并后的可读输出：stdout，必要时附 stderr。
	Output    string
	ExitCode  int
	Duration  time.Duration
	TimedOut  bool
	Truncated bool
	// Started 表示进程确实被创建过。
	//
	// 必须有这个字段：命令根本没能启动时 ExitCode 的零值就是 0，
	// 只看退出码会把"从未运行"误判成"运行成功"——这是这类封装里
	// 最隐蔽也最危险的一种假成功。
	Started bool
}

// OK 表示命令确实跑起来了、正常结束、且退出码为 0。
func (r Result) OK() bool { return r.Started && r.ExitCode == 0 && !r.TimedOut }

// Run 执行命令。
//
// 返回值约定：error 非空即失败，且 Result 仍然可用（含部分输出）。
// 非零退出码也算失败——静默忽略非零退出是这类封装最常见的坑。
func Run(ctx context.Context, name string, args []string, opts Options) (Result, error) {
	res := Result{Command: Display(name, args)}
	if strings.TrimSpace(name) == "" {
		return res, fmt.Errorf("%w: 命令名为空", ErrStart)
	}

	timeout := cmp.Or(opts.Timeout, DefaultTimeout)
	maxOut := cmp.Or(opts.MaxOutput, DefaultMaxOutput)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	realName, realArgs := commandLine(name, args)
	cmd := exec.CommandContext(ctx, realName, realArgs...)
	// 桌面版（-H windowsgui）自己没有控制台：不设这个，每个子进程都会弹出一个
	// 终端窗口（跑一次 `arkperf check` 就弹 4 个）。详见 HideWindow 的注释。
	HideWindow(cmd)
	cmd.Dir = opts.Dir
	cmd.Env = childEnv(opts.Env)
	// Stdin 留空：给子进程连上空设备，交互式工具才不会等输入等到超时。
	cmd.Stdin = nil
	// 上下文取消后最多再等 2s 收尾，避免 I/O 协程把 Wait 挂住。
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr capBuffer
	stdout.max, stderr.max = maxOut, maxOut
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	start := time.Now()
	// Start/Wait 分开而不是直接 Run：只有 Start 成功才说明进程真的被创建，
	// 这个信息必须留下来（见 Result.Started）。
	if startErr := cmd.Start(); startErr != nil {
		res.Duration = time.Since(start)
		// ctx 已取消时 Start 也会失败，但那不是"工具链有问题"
		if ctx.Err() != nil {
			return res, fmt.Errorf("命令被取消: %s: %w", res.Command, context.Cause(ctx))
		}
		return res, fmt.Errorf("%w: %s: %w", ErrStart, res.Command, startErr)
	}
	res.Started = true
	err := cmd.Wait()
	res.Duration = time.Since(start)
	res.Stdout = clean(stdout.String())
	res.Stderr = clean(stderr.String())
	res.Truncated = stdout.truncated || stderr.truncated
	res.Output = merge(res.Stdout, res.Stderr)

	switch {
	case errors.Is(context.Cause(ctx), context.DeadlineExceeded):
		res.TimedOut = true
		return res, fmt.Errorf("%w（%s）: %s", ErrTimeout, timeout, res.Command)
	case ctx.Err() != nil:
		// 上游取消（Ctrl+C / 调用方放弃）：既不是超时，也不是启动失败，
		// 报错时必须区分开，否则会把"人按了停止"当成"工具链坏了"。
		return res, fmt.Errorf("命令被取消: %s: %w", res.Command, context.Cause(ctx))
	case err != nil:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, fmt.Errorf("%s: 退出码 %d%s", res.Command, res.ExitCode, detail(res))
		}
		return res, fmt.Errorf("%w: %s: %w", ErrStart, res.Command, err)
	}

	return res, nil
}

// commandLine 把 .bat/.cmd 包进 cmd.exe。
//
// Windows 的 CreateProcess 不能直接执行批处理文件，而 DevEco 的
// hvigorw 与 ohpm 恰恰都是 .bat——不做这层包装，工具永远启动不了。
func commandLine(name string, args []string) (string, []string) {
	if runtime.GOOS != "windows" {
		return name, args
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".bat", ".cmd":
		return "cmd.exe", append([]string{"/C", name}, args...)
	}
	return name, args
}

// childEnv 在继承环境的基础上钉住 UTF-8。
//
// 中文 Windows 上 Java 工具链默认按系统代码页（GBK）写 stdout，
// 我们按 UTF-8 读回来就是乱码。显式钉死编码比事后猜代码页可靠得多。
func childEnv(extra map[string]string) []string {
	env := append(os.Environ(),
		// hvigor 是 Java 写的；JVM 会因此往 stderr 打一行 "Picked up ..."，
		// 那行噪声在 clean() 里被过滤掉。
		"JAVA_TOOL_OPTIONS=-Dfile.encoding=UTF-8 -Dsun.stdout.encoding=UTF-8 -Dsun.stderr.encoding=UTF-8",
		"PYTHONIOENCODING=utf-8",
	)
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// ansiPattern 匹配终端的 ANSI 转义序列。
//
// hvigor 即使输出被管道接走也照样打颜色码（实测输出里全是 [32m 这类），
// 不清掉的话这些字节会进入给模型的上下文：既占 token，又让构建日志难读。
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// clean 规范化输出：去 ANSI 颜色、统一换行、去掉已知噪声行、处理非 UTF-8。
func clean(s string) string {
	if s == "" {
		return ""
	}
	s = ansiPattern.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")

	lines := make([]string, 0, 16)
	for line := range strings.SplitSeq(s, "\n") {
		// JVM 在 JAVA_TOOL_OPTIONS 生效时固定打印这一行，与命令无关
		if strings.HasPrefix(line, "Picked up JAVA_TOOL_OPTIONS") {
			continue
		}
		lines = append(lines, strings.TrimRight(line, " \t"))
	}

	out := strings.TrimSpace(strings.Join(lines, "\n"))
	if out == "" {
		return ""
	}
	if !utf8.ValidString(out) {
		// 与其把乱码喂给模型（它会基于乱码"推理"出结论），不如显式说明丢过东西
		out = strings.ToValidUTF8(out, "")
		out += "\n[注意：命令输出不是合法 UTF-8，可能被控制台代码页破坏；部分内容已丢弃]"
	}
	return out
}

func merge(stdout, stderr string) string {
	switch {
	case stderr == "":
		return stdout
	case stdout == "":
		return stderr
	default:
		return stdout + "\n--- stderr ---\n" + stderr
	}
}

// detail 给错误附一段输出摘要，否则"退出码 1"等于什么都没说。
func detail(r Result) string {
	s := strings.TrimSpace(r.Output)
	if s == "" {
		return ""
	}
	const limit = 400
	if len(s) > limit {
		// 报错摘要取下限不取上限：失败原因通常在输出的末尾
		s = "…" + s[len(s)-limit:]
	}
	return ": " + strings.ReplaceAll(s, "\n", " | ")
}

// Display 生成可读命令行，仅用于展示（带空格的参数加引号）。
func Display(name string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, name)
	for _, a := range args {
		if strings.ContainsAny(a, " \t\"") {
			parts = append(parts, `"`+a+`"`)
			continue
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// capBuffer 是有上限的输出缓冲，超限时保留头尾、掐掉中间。
//
// 为什么保留头尾：构建日志的开头有任务名与阶段，结尾有失败原因与耗时分项，
// 中间的大段进度输出最没信息量。整体截断任意一端都会丢掉关键的一半。
type capBuffer struct {
	mu        sync.Mutex
	max       int
	buf       []byte
	truncated bool
}

// TruncationMarker 是掐掉中间时留下的标记。
const TruncationMarker = "\n...[输出过长，中间部分已省略]...\n"

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.truncated = true
		head := b.max / 2
		tail := b.max - head
		keep := make([]byte, 0, b.max)
		keep = append(keep, b.buf[:head]...)
		keep = append(keep, b.buf[len(b.buf)-tail:]...)
		b.buf = keep
	}
	return len(p), nil
}

func (b *capBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.truncated {
		return string(b.buf)
	}
	head := b.max / 2
	tail := b.max - head
	out := make([]byte, 0, b.max+len(TruncationMarker))
	out = append(out, b.buf[:head]...)
	out = append(out, TruncationMarker...)
	out = append(out, b.buf[head:head+tail]...)
	return string(out)
}
