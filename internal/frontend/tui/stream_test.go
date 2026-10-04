package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// delta 模拟一次流式增量到达。
func (h *harness) delta(kind kernel.DeltaKind, text string) {
	h.m.Update(event{kind: evDelta, delta: kind, text: text})
}

// assistant 模拟一轮的文字定稿。
func (h *harness) assistant(text string) {
	h.m.Update(event{kind: evAssistant, text: text})
}

// 流式最容易出的错是"打两遍"：增量已经显示过，定稿时又完整打一次。
// 这条守的就是它。
func TestStreamedContentRendersExactlyOnce(t *testing.T) {
	h := newHarness(t, false)
	h.delta(kernel.DeltaContent, "结论")
	h.delta(kernel.DeltaContent, "是没有泄漏")
	h.assistant("结论是没有泄漏")

	out := h.allTranscript()
	if n := strings.Count(out, "结论是没有泄漏"); n != 1 {
		t.Fatalf("内容应当只出现一次，实际 %d 次：%q", n, out)
	}
	if n := strings.Count(out, agentMarker+" arkperf"); n != 1 {
		t.Fatalf("头部应当只出现一次，实际 %d 次：%q", n, out)
	}
}

// 活块的要点：多段增量只占**一个**转录元素，而不是一段一行。
// 一段一行会被折行规则切碎，定稿时还得回头收拾碎片。
func TestStreamKeepsOneTranscriptBlock(t *testing.T) {
	h := newHarness(t, false)
	before := len(h.m.transcript)

	h.delta(kernel.DeltaContent, "第一段")
	h.delta(kernel.DeltaContent, "第二段")
	h.delta(kernel.DeltaContent, "第三段")

	if got := len(h.m.transcript) - before; got != 1 {
		t.Fatalf("三段增量应当只占一个块，实际多了 %d 个：%q", got, h.allTranscript())
	}
	if !strings.Contains(h.allTranscript(), "第一段第二段第三段") {
		t.Fatalf("增量应当被累积：%q", h.allTranscript())
	}
}

// 增量与完整文本有出入时，以完整文本为准——否则屏幕上留下的
// 会和下一轮上下文里的不一样。
func TestFinalTextWinsOverDeltas(t *testing.T) {
	h := newHarness(t, false)
	h.delta(kernel.DeltaContent, "结论是甲")
	h.assistant("结论是乙")

	out := h.allTranscript()
	if !strings.Contains(out, "结论是乙") {
		t.Fatalf("应当以定稿文本为准：%q", out)
	}
	if strings.Contains(out, "结论是甲") {
		t.Fatalf("旧增量不该留在屏幕上：%q", out)
	}
}

// 思考过程不进转录：它常常比回答长一个数量级，全留在转录里会把结论冲得看不见。
func TestReasoningDeltasStayOutOfTranscript(t *testing.T) {
	h := newHarness(t, false)
	h.delta(kernel.DeltaReasoning, "先看内存")
	h.delta(kernel.DeltaReasoning, "再看启动")

	if out := h.allTranscript(); strings.Contains(out, "先看内存") {
		t.Fatalf("思考过程不该进转录：%q", out)
	}
	// 但"它在想"必须可见，否则流式刚开始那几秒屏幕像卡住了
	h.m.st = stateRunning
	if line := h.m.workingLine(120); !strings.Contains(line, "思考中 8 字") {
		t.Fatalf("运行指示行应当报告思考字数：%q", line)
	}
}

func TestReasoningIndicatorClearsWhenContentStarts(t *testing.T) {
	h := newHarness(t, false)
	h.delta(kernel.DeltaReasoning, "想了一会儿")
	h.delta(kernel.DeltaContent, "结论")

	if h.m.reasoningChars != 0 {
		t.Fatalf("开始产出可见内容后不该再有思考计数，实际 %d", h.m.reasoningChars)
	}
}

// 同一轮里模型可能"答一段 → 调工具 → 再答一段"。
// 头部只在轮首打一次，流式不能破坏这条。
func TestStreamHeaderPrintedOncePerTurn(t *testing.T) {
	h := newHarness(t, false)

	h.delta(kernel.DeltaContent, "先看设备")
	h.assistant("先看设备")
	h.m.Update(event{kind: evToolCall, name: "harmony_devices"})
	h.m.Update(event{kind: evToolResult, name: "harmony_devices", text: "无设备"})
	h.delta(kernel.DeltaContent, "设备没连上")
	h.assistant("设备没连上")

	out := h.allTranscript()
	if n := strings.Count(out, agentMarker+" arkperf"); n != 1 {
		t.Fatalf("同一轮里头部只该出现一次，实际 %d 次：%q", n, out)
	}
	if !strings.Contains(out, "先看设备") || !strings.Contains(out, "设备没连上") {
		t.Fatalf("两段内容都该在：%q", out)
	}
}

// 中断时不会有 OnAssistant，活块就停在半截。
// 已经流出来的内容是真实发生过的，要留住，只是不再当它是"活的"。
func TestInterruptKeepsPartialStream(t *testing.T) {
	h := newHarness(t, false)
	h.delta(kernel.DeltaContent, "话说到一半")

	h.m.Update(event{kind: evDone, result: kernel.LoopResult{Reason: kernel.StopInterrupted}})

	if !strings.Contains(h.allTranscript(), "话说到一半") {
		t.Fatalf("中断后已流出的内容应当保留：%q", h.allTranscript())
	}
	if h.m.streamIdx != -1 {
		t.Fatalf("任务结束后不该还有活块，streamIdx=%d", h.m.streamIdx)
	}
}

// 极少见但必须处理：流出来又变空。留一行只有头部的空白会被当成渲染 bug。
func TestEmptyFinalTextDropsBlock(t *testing.T) {
	h := newHarness(t, false)
	h.delta(kernel.DeltaContent, "临时内容")
	h.assistant("   ")

	if out := h.allTranscript(); strings.Contains(out, "临时内容") {
		t.Fatalf("空定稿应当把整块撤掉：%q", out)
	}
}

// 不挂 OnDelta 的调用方（桌面端、一次性执行）走的是老路，行为必须完全不变。
func TestNonStreamingAssistantStillWorks(t *testing.T) {
	h := newHarness(t, false)
	h.assistant("整段回答")

	out := h.allTranscript()
	if !strings.Contains(out, "整段回答") {
		t.Fatalf("非流式路径不该受影响：%q", out)
	}
	if !strings.Contains(out, agentMarker+" arkperf") {
		t.Fatalf("非流式路径仍要打头部：%q", out)
	}
}

// /clear 与开始新任务都要把活块收掉，否则下一轮的增量会写进上一轮的块里。
func TestStreamStateResetsBetweenTasks(t *testing.T) {
	h := newHarness(t, false)
	h.delta(kernel.DeltaContent, "上一轮的话")

	h.m.resetViewState()

	if h.m.streamIdx != -1 || h.m.streamBuf != "" || h.m.streamHead != "" {
		t.Fatalf("清屏后流式状态应当复位：%d %q %q", h.m.streamIdx, h.m.streamBuf, h.m.streamHead)
	}
	// 清屏后新一轮的增量应当重新打头部（assistantOpen 也复位了）
	h.delta(kernel.DeltaContent, "这一轮的话")
	if !strings.Contains(h.allTranscript(), agentMarker+" arkperf") {
		t.Fatalf("新一轮应当重新打头部：%q", h.allTranscript())
	}
}

// Bridge 必须把 OnDelta 挂上：不挂的话内核根本不会走流式，
// 表现为"界面看起来支持流式，但模型还是等说完才一次性吐出来"。
func TestBridgeWiresOnDelta(t *testing.T) {
	h := newHarness(t, false)
	var got []event
	h.b.SetEmit(func(msg tea.Msg) {
		if ev, ok := msg.(event); ok {
			got = append(got, ev)
		}
	})

	ev := h.b.Events()
	if ev.OnDelta == nil {
		t.Fatal("Bridge.Events 必须挂上 OnDelta，否则内核不会流式")
	}
	if ev.OnAssistant == nil {
		t.Fatal("OnAssistant 仍然要挂——它是定稿通知")
	}

	ev.OnDelta(kernel.DeltaReasoning, "想")
	ev.OnDelta(kernel.DeltaContent, "说")
	ev.OnAssistant("说")

	if len(got) != 3 {
		t.Fatalf("事件数 = %d，期望 3：%+v", len(got), got)
	}
	if got[0].kind != evDelta || got[0].delta != kernel.DeltaReasoning {
		t.Fatalf("第一件事应当是 reasoning 增量：%+v", got[0])
	}
	if got[1].kind != evDelta || got[1].delta != kernel.DeltaContent {
		t.Fatalf("第二件应当是 content 增量：%+v", got[1])
	}
	if got[2].kind != evAssistant || got[2].text != "说" {
		t.Fatalf("第三件应当是定稿：%+v", got[2])
	}
}
