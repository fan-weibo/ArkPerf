package kernel

import (
	"strings"
	"testing"
)

// assistantWithCalls 造一条带 tool_calls 的 assistant 消息。
func assistantWithCalls(callIDs ...string) Message {
	calls := make([]ToolCall, 0, len(callIDs))
	for _, id := range callIDs {
		calls = append(calls, ToolCall{ID: id, Function: FunctionCall{Name: "some_tool", Arguments: "{}"}})
	}
	return Message{Role: "assistant", ToolCalls: calls}
}

func toolReply(id, content string) Message {
	return Message{Role: "tool", ToolCallID: id, Content: content}
}

// assertNoDanglingToolCalls 按端点的校验规则检查历史：
// 每个 assistant 的 tool_calls 都必须有对应的 tool 回复，
// 否则下一轮请求直接 400（实测把用户会话卡死，之后一条都发不出去）。
func assertNoDanglingToolCalls(t *testing.T, msgs []Message) {
	t.Helper()
	for i, m := range msgs {
		if len(m.ToolCalls) == 0 {
			continue
		}
		answered := make(map[string]bool, len(m.ToolCalls))
		for _, x := range msgs[i+1:] {
			if x.Role == "tool" && x.ToolCallID != "" {
				answered[x.ToolCallID] = true
			}
		}
		for _, c := range m.ToolCalls {
			if !answered[c.ID] {
				t.Fatalf("第 %d 条 assistant 的 tool_call %s 没有对应的 tool 回复（端点会 400）", i+1, c.ID)
			}
		}
	}
}

// 中断在工具执行中途时，历史以「带 tool_calls 的 assistant」结尾。
// 旧逻辑把后面的 tool 回复删了、却留着这条 assistant——
// 下一轮请求 400，而且历史已落盘，会话从此说不了话。
func TestSanitizeTailRepairsInterruptedToolCall(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "开始体检"},
		assistantWithCalls("call_1"),
	}

	got := sanitizeTail(msgs)
	assertNoDanglingToolCalls(t, got)

	var repaired bool
	for _, m := range got {
		if m.Role == "tool" && m.ToolCallID == "call_1" && strings.Contains(m.Content, "中断") {
			repaired = true
		}
	}
	if !repaired {
		t.Fatalf("应当补上说明被中断的合成回复：%+v", got)
	}
}

// 多个 tool_calls 只回答了一半（中断打断了循环中间）：
// 缺的那几个要补，已有的不能动。
func TestSanitizeTailRepairsPartiallyAnsweredCalls(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q"},
		assistantWithCalls("call_1", "call_2"),
		toolReply("call_1", "第一个的结果"),
	}

	got := sanitizeTail(msgs)
	assertNoDanglingToolCalls(t, got)

	for _, m := range got {
		if m.ToolCallID == "call_1" && m.Content != "第一个的结果" {
			t.Fatalf("已回答的 call_1 不该被动过：%q", m.Content)
		}
	}
}

// 完整的历史不能被改动：修整只针对半截轮。
func TestSanitizeTailKeepsCompleteHistory(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q"},
		assistantWithCalls("call_1"),
		toolReply("call_1", "结果"),
		{Role: "assistant", Content: "结论"},
	}
	got := sanitizeTail(msgs)
	if len(got) != len(msgs) {
		t.Fatalf("完整历史不该被动过：%+v", got)
	}
	assertNoDanglingToolCalls(t, got)
}

// 中断发生在模型开口之前：尾部是一条没人回答的 user。
// 旧逻辑同样处理不了它（会留着，下一轮拼出连续两条 user）。
func TestSanitizeTailDropsUnansweredUser(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2 没被回答"},
	}
	got := sanitizeTail(msgs)
	if len(got) != 2 || got[len(got)-1].Role == "user" {
		t.Fatalf("没被回答的 user 应当被丢掉：%+v", got)
	}
}

// 端到端：Restore 一份"中断时落盘"的坏历史，重建出来的必须合法。
// 这是用户实际踩到的路径——中断一次，之后整个会话都发不出请求。
func TestRestoreRepairsInterruptedSession(t *testing.T) {
	c := NewConversation()
	c.Restore([]Message{
		{Role: "user", Content: "开始体检"},
		assistantWithCalls("call_1"),
	})
	assertNoDanglingToolCalls(t, c.History())

	// 修复用的合成回复要留在历史里：下一轮模型能看到"上次被中断了"
	var repaired bool
	for _, m := range c.History() {
		if m.Role == "tool" && m.ToolCallID == "call_1" {
			repaired = true
		}
	}
	if !repaired {
		t.Fatal("合成回复应当留在历史里，模型需要知道上次被中断")
	}
}

// 恢复之后再跑一轮，历史要仍然合法（修复件不破坏后续追加）。
func TestContinueAfterRepairedRestore(t *testing.T) {
	c := NewConversation()
	c.Restore([]Message{
		{Role: "user", Content: "开始体检"},
		assistantWithCalls("call_1"),
	})

	c.Set(append(c.History(),
		Message{Role: "user", Content: "还在吗"},
		(Message{Role: "assistant", Content: "在的"}),
	))
	assertNoDanglingToolCalls(t, c.History())
	if got := c.History(); len(got) == 0 || got[len(got)-1].Content != "在的" {
		t.Fatalf("修复后要能继续对话：%+v", got)
	}
}
