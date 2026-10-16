package tui

import (
	"fmt"
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
		{Name: "rename", Help: "重命名当前会话：/rename 新名字（空串恢复自动标题）", Run: cmdRename},
		{Name: "delete", Help: "删除当前会话（不可恢复；自动切到最近的会话）", Run: cmdDelete},
		{Name: "exit", Help: "退出 TUI", Run: cmdExit},
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
	// 非 delete 的命令都会取消待确认的删除：意图已经变了，
	// "再敲一次 /delete"不能一直挂着等人误触。
	if name != "delete" {
		m.deletePendingID = ""
	}
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
		styleDim.Render("  Ctrl+B / Ctrl+E 折叠、展开左侧栏（会话）/ 右侧栏（文件）"),
		"",
		styleBold.Render("会话"),
		styleDim.Render("  · 同一界面里是多轮对话：模型能看到之前几轮说过的话与工具结果"),
		styleDim.Render("  · 会话按工作目录保存，重开界面自动接上最近一段；/new 开始新会话"),
		styleDim.Render("  · 历史超预算时会丢掉最旧的整轮，状态行会标出被裁剪的轮数"),
		styleDim.Render("  · /clear 只清屏幕，不影响模型的上下文；要清上下文用 /new"),
		"",
		styleBold.Render("说明"),
		styleDim.Render("  · 三栏布局：左=当前工作区会话，中=对话，右=工作区文件树"),
		styleDim.Render("  · 终端窄于 120 列时自动退回单栏；点右栏文件名可把相对路径插进输入框"),
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

// /cd（在 TUI 内切换工作区）已移除。
//
// 换工作区现在只有两条路：换目录再启动，或者切到别的目录的会话
//（openSessionIdx 会把 CWD 一起跟过去）。保留一个能从界面里改工作区的命令，
// 等于给"屏幕显示 A、模型拿着 B"这种错位多开一个入口——收益不抵风险，
// 而且它要求整屏清空，本来就与"边看边切"的使用方式是冲突的。
//
// 装配层的 SetWorkspace 仍在（跨工作区的会话切换依赖它）。

// currentSessionID 返回当前会话的 id；列表里找不到（新会话还没落盘）则返回空串。
func (m *Model) currentSessionID() string {
	for _, s := range m.sessionsCache {
		if s.Current {
			return s.ID
		}
	}
	return ""
}

// cmdRename 重命名当前会话。
//
// 参数整串当名字（允许空格）；空串恢复"第一条提问"的自动标题（kernel.Rename 的语义）。
// 只对列表里的会话有效：新会话还没落盘，没有文件可改名。
func cmdRename(m *Model, arg string) tea.Cmd {
	if m.opts.RenameSession == nil {
		return m.print(styleDim.Render("（当前不支持重命名会话）"))
	}
	arg = strings.TrimSpace(arg)
	id := m.currentSessionID()
	if id == "" {
		return m.print(styleDim.Render("当前会话还没有内容，先聊一轮再来命名它"))
	}
	if err := m.opts.RenameSession(id, arg); err != nil {
		return m.print(styleErr.Render("✗ 重命名失败：" + err.Error()))
	}
	m.refreshSessions()
	if arg == "" {
		return m.print(styleDim.Render("已清除自定义名，恢复自动标题"))
	}
	return m.print(styleDim.Render("已重命名：" + arg))
}

// currentSessionTitle 返回当前会话的展示标题（自定义名或会话 id）；找不到返回空串。
func (m *Model) currentSessionTitle() string {
	for _, s := range m.sessionsCache {
		if s.Current {
			if t := strings.TrimSpace(s.Title); t != "" {
				return t
			}
			return s.ID
		}
	}
	return ""
}

// cmdDelete 删除当前会话（不可恢复），**两段式确认**：
// 第一次只提示（会话名），第二次才真删。
//
// 待确认时记录的是**被确认的那个会话 id**——第二次删它，而不是删的时候
// 重新取当前会话：中间切了会话的话，删的也该是用户看过确认提示的那个。
// 任何其他提交（别的命令、普通消息）都会取消待确认状态。
func cmdDelete(m *Model, _ string) tea.Cmd {
	if m.opts.DeleteSession == nil {
		return m.print(styleDim.Render("（当前不支持删除会话）"))
	}

	id := m.deletePendingID
	if id == "" {
		id = m.currentSessionID()
		if id == "" {
			return m.print(styleDim.Render("当前会话还没有内容——点左栏的 + New session 开新的就行"))
		}
		m.deletePendingID = id
		return m.print(styleWarn.Render(fmt.Sprintf(
			"⚠ 将删除会话「%s」，不可恢复。确认的话再敲一次 /delete；敲其他命令取消。",
			m.currentSessionTitle())))
	}
	m.deletePendingID = ""

	if err := m.opts.DeleteSession(id); err != nil {
		return m.print(styleErr.Render("✗ 删除失败：" + err.Error()))
	}
	m.refreshSessions()
	// 找删后的当前会话，把它的历史回放到屏幕上
	for _, s := range m.sessionsCache {
		if !s.Current {
			continue
		}
		if lines, dir, err := m.opts.OpenSession(s.ID); err == nil {
			if dir != "" {
				m.opts.CWD = dir
				m.resetFilesView()
				m.refreshFiles()
			}
			m.replay(lines)
			return m.print(styleDim.Render("已删除会话；已切到「" + s.Title + "」"))
		}
	}
	// 一条都不剩：回到新会话的空白 + 横幅
	m.resetViewState()
	m.printBanner()
	return m.print(styleDim.Render("已删除会话；这是新的开始"))
}

func cmdExit(_ *Model, _ string) tea.Cmd { return tea.Quit }
