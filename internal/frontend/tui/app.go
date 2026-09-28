package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

type state int

const (
	stateIdle state = iota
	stateRunning
	stateApproving
)

// transcriptCap 限制内存里保留的转录行数。
//
// 终端自己的滚动区不受此限制；这份内存副本是给 /copy、未来的会话导出用的，
// 没必要无限增长。
const transcriptCap = 4000

// wheelLinesPerNotch 是滚轮滚一格移动几行。
//
// 取 3 是终端滚轮的通用步长；太大会一晃就翻过整屏，太小则长对话根本翻不动。
const wheelLinesPerNotch = 3

// 转录里的行标记，与输入框提示符保持同一套符号：
//   - userMarker  ── 用户说的话
//   - agentMarker ── agent 说的话（横向扫一眼就能分清谁说的）
//
// 这是从 Reasonix 学来的：只用颜色区分谁说的，在长对话里不够。
const (
	userMarker  = "›"
	agentMarker = "◆"
)

// maxInputRows 是输入框最多占几行（超出后内部滚动）。
const maxInputRows = 8

// Model 是 TUI 的状态。
//
// 方法一律用指针接收者：这个模型到处都在就地改字段，值接收者会让
// "改了但没生效"变成一类静默 bug（Bubble Tea 允许返回同一个指针）。
type Model struct {
	ctx    context.Context
	opts   Options
	bridge *Bridge
	run    RunFunc

	input   textarea.Model
	spinner spinner.Model

	width  int
	height int
	st     state

	turns    int
	toolUses int
	lastStop kernel.StopReason

	pending *event

	// pendingCall 暂存尚未定稿的工具调用行：等结果到达后与结果一起
	// 打进滚动区，保证一次输出就是一个有序单元（不会出现"调用有了、结果还没来"
	// 的中间态被永久留在转录里）。
	pendingCall string

	// transcript 是已定稿行的内存副本。
	transcript []string
	// follow 为 true 时视口钉在最新；false 表示用户正在回看历史。
	follow bool
	// viewTop 是不跟随时的视口首行（绝对行号）。
	viewTop int
	// lineCount / viewportRows 是上次渲染记录的行数，用于计算可滚动范围。
	lineCount    int
	viewportRows int

	history []string
	histIdx int
	draft   string

	slashMatches []SlashCommand
	slashIdx     int
	slashActive  bool

	// queued 是运行中提交的下一轮任务。
	queued string

	// mouseCaptureOff 把鼠标交还给终端（View 设 MouseModeNone 而非 CellMotion）。
	// 默认 false（开捕获）：滚轮/滚动条和**app 内选中**同时可用——选中不是
	// 靠终端原生划选，而是自己实现（左键拖拽→反色高亮→松手自动复制，见 selection.go）。
	// 只有 SSH/Termux 这类"终端本地复制更靠谱"的环境才需要 /mouse 关掉捕获。
	mouseCaptureOff bool

	// sel 是正在进行的 app 内左键拖选（见 selection.go）。
	sel selection
	// contentLines 是上次渲染折好的内容行（绝对行号），供选中取文本与坐标映射。
	contentLines []string
	// contentW 是上次渲染的内容列宽（不含右侧滚动条列），用于判定点击落在哪。
	contentW int
	// lastTop 是上次渲染的视口首行绝对行号：屏幕行 y → 内容行 lastTop+y。
	lastTop int

	// assistantOpen 标记本轮是否已经打过 "◆ arkperf" 头部。
	// 一轮里模型常常先答一段、调工具、再答一段——每段都打头部的话，
	// 用户会在同一轮里看到两三个 "◆ arkperf"（实测撞过）。
	// 头部只在轮首打一次，后续段落是同一轮的延续。
	// startTask（含排队任务启动）与 /clear 时复位。
	assistantOpen bool
}

// NewModel 构造 TUI 模型。
func NewModel(ctx context.Context, o Options, b *Bridge) *Model {
	ti := textarea.New()
	ti.SetPromptFunc(4, func(info textarea.PromptInfo) string {
		if info.LineNumber != 0 {
			return "" // 续行不重复提示符，保持与真实光标对齐
		}
		// 与转录里的用户标记同一套符号：翻历史时视觉上连得起来。
		// 前面留两格空格——学自 Reasonix，让用户输入区缩进、更易识别。
		if info.Focused {
			return styleAccent.Render("  " + userMarker + " ")
		}
		return styleDim.Render("  " + userMarker + " ")
	})
	ti.CharLimit = 16384
	// 提示符已经占据了视觉位置，不再叠一层占位文字
	ti.Placeholder = ""
	ti.DynamicHeight = true
	ti.MinHeight = 1
	ti.MaxHeight = maxInputRows
	ti.ShowLineNumbers = false
	// 用真实终端光标而不是虚拟方块：输入法的候选窗会跟着插入点走，
	// 中文输入体验完全不同。
	ti.SetVirtualCursor(false)
	// 回车交给本模型处理（提交），换行改到 Ctrl+J / Alt+Enter
	ti.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "alt+enter"))
	ti.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = styleAccent

	m := &Model{
		ctx:     ctx,
		opts:    o,
		bridge:  b,
		input:   ti,
		spinner: sp,
		follow:  true, // 默认跟随最新
		// 默认开着鼠标捕获：滚轮/滚动条可用，选中由 app 自己实现
		// （左键拖拽→反色高亮→松手自动复制）。学自 Reasonix。
		mouseCaptureOff: false,
	}
	if o.NewAgent != nil {
		m.run = o.NewAgent(b, b.Events())
	}
	return m
}

func (m *Model) Init() tea.Cmd {
	// 启动横幅：第一行是身份（◆ 名字 · 模型 · 工具），常驻在转录的最顶上；
	// 之后的行是提示。内容由 main.go 组装，这里只负责配色。
	if m.opts.StartupNotice != "" {
		parts := strings.SplitN(m.opts.StartupNotice, "\n", 2)
		out := styleAccent.Bold(true).Render("◆ " + parts[0])
		if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
			out += "\n" + styleDim.Render(parts[1])
		}
		return tea.Batch(m.print(out), m.spinner.Tick)
	}
	return m.spinner.Tick
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeInput()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tea.MouseWheelMsg:
		// alt-screen 里没有原生滚动区，滚轮只能自己处理。
		// 普通一格滚 3 行；按住 Shift 翻整页（与终端的习惯一致）。
		step := wheelLinesPerNotch
		if msg.Mod&tea.ModShift != 0 {
			step = max(m.viewportRows-1, 1)
		}
		switch msg.Button {
		case tea.MouseWheelUp:
			m.scrollBy(-step)
		case tea.MouseWheelDown:
			m.scrollBy(step)
		default:
			return m, nil
		}
		return m, nil

	case tea.MouseClickMsg:
		// 左键按下：在转录区里开始一次拖选。
		// （右/中键留给终端或后续功能，这里不动。）
		if msg.Button == tea.MouseLeft && m.inTranscript(msg.X, msg.Y) {
			at := m.transcriptCaret(msg.X, msg.Y)
			m.sel = selection{active: true, anchor: at, head: at}
		}
		return m, nil

	case tea.MouseMotionMsg:
		// CellMotion 只在按住键时上报移动，所以这就是拖拽：延伸选区。
		if m.sel.active {
			m.sel.head = m.transcriptCaret(msg.X, msg.Y)
		}
		return m, nil

	case tea.MouseReleaseMsg:
		// 松手**不复制**：选中只是把内容圈出来，复制留给 Ctrl+C / Ctrl+Insert。
		// （Reasonix 是松手即复制，但这里按用户要求改成显式复制——
		// 选完还能反悔、还能接着改选区，不会手一抖就把剪贴板覆盖了。）
		// 只点一下没拖动，则清掉旧选区。
		if msg.Button == tea.MouseLeft && m.sel.active && m.sel.isEmpty() {
			m.sel = selection{}
		}
		return m, nil

	case copiedMsg:
		return m, m.copyResultCmd(msg)

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case event:
		return m.handleEvent(msg)

	case notice:
		return m, m.print(styleDim.Render(msg.text))

	case reportMsg:
		if msg.err != nil {
			return m, m.print(styleErr.Render("✗ " + msg.title + "：" + msg.err.Error()))
		}
		return m, m.print(styleBold.Render(msg.title) + "\n" + wrapText(msg.body, m.contentWidth()))

	case resetView:
		m.transcript = nil
		m.assistantOpen = false
		m.sel = selection{}
		return m, tea.ClearScreen
	}
	return m, nil
}

// View 渲染整屏：上面是转录视口，下面是钉在底边的活区。
//
// 采用 alt-screen——这是刻意的一次翻转。早期版本把定稿行用 tea.Println
// 打进终端原生滚动区，滚轮/划选/搜索免费，但**输入框没法钉在屏幕底边、
// agent 名字也没法常驻左上角**，而这两点是交互界面的基本样子。
// 翻转的代价是回看历史要自己做（PageUp/PageDown），好在转录本来就在内存里。
func (m *Model) View() tea.View {
	if m.width <= 0 || m.height <= 0 {
		// 还没收到窗口尺寸：此时算高度会得到负数
		return tea.NewView(styleDim.Render("ArkPerf 启动中…"))
	}
	w := max(m.width, 24)

	var parts []string
	rowsAbove := 0
	appendBlock := func(s string) {
		if s == "" {
			return
		}
		parts = append(parts, s)
		rowsAbove += strings.Count(s, "\n") + 1
	}

	// 回看历史时必须明示：否则用户会以为前面的内容没了
	if !m.follow {
		appendBlock(m.scrollIndicator(w))
	}
	if m.st == stateRunning {
		appendBlock(m.workingLine(w))
	}
	if m.st == stateApproving {
		appendBlock(m.approvalCard(w))
	}
	if m.slashActive {
		appendBlock(m.slashMenu(w))
	}

	box := inputBoxStyle.Width(m.boxContentWidth()).Render(m.input.View())
	parts = append(parts, box)
	parts = append(parts, m.statusBlock(w))

	bottom := strings.Join(parts, "\n")
	bottomRows := strings.Count(bottom, "\n") + 1

	// 转录视口填满剩余高度：底部区域才能钉在屏幕底边
	avail := max(m.height-bottomRows, 1)
	transcript := m.renderTranscript(w, avail)

	v := tea.NewView(transcript + "\n" + bottom)
	v.AltScreen = true
	// 鼠标模式随 /mouse 切换（学自 Reasonix）：
	//   · 开捕获（默认）：滚轮/滚动条可用；选中由 app 自己实现——
	//     左键拖拽→反色高亮→松手自动复制到剪贴板（见 selection.go）。
	//   · 关捕获：把鼠标交还终端，用它的原生划选/右键菜单；此时滚轮失效，
	//     回看改用 PageUp / PageDown。给 SSH/Termux 这类环境留的退路。
	if m.mouseCaptureOff {
		v.MouseMode = tea.MouseModeNone
	} else {
		v.MouseMode = tea.MouseModeCellMotion
	}

	// 把真实光标钉到插入点：textarea 的坐标是相对自身的，
	// 需要加上转录区高度、上方占用的行数与边框/内边距。
	if cur := m.input.Cursor(); cur != nil {
		cur.X += 2                     // 左边框 1 + 左内边距 1
		cur.Y += avail + rowsAbove + 1 // 转录区 + 上方行数 + 上边框 1
		v.Cursor = cur
	}
	return v
}

// renderTranscript 渲染转录视口：恰好填满 avail 行。
//
// 内容**从顶部开始画**，不足 avail 行时在**底部**留空——和真实终端一致：
// 启动横幅在最顶上，输出往下长，长满后才滚动。
// （这里曾补在顶部，结果启动横幅沉到了输入框上面——顶部对齐才对。）
func (m *Model) renderTranscript(w, avail int) string {
	// 内容按整屏宽度折行。
	//
	// 这里曾留一列画竖向滚动指示条（学 Reasonix），但字形是直接拼在行尾的、
	// 内容行没补齐到整宽，结果每行文字后面都跟着一个灰方块（用户截图吐槽）。
	// 滚动位置本来就有"回看历史中，下方还有 N 行"那行明示，够用了，直接去掉。
	bodyW := max(w, 8)

	var lines []string
	for _, entry := range m.transcript {
		// 渲染时再折行：终端宽度变了，旧行也能适配新宽度
		lines = append(lines, strings.Split(ansi.Wrap(entry, bodyW, ""), "\n")...)
	}
	m.lineCount = len(lines)
	m.viewportRows = avail
	// 记下来供选中用：屏幕行 y → 内容行 lastTop+y，列宽 contentW 决定点在哪算"在转录区里"
	m.contentLines = lines
	m.contentW = bodyW

	// 跟随最新时永远显示最后 avail 行；回看时按绝对行号定位，
	// 这样新输出到达也不会把用户的视线拽回底部。
	start := m.currentTop()
	m.lastTop = start
	end := min(start+avail, len(lines))
	window := lines[start:end]

	// 给落在选区里的可视行打反色高亮（学自 Reasonix）。
	// 选区用绝对行号，所以滚动时高亮跟着内容走，不会漂。
	if m.sel.active && !m.sel.isEmpty() {
		s0, s1 := m.sel.ordered()
		for i := range window {
			if lo, hi, ok := selSpan(start+i, s0, s1, bodyW); ok {
				window[i] = highlightRange(window[i], lo, hi, bodyW)
			}
		}
	}

	// 底部留空（不是顶部）：内容贴着屏幕顶端，输入框钉在底边
	rows := make([]string, 0, avail)
	rows = append(rows, window...)
	for range avail - len(window) {
		rows = append(rows, "")
	}
	return strings.Join(rows, "\n")
}

// currentTop 返回视口首行的绝对行号。
func (m *Model) currentTop() int {
	if m.follow {
		return max(m.lineCount-m.viewportRows, 0)
	}
	return clamp(m.viewTop, 0, m.scrollable())
}

// scrollable 返回还能往上滚多少行。
func (m *Model) scrollable() int { return max(m.lineCount-m.viewportRows, 0) }

// scrollBy 回看历史：正数向下（朝最新），负数向上（朝更早）。
//
// 滚到底部就自动恢复"跟随最新"——这样用户不需要再按一次什么键，
// 新输出自然接着出现。
// scrollBy 回看历史：正数向下（朝最新），负数向上（朝更早）。
//
// 滚到底部就自动恢复"跟随最新"——这样用户不需要再按一次什么键，
// 新输出自然接着出现。
func (m *Model) scrollBy(n int) {
	// 可滚动范围来自上一次渲染记录的行数。
	// 真实循环里渲染永远发生在输入之前，所以这两个值一定是最新的。
	top := clamp(m.currentTop()+n, 0, m.scrollable())
	m.viewTop = top
	m.follow = top >= m.scrollable()
}

// ScrollToLatest 立即回到最新。
func (m *Model) ScrollToLatest() {
	m.follow = true
	m.viewTop = m.scrollable()
}

// clamp 把 v 限制在 [lo, hi]。hi < lo 时返回 lo。
func clamp(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	default:
		return v
	}
}

// ---------------------------------------------------------------- 事件

func (m *Model) handleEvent(e event) (tea.Model, tea.Cmd) {
	switch e.kind {
	case evAssistant:
		text := strings.TrimSpace(e.text)
		if text == "" {
			return m, nil
		}
		if m.assistantOpen {
			// 同一轮的后续段落（工具调用之后模型接着说）：
			// 不再重复 "◆ arkperf" 头部，直接续着打
			return m, m.print(renderAssistant(text, m.contentWidth()))
		}
		m.assistantOpen = true
		m.turns++
		// 带上 agent 标记：长对话里只靠颜色分不清谁在说话
		return m, m.print(styleAccent.Render(agentMarker+" arkperf") + "\n" + renderAssistant(text, m.contentWidth()))

	case evToolCall:
		m.toolUses++
		if m.pendingCall != "" {
			// 上一条调用没等到结果就被顶掉了：先把它定稿，别丢
			pending := m.pendingCall
			m.pendingCall = m.toolCallLine(e.name, e.args)
			return m, m.print(pending)
		}
		m.pendingCall = m.toolCallLine(e.name, e.args)
		return m, nil

	case evToolResult:
		head := styleOK.Render("✓ ")
		if e.isErr {
			head = styleErr.Render("✗ ")
		}
		prefix := head + e.name + " "
		line := prefix + styleDim.Render(firstLine(e.text, m.lineBudget(prefix)))
		if m.pendingCall != "" {
			both := m.pendingCall + "\n" + line
			m.pendingCall = ""
			return m, m.print(both)
		}
		return m, m.print(line)

	case evApproval:
		m.pending = &e
		m.st = stateApproving
		m.input.Blur()
		return m, nil

	case evDone:
		return m.finish(e)
	}
	return m, nil
}

func (m *Model) finish(e event) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	if m.pendingCall != "" {
		pending := m.pendingCall
		m.pendingCall = ""
		cmds = append(cmds, m.print(pending))
	}

	m.st = stateIdle
	m.pending = nil
	m.input.Focus()

	switch {
	case e.err != nil:
		cmds = append(cmds, m.print(styleErr.Render("✗ 任务失败："+e.err.Error())))
	case e.result.Reason == kernel.StopFinal:
		m.lastStop = e.result.Reason
		cmds = append(cmds, m.print(styleDim.Render(fmt.Sprintf(
			"──── 完成 · %d 轮 · %d 次工具调用", e.result.Turns, e.result.ToolUses))))
	default:
		m.lastStop = e.result.Reason
		cmds = append(cmds, m.print(styleWarn.Render(fmt.Sprintf(
			"──── 停止：%s · %d 轮 · %d 次工具调用",
			reasonText(e.result.Reason), e.result.Turns, e.result.ToolUses))))
	}

	if m.queued != "" {
		task := m.queued
		m.queued = ""
		_, cmd := m.startTask(task)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// toolCallLine 渲染一行工具调用摘要。
//
// 参数必须按**当前终端宽度**截断，不能固定长度：长路径（Windows 上的
// hap 路径动辄八十多列）会把这一行顶出终端宽度，终端一折行，底部活区
// 就整体错位——而且只在特定宽度下复现。
func (m *Model) toolCallLine(name string, args map[string]any) string {
	prefix := styleAccent.Render("→ ") + name + " "
	return prefix + styleDim.Render(ansi.Truncate(compact(args), m.lineBudget(prefix), "…"))
}

// lineBudget 算出一行在给定前缀之后还剩多少显示宽度可用。
func (m *Model) lineBudget(prefix string) int {
	return max(m.contentWidth()-ansi.StringWidth(prefix), 8)
}

// ---------------------------------------------------------------- 按键

func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()

	// 审批卡是模态的：此时输入框失焦，按键只属于它
	if m.st == stateApproving {
		return m.handleApprovalKey(k)
	}

	// 有选区时 Ctrl+C / Ctrl+Insert 是"复制"，不是"中断/退出"。
	//
	// 必须排在下面的 ctrl+c 分支之前——否则用户拖选完按 Ctrl+C，
	// 走的是"空闲=退出"，程序直接关掉，选中的东西还没到手（实测撞到）。
	// 终端里"Ctrl+C 复制选区"是通用习惯（学自 Reasonix）。
	if k == "ctrl+c" || k == "ctrl+insert" {
		if m.sel.active && !m.sel.isEmpty() {
			text := m.selectedText()
			m.sel = selection{}
			return m, copyText(text)
		}
		if k == "ctrl+insert" {
			// 没选区时 Ctrl+Insert 什么都不做：它是"纯复制"键，
			// 不该有 Ctrl+C 那种中断/清空/退出的副作用。
			return m, nil
		}
	}

	// 其他任何按键都取消"已完成"的选区（Reasonix 同款）：
	// 高亮只是"刚选了什么"的临时提示，用户继续操作后不该还留在屏幕上。
	m.sel = selection{}

	switch k {
	case "ctrl+c":
		if m.st == stateRunning {
			m.bridge.Interrupt()
			return m, m.print(styleWarn.Render("已请求中断…"))
		}
		return m, tea.Quit

	case "ctrl+d":
		if strings.TrimSpace(m.input.Value()) == "" {
			return m, tea.Quit
		}

	case "esc":
		if m.st == stateRunning {
			m.bridge.Interrupt()
			return m, m.print(styleWarn.Render("已请求中断…"))
		}
		if m.slashActive {
			m.slashActive = false
			return m, nil
		}
		m.input.SetValue("")
		return m, nil

	case "enter":
		return m.handleEnter()

	case "ctrl+p":
		m.recallHistory(-1)
		return m, nil

	case "pgup":
		// alt-screen 里没有原生滚动区，回看历史自己做
		m.scrollBy(-max(m.height/2, 1))
		return m, nil

	case "pgdown":
		m.scrollBy(max(m.height/2, 1))
		return m, nil

	case "ctrl+n":
		m.recallHistory(1)
		return m, nil

	case "tab":
		if m.slashActive && len(m.slashMatches) > 0 {
			m.input.SetValue("/" + m.slashMatches[m.slashIdx].Name)
			m.input.CursorEnd()
			m.refreshSlash()
			return m, nil
		}

	case "up":
		if m.slashActive && len(m.slashMatches) > 0 {
			m.slashIdx = (m.slashIdx - 1 + len(m.slashMatches)) % len(m.slashMatches)
			return m, nil
		}

	case "down":
		if m.slashActive && len(m.slashMatches) > 0 {
			m.slashIdx = (m.slashIdx + 1) % len(m.slashMatches)
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.refreshSlash()
	return m, cmd
}

func (m *Model) handleEnter() (tea.Model, tea.Cmd) {
	if m.slashActive && len(m.slashMatches) > 0 {
		return m.runSlash(m.slashMatches[m.slashIdx].Name, nil)
	}

	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return m, nil
	}

	if strings.HasPrefix(text, "/") {
		m.input.SetValue("")
		m.slashActive = false
		name, arg, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
		return m.runSlash(strings.TrimSpace(name), &arg)
	}

	m.input.SetValue("")
	m.remember(text)

	if m.st == stateRunning {
		// 运行中提交：排队，等当前任务结束自动接上。
		// 明确告诉用户"排上了"，而不是让它看起来像没反应。
		m.queued = text
		return m, m.print(styleDim.Render("已排队（当前任务结束后执行）"))
	}
	_, cmd := m.startTask(text)
	return m, cmd
}

func (m *Model) startTask(text string) (tea.Model, tea.Cmd) {
	if m.run == nil {
		return m, m.print(styleErr.Render("✗ 未注入任务执行器"))
	}
	m.st = stateRunning
	m.turns, m.toolUses = 0, 0
	m.pendingCall = ""
	m.queued = ""
	m.slashActive = false
	m.assistantOpen = false // 新的一轮：重新允许打 "◆ arkperf" 头部
	m.sel = selection{}     // 提交后清掉选区，免得高亮留在旧内容上

	// 轮次之间空一行：长对话里没有空行，上一轮到哪儿结束根本看不出来
	lines := []string{}
	if len(m.transcript) > 0 {
		lines = append(lines, "")
	}
	// 前缀 "  › " 占 4 列（比 agent 的 "◆ " 多 2 列前导空格），
	// 折行宽度相应减 2，避免首行顶出屏幕右边界。
	lines = append(lines, styleAccent.Render("  "+userMarker+" ")+wrapText(text, m.contentWidth()-2))
	cmd := m.print(lines...)

	go m.bridge.RunTask(m.ctx, m.run, text)
	return m, tea.Batch(cmd, m.spinner.Tick)
}

// handleApprovalKey 处理审批卡上的按键。
//
// y/Enter 批准，n/Esc 拒绝——两键都有明确语义，不留"随手按回车就放行"的空间。
func (m *Model) handleApprovalKey(k string) (tea.Model, tea.Cmd) {
	if m.pending == nil {
		m.st = stateIdle
		m.input.Focus()
		return m, nil
	}

	var granted bool
	switch k {
	case "y", "Y", "enter":
		granted = true
	case "n", "N", "esc", "ctrl+c":
		granted = false
	default:
		return m, nil // 其他键忽略，审批必须先回答
	}

	p := m.pending
	m.pending = nil
	m.st = stateRunning
	m.input.Focus()
	// 非阻塞投递：后台线程一定在等这个 reply
	select {
	case p.reply <- granted:
	default:
	}

	label := "已批准"
	if !granted {
		label = "已拒绝"
	}
	return m, m.print(styleDim.Render("〔审批〕" + label + " · " + p.name))
}

// ---------------------------------------------------------------- 底部区域渲染

// scrollIndicator 说明当前正在回看历史，并给出回到最新的办法。
//
// 不写这一行，用户看到"内容停在几轮之前"会以为输出丢了。
func (m *Model) scrollIndicator(w int) string {
	behind := m.scrollable() - m.currentTop()
	return ansi.Truncate(
		styleWarn.Render(fmt.Sprintf("回看历史中（下方还有 %d 行新内容）· 滚轮下滚 / PageDown 回到最新", behind)),
		w, "…")
}

func (m *Model) workingLine(w int) string {
	// 旋转指示放在输入框上方（Claude Code 同款）：进度在输入区上面，
	// 快捷键与统计在下面，输入框本身位置固定，不会因为状态变化而跳动。
	parts := []string{m.spinner.View() + " " + styleDim.Render("运行中")}
	if m.turns > 0 || m.toolUses > 0 {
		parts = append(parts, styleDim.Render(fmt.Sprintf("第 %d 轮 · %d 次工具调用", m.turns, m.toolUses)))
	}
	if m.queued != "" {
		parts = append(parts, styleWarn.Render("已排队 1 条"))
	}
	return ansi.Truncate(strings.Join(parts, styleDim.Render(" · ")), w, "…")
}

func (m *Model) approvalCard(w int) string {
	if m.pending == nil {
		return ""
	}
	// inner 减去内边距 2 与边框 2，卡片总宽才能与状态行对齐
	inner := max(w-4, 16)
	lines := []string{
		styleWarn.Bold(true).Render("需要批准") + styleDim.Render("　工具即将执行，可能改变工程或设备状态"),
		"",
		styleAccent.Render(m.pending.name) + " " + styleDim.Render(compact(m.pending.args)),
		"",
		styleOK.Render("y") + styleDim.Render(" 允许　") + styleErr.Render("n") + styleDim.Render(" 拒绝"),
	}
	for i, l := range lines {
		lines[i] = wrapText(l, inner)
	}
	return cardStyle.Width(inner).Render(strings.Join(lines, "\n"))
}

func (m *Model) statusBlock(w int) string {
	tag := modeTagStyle(colModeAsk).Render("ask")
	if m.bridge != nil && m.bridge.AutoApprove() {
		tag = modeTagStyle(colModeAuto).Render("auto")
	}

	// 名字放在状态行里：启动横幅会随转录滚走，这里永远看得到
	row1 := []string{tag, styleAccent.Render("arkperf")}
	if m.opts.ModelName != "" {
		row1 = append(row1, styleDim.Render(m.opts.ModelName))
	}
	if m.opts.ToolCount > 0 {
		row1 = append(row1, styleDim.Render(fmt.Sprintf("%d 工具", m.opts.ToolCount)))
	}
	if m.opts.MCP != "" {
		row1 = append(row1, styleDim.Render(m.opts.MCP))
	}

	state := styleDim.Render("空闲")
	switch m.st {
	case stateRunning:
		state = styleAccent.Render("运行中")
	case stateApproving:
		state = styleWarn.Render("待审批")
	}
	row2 := []string{
		styleDim.Render(shortenPath(m.opts.CWD)),
		state,
		styleDim.Render(m.sessionLabel()),
		styleDim.Render("⏎ 发送 · ^J 换行 · Esc 中断 · ^C 退出 · /help"),
	}

	return ansi.Truncate(strings.Join(row1, styleDim.Render(" · ")), w, "…") + "\n" +
		ansi.Truncate(strings.Join(row2, styleDim.Render(" · ")), w, "…")
}

// sessionLabel 描述会话上下文状态。
//
// 保留轮数少于总轮数时必须说出来：否则"agent 忘了前面说过的话"会被当成
// bug，而实际是历史超预算被裁剪了。这种静默的能力退化，用户有权知道。
func (m *Model) sessionLabel() string {
	if m.opts.SessionTurns == nil && m.opts.SessionRetained == nil {
		return "会话单轮"
	}
	turns, retained := m.sessionCounts()

	var label string
	switch {
	case turns == 0:
		label = "新会话"
	case retained < turns:
		label = fmt.Sprintf("会话 %d 轮（较早 %d 轮已裁剪）", turns, turns-retained)
	default:
		label = fmt.Sprintf("会话 %d 轮", turns)
	}
	// 被压缩过要说出来：否则"agent 说的内容变少了"没有可归因的原因
	if compacted := m.sessionCompacted(); compacted > 0 {
		label += fmt.Sprintf(" · 已压缩 %d 段工具输出", compacted)
	}
	return label
}

func (m *Model) sessionCompacted() int {
	if m.opts.SessionCompacted == nil {
		return 0
	}
	return m.opts.SessionCompacted()
}

func (m *Model) sessionCounts() (turns, retained int) {
	if m.opts.SessionTurns != nil {
		turns = m.opts.SessionTurns()
	}
	if m.opts.SessionRetained != nil {
		retained = m.opts.SessionRetained()
	} else {
		retained = turns
	}
	return turns, retained
}

func (m *Model) slashMenu(w int) string {
	if len(m.slashMatches) == 0 {
		return styleDim.Render("（无匹配命令）")
	}
	lines := make([]string, 0, len(m.slashMatches))
	for i, c := range m.slashMatches {
		cursor := "  "
		name := styleDim.Render("/" + c.Name)
		help := styleDim.Render(c.Help)
		if i == m.slashIdx {
			cursor = styleAccent.Render("❯ ")
			name = styleAccent.Bold(true).Render("/" + c.Name)
			help = styleDim.Render(c.Help)
		}
		lines = append(lines, ansi.Truncate(cursor+name+"  "+help, w, "…"))
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------- 工具方法

// contentWidth 是转录文本可用的宽度（留出一点右边距，避免贴边折行）。
func (m *Model) contentWidth() int { return max(m.width-2, 20) }

// boxContentWidth 是输入框内容的宽度：减去边框 2 与内边距 2。
func (m *Model) boxContentWidth() int { return max(m.width-4, 16) }

func (m *Model) resizeInput() {
	m.input.SetWidth(m.boxContentWidth())
	m.input.SetHeight(min(m.input.LineCount(), maxInputRows))
}

// print 把行写进转录缓冲，由 View 每帧重绘到视口里。
//
// alt-screen 布局下**不能再走 tea.Println**——那会把行打到视口之外，
// 破坏整屏布局。函数保留命令返回值（恒为 nil）是为了调用点不用改：
// tea.Batch 会自动忽略 nil 命令。
//
// 这是唯一的输出出口：所有向转录写内容的地方都走它，
// 免得出现"屏幕上有、内存里没有"（或反过来）的分叉。
func (m *Model) print(lines ...string) tea.Cmd {
	m.transcript = append(m.transcript, lines...)
	if len(m.transcript) > transcriptCap {
		m.transcript = m.transcript[len(m.transcript)-transcriptCap:]
	}
	// 刻意不重置视口：跟随模式下视口自动跟着走；
	// 用户正在回看时，把画面拽回底部等于打断他正在读的东西。
	// 回看状态由 scrollIndicator 明示，不会让人以为内容丢了。
	return nil
}

func (m *Model) remember(text string) {
	if len(m.history) > 0 && m.history[len(m.history)-1] == text {
		return
	}
	m.history = append(m.history, text)
	m.histIdx = len(m.history)
	m.draft = ""
}

// recallHistory 用 Ctrl+P / Ctrl+N 翻历史。
//
// 刻意不用方向键：输入框是多行的，上下键必须留给光标移动。
func (m *Model) recallHistory(delta int) {
	if len(m.history) == 0 {
		return
	}
	if m.histIdx >= len(m.history) {
		m.draft = m.input.Value()
	}
	next := m.histIdx + delta
	switch {
	case next < 0:
		next = 0
	case next >= len(m.history):
		next = len(m.history)
	}
	m.histIdx = next
	if next == len(m.history) {
		m.input.SetValue(m.draft)
	} else {
		m.input.SetValue(m.history[next])
	}
	m.input.CursorEnd()
	m.refreshSlash()
}

// refreshSlash 根据当前输入更新斜杠菜单。
func (m *Model) refreshSlash() {
	v := m.input.Value()
	if !strings.HasPrefix(v, "/") || strings.Contains(v, " ") {
		m.slashActive = false
		m.slashMatches = nil
		return
	}
	prefix := strings.TrimPrefix(v, "/")
	matches := matchingCommands(prefix)
	m.slashMatches = matches
	m.slashActive = len(matches) > 0
	if m.slashIdx >= len(matches) {
		m.slashIdx = 0
	}
}

// ---------------------------------------------------------------- 小工具

func reasonText(r kernel.StopReason) string {
	switch r {
	case kernel.StopIdle:
		return "卡住（连续多轮没有成功调用工具）"
	case kernel.StopTurnValve:
		return "达到轮次上限"
	case kernel.StopInterrupted:
		return "被中断"
	case kernel.StopFinal:
		return "完成"
	default:
		return string(r)
	}
}

// renderAssistant 渲染模型的回答：markdown → 带样式的终端文本。
// 裸打 markdown 的话 ** 和 “ 全是字面字符（实测被用户吐槽）。
func renderAssistant(text string, w int) string {
	return newMarkdownRenderer(w).Render(text)
}

// wrapText 按显示宽度折行（CJK 按 2 列算）。
func wrapText(s string, w int) string {
	if w <= 0 {
		return s
	}
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		out = append(out, ansi.Wrap(line, w, ""))
	}
	return strings.Join(out, "\n")
}

// compact 把参数压成一行。这里不截断：截到多宽取决于终端宽度，
// 由调用方用 lineBudget 决定——固定长度截断会在窄终端上溢出。
func compact(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprintf("%v", args)
	}
	return string(b)
}

func firstLine(s string, limit int) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return ansi.Truncate(line, max(limit, 10), "…")
		}
	}
	return "(无输出)"
}

// shortenPath 保留路径尾部（前面的目录名对用户没信息量）。
func shortenPath(p string) string {
	const keep = 48
	if len(p) <= keep {
		return p
	}
	return "…" + p[len(p)-keep:]
}
