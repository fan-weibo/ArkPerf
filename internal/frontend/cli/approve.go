// Package cli 是 ArkPerf 的终端前端。
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// Approver 在终端逐条询问危险工具是否放行。
type Approver struct {
	In  io.Reader
	Out io.Writer
	// Auto 为 true 时全部放行（对应 --yes）。危险，仅用于自动化场景。
	Auto bool

	// reader 惰性创建：一个任务里可能问多次，不该每次重建缓冲区
	// （重建会丢掉已经预读进缓冲区的字节）。
	reader func() *bufio.Reader
}

var _ kernel.Approver = (*Approver)(nil)

// NewApprover 构造终端审批器。
func NewApprover(in io.Reader, out io.Writer, auto bool) *Approver {
	a := &Approver{In: in, Out: out, Auto: auto}
	a.reader = sync.OnceValue(func() *bufio.Reader { return bufio.NewReader(a.In) })
	return a
}

// Ask 询问一次。非交互式 stdin（管道、CI、重定向）一律驳回：
// 没有人能回答的时候，安全缺省是拒绝，而不是默默放行。
//
// scope 非空时多给一个 [a]："这一类以后都不问"。**必须把类别原样显示出来**——
// 只显示工具名的话，"总是允许"会被理解成"永远允许这个工具"，
// 而实际范围可能小得多（比如只是某个目录），也可能大得多。
func (a *Approver) Ask(ctx context.Context, name string, args map[string]any, scope string) (kernel.ApprovalDecision, error) {
	if a.Auto {
		fmt.Fprintf(a.Out, "⚡ 自动批准 %s %s\n", name, compact(args))
		return kernel.ApprovalOnce, nil
	}
	if err := ctx.Err(); err != nil {
		return kernel.ApprovalDeny, err
	}
	if !isInteractive(a.In) {
		fmt.Fprintf(a.Out, "⚠ %s 需要审批，但 stdin 不是交互终端 → 已驳回（如需放行请加 --yes）\n", name)
		return kernel.ApprovalDeny, nil
	}

	fmt.Fprintf(a.Out, "⚠ 允许执行 %s %s ?\n", name, compact(args))
	if scope != "" {
		fmt.Fprintf(a.Out, "  类别：%s\n", scope)
		fmt.Fprint(a.Out, "  [y] 允许一次　[a] 这一类以后都不问　[N] 拒绝 > ")
	} else {
		fmt.Fprint(a.Out, "  [y] 允许一次　[N] 拒绝 > ")
	}

	line, err := a.reader().ReadString('\n')
	if err != nil && line == "" {
		return kernel.ApprovalDeny, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return kernel.ApprovalOnce, nil
	case "a", "always":
		if scope == "" {
			// 没有可记忆的类别，只能放行这一次。必须说出来，
			// 否则用户以为已经一劳永逸，下次被再问一遍只会觉得功能坏了。
			fmt.Fprint(a.Out, "  这一次没有可记忆的类别（这类调用每次都不一样），只放行本次\n")
			return kernel.ApprovalOnce, nil
		}
		return kernel.ApprovalAlways, nil
	default:
		return kernel.ApprovalDeny, nil
	}
}

// isInteractive 判断输入是不是一个真实终端。
func isInteractive(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// compact 把参数压成一行短摘要用于提示。
func compact(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprintf("%v", args)
	}
	s := string(b)
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
