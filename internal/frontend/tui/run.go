// Package tui 是 ArkPerf 的终端交互前端（Bubble Tea v2）。
//
// 布局采用 alt-screen 整屏：转录在上方视口（PageUp/PageDown 回看），
// 底部是活区（运行指示 + 输入框 + 状态行），永远钉在屏幕底边。
// 这样"输入框沉底、agent 名字常驻左上"成立；鼠标划选仍交给终端，
// 所以复制不受影响。
//
// 所有外部依赖都从 Options 注入：TUI 自己不去读配置、不去连 MCP、
// 不去探测工具链。这样它既容易测（塞假执行器即可），也不跟内核耦合。
package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// Options 是启动 TUI 的全部输入。
type Options struct {
	// CWD 是当前工作目录（展示用）。
	CWD string
	// ModelName 是当前模型名（展示用）。
	ModelName string
	// Registry 供 /tools 与工具计数使用。
	Registry *kernel.Registry
	// ToolCount 是注册表里的工具总数。
	ToolCount int
	// MCP 是 MCP 装配摘要，如 "MCP 5/5 · 13 工具"。
	MCP string
	// Toolchain 是工具链摘要。
	Toolchain string
	// AutoApprove 对应 --yes。
	AutoApprove bool

	// NewAgent 用给定的审批器与事件回调构造任务执行器。
	// 由调用方注入，TUI 因此不必知道 Runner 与内核细节。
	NewAgent func(ap kernel.Approver, ev kernel.LoopEvents) RunFunc
	// ToolchainReport / DeviceReport 分别服务 /check 与 /devices。
	ToolchainReport func(ctx context.Context) (string, error)
	DeviceReport    func(ctx context.Context) (string, error)
	// SkillReport 服务 /skills：三层技能目录、每个技能的来源路径、被跳过的坏文件。
	//
	// 与上面两个同样是注入的闭包：TUI 既不知道 app、也不 import skill，
	// 它只需要一段能直接打印的文本。用闭包而不是固定值，是因为工作目录
	// 会随 /cd 变化，而项目级技能就挂在 <工作目录>/.arkperf/skills 下。
	SkillReport func() string
	// SkillSummary 是一行技能摘要（如 "技能 3 个 · 内置 3"），供状态行与 /status。
	SkillSummary func() string
	// RulesReport 服务 /rules：列出已保存的审批规则。
	//
	// 规则只会**减少询问**，一条过宽的规则是个静默的陷阱——用户得有个地方
	// 看见自己到底放行了什么。同样是注入的闭包，TUI 不碰 kernel 的规则文件。
	RulesReport func() string

	// SessionTurns / SessionRetained 用于在状态行展示会话规模。
	// 两者都为空表示单轮模式。
	SessionTurns    func() int
	SessionRetained func() int
	// SessionCompacted 返回已被压缩的工具输出条数。
	SessionCompacted func() int
	// ResetSession 开始新会话（/new）：清空上下文并新建持久化记录。
	ResetSession func() error
	// SwitchWorkspace 切换工作目录（工作区），返回切换后的目录（/cd）。
	//
	// 由调用方注入：TUI 自己不去读配置、不碰 app.Session。
	// 切换后该目录的最近会话要一并接上——那是装配层的事，TUI 只负责说清楚。
	SwitchWorkspace func(path string) (string, error)
	// StartupNotice 在进入界面时先打进转录（例如"已恢复上次会话"）。
	StartupNotice string
}

// Run 启动 TUI，阻塞到用户退出。
func Run(ctx context.Context, o Options) error {
	bridge := NewBridge(o.AutoApprove)
	m := NewModel(ctx, o, bridge)

	p := tea.NewProgram(m)
	// 后台线程通过 program.Send 推事件；必须用 Send 而不是直接改模型，
	// 否则就绕过了 Bubble Tea 的消息循环，会跟渲染竞争。
	bridge.SetEmit(func(msg tea.Msg) { p.Send(msg) })

	_, err := p.Run()

	// 退出前先取消在跑的任务，再关闭投递通道：
	// 反过来的话，被取消的任务会把 Done 事件发进已经停掉的 program。
	bridge.Interrupt()
	bridge.Close()
	return err
}
