package tui

import (
	"fmt"
	"image/color"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
)

// markdown 渲染交给 glamour（Charm 官方那套）。
//
// 之前是自研的 goldmark 渲染器：能用，但标题/列表/表格/代码块的样式与宽度
// 分配都得自己维护，也没有语法高亮。glamour 把这些都做完了。
//
// 代价：它依赖 **lipgloss v1**（github.com/charmbracelet/lipgloss），
// 与本项目用的 charm.land/lipgloss/v2 不是同一个 module path，可以共存
//（若是同一模块的两个版本就不能共存了）。glamour 只产出字符串、
// 不参与我们的样式与宽度计算，所以两边互不干扰。
//
// 唯一要注意的是**它同样基于 goldmark**，所以 CJK 强调的盲区也一模一样——
// 见 fixCJKEmphasis。

// mdCache 缓存渲染器。
//
// glamour 每次 NewTermRenderer 都要重建一遍样式树，而流式输出时
// 每段增量都会调一次 Render；不缓存的话每次增量都在重建。
var (
	mdMu       sync.Mutex
	mdRenderer *glamour.TermRenderer
	mdWidth    int
	mdTheme    string
)

func markdownRenderer(w int) *glamour.TermRenderer {
	mdMu.Lock()
	defer mdMu.Unlock()
	if mdRenderer != nil && mdWidth == w && mdTheme == currentTheme.Name {
		return mdRenderer
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(glamourStyle()),
		glamour.WithWordWrap(w),
	)
	if err != nil {
		return nil
	}
	mdRenderer, mdWidth, mdTheme = r, w, currentTheme.Name
	return r
}

// renderAssistant 渲染模型的回答：markdown → 带样式的终端文本。
//
// 裸打 markdown 的话 ** 和 ` 全是字面字符（实测被用户吐槽过）。
func renderAssistant(text string, w int) string {
	r := markdownRenderer(w)
	if r == nil {
		return text
	}
	out, err := r.Render(fixCJKEmphasis(text))
	if err != nil {
		return text // 渲染失败退回原文：总比整段不显示强
	}
	return cleanGlamour(strings.Trim(out, "\n"))
}

// coloredSpaceRe 匹配"被单独染色的一个空格"。
var coloredSpaceRe = regexp.MustCompile(`\x1b\[[0-9;]*m \x1b\[0m`)

// cleanGlamour 压掉 glamour 输出里的一处显著冗余。
//
// glamour 会把每行**补齐到词宽的空白逐字符染色**，于是行尾拖着一长串
// `\x1b[..m \x1b[0m`（实测一行能有几十个）。后果有两个，都不是小事：
//   - 转录体积翻十倍（转录是每帧整屏重绘的）；
//   - 文本被转义码切碎，`strings.Contains` 之类的内容判断会失效
//     （测试里"模型输出没进转录"就是这么炸出来的）。
//
// 我们自己会按正文宽重新折行（renderTranscript），本来就不需要它补，
// 所以直接把这类空格还原成普通空格、再把行尾空白去掉。
func cleanGlamour(s string) string {
	s = coloredSpaceRe.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n")
}

// glamourStyle 从当前主题生成 glamour 的样式表。
//
// 只映射影响观感的部分（颜色、粗细、项目符号）；间距类字段留给 glamour
// 默认值——那些属于"版式"而不是"配色"，跟着主题变只会破坏排版。
func glamourStyle() ansi.StyleConfig {
	var s ansi.StyleConfig
	t := currentTheme
	hex := func(c color.Color) *string {
		v := fmt.Sprintf("#%02x%02x%02x", rgba(c).R, rgba(c).G, rgba(c).B)
		return &v
	}
	yes := func() *bool { b := true; return &b }

	s.Document.Color = hex(t.Text)
	s.Document.Margin = nil

	// 标题统一用主色；层级差别交给 glamour 的默认字号与间距
	s.Heading.Color = hex(t.Accent)
	s.Heading.Bold = yes()
	s.H1.Color = hex(t.Accent)
	s.H1.Bold = yes()
	s.H2.Color = hex(t.Accent)
	s.H3.Color = hex(t.Accent)
	s.H4.Color = hex(t.Accent)
	s.H5.Color = hex(t.Accent)
	s.H6.Color = hex(t.TextDim)

	s.Emph.Italic = yes()
	s.Strong.Bold = yes()
	s.Strong.Color = hex(t.Warn) // 强调用暖色，与正文一眼分开
	s.Code.Color = hex(t.OK)
	s.Code.BackgroundColor = hex(t.Surface)
	s.CodeBlock.Color = hex(t.Text)
	s.CodeBlock.BackgroundColor = hex(t.Surface)
	s.Link.Color = hex(t.Accent)
	s.Link.Underline = yes()
	s.BlockQuote.Color = hex(t.TextDim)
	s.Table.Color = hex(t.Text)
	// 列表符号：glamour 的默认配置把 BlockPrefix 留空（列表只靠缩进区分），
	// 显式给上项目符号——终端里 • 才是"这是列表"最直接的线索。
	bullet := "• "
	s.List.BlockPrefix = bullet
	return s
}

// ---------------------------------------------------------------- CJK 补丁

// fixCJKEmphasis 绕过 goldmark（CommonMark）不把 CJK 标点当作 Unicode 标点的
// 问题：闭合 ** 只有在前一个字符是标点时才算右翼分界，于是「**X，**Y」里的
// **X，** 不会被加粗（，是 U+FF0C）。在这类闭合符后面插一个空格即可修复。
// 空格只能插在**闭合符**后——插在开启符后会反过来破坏左翼分界。
// 行内代码与围栏代码块原样跳过，代码里的字面 ** 不受影响。
//
// glamour 也走 goldmark，所以进它之前必须先过这一道。
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
