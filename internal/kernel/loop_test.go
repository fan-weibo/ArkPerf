package kernel

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scriptedChat 按脚本返回响应，并记录每次收到的请求。
// 有了它，整个循环可以完全离线测试——不烧钱、不依赖网络、结果确定。
type scriptedChat struct {
	t        *testing.T
	steps    []ChatResponse
	requests []ChatRequest
	// beforeReturn 在返回响应前调用，可用来模拟中断。
	beforeReturn func(step int)
}

func (s *scriptedChat) chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	s.requests = append(s.requests, req)
	n := len(s.requests)
	if n > len(s.steps) {
		s.t.Fatalf("model called %d times, only %d responses scripted", n, len(s.steps))
	}
	if s.beforeReturn != nil {
		s.beforeReturn(n)
	}
	if err := ctx.Err(); err != nil {
		return ChatResponse{}, err
	}
	return s.steps[n-1], nil
}

func assistantText(text string) ChatResponse {
	return ChatResponse{Message: Message{Role: "assistant", Content: text}}
}

func assistantToolCall(id, name, args string) ChatResponse {
	return ChatResponse{Message: Message{
		Role:      "assistant",
		ToolCalls: []ToolCall{{ID: id, Function: FunctionCall{Name: name, Arguments: args}}},
	}}
}

type yesApprover struct{ asked int }

func (a *yesApprover) Ask(context.Context, string, map[string]any) (bool, error) {
	a.asked++
	return true, nil
}

type noApprover struct{ asked int }

func (a *noApprover) Ask(context.Context, string, map[string]any) (bool, error) {
	a.asked++
	return false, nil
}

func TestRunLoop_ToolThenFinalAnswer(t *testing.T) {
	reg := NewRegistry()
	reg.Register(fakeTool{name: "echo", output: "echoed: hi"})

	chat := &scriptedChat{t: t, steps: []ChatResponse{
		assistantToolCall("c1", "echo", `{"text":"hi"}`),
		assistantText("  完成  "),
	}}

	res, err := RunLoop(t.Context(), LoopConfig{
		Registry: reg,
		Chat:     chat.chat,
		Ctx:      ToolCtx{CWD: `E:\work`},
	}, "do the thing")
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}

	if res.Reason != StopFinal {
		t.Fatalf("reason: %s", res.Reason)
	}
	if res.Text != "完成" {
		t.Fatalf("text should be trimmed: %q", res.Text)
	}
	if res.Turns != 2 || res.ToolUses != 1 {
		t.Fatalf("turns=%d toolUses=%d", res.Turns, res.ToolUses)
	}

	// 第一次请求必须带上系统提示词与工具定义
	first := chat.requests[0]
	if len(first.Tools) != 1 || first.Tools[0].Name != "echo" {
		t.Fatalf("tools must be advertised to the model: %+v", first.Tools)
	}
	if first.Messages[0].Role != "system" || !strings.Contains(first.Messages[0].Content, `E:\work`) {
		t.Fatalf("system prompt must carry the host facts: %q", first.Messages[0].Content)
	}
	if first.Messages[1].Role != "user" || first.Messages[1].Content != "do the thing" {
		t.Fatalf("user task missing: %+v", first.Messages[1])
	}

	// 第二次请求必须看到回填的工具结果
	tail := chat.requests[1].Messages
	last := tail[len(tail)-1]
	if last.Role != "tool" || last.ToolCallID != "c1" {
		t.Fatalf("tool result must be fed back: %+v", last)
	}
	if !strings.Contains(last.Content, "echoed: hi") {
		t.Fatalf("tool output lost: %q", last.Content)
	}
}

func TestRunLoop_DeniedApprovalIsReportedNotFatal(t *testing.T) {
	reg := NewRegistry()
	reg.Register(fakeTool{name: "danger", approval: true, output: "should never run"})

	denier := &noApprover{}
	chat := &scriptedChat{t: t, steps: []ChatResponse{
		assistantToolCall("c1", "danger", `{}`),
		assistantText("好的，我不执行"),
	}}

	res, err := RunLoop(t.Context(), LoopConfig{
		Registry: reg,
		Chat:     chat.chat,
		Approver: denier,
	}, "do it")
	if err != nil {
		t.Fatalf("a denial must not fail the task: %v", err)
	}
	if denier.asked != 1 {
		t.Fatalf("approver should be asked once, got %d", denier.asked)
	}
	if res.Reason != StopFinal {
		t.Fatalf("reason: %s", res.Reason)
	}

	// 驳回必须变成一条 IsError 的工具结果回到模型，而不是静默跳过
	tail := chat.requests[1].Messages
	last := tail[len(tail)-1]
	if !strings.Contains(last.Content, "denied") {
		t.Fatalf("denial must be fed back to the model: %q", last.Content)
	}
}

func TestRunLoop_AutoApprovalSkipsApprover(t *testing.T) {
	reg := NewRegistry()
	reg.Register(fakeTool{name: "danger", approval: true, output: "ran"})

	approver := &yesApprover{}
	chat := &scriptedChat{t: t, steps: []ChatResponse{
		assistantToolCall("c1", "danger", `{}`),
		assistantText("done"),
	}}

	if _, err := RunLoop(t.Context(), LoopConfig{
		Registry: reg,
		Chat:     chat.chat,
		Approval: "auto",
		Approver: approver,
	}, "do it"); err != nil {
		t.Fatal(err)
	}
	if approver.asked != 0 {
		t.Fatalf("auto mode must not ask, asked=%d", approver.asked)
	}
}

// 没有审批通道时要拒绝而不是放行：安全缺省必须是"否"。
func TestRunLoop_MissingApproverDeniesByDefault(t *testing.T) {
	reg := NewRegistry()
	reg.Register(fakeTool{name: "danger", approval: true, output: "ran"})

	chat := &scriptedChat{t: t, steps: []ChatResponse{
		assistantToolCall("c1", "danger", `{}`),
		assistantText("ok"),
	}}

	if _, err := RunLoop(t.Context(), LoopConfig{Registry: reg, Chat: chat.chat}, "do it"); err != nil {
		t.Fatal(err)
	}
	tail := chat.requests[1].Messages
	if last := tail[len(tail)-1]; !strings.Contains(last.Content, "denied") {
		t.Fatalf("no approver must mean deny: %q", last.Content)
	}
}

func TestRunLoop_UnknownToolIsRecoverable(t *testing.T) {
	reg := NewRegistry()
	chat := &scriptedChat{t: t, steps: []ChatResponse{
		assistantToolCall("c1", "ghost", `{}`),
		assistantText("改用别的办法"),
	}}

	res, err := RunLoop(t.Context(), LoopConfig{
		Registry:     reg,
		Chat:         chat.chat,
		MaxIdleTurns: 5,
	}, "do it")
	if err != nil {
		t.Fatalf("unknown tool must not crash the loop: %v", err)
	}
	if res.Reason != StopFinal {
		t.Fatalf("reason: %s", res.Reason)
	}
	tail := chat.requests[1].Messages
	if last := tail[len(tail)-1]; !strings.Contains(last.Content, "unknown tool") {
		t.Fatalf("model should learn the tool is unknown: %q", last.Content)
	}
}

func TestRunLoop_MalformedArgumentsAreReported(t *testing.T) {
	reg := NewRegistry()
	reg.Register(fakeTool{name: "echo", output: "never"})

	chat := &scriptedChat{t: t, steps: []ChatResponse{
		assistantToolCall("c1", "echo", `{"text": not-json`),
		assistantText("ok"),
	}}

	if _, err := RunLoop(t.Context(), LoopConfig{
		Registry:     reg,
		Chat:         chat.chat,
		MaxIdleTurns: 5,
	}, "do it"); err != nil {
		t.Fatal(err)
	}
	tail := chat.requests[1].Messages
	if last := tail[len(tail)-1]; !strings.Contains(last.Content, "invalid JSON") {
		t.Fatalf("malformed args must be reported: %q", last.Content)
	}
}

func TestRunLoop_IdleDetection(t *testing.T) {
	reg := NewRegistry()
	chat := &scriptedChat{t: t, steps: []ChatResponse{
		assistantToolCall("c1", "ghost", `{}`),
		assistantToolCall("c2", "ghost", `{}`),
	}}

	res, err := RunLoop(t.Context(), LoopConfig{
		Registry:     reg,
		Chat:         chat.chat,
		MaxIdleTurns: 2,
	}, "do it")
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != StopIdle {
		t.Fatalf("reason: %s (a stuck agent must stop, not spin)", res.Reason)
	}
	if len(chat.requests) != 2 {
		t.Fatalf("should stop right at the threshold, calls=%d", len(chat.requests))
	}
}

func TestRunLoop_TurnValve(t *testing.T) {
	reg := NewRegistry()
	reg.Register(fakeTool{name: "echo", output: "ok"})

	chat := &scriptedChat{t: t, steps: []ChatResponse{
		assistantToolCall("c1", "echo", `{}`),
		assistantToolCall("c2", "echo", `{}`),
		assistantToolCall("c3", "echo", `{}`),
	}}

	res, err := RunLoop(t.Context(), LoopConfig{
		Registry: reg,
		Chat:     chat.chat,
		MaxTurns: 3,
	}, "do it")
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != StopTurnValve {
		t.Fatalf("reason: %s", res.Reason)
	}
	if res.Turns != 3 {
		t.Fatalf("turns: %d", res.Turns)
	}
}

func TestRunLoop_Interrupt(t *testing.T) {
	reg := NewRegistry()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	chat := &scriptedChat{
		t:     t,
		steps: []ChatResponse{assistantToolCall("c1", "echo", `{}`)},
		beforeReturn: func(int) {
			cancel() // 模拟用户在中途按了 Ctrl+C
		},
	}
	reg.Register(fakeTool{name: "echo", output: "ok"})

	res, err := RunLoop(ctx, LoopConfig{Registry: reg, Chat: chat.chat}, "do it")
	if err != nil {
		t.Fatalf("interrupt must not surface as an error: %v", err)
	}
	if res.Reason != StopInterrupted {
		t.Fatalf("reason: %s", res.Reason)
	}
}

// 模型调用真出错时必须冒泡，不能被当成正常收尾悄悄吞掉。
func TestRunLoop_ModelErrorPropagates(t *testing.T) {
	reg := NewRegistry()
	boom := errors.New("endpoint unreachable")

	chat := func(context.Context, ChatRequest) (ChatResponse, error) {
		return ChatResponse{}, boom
	}

	_, err := RunLoop(t.Context(), LoopConfig{Registry: reg, Chat: chat}, "do it")
	if !errors.Is(err, boom) {
		t.Fatalf("error must wrap the cause: %v", err)
	}
}

func TestRunLoop_RequiresChatAndRegistry(t *testing.T) {
	if _, err := RunLoop(t.Context(), LoopConfig{}, "x"); err == nil {
		t.Fatal("missing Chat must error")
	}
	if _, err := RunLoop(t.Context(), LoopConfig{Chat: func(context.Context, ChatRequest) (ChatResponse, error) {
		return assistantText(""), nil
	}}, "x"); err == nil {
		t.Fatal("missing Registry must error")
	}
}
