// Package app 是应用装配层：把配置、工具、MCP、会话与转录组装成一个
// 前端无关的运行态，供 CLI、TUI、桌面端、Web 端共用。
//
// 为什么单独一层：装配逻辑（读配置 → 建注册表 → 连 MCP → 恢复会话 → 组横幅）
// 一旦写在某个前端里，第二个前端一定会复制一份，然后两处慢慢长歪——
// 本项目的 toolchainReport 就是为同样的原因抽出来的（"命令行说 5/5、
// 界面说 4/5"这种分裂比缺功能更难查）。
//
// 边界：这一层只有**数据与动作**，不含任何措辞与渲染。
// 提示语（"Ctrl+D 退出"还是"点击关闭按钮"）属于前端，留在前端。
package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fan-weibo/ArkPerf/internal/domain/harmony"
	"github.com/fan-weibo/ArkPerf/internal/kernel"
	"github.com/fan-weibo/ArkPerf/internal/mcp"
	"github.com/fan-weibo/ArkPerf/internal/tools"
)

// discoverToolchain 惰性探测一次工具链并缓存全进程复用。
//
// 这里用 Background 是刻意的：本次探测不开版本探测，因此不会启动任何子进程，
// 只是十几次 stat。若将来把 ProbeVersion 打开，必须一并把 ctx 传进来。
var discoverToolchain = sync.OnceValue(func() *harmony.Toolchain {
	return harmony.Discover(context.Background(), harmony.DiscoverOptions{})
})

// Toolchain 返回缓存的工具链探测结果（不启动子进程）。
func Toolchain() *harmony.Toolchain { return discoverToolchain() }

// NewRegistry 装配本地工具：只读文件访问 + 文件写入 + 命令执行 + 鸿蒙域。
func NewRegistry() *kernel.Registry {
	reg := kernel.NewRegistry()
	for _, t := range tools.FS() {
		reg.Register(t)
	}
	// 写入与执行是"能干活"的前提：没有它们，agent 只能出报告不能落地
	for _, t := range tools.Edit() {
		reg.Register(t)
	}
	for _, t := range tools.Exec() {
		reg.Register(t)
	}
	for _, t := range tools.Harmony(Toolchain()) {
		reg.Register(t)
	}
	return reg
}

// AttachMCP 连接配置里的 MCP 服务器，把远端工具注册进同一个注册表。
//
// 内核只认 kernel.Tool 这一个形状，所以"本地工具"和"远端工具"在下游
// 没有任何区别——审批、截断、结果回填全部走同一条路径。
func AttachMCP(ctx context.Context, cfg *kernel.Config, reg *kernel.Registry) *mcp.Set {
	return mcp.LoadAll(ctx, reg, mcp.FromConfig(cfg))
}

// Options 是打开一次交互式会话的输入。
type Options struct {
	// NoMCP 跳过 MCP 连接，只用本地工具。
	NoMCP bool
	// AutoApprove 对应 --yes：需要审批的工具直接执行。
	AutoApprove bool
	// MaxTurns 覆盖配置里的轮次上限；0 表示用配置值。
	MaxTurns int
	// Workspace 显式指定工作目录。优先级最高；为空时依次回退：
	// 配置的 workspace → 最近一次会话的目录 → 进程 CWD。
	Workspace string
}

// RunFunc 是一次任务调用的形状，与各前端自己的 RunFunc 底层类型一致
// （前端可以直接转换，不必再包一层）。
type RunFunc func(ctx context.Context, task string) (kernel.LoopResult, error)

// Session 是一个前端共用的交互式会话运行态。
type Session struct {
	cfg      *kernel.Config
	reg      *kernel.Registry
	mcpSet   *mcp.Set
	cwd      string
	tc       *harmony.Toolchain
	mcpBrief string
	auto     bool
	maxTurns int

	store *kernel.SessionStore
	// mu 保护 sess 与 conv：会话既有后台任务线程在写，也有 /new 在重置，
	// 加锁避免互相踩。
	mu   sync.Mutex
	sess *kernel.Session
	conv *kernel.Conversation
	// restored 是启动时接上的旧会话（nil 表示新建）。
	restored *kernel.Session
}

// Open 读配置、装配工具与 MCP、按工作目录接上最近一段会话。
func Open(ctx context.Context, o Options) (*Session, error) {
	cfg, err := kernel.Load(kernel.ConfigPath())
	if err != nil {
		return nil, err
	}
	// 先校验配置再拉子进程：配置本身有问题时不该先起一堆服务。
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// store 要先建：工作目录的解析要用到会话历史。
	store := kernel.NewSessionStore(kernel.Home())

	// 工作目录的优先级：显式指定 > 配置的 workspace > 最近一次会话的目录 > 进程 CWD。
	//
	// 桌面版尤其需要这条链：双击 exe 启动时进程 CWD 是 **exe 所在目录**，
	// 不解析的话"最近会话恢复"和"工作区面板"就全对不上号（实测踩过：
	// 会话都存在项目目录下，双击启动后 CWD 却变成了 exe 所在的 bin 目录，
	// 于是侧栏切会话全部被拒、右栏面板报"会话未初始化"）。
	// 每个候选都要确认目录真实存在，不存在就继续往后回退。
	cands := []string{o.Workspace, cfg.Workspace}
	for _, prev := range store.List() {
		if len(prev.Messages) > 0 {
			cands = append(cands, prev.CWD)
		}
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, wd)
	}
	cwd := "."
	for _, c := range cands {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			cwd = c
			break
		}
	}

	s := &Session{
		cfg:      cfg,
		reg:      NewRegistry(),
		cwd:      cwd,
		tc:       Toolchain(),
		mcpBrief: "MCP 未启用",
		auto:     o.AutoApprove || cfg.Approval == "auto",
		maxTurns: o.MaxTurns,
		store:    store,
		conv:     kernel.NewConversation(),
	}
	if !o.NoMCP {
		s.mcpSet = AttachMCP(ctx, cfg, s.reg)
		s.mcpBrief = "MCP " + s.mcpSet.Summary()
	}

	// 会话：按工作目录恢复最近一段，恢复不到就新建。
	// 让"重开界面接着上次说"成为默认行为，而不是需要记住命令的操作。
	if prev := s.store.LatestForCWD(cwd); prev != nil {
		s.sess = prev
		s.restored = prev
		s.conv.Restore(prev.Messages)
	} else {
		s.sess = s.store.New(cwd)
	}
	return s, nil
}

// Close 断开 MCP 服务器。可重复调用。
func (s *Session) Close() {
	if s.mcpSet != nil {
		s.mcpSet.Close()
	}
}

// Save 把当前转录落盘。
//
// 落盘失败由调用方决定怎么提示：会话保存是辅助功能，
// 不该让一次已经跑完的测量因为磁盘问题被当成失败。
func (s *Session) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sess.Messages = s.conv.Messages()
	return s.store.Save(s.sess)
}

// Reset 开始新会话（/new）：清空模型上下文并新建持久化记录。
// 屏幕上的转录不归这一层管，前端自己决定要不要清。
func (s *Session) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conv.Reset()
	s.sess = s.store.New(s.cwd)
	s.restored = nil
	return nil
}

// Sessions 返回全部已持久化会话（最近更新在前）。
// 侧栏列表用：跨目录展示，让用户看得到"在哪个项目里聊过什么"。
func (s *Session) Sessions() []*kernel.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.List()
}

// CurrentID 返回当前会话 ID（侧栏用它标记当前项）。
func (s *Session) CurrentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess.ID
}

// Transcript 返回当前会话的完整消息列表（切会话后回放用）。
func (s *Session) Transcript() []kernel.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conv.Messages()
}

// LoadByID 切换到指定会话：读盘并恢复转录。
//
// 只允许切换**同一工作目录**下的会话：CWD 决定了工具的执行上下文，
// 切到别的项目的会话却留着当前 CWD，"看到的历史"和"实际操作的目录"会错开——
// 这是比没有历史更糟的一种错（与 LatestForCWD 的过滤是同一条原则）。
// 运行中的任务不能切换：runner 还抓着旧的 Conversation，中途换会错乱。
func (s *Session) LoadByID(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.store.Load(id)
	if err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Clean(sess.CWD), filepath.Clean(s.cwd)) {
		return fmt.Errorf("该会话属于其他工作目录（%s），不能在当前目录里切换", sess.CWD)
	}
	s.sess = sess
	s.conv = kernel.NewConversation()
	s.conv.Restore(sess.Messages)
	s.restored = sess
	return nil
}

// NewRunner 用给定的审批器与事件回调构造任务执行器。
//
// 每个前端自己提供 Approver（终端问答 / 界面弹窗）与 LoopEvents（渲染进度），
// 内核只认这两个接口，所以完全不需要知道前端是什么。
func (s *Session) NewRunner(ap kernel.Approver, ev kernel.LoopEvents) RunFunc {
	runner := &kernel.Runner{
		Cfg:          s.cfg,
		Registry:     s.reg,
		Approver:     ap,
		Events:       &ev,
		Conversation: s.conv, // 多轮会话的载体
		// 前端自己渲染事件，Runner 的文本输出丢弃
		Out: io.Discard,
	}
	return func(ctx context.Context, task string) (kernel.LoopResult, error) {
		res, err := runner.Run(ctx, kernel.Task{Text: task, MaxTurns: s.maxTurns})
		if saveErr := s.Save(); saveErr != nil {
			fmt.Fprintf(os.Stderr, "arkperf: 会话保存失败: %v\n", saveErr)
		}
		return res, err
	}
}

// ---------------------------------------------------------------- 只读视图

// CWD 是当前工作目录。
func (s *Session) CWD() string { return s.cwd }

// Registry 是已装配的工具注册表。
func (s *Session) Registry() *kernel.Registry { return s.reg }

// Config 是已校验的配置。
func (s *Session) Config() *kernel.Config { return s.cfg }

// ModelName 是当前模型名（展示用）。
func (s *Session) ModelName() string { return s.cfg.Provider.Model }

// ToolchainSummary 是工具链摘要。
func (s *Session) ToolchainSummary() string { return s.tc.Summary() }

// MCPSummary 是 MCP 装配摘要，如 "MCP 5/5 · 13 工具"。
func (s *Session) MCPSummary() string { return s.mcpBrief }

// AutoApprove 表示是否需要审批的工具直接执行。
func (s *Session) AutoApprove() bool { return s.auto }

// Restored 返回启动时接上的旧会话；nil 表示本次是新建会话。
// 前端的启动提示据此措辞（"已恢复上次会话" / "新会话"）。
func (s *Session) Restored() *kernel.Session { return s.restored }

// Banner 是身份行：名字 · 模型 · 工具数 · MCP 摘要。
// 渲染（颜色、放在哪一行）由前端决定。
func (s *Session) Banner() string {
	return fmt.Sprintf("arkperf · %s · %d 工具 · %s", s.cfg.Provider.Model, s.reg.Len(), s.mcpBrief)
}

// Turns 返回会话里的用户轮数。
func (s *Session) Turns() int { return s.conv.Turns() }

// Retained 返回仍留在上下文里的轮数（超出预算会裁剪最旧的整轮）。
func (s *Session) Retained() int { return s.conv.Retained() }

// Compacted 返回已被压缩的工具输出条数。
func (s *Session) Compacted() int { return s.conv.Compacted() }

// ---------------------------------------------------------------- 报告

// ToolchainReport 生成工具链探测报告。
//
// 命令行 `arkperf check` 与各前端的 /check 共用这一份实现——
// 两处各写一份，迟早会出现"命令行说 5/5、界面说 4/5"这种分裂。
func ToolchainReport(ctx context.Context) (string, error) {
	tc := harmony.Discover(ctx, harmony.DiscoverOptions{ProbeVersion: true})

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n", tc.Summary())
	if tc.DevEcoSource != "" {
		fmt.Fprintf(&sb, "DevEco 根目录：%s（%s）\n", tc.DevEcoRoot, tc.DevEcoSource)
	}
	sb.WriteString("\n")

	for _, t := range tc.Tools {
		if t.Found() {
			fmt.Fprintf(&sb, "✓ %-8s %s   [%s]\n", t.Name, t.Path, orUnknown(t.Version))
			continue
		}
		// 未找到时把"找过哪里"一并列出：否则用户只能猜我们找没找
		fmt.Fprintf(&sb, "✗ %-8s 未找到。建议：%s\n", t.Name, t.Hint)
		for _, s := range t.Searched {
			fmt.Fprintf(&sb, "  %-8s   找过：%s\n", "", s)
		}
	}
	if tc.EmulatorDir != "" {
		fmt.Fprintf(&sb, "\n模拟器工具目录：%s\n", tc.EmulatorDir)
	}
	if tc.SDKDir != "" {
		fmt.Fprintf(&sb, "SDK 目录：%s（构建时会自动补 DEVECO_SDK_HOME）\n", tc.SDKDir)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// DeviceReport 生成设备列表报告。
//
// "没有设备"是一次成功的查询（答案是"没有"），所以返回的不是错误；
// 无法查询（hdc 缺失/失败）才返回错误。
func DeviceReport(ctx context.Context) (string, error) {
	devices, err := Toolchain().ListDevices(ctx)
	if err != nil {
		return "", err
	}
	if len(devices) == 0 {
		return strings.Join([]string{
			"没有已连接的设备。",
			"检查：模拟器是否已启动；真机是否已开启调试并授权；",
			"网络设备需先连接：hdc tconn <ip:port>（模拟器通常为 127.0.0.1:5555）。",
		}, "\n"), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "已连接 %d 个设备：\n", len(devices))
	for _, d := range devices {
		fmt.Fprintf(&sb, "- %s\n", d.Serial)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "版本未知"
	}
	return s
}
