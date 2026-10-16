package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// dialog 是模态覆盖层。
//
// 只做"列表 + 选中 + 确认"这一种形态，因为现在的用途（会话列表、主题列表）
// 都是它。opencode 那十几个 dialog 里真正需要输入的（改会话名、填配置项）
// 我们还没有对应功能，等有了再按需扩展——先做通用输入框只会变成没人用的脚手架。
//
// 模态的含义：打开期间所有按键都归它，不会被底下的输入框吃掉。
// 这也是它必须尽早拦在 handleKey 前面的原因。
type dialog struct {
	title string
	hint  string
	items []dialogItem
	idx   int
	// onPick 在确认某项时调用。返回的命令继续执行（通常是一句回显）。
	onPick func(idx int) tea.Cmd
}

type dialogItem struct {
	label string // 主文本
	meta  string // 次要文本（跟在后面，用暗色）
}

// move 上下移动选中项；环形滚动，到头继续按会绕回另一端。
func (d *dialog) move(delta int) {
	if len(d.items) == 0 {
		return
	}
	d.idx = (d.idx + delta + len(d.items)) % len(d.items)
}

// dialogWindow 是弹窗里最多同时显示多少项。
//
// 超出的部分靠滚动窗口看——不把整张列表铺开是因为弹窗高度一旦超过屏幕，
// 居中和覆盖就都没法算了（会直接盖住状态行）。
const dialogWindow = 12

// boxWidth 是弹窗宽度：够放内容，但两侧必须留出空间。
//
// 留白是弹窗能被"看出来是弹出来的"的唯一线索——占满整屏的卡片
// 和切了一屏没有区别，用户会以为界面变了。
func (d *dialog) boxWidth(w int) int {
	maxW := ansi.StringWidth(d.title) + 6
	for _, it := range d.items {
		if n := ansi.StringWidth(it.label) + ansi.StringWidth(it.meta) + 8; n > maxW {
			maxW = n
		}
	}
	limit := max(w-8, 20)
	return clamp(maxW, 24, limit)
}

// render 把弹窗渲染成 w 宽的一组行（左右补白，居中）。
func (d *dialog) render(w int) []string {
	bw := d.boxWidth(w)
	inner := max(bw-4, 12) // 减边框 2 + 内边距 2

	var lines []string
	lines = append(lines, styleAccent.Bold(true).Render(ansi.Truncate(d.title, inner, "…")))
	lines = append(lines, "")

	start := 0
	if d.idx >= dialogWindow {
		start = d.idx - dialogWindow + 1
	}
	end := min(start+dialogWindow, len(d.items))
	for i := start; i < end; i++ {
		it := d.items[i]
		cursor, label := "  ", styleText
		if i == d.idx {
			cursor, label = styleAccent.Render("❯ "), styleAccent.Bold(true)
		}
		meta := ""
		if it.meta != "" {
			meta = "  " + styleDim.Render(it.meta)
		}
		lines = append(lines, ansi.Truncate(cursor+label.Render(it.label)+meta, inner, "…"))
	}
	if end < len(d.items) {
		lines = append(lines, styleDim.Render(fmt.Sprintf("  … 还有 %d 项", len(d.items)-end)))
	}
	if start > 0 {
		lines = append(lines, styleDim.Render(fmt.Sprintf("  … 上面还有 %d 项", start)))
	}
	if d.hint != "" {
		lines = append(lines, "", styleDim.Render(ansi.Truncate(d.hint, inner, "…")))
	}

	box := cardStyle.Width(inner).Render(strings.Join(lines, "\n"))
	boxLines := strings.Split(box, "\n")
	pad := max((w-ansi.StringWidth(boxLines[0]))/2, 0)
	out := make([]string, 0, len(boxLines))
	for _, l := range boxLines {
		out = append(out, strings.Repeat(" ", pad)+l)
	}
	return out
}

// overlayDialog 把弹窗按行覆盖到整屏中间。
//
// 按**整行**替换而不是按列插入：整行替换不用管底下内容里的 ANSI，
// 只要把整行重写就行；按列插入得先把原行按可视列切开再拼，
// 中文宽字符会让那个换算到处出错。代价是弹窗左右两侧会变成空白，
// 而那正是"弹出了一层"该有的样子。
func overlayDialog(screen []string, dlg []string, w int) []string {
	if len(dlg) == 0 {
		return screen
	}
	start := max((len(screen)-len(dlg))/2, 0)
	out := append([]string(nil), screen...)
	for i, l := range dlg {
		row := start + i
		if row < 0 || row >= len(out) {
			break
		}
		out[row] = padTo(l, w)
	}
	return out
}

// handleDialogKey 处理弹窗上的按键。
//
// 模态：未识别的键**一律吞掉**，不往下走到输入框——否则用户在弹窗里
// 顺手打字会同时改到对话输入框，关掉弹窗才发现输入框里多了半句话。
func (m *Model) handleDialogKey(k string) (tea.Model, tea.Cmd) {
	d := m.dialog
	switch k {
	case "esc", "ctrl+c", "ctrl+o", "ctrl+t":
		// 再按一次"打开它的那个键"也关闭——与"按同一个键收起侧栏"同一套直觉
		m.dialog = nil
		return m, nil
	case "up", "k":
		d.move(-1)
		return m, nil
	case "down", "j":
		d.move(1)
		return m, nil
	case "enter":
		m.dialog = nil
		if d.onPick != nil {
			return m, d.onPick(d.idx)
		}
		return m, nil
	}
	return m, nil
}

// ---------------------------------------------------------------- 具体弹窗

// openSessionDialog 打开会话列表弹窗（Ctrl+P）。
//
// 左栏已经能点会话了，弹窗的价值在于**能一眼看全**：
// 侧栏放不下十来个会话，弹窗里可以。
func (m *Model) openSessionDialog() (tea.Model, tea.Cmd) {
	m.refreshSessions()
	if len(m.sessionsCache) == 0 {
		return m, m.print(styleDim.Render("（当前工作区暂无会话）"))
	}
	items := make([]dialogItem, 0, len(m.sessionsCache))
	for _, s := range m.sessionsCache {
		meta := sessionMeta(s)
		if s.Current {
			meta = "当前 · " + meta
		}
		items = append(items, dialogItem{label: sessionTitle(s), meta: meta})
	}
	m.dialog = &dialog{
		title: fmt.Sprintf("会话（%d）", len(items)),
		hint:  "↑↓ 选择 · ⏎ 切换 · Esc 关闭",
		items: items,
		idx:   max(m.sessionIdx, 0),
		onPick: func(i int) tea.Cmd {
			_, cmd := m.openSessionIdx(i)
			return cmd
		},
	}
	return m, nil
}

// openThemeDialog 打开主题列表弹窗（Ctrl+T）。
func (m *Model) openThemeDialog() (tea.Model, tea.Cmd) {
	names := themeNames()
	items := make([]dialogItem, 0, len(names))
	idx := 0
	for i, n := range names {
		meta := ""
		if n == m.themeName {
			idx = i
			meta = "当前"
		}
		items = append(items, dialogItem{label: n, meta: meta})
	}
	m.dialog = &dialog{
		title: "主题",
		hint:  "↑↓ 选择 · ⏎ 应用 · Esc 关闭",
		items: items,
		idx:   idx,
		onPick: func(i int) tea.Cmd {
			m.setTheme(names[i])
			return m.print(styleDim.Render("主题：" + m.themeName))
		},
	}
	return m, nil
}
