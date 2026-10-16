package tui

import (
	"fmt"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// newPanesHarness 造一个带侧栏数据源的模型。
//
// 三栏的所有断言都依赖"上下文里有会话、工作区里有文件"，
// 默认 harness 这两项都是空的（于是左栏只有"暂无会话"），
// 测不出任何交互。
func newPanesHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, false)
	h.m.opts.Sessions = func() []SessionBrief {
		return []SessionBrief{
			{ID: "s1", Title: "冷启动优化", Turns: 3, Updated: "10-16", Current: true},
			{ID: "s2", Title: "帧率分析", Turns: 8, Updated: "10-15"},
			{ID: "s3", Title: "内存排查", Turns: 2, Updated: "10-14"},
		}
	}
	h.m.opts.OpenSession = func(id string) ([]ReplayLine, string, error) {
		return []ReplayLine{{Role: "user", Content: "历史：" + id}}, "", nil
	}
	h.m.opts.ListFiles = func() []FileNode {
		return []FileNode{
			{Name: "internal", Path: "internal", IsDir: true, Children: []FileNode{
				{Name: "kernel", Path: "internal/kernel", IsDir: true, Children: []FileNode{
					{Name: "app.go", Path: "internal/kernel/app.go"},
				}},
			}},
			{Name: "README.md", Path: "README.md"},
		}
	}
	return h
}

// 宽度决策：按 opencode 的策略——能放下 [栏 + 正文**偏好**宽 64] 才显示侧栏。
func TestColumnsFallBackOnNarrowTerminal(t *testing.T) {
	h := newPanesHarness(t)

	cases := []struct {
		width                   int
		wantSessions, wantFiles int
	}{
		{60, 0, 0},    // 窄终端：单栏
		{91, 0, 0},    // 差一列放不下 [左栏 28 + 正文偏好 64]：整块让给正文
		{92, 28, 0},   // 刚好够左栏
		{123, 28, 0},  // 还差一列放右栏
		{124, 28, 32}, // 刚好够三栏
		{240, 28, 32},
	}
	for _, c := range cases {
		h.resize(c.width, 30)
		sw, bw, fw := h.m.columns()
		if sw != c.wantSessions || fw != c.wantFiles {
			t.Errorf("宽 %d：侧栏 %d/%d，期望 %d/%d", c.width, sw, fw,
				c.wantSessions, c.wantFiles)
		}
		if sw+bw+fw != c.width {
			t.Errorf("宽 %d：三栏之和 %d，必须恰好等于终端宽度",
				c.width, sw+bw+fw)
		}
		if bw < contentMinW {
			t.Errorf("宽 %d：正文只剩 %d 列（下限 %d）", c.width, bw, contentMinW)
		}
	}
}

// 三栏渲染的每一行都必须**恰好**等于终端宽度。
//
// 多一列就会被终端自动折行，整屏布局随之错位——而且只在特定宽度下复现，
// 所以这里扫一组宽度而不是只测一个。
func TestPaneLinesFillExactTerminalWidth(t *testing.T) {
	h := newPanesHarness(t)
	for _, w := range []int{120, 121, 140, 160, 199, 200} {
		h.resize(w, 24)
		for i, line := range strings.Split(h.m.View().Content, "\n") {
			if got := ansi.StringWidth(line); got != w {
				t.Fatalf("宽 %d 的第 %d 行宽度为 %d（应为 %d）：%q",
					w, i, got, w, ansi.Strip(line))
			}
		}
	}
}

// 行数恒等于终端高度（三栏同高，横向拼接不会改变行数）。
func TestPaneRowsMatchTerminalHeight(t *testing.T) {
	h := newPanesHarness(t)
	for _, ht := range []int{12, 24, 40} {
		h.resize(160, ht)
		if got := strings.Count(h.m.View().Content, "\n") + 1; got != ht {
			t.Fatalf("高 %d：帧行数 %d", ht, got)
		}
	}
}

// 窄终端不渲染侧栏（也不该出现侧栏的标题）。
func TestNarrowTerminalHasNoPanes(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(80, 24)
	c := ansi.Strip(h.m.View().Content)
	for _, unwanted := range []string{"Sessions", "Files"} {
		if strings.Contains(c, unwanted) {
			t.Fatalf("窄终端不该渲染 %s 栏：\n%s", unwanted, c)
		}
	}
}

// 宽终端里三栏都出现，且内容确实是数据源给的。
func TestWideTerminalShowsAllPanes(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)
	c := ansi.Strip(h.m.View().Content)
	for _, want := range []string{"Sessions", "冷启动优化", "帧率分析", "Files", "internal", "README.md"} {
		if !strings.Contains(c, want) {
			t.Fatalf("三栏里缺少 %q：\n%s", want, c)
		}
	}
}

// 点左栏的标题行是"折叠"，不是"切会话"。
//
// 这两件事撞在一起很常见（标题行也在左栏里），混起来的话用户想收侧栏
// 却把当前对话切走了。
func TestClickSessionsHeaderToggles(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)
	if !h.m.showSessions {
		t.Fatal("默认应当是展开的")
	}
	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 0})
	if h.m.showSessions {
		t.Fatal("点标题行应当折叠左栏")
	}
	// 折叠后只剩一个图标列，再点一次应当展开
	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: 0})
	if !h.m.showSessions {
		t.Fatal("再点应当展开左栏")
	}
}

// 点左栏的会话项立刻切换（用户明确要求"立刻切换"）。
func TestClickSessionSwitchesImmediately(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	// 列表从第 4 行开始（标题 1 行 + 按钮 3 行）、每项 2 行：idx1（帧率分析）落在 y=6
	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 6})

	if h.m.sessionIdx != 1 {
		t.Fatalf("高亮项应为 1，实得 %d", h.m.sessionIdx)
	}
	if out := h.allTranscript(); !strings.Contains(out, "历史：s2") {
		t.Fatalf("应当回放目标会话的历史：\n%s", out)
	}
	if out := h.allTranscript(); !strings.Contains(out, "已切换到会话") {
		t.Fatalf("应当提示已切换：\n%s", out)
	}
}

// 点当前会话不该清屏重放（那是无意义的闪烁，还会把正在读的内容顶掉）。
func TestClickCurrentSessionIsNoop(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)
	h.m.print("原有内容")

	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 2}) // idx0 = 当前

	if out := h.allTranscript(); !strings.Contains(out, "原有内容") {
		t.Fatalf("切到当前会话不该清空转录：\n%s", out)
	}
}

// 点右栏的文件把相对路径插进输入框。
func TestClickFileInsertsPath(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	// 默认只展开第一层（顶层目录展开、其子目录折叠），
	// 所以要够到 app.go 得先把 internal/kernel 打开。
	delete(h.m.collapsedDirs, "internal/kernel")
	h.m.filesRows = nil // 摊平结果是渲染时算的，改了折叠状态要让它重建

	// 右栏起始列 = 42 + 76 = 118；树从 y=2 开始
	// y=2 internal / y=3 kernel / y=4 app.go
	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 140, Y: 4})

	if got := h.m.input.Value(); got != "internal/kernel/app.go" {
		t.Fatalf("输入框应插入相对路径，实得 %q", got)
	}
}

// 点右栏的目录是折叠该目录，不是插入路径。
func TestClickDirCollapsesOnlyThatDir(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 140, Y: 2}) // internal

	if !h.m.collapsedDirs["internal"] {
		t.Fatal("internal 应被折叠（它默认是展开的，点一下应当收起）")
	}
	// 点父目录不该连带翻转子目录的状态：它们是各自独立的
	if !h.m.collapsedDirs["internal/kernel"] {
		t.Fatal("点父目录不该改变子目录的折叠状态")
	}
	if got := h.m.input.Value(); got != "" {
		t.Fatalf("点目录不该往输入框写东西，实得 %q", got)
	}
	// 折叠后子节点不该再出现在右栏
	if c := ansi.Strip(h.m.View().Content); strings.Contains(c, "kernel") {
		t.Fatalf("折叠后不该还显示子目录：\n%s", c)
	}
}

// 在侧栏上按下鼠标**不能**在正文里划出选区。
//
// 这是加栏之后最容易出的问题：列坐标没减左栏偏移，
// 点侧栏就等于点了正文的某个位置。
func TestClickOnPanesDoesNotStartSelection(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)
	h.m.print("正文内容")
	h.m.View() // 建立 contentLines / viewportRows

	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 20})
	if h.m.sel.active {
		t.Fatalf("点左栏不该开始选区：%+v", h.m.sel)
	}
	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 150, Y: 20})
	if h.m.sel.active {
		t.Fatalf("点右栏不该开始选区：%+v", h.m.sel)
	}
}

// 正文里的拖选坐标要减掉左栏偏移：点正文最左列 → 内容列 0。
func TestSelectionColumnExcludesLeftPane(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)
	h.m.print("正文内容")
	h.m.View()

	sw, _, _ := h.m.columns()
	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: sw, Y: 0})

	if !h.m.sel.active {
		t.Fatal("在正文上按下应当开始选区")
	}
	if got := h.m.sel.anchor.col; got != 0 {
		t.Fatalf("正文最左列应映射成内容列 0，实得 %d（左栏偏移没减？）", got)
	}
}

// 折叠侧栏后正文变宽：这是"拉出/收起"的实际收益。
func TestCollapsingPaneWidensBody(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)
	before := h.m.bodyWidth()

	h.m.toggleSessions()
	h.m.resizeInput()
	if after := h.m.bodyWidth(); after <= before {
		t.Fatalf("折叠左栏后正文应变宽：%d → %d", before, after)
	}

	// 折叠到只剩图标列时，点击那个图标要能拉回来
	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: 0})
	if !h.m.showSessions {
		t.Fatal("点折叠条应当展开左栏")
	}
}

// 右栏标题行的折叠（Ctrl+E 与点击走同一条路径）。
func TestToggleFilesByKey(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	if !h.m.showFiles {
		t.Fatal("右栏默认应当打开")
	}
	h.m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	if h.m.showFiles {
		t.Fatal("Ctrl+E 应当折叠右栏")
	}
	h.m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	if !h.m.showFiles {
		t.Fatal("再按一次应当展开右栏")
	}
}

// 正文宽度随栏位变化，折行也必须跟着变——
// 否则转录会顶穿右栏（最后几列被右栏覆盖）。
func TestTranscriptWrapFollowsBodyWidth(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)
	h.m.print(strings.Repeat("宽", 200))
	h.m.View()

	bw := h.m.bodyWidth()
	for _, l := range h.m.contentLines {
		if got := ansi.StringWidth(l); got > bw {
			t.Fatalf("转录行宽 %d 超过正文栏宽 %d", got, bw)
		}
	}
}

// TestPanesPreview 把三栏布局渲染出来，供人工核对外观。
//
// 只在设置了 ARKPERF_PANE_PREVIEW=1 时运行：它是一张"截图"而不是断言，
// 默认跑它只会给测试输出添噪声。
func TestPanesPreview(t *testing.T) {
	if os.Getenv("ARKPERF_PANE_PREVIEW") == "" {
		t.Skip("设 ARKPERF_PANE_PREVIEW=1 查看三栏预览")
	}
	h := newPanesHarness(t)
	h.m.opts.ModelName = "deepseek-v4.1"
	h.m.opts.ToolCount = 30
	h.m.opts.MCP = "MCP 5/5 · 13 工具"
	h.m.opts.CWD = `E:/ArkPerf`
	h.m.opts.SessionTurns = func() int { return 3 }
	h.resize(160, 34)

	h.m.print(styleAccent.Render("◆ arkperf"))
	h.m.print(renderAssistant("冷启动耗时 **-68.2%**（p=0.0002），报告已归档到 reports/。", h.m.contentWidth()))
	h.m.print(styleDim.Render("──── 完成 · 3 轮 · 7 次工具调用"))

	fmt.Println("===== A. 未探测到终端背景色：退回竖线分栏 =====")
	fmt.Println(h.m.View().Content)

	// 测试环境的 stdout 不是 TTY，lipgloss 会把颜色降级掉、bgSeq 拿到空串
	// ——这正是 A 展示的行为（无色终端照样有清晰的三栏）。
	// 想看色块模式就手动给上序列；这一段只服务预览，不参与任何断言。
	h.m.panePrefix, h.m.paneReset = "\x1b[48;5;235m", "\x1b[m"

	fmt.Println("\n===== B. 探测到深色终端：背景色块分栏 =====")
	fmt.Println(h.m.View().Content)

	// C. 内容超过一屏：正文最右一列出现滚动条（对齐 opencode 主滚动区那条）
	h.resize(160, 20)
	for i := range 40 {
		h.m.print(fmt.Sprintf("第 %d 行内容", i))
	}
	// 先渲染一帧：滚动条的有无取决于上一帧的行数（见 renderTranscript 里的说明），
	// 所以第一帧还没有它。真实运行时每帧都渲染，肉眼看不出这一帧的差。
	h.m.View()
	fmt.Println("\n===== C. 转录超过一屏：右侧滚动条 =====")
	fmt.Println(h.m.View().Content)
}

// 探测到终端背景色时，整行都要落在色块里。
//
// 重点是行内样式结束后的部分：lipgloss 会输出 reset，把外层背景一并清掉，
// 不在 reset 之后补回背景的话，色块会在"彩色文字"那里断掉，
// 表现为栏里只有文字那一小段有色、后面补齐的空格全是底色。
//
// 直接测 paintPane 而不测 renderPane：后者末尾还有竖线自己的一组样式码，
// 两组 reset 混在一行里没法点数。
func TestPaneColorBlockCoversWholeLine(t *testing.T) {
	h := newPanesHarness(t)
	h.m.panePrefix, h.m.paneReset = "\x1b[48;5;235m", "\x1b[m"

	// 一行里塞两段带颜色的内容 + 补齐的空格：每段结束都会产生一个 reset
	line := styleAccent.Render("Sessions") + styleDim.Render(" 3") + "        "
	painted := h.m.paintPane(line)

	if !strings.HasPrefix(painted, h.m.panePrefix) {
		t.Fatalf("行首没有铺背景：%q", painted)
	}
	if !strings.HasSuffix(painted, h.m.paneReset) {
		t.Fatalf("行尾没有收尾 reset：%q", painted)
	}
	// 去掉行尾收尾的那一个，剩下每个 reset 后面都必须紧跟背景序列
	inner := strings.TrimSuffix(painted, h.m.paneReset)
	if n1, n2 := strings.Count(inner, h.m.paneReset),
		strings.Count(inner, h.m.paneReset+h.m.panePrefix); n1 != n2 {
		t.Fatalf("有 %d 个 reset 没补回背景（色块会断开）：%q", n1-n2, painted)
	}
	// 补出来的空格要落在色块里
	if !strings.HasSuffix(strings.TrimSuffix(painted, h.m.paneReset), "        ") {
		t.Fatalf("行尾填充没有落在色块内：%q", painted)
	}
}

// 未探明主题时不铺任何背景：宁可少一层观感，也不猜。
func TestPaneWithoutProbePaintsNothing(t *testing.T) {
	h := newPanesHarness(t)
	line := styleAccent.Render("Sessions") + "        "
	if got := h.m.paintPane(line); got != line {
		t.Fatalf("未探测到背景色时不该改动内容：\n得到 %q\n期望 %q", got, line)
	}
}

// 没有颜色能力时退回竖线：这是"不猜终端主题"的落地方式。
//
// bgSeq 依赖 lipgloss 的颜色能力判断；拿不到序列时 panePrefix 为空，
// 分栏完全靠竖线，结构依然清楚。
func TestPaneFallsBackToBorderWithoutColor(t *testing.T) {
	prefix, _ := bgSeq(currentTheme.Surface)
	if prefix != "" {
		t.Skipf("当前环境有颜色能力（prefix=%q），该用例只在无颜色环境下生效", prefix)
	}

	h := newPanesHarness(t)
	h.resize(160, 30)
	if got := h.m.View().Content; strings.Contains(got, "\x1b[48;5;235m") {
		t.Fatalf("无颜色能力时不该铺色块：%q", got)
	}
	if got := h.m.View().Content; !strings.Contains(got, "│") {
		t.Fatalf("无色环境下必须靠竖线分栏，但没找到分隔线：%q", got)
	}
}

// 未收到背景色消息之前不得铺栏背景。
//
// 这里必须精确匹配栏背景色（235/253），不能用 "\x1b[48;5;" 泛匹配——
// 模式徽标（ask/auto）自己就带背景色，泛匹配会把"没铺"误判成"铺了"。
func TestNoBackgroundPaintBeforeProbe(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	got := h.m.View().Content
	for _, seq := range []string{"\x1b[48;5;235m", "\x1b[48;5;253m"} {
		if strings.Contains(got, seq) {
			t.Fatalf("还没收到背景色消息就铺了栏背景 %q：%q", seq, got)
		}
	}
}

// 文件树默认**只展开第一层**（顶层目录展开、它们的子目录折叠）。
//
// 全展开会把几十上百行塞进只有几十行的侧栏：首屏全是枝叶、要滚动才看得全，
// 顶层有哪些目录反而被淹没。只展开第一层时，首屏恰好是
// "顶层有哪些目录 + 每个目录里有什么"，一眼看完。
func TestFileTreeDefaultsToFirstLevelOnly(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	if h.m.collapsedDirs["internal"] {
		t.Fatal("顶层目录不该默认折叠")
	}
	if !h.m.collapsedDirs["internal/kernel"] {
		t.Fatal("更深一层的目录应当默认折叠")
	}
	// 屏幕上也不该出现第三层的内容
	if c := ansi.Strip(h.m.View().Content); strings.Contains(c, "app.go") {
		t.Fatalf("默认不该展开到第三层：\n%s", c)
	}
}

// 用户手动展开过的目录，不能被后续刷新收回去。
//
// 尺寸变化等都会走 refreshFiles；默认折叠只该在**首次**应用一次，
// 否则用户每调一次窗口，刚展开的目录就自己收起来了。
func TestUserExpandedDirSurvivesRefresh(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	delete(h.m.collapsedDirs, "internal/kernel") // 手动展开
	h.m.refreshFiles()                           // 模拟尺寸变化触发的刷新

	if h.m.collapsedDirs["internal/kernel"] {
		t.Fatal("刷新不该把用户展开过的目录收回去")
	}
}

// 换工作区时重置文件树的浏览状态：旧的折叠记录按路径对不上，
// 滚动位置也没有意义（容易表现为"新目录里某个文件夹莫名其妙是收起的"）。
func TestResetFilesViewClearsExpansion(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	delete(h.m.collapsedDirs, "internal/kernel")
	h.m.filesScroll = 3

	h.m.resetFilesView()
	h.m.refreshFiles()

	if !h.m.collapsedDirs["internal/kernel"] {
		t.Fatal("换工作区后应当回到默认折叠")
	}
	if h.m.filesScroll != 0 {
		t.Fatalf("滚动位置应当归零，实得 %d", h.m.filesScroll)
	}
}

// 转录超过一屏时，正文最右一列出现滚动条（对齐 opencode 主滚动区那条）。
//
// 不满一屏时**不留**这一列：常驻会让每一行都多一个行尾字符，
// 而绝大多数时候内容根本不满一屏。
func TestTranscriptScrollbarAppearsWhenOverflowing(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 100, 20
	h.m.resizeInput()

	h.m.print("只有一行")
	if c := h.m.View().Content; strings.Contains(c, "▐") {
		t.Fatal("内容不满一屏时不该有滚动条")
	}

	for i := range 40 {
		h.m.print(fmt.Sprintf("第 %d 行", i))
	}
	// 先渲染一帧建立行数：滚动条的有无取决于上一帧的行数（见 renderTranscript）
	h.m.View()
	if c := h.m.View().Content; !strings.Contains(c, "▐") {
		t.Fatal("内容超过一屏时应当出现滚动条")
	}
}

// 滚动条那一列**不算进正文宽度**：它是装饰，不是内容。
// 算进去的话，命中测试会把"点在滚动条上"当成"点在正文里"，
// 拖选会从内容之外开始。
func TestScrollbarColumnExcludedFromContentWidth(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 100, 20
	h.m.resizeInput()
	for i := range 40 {
		h.m.print(fmt.Sprintf("第 %d 行", i))
	}
	h.m.View()
	h.m.View() // 第二帧起滚动条生效

	_, bw, _ := h.m.columns()
	if h.m.contentW != bw-1 {
		t.Fatalf("有滚动条时正文宽应为 %d，实得 %d", bw-1, h.m.contentW)
	}
}

// 滚动条可以**点**也可以**拖**，不是摆设。
func TestScrollbarClickJumpsToPosition(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 100, 20
	h.m.resizeInput()
	for i := range 100 {
		h.m.print(fmt.Sprintf("第 %d 行", i))
	}
	h.m.View()
	h.m.View() // 第二帧起滚动条生效

	col := h.m.scrollbarCol()
	if col < 0 {
		t.Fatal("内容超屏时应当有滚动条")
	}

	// 点最上面一行 → 跳到最顶
	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: col, Y: 0})
	if got := h.m.currentTop(); got != 0 {
		t.Fatalf("点滚动条顶部应当跳到最顶，实得 %d", got)
	}
	if h.m.follow {
		t.Fatal("不在底部时不该处于跟随状态")
	}

	// 点最下面一行 → 到底，并恢复跟随
	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: col,
		Y: h.m.viewportRows - 1})
	if !h.m.follow {
		t.Fatal("拖到底应当恢复跟随最新")
	}
}

// 拖动滚动条时视口跟着走。
func TestScrollbarDragMovesViewport(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 100, 20
	h.m.resizeInput()
	for i := range 100 {
		h.m.print(fmt.Sprintf("第 %d 行", i))
	}
	h.m.View()
	h.m.View()

	col := h.m.scrollbarCol()
	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: col, Y: 0})
	top := h.m.currentTop()

	h.m.Update(tea.MouseMotionMsg{X: col, Y: h.m.viewportRows / 2})
	if h.m.currentTop() <= top {
		t.Fatalf("往下拖应当让视口下移：%d → %d", top, h.m.currentTop())
	}

	h.m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft})
	if h.m.barDragging {
		t.Fatal("松手应当结束拖动")
	}
}

// 点滚动条不该在正文里划出选区：它虽然在正文栏的最右一列，但不是内容。
func TestScrollbarClickDoesNotStartSelection(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 100, 20
	h.m.resizeInput()
	for i := range 100 {
		h.m.print(fmt.Sprintf("第 %d 行", i))
	}
	h.m.View()
	h.m.View()

	col := h.m.scrollbarCol()
	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: col, Y: 3})
	if h.m.sel.active {
		t.Fatalf("点滚动条不该开始选区：%+v", h.m.sel)
	}
}

// 左栏第 1 行固定是「+ New session」——新建会话的可见入口（对齐 opencode）。
// 空列表时也要有：那正是新用户第一次看到界面时的状态。
func TestSessionPaneShowsNewSessionRow(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	if c := ansi.Strip(h.m.View().Content); !strings.Contains(c, "+ New session") {
		t.Fatalf("左栏应显示 + New session 行：\n%s", c)
	}
}

// 点「+ New session」行（y=1）：触发 ResetSession，并像刚启动一样
// 把装配横幅重新打出来——而不是 /new 那种一屏空白。
func TestClickNewSessionRow(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	called := false
	h.m.opts.ResetSession = func() error { called = true; return nil }
	h.m.opts.StartupNotice = "arkperf · deepseek-flash · 43 工具\n新会话。上下文跨轮保留。"

	h.m.print("旧内容") // 先让屏幕上有东西，验证点击后会清掉

	h.m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 5, Y: 1})

	if !called {
		t.Fatal("点击 + New session 应触发 ResetSession")
	}
	c := ansi.Strip(h.m.View().Content)
	if !strings.Contains(c, "deepseek-flash") || !strings.Contains(c, "新会话。上下文跨轮保留。") {
		t.Fatalf("点击后应重打启动横幅：\n%s", c)
	}
	if strings.Contains(c, "旧内容") {
		t.Fatal("点击后应清掉旧转录")
	}
}

// /rename：重命名列表里 Current 的会话，并刷新左栏。
func TestRenameCommandRenamesCurrent(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	var gotID, gotTitle string
	h.m.opts.RenameSession = func(id, title string) error {
		gotID, gotTitle = id, title
		return nil
	}

	h.submit("/rename 冷启动基线")

	if gotID != "s1" || gotTitle != "冷启动基线" {
		t.Fatalf("应重命名当前会话 s1，实得 %q / %q", gotID, gotTitle)
	}
}

// /rename 空参：清除自定义名（恢复自动标题）。
func TestRenameCommandEmptyArgClearsTitle(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	var gotTitle string
	h.m.opts.RenameSession = func(id, title string) error {
		gotTitle = title
		return nil
	}

	h.submit("/rename ")

	if gotTitle != "" {
		t.Fatalf("空参应传空串恢复自动标题，实得 %q", gotTitle)
	}
}

// 当前会话不在列表里（新会话未落盘）：rename/delete 都应给出提示而不是空转。
func TestSessionCommandsSkipUnlistedSession(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)
	h.m.opts.Sessions = func() []SessionBrief { return nil }
	h.m.refreshSessions() // 让 cache 跟上（模拟"列表已经空了"的运行时状态）
	// 注入空实现：让它走到「会话不在列表」的分支，而不是「未注入」的分支
	h.m.opts.RenameSession = func(id, title string) error { return nil }
	h.m.opts.DeleteSession = func(id string) error { return nil }

	h.submit("/rename 新名字")
	if out := h.allTranscript(); !strings.Contains(out, "还没有内容") {
		t.Fatalf("rename 应提示会话还没有内容：\n%s", out)
	}

	h.submit("/delete")
	if out := h.allTranscript(); !strings.Contains(out, "+ New session") {
		t.Fatalf("delete 应提示用 + New session 开新的：\n%s", out)
	}
}

// /delete：删掉当前会话后自动切到最近的替代并回放其历史。
//
// DeleteSession 的 spy 顺带更新 Sessions——模拟装配层的真实行为：
// 删掉当前会话后 Current 自动落到最近的替代上。
func TestDeleteCommandDeletesAndSwitches(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	deleted := ""
	h.m.opts.DeleteSession = func(id string) error {
		deleted = id
		h.m.opts.Sessions = func() []SessionBrief {
			return []SessionBrief{{ID: "s2", Title: "帧率分析", Turns: 8, Current: true}}
		}
		return nil
	}

	h.submit("/delete")
	if deleted != "" {
		t.Fatal("第一次 /delete 只应提示确认，不应删除")
	}
	if out := h.allTranscript(); !strings.Contains(out, "不可恢复") {
		t.Fatalf("第一次应给出确认提示：\n%s", out)
	}

	h.submit("/delete") // 确认
	if deleted != "s1" {
		t.Fatalf("确认后应删除当前会话 s1，实得 %q", deleted)
	}
	if out := h.allTranscript(); !strings.Contains(out, "历史：s2") {
		t.Fatalf("应回放新当前会话的历史：\n%s", out)
	}
	if out := h.allTranscript(); !strings.Contains(out, "已切到「帧率分析」") {
		t.Fatalf("应提示已切到替代会话：\n%s", out)
	}
}

// /delete：删到最后一条都不剩 → 回到新会话的空白 + 横幅。
func TestDeleteCommandWhenListEmpties(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	h.m.opts.DeleteSession = func(id string) error {
		h.m.opts.Sessions = func() []SessionBrief { return nil }
		return nil
	}

	h.submit("/delete") // 第一次：只提示
	h.submit("/delete") // 确认

	if out := h.allTranscript(); !strings.Contains(out, "这是新的开始") {
		t.Fatalf("删空后应提示新的开始：\n%s", out)
	}
}

// 待确认状态下敲了别的命令或发了消息：取消待确认，
// 之后的 /delete 要重新走一遍确认——不能让"再敲一次"挂十分钟后的误删。
func TestDeleteConfirmCancelledByOtherInput(t *testing.T) {
	h := newPanesHarness(t)
	h.resize(160, 30)

	deleted := ""
	h.m.opts.DeleteSession = func(id string) error { deleted = id; return nil }

	h.submit("/delete") // 待确认
	h.submit("/help")   // 别的命令：取消
	h.submit("/delete") // 重新待确认
	if deleted != "" {
		t.Fatal("取消后的 /delete 应重新进入待确认，不应直接删")
	}

	h.submit("/delete") // 确认
	if deleted != "s1" {
		t.Fatalf("确认后应删除，实得 %q", deleted)
	}

	h.submit("/delete") // 又一次待确认
	h.submit("普通消息")    // 发消息：同样取消（会走 startTask，未注入执行器只打提示）
	h.submit("/delete") // 重新待确认
	if deleted != "s1" {
		t.Fatal("发消息取消后的 /delete 也不应直接删")
	}
}
