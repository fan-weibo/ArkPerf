package tui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// 三栏布局：左=当前工作区会话，中=对话，右=工作区文件树。
//
// 布局常量对齐 opencode（packages/tui/src/ui/layout.ts）。
//
// 关键差别在**用什么宽度决定要不要显示侧栏**：opencode 用的是
// 「侧栏 + 正文**偏好**宽」，而不是「侧栏 + 正文**最小**宽」。
// 后者会把侧栏挤到极限再显示，结果是栏和正文都难读；
// 前者在放不下时干脆把整块让给正文——空间不够时，正文比侧栏重要。
const (
	// paneSessionsW / paneFilesW 是展开态的默认栏宽。
	//
	// 比 opencode 的 42 窄得多，理由：42+42 在多数终端上会把正文挤到 70 列上下，
	// 而正文（转录 + 输入框）才是这块屏幕的主体，侧栏只是索引。
	// 取"够用就好"的最小值——28 放得下「❯ + 会话标题」，32 放得下文件名（含缩进），
	// 再多就是拿正文换侧栏的留白。嫌窄直接拖分隔线（见 dragTo），那会覆盖这两个默认值。
	paneSessionsW = 28
	paneFilesW    = 32
	// paneMinW / paneMaxW 是拖拽调整的上下限（opencode: 5 ~ 72）。
	//
	// 下限取 5 而不是 1：比这更窄就只能显示一个图标，而那种状态下
	// 用户真正想要的是「折叠」——两件事不该混在一起，否则会出现
	// "看起来折叠了但并没折叠"的中间态。
	paneMinW = 5
	paneMaxW = 72
	// paneRailW 是折叠态：只留一个可点击的窄条，随时能拉回来。
	// 不用"完全隐藏"是因为那就没有入口了。
	paneRailW = 3
	// contentMinW / contentPreferredW 是正文的最小可用宽与偏好宽
	// （opencode: 44 / 64）。显示侧栏与否看的是**偏好宽**。
	contentMinW       = 44
	contentPreferredW = 64
	// paneListTop 是**右栏**里列表的起始行（第 0 行是标题，第 1 行留空）。
	// 点击映射靠它把屏幕行换算成列表下标，必须与渲染保持一致。
	paneListTop = 2
	// sessionListTop 是**左栏**里列表的起始行：第 0 行标题，
	// 第 1-3 行是「+ New session」按钮（圆角边框三行），列表从第 4 行起。
	// 两栏的头部高度不同，所以拆成两个常量；都必须与渲染保持一致。
	sessionListTop = 4
	// paneItemRows 是每个会话项占的行数（标题一行 + 元信息一行）。
	paneItemRows = 2
)

// columns 返回三栏列宽。bodyW 是正文（转录 + 输入框）的宽度。
//
// 判据来自 opencode 的 sessionTabsFitVertically：
//
//	能放下 [左栏 + 正文偏好宽] → 显示左栏
//	再能放下 [右栏]            → 显示右栏
//
// 放不下的侧栏**整块不显示**（宽度返回 0），而不是压到很窄再挤出来：
// 一个 10 列的会话列表既显示不了名字、又占着正文的空间，两边都亏。
// 此时用户若点折叠/展开，会得到一句"终端太窄"的说明（见 clickSessions）。
func (m *Model) columns() (sessionsW, bodyW, filesW int) {
	w := m.width
	sessWant := clamp(m.sessionsWidth, paneMinW, paneMaxW)
	filesWant := clamp(m.filesWidth, paneMinW, paneMaxW)

	if m.showSessions && w >= sessWant+contentPreferredW {
		sessionsW = sessWant
	} else if !m.showSessions {
		// 折叠态：留窄条（它不占正文预算，因为很窄）
		sessionsW = paneRailW
	}

	if m.showFiles && sessionsW > 0 && w >= sessionsW+filesWant+contentPreferredW {
		filesW = filesWant
	} else if !m.showFiles && sessionsW > 0 {
		filesW = paneRailW
	}

	if w-sessionsW-filesW < contentMinW {
		// 放不下就不给侧栏，正文拿全部。
		// 窄终端下正文可能仍低于 contentMinW（比如 40 列），那也没办法——
		// 总不能为了凑够最小宽而溢出终端（opencode 在 width<44 时把左右
		// padding 从 2 降到 1，是同一个应对）。
		return 0, w, 0
	}
	return sessionsW, w - sessionsW - filesW, filesW
}

// bodyWidth 是正文可用宽度（单栏时就是整屏宽度）。
//
// contentWidth / boxContentWidth 都以它为基准——这是"三栏之后正文变窄"
// 这件事的唯一落点：改了这里，折行、截断、输入框宽度一起跟上。
func (m *Model) bodyWidth() int {
	_, bw, _ := m.columns()
	return max(bw, 20)
}

// ---------------------------------------------------------------- 命中测试

// paneHit 表示一个屏幕坐标落在哪一栏。
type paneHit int

const (
	hitNone     paneHit = iota
	hitSessions         // 左栏
	hitBody             // 中间正文
	hitFiles            // 右栏
)

// paneAt 判断列 x 落在哪一栏。单栏时恒为 hitBody。
func (m *Model) paneAt(x int) paneHit {
	sw, bw, fw := m.columns()
	if sw == 0 && fw == 0 {
		return hitBody
	}
	switch {
	case x < sw:
		return hitSessions
	case x < sw+bw:
		return hitBody
	default:
		return hitFiles
	}
}

// sessionIndexAt 把左栏内的屏幕行换算成会话下标；-1 表示不是会话行。
//
// 换算必须与 sessionsPaneLines 的排版一致：标题占第 0 行、
// 「+ New session」按钮占第 1-3 行（点击走 newSessionFromPane，不进这里），
// 之后每项占 paneItemRows 行。改排版就要同步改这里。
func (m *Model) sessionIndexAt(y int) int {
	if y < sessionListTop {
		return -1
	}
	// 加滚动偏移：标题区固定不动，只有列表跟着滚（见 paneViewport）
	idx := (y - sessionListTop + m.sessionsScroll) / paneItemRows
	if idx < 0 || idx >= len(m.sessionsCache) {
		return -1
	}
	return idx
}

// treeNodeAt 把右栏内的屏幕行换算成文件树节点；nil 表示该行不是节点。
//
// 必要时先按当前状态摊平一次：摊平结果本来是在渲染时产生的，
// 但"能不能点击"不该依赖"渲染是否已经发生过"——测试里直接喂点击事件、
// 或某些版本的消息顺序下，渲染可能还没跑过。
func (m *Model) treeNodeAt(y int) *FileNode {
	if m.filesRows == nil && m.showFiles && m.opts.ListFiles != nil {
		m.filesRows = m.treeRows()
	}
	// 加滚动偏移：标题区固定不动，只有列表跟着滚（见 paneViewport）
	idx := y - paneListTop + m.filesScroll
	if idx < 0 || idx >= len(m.filesRows) {
		return nil
	}
	return m.filesRows[idx].node
}

// ---------------------------------------------------------------- 栏渲染

// paneSide 决定分隔线画在栏的哪一侧。
type paneSide int

const (
	borderRight paneSide = iota // 左栏用：线在右缘
	borderLeft                  // 右栏用：线在左缘
)

// paneDrag 表示正在拖拽调整哪一栏的宽度。
type paneDrag int

const (
	paneDragNone     paneDrag = iota
	paneDragSessions          // 左栏的右边界
	paneDragFiles             // 右栏的左边界
)

// paneBorderCol 返回某一栏边界的屏幕列；-1 表示该栏当前没显示。
//
// 边界列同时服务三件事：判断"点在没点在边界上"、拖拽时按位移算新宽度、
// 以及渲染时把可拖拽的边界画得和普通分隔线不一样。
func (m *Model) paneBorderCol(which paneDrag) int {
	sw, bw, fw := m.columns()
	switch which {
	case paneDragSessions:
		if sw == 0 {
			return -1
		}
		return sw - 1 // 左栏的右缘（renderPane 把线画在栏内最后一列）
	case paneDragFiles:
		if fw == 0 {
			return -1
		}
		return sw + bw // 右栏的左缘
	}
	return -1
}

// bgSeq 取"铺栏背景"的转义前缀与重置序列。
//
// 用哨兵字符从 lipgloss 手里借序列，而不是手写 \x1b[48;5;Nm：
// 终端颜色能力不足时（TERM=dumb、16 色终端）lipgloss 会自己降级，
// 这里拿到的就是空串，色块自动消失、退回竖线分栏——手写序列没有这层保护，
// 在无色终端上会直接吐出乱码。
func bgSeq(c color.Color) (prefix, reset string) {
	probe := lipgloss.NewStyle().Background(c).Render(" ")
	i := strings.Index(probe, " ")
	if i <= 0 {
		return "", ""
	}
	return probe[:i], probe[i+1:]
}

// renderPane 渲染一栏：contentW 是内容宽度，返回的总宽 = contentW + 1。
//
// 分隔线算在栏宽**之内**（占 1 列），所以三栏拼起来的总宽恰好等于终端宽度。
// 若分隔线额外占列，整屏就会比终端宽一列，终端自动折行会让布局整体错位——
// 而且只在"刚好整除"的宽度下才看得出来。
//
// 正文栏不用这个函数（padBlock）：它的两侧分隔线分别由左栏和右栏提供，
// 自己再画一条就会把内容挤窄一列，折行宽度与 contentWidth() 就对不上了。
func (m *Model) renderPane(lines []string, contentW, h int, side paneSide) string {
	if contentW <= 0 || h <= 0 {
		return ""
	}
	lineColor := m.paneLineColor()
	// 正在拖拽的那条边界高亮。拖拽过程中没有别的反馈，
	// 不给点颜色用户会怀疑"到底拖动了没有"。
	if (side == borderRight && m.dragging == paneDragSessions) ||
		(side == borderLeft && m.dragging == paneDragFiles) {
		lineColor = currentTheme.Accent
	}
	lineStyle := lipgloss.NewStyle().Foreground(lineColor)
	rows := make([]string, 0, h)
	for i := range h {
		s := ""
		if i < len(lines) {
			s = lines[i]
		}
		// 先补齐宽度、再铺背景：补出来的空格也要落在色块里，
		// 否则栏的右侧会缺一角。顺序反了就只有文字那一小段有色。
		body := m.paintPane(padTo(s, contentW))
		if side == borderLeft {
			rows = append(rows, lineStyle.Render("│")+body)
		} else {
			rows = append(rows, body+lineStyle.Render("│"))
		}
	}
	return strings.Join(rows, "\n")
}

// paintPane 给整行铺上栏背景色。
//
// 不能简单地 Style.Render(line)：lipgloss 在行内样式结束时会输出 reset，
// 而 reset 会把**外层背景一并清掉**——于是"彩色文字"之后的部分没有背景，
// 色块断成一截一截（实测确认过）。做法是在每个 reset 之后重新注入背景序列。
//
// 未探测到终端背景色时 panePrefix 为空，本函数原样返回：此时不画色块，
// 分栏完全交给竖线。宁可少一层好看，也不猜一个可能把浅色终端涂黑的主题。
func (m *Model) paintPane(line string) string {
	if m.panePrefix == "" {
		return line
	}
	if m.paneReset != "" {
		line = strings.ReplaceAll(line, m.paneReset, m.paneReset+m.panePrefix)
	}
	return m.panePrefix + line + m.paneReset
}

// paneLineColor 是竖分隔线的颜色。
//
// 直接取主题的 border 派生色：它本来就只比背景亮一点，深浅主题下都合适，
// 不需要再按"探没探到背景"分情况——那是硬编码配色时代才要处理的事。
func (m *Model) paneLineColor() color.Color {
	return currentTheme.Border
}

// padBlock 把正文补到 w 宽、h 高（不加分隔线）。
func padBlock(lines []string, w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	rows := make([]string, 0, h)
	for i := range h {
		s := ""
		if i < len(lines) {
			s = lines[i]
		}
		rows = append(rows, padTo(s, w))
	}
	return strings.Join(rows, "\n")
}

// padTo 把一行视觉宽度补齐到 w（CJK 按 2 列算）；超宽则截断加省略号。
func padTo(s string, w int) string {
	if w <= 0 {
		return ""
	}
	switch d := w - ansi.StringWidth(s); {
	case d > 0:
		return s + strings.Repeat(" ", d)
	case d < 0:
		return ansi.Truncate(s, w, "…")
	default:
		return s
	}
}

// sessionsPaneLines 渲染左栏内容（不含补齐与分隔线）。
func (m *Model) sessionsPaneLines(w int) []string {
	if !m.showSessions {
		// 折叠态：一列图标，点它展开
		return []string{paneToggleStyle.Render("»")}
	}
	inner := w - 1
	head := styleAccent.Bold(true).Render("Sessions") +
		styleDim.Render(fmt.Sprintf(" %d", len(m.sessionsCache))) +
		styleDim.Render("  ⊟")
	// 「+ New session」按钮：圆角边框三行、撑满栏宽，做成一个**真的按钮**——
	// 第一版是单行 dim 文本，用户反馈"太小了，不显眼"。边框用 dim、文字用
	// 普通亮度：框提供结构，字保持可读。占第 1-3 行（sessionListTop 之前），
	// 不随列表滚动、恒在顶部，列表的 y→下标映射不受影响。
	label := "+ New session"
	btn := []string{
		styleDim.Render("╭" + strings.Repeat("─", inner-2) + "╮"),
		styleDim.Render("│") + " " + label +
			strings.Repeat(" ", max(inner-4-len(label), 0)) + " " +
			styleDim.Render("│"),
		styleDim.Render("╰" + strings.Repeat("─", inner-2) + "╯"),
	}
	if len(m.sessionsCache) == 0 {
		return append([]string{head}, append(btn, styleDim.Render("（暂无会话）"))...)
	}

	lines := append([]string{head}, btn...)
	for i, s := range m.sessionsCache {
		mark := "  "
		title := styleBold
		if i == m.sessionIdx {
			mark = styleAccent.Render("❯ ")
			title = styleAccent.Bold(true)
		} else if s.Current {
			mark = styleAccent.Render("· ")
		}
		name := s.Title
		if strings.TrimSpace(name) == "" {
			name = s.ID
		}
		lines = append(lines,
			mark+title.Render(ansi.Truncate(name, max(inner-4, 6), "…")),
			"  "+styleDim.Render(truncate(sessionMeta(s), max(inner-3, 6))),
		)
	}
	return lines
}

// sessionMeta 是会话项的第二行（轮数与更新时间）。
//
// 用 "·" 分隔，两者都空时返回空串——不留一个孤零零的分隔符。
func sessionMeta(s SessionBrief) string {
	parts := make([]string, 0, 2)
	if s.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d 轮", s.Turns))
	}
	if s.Updated != "" {
		parts = append(parts, s.Updated)
	}
	return strings.Join(parts, " · ")
}

// truncate 按视觉宽度截断（不加省略号，因为内容本身已被 padTo 处理）。
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

// filesPaneLines 渲染右栏内容，同时把树摊平成可点击的行。
//
// 摊平结果存进 m.filesRows：点击时要用同一份排版把屏幕行换算成节点，
// 两处各算一次必然会错行。
func (m *Model) filesPaneLines(w int) []string {
	if !m.showFiles {
		m.filesRows = nil
		return []string{paneToggleStyle.Render("«")}
	}
	inner := w - 1
	head := styleAccent.Bold(true).Render("Files") + styleDim.Render("  ⊟")
	lines := []string{head, ""}

	if m.opts.ListFiles == nil {
		m.filesRows = nil
		return append(lines, styleDim.Render("（未提供）"))
	}
	rows := m.treeRows()
	m.filesRows = rows
	if len(rows) == 0 {
		return append(lines, styleDim.Render("（空目录）"))
	}
	for _, r := range rows {
		indent := strings.Repeat(" ", r.depth*2)
		glyph, style := "·", styleDim
		if r.node.IsDir {
			if m.collapsedDirs[r.node.Path] {
				glyph = "▸"
			} else {
				glyph = "▾"
			}
			style = styleAccent
		}
		name := ansi.Truncate(r.node.Name, max(inner-4-r.depth*2, 4), "…")
		lines = append(lines, indent+style.Render(glyph)+" "+style.Render(name))
	}
	return lines
}

// treeLine 是文件树摊平后的一行。
type treeLine struct {
	node  *FileNode
	depth int
}

// treeRows 把文件树按当前折叠状态摊平成行序列。
func (m *Model) treeRows() []treeLine {
	var out []treeLine
	var walk func(nodes []FileNode, depth int)
	walk = func(nodes []FileNode, depth int) {
		for i := range nodes {
			n := nodes[i] // 复制一份：取址不能指向切片元素（下次刷新会变）
			out = append(out, treeLine{node: &n, depth: depth})
			if n.IsDir && !m.collapsedDirs[n.Path] && len(n.Children) > 0 {
				walk(n.Children, depth+1)
			}
		}
	}
	walk(m.filesCache, 0)
	return out
}

// ---------------------------------------------------------------- 拖拽调宽

// dblClickWindow 是两次点击边界算作"双击"的最大间隔。
//
// 250ms 是桌面环境的常见取值：够容纳手速稍慢的连击，
// 又不会把"点一下、过一会儿再点一下"误判成双击。
const dblClickWindow = 250 * time.Millisecond

// beginOrResetDrag 处理"在分隔线上按下"。
//
// 双击复位是必需的：栏拖宽之后想回到默认值，没有复位就只能来回试——
// 而"默认值到底是多少"用户本来就不知道（opencode 的 pane-resize 同理）。
func (m *Model) beginOrResetDrag(which paneDrag, x int) {
	now := time.Now()
	if !m.lastBorderClick.IsZero() && now.Sub(m.lastBorderClick) <= dblClickWindow {
		// 双击：复位该栏到默认宽
		m.lastBorderClick = time.Time{}
		m.dragging = paneDragNone
		switch which {
		case paneDragSessions:
			m.sessionsWidth = paneSessionsW
		case paneDragFiles:
			m.filesWidth = paneFilesW
		}
		m.resizeInput()
		return
	}
	m.lastBorderClick = now
	m.dragging = which
	m.dragOriginX = x
	switch which {
	case paneDragSessions:
		m.dragOriginW = m.sessionsWidth
	case paneDragFiles:
		m.dragOriginW = m.filesWidth
	}
}

// dragTo 按当前鼠标列更新被拖拽栏的宽度。
//
// 按**位移**而不是绝对列算：绝对列会让栏在按下的瞬间跳一下
// （鼠标落在边界附近几列内都算命中边界）。
func (m *Model) dragTo(x int) {
	if m.dragging == paneDragNone {
		return
	}
	delta := x - m.dragOriginX
	switch m.dragging {
	case paneDragSessions:
		// 左栏的边界往右拖 = 变宽
		m.sessionsWidth = clamp(m.dragOriginW+delta, paneMinW, paneMaxW)
	case paneDragFiles:
		// 右栏的边界往左拖 = 变宽，所以是减
		m.filesWidth = clamp(m.dragOriginW-delta, paneMinW, paneMaxW)
	}
	m.resizeInput()
}

// endDrag 结束拖拽。
func (m *Model) endDrag() { m.dragging = paneDragNone }

// ---------------------------------------------------------------- 侧栏滚动

// paneViewport 把侧栏内容裁进高度 h 的窗口，需要时在最后一列画滚动条。
//
// 前 listTop 行是标题区，**固定不动**；只有列表部分滚动。
// 标题跟着滚走的话，用户滚到一半就不知道这一栏是什么了
// （opencode 的 scrollbox 也是只滚内容，标题留在外面）。
// 左栏传 sessionListTop（它有按钮区），右栏传 paneListTop。
//
// contentW 是**含滚动条列**的宽度（即 renderPane 的内容宽）：
// 文本占 contentW-1 列，最后一列留给滚动条。滚动条位**永远保留**
// （不管这一帧需不需要）——宽度随"要不要滚动条"变化的话，
// 内容会在滚动到一半时整体重排，比白白浪费一列难看得多。
func paneViewport(lines []string, scroll, contentW, h, listTop int) []string {
	if contentW <= 0 || h <= 0 {
		return nil
	}
	textW := max(contentW-1, 1)
	head := lines[:min(listTop, len(lines))]
	body := lines[min(listTop, len(lines)):]

	maxScroll := max(len(body)-(h-len(head)), 0)
	sc := clamp(scroll, 0, maxScroll)

	out := make([]string, 0, h)
	for i := range h {
		if i < len(head) {
			out = append(out, padTo(head[i], contentW))
			continue
		}
		bi := sc + (i - len(head))
		s := ""
		if bi < len(body) {
			s = ansi.Truncate(body[bi], textW, "…")
		}
		out = append(out, padTo(s, textW)+scrollbarGlyph(i-len(head), h-len(head),
			len(body), sc, false))
	}
	return out
}

// scrollbarGlyph 是滚动条某一行的字形。
//
// 滑块长度按"可视行数 / 总行数"等比，**最少 1 行**——否则内容很长时
// 滑块会算成 0 行，滚动条上什么都不显示，看起来就是坏的。
//
// 用灰阶而不是主色：滚动条是"指示我在哪儿"的装置，不是要抢注意力的元素，
// 主色滑块会把视线一直拽向右边缘。只有正在拖动（active）时才用主色——
// 那时候它确实需要被看见。
func scrollbarGlyph(row, h, total, scroll int, active bool) string {
	if total <= h || h <= 0 {
		return " " // 不需要滚动：留空，不画一根满是刻度的轨道
	}
	thumb := max(1, h*h/total)
	pos := (h - thumb) * scroll / max(total-h, 1)
	if row >= pos && row < pos+thumb {
		if active {
			return styleAccent.Render("▐")
		}
		return styleScrollbar.Render("▐")
	}
	return styleBorder.Render("│")
}

// scrollPaneBy 滚动某一侧栏。delta 为正表示向下（往更后面看）。
func (m *Model) scrollPaneBy(p paneHit, delta int) {
	switch p {
	case hitSessions:
		m.sessionsScroll = clamp(m.sessionsScroll+delta, 0,
			max(m.sessionsTotal-m.height, 0))
	case hitFiles:
		m.filesScroll = clamp(m.filesScroll+delta, 0,
			max(m.filesTotal-m.height, 0))
	}
}

// ---------------------------------------------------------------- 滚动条交互

// scrollbarCol 返回转录区滚动条所在的屏幕列；-1 表示这一帧没有滚动条。
//
// 位置 = 左栏宽 + 正文宽。单栏时左栏为 0，滚动条就落在正文最右一列。
func (m *Model) scrollbarCol() int {
	if !m.barVisible {
		return -1
	}
	sw, _, _ := m.columns()
	return sw + m.contentW
}

// beginScrollbarDrag 在滚动条上按下：先跳到按下的位置，再进入拖动。
//
// 按下就跳而不是"按住等移动"：点哪儿去哪儿是滚动条的通用交互，
// 要求先按住再拖会让人以为没反应。
func (m *Model) beginScrollbarDrag(y int) {
	m.barDragging = true
	m.scrollBarToRow(y)
}

// dragScrollbarTo 拖动中：把滑块挪到鼠标所在行。
func (m *Model) dragScrollbarTo(y int) { m.scrollBarToRow(y) }

// endScrollbarDrag 结束拖动。
func (m *Model) endScrollbarDrag() { m.barDragging = false }

// scrollBarToRow 把鼠标行 y 换算成滚动位置。
//
// 用滑块**中心**对齐鼠标（所以减 thumb/2），而不是让滑块顶端贴着鼠标：
// 抓滑块中间往下拖是本能动作，按顶端算的话一按下滑块就会往下跳半截。
func (m *Model) scrollBarToRow(y int) {
	h := m.viewportRows
	total := m.lineCount
	if h <= 0 || total <= h {
		return
	}
	thumb := max(1, h*h/total)
	pos := clamp(y-thumb/2, 0, h-thumb)

	maxScroll := total - h
	span := h - thumb
	scroll := 0
	if span > 0 {
		scroll = pos * maxScroll / span
	}
	scroll = clamp(scroll, 0, maxScroll)

	m.viewTop = scroll
	// 拖到底部就恢复"跟随最新"：否则用户拖到底之后新内容来了画面却不动，
	// 还得再滚一次——而"已经在底部"本来就是跟随的语义。
	m.follow = scroll >= maxScroll
}

// ---------------------------------------------------------------- 状态

// toggleSessions 折叠/展开左栏。
func (m *Model) toggleSessions() { m.showSessions = !m.showSessions }

// toggleFiles 折叠/展开右栏。
func (m *Model) toggleFiles() { m.showFiles = !m.showFiles }

// paneCanExpand 判断当前终端宽度放不放得下某一栏的展开态。
//
// 判据与 columns() 一致（栏宽 + 正文偏好宽），否则会出现
// "提示说能显示、实际没显示"这种自相矛盾。
func (m *Model) paneCanExpand(which paneDrag) bool {
	want := clamp(m.sessionsWidth, paneMinW, paneMaxW)
	if which == paneDragFiles {
		sw, _, _ := m.columns()
		want = clamp(m.filesWidth, paneMinW, paneMaxW)
		return sw > 0 && m.width >= sw+want+contentPreferredW
	}
	return m.width >= want+contentPreferredW
}

// paneTooNarrowHint 说明"为什么展开没反应"，并给出需要的列数。
//
// 必须给说明：不然用户点了展开、栏没出现，只会认为功能坏了。
// 带上需要的列数，他就知道该把窗口拉多宽。
func (m *Model) paneTooNarrowHint(which paneDrag) string {
	if which == paneDragFiles {
		sw, _, _ := m.columns()
		want := clamp(m.filesWidth, paneMinW, paneMaxW)
		return fmt.Sprintf(
			"终端只有 %d 列，放不下文件栏（需 %d 列 = 会话栏 %d + 文件栏 %d + 正文 %d）；拉宽窗口会自动显示",
			m.width, sw+want+contentPreferredW, sw, want, contentPreferredW)
	}
	want := clamp(m.sessionsWidth, paneMinW, paneMaxW)
	return fmt.Sprintf(
		"终端只有 %d 列，放不下会话栏（需 %d 列 = 栏 %d + 正文 %d）；拉宽窗口会自动显示",
		m.width, want+contentPreferredW, want, contentPreferredW)
}

// toggleSessionsWithNotice 折叠/展开左栏；展开但放不下时给一句说明。
func (m *Model) toggleSessionsWithNotice() (tea.Model, tea.Cmd) {
	expanding := !m.showSessions
	m.toggleSessions()
	m.resizeInput() // 栏宽变了，输入框宽度必须跟着变
	if expanding && !m.paneCanExpand(paneDragSessions) {
		return m, m.print(styleWarn.Render(m.paneTooNarrowHint(paneDragSessions)))
	}
	return m, nil
}

// toggleFilesWithNotice 折叠/展开右栏；展开但放不下时给一句说明。
func (m *Model) toggleFilesWithNotice() (tea.Model, tea.Cmd) {
	expanding := !m.showFiles
	m.toggleFiles()
	m.resizeInput()
	if expanding && !m.paneCanExpand(paneDragFiles) {
		return m, m.print(styleWarn.Render(m.paneTooNarrowHint(paneDragFiles)))
	}
	return m, nil
}

// refreshSessions 重新拉取会话列表。
//
// 列表要缓存：View 每帧都渲染，而 Sessions() 会去读会话目录——
// 每帧读一次磁盘，滚动时掉帧就是它。
func (m *Model) refreshSessions() {
	if m.opts.Sessions == nil {
		return
	}
	m.sessionsCache = m.opts.Sessions()
	// 选中项跟着 Current 走；没有 Current 就保持原下标（不越界）
	for i, s := range m.sessionsCache {
		if s.Current {
			m.sessionIdx = i
			return
		}
	}
	if m.sessionIdx >= len(m.sessionsCache) {
		m.sessionIdx = max(len(m.sessionsCache)-1, 0)
	}
}

// refreshFiles 重新拉取文件树。
func (m *Model) refreshFiles() {
	if m.opts.ListFiles == nil {
		m.filesCache = nil
		return
	}
	m.filesCache = m.opts.ListFiles()
	// 首次拿到树时按默认策略折叠一次（只展开第一层）。
	// 之后刷新不动它：用户手动展开过的目录不该被收回去。
	if !m.filesDefaulted {
		m.filesDefaulted = true
		m.collapseDeepDirs()
	}
}

// collapseDeepDirs 把**深层目录**标记为折叠，只留第一层展开。
//
// 为什么不全展开：侧栏只有几十行，而树最多能到 400 个节点——全展开的结果是
// 一屏枝叶、要滚动才能看全，顶层结构反而被淹没。只展开第一层时首屏恰好是
// "顶层有哪些目录 + 每个目录里有什么"，一眼看完。
//
// 用户手动点开的目录记录在同一个 collapsedDirs 里，与这里的默认值天然共存：
// 默认只负责"没被用户碰过的那些"。
func (m *Model) collapseDeepDirs() {
	var walk func(nodes []FileNode, depth int)
	walk = func(nodes []FileNode, depth int) {
		for i := range nodes {
			n := nodes[i]
			if !n.IsDir {
				continue
			}
			if depth > 0 {
				m.collapsedDirs[n.Path] = true
			}
			walk(n.Children, depth+1)
		}
	}
	walk(m.filesCache, 0)
}

// resetFilesView 换工作区时重置文件树的浏览状态。
//
// 换了目录就是换了一棵树：旧的折叠记录按路径对不上（还容易撞上同名目录，
// 表现为"新目录里某个文件夹莫名其妙是收起的"），滚动位置也没有意义。
func (m *Model) resetFilesView() {
	m.collapsedDirs = map[string]bool{}
	m.filesDefaulted = false
	m.filesScroll = 0
	m.filesCache = nil
	m.filesRows = nil
	m.filesTotal = 0
}

// openSessionIdx 切到左栏第 idx 个会话并回放它的历史。
//
// 刻意只由**显式点击/按键**触发，不做"光标停在哪就切哪"：
// 切换会话会清屏重放，误触一次就把当前对话顶掉了。
func (m *Model) openSessionIdx(idx int) (tea.Model, tea.Cmd) {
	if idx < 0 || idx >= len(m.sessionsCache) {
		return m, nil
	}
	b := m.sessionsCache[idx]
	if b.Current {
		return m, nil // 已经是当前会话，不重复切换
	}
	if m.opts.OpenSession == nil {
		return m, m.print(styleErr.Render("✗ 未注入会话切换（OpenSession）"))
	}
	lines, dir, err := m.opts.OpenSession(b.ID)
	if err != nil {
		return m, m.print(styleErr.Render("✗ 切换会话失败：" + err.Error()))
	}
	// 跨工作区的会话要连工作区一起切：否则状态行写着旧目录、
	// 模型却在另一个目录里干活（与 /sessions <序号> 同一条约定）。
	if dir != "" {
		m.opts.CWD = dir
		// 换成另一棵文件树：先清浏览状态再重拉，否则右栏还停在旧目录的内容上
		m.resetFilesView()
		m.refreshFiles()
	}
	m.replay(lines)
	m.refreshSessions()
	// 刷新之后再钉一次高亮：数据源若还没把新会话标成 Current
	// （有些实现要等下一次落盘才更新），refreshSessions 会把高亮退回旧项，
	// 屏幕上就是"刚点的那一项自己跳回去了"。以用户这次点击为准。
	m.sessionIdx = idx
	return m, m.print(styleDim.Render("已切换到会话：" + sessionTitle(b)))
}

// sessionTitle 取会话的展示名（标题为空时退回 ID）。
func sessionTitle(b SessionBrief) string {
	if strings.TrimSpace(b.Title) != "" {
		return b.Title
	}
	return b.ID
}

// ---------------------------------------------------------------- 点击

// clickSessions 处理左栏的点击：标题行折叠/展开，其余行切换会话。
// newSessionFromPane 是左栏「+ New session」动作行（y==1）的处理。
//
// 与 /new 的差别只在收尾：这里把**启动时那块横幅**（装配摘要 + 新会话提示）
// 重新打出来——用户要的是"点了之后跟刚启动的界面一样，最上面是有东西的"，
// 而不是 /new 那种一屏空白。横幅渲染复用 printBanner，两处必然一致。
func (m *Model) newSessionFromPane() tea.Cmd {
	if m.opts.ResetSession == nil {
		return m.print(styleDim.Render("（当前不支持新建会话）"))
	}
	if err := m.opts.ResetSession(); err != nil {
		return m.print(styleErr.Render("✗ 新建会话失败：" + err.Error()))
	}
	m.resetViewState()
	m.refreshSessions() // 新会话的 Current 标记要在列表里同步，旧会话的标记要摘掉
	m.printBanner()
	return nil
}

func (m *Model) clickSessions(y int) (tea.Model, tea.Cmd) {
	if y == 0 {
		// 标题行（折叠态下就是那个图标）：折叠 / 展开。
		// 标题行不承担"选会话"——否则用户想收起侧栏却误切了会话。
		return m.toggleSessionsWithNotice()
	}
	if y >= 1 && y <= 3 {
		// 「+ New session」按钮的三行（边框 + 文字）都算按钮，点哪都触发。
		return m, m.newSessionFromPane()
	}
	if !m.showSessions {
		return m, nil
	}
	idx := m.sessionIndexAt(y)
	if idx < 0 {
		return m, nil
	}
	m.sessionIdx = idx
	return m.openSessionIdx(idx)
}

// clickFiles 处理右栏的点击：
//   - 标题行折叠 / 展开整栏；
//   - 目录行折叠 / 展开该目录；
//   - 文件行把相对路径插进输入框（比手敲快得多，也少打错）。
func (m *Model) clickFiles(y int) (tea.Model, tea.Cmd) {
	if y == 0 {
		return m.toggleFilesWithNotice()
	}
	if !m.showFiles {
		return m, nil
	}
	node := m.treeNodeAt(y)
	if node == nil {
		return m, nil
	}
	if node.IsDir {
		// 只翻这一棵：折叠状态是每个目录各自独立的
		m.collapsedDirs[node.Path] = !m.collapsedDirs[node.Path]
		return m, nil
	}
	m.insertPath(node.Path)
	return m, nil
}

// insertPath 把路径插到输入框末尾（已以空格结尾时不重复补分隔）。
func (m *Model) insertPath(p string) {
	v := m.input.Value()
	if v != "" && !strings.HasSuffix(v, " ") {
		v += " "
	}
	m.input.SetValue(v + p)
	m.input.CursorEnd()
	m.refreshSlash()
	m.resizeInput()
}
