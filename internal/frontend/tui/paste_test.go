package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// 终端里的 Ctrl+V 走的是**括号粘贴**：bubbletea 把它作为独立的 PasteMsg
// 交上来，而不是一串按键。不处理它，用户按 Ctrl+V 就是"完全没反应"
// （实测被用户当场问到"tui 模式没有 ctrl+v 粘贴吗"）。
func TestPasteInsertsIntoInput(t *testing.T) {
	h := newHarness(t, false)

	h.m.Update(tea.PasteMsg{Content: "对 com.example.perflab 做一次完整体验体检"})

	got := h.m.input.Value()
	if !strings.Contains(got, "com.example.perflab") {
		t.Fatalf("粘贴内容没进输入框：%q", got)
	}
}

// 多行粘贴要原样进去：任务描述经常是从别处整段拷来的。
func TestPasteKeepsMultipleLines(t *testing.T) {
	h := newHarness(t, false)

	h.m.Update(tea.PasteMsg{Content: "第一行\n第二行"})

	got := h.m.input.Value()
	if !strings.Contains(got, "第一行") || !strings.Contains(got, "第二行") {
		t.Fatalf("多行内容被截断了：%q", got)
	}
}

// 审批卡是模态的：此时粘贴不该越过它写进输入框。
func TestPasteIgnoredWhileApproving(t *testing.T) {
	h := newHarness(t, false)

	// 直接切到审批态（真实路径由 approval 事件驱动，这里只关心模态性）
	h.m.st = stateApproving
	h.m.Update(tea.PasteMsg{Content: "偷偷塞进去"})

	if got := h.m.input.Value(); strings.Contains(got, "偷偷塞进去") {
		t.Fatalf("审批态不该接受粘贴：%q", got)
	}
}
