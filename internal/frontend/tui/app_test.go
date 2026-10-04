package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// 说明：这个文件用"直接喂消息给 Update"的方式测界面行为。
// 这是唯一能在无人值守环境里验证 TUI 的办法——交互式跑一遍没法自动化，
// 而界面逻辑（状态机、审批、排队、键盘分流）恰恰是最容易出错的部分。

// collector 收集桥接器投递的消息，相当于无终端的 program.Send。
type collector struct {
	mu   sync.Mutex
	msgs []tea.Msg
}

func (c *collector) emit(msg tea.Msg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, msg)
}

func (c *collector) take() []tea.Msg {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.msgs
	c.msgs = nil
	return out
}

type harness struct {
	m    *Model
	b    *Bridge
	c    *collector
	ev   kernel.LoopEvents
	ap   kernel.Approver
	task chan string
}

// newHarness 造一个不接终端的模型：投递函数换成收集器，
// 任务执行器换成一个只记录调用的假实现。
func newHarness(t *testing.T, block bool) *harness {
	t.Helper()
	h := &harness{c: &collector{}, task: make(chan string, 8)}

	o := Options{
		CWD:       `C:\proj`,
		ModelName: "test-model",
		ToolCount: 3,
		MCP:       "MCP 5/5 · 13 工具",
		Toolchain: "5/5 个工具可用",
		NewAgent: func(ap kernel.Approver, ev kernel.LoopEvents) RunFunc {
			h.ap, h.ev = ap, ev
			return func(ctx context.Context, task string) (kernel.LoopResult, error) {
				h.task <- task
				if block {
					<-ctx.Done()
					return kernel.LoopResult{Reason: kernel.StopInterrupted}, nil
				}
				return kernel.LoopResult{Reason: kernel.StopFinal, Turns: 1, ToolUses: 0}, nil
			}
		},
	}

	h.b = NewBridge(false)
	h.b.SetEmit(h.c.emit)
	h.m = NewModel(t.Context(), o, h.b)
	h.m.width, h.m.height = 100, 30
	h.m.resizeInput()
	return h
}

// waitTask 等后台线程真的开始跑（run 在 goroutine 里，必须同步）。
func (h *harness) waitTask(t *testing.T) string {
	t.Helper()
	select {
	case task := <-h.task:
		return task
	case <-time.After(3 * time.Second):
		t.Fatal("任务一直没有开始执行")
		return ""
	}
}

func (h *harness) submit(text string) {
	h.m.input.SetValue(text)
	h.m.refreshSlash()
	h.m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
}

func (h *harness) key(code rune) {
	h.m.Update(tea.KeyPressMsg{Code: code})
}

func (h *harness) keyMod(code rune, mod tea.KeyMod) {
	h.m.Update(tea.KeyPressMsg{Code: code, Mod: mod})
}

func (h *harness) lastTranscript() string {
	if len(h.m.transcript) == 0 {
		return ""
	}
	return h.m.transcript[len(h.m.transcript)-1]
}

func (h *harness) allTranscript() string { return strings.Join(h.m.transcript, "\n") }

// ---------------------------------------------------------------- 提交任务

func TestSubmitStartsTaskAndClearsInput(t *testing.T) {
	h := newHarness(t, false)
	h.submit("测量 com.example.app 的冷启动")

	if got := h.waitTask(t); got != "测量 com.example.app 的冷启动" {
		t.Fatalf("任务内容不对：%q", got)
	}
	if h.m.st != stateRunning {
		t.Fatalf("状态应为运行中，实际 %v", h.m.st)
	}
	if h.m.input.Value() != "" {
		t.Fatalf("提交后输入框应清空，实际 %q", h.m.input.Value())
	}
	if !strings.Contains(h.allTranscript(), "测量 com.example.app") {
		t.Fatalf("转录里应有用户输入：%q", h.allTranscript())
	}
}

func TestEmptyEnterDoesNothing(t *testing.T) {
	h := newHarness(t, false)
	h.submit("   ")
	select {
	case task := <-h.task:
		t.Fatalf("空输入不该触发任务：%q", task)
	case <-time.After(100 * time.Millisecond):
	}
}

// 运行中提交要排队并明确告知，而不是静默吞掉。
func TestSubmitWhileRunningQueues(t *testing.T) {
	h := newHarness(t, true)
	h.submit("第一个任务")
	h.waitTask(t)

	h.submit("第二个任务")
	if h.m.queued != "第二个任务" {
		t.Fatalf("第二个任务应排队，实际 %q", h.m.queued)
	}
	if !strings.Contains(h.allTranscript(), "已排队") {
		t.Fatalf("排队要告诉用户：%q", h.allTranscript())
	}
}

// ---------------------------------------------------------------- 事件

func TestAssistantTextGoesToTranscript(t *testing.T) {
	h := newHarness(t, false)
	h.ev.OnAssistant("分析完成，冷启动中位数 320ms。")

	msgs := h.c.take()
	if len(msgs) != 1 {
		t.Fatalf("应投递 1 条事件，实际 %d", len(msgs))
	}
	h.m.Update(msgs[0])

	if !strings.Contains(h.allTranscript(), "冷启动中位数 320ms") {
		t.Fatalf("模型输出没进转录：%q", h.allTranscript())
	}
	if h.m.turns != 1 {
		t.Fatalf("轮次统计：%d", h.m.turns)
	}
}

// 工具调用与结果必须一次性定稿：不能让"调用有了、结果还没来"的中间态
// 永久留在转录里。
func TestToolCallCommitsTogetherWithResult(t *testing.T) {
	h := newHarness(t, false)

	h.ev.OnToolCall("harmony_build", map[string]any{"task": "assembleHap"})
	h.m.Update(h.c.take()[0])
	if len(h.m.transcript) != 0 {
		t.Fatalf("结果到达前不应定稿：%q", h.allTranscript())
	}

	h.ev.OnToolResult("harmony_build", "BUILD SUCCESSFUL in 2s", false)
	h.m.Update(h.c.take()[0])

	if len(h.m.transcript) != 1 {
		t.Fatalf("调用与结果应合成一次输出，实际 %d 条", len(h.m.transcript))
	}
	both := h.m.transcript[0]
	for _, want := range []string{"harmony_build", "BUILD SUCCESSFUL"} {
		if !strings.Contains(both, want) {
			t.Fatalf("合并输出缺少 %q：%q", want, both)
		}
	}
}

func TestToolFailureIsMarked(t *testing.T) {
	h := newHarness(t, false)
	h.ev.OnToolCall("harmony_install", nil)
	h.m.Update(h.c.take()[0])
	h.ev.OnToolResult("harmony_install", "msg:install bundle failed", true)
	h.m.Update(h.c.take()[0])

	if !strings.Contains(h.m.transcript[0], "✗") {
		t.Fatalf("失败的调用要有失败标记：%q", h.m.transcript[0])
	}
}

func TestDoneResetsToIdleAndSummarizes(t *testing.T) {
	h := newHarness(t, false)
	h.submit("任务")
	h.waitTask(t)

	h.m.Update(event{kind: evDone, result: kernel.LoopResult{Reason: kernel.StopFinal, Turns: 3, ToolUses: 2}})

	if h.m.st != stateIdle {
		t.Fatalf("结束后应回空闲，实际 %v", h.m.st)
	}
	last := h.lastTranscript()
	if !strings.Contains(last, "完成") || !strings.Contains(last, "3 轮") {
		t.Fatalf("结束摘要不对：%q", last)
	}
}

// 非正常收尾必须说清原因，不能和"做完了"混为一谈。
func TestAbnormalStopIsLabelled(t *testing.T) {
	h := newHarness(t, false)
	h.submit("任务")
	h.waitTask(t)

	h.m.Update(event{kind: evDone, result: kernel.LoopResult{Reason: kernel.StopTurnValve, Turns: 200}})

	last := h.lastTranscript()
	if !strings.Contains(last, "达到轮次上限") {
		t.Fatalf("停止原因要写清楚：%q", last)
	}
	if strings.Contains(last, "完成 ·") {
		t.Fatalf("非正常收尾不该显示为完成：%q", last)
	}
}

func TestTaskErrorIsShown(t *testing.T) {
	h := newHarness(t, false)
	h.submit("任务")
	h.waitTask(t)

	h.m.Update(event{kind: evDone, err: context.DeadlineExceeded})

	if !strings.Contains(h.lastTranscript(), "任务失败") {
		t.Fatalf("错误要显示：%q", h.lastTranscript())
	}
}

// ---------------------------------------------------------------- 审批

func TestApprovalCardAcceptsAndRejects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		key      rune
		decision kernel.ApprovalDecision
	}{
		{"按 y 批准一次", 'y', kernel.ApprovalOnce},
		{"按 Enter 批准一次", tea.KeyEnter, kernel.ApprovalOnce},
		{"按 n 拒绝", 'n', kernel.ApprovalDeny},
		{"按 Esc 拒绝", tea.KeyEscape, kernel.ApprovalDeny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			reply := make(chan kernel.ApprovalDecision, 1)

			h.m.Update(event{kind: evApproval, name: "harmony_build", args: map[string]any{"task": "assembleHap"}, reply: reply})

			if h.m.st != stateApproving {
				t.Fatalf("应进入待审批状态，实际 %v", h.m.st)
			}
			if h.m.input.Focused() {
				t.Fatal("待审批时输入框应失焦，否则按键会同时进输入框")
			}
			if card := h.m.View().Content; !strings.Contains(card, "harmony_build") {
				t.Fatalf("审批卡应显示工具名：%q", card)
			}

			h.key(tc.key)

			select {
			case got := <-reply:
				if got != tc.decision {
					t.Fatalf("审批结果 %v，期望 %v", got, tc.decision)
				}
			case <-time.After(time.Second):
				t.Fatal("审批结果没有回传给后台线程")
			}
			if h.m.st != stateRunning {
				t.Fatalf("审批后应回到运行中，实际 %v", h.m.st)
			}
			if !h.m.input.Focused() {
				t.Fatal("审批后输入框应恢复焦点")
			}
		})
	}
}

// 审批期间其他按键必须被忽略：随手敲字不能变成"放行"。
//
// 小写 a 也在被忽略之列——"总是允许"只认大写 A（Shift+a）。
// 理由：审批卡是模态的，而随手敲的一句话里出现小写 a 太常见，
// 把它绑成持久规则等于让一次手滑永久放行一类调用。
func TestApprovalIgnoresUnrelatedKeys(t *testing.T) {
	h := newHarness(t, false)
	reply := make(chan kernel.ApprovalDecision, 1)
	h.m.Update(event{kind: evApproval, name: "harmony_shell", reply: reply})

	h.key('a')
	h.key(' ')
	h.key(tea.KeyUp)

	if h.m.st != stateApproving {
		t.Fatalf("无关按键不该离开待审批状态：%v", h.m.st)
	}
	select {
	case v := <-reply:
		t.Fatalf("无关按键不该回传决定：%v", v)
	default:
	}
}

func TestApprovalWithoutReplyChannelDoesNotBlock(t *testing.T) {
	h := newHarness(t, false)
	h.m.Update(event{kind: evApproval, name: "harmony_build"})
	h.key('y') // reply 为 nil，不能 panic 也不能卡住
	if h.m.st != stateRunning {
		t.Fatalf("应回到运行中：%v", h.m.st)
	}
}

// Bridge.Ask 端到端：发事件 → 等回答。这是 TUI 与内核之间唯一的同步点。
func TestBridgeAskWaitsForReply(t *testing.T) {
	c := &collector{}
	b := NewBridge(false)
	b.SetEmit(c.emit)

	done := make(chan kernel.ApprovalDecision, 1)
	go func() {
		decision, err := b.Ask(context.Background(), "harmony_build", nil, "")
		if err != nil {
			done <- kernel.ApprovalDeny
			return
		}
		done <- decision
	}()

	var ev event
	deadline := time.Now().Add(2 * time.Second)
	for ev.reply == nil {
		if time.Now().After(deadline) {
			t.Fatal("没有收到审批请求")
		}
		msgs := c.take()
		if len(msgs) == 0 {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		e, ok := msgs[0].(event)
		if !ok {
			continue
		}
		ev = e
	}
	if ev.kind != evApproval || ev.name != "harmony_build" {
		t.Fatalf("审批事件内容不对：%+v", ev)
	}

	ev.reply <- kernel.ApprovalOnce
	select {
	case decision := <-done:
		if !decision.Granted() {
			t.Fatal("应返回放行")
		}
	case <-time.After(time.Second):
		t.Fatal("Ask 没有返回")
	}
}

func TestBridgeAutoApproveSkipsUI(t *testing.T) {
	c := &collector{}
	b := NewBridge(true)
	b.SetEmit(c.emit)

	decision, err := b.Ask(context.Background(), "harmony_build", nil, "")
	if err != nil || !decision.Granted() {
		t.Fatalf("自动批准应直接放行：decision=%v err=%v", decision, err)
	}
	if len(c.take()) != 0 {
		t.Fatal("自动批准不该产生界面事件")
	}
}

// 界面关闭后 Ask 必须立刻返回：否则后台线程会永远挂着，
// 进程退不掉。
func TestBridgeAskReturnsWhenClosed(t *testing.T) {
	c := &collector{}
	b := NewBridge(false)
	b.SetEmit(c.emit)

	done := make(chan error, 1)
	go func() {
		_, err := b.Ask(context.Background(), "harmony_build", nil, "")
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	b.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("界面关闭后 Ask 应返回错误")
		}
	case <-time.After(time.Second):
		t.Fatal("界面关闭后 Ask 卡住了")
	}
}

// ---------------------------------------------------------------- 中断

func TestEscapeInterruptsRunningTask(t *testing.T) {
	h := newHarness(t, true)
	h.submit("长任务")
	h.waitTask(t)

	h.m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	if !strings.Contains(h.allTranscript(), "已请求中断") {
		t.Fatalf("中断要有反馈：%q", h.allTranscript())
	}
	// 假执行器阻塞在 ctx.Done()：恢复说明 ctx 确实被取消了
	h.m.Update(event{kind: evDone, result: kernel.LoopResult{Reason: kernel.StopInterrupted}})
}

func TestCtrlCInterruptsWhenRunningAndQuitsWhenIdle(t *testing.T) {
	h := newHarness(t, true)
	h.submit("长任务")
	h.waitTask(t)

	_, cmd := h.m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if _, isQuit := callCmd(cmd).(tea.QuitMsg); isQuit {
		t.Fatal("运行中按 Ctrl+C 应该是中断，而不是退出")
	}

	h.m.Update(event{kind: evDone, result: kernel.LoopResult{Reason: kernel.StopInterrupted}})
	_, cmd = h.m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if _, isQuit := callCmd(cmd).(tea.QuitMsg); !isQuit {
		t.Fatal("空闲时按 Ctrl+C 应该退出")
	}
}

func TestEscapeClearsInputWhenIdle(t *testing.T) {
	h := newHarness(t, false)
	h.m.input.SetValue("写了一半")
	h.m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if h.m.input.Value() != "" {
		t.Fatalf("空闲时 Esc 应清空输入：%q", h.m.input.Value())
	}
}

// callCmd 执行一次性命令并返回它产生的消息（用于断言 Quit 之类的信号）。
func callCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

// ---------------------------------------------------------------- 斜杠命令

func TestSlashMenuFilters(t *testing.T) {
	h := newHarness(t, false)

	h.m.input.SetValue("/")
	h.m.refreshSlash()
	all := len(h.m.slashMatches)
	if all < 5 {
		t.Fatalf("空前缀应列出全部命令，实际 %d", all)
	}

	h.m.input.SetValue("/he")
	h.m.refreshSlash()
	if len(h.m.slashMatches) != 1 || h.m.slashMatches[0].Name != "help" {
		t.Fatalf("/he 应只匹配 help：%+v", h.m.slashMatches)
	}

	h.m.input.SetValue("/zzz")
	h.m.refreshSlash()
	if h.m.slashActive {
		t.Fatal("无匹配时不该弹菜单")
	}
}

func TestSlashHelpListsCommands(t *testing.T) {
	h := newHarness(t, false)
	h.submit("/help")

	out := h.allTranscript()
	for _, want := range []string{"/tools", "/check", "/devices", "/yes", "/new", "/exit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("帮助里缺少 %q：%q", want, out)
		}
	}
	// 多轮会话是这个界面的核心能力，必须写清楚
	if !strings.Contains(out, "多轮对话") {
		t.Fatalf("帮助应说明支持多轮对话：%q", out)
	}
	// /clear 与 /new 的区别是最容易混淆的一对，必须讲明白
	if !strings.Contains(out, "上下文") {
		t.Fatalf("帮助应说明 /clear 与 /new 对上下文的区别：%q", out)
	}
}

func TestSlashCommandMenuSelection(t *testing.T) {
	h := newHarness(t, false)
	h.m.input.SetValue("/")
	h.m.refreshSlash()

	// 下键移到第二项，回车执行它
	h.m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if h.m.slashIdx != 1 {
		t.Fatalf("下键应移动选中项，实际 %d", h.m.slashIdx)
	}
	selected := h.m.slashMatches[h.m.slashIdx].Name
	h.m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if h.m.slashActive {
		t.Fatal("执行命令后菜单应关闭")
	}
	if len(h.m.transcript) == 0 {
		t.Fatalf("/%s 执行后应有输出", selected)
	}
}

func TestUnknownSlashCommandIsReported(t *testing.T) {
	h := newHarness(t, false)
	h.submit("/nope")
	if !strings.Contains(h.allTranscript(), "未知命令") {
		t.Fatalf("未知命令要报错：%q", h.allTranscript())
	}
}

func TestSlashYesTogglesAutoApprove(t *testing.T) {
	h := newHarness(t, false)
	if h.b.AutoApprove() {
		t.Fatal("默认应是需审批")
	}

	h.submit("/yes")
	if !h.b.AutoApprove() {
		t.Fatal("/yes 应打开自动批准")
	}
	if !strings.Contains(h.allTranscript(), "自动批准") {
		t.Fatalf("切换要有明确反馈：%q", h.allTranscript())
	}

	h.submit("/yes")
	if h.b.AutoApprove() {
		t.Fatal("再执行一次应关闭")
	}
}

func TestSlashToolsListsRegistry(t *testing.T) {
	h := newHarness(t, false)
	h.submit("/tools")
	// 测试用的 Options 没给 Registry，应当给出可读提示而不是崩
	if !strings.Contains(h.allTranscript(), "不可用") {
		t.Fatalf("没有 Registry 时应说明：%q", h.allTranscript())
	}
}

// 需要起子进程的命令要异步执行，不能卡住界面。
func TestSlashCheckRunsAsync(t *testing.T) {
	h := newHarness(t, false)
	released := make(chan struct{})
	h.m.opts.ToolchainReport = func(context.Context) (string, error) {
		<-released
		return "5/5 个工具可用", nil
	}

	_, cmd := h.m.runSlash("check", nil)
	if cmd == nil {
		t.Fatal("/check 应返回命令")
	}

	// 直接投递报告消息，验证渲染路径
	h.m.Update(reportMsg{title: "工具链", body: "5/5 个工具可用"})
	if !strings.Contains(h.allTranscript(), "5/5 个工具可用") {
		t.Fatalf("报告没进转录：%q", h.allTranscript())
	}

	h.m.Update(reportMsg{title: "工具链", err: context.DeadlineExceeded})
	if !strings.Contains(h.lastTranscript(), "✗") {
		t.Fatalf("报告失败要有错误标记：%q", h.lastTranscript())
	}
	close(released)
}

// ---------------------------------------------------------------- 历史与视图

func TestHistoryRecallWithCtrlPN(t *testing.T) {
	h := newHarness(t, false)
	h.submit("第一条")
	h.waitTask(t)
	h.m.Update(event{kind: evDone, result: kernel.LoopResult{Reason: kernel.StopFinal}})

	h.submit("第二条")
	h.waitTask(t)
	h.m.Update(event{kind: evDone, result: kernel.LoopResult{Reason: kernel.StopFinal}})

	h.keyMod('p', tea.ModCtrl)
	if got := h.m.input.Value(); got != "第二条" {
		t.Fatalf("Ctrl+P 应取回最近一条，实际 %q", got)
	}
	h.keyMod('p', tea.ModCtrl)
	if got := h.m.input.Value(); got != "第一条" {
		t.Fatalf("再按一次应取更早一条，实际 %q", got)
	}
	h.keyMod('n', tea.ModCtrl)
	if got := h.m.input.Value(); got != "第二条" {
		t.Fatalf("Ctrl+N 应往回走，实际 %q", got)
	}
}

func TestViewShowsModeStatusAndHints(t *testing.T) {
	h := newHarness(t, false)
	content := h.m.View().Content

	// 注意这里是**小写** arkperf：界面里的它是"正在运行的 CLI 名"，
	// 不是品牌名。之前这个断言写的是 "ArkPerf"，一直靠夹具路径
	// `E:\ArkPerf` 里恰好含这几个字母才通过——夹具换成中性路径后立刻暴露。
	for _, want := range []string{"ask", "test-model", "arkperf", "/help"} {
		if !strings.Contains(content, want) {
			t.Fatalf("状态行缺少 %q：\n%s", want, content)
		}
	}

	h.b.SetAutoApprove(true)
	if got := h.m.View().Content; !strings.Contains(got, "auto") {
		t.Fatalf("自动批准模式下状态行应显示 auto：\n%s", got)
	}
}

func TestViewBeforeWindowSizeIsSafe(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 0, 0
	if got := h.m.View().Content; got == "" {
		t.Fatal("尚未收到窗口尺寸时也要有可渲染内容")
	}
}

func TestWorkingLineOnlyWhileRunning(t *testing.T) {
	h := newHarness(t, true)
	if strings.Contains(h.m.View().Content, "运行中") {
		t.Fatal("空闲时不该有运行指示")
	}

	h.submit("任务")
	h.waitTask(t)
	if !strings.Contains(h.m.View().Content, "运行中") {
		t.Fatal("运行中应显示运行指示")
	}
}

func TestClearResetsTranscript(t *testing.T) {
	h := newHarness(t, false)
	h.submit("/help")
	if len(h.m.transcript) == 0 {
		t.Fatal("前置条件：应有转录内容")
	}

	h.m.Update(resetView{})
	if len(h.m.transcript) != 0 {
		t.Fatalf("清屏后内存转录应清空，实际 %d 条", len(h.m.transcript))
	}
}

// TestViewLinesFitWithinTerminalWidth 是布局的硬约束：
// 每一行的显示宽度都不能超过终端宽度。
//
// 这类问题只能靠测量——CJK 占 2 列、ANSI 序列占 0 列，
// 肉眼看"对齐"完全不可靠；一旦某行超宽，终端就会折行，
// 整块底部区域会错位（而且只在特定宽度/特定中文内容下才复现）。
func TestViewLinesFitWithinTerminalWidth(t *testing.T) {
	for _, width := range []int{40, 60, 80, 120} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			h := newHarness(t, true)
			h.m.width, h.m.height = width, 24
			h.m.resizeInput()

			h.submit("分析 com.example.perflab 的冷启动，重点关注首帧与图片解码")
			h.waitTask(t)
			h.m.Update(event{kind: evToolCall, name: "harmony_install",
				args: map[string]any{"hap": `E:\proj\entry\build\default\outputs\default\entry-default-unsigned.hap`}})
			h.m.Update(event{kind: evToolResult, name: "harmony_install", text: "msg:install bundle successfully."})

			check := func(label, content string) {
				t.Helper()
				for i, line := range strings.Split(content, "\n") {
					if got := ansi.StringWidth(line); got > width {
						t.Fatalf("%s 第 %d 行宽 %d > 终端宽 %d：\n%s", label, i+1, got, width, ansi.Strip(line))
					}
				}
			}
			check("活区", h.m.View().Content)

			// 审批卡是唯一带边框的模态区域，最容易算错宽度
			reply := make(chan kernel.ApprovalDecision, 1)
			h.m.Update(event{kind: evApproval, name: "harmony_install",
				args: map[string]any{"hap": `E:\proj\entry\build\default\outputs\default\entry-default-unsigned.hap`}, reply: reply})
			check("审批态", h.m.View().Content)

			// 转录行也一样：它们是打进终端滚动区的，超宽同样会折行
			check("转录", h.allTranscript())
			reply <- kernel.ApprovalDeny
		})
	}
}

// 长时间运行不能让内存副本无限增长。
func TestTranscriptIsCapped(t *testing.T) {
	h := newHarness(t, false)
	for range transcriptCap + 50 {
		h.m.print("x")
	}
	if len(h.m.transcript) != transcriptCap {
		t.Fatalf("转录副本应有上限 %d，实际 %d", transcriptCap, len(h.m.transcript))
	}
}

// 整屏布局的三条硬约束：
//  1. 帧的总行数 == 终端高度——多一行少一行，alt-screen 都会渲染错位
//  2. 最后一行是状态行（底部区域钉在底边，这就是"输入框沉底"）
//  3. 有启动横幅时，第一个非空行是 ◆ 开头的身份行（名字在左上角）
func TestFullScreenLayoutInvariants(t *testing.T) {
	h := newHarness(t, true)
	h.m.width, h.m.height = 80, 24
	h.m.resizeInput()
	h.m.opts.StartupNotice = "arkperf · test-model · 3 工具 · MCP 5/5 · 13 工具\n新会话。上下文跨轮保留；/help 看命令与按键。"
	h.m.Init()

	h.submit("分析任务")
	h.waitTask(t)
	h.m.Update(event{kind: evDone, result: kernel.LoopResult{Reason: kernel.StopFinal}})

	lines := strings.Split(h.m.View().Content, "\n")
	if len(lines) != 24 {
		t.Fatalf("帧行数 %d，应为终端高度 24", len(lines))
	}

	last := ansi.Strip(lines[len(lines)-1])
	if !strings.Contains(last, "/help") {
		t.Fatalf("最后一行应是状态行（钉在底边）：%q", last)
	}
	secondLast := ansi.Strip(lines[len(lines)-2])
	if !strings.Contains(secondLast, "ask") {
		t.Fatalf("倒数第二行应是状态行首行：%q", secondLast)
	}

	// 第一个非空行是 ◆ 身份行
	for _, l := range lines {
		if strings.TrimSpace(ansi.Strip(l)) == "" {
			continue
		}
		if !strings.Contains(ansi.Strip(l), "◆ arkperf") {
			t.Fatalf("第一行应是 ◆ 身份横幅：%q", ansi.Strip(l))
		}
		break
	}
}

// alt-screen 没有原生滚动区，回看历史必须自己做。
func TestScrollRevealsOlderTranscript(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 12
	h.m.resizeInput()

	for i := range 20 {
		h.m.print(fmt.Sprintf("第 %d 行", i))
	}

	// 默认跟随最新：最早的内容不在画面里
	if strings.Contains(h.m.View().Content, "第 0 行") {
		t.Fatal("跟随最新时不应看到最早的内容")
	}

	before := h.m.View().Content
	h.m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if h.m.View().Content == before {
		t.Fatal("PageUp 应改变可见内容")
	}

	// 翻到最顶上：不能把窗口缩没（这条曾经是 bug）
	h.m.scrollBy(-9999)
	if !strings.Contains(h.m.View().Content, "第 0 行") {
		t.Fatal("翻到顶应能看到最早的行，且窗口仍是满的")
	}
	if got := strings.Count(h.m.View().Content, "\n") + 1; got != 12 {
		t.Fatalf("翻到顶时帧行数 %d，应为 12", got)
	}
}

// 翻上去看历史时，新输出不该把画面拽回底部——那等于打断正在读的东西。
func TestScrollUpIsNotYankedBackByNewOutput(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 12
	h.m.resizeInput()
	for i := range 15 {
		h.m.print(fmt.Sprintf("第 %d 行", i))
	}
	h.m.View() // 先渲染一帧：可滚动范围来自上次渲染记录的行数

	h.m.scrollBy(-9999) // 翻到最顶
	top := h.m.View().Content

	h.m.print("新的一行")
	if !strings.Contains(h.m.View().Content, "第 0 行") {
		t.Fatal("回看时新输出不该把画面拽回底部")
	}
	// 视口仍停在原处（新内容在下方，不在画面里）
	if !strings.Contains(h.m.View().Content, "第 1 行") {
		t.Fatal("应仍停留在原位置")
	}
	// 回看状态要明示，否则会以为内容丢了
	if !strings.Contains(h.m.View().Content, "回看历史中") {
		t.Fatal("回看时应给出提示")
	}
	_ = top

	h.m.ScrollToLatest()
	if !strings.Contains(h.m.View().Content, "新的一行") {
		t.Fatal("回到最新后应看到新输出")
	}
}

// 滚轮：alt-screen 里没有原生滚动区，只能自己处理。
func TestMouseWheelScrolls(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 12
	h.m.resizeInput()
	for i := range 20 {
		h.m.print(fmt.Sprintf("第 %d 行", i))
	}
	h.m.View() // 先渲染一帧，让可滚动范围生效

	bottom := h.m.currentTop()
	h.m.Update(tea.MouseWheelMsg{X: 10, Y: 3, Button: tea.MouseWheelUp})
	afterUp := h.m.currentTop()
	if afterUp >= bottom {
		t.Fatalf("滚轮上滚应让视口往上：%d → %d", bottom, afterUp)
	}

	h.m.Update(tea.MouseWheelMsg{X: 10, Y: 3, Button: tea.MouseWheelDown})
	if h.m.currentTop() <= afterUp {
		t.Fatal("滚轮下滚应让视口往下")
	}

	// 一路滚到底应恢复"跟随最新"
	h.m.scrollBy(9999)
	if !h.m.follow {
		t.Fatal("滚到底应自动恢复跟随最新")
	}
}

// 翻看历史时转录必须仍然填满视口（底部留空，而不是顶部），
// 否则启动横幅会沉到输入框上面——那是用户第一眼就能看出的错位。
// 内容从顶部开始画，和真实终端一致。
func TestTranscriptAreaAlwaysFillsViewport(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 20
	h.m.resizeInput()
	h.m.opts.StartupNotice = "arkperf · test-model\n新会话。"
	h.m.Init()
	h.m.print("之后的一行")

	lines := strings.Split(h.m.View().Content, "\n")
	if len(lines) != 20 {
		t.Fatalf("帧行数 %d，应为 20", len(lines))
	}
	// 前几行应是内容（横幅在最顶上），后面留空
	if !strings.Contains(ansi.Strip(lines[0]), "◆ arkperf") {
		t.Fatalf("横幅应在屏幕顶端：%q", ansi.Strip(lines[0]))
	}
	if !strings.Contains(ansi.Strip(lines[1]), "新会话") {
		t.Fatalf("第二行应是提示：%q", ansi.Strip(lines[1]))
	}
	if !strings.Contains(ansi.Strip(lines[2]), "之后的一行") {
		t.Fatalf("输出应紧随其后：%q", ansi.Strip(lines[2]))
	}
	// 其余全部留空（转录区共 15 行：0-2 是内容，3-14 留空；15 起是输入框）
	for i, l := range lines[3:15] {
		if strings.TrimSpace(l) != "" {
			t.Fatalf("第 %d 行应为空行（底部留空）：%q", i+3, ansi.Strip(l))
		}
	}
}

// 斜杠命令执行后必须清空输入框——否则命令文本会变成下一轮的草稿，
// 用户得手动删一遍（实测撞到过）。
func TestSlashCommandClearsInput(t *testing.T) {
	h := newHarness(t, false)

	// 路径一：从补全菜单里选中后回车
	h.m.input.SetValue("/")
	h.m.refreshSlash()
	h.m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := h.m.input.Value(); got != "" {
		t.Fatalf("菜单选中后输入框应清空，实际 %q", got)
	}

	// 路径二：直接输完整命令后回车
	h.m.input.SetValue("/help")
	h.m.refreshSlash()
	h.m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := h.m.input.Value(); got != "" {
		t.Fatalf("直接输入后输入框应清空，实际 %q", got)
	}

	// 路径三：带参数（菜单此时已关闭，走另一条分支）
	h.m.input.SetValue("/tools 参数")
	h.m.refreshSlash()
	h.m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := h.m.input.Value(); got != "" {
		t.Fatalf("带参数时输入框也应清空，实际 %q", got)
	}
}

// 转录里不该出现任何滚动指示方块。
//
// 这里曾有一条"滚动指示列"：字形直接拼在行尾，而内容行没补齐到整宽，
// 于是每行文字后面都挂着一个灰方块（用户截图吐槽"太不美观"）。已整体去掉，
// 滚动位置改由"回看历史中，下方还有 N 行"那行明示。
func TestTranscriptHasNoScrollbarArtifacts(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 12
	h.m.resizeInput()

	h.m.print("只有一行")
	if c := h.m.View().Content; strings.Contains(c, "█") || strings.Contains(c, "░") {
		t.Fatalf("内容不满屏时也不该有滚动指示：%q", c)
	}

	for i := range 30 {
		h.m.print(fmt.Sprintf("第 %d 行", i))
	}
	content := h.m.View().Content
	if strings.Contains(content, "█") || strings.Contains(content, "░") {
		t.Fatalf("可滚动时也不该再画滚动指示方块：%q", content)
	}
	// 内容行不该被右侧补齐：行尾不能挂着空白填充
	for _, l := range strings.Split(ansi.Strip(content), "\n") {
		if strings.HasSuffix(l, " ") {
			t.Fatalf("转录行不该有行尾填充：%q", l)
		}
	}
}

// 谁说的必须一眼看出来：长对话里只靠颜色分不清。
func TestTranscriptMarksWhoSaidWhat(t *testing.T) {
	// 不用阻塞型假执行器：它会一直占着"正在运行"，
	// 第二轮提交就会被当成排队而永远不启动。
	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 20
	h.m.resizeInput()

	h.submit("第一轮")
	h.waitTask(t)
	if plain := ansi.Strip(h.m.View().Content); !strings.Contains(plain, "› 第一轮") {
		t.Fatalf("用户消息应带 › 标记：%q", plain)
	}
	h.m.Update(event{kind: evDone, result: kernel.LoopResult{Reason: kernel.StopFinal}})

	h.m.Update(event{kind: evAssistant, text: "分析完成"})
	if plain := ansi.Strip(h.m.View().Content); !strings.Contains(plain, "◆ arkperf") {
		t.Fatalf("agent 回答应带 ◆ 标记：%q", plain)
	}

	// 第二轮之前空一行：没有空行的话轮次边界根本看不出来
	h.submit("第二轮")
	h.waitTask(t)
	lines := strings.Split(ansi.Strip(h.m.View().Content), "\n")
	for i, l := range lines {
		if !strings.Contains(l, "› 第二轮") {
			continue
		}
		prev := lines[i-1]
		if strings.TrimSpace(prev) != "" {
			t.Fatalf("新一轮之前应空一行，实际 %q", lines[i-1])
		}
		return
	}
	t.Fatal("应显示第二轮的用户消息")
}

// markdown 不能裸打：** 和 ` 是给终端用户看的标记，不是内容。
func TestAssistantMarkdownIsRendered(t *testing.T) {
	out := ansi.Strip(renderAssistant("**加粗** 和 `代码` 与\n\n- 项目一\n- 项目二", 60))
	if strings.Contains(out, "**") {
		t.Fatalf("不应残留字面 ** ：%q", out)
	}
	if strings.Contains(out, "`") {
		t.Fatalf("不应残留字面反引号：%q", out)
	}
	if !strings.Contains(out, "加粗") || !strings.Contains(out, "代码") {
		t.Fatalf("内容不能丢：%q", out)
	}
	if !strings.Contains(out, "•") {
		t.Fatalf("列表应渲染成项目符号：%q", out)
	}
}

// CJK 标点紧贴的加粗是 goldmark 的已知盲区（**X，**Y 不认），
// fixCJKEmphasis 必须把它救回来——中文回答几乎每句都踩。
func TestAssistantMarkdownCJKEmphasis(t *testing.T) {
	out := ansi.Strip(renderAssistant("**测试标题，**后面还有字", 60))
	if strings.Contains(out, "**") {
		t.Fatalf("CJK 标点旁的 ** 应被渲染掉而不是裸露：%q", out)
	}
	if !strings.Contains(out, "测试标题") || !strings.Contains(out, "后面还有字") {
		t.Fatalf("内容不能丢：%q", out)
	}
}

// 一轮里模型先答一段、调工具、再答一段，是常态而不是例外——
// 头部必须只打一次，否则满屏 "◆ arkperf"（用户截图吐槽过）。
func TestAssistantHeaderPrintedOncePerTurn(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 20
	h.m.resizeInput()

	h.submit("第一轮")
	h.waitTask(t)
	h.m.Update(event{kind: evDone, result: kernel.LoopResult{Reason: kernel.StopFinal}})
	h.m.Update(event{kind: evAssistant, text: "第一段"})
	h.m.Update(event{kind: evToolCall, name: "tools_list", args: map[string]any{}})
	h.m.Update(event{kind: evToolResult, name: "tools_list", text: "ok"})
	h.m.Update(event{kind: evAssistant, text: "第二段"})

	plain := ansi.Strip(h.m.View().Content)
	if got := strings.Count(plain, "◆ arkperf"); got != 1 {
		t.Fatalf("同一轮应只出现一次 ◆ arkperf，实际 %d 次：\n%s", got, plain)
	}
	if !strings.Contains(plain, "第一段") || !strings.Contains(plain, "第二段") {
		t.Fatalf("两段内容都必须在：%s", plain)
	}

	// 新的一轮：头部要重新出现
	h.m.Update(event{kind: evAssistant, text: ""}) // 空段不打、不重置
	h.submit("第二轮")
	h.waitTask(t)
	h.m.Update(event{kind: evDone, result: kernel.LoopResult{Reason: kernel.StopFinal}})
	h.m.Update(event{kind: evAssistant, text: "新轮回答"})
	plain = ansi.Strip(h.m.View().Content)
	if got := strings.Count(plain, "◆ arkperf"); got != 2 {
		t.Fatalf("新的一轮应重新出现 ◆ arkperf，实际共 %d 次", got)
	}
}

// 默认开着鼠标捕获：这样滚轮/滚动条可用，而"选中"由 app 自己实现
// （左键拖拽→反色高亮→松手自动复制），两者可以同时存在。
// 这正是 Reasonix 的做法——不必在"原生划选"和"滚轮"之间二选一。
func TestViewDefaultsToMouseCapture(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 20
	h.m.resizeInput()
	h.m.View()
	if got := h.m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("默认 MouseMode = %v，应为 MouseModeCellMotion（滚轮+内置选中）", got)
	}
}

// TestMouseCommandTogglesCapture 锁定 /mouse 在两种鼠标模式间切换，
// 且 View 立刻反映出来：开=滚轮+内置选中（CellMotion），关=交还终端（None）。
func TestMouseCommandTogglesCapture(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 20
	h.m.resizeInput()
	h.m.View()

	if h.m.mouseCaptureOff {
		t.Fatal("初始应为 mouseCaptureOff=false（开捕获）")
	}
	if got := h.m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("初始 MouseMode = %v，应为 CellMotion", got)
	}

	cmdMouse(h.m, "")
	if !h.m.mouseCaptureOff {
		t.Fatal("/mouse 后应关掉捕获")
	}
	if got := h.m.View().MouseMode; got != tea.MouseModeNone {
		t.Fatalf("关捕获后 MouseMode = %v，应为 None", got)
	}

	cmdMouse(h.m, "")
	if h.m.mouseCaptureOff {
		t.Fatal("再次 /mouse 应回到开捕获")
	}
	if got := h.m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Fatalf("再次开捕获后 MouseMode = %v，应为 CellMotion", got)
	}
}

// 选中靠"app 自己实现"才能和滚轮共存：拖拽要能选出正确的文本，
// 松手要自动写剪贴板。这里把剪贴板写入替换成收集器来验证。
// 拖拽**只选中不复制**：复制必须由用户显式按 Ctrl+C 触发，
// 免得手一抖就把剪贴板覆盖了、也没法在松手后继续调整选区。
func TestDragSelectWaitsForCtrlC(t *testing.T) {
	orig := writeClipboardText
	t.Cleanup(func() { writeClipboardText = orig })
	var got string
	writeClipboardText = func(s string) error { got = s; return nil }

	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 20
	h.m.resizeInput()
	h.m.print("第一行内容")
	h.m.print("第二行内容")
	h.m.View() // 渲染建立 contentLines/lastTop 等坐标

	// 从第 0 行第 0 列拖到第 1 行第 3 列
	h.m.Update(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	h.m.Update(tea.MouseMotionMsg{X: 3, Y: 1, Button: tea.MouseLeft})
	if !h.m.sel.active || h.m.sel.isEmpty() {
		t.Fatal("拖拽后应有非空选区")
	}

	// 松手：不复制，选区还在（还能接着按住 Ctrl+C）
	_, cmd := h.m.Update(tea.MouseReleaseMsg{X: 3, Y: 1, Button: tea.MouseLeft})
	if cmd != nil {
		if _, ok := callCmd(cmd).(copiedMsg); ok {
			t.Fatal("松手不该复制")
		}
	}
	if got != "" {
		t.Fatalf("松手就写了剪贴板：%q", got)
	}
	if !h.m.sel.active || h.m.sel.isEmpty() {
		t.Fatal("松手后选区应保留（等 Ctrl+C）")
	}

	// Ctrl+C 才复制
	_, cmd = h.m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("Ctrl+C 应触发复制")
	}
	if msg, ok := callCmd(cmd).(copiedMsg); ok {
		h.m.Update(msg)
	} else {
		t.Fatal("Ctrl+C 应返回复制命令")
	}
	if !strings.Contains(got, "第一行内容") || !strings.Contains(got, "第二") {
		t.Fatalf("复制的文本不对：%q", got)
	}
}

// 选区必须在转录区可见时才高亮：点一下就清空、不残留反色。
func TestPlainClickClearsSelection(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 20
	h.m.resizeInput()
	h.m.print("一些内容")
	h.m.View()

	h.m.Update(tea.MouseClickMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	if !h.m.sel.active {
		t.Fatal("按下后应进入选中态")
	}
	h.m.Update(tea.MouseReleaseMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	if h.m.sel.active {
		t.Fatal("没有拖动的点击应清空选区")
	}
}

// 选中后按 Ctrl+C 应该是"复制"，不是"退出"——空闲态 Ctrl+C 是退出键，
// 但只要有选区就必须让选区优先（终端通用习惯，学自 Reasonix）。
// 之前直接退出，用户选中的东西还没到手程序就关了。
func TestCtrlCCopiesSelectionInsteadOfQuitting(t *testing.T) {
	orig := writeClipboardText
	t.Cleanup(func() { writeClipboardText = orig })
	var got string
	writeClipboardText = func(s string) error { got = s; return nil }

	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 20
	h.m.resizeInput()
	h.m.print("要复制的文字")
	h.m.View()

	h.m.Update(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	h.m.Update(tea.MouseMotionMsg{X: 20, Y: 0, Button: tea.MouseLeft})
	if h.m.sel.isEmpty() {
		t.Fatal("拖拽后应有非空选区")
	}

	_, cmd := h.m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("有选区时 Ctrl+C 应触发复制")
	}
	msg := callCmd(cmd)
	if _, isQuit := msg.(tea.QuitMsg); isQuit {
		t.Fatal("有选区时 Ctrl+C 绝不能退出")
	}
	if cm, ok := msg.(copiedMsg); ok {
		h.m.Update(cm)
	} else {
		t.Fatalf("应返回复制命令，实际 %T", msg)
	}
	if !strings.Contains(got, "要复制的文字") {
		t.Fatalf("应复制选区内容，实际 %q", got)
	}
	if h.m.sel.active {
		t.Fatal("复制后应清掉选区")
	}
}

// Ctrl+Insert 是"纯复制"键：有选区就复制，没选区什么都不做
// （不能像 Ctrl+C 那样中断/退出——它没有破坏性副作用）。
func TestCtrlInsertCopiesOnly(t *testing.T) {
	orig := writeClipboardText
	t.Cleanup(func() { writeClipboardText = orig })
	writeClipboardText = func(s string) error { return nil }

	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 20
	h.m.resizeInput()
	h.m.print("文字")
	h.m.View()

	// 没选区：不退出、不复制
	_, cmd := h.m.Update(tea.KeyPressMsg{Code: tea.KeyInsert, Mod: tea.ModCtrl})
	if cmd != nil {
		if _, isQuit := callCmd(cmd).(tea.QuitMsg); isQuit {
			t.Fatal("Ctrl+Insert 不该退出")
		}
	}

	// 有选区：复制
	h.m.Update(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	h.m.Update(tea.MouseMotionMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	_, cmd = h.m.Update(tea.KeyPressMsg{Code: tea.KeyInsert, Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("有选区时 Ctrl+Insert 应触发复制")
	}
	if _, ok := callCmd(cmd).(copiedMsg); !ok {
		t.Fatal("Ctrl+Insert 应走复制路径")
	}
}

// 高亮是临时提示：用户继续敲键后不该还留在屏幕上。
func TestOtherKeyDismissesSelection(t *testing.T) {
	h := newHarness(t, false)
	h.m.width, h.m.height = 60, 20
	h.m.resizeInput()
	h.m.print("一些内容")
	h.m.View()

	h.m.Update(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	h.m.Update(tea.MouseMotionMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	if h.m.sel.isEmpty() {
		t.Fatal("拖拽后应有选区")
	}
	h.key('a')
	if h.m.sel.active {
		t.Fatal("敲其他键后应取消选区")
	}
}

// TestRenderFrameSnapshot 把完整一帧打出来供人工核对布局。
//
// 交互式界面没法在无人值守环境里"看一眼"，所以把渲染结果固定成可读文本：
//
//	go test ./internal/frontend/tui/ -run Snapshot -v
//
// 它同时能挡住 CJK 宽度算错导致的错位——那种问题单看断言很难发现。
func TestRenderFrameSnapshot(t *testing.T) {
	h := newHarness(t, true)
	h.m.width, h.m.height = 78, 24
	h.m.resizeInput()

	h.submit("分析 com.example.perflab 的冷启动")
	h.waitTask(t)

	h.m.Update(event{kind: evToolCall, name: "harmony_build", args: map[string]any{"task": "assembleHap"}})
	h.m.Update(event{kind: evToolResult, name: "harmony_build", text: "BUILD SUCCESSFUL in 2 s 467 ms"})
	h.m.Update(event{kind: evAssistant, text: "构建成功。接下来启动应用并采集冷启动耗时，\n这条中文比较长，用来验证按显示宽度折行是否正确。"})

	t.Log("\n--- 转录缓冲（View 会把它渲染进上方视口）---\n" + h.allTranscript() +
		"\n--- 整屏（View 每帧重绘，alt-screen）---\n" + h.m.View().Content)

	// 审批卡也要看一眼：它是唯一带边框的模态区域
	reply := make(chan kernel.ApprovalDecision, 1)
	h.m.Update(event{kind: evApproval, name: "harmony_install",
		args: map[string]any{"hap": `E:\proj\entry\build\default\outputs\default\entry.hap`}, reply: reply})
	t.Log("\n--- 审批态 ---\n" + h.m.View().Content)
	reply <- kernel.ApprovalDeny
}
