package kernel

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Task 是一次任务的输入。
type Task struct {
	// Text 是用户的任务描述。
	Text string
	// Approval 覆盖配置里的审批模式（"ask" | "auto"）。空表示沿用配置。
	Approval string
	// MaxTurns 覆盖配置里的轮次上限。0 表示沿用配置。
	MaxTurns int
}

// Runner 把配置、工具注册表、审批通道与输出组装成一次可执行的任务。
type Runner struct {
	Cfg      *Config
	Registry *Registry
	// Approver 为空时，需要审批的工具一律驳回。
	Approver Approver
	// Out 是进度与回答的输出目标。
	Out io.Writer
	// SilenceProgress 为 true 时只输出最终回答（脚本/CI 用）。
	SilenceProgress bool
	// Events 非空时覆盖默认的文本进度输出。
	//
	// 给 TUI 这类"自己渲染"的前端用：它们要的是结构化事件，而不是
	// 已经被拍平成文本的进度行。为空时走 Out 的默认文本渲染。
	Events *LoopEvents
	// Conversation 非空时，任务之间共享对话上下文（多轮会话）。
	//
	// 为空则每次 Run 都是全新对话（命令行一次性调用的语义）。
	Conversation *Conversation
	// Chat 覆盖模型调用（nil 则按 Cfg.Provider 构造）。
	// 与 LoopConfig.Chat 同源：有了它，Runner+Conversation 这条链路也能离线测。
	Chat ChatFunc
}

// Run 执行一次任务。
func (r *Runner) Run(ctx context.Context, t Task) (LoopResult, error) {
	if err := r.Cfg.Validate(); err != nil {
		return LoopResult{}, err
	}
	if r.Registry == nil {
		return LoopResult{}, fmt.Errorf("runner: Registry is required")
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	events := r.defaultEvents()
	if r.Events != nil {
		events = *r.Events
	}

	var history []Message
	if r.Conversation != nil {
		history = r.Conversation.History()
	}

	chat := r.Chat
	if chat == nil {
		chat = NewClient(r.Cfg.Provider).Chat
	}

	res, runErr := RunLoop(ctx, LoopConfig{
		Registry: r.Registry,
		Ctx:      ToolCtx{CWD: cwd, Home: Home()},
		Chat:     chat,
		MaxTurns: cmp.Or(t.MaxTurns, r.Cfg.MaxTurns),
		Approval: cmp.Or(t.Approval, r.Cfg.Approval, "ask"),
		Approver: r.Approver,
		Events:   events,
		History:  history,
	}, t.Text)

	// 把这一轮并入会话。所有收尾路径都过这里（含中断与失败）：
	// Conversation.Set 自己会丢掉没跑完的半截轮，调用方不必分情况判断——
	// 让调用方判断的话，迟早有一个分支忘了更新，表现为"会话偶尔断片"。
	if r.Conversation != nil {
		r.Conversation.Set(res.Messages)
	}
	return res, runErr
}

// defaultEvents 是面向普通 CLI 的文本进度渲染。
func (r *Runner) defaultEvents() LoopEvents {
	out := r.Out
	if out == nil {
		out = os.Stdout
	}

	events := LoopEvents{
		OnAssistant: func(text string) { fmt.Fprintln(out, text) },
	}
	if r.SilenceProgress {
		return events
	}
	events.OnToolCall = func(name string, args map[string]any) {
		fmt.Fprintf(out, "→ %s %s\n", name, compactArgs(args))
	}
	events.OnToolResult = func(name, output string, isError bool) {
		mark := "✓"
		if isError {
			mark = "✗"
		}
		fmt.Fprintf(out, "%s %s %s\n", mark, name, firstLine(output))
	}
	return events
}

// compactArgs 把参数压成一行，太长就截断。
func compactArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprintf("%v", args)
	}
	return truncate(string(b), 160)
}

// firstLine 取输出的第一行非空内容，用于进度行。
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncate(line, 120)
		}
	}
	return "(no output)"
}
