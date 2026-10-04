package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// SlashCommand 是一条前端命令。
type SlashCommand struct {
	Name string
	Help string
	Run  func(m *Model, arg string) tea.Cmd
}

// reportMsg 承载命令的异步结果（探测工具链、查设备都要起子进程，不能卡界面）。
type reportMsg struct {
	title string
	body  string
	err   error
}

// commands 是命令表。
//
// 它同时驱动 /help 与斜杠补全菜单——单一来源，不会出现
// "help 里写着但实际没有"这种分裂。
func commands() []SlashCommand {
	return []SlashCommand{
		{Name: "help", Help: "列出所有命令与按键", Run: cmdHelp},
		{Name: "tools", Help: "列出已注册的工具", Run: cmdTools},
		{Name: "skills", Help: "列出技能（三层目录、来源路径、被跳过的）", Run: cmdSkills},
		{Name: "check", Help: "探测 OpenHarmony 工具链", Run: cmdCheck},
		{Name: "devices", Help: "列出已连接的设备", Run: cmdDevices},
		{Name: "yes", Help: "切换自动批准（免确认执行危险工具）", Run: cmdYes},
		{Name: "rules", Help: "查看已保存的审批规则（哪些类别不再询问）", Run: cmdRules},
		{Name: "status", Help: "显示当前装配状态", Run: cmdStatus},
		{Name: "new", Help: "开始新会话（清空上下文与屏幕）", Run: cmdNew},
		{Name: "sessions", Help: "列出当前工作区的会话；/sessions <序号> 切过去", Run: cmdSessions},
		{Name: "cd", Help: "切换工作目录（工作区，会换成该目录的会话）", Run: cmdCd},
		{Name: "clear", Help: "清空屏幕上的转录（不影响上下文）", Run: cmdClear},
		{Name: "mouse", Help: "切换鼠标捕获：开=滚轮+内置选中，关=交还终端原生划选", Run: cmdMouse},
		{Name: "exit", Help: "退出 TUI", Run: cmdExit},
		{Name: "quit", Help: "退出 TUI（同 /exit）", Run: cmdExit},
	}
}

// matchingCommands 按前缀过滤命令，供斜杠菜单使用。
func matchingCommands(prefix string) []SlashCommand {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	var out []SlashCommand
	for _, c := range commands() {
		if prefix == "" || strings.HasPrefix(c.Name, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// runSlash 执行一条命令。arg 为 nil 表示从菜单选中（无参数）。
//
// **必须先清空输入框**：命令文本留在框里会被当成下一轮的草稿，
// 用户得手动删一遍（实测撞到过）。两条进入路径（菜单选中回车、直接输入回车）
// 都要走这里，所以清在这里最保险。
func (m *Model) runSlash(name string, arg *string) (tea.Model, tea.Cmd) {
	m.slashActive = false
	m.slashMatches = nil
	m.input.SetValue("")
	argVal := ""
	if arg != nil {
		argVal = *arg
	}
	for _, c := range commands() {
		if c.Name == name {
			return m, c.Run(m, argVal)
		}
	}
	return m, m.print(styleErr.Render("未知命令：/" + name + "（用 /help 看清单）"))
}

// ---------------------------------------------------------------- 各命令实现

func cmdHelp(m *Model, _ string) tea.Cmd {
	lines := []string{styleBold.Render("命令")}
	for _, c := range commands() {
		lines = append(lines, "  "+styleAccent.Render("/"+c.Name)+"  "+styleDim.Render(c.Help))
	}
	lines = append(lines,
		"",
		styleBold.Render("按键"),
		styleDim.Render("  ⏎ 发送任务　Ctrl+J / Alt+⏎ 换行　Ctrl+P / Ctrl+N 翻历史"),
		styleDim.Render("  Esc 运行中=中断、空闲=清空输入　Tab 补全命令"),
		styleDim.Render("  Ctrl+C 有选区=复制、否则运行中=中断/空闲=退出　Ctrl+Insert 只复制"),
		"",
		styleBold.Render("会话"),
		styleDim.Render("  · 同一界面里是多轮对话：模型能看到之前几轮说过的话与工具结果"),
		styleDim.Render("  · 会话按工作目录保存，重开界面自动接上最近一段；/new 开始新会话"),
		styleDim.Render("  · 历史超预算时会丢掉最旧的整轮，状态行会标出被裁剪的轮数"),
		styleDim.Render("  · /clear 只清屏幕，不影响模型的上下文；要清上下文用 /new"),
		"",
		styleBold.Render("说明"),
		styleDim.Render("  · 全屏布局：转录在上方视口，输入框钉在屏幕底边"),
		styleDim.Render("  · PageUp / PageDown 或滚轮回看历史；左键拖拽选中文字，Ctrl+C 复制"),
		styleDim.Render("  · 回看时视口不会被新输出拽回底部，滚到最底才恢复跟随最新"),
		styleDim.Render("  · 运行中提交的任务会排队，当前任务结束后自动开始"),
	)
	return m.print(strings.Join(lines, "\n"))
}

func cmdTools(m *Model, _ string) tea.Cmd {
	if m.opts.Registry == nil {
		return m.print(styleDim.Render("（工具清单不可用）"))
	}
	specs := m.opts.Registry.Specs()
	lines := []string{styleBold.Render(fmt.Sprintf("工具（%d）", len(specs)))}
	for _, s := range specs {
		lines = append(lines, "  "+styleAccent.Render(s.Name)+"  "+
			styleDim.Render(ansi.Truncate(s.Description, max(m.contentWidth()-len(s.Name)-6, 20), "…")))
	}
	return m.print(strings.Join(lines, "\n"))
}

// cmdSkills 列出技能：三层目录、每个技能的来源路径、以及被跳过的坏文件与原因。
//
// 技能是**纯数据**——加一个技能不用改代码，往目录里丢个文件夹就行。
// 代价是"我放进去为什么没生效"变成最高频的问题，而它对用户是完全不可见的：
// 模型不会说"我没读到你的技能"，它只会答得不如预期。
// 所以这一条命令把三层目录、胜出者路径、跳过原因全打出来，让人自己就能定位。
//
// 同步执行（不像 /check 那样丢后台）：只读三层目录、十几毫秒，
// 为它绕一圈异步反而让输出顺序变得不确定。
func cmdSkills(m *Model, _ string) tea.Cmd {
	if m.opts.SkillReport == nil {
		return m.print(styleDim.Render("（技能清单不可用）"))
	}
	return m.print(m.opts.SkillReport())
}

// cmdCheck 探测工具链。会用 ProbeVersion 起子进程，所以放到后台执行。
func cmdCheck(m *Model, _ string) tea.Cmd {
	if m.opts.ToolchainReport == nil {
		return m.print(styleDim.Render("（工具链探测不可用）"))
	}
	report := m.opts.ToolchainReport
	ctx := m.ctx
	return tea.Batch(
		m.print(styleDim.Render("探测工具链中…")),
		func() tea.Msg {
			body, err := report(ctx)
			return reportMsg{title: "工具链", body: body, err: err}
		},
	)
}

func cmdDevices(m *Model, _ string) tea.Cmd {
	if m.opts.DeviceReport == nil {
		return m.print(styleDim.Render("（设备查询不可用）"))
	}
	report := m.opts.DeviceReport
	ctx := m.ctx
	return tea.Batch(
		m.print(styleDim.Render("查询设备中…")),
		func() tea.Msg {
			body, err := report(ctx)
			return reportMsg{title: "设备", body: body, err: err}
		},
	)
}

// cmdYes 切换自动批准。危险的开关，所以每次切换都把当前状态说清楚。
func cmdYes(m *Model, _ string) tea.Cmd {
	on := !m.bridge.AutoApprove()
	m.bridge.SetAutoApprove(on)
	m.opts.AutoApprove = on
	if on {
		return m.print(styleWarn.Render("已开启自动批准：需要审批的工具将直接执行，不再询问"))
	}
	return m.print(styleDim.Render("已关闭自动批准：需要审批的工具会先询问"))
}

// cmdRules 列出"以后别再问"的记忆。
//
// 为什么必须有这条命令：规则的作用是**减少询问**，所以一条过宽的规则
// 是静默的——不会有任何提示告诉用户"你上次放行的那一类其实很宽"。
// 有一个地方能看见自己放行了什么，是这类功能的基本要求。
func cmdRules(m *Model, _ string) tea.Cmd {
	if m.opts.RulesReport == nil {
		return m.print(styleDim.Render("（审批规则不可用）"))
	}
	return m.print(m.opts.RulesReport())
}

func cmdStatus(m *Model, _ string) tea.Cmd {
	lines := []string{styleBold.Render("装配状态")}
	add := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			lines = append(lines, "  "+styleDim.Render(k+"：")+v)
		}
	}
	add("模型", m.opts.ModelName)
	add("工具", fmt.Sprintf("%d 个", m.opts.ToolCount))
	if m.opts.SkillSummary != nil {
		add("技能", m.opts.SkillSummary())
	}
	add("工具链", m.opts.Toolchain)
	add("MCP", m.opts.MCP)
	if m.bridge.AutoApprove() {
		add("审批", styleWarn.Render("auto（免确认）"))
	} else {
		add("审批", "ask（危险工具先询问）")
	}
	add("工作目录", m.opts.CWD)
	turns, retained := m.sessionCounts()
	if turns == 0 {
		add("会话", styleDim.Render("新会话（尚无历史）"))
	} else if retained < turns {
		add("会话", fmt.Sprintf("%d 轮（保留 %d 轮，较早的已裁剪）", turns, retained))
	} else {
		add("会话", fmt.Sprintf("%d 轮（全部保留在上下文中）", turns))
	}
	if compacted := m.sessionCompacted(); compacted > 0 {
		add("已压缩", fmt.Sprintf("%d 段早期工具输出（保留首尾摘录，结构完整）", compacted))
	}
	return m.print(strings.Join(lines, "\n"))
}

// cmdNew 开始新会话：清空模型上下文并新建持久化记录。
//
// **屏幕一起清掉**。旧版留着上一段对话，理由是"那是历史记录"——
// 但用户的心智是"我开了个新会话"：屏幕还挂着旧对话就不像新的，
// 也容易出现"屏幕上写着 A、模型手里是空的"这种错位（与 /cd 同一条原则）。
// 旧会话并没有丢：每轮任务结束都落过盘，可以用 /sessions 切回去。
func cmdNew(m *Model, _ string) tea.Cmd {
	if m.opts.ResetSession == nil {
		return m.print(styleDim.Render("（当前不支持新建会话）"))
	}
	if err := m.opts.ResetSession(); err != nil {
		return m.print(styleErr.Render("✗ 新建会话失败：" + err.Error()))
	}
	m.resetViewState()
	return m.print(styleWarn.Render("已开始新会话：上下文与屏幕都已清空") + "\n" +
		styleDim.Render("之前的对话仍在磁盘上，用 /sessions 可以切回去"))
}

// cmdSessions 列出当前工作区的会话；带序号则切过去。
//
// 为什么需要它：TUI 是交付形态之一，但在它之前**没有任何办法切回旧会话**——
// /new 之后那段对话就只能靠桌面端侧栏找回来（实测被用户问到"新旧会话怎么切"）。
func cmdSessions(m *Model, arg string) tea.Cmd {
	if m.opts.Sessions == nil {
		return m.print(styleDim.Render("（当前不支持会话列表）"))
	}

	list := m.opts.Sessions()
	arg = strings.TrimSpace(arg)

	if arg == "" {
		if len(list) == 0 {
			return m.print(styleDim.Render("当前工作区还没有会话（发第一条消息后就会出现）"))
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s\n", styleBold.Render(fmt.Sprintf("当前工作区的会话（%d）", len(list))))
		for i, s := range list {
			mark := "  "
			if s.Current {
				mark = styleAccent.Render("▸ ")
			}
			fmt.Fprintf(&b, "%s%d. %s  %s\n", mark, i+1, s.Title,
				styleDim.Render(fmt.Sprintf("(%d 轮 · %s)", s.Turns, s.Updated)))
		}
		b.WriteString(styleDim.Render("用法：/sessions <序号> 切过去（会清屏并回放该会话的历史）"))
		return m.print(strings.TrimRight(b.String(), "\n"))
	}

	idx, err := strconv.Atoi(arg)
	if err != nil || idx < 1 || idx > len(list) {
		return m.print(styleErr.Render(fmt.Sprintf("✗ 序号无效：%s（先输 /sessions 看列表）", arg)))
	}
	target := list[idx-1]
	if target.Current {
		return m.print(styleDim.Render("已经在这个会话里了"))
	}
	if m.opts.OpenSession == nil {
		return m.print(styleDim.Render("（当前不支持切换会话）"))
	}

	lines, dir, err := m.opts.OpenSession(target.ID)
	if err != nil {
		return m.print(styleErr.Render("✗ 切换会话失败：" + err.Error()))
	}

	// 工作区可能跟着会话一起切了：状态行必须以返回值更新，
	// 否则屏幕写着旧目录、模型实际在新目录里干活。
	m.opts.CWD = dir
	m.replay(lines)
	return m.print(styleWarn.Render("已切到会话："+target.Title) + "\n" +
		styleDim.Render(fmt.Sprintf("工作区 %s · %d 轮 · 屏幕已回放该会话的历史",
			dir, target.Turns)))
}

func cmdClear(m *Model, _ string) tea.Cmd {
	// 交给 Update 统一处理：那里既能清内存副本，也能清屏
	return func() tea.Msg { return resetView{} }
}

// cmdCd 切换工作目录（工作区）。
//
// 换目录＝换项目：模型上下文随之换成那个目录自己的最近会话（装配层负责接上）。
// **屏幕必须一起清掉**——否则会出现"屏幕上是 A 项目的对话，模型却拿着 B 项目的
// 历史在干活"这种最危险的错位。这与 /new、/clear 的取舍是同一条原则：
// 任何时候都别让屏幕显示的东西和模型实际持有的东西不一致。
func cmdCd(m *Model, arg string) tea.Cmd {
	dir := strings.TrimSpace(arg)
	if dir == "" {
		return m.print(styleDim.Render("用法：/cd <目录>") + "\n" +
			styleDim.Render("  当前工作目录："+m.opts.CWD))
	}
	if m.opts.SwitchWorkspace == nil {
		return m.print(styleDim.Render("（当前不支持切换工作目录）"))
	}

	next, err := m.opts.SwitchWorkspace(dir)
	if err != nil {
		return m.print(styleErr.Render("✗ 切换失败：" + err.Error()))
	}

	m.opts.CWD = next // 状态行与 /status 显示的目录要跟着变
	// 同步清屏（不用 tea.Batch：并发跑"清屏"与"打印说明"的顺序是不保证的，
	// 说明有可能被自己清掉），再把说明放进清空后的屏幕
	m.resetViewState()
	return m.print(styleWarn.Render("已切换工作目录："+next) + "\n" +
		styleDim.Render("上下文已换成该目录的最近会话（屏幕一并清空，避免与上下文错位）；"+
			"/status 看会话规模，/new 开始新会话"))
}

func cmdExit(_ *Model, _ string) tea.Cmd { return tea.Quit }

// cmdMouse 切换鼠标捕获（学自 Reasonix 的 /mouse）。
//
// 默认是**开捕获**：滚轮/滚动条和 app 内选中（左键拖拽→反色→松手自动复制）
// 同时可用。关掉捕获是把鼠标交还终端，用它的原生划选/右键菜单——
// 只有 SSH/Termux 这类"终端本地复制更靠谱"的环境才需要。
func cmdMouse(m *Model, _ string) tea.Cmd {
	m.mouseCaptureOff = !m.mouseCaptureOff
	// 关捕获时清掉 app 内选区：鼠标已经交还终端，这套高亮没意义了
	m.sel = selection{}
	if m.mouseCaptureOff {
		return m.print(styleDim.Render("已关闭鼠标捕获：改用终端原生划选/右键菜单（滚轮失效，回看用 PageUp / PageDown）"))
	}
	return m.print(styleDim.Render("已开启鼠标捕获：滚轮/滚动条可用；左键拖拽选中文字，Ctrl+C 复制"))
}
