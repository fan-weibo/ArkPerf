package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// resize 模拟窗口尺寸消息（真实启动时 bubbletea 会先发这个）。
func (h *harness) resize(w, ht int) {
	h.m.Update(tea.WindowSizeMsg{Width: w, Height: ht})
}

// 启动恢复的标准形态：横幅在最上，历史接在后面。
//
// 不做回放的话，提示语写着"已恢复上次会话：N 轮"、屏幕却是空的——
// 用户看到的是"说有历史、什么都没有"，最容易理解成数据丢了（实测被当场问到）。
func TestInitReplaysRestoredHistory(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.StartupNotice = "arkperf · test-model · 43 工具\n已恢复上次会话：3 轮 · 10-05 06:06"
	h.m.opts.InitialReplay = []ReplayLine{
		{Role: "user", Content: "上次问的问题"},
		{Role: "assistant", Content: "上次的回答"},
		{Role: "tool", Name: "read_file", Content: `{"path":"a.txt"}`},
		{Role: "result", Name: "read_file", Content: "文件内容"},
	}
	h.m.pendingReplay = h.m.opts.InitialReplay

	h.m.Init()
	h.resize(120, 40)

	out := h.allTranscript()
	for _, want := range []string{"已恢复上次会话", "上次问的问题", "上次的回答", "read_file", "文件内容"} {
		if !strings.Contains(out, want) {
			t.Fatalf("启动后转录里缺少 %q：\n%s", want, out)
		}
	}

	// 横幅必须在历史**之前**：反过来看会以为横幅是历史的一部分
	if strings.Index(out, "已恢复上次会话") > strings.Index(out, "上次问的问题") {
		t.Fatalf("横幅应当排在历史之前：\n%s", out)
	}
}

// **尺寸到达之前不能渲染历史**：那时 m.width 是 0，折行会退回 20 列兜底宽度，
// 表现就是"历史全挤在左边一条窄栏里"（实测被用户当场指出）。
func TestReplayWaitsForWindowSize(t *testing.T) {
	h := newHarness(t, false)
	// 40 个宽字符 = 80 显示列：装得进 120 列，装不进 20 列的兜底宽度
	long := strings.Repeat("宽", 40)
	h.m.pendingReplay = []ReplayLine{{Role: "assistant", Content: long}}

	h.m.Init()
	if out := h.allTranscript(); strings.Contains(out, "宽宽") {
		t.Fatalf("尺寸未知时不该渲染历史：\n%s", out)
	}

	h.resize(120, 40)

	// 渲染出来后必须是**完整一行**，而不是被折成窄栏
	out := h.allTranscript()
	if !strings.Contains(out, long) {
		t.Fatalf("历史应当按 120 列渲染成完整一行（不该折在 20 列里）：\n%s", out)
	}

	// 再收到尺寸消息也不该重复渲染
	before := h.allTranscript()
	h.resize(100, 30)
	if after := h.allTranscript(); after != before {
		t.Fatalf("历史只该渲染一次：\n之前：%s\n之后：%s", before, after)
	}
}

// 没有可恢复的历史时，只有横幅，不该凭空多出内容。
func TestInitWithoutReplayShowsOnlyBanner(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.StartupNotice = "arkperf · test-model\n新会话。"
	h.m.pendingReplay = nil

	h.m.Init()
	h.resize(100, 30)

	out := h.allTranscript()
	if !strings.Contains(out, "新会话") {
		t.Fatalf("横幅没打出来：%q", out)
	}
}

// 回放之后 assistantOpen 要复位：下一段实时回答该重新挂头部，
// 否则新会话的第一段回答会"续"在历史后面而没有标识。
func TestAppendReplayResetsAssistantHeader(t *testing.T) {
	h := newHarness(t, false)
	h.m.resizeInput()
	h.m.assistantOpen = true

	h.m.appendReplay([]ReplayLine{{Role: "assistant", Content: "历史回答"}})

	if h.m.assistantOpen {
		t.Fatal("回放后 assistantOpen 应当复位")
	}
}
