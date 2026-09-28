package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/fan-weibo/ArkPerf/internal/app"
	"github.com/fan-weibo/ArkPerf/internal/domain/harmony"
	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// 事件名。前后端各认这几个名字，改一边就得改另一边——所以集中在这里，
// 并由 main.go 的 init 用 RegisterEvent 注册载荷类型；不注册的话
// 生成的 TS 事件 API 只能拿到 any，前端就失去了类型保护。
const (
	evAssistant        = "arkperf:assistant"
	evToolCall         = "arkperf:toolcall"
	evToolResult       = "arkperf:toolresult"
	evApprovalRequest  = "arkperf:approval:request"
	evApprovalResolved = "arkperf:approval:resolved"
	evDone             = "arkperf:done"
)

// BootstrapInfo 是前端启动时一次性要的信息。
type BootstrapInfo struct {
	Banner    string
	Model     string
	ToolCount int
	MCP       string
	Toolchain string
	CWD       string
	Restored  bool
	Turns     int
	Updated   string
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

// ApprovalInfo 是一次待用户决定的审批。
type ApprovalInfo struct {
	ID   string
	Name string
	Args string
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
	pending map[string]chan bool
}

// NewService 构造服务。
func NewService() *Service {
	return &Service{pending: map[string]chan bool{}}
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

	info := BootstrapInfo{
		Banner:    sess.Banner(),
		Model:     sess.ModelName(),
		ToolCount: sess.Registry().Len(),
		MCP:       sess.MCPSummary(),
		Toolchain: sess.ToolchainSummary(),
		CWD:       sess.CWD(),
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
func (s *Service) AnswerApproval(id string, allow bool) error {
	s.askMu.Lock()
	ch, ok := s.pending[id]
	s.askMu.Unlock()
	if !ok {
		return fmt.Errorf("审批请求已失效：%s", id)
	}
	select {
	case ch <- allow:
	default: // 已经有答案了，忽略重复点击
	}
	return nil
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

func (a approver) Ask(ctx context.Context, name string, args map[string]any) (bool, error) {
	return a.svc.ask(ctx, name, args)
}

// ask 把审批推给前端并**阻塞等待**回答。
//
// 阻塞是安全的：任务跑在独立 goroutine 里，等的是用户点按钮，不是界面线程。
func (s *Service) ask(ctx context.Context, name string, args map[string]any) (bool, error) {
	id := s.nextAskID()
	ch := make(chan bool, 1)

	s.askMu.Lock()
	s.pending[id] = ch
	s.askMu.Unlock()
	defer func() {
		s.askMu.Lock()
		delete(s.pending, id)
		s.askMu.Unlock()
	}()

	s.emit(evApprovalRequest, ApprovalInfo{ID: id, Name: name, Args: compactJSON(args)})

	select {
	case allow := <-ch:
		return allow, nil
	case <-ctx.Done():
		// 用户按了中断：当作拒绝，让循环走正常的收尾路径
		return false, ctx.Err()
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
	// SameDir 表示该会话与当前工作目录同处——只有这种允许切换，
	// 切到别的项目却留着当前 CWD，"看到的历史"和"实际操作的目录"会错开。
	SameDir bool
}

// ListSessions 返回会话列表（最近更新在前），当前会话打标。
// 空会话不上列表：切换过去等于没切换。
func (s *Service) ListSessions() ([]SessionInfo, error) {
	sess, err := s.ensureSession()
	if err != nil {
		return nil, err
	}
	cur := sess.CurrentID()
	cwd := sess.CWD()
	all := sess.Sessions()
	out := make([]SessionInfo, 0, len(all))
	for _, x := range all {
		if len(x.Messages) == 0 {
			continue
		}
		out = append(out, SessionInfo{
			ID:      x.ID,
			Title:   sessionTitle(x),
			CWD:     x.CWD,
			Updated: x.Updated.Format("01-02 15:04"),
			Turns:   x.Turns(),
			Current: x.ID == cur,
			SameDir: sameDirLoose(x.CWD, cwd),
		})
	}
	return out, nil
}

// SwitchSession 切换到指定会话。
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
	if err := sess.LoadByID(id); err != nil {
		return nil, err
	}
	msgs := sess.Transcript()
	out := make([]MsgLine, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "user" || m.Role == "assistant" {
			out = append(out, MsgLine{Role: m.Role, Content: m.Content})
		}
	}
	return out, nil
}

// MsgLine 是回放用的转录行。
type MsgLine struct {
	Role    string
	Content string
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

// sessionTitle 取会话的标题：第一条用户消息的首行（截 40 字）。
// 会话没有名字字段，"首条提问"就是它最好的名字——比 ID 和时间好认得多。
func sessionTitle(sess *kernel.Session) string {
	for _, m := range sess.Messages {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			t := strings.SplitN(strings.TrimSpace(m.Content), "\n", 2)[0]
			if r := []rune(t); len(r) > 40 {
				t = string(r[:40]) + "…"
			}
			return t
		}
	}
	return "（空会话）"
}
