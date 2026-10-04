package kernel

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fan-weibo/ArkPerf/internal/skill"
)

// Task 是一次任务的输入。
type Task struct {
	// Text 是用户的任务描述。
	Text string
	// CWD 是这次任务的工作目录：工具里相对路径的基准。
	//
	// 留空表示用 Runner.CWD，再留空才回退到进程 CWD。
	// 按次给而不是只给 Runner：同一个 Runner 可能被前端复用，
	// 而用户切了工作区之后，下一次任务就该用新的目录。
	CWD string
	// Approval 覆盖配置里的审批模式（"ask" | "auto"）。空表示沿用配置。
	Approval string
	// MaxTurns 覆盖配置里的轮次上限。0 表示沿用配置。
	MaxTurns int
	// Skills 是这次任务可用的技能，覆盖 Runner.Skills。
	//
	// 按次给而不是只给 Runner，理由与 CWD 相同：同一个 Runner 会被前端
	// 复用，而用户切了工作区之后，下一次任务就该用新目录下的技能。
	// nil 表示沿用 Runner.Skills；传非 nil 的空切片表示"这次不用技能"。
	Skills []skill.Skill
}

// Runner 把配置、工具注册表、审批通道与输出组装成一次可执行的任务。
type Runner struct {
	Cfg      *Config
	Registry *Registry
	// CWD 是默认工作目录（工具相对路径的基准）。
	//
	// **要显式设置**：不设的话工具会用进程 CWD，而桌面版双击启动时
	// 进程 CWD 是 exe 所在目录——界面显示的工作区和工具实际操作的目录
	// 会错开，表现为"Agent 说文件不存在，可那个文件明明在界面上"
	// （实测踩过）。Task.CWD 可按次覆盖。
	CWD string
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
	// Skills 是默认技能集（按工作目录解析出来的一组技能）。
	//
	// 为空表示不带技能。Task.Skills 可按次覆盖——同一个 Runner 被复用
	// 而工作区变了的时候，需要换一组技能。
	Skills []skill.Skill
	// Rules 是"以后别再问"的审批记忆。为空表示不做记忆（每次都问）。
	Rules *ApprovalRules
}

// Run 执行一次任务。
func (r *Runner) Run(ctx context.Context, t Task) (LoopResult, error) {
	if err := r.Cfg.Validate(); err != nil {
		return LoopResult{}, err
	}
	if r.Registry == nil {
		return LoopResult{}, fmt.Errorf("runner: Registry is required")
	}

	// 工具的工作目录：Task 按次指定 > Runner 默认 > 进程 CWD。
	//
	// 这个顺序是刻意的：进程 CWD 在最末，因为它最不可靠——
	// 图形界面双击启动时它是 exe 所在目录，跟用户以为的工作区毫无关系。
	cwd := cmp.Or(t.CWD, r.CWD)
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		} else {
			cwd = "."
		}
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

	// 技能：Task 按次指定 > Runner 默认。与 cwd 同一套覆盖顺序。
	//
	// 这里不能用 cmp.Or：切片不可比较。用 nil 判断而不是长度判断，
	// 是因为"这次明确不用技能"（非 nil 的空切片）必须能与"没指定"区分开。
	skills := r.Skills
	if t.Skills != nil {
		skills = t.Skills
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
		Skills:   skills,
		Rules:    r.Rules,
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

	if r.SilenceProgress {
		// 静默模式只输出最终回答：因此**不挂 OnDelta**，模型走一次性路径，
		// OnAssistant 拿到的就是唯一一次输出。挂上它反而会把中间过程也打出来。
		return LoopEvents{
			OnAssistant: func(text string) { fmt.Fprintln(out, text) },
		}
	}

	// 交互模式走流式：字边产边出，而不是等整段说完。
	events := LoopEvents{
		OnDelta: func(kind DeltaKind, text string) {
			// 只打可见回答。思考过程刻意不打：这条路径的 stdout 常常被脚本
			// 接走（`arkperf "任务" > 报告.txt`），把思考混进去会污染结果。
			if kind == DeltaContent {
				fmt.Fprint(out, text)
			}
		},
		// 流式之后 OnAssistant 退化成"收尾换行"：文本已经在屏幕上了，
		// 再打一遍就是同一段话出现两次。
		OnAssistant: func(string) { fmt.Fprintln(out) },
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
