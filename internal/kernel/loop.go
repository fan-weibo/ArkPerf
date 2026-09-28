package kernel

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ChatFunc 是可注入的模型调用。生产传 Client.Chat，测试传假实现——
// 有了这个接缝，整个循环可以完全离线测试，不烧钱、不依赖网络。
type ChatFunc func(ctx context.Context, req ChatRequest) (ChatResponse, error)

// Approver 决定一个需要审批的工具能否执行。
type Approver interface {
	Ask(ctx context.Context, name string, args map[string]any) (bool, error)
}

// LoopEvents 是循环对外的事件回调，前端靠它渲染进度。
// 全部可空：没人监听时循环照跑。
type LoopEvents struct {
	OnAssistant  func(text string)
	OnToolCall   func(name string, args map[string]any)
	OnToolResult func(name, output string, isError bool)
	OnApproval   func(name string, granted bool)
}

// StopReason 说明循环为什么停下。区分这些原因是可观测性的基础：
// "模型答完了"和"撞上阀门了"是两件完全不同的事。
type StopReason string

const (
	StopFinal       StopReason = "final"       // 模型给出了不再调工具的最终回答
	StopIdle        StopReason = "idle"        // 连续多轮零成功工具调用，判定卡死
	StopTurnValve   StopReason = "turn-valve"  // 撞上 MaxTurns
	StopInterrupted StopReason = "interrupted" // 被中断（Ctrl+C / 上游取消）
)

const (
	defaultMaxTurns     = 200
	defaultMaxIdleTurns = 12
	// maxToolOutputBytes 是单条工具结果进入上下文的字节上限。
	// 没有这个阀门，一次失控的目录列举就能把上下文顶爆。
	maxToolOutputBytes = 40_000
)

// LoopResult 是一次任务的完整结果。
type LoopResult struct {
	Text     string
	Turns    int
	ToolUses int
	Reason   StopReason
	// Messages 是本次运行结束后的完整转录（含 system 与历史）。
	//
	// 调用方据此把会话继续下去：把它交给下一轮的 LoopConfig.History，
	// 多轮对话就成立了。这是"会话"这个概念的载体。
	Messages []Message
}

// LoopConfig 是循环的全部输入。
type LoopConfig struct {
	// Registry 必填：工具来源。
	Registry *Registry
	// Chat 必填：模型调用。
	Chat ChatFunc
	// Ctx 是工具执行环境（工作目录、状态根）。
	Ctx ToolCtx

	// History 是本轮之前的对话（不含 system，且必须以 user 消息开头）。
	//
	// 为空表示新会话。它由上一轮的 LoopResult.Messages 去掉 system 得到——
	// 这样"多轮对话"和"单轮任务"是同一条代码路径，没有第二套实现。
	History []Message

	// MaxTurns 是轮次上限（一轮 = 一次模型请求 + 它要调的工具）。
	// 零值用 defaultMaxTurns。这是防死循环烧钱的阀门。
	MaxTurns int
	// MaxIdleTurns 是连续零成功工具调用的容忍轮数，零值用 defaultMaxIdleTurns。
	MaxIdleTurns int

	// Approval 为 "auto" 时跳过审批；否则需要审批的工具必须取得 Approver 同意。
	Approval string
	// Approver 为空时，需要审批的工具一律驳回——安全缺省是拒绝，不是放行。
	Approver Approver

	Events LoopEvents
}

// RunLoop 执行一次完整任务：模型 → 工具 → 回填 → 再问，直到模型给出最终回答。
//
// 循环本身刻意保持"无聊"：没有规划器、没有子代理、没有自省——
// 复杂度全部放在工具里。这是 loop engineering 的核心取舍：
// 一个能看懂、能单步调试的 while 循环，比聪明但不可预测的编排更可靠。
func RunLoop(ctx context.Context, cfg LoopConfig, task string) (res LoopResult, runErr error) {
	if cfg.Chat == nil {
		return res, fmt.Errorf("kernel/loop: Chat is required")
	}
	if cfg.Registry == nil {
		return res, fmt.Errorf("kernel/loop: Registry is required")
	}

	maxTurns := cmp.Or(cfg.MaxTurns, defaultMaxTurns)
	maxIdle := cmp.Or(cfg.MaxIdleTurns, defaultMaxIdleTurns)
	specs := cfg.Registry.Specs()

	msgs := make([]Message, 0, len(cfg.History)+2)
	msgs = append(msgs, Message{Role: "system", Content: SystemPrompt(cfg.Ctx.CWD, specs)})
	// 历史接在 system 之后、本轮用户输入之前
	msgs = append(msgs, cfg.History...)
	msgs = append(msgs, Message{Role: "user", Content: task})

	// 用命名返回值 + defer：循环有五个出口，靠每个 return 各自记得挂转录
	// 迟早会漏一个，而漏掉的表现是"会话莫名其妙断了"，几乎无法定位。
	defer func() { res.Messages = msgs }()

	idle := 0

	for turn := 1; turn <= maxTurns; turn++ {
		res.Turns = turn

		if ctx.Err() != nil {
			res.Reason = StopInterrupted
			return res, nil
		}

		resp, err := cfg.Chat(ctx, ChatRequest{Messages: msgs, Tools: specs})
		if err != nil {
			// 中断引发的失败不是故障，按 interrupted 正常收尾。
			if ctx.Err() != nil {
				res.Reason = StopInterrupted
				return res, nil
			}
			return res, fmt.Errorf("turn %d: %w", turn, err)
		}
		msgs = append(msgs, resp.Message)

		// 没有工具调用 = 最终回答，闭环结束。
		if len(resp.Message.ToolCalls) == 0 {
			res.Text = strings.TrimSpace(resp.Message.Content)
			res.Reason = StopFinal
			emitAssistant(cfg.Events, res.Text)
			return res, nil
		}
		// 有些模型会"边说话边调工具"，这段文字也要让用户看到。
		emitAssistant(cfg.Events, strings.TrimSpace(resp.Message.Content))

		succeeded := 0
		for _, call := range resp.Message.ToolCalls {
			result := runOneTool(ctx, cfg, call)
			res.ToolUses++
			if !result.IsError {
				succeeded++
			}
			msgs = append(msgs, Message{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    caption(call, result),
			})
		}

		if succeeded == 0 {
			idle++
			if idle >= maxIdle {
				res.Reason = StopIdle
				return res, nil
			}
			continue
		}
		idle = 0
	}

	res.Reason = StopTurnValve
	return res, nil
}

// runOneTool 执行单个工具调用。
//
// 审批驳回、工具缺失、参数非法、执行报错——全部折叠成一条 ToolResult{IsError: true}，
// 而不是中断整个任务。理由是模型看到失败原因后通常能自己换策略；
// 一次工具失败就让任务崩掉，等于把可恢复的挫折升级成硬故障。
func runOneTool(ctx context.Context, cfg LoopConfig, call ToolCall) ToolResult {
	name := call.Function.Name

	args, err := parseArgs(call.Function.Arguments)
	if err != nil {
		return fail(fmt.Sprintf("%s: invalid JSON arguments %q: %v", name, truncate(call.Function.Arguments, 200), err))
	}

	if cfg.Events.OnToolCall != nil {
		cfg.Events.OnToolCall(name, args)
	}

	tool, ok := cfg.Registry.Get(name)
	if !ok {
		return fail(fmt.Sprintf("unknown tool %q; available: %s", name, strings.Join(cfg.Registry.Names(), ", ")))
	}

	if tool.NeedsApproval(args) && cfg.Approval != "auto" {
		granted, err := askApproval(ctx, cfg, name, args)
		if cfg.Events.OnApproval != nil {
			cfg.Events.OnApproval(name, granted)
		}
		switch {
		case err != nil:
			return fail(fmt.Sprintf("%s: approval failed: %v", name, err))
		case !granted:
			return fail(fmt.Sprintf("the user denied running %s; do not retry it, ask the user how to proceed", name))
		}
	}

	result, err := tool.Execute(ctx, args, cfg.Ctx)
	if err != nil {
		return fail(fmt.Sprintf("%s: %v", name, err))
	}

	if cfg.Events.OnToolResult != nil {
		cfg.Events.OnToolResult(name, result.Output, result.IsError)
	}
	return result
}

// askApproval 在没有审批通道时返回 false：安全缺省必须是拒绝。
func askApproval(ctx context.Context, cfg LoopConfig, name string, args map[string]any) (bool, error) {
	if cfg.Approver == nil {
		return false, nil
	}
	return cfg.Approver.Ask(ctx, name, args)
}

// parseArgs 解析模型给的参数。空字符串按空对象处理（无参工具很常见）。
func parseArgs(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, err
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

// caption 截断过长的工具输出，并在截断处显式标注——
// 悄悄丢内容会让模型基于残缺信息自信地胡说。
func caption(_ ToolCall, r ToolResult) string {
	if r.IsError {
		return "ERROR: " + truncate(r.Output, maxToolOutputBytes)
	}
	return truncate(r.Output, maxToolOutputBytes)
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + fmt.Sprintf("\n... [truncated: original %d bytes, showing first %d]", len(s), limit)
}

func fail(msg string) ToolResult { return ToolResult{Output: msg, IsError: true} }

func emitAssistant(e LoopEvents, text string) {
	if e.OnAssistant != nil && text != "" {
		e.OnAssistant(text)
	}
}
