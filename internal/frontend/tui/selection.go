package tui

import (
	"encoding/base64"
	"fmt"
	"strings"

	"charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"
)

// 这份文件实现"app 内文本选中"，学自 Reasonix（internal/cli/transcript.go）。
//
// 为什么必须有它：终端的原生划选和"app 收滚轮"是**互斥**的——一旦开了
// 鼠标捕获（MouseModeCellMotion）拿滚轮事件，终端就不再自己处理鼠标，
// 原生划选随之失效。Reasonix 的解法不是二选一，而是**自己把选中做一遍**：
// 左键按下记锚点 → 拖动延伸 → 反色高亮标出选区。这样滚轮/滚动条和"选中"
// 可以同时存在；/mouse 只是给 SSH/Termux 这类"终端本地复制更靠谱"的环境留的退路。
//
// 复制时机：Reasonix 是**松手即复制**，这里改成**只有 Ctrl+C / Ctrl+Insert 才复制**
// （按用户要求）——选中只是圈出来，不会手一抖就覆盖剪贴板，选完还能接着调。
//
// 坐标一律用**内容绝对行号**（与滚动位置无关），列用可视列。
// 内容行在 renderTranscript 里按 bodyW 折好，所以屏幕列 == 可视列。

// selPos 是选区里的一个基点：内容绝对行号 + 可视列。
type selPos struct{ line, col int }

// selection 是正在进行的左键拖选。
// anchor 是按下处，head 是当前处；active 控制渲染与复制。
type selection struct {
	active bool
	anchor selPos
	head   selPos
}

// ordered 返回按阅读顺序排好的起止点。
func (s selection) ordered() (start, end selPos) {
	if s.anchor.line > s.head.line || (s.anchor.line == s.head.line && s.anchor.col > s.head.col) {
		return s.head, s.anchor
	}
	return s.anchor, s.head
}

// isEmpty 表示只是一次点击（没有拖动）。
func (s selection) isEmpty() bool { return s.anchor == s.head }

// selStyle 是选区高亮：反色。不去改前景色——反色在任何主题下都清晰，
// 且能盖住底下原本的着色（StyleRanges 用去 ANSI 后的文本渲染）。
var selStyle = lipgloss.NewStyle().Reverse(true)

// selSpan 算出内容行 idx 上选区覆盖的 [lo, hi) 可视列区间。
// ok=false 表示这行不在选区内。cw 限制右边界，多行选区才能铺到屏幕右缘。
func selSpan(idx int, start, end selPos, cw int) (lo, hi int, ok bool) {
	if idx < start.line || idx > end.line {
		return 0, 0, false
	}
	lo, hi = 0, cw
	if idx == start.line {
		lo = start.col
	}
	if idx == end.line {
		hi = end.col
	}
	hi = min(hi, cw)
	if lo >= hi {
		return 0, 0, false
	}
	return lo, hi, true
}

// highlightRange 给 line 的 [lo, hi) 列打上反色。先补空格到 hi 列，
// 反色块才能延伸到选区右边界（否则一行末尾的高亮会断在字符处）。
func highlightRange(line string, lo, hi, cw int) string {
	hi = min(hi, cw)
	if lo >= hi {
		return line
	}
	if w := ansi.StringWidth(line); w < hi {
		line += strings.Repeat(" ", hi-w)
	}
	return lipgloss.StyleRanges(line, lipgloss.NewRange(lo, hi, selStyle))
}

// transcriptCaret 把屏幕坐标（转录视口内，0 基）映射成内容绝对位置。
//
// 依赖上一次渲染记录的 lastTop：转录从视口顶部开始画，所以
// 内容行 = lastTop + y。真实循环里渲染永远在输入之前，这个前提成立。
func (m *Model) transcriptCaret(x, y int) selPos {
	line := clamp(m.lastTop+y, 0, max(len(m.contentLines)-1, 0))
	return selPos{line: line, col: max(x, 0)}
}

// inTranscript 判断某屏幕坐标是否落在转录视口里（不含右侧滚动条列）。
func (m *Model) inTranscript(x, y int) bool {
	return y >= 0 && y < m.viewportRows && x >= 0 && x < m.contentW
}

// selectedText 取出当前选区的纯文本。
//
// 按可视列切片（不是 rune 下标），中文才不会切错；逐行取，行间用 \n 连。
func (m *Model) selectedText() string {
	start, end := m.sel.ordered()
	var out []string
	for i := max(start.line, 0); i <= end.line && i < len(m.contentLines); i++ {
		plain := ansi.Strip(m.contentLines[i])
		lo, hi := 0, ansi.StringWidth(plain)
		if i == start.line {
			lo = start.col
		}
		if i == end.line {
			hi = end.col
		}
		out = append(out, sliceByCol(plain, lo, hi))
	}
	return strings.Join(out, "\n")
}

// sliceByCol 按可视列 [lo, hi) 切出子串。宽字符按 2 列算，
// 零宽字符（组合符）跟着前一个字符走。
func sliceByCol(s string, lo, hi int) string {
	if hi <= lo {
		return ""
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		w := ansi.StringWidth(string(r))
		if w == 0 {
			if col >= lo && col < hi {
				b.WriteRune(r)
			}
			continue
		}
		// 与选区有交叠的整字符都取（宽字符不会被切一半）
		if col+w > lo && col < hi {
			b.WriteRune(r)
		}
		col += w
		if col >= hi {
			break
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- 剪贴板

// copiedMsg 是异步复制的结果。
type copiedMsg struct {
	text string
	n    int
	err  error
}

// writeClipboardText 是本地剪贴板写入，测试可替换。
var writeClipboardText = clipboard.WriteAll

// copyText 把文本复制到剪贴板：本地优先走系统剪贴板（能验证成没成），
// 失败或远程会话时退回 OSC52（终端协议，把文本写进用户本地剪贴板）。
func copyText(text string) tea.Cmd {
	return func() tea.Msg {
		err := writeClipboardText(text)
		return copiedMsg{text: text, n: len([]rune(text)), err: err}
	}
}

// osc52 是 OSC52 复制转义序列（base64 编码，避免非 ASCII 出问题）。
func osc52(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
}

// copyResultCmd 把复制结果变成给用户的反馈：
// 成功就报字数；本地失败则退回 OSC52，并如实说明走的是终端协议。
func (m *Model) copyResultCmd(msg copiedMsg) tea.Cmd {
	if msg.err != nil {
		return tea.Batch(
			tea.Raw(osc52(msg.text)),
			m.print(styleDim.Render(fmt.Sprintf("已复制 %d 个字符（经终端 OSC52）", msg.n))),
		)
	}
	return m.print(styleDim.Render(fmt.Sprintf("已复制 %d 个字符到剪贴板", msg.n)))
}
