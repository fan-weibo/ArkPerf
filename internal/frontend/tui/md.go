package tui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// mdRenderer 把模型的 markdown 回答渲染成带 ANSI 样式的终端文本。
// 这是移植自 Reasonix（internal/cli/md.go）的精简版：只实现聊天模型
// 稳定会输出的构造——标题、段落、列表、围栏代码、引用、表格、
// 粗体/斜体/行内代码、链接、分隔线——其余降级为纯文本，绝不吃字。
//
// 之所以不用 glamour：它的样式自带边距和换行策略，和转录视口的
// 折行/配色对不上；自己走 goldmark AST 反而可控（Reasonix 同款选择）。
//
// 折行尊重 CJK 宽度（宽字符算 2 列），SGR 转义按 0 列计。
type mdRenderer struct {
	md    goldmark.Markdown
	width int
}

func newMarkdownRenderer(width int) *mdRenderer {
	if width <= 0 {
		width = 80
	}
	// 开 GFM 表格扩展：| 表头 | 行 | 会被解析成 Table 节点而不是字面文本
	return &mdRenderer{
		md:    goldmark.New(goldmark.WithExtensions(extension.Table)),
		width: width,
	}
}

// Render 解析 markdown 并返回带样式的文本。空输入返回空串。
func (r *mdRenderer) Render(input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}
	input = fixCJKEmphasis(input)
	src := []byte(input)
	doc := r.md.Parser().Parse(text.NewReader(src))
	var buf strings.Builder
	r.renderBlocks(&buf, doc, src, 0)
	out := strings.TrimRight(buf.String(), "\n")
	if out == "" {
		return ""
	}
	return out
}

// fixCJKEmphasis 绕过 goldmark（CommonMark）不把 CJK 标点当作 Unicode 标点的
// 问题：闭合 ** 只有在前一个字符是标点时才算右翼分界，于是「**X，**Y」里的
// **X，** 不会被加粗（，是 U+FF0C）。在这类闭合符后面插一个空格即可修复。
// 空格只能插在**闭合符**后——插在开启符后会反过来破坏左翼分界。
// 行内代码与围栏代码块原样跳过，代码里的字面 ** 不受影响。
func fixCJKEmphasis(s string) string {
	runes := []rune(s)
	n := len(runes)
	var b strings.Builder
	b.Grow(len(s) + 16)

	inFenced := false   // 在 ``` 围栏代码块里
	inCode := false     // 在 ` 行内代码里
	inEmphasis := false // 在一对 ** 之间

	for i := 0; i < n; i++ {
		r := runes[i]

		// ``` 切换围栏进出
		if r == '`' && i+2 < n && runes[i+1] == '`' && runes[i+2] == '`' {
			inFenced = !inFenced
			b.WriteString("```")
			i += 2
			continue
		}
		// ` 切换行内代码进出（围栏内不算）
		if r == '`' && !inFenced {
			inCode = !inCode
			b.WriteRune(r)
			continue
		}
		// 代码内原样通过
		if inCode || inFenced {
			b.WriteRune(r)
			continue
		}
		// 强调不能跨硬换行：复位，免得上一行没闭合的 ** 影响下一行
		if r == '\n' {
			inEmphasis = false
			b.WriteRune(r)
			continue
		}

		if r == '*' && i+1 < n && runes[i+1] == '*' {
			b.WriteString("**")
			i++
			inEmphasis = !inEmphasis

			// 只有闭合符（强调刚结束）紧贴 CJK 标点才需要补空格
			if !inEmphasis && i >= 2 && !isASCIISpace(runes[i-2]) && isCJKPunct(runes[i-2]) {
				b.WriteByte(' ')
			}
			continue
		}

		b.WriteRune(r)
	}
	return b.String()
}

// isCJKPunct 判断 r 是否为 CJK 全角标点。
// CommonMark 规范没把它们归为 Unicode 标点，会破坏右翼分界判断。
func isCJKPunct(r rune) bool {
	if r <= 0x7F {
		return false // ASCII 标点 CommonMark 处理得没问题
	}
	switch {
	case r >= 0x3000 && r <= 0x303F: // CJK 符号和标点（。、等）
		return true
	case r >= 0xFF01 && r <= 0xFF0F: // 全角 forms I
		return true
	case r >= 0xFF1A && r <= 0xFF20: // 全角 forms II
		return true
	case r >= 0xFF3B && r <= 0xFF3F: // 全角 forms III
		return true
	case r >= 0xFF5B && r <= 0xFF65: // 全角 forms IV
		return true
	}
	return unicode.IsPunct(r)
}

func isASCIISpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

// visibleWidth 计算显示宽度：SGR 转义 0 列，CJK 宽字符 2 列。
func visibleWidth(s string) int { return ansi.StringWidth(s) }

func (r *mdRenderer) renderBlocks(buf *strings.Builder, parent ast.Node, src []byte, indent int) {
	for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
		r.renderBlock(buf, c, src, indent)
	}
}

func (r *mdRenderer) renderBlock(buf *strings.Builder, node ast.Node, src []byte, indent int) {
	switch n := node.(type) {
	case *ast.Heading:
		r.renderHeading(buf, n, src, indent)
	case *ast.Paragraph:
		r.renderInlineBlock(buf, n, src, indent, true)
	case *ast.TextBlock:
		// TextBlock 是 goldmark 给紧凑列表项装行内内容的容器（无尾空行）
		r.renderInlineBlock(buf, n, src, indent, false)
	case *ast.List:
		r.renderList(buf, n, src, indent)
	case *ast.FencedCodeBlock, *ast.CodeBlock:
		r.renderFenced(buf, n, src, indent)
	case *ast.Blockquote:
		r.renderBlockquote(buf, n, src, indent)
	case *extast.Table:
		r.renderTable(buf, n, src, indent)
	case *ast.ThematicBreak:
		w := max(r.width-indent, 8)
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString(styleDim.Render(strings.Repeat("─", w)))
		buf.WriteString("\n\n")
	default:
		// 未知块：递归子节点而不是丢内容
		r.renderBlocks(buf, node, src, indent)
	}
}

func (r *mdRenderer) renderHeading(buf *strings.Builder, n *ast.Heading, src []byte, indent int) {
	inline := r.collectInline(n, src)
	buf.WriteString(strings.Repeat(" ", indent))
	buf.WriteString(styleBold.Render(styleAccent.Render(inline)))
	buf.WriteString("\n")
	// 一级标题加下划线；更深的层级靠加粗+颜色就够层级分明，
	// 长回答里满屏 ### 再叠视觉重量反而闹。
	if n.Level == 1 {
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString(styleAccent.Render(strings.Repeat("─", max(visibleWidth(inline), 1))))
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
}

func (r *mdRenderer) renderInlineBlock(buf *strings.Builder, n ast.Node, src []byte, indent int, trailingBlank bool) {
	inline := r.collectInline(n, src)
	prefix := strings.Repeat(" ", indent)
	wrapped := ansi.Wrap(inline, max(r.width-indent, 4), "")
	for line := range strings.SplitSeq(wrapped, "\n") {
		buf.WriteString(prefix)
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	if trailingBlank {
		buf.WriteString("\n")
	}
}

func (r *mdRenderer) renderList(buf *strings.Builder, n *ast.List, src []byte, indent int) {
	idx := 1
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		item, ok := c.(*ast.ListItem)
		if !ok {
			continue
		}
		var marker string
		if n.IsOrdered() {
			marker = fmt.Sprintf("%d.", idx)
			idx++
		} else {
			marker = "•"
		}
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString(styleAccent.Render(marker) + " ")
		markerW := visibleWidth(marker) + 1

		first := item.FirstChild()
		// goldmark 用 TextBlock 装紧凑列表项、Paragraph 装松散列表项；
		// 两种都当标记行的载体，行内内容才能贴着项目符号。
		inlineHost := inlineCarrier(first)
		if inlineHost != nil {
			inline := r.collectInline(inlineHost, src)
			wrapped := ansi.Wrap(inline, max(r.width-indent-markerW, 4), "")
			lines := strings.Split(wrapped, "\n")
			buf.WriteString(lines[0] + "\n")
			for _, l := range lines[1:] {
				buf.WriteString(strings.Repeat(" ", indent+markerW))
				buf.WriteString(l + "\n")
			}
			for s := first.NextSibling(); s != nil; s = s.NextSibling() {
				r.renderBlock(buf, s, src, indent+markerW)
			}
		} else {
			buf.WriteString("\n")
			r.renderBlocks(buf, item, src, indent+2)
		}
	}
	buf.WriteString("\n")
}

func (r *mdRenderer) renderFenced(buf *strings.Builder, n ast.Node, src []byte, indent int) {
	prefix := strings.Repeat(" ", indent) + styleDim.Render("│ ")
	for i := range n.Lines().Len() {
		l := n.Lines().At(i)
		line := strings.TrimRight(string(l.Value(src)), "\n")
		buf.WriteString(prefix)
		buf.WriteString(styleAccent.Render(line))
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
}

func (r *mdRenderer) renderBlockquote(buf *strings.Builder, n *ast.Blockquote, src []byte, indent int) {
	var inner strings.Builder
	r.renderBlocks(&inner, n, src, 0)
	prefix := strings.Repeat(" ", indent) + styleDim.Render("▎ ")
	for line := range strings.SplitSeq(strings.TrimRight(inner.String(), "\n"), "\n") {
		buf.WriteString(prefix)
		buf.WriteString(styleDim.Render(line))
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
}

// renderTable 把 GFM 表格铺成终端列，列间用暗色 "│" 竖轨、表头下加 "─┼─"。
// 列宽自适应最宽单元格，并按比例压到终端宽度以内；长单元格折成多行
// （整行撑到最高单元格），内容不丢。
func (r *mdRenderer) renderTable(buf *strings.Builder, n *extast.Table, src []byte, indent int) {
	var header []string
	var rows [][]string

	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch row := c.(type) {
		case *extast.TableHeader:
			header = r.collectCells(row, src)
		case *extast.TableRow:
			rows = append(rows, r.collectCells(row, src))
		}
	}
	if len(header) == 0 && len(rows) == 0 {
		return
	}

	cols := len(header)
	for _, row := range rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	if cols == 0 {
		return
	}

	// 初始宽度按各列最宽内容
	widths := make([]int, cols)
	pick := func(i, w int) {
		if i < cols && w > widths[i] {
			widths[i] = w
		}
	}
	for i, h := range header {
		pick(i, visibleWidth(h))
	}
	for _, row := range rows {
		for i, c := range row {
			pick(i, visibleWidth(c))
		}
	}

	// 压到终端能放下：总宽 = 列宽和 + 分隔符(3*(cols-1)) + 缩进。
	// 超了就按自然宽度比例分——内容多的列多留点。
	available := max(r.width-indent-3*(cols-1), cols*3)
	total := 0
	for _, w := range widths {
		total += w
	}
	if total > available {
		for i := range widths {
			widths[i] = max(widths[i]*available/total, 3)
		}
	}

	prefix := strings.Repeat(" ", indent)
	sep := styleDim.Render(" │ ")

	if len(header) > 0 {
		r.renderTableRow(buf, prefix, sep, header, widths, true)
		buf.WriteString(prefix)
		for i := range widths {
			if i > 0 {
				buf.WriteString(styleDim.Render("─┼─"))
			}
			buf.WriteString(styleDim.Render(strings.Repeat("─", widths[i])))
		}
		buf.WriteByte('\n')
	}
	for _, row := range rows {
		r.renderTableRow(buf, prefix, sep, row, widths, false)
	}
	buf.WriteByte('\n')
}

// renderTableRow 铺一行。任一单元格折行时，整行撑到最高单元格；
// 没内容的格子补空格，让 "│" 竖轨保持对齐。
func (r *mdRenderer) renderTableRow(buf *strings.Builder, prefix, sep string, cells []string, widths []int, isHeader bool) {
	cols := len(widths)
	wrapped := make([][]string, cols)
	maxLines := 1
	for i := range cols {
		var text string
		if i < len(cells) {
			text = cells[i]
		}
		wrapped[i] = strings.Split(ansi.Wrap(text, max(widths[i], 4), ""), "\n")
		if len(wrapped[i]) > maxLines {
			maxLines = len(wrapped[i])
		}
	}
	for line := range maxLines {
		buf.WriteString(prefix)
		for i := range cols {
			if i > 0 {
				buf.WriteString(sep)
			}
			var cell string
			if line < len(wrapped[i]) {
				cell = wrapped[i][line]
			}
			padded := padRight(cell, widths[i])
			if isHeader {
				padded = styleBold.Render(padded)
			}
			buf.WriteString(padded)
		}
		buf.WriteByte('\n')
	}
}

func (r *mdRenderer) collectCells(parent ast.Node, src []byte) []string {
	var out []string
	for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
		if cell, ok := c.(*extast.TableCell); ok {
			out = append(out, strings.TrimSpace(r.collectInline(cell, src)))
		}
	}
	return out
}

// inlineCarrier 返回 n 当它是段落/文本块（都装行内内容）时，否则 nil。
func inlineCarrier(n ast.Node) ast.Node {
	switch n.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		return n
	}
	return nil
}

func (r *mdRenderer) collectInline(n ast.Node, src []byte) string {
	var b strings.Builder
	r.appendInline(&b, n, src)
	return b.String()
}

func (r *mdRenderer) appendInline(b *strings.Builder, n ast.Node, src []byte) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch v := c.(type) {
		case *ast.Text:
			b.Write(v.Segment.Value(src))
			switch {
			case v.HardLineBreak():
				b.WriteByte('\n')
			case v.SoftLineBreak():
				b.WriteByte(' ')
			}
		case *ast.Emphasis:
			var inner strings.Builder
			r.appendInline(&inner, v, src)
			if v.Level == 2 {
				b.WriteString(styleBold.Render(inner.String()))
			} else {
				b.WriteString("\033[3m" + inner.String() + "\033[0m")
			}
		case *ast.CodeSpan:
			var inner strings.Builder
			r.appendInline(&inner, v, src)
			b.WriteString(styleAccent.Render(inner.String()))
		case *ast.Link:
			var inner strings.Builder
			r.appendInline(&inner, v, src)
			b.WriteString(inner.String())
			b.WriteString(styleDim.Render(" (" + string(v.Destination) + ")"))
		case *ast.AutoLink:
			b.WriteString(string(v.URL(src)))
		case *ast.RawHTML:
			// 丢弃：聊天输出里罕见，打出来就是字面转义
		case *ast.String:
			b.Write(v.Value)
		default:
			r.appendInline(b, c, src)
		}
	}
}

// padRight 把 s 右侧补空格到 w 显示宽度（SGR 0 列、CJK 2 列）。
func padRight(s string, w int) string {
	gap := w - visibleWidth(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(" ", gap)
}
