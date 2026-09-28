package tui

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// eventKind 区分后台任务线程发来的事件。
type eventKind int

const (
	evAssistant  eventKind = iota // 模型的文字输出
	evToolCall                    // 开始调用工具
	evToolResult                  // 工具返回
	evApproval                    // 需要用户批准（后台线程会阻塞等回答）
	evDone                        // 任务结束
)

// event 是后台任务线程推给 TUI 的消息，直接当 tea.Msg 用。
type event struct {
	kind   eventKind
	text   string
	name   string
	args   map[string]any
	isErr  bool
	result kernel.LoopResult
	err    error
	// reply 只用于审批：TUI 把用户决定写回，后台线程阻塞等它。
	reply chan bool
}

// notice 是前端自身的提示行（不来自内核）。
type notice struct{ text string }

// resetView 请求清空转录并重画底部区域。
type resetView struct{}

// RunFunc 执行一次任务。由调用方注入，因此 TUI 不必知道内核细节，
// 测试里也能塞一个假执行器。
type RunFunc func(ctx context.Context, task string) (kernel.LoopResult, error)

// Bridge 是内核与 TUI 之间唯一的桥：
//   - 把内核回调折叠成 tea.Msg 推给前端
//   - 实现 kernel.Approver，把审批请求转成一次界面交互
//   - 在后台线程里跑任务，让 UI 始终可响应
type Bridge struct {
	mu   sync.Mutex
	emit func(tea.Msg)

	auto    atomic.Bool
	running atomic.Bool

	cancelMu sync.Mutex
	cancel   context.CancelFunc

	// closed 在退出时置位：后台线程不能再往已经停掉的 program 里发消息。
	closed chan struct{}
	once   sync.Once
}

// NewBridge 构造桥。emit 稍后由 SetEmit 注入真实的 program.Send。
func NewBridge(autoApprove bool) *Bridge {
	b := &Bridge{closed: make(chan struct{})}
	b.auto.Store(autoApprove)
	return b
}

// SetEmit 注入消息投递函数（通常是 program.Send）。
func (b *Bridge) SetEmit(emit func(tea.Msg)) {
	b.mu.Lock()
	b.emit = emit
	b.mu.Unlock()
}

// Close 停止投递。退出时必须调用，否则后台线程会往已停的 program 里发消息。
func (b *Bridge) Close() {
	b.once.Do(func() { close(b.closed) })
}

// AutoApprove 返回当前是否自动批准。
func (b *Bridge) AutoApprove() bool { return b.auto.Load() }

// SetAutoApprove 切换自动批准。对应 /yes 命令。
func (b *Bridge) SetAutoApprove(v bool) { b.auto.Store(v) }

// Running 返回是否有任务在跑。
func (b *Bridge) Running() bool { return b.running.Load() }

// Interrupt 取消当前任务（Esc / Ctrl+C）。
func (b *Bridge) Interrupt() {
	b.cancelMu.Lock()
	cancel := b.cancel
	b.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Events 返回内核需要的回调集合。
func (b *Bridge) Events() kernel.LoopEvents {
	return kernel.LoopEvents{
		OnAssistant: func(text string) {
			b.Send(event{kind: evAssistant, text: text})
		},
		OnToolCall: func(name string, args map[string]any) {
			b.Send(event{kind: evToolCall, name: name, args: args})
		},
		OnToolResult: func(name, output string, isErr bool) {
			b.Send(event{kind: evToolResult, name: name, text: output, isErr: isErr})
		},
	}
}

// Ask 实现 kernel.Approver。
//
// 后台线程在这里阻塞，直到用户按下 y/n（或任务被取消、界面退出）。
// 三路 select 缺一不可：少了 ctx 分支，中断后线程会永远挂着；
// 少了 closed 分支，退出后同样挂死。
func (b *Bridge) Ask(ctx context.Context, name string, args map[string]any) (bool, error) {
	if b.auto.Load() {
		return true, nil
	}
	reply := make(chan bool, 1)
	if !b.Send(event{kind: evApproval, name: name, args: args, reply: reply}) {
		return false, errors.New("界面已关闭")
	}

	select {
	case granted := <-reply:
		return granted, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-b.closed:
		return false, errors.New("界面已关闭")
	}
}

// Send 投递一条消息。界面已关闭时返回 false 而不是永久阻塞。
func (b *Bridge) Send(msg tea.Msg) bool {
	b.mu.Lock()
	emit := b.emit
	b.mu.Unlock()
	if emit == nil {
		return false
	}

	select {
	case <-b.closed:
		return false
	default:
		emit(msg)
		return true
	}
}

// RunTask 在后台执行一次任务，结束时投递 evDone。
//
// 同时只允许一个任务：第二个任务会被明确拒绝，而不是静默排队——
// 静默排队在最坏情况下会让用户以为"没反应"。
func (b *Bridge) RunTask(ctx context.Context, run RunFunc, task string) {
	if !b.running.CompareAndSwap(false, true) {
		b.Send(notice{text: "已有任务在运行中，请等它结束或按 Esc 中断"})
		return
	}
	defer b.running.Store(false)

	ctx, cancel := context.WithCancel(ctx)
	b.cancelMu.Lock()
	b.cancel = cancel
	b.cancelMu.Unlock()
	defer func() {
		cancel()
		b.cancelMu.Lock()
		b.cancel = nil
		b.cancelMu.Unlock()
	}()

	res, err := run(ctx, task)
	b.Send(event{kind: evDone, result: res, err: err})
}
