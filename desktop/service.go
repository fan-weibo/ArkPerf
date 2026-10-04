package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/fan-weibo/ArkPerf/internal/app"
	"github.com/fan-weibo/ArkPerf/internal/domain/harmony"
	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// 事件名。前后端各认这几个名字，改一边就得改另一边——所以集中在这里，
// 并由 main.go 的 init 用 RegisterEvent 注册载荷类型；不注册的话
// 生成的 TS 事件 API 只能拿到 any，前端就失去了类型保护。
const (
	evDelta            = "arkperf:delta"
	evAssistant        = "arkperf:assistant"
	evToolCall         = "arkperf:toolcall"
	evToolResult       = "arkperf:toolresult"
	evApprovalRequest  = "arkperf:approval:request"
	evApprovalRule     = "arkperf:approval:rule"
	evApprovalResolved = "arkperf:approval:resolved"
	evWorkspaces       = "arkperf:workspaces" // 工作区树变了（重命名/删除会话后让前端重新拉）
	evDone             = "arkperf:done"
)

// BootstrapInfo 是前端启动时一次性要的信息。
type BootstrapInfo struct {
	Banner    string
	Model     string
	ToolCount int
	MCP       string
	Toolchain string
	// ToolchainFound / ToolchainTotal 供界面直接判断"工具链齐不齐"（状态点用），
	// 不必去解析 Toolchain 那句给人读的话。
	ToolchainFound int
	ToolchainTotal int
	CWD            string
	Restored       bool
	Turns          int
	Updated        string
}

// ToolInfo 是工具清单里的一项。
type ToolInfo struct {
	Name        string
	Description string
	Approval    string
}

// ToolCallInfo 是一次工具调用的开始。
type ToolCallInfo struct {
	Name string
	Args string
}

// ToolResultInfo 是一次工具调用的结果。输出是全文，界面自己决定截断。
type ToolResultInfo struct {
	Name    string
	Output  string
	IsError bool
}

// DeltaInfo 是流式输出的一个增量片段。
type DeltaInfo struct {
	// Kind 是 "content"（可见回答）或 "reasoning"（思考过程）。
	// 与 kernel.DeltaKind 的字符串形式一致。
	Kind string
	Text string
}

// ApprovalRuleInfo 说明一次审批规则的来龙去脉，措辞由前端决定。
type ApprovalRuleInfo struct {
	Name  string
	Scope string
	Hit   bool
	Saved bool
	Err   string
}

// ApprovalInfo 是一次待用户决定的审批。
type ApprovalInfo struct {
	ID   string
	Name string
	Args string
	// Scope 是这次调用的"类别"（可能为空）。界面应当把它显示出来：
	// 用户必须清楚自己放行的是多大范围，只给工具名会让人以为是整片放行。
	Scope string
}

// ApprovalResolved 是审批的最终结果，用于把界面上的按钮收掉。
type ApprovalResolved struct {
	Name    string
	Granted bool
}

// DoneInfo 是一轮任务的收尾信息。
type DoneInfo struct {
	Reason   string
	Turns    int
	ToolUses int
	Err      string
}

// Service 是暴露给前端的服务。
//
// 它是个**薄壳**：不读配置、不建注册表、不管会话，全部转发给 internal/app。
// 一旦这里长出业务逻辑，桌面端与 TUI 就会开始漂移。
//
// 注意 Wails 会把本类型**所有导出方法**暴露给前端，所以：
//   - 只放真正要给前端调的（8 个）
//   - 实现内核接口用的方法（Ask）放到不注册的 approver 里，别混进来
//   - 注入用的 SetApp 用小写（同包 main 调得到，前端看不到）
type Service struct {
	mu    sync.Mutex
	wails *application.App
	sess  *app.Session

	runMu   sync.Mutex
	running bool
	cancel  context.CancelFunc

	askMu   sync.Mutex
	askSeq  int
	pending map[string]chan kernel.ApprovalDecision
}

// NewService 构造服务。
func NewService() *Service {
	return &Service{pending: map[string]chan kernel.ApprovalDecision{}}
}

// setApp 注入 Wails 应用句柄（用于向前端推事件）。
//
// 刻意小写：这是 Go 侧的接线，不是给前端调的 API。
func (s *Service) setApp(a *application.App) { s.wails = a }

// Bootstrap 打开会话（幂等）并返回启动信息。
//
// 配置有错时**把错误如实返回前端**，而不是让进程静默退出——
// 桌面用户没有终端可看，静默失败等于"点了没反应"。
func (s *Service) Bootstrap() (BootstrapInfo, error) {
	sess, err := s.ensureSession()
	if err != nil {
		return BootstrapInfo{}, err
	}

	found, total := sess.ToolchainCounts()
	info := BootstrapInfo{
		Banner:         sess.Banner(),
		Model:          sess.ModelName(),
		ToolCount:      sess.Registry().Len(),
		MCP:            sess.MCPSummary(),
		Toolchain:      sess.ToolchainSummary(),
		ToolchainFound: found,
		ToolchainTotal: total,
		CWD:            sess.CWD(),
	}
	if prev := sess.Restored(); prev != nil {
		info.Restored = true
		info.Turns = prev.Turns()
		info.Updated = prev.Updated.Format("01-02 15:04")
	}
	return info, nil
}

// ListTools 返回工具清单。审批标注与命令行 `arkperf tools` 同一套判定，
// 避免两个前端对"这个工具要不要审批"给出不同答案。
func (s *Service) ListTools() ([]ToolInfo, error) {
	sess, err := s.ensureSession()
	if err != nil {
		return nil, err
	}

	reg := sess.Registry()
	out := make([]ToolInfo, 0, reg.Len())
	for _, name := range reg.Names() {
		tool, ok := reg.Get(name)
		if !ok {
			continue
		}
		approval := "只读"
		if cond, ok := tool.(kernel.ConditionalTool); ok {
			approval = "按需 · " + cond.ApprovalNote()
		} else if tool.NeedsApproval(map[string]any{}) {
			approval = "需审批"
		}
		out = append(out, ToolInfo{Name: name, Description: tool.Description(), Approval: approval})
	}
	return out, nil
}

// RunTask 起一轮任务，立即返回；进度通过事件推给前端。
//
// 放独立 goroutine 跑：一次任务里模型调用要几十秒，
// 同步返回会把界面卡住（TUI 那边也是 `go RunTask` 的同一思路）。
func (s *Service) RunTask(task string) error {
	sess, err := s.ensureSession()
	if err != nil {
		return err
	}
	if strings.TrimSpace(task) == "" {
		return fmt.Errorf("任务为空")
	}

	s.runMu.Lock()
	if s.running {
		s.runMu.Unlock()
		return fmt.Errorf("上一轮还在运行，请先中断或等它结束")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.running = true
	s.runMu.Unlock()

	// Approver 与 LoopEvents 都由本服务提供：内核只认这两个接口，
	// 所以它完全不需要知道"前端是终端还是浏览器"。
	run := sess.NewRunner(approver{svc: s}, kernel.LoopEvents{
		// 流式：模型的字边产边发。思考过程原样转发，
		// 前端自己决定显示成"思考中 N 字"还是忽略。
		OnDelta: func(kind kernel.DeltaKind, text string) {
			s.emit(evDelta, DeltaInfo{Kind: string(kind), Text: text})
		},
		OnAssistant: func(text string) { s.emit(evAssistant, text) },
		OnToolCall: func(name string, args map[string]any) {
			s.emit(evToolCall, ToolCallInfo{Name: name, Args: compactJSON(args)})
		},
		OnToolResult: func(name, output string, isErr bool) {
			s.emit(evToolResult, ToolResultInfo{Name: name, Output: output, IsError: isErr})
		},
		OnApproval: func(name string, granted bool) {
			s.emit(evApprovalResolved, ApprovalResolved{Name: name, Granted: granted})
		},
		OnApprovalRule: func(note kernel.ApprovalRuleNote) {
			errText := ""
			if note.Err != nil {
				errText = note.Err.Error()
			}
			s.emit(evApprovalRule, ApprovalRuleInfo{
				Name: note.Name, Scope: note.Scope, Hit: note.Hit, Saved: note.Saved, Err: errText,
			})
		},
	})

	go func() {
		defer func() {
			s.runMu.Lock()
			s.running = false
			s.cancel = nil
			s.runMu.Unlock()
			cancel()
		}()

		res, err := run(ctx, task)
		done := DoneInfo{Reason: string(res.Reason), Turns: res.Turns, ToolUses: res.ToolUses}
		if err != nil {
			done.Err = err.Error()
		}
		s.emit(evDone, done)
	}()
	return nil
}

// Interrupt 请求中断当前任务（循环在轮次边界处响应）。
func (s *Service) Interrupt() error {
	s.runMu.Lock()
	cancel := s.cancel
	s.runMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	return nil
}

// AnswerApproval 由前端回答审批。
//
// decision 取 "once" / "always" / "deny"：
//   - once  只放行这一次
//   - always 这一类以后都不再问（**只有请求里带了 scope 时才有意义**，
//     没有 scope 时会被当成 once 处理——内核那边记不住一个不存在的类别）
//   - deny  拒绝
//
// 认不出来的值一律按 deny 处理：审批的安全缺省是拒绝，不是放行。
func (s *Service) AnswerApproval(id string, decision string) error {
	s.askMu.Lock()
	ch, ok := s.pending[id]
	s.askMu.Unlock()
	if !ok {
		return fmt.Errorf("审批请求已失效：%s", id)
	}
	select {
	case ch <- parseDecision(decision):
	default: // 已经有答案了，忽略重复点击
	}
	return nil
}

// parseDecision 把前端传来的字符串决定转成内核的枚举。
func parseDecision(s string) kernel.ApprovalDecision {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "once":
		return kernel.ApprovalOnce
	case "always":
		return kernel.ApprovalAlways
	default:
		// 含空串：没选/乱传都按拒绝处理
		return kernel.ApprovalDeny
	}
}

// CheckToolchain 跑一次工具链探测（与命令行 `arkperf check` 同一实现）。
func (s *Service) CheckToolchain() (string, error) {
	return app.ToolchainReport(context.Background())
}

// ListDevices 列出已连接设备（与命令行 `arkperf devices` 同一实现）。
func (s *Service) ListDevices() (string, error) {
	return app.DeviceReport(context.Background())
}

// ResetSession 开始新会话（清空模型上下文，不动屏幕上的转录）。
func (s *Service) ResetSession() error {
	sess, err := s.ensureSession()
	if err != nil {
		return err
	}
	return sess.Reset()
}

// ---------------------------------------------------------------- 内部

// ensureSession 返回已打开的会话；还没打开就现在打开（幂等）。
//
// 面板组件和 Bootstrap 是并行挂载的：工作区面板可能比 Bootstrap 先调到后端，
// 不做这一步就会报"会话未初始化"（实测撞过）。所以每个绑定方法都走这里。
func (s *Service) ensureSession() (*app.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess == nil {
		sess, err := app.Open(context.Background(), app.Options{})
		if err != nil {
			return nil, err
		}
		s.sess = sess
	}
	return s.sess, nil
}

// approver 实现 kernel.Approver。
//
// 单独放一个**不注册为服务**的类型：Wails 只暴露注册过的服务上的方法，
// 所以 Ask（带 context、也不是给人点的）不会出现在前端 API 里。
type approver struct{ svc *Service }

func (a approver) Ask(ctx context.Context, name string, args map[string]any, scope string) (kernel.ApprovalDecision, error) {
	return a.svc.ask(ctx, name, args, scope)
}

// ask 把审批推给前端并**阻塞等待**回答。
//
// 阻塞是安全的：任务跑在独立 goroutine 里，等的是用户点按钮，不是界面线程。
func (s *Service) ask(ctx context.Context, name string, args map[string]any, scope string) (kernel.ApprovalDecision, error) {
	id := s.nextAskID()
	ch := make(chan kernel.ApprovalDecision, 1)

	s.askMu.Lock()
	s.pending[id] = ch
	s.askMu.Unlock()
	defer func() {
		s.askMu.Lock()
		delete(s.pending, id)
		s.askMu.Unlock()
	}()

	s.emit(evApprovalRequest, ApprovalInfo{ID: id, Name: name, Args: compactJSON(args), Scope: scope})

	select {
	case decision := <-ch:
		return decision, nil
	case <-ctx.Done():
		// 用户按了中断：当作拒绝，让循环走正常的收尾路径
		return kernel.ApprovalDeny, ctx.Err()
	}
}

func (s *Service) nextAskID() string {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	s.askSeq++
	return fmt.Sprintf("ask-%d", s.askSeq)
}

func (s *Service) emit(name string, data any) {
	if s.wails == nil {
		return
	}
	s.wails.Event.Emit(name, data)
}

// compactJSON 把工具参数压成一行供界面展示。
// 截断是刻意的：界面要能一眼看出"它在干什么"，不是把参数全倒出来。
func compactJSON(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	s := string(b)
	const limit = 240
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + "…"
	}
	return s
}

// ---------------------------------------------------------------- 侧栏与工作区

// SessionInfo 是侧栏会话列表里的一项。
type SessionInfo struct {
	ID      string
	Title   string // 第一条用户消息的首行（截 40 字）
	CWD     string
	Updated string
	Turns   int
	Current bool
}

// CurrentTranscript 返回当前会话的回放行。
//
// 启动时前端要它把"抢救回来的历史"立刻上屏：只提示"已恢复上次会话"、
// 屏幕却空着，用户会以为数据丢了（TUI 端实测被用户当场问到同样的问题）。
func (s *Service) CurrentTranscript() ([]MsgLine, error) {
	sess, err := s.ensureSession()
	if err != nil {
		return nil, err
	}
	return msgLines(sess.TranscriptLines()), nil
}

// SwitchSession 切换到指定会话。
//
// **会话属于哪个工作区，就跟到哪个工作区去**：侧栏的树里点一条别的项目的会话，
// 用户要的是"切过去接着看"，不是"被拒绝"——早先直接报错的版本被用户当场抓到
// （满屏"该会话属于其他工作目录"）。工作区因此不再是障碍，只是随会话自动切换的上下文。
//
// 任务运行中不能切：runner 还抓着旧的 Conversation，中途换会错乱。
// 返回会话的 user/assistant 消息列表，供前端回放历史转录
// （tool 消息是长输出，回放时跳过）。
func (s *Service) SwitchSession(id string) ([]MsgLine, error) {
	sess, err := s.ensureSession()
	if err != nil {
		return nil, err
	}
	s.runMu.Lock()
	running := s.running
	s.runMu.Unlock()
	if running {
		return nil, fmt.Errorf("任务运行中，请先中断或等它结束")
	}

	target, ok := findSession(sess, id)
	if !ok {
		return nil, fmt.Errorf("找不到会话：%s", id)
	}
	if !sameDirLoose(target.CWD, sess.CWD()) {
		if err := sess.SetWorkspace(target.CWD); err != nil {
			return nil, err
		}
	}
	if err := sess.LoadByID(id); err != nil {
		return nil, err
	}
	return msgLines(sess.TranscriptLines()), nil
}

// MsgLine 是回放用的转录行（前端绑定的形状）。
//
// Name / IsErr 只对 tool / result 两种角色有意义：
//   - tool：Name 是工具名，Content 是参数串
//   - result：Name 是工具名，IsErr 表示这次调用失败了
//
// **回放逻辑本身在 app 层**（`Session.TranscriptLines`）——TUI 与桌面端共用一份，
// 两处各写一遍必然漂移（今天刚在 samePath 上踩过一次）。这里只做形状转换。
type MsgLine struct {
	Role    string
	Content string
	Name    string
	IsErr   bool
}

// msgLines 把 app 层的回放行转成前端绑定的形状。
func msgLines(lines []app.TranscriptLine) []MsgLine {
	out := make([]MsgLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, MsgLine{Role: l.Role, Content: l.Content, Name: l.Name, IsErr: l.IsError})
	}
	return out
}

// WorkspaceChoice 是侧栏工作区树里的一项（含它自己的会话）。
//
// 树而不是"只列当前工作区"：会话是跨工作区存在的，只列当前那个会让人
// 切过去之后就"看不到别的项目、也没法切回来"（用户当场指出）。
// 列表本身不另建一套存储——会话就是工作区的记录，少一份状态就少一处不同步。
type WorkspaceChoice struct {
	Path     string
	Name     string // 显示名：目录最后一段
	Updated  string // 该工作区下最近一次会话的时间
	Sessions []SessionInfo
	Current  bool
}

// ListWorkspaces 列出**全部工作区及其会话**（当前工作区排第一，其余按最近使用倒序）。
//
// 侧栏用它渲染成一棵树：工作区 → 它自己的会话。这样切走之后别的项目仍然看得见，
// 也点得回去——这正是"只列当前工作区"那版做不到的。
// 空会话不进树（点进去等于没点），但**当前工作区永远在**，哪怕它还没有任何会话。
func (s *Service) ListWorkspaces() ([]WorkspaceChoice, error) {
	sess, err := s.ensureSession()
	if err != nil {
		return nil, err
	}

	cur := sess.CWD()
	curKey := strings.ToLower(filepath.Clean(cur))
	curID := sess.CurrentID()

	byKey := map[string]*WorkspaceChoice{}
	firstSeen := map[string]time.Time{}
	createdAt := map[string]int64{}     // 会话 ID → 创建时间（仅用于排序，不外露）
	for _, x := range sess.Sessions() { // 已按更新时间倒序
		if strings.TrimSpace(x.CWD) == "" || len(x.Messages) == 0 {
			continue
		}
		key := strings.ToLower(filepath.Clean(x.CWD))
		w, ok := byKey[key]
		if !ok {
			w = &WorkspaceChoice{Path: x.CWD, Name: dirName(x.CWD), Sessions: []SessionInfo{}}
			byKey[key] = w
			firstSeen[key] = x.Created
		} else if x.Created.Before(firstSeen[key]) {
			firstSeen[key] = x.Created // 该工作区最早那条会话的创建时间 = 它"多会儿出现的"
		}
		if w.Updated == "" {
			w.Updated = x.Updated.Format("01-02 15:04") // 该工作区最近一次活动
		}
		createdAt[x.ID] = x.Created.UnixNano()
		w.Sessions = append(w.Sessions, SessionInfo{
			ID:      x.ID,
			Title:   x.DisplayName(),
			CWD:     x.CWD,
			Updated: x.Updated.Format("01-02 15:04"),
			Turns:   x.Turns(),
			Current: x.ID == curID,
		})
	}

	// 每个工作区内部的会话按**创建时间倒序**排，且必须稳定。
	//
	// 不能按 Updated 排：Updated 会随每次 Save 变化，而"切一下工作区"就会 Save，
	// 于是同一批会话的时间戳会挤到同一分钟——再遇上不稳定的排序，
	// 每次刷新出来的次序都不一样（实测被用户当场抓到"一直在乱动"）。
	// Created 写死不变，用它排出来的次序从会话诞生那天起就不再动。
	for _, w := range byKey {
		sort.SliceStable(w.Sessions, func(i, j int) bool {
			return createdAt[w.Sessions[i].ID] > createdAt[w.Sessions[j].ID]
		})
	}

	// 稳定顺序：按"首次出现"升序（老工作区在上，新工作区追加在末尾）。
	//
	// **不能把当前工作区提到最前**——那样每切一次整个列表就重排一次，
	// 用户刚点过的条目会在他眼皮底下换位置（实测被用户当场指出）。
	// 用 Created 而不是 Updated：Updated 会随使用变化，等于换了个地方继续漂。
	order := make([]string, 0, len(byKey))
	for k := range byKey {
		order = append(order, k)
	}
	sort.Slice(order, func(i, j int) bool { return firstSeen[order[i]].Before(firstSeen[order[j]]) })

	out := make([]WorkspaceChoice, 0, len(order)+1)
	for _, k := range order {
		w := *byKey[k]
		w.Current = k == curKey
		out = append(out, w)
	}
	if _, ok := byKey[curKey]; !ok {
		// 当前工作区还没产生任何会话（刚用"打开文件夹"切过来）：
		// 追加在末尾而不是插到最前——插到最前会让它在攒出第一条会话时再跳一次位置。
		out = append(out, WorkspaceChoice{
			Path: cur, Name: dirName(cur), Current: true, Sessions: []SessionInfo{},
		})
	}
	return out, nil
}

// findSession 在全部会话里按 id 找一条（切会话时要先知道它属于哪个工作区）。
func findSession(sess *app.Session, id string) (*kernel.Session, bool) {
	for _, x := range sess.Sessions() {
		if x.ID == id {
			return x, true
		}
	}
	return nil, false
}

// SwitchWorkspace 切换工作区，并返回该目录下要回放的转录。
//
// 两条规则：
//  1. 当前**已经有对话**：接上目标目录自己的最近一段会话（"接着上次在那个项目里说"）；
//  2. 当前是**刚开的新会话（空的）**：目标目录也开新会话，**不载入它的旧对话**。
//     用户点过「新会话」就是"我要一段干净的对话"，此时把他丢进旧会话是最费解的结果
//     （实测被用户当场指出：新建会话时选工作区，结果跳进了已有会话）。
//
// 与 SwitchSession 同一条护栏：任务运行中不能切——runner 还抓着旧的
// Conversation 与工作目录，中途换会让"这次任务到底在哪操作"变得说不清。
func (s *Service) SwitchWorkspace(path string) ([]MsgLine, error) {
	sess, err := s.ensureSession()
	if err != nil {
		return nil, err
	}
	s.runMu.Lock()
	running := s.running
	s.runMu.Unlock()
	if running {
		return nil, fmt.Errorf("任务运行中，请先中断或等它结束")
	}

	// 先判断再切换：SetWorkspace 会把当前会话（可能为空）落盘并载入目标目录的会话
	fresh := len(sess.Transcript()) == 0

	if err := sess.SetWorkspace(path); err != nil {
		return nil, err
	}
	if fresh {
		// 让"空"这件事延续到新目录：Reset 会为当前目录新建一条空记录
		if err := sess.Reset(); err != nil {
			return nil, err
		}
	}
	return msgLines(sess.TranscriptLines()), nil
}

// dirName 取路径最后一段（Windows 的 \ 与 Unix 的 / 都认）。
func dirName(p string) string {
	clean := strings.TrimRight(p, `\/`)
	i := strings.LastIndexAny(clean, `\/`)
	if i < 0 {
		return clean
	}
	return clean[i+1:]
}

// WorkspaceInfo 是右侧工作区面板的数据。
type WorkspaceInfo struct {
	IsProject bool
	Root      string
	Modules   []string
	SDKDir    string
	Toolchain string
}

// Workspace 探测当前目录所在的 OpenHarmony 工程。
// 不是工程也如实返回（IsProject=false），面板自己决定显示什么。
func (s *Service) Workspace() (WorkspaceInfo, error) {
	sess, err := s.ensureSession()
	if err != nil {
		return WorkspaceInfo{}, err
	}
	cwd := sess.CWD()
	info := WorkspaceInfo{Root: cwd, Toolchain: sess.ToolchainSummary()}
	if proj, err := harmony.FindProject(cwd); err == nil && proj != nil {
		info.IsProject = true
		info.Root = proj.Root
		info.Modules = proj.Modules
		info.SDKDir = proj.SDKDir
	}
	return info, nil
}

// WsEntry 是工作区文件列表里的一项。
type WsEntry struct {
	Name string
	Dir  bool
	Size int64
}

// 这些目录是构建产物 / 第三方依赖 / IDE 元数据，列出来只会淹没真正想看的文件。
var wsJunkDirs = map[string]bool{
	"build": true, "oh_modules": true, ".hvigor": true, "node_modules": true,
	".idea": true, ".git": true, ".cxx": true, "libs": false,
}

func (s *Service) ListDir(path string) ([]WsEntry, error) {
	sess, err := s.ensureSession()
	if err != nil {
		return nil, err
	}
	clean, err := insideRoot(sess.CWD(), path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		return nil, err
	}
	out := make([]WsEntry, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() && wsJunkDirs[name] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, WsEntry{Name: name, Dir: e.IsDir(), Size: info.Size()})
		if len(out) >= 200 {
			break // 上限保护：超大目录也不至于把界面卡死
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir // 目录在前
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// ReadTextFile 读取工作区内一个文本文件（供面板预览）。
//
// 三道护栏：必须在工作区内、必须是普通文件、不超过 200KB——
// 预览不是"读任意文件"的后门。
func (s *Service) ReadTextFile(path string) (string, error) {
	sess, err := s.ensureSession()
	if err != nil {
		return "", err
	}
	clean, err := insideRoot(sess.CWD(), path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(clean)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s 是目录，不是文件", path)
	}
	const maxRead = 200 * 1024
	if info.Size() > maxRead {
		return "", fmt.Errorf("文件超过 200KB，暂不支持预览")
	}
	data, err := os.ReadFile(clean)
	if err != nil {
		return "", err
	}
	// 二进制启发：前 800 字节里出现 NUL 就当二进制，别把乱码刷进界面
	head := data
	if len(head) > 800 {
		head = head[:800]
	}
	for _, b := range head {
		if b == 0 {
			return "", fmt.Errorf("看起来是二进制文件，不支持预览")
		}
	}
	return string(data), nil
}

// insideRoot 校验 path 落在 root 之内并返回其绝对路径。
// 越界（..\..\ 之类）一律拒绝——预览功能不做"读任意文件"的后门。
func insideRoot(root, path string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rootLow := strings.ToLower(absRoot)
	absLow := strings.ToLower(abs)
	if absLow == rootLow {
		return absRoot, nil
	}
	sep := string(os.PathSeparator)
	if !strings.HasPrefix(absLow, rootLow+sep) {
		return "", fmt.Errorf("路径越出了工作区：%s", path)
	}
	return abs, nil
}

func sameDirLoose(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// OpenFolder 在系统资源管理器里打开一个目录（侧栏会话右键）。
//
// 这是**用户主动点的按钮**，不是模型发起的动作，所以不走审批——
// 审批拦的是"模型要改东西"，不是"用户自己要看一眼"。
// 仍然校验它确实是个目录：路径来自前端，别把任意字符串丢给 explorer。
func (s *Service) OpenFolder(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("路径为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("解析路径失败：%w", err)
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return fmt.Errorf("不是目录（可能已被移动或删除）：%s", abs)
	}
	// explorer 对"已经打开过"的窗口会复用，不会无限开新窗口；
	// 加 /select, 是让它定位到该目录而不是打开它的父目录。
	return exec.Command("explorer", "/select,"+abs).Start()
}

// RenameSession 给会话起名（侧栏"重命名"）。空串表示清除自定义名。
func (s *Service) RenameSession(id, title string) error {
	sess, err := s.ensureSession()
	if err != nil {
		return err
	}
	if err := sess.RenameSession(id, title); err != nil {
		return err
	}
	return s.refreshAfterSidebarChange()
}

// DeleteSession 删掉一个会话（侧栏"从列表中移除"）。只删会话记录，不碰工作目录。
func (s *Service) DeleteSession(id string) error {
	sess, err := s.ensureSession()
	if err != nil {
		return err
	}
	s.runMu.Lock()
	running := s.running
	s.runMu.Unlock()
	if running {
		return fmt.Errorf("任务运行中，请先中断或等它结束")
	}
	if err := sess.DeleteSession(id); err != nil {
		return err
	}
	return s.refreshAfterSidebarChange()
}

// refreshAfterSidebarChange 让前端重新拉一次工作区树。
//
// 列表是前端自己拉的，这里推一个事件而不是让前端在每次操作后再手动 load()——
// 忘了 load 的话，界面就会停在旧列表上（删掉的还挂着，改名的没变）。
func (s *Service) refreshAfterSidebarChange() error {
	ws, err := s.ListWorkspaces()
	if err != nil {
		return err
	}
	s.emit(evWorkspaces, ws)
	return nil
}
