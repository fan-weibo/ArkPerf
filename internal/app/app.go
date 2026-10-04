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
	"github.com/fan-weibo/ArkPerf/internal/skill"
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
	// 搜索（grep / find）：run_command 不经过 shell、没有管道，
	// 所以搜索必须有专用入口，否则搜大日志只能全读进来
	for _, t := range tools.Search() {
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
func AttachMCP(ctx context.Context, cfg *kernel.Config, reg *kernel.Registry, dir string) *mcp.Set {
	return mcp.LoadAll(ctx, reg, mcp.FromConfig(cfg), dir)
}

// SkillsFor 按工作目录解析可用技能（项目级 > 用户级 > 内置），并返回发现过程中的问题。
//
// 每次都重新解析，不做缓存：三层加起来不过十几次目录读取，而任何缓存都要
// 处理"用户切了工作区""用户刚往 skills/ 里丢了一个新技能"这些失效场景——
// 缓存失效的表现是"技能时有时无"，比多读几次目录难查得多。
//
// 诊断不在这里打印：调用方最清楚该写到哪里（CLI 写 stderr、GUI 弹提示）。
func SkillsFor(cwd string) ([]skill.Skill, []skill.Diagnostic) {
	return skill.Discover(cwd, kernel.Home(), skill.ResolveBuiltinDir())
}

// refreshSkills 按当前 cwd 重建技能集。
//
// 调用方要么在构造期（对象还没被共享），要么已经持有 s.mu——本函数自己不加锁，
// 因为它会被 SetWorkspace 在持锁状态下调用，再加一次就是死锁。
func (s *Session) refreshSkills() {
	s.skills, s.skillDiags = SkillsFor(s.cwd)
}

// WarnSkillDiagnostics 把技能发现的问题打到 stderr。
//
// 只打一次（启动时）。技能是外部输入，坏了只跳过、不阻断——但也不能不吭声：
// 用户最常见的困惑是"我明明把技能放进去了，模型还是不会用"，
// 而原因（frontmatter 少一行、名字带了大写）不打印出来就完全看不见。
//
// 放在 app 而不是各前端，是为了让措辞只有一份：同一句话在三个前端里
// 各写一遍，迟早会有一处漏掉新加的那类问题。
func WarnSkillDiagnostics(diags []skill.Diagnostic) {
	for _, d := range diags {
		fmt.Fprintf(os.Stderr, "arkperf: 技能已跳过 %s：%s\n", d.Path, d.Message)
	}
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
	// skills 是当前工作目录下的技能集，随 cwd 变化而重建（见 refreshSkills）。
	skills []skill.Skill
	// skillDiags 是上次解析技能时记下的问题，供 /skills 与启动提示展示。
	skillDiags []skill.Diagnostic

	// rules 是"以后别再问"的审批记忆。为 nil 表示不可用（文件读坏了），
	// 此时行为退化成每次都问——不会放宽任何东西。
	rules *kernel.ApprovalRules
}

// resolveWorkspace 决定这次启动用哪个工作目录。
//
// 优先级：显式指定 > 配置的 workspace > **进程 CWD**（有信息量时）
//
//	> 最近一次会话的目录 > 进程 CWD（兜底）
//
// 进程 CWD 为什么要分两种待遇：它在不同启动方式下含义完全不同。
//   - 终端里 `cd E:\proj && arkperf tui`：CWD 是**用户刚敲下的明确意图**，
//     理应压过"上次在哪个目录聊过天"。
//   - 双击 exe：CWD 只是可执行文件所在目录（Windows 的行为），没有信息量；
//     这时该退回"最近一次会话的目录"，否则双击启动会落进 bin/ 这种地方
//     （实测踩过：侧栏切会话全被拒、右栏面板报"会话未初始化"）。
//
// 这个区别早先没做，于是终端场景被桌面场景的规则劫持：用户
// `cd E:\.OpenHarmony` 启动 TUI，工作区却停在历史里的 `E:\ArkPerf-Test`
// （实测被用户当场抓到）。
//
// 每个候选都要确认目录真实存在，不存在就继续往后回退。
func resolveWorkspace(explicit, cfgWorkspace string, store *kernel.SessionStore) string {
	var recent []string
	if store != nil {
		for _, prev := range store.List() {
			if len(prev.Messages) > 0 {
				recent = append(recent, prev.CWD)
			}
		}
	}
	wd, _ := os.Getwd()
	return pickWorkspace(explicit, cfgWorkspace, recent, wd, exeDir())
}

// pickWorkspace 是 resolveWorkspace 里真正做决定的纯函数部分。
// wd / exe / recent 由调用方传入，逻辑因此可以脱离真实进程状态单测。
func pickWorkspace(explicit, cfgWorkspace string, recent []string, wd, exe string) string {
	cands := []string{explicit, cfgWorkspace}

	// 进程 CWD 等于 exe 目录 = 双击启动的副作用，没有信息量
	cwdUsable := strings.TrimSpace(wd) != "" && !samePath(wd, exe)
	if cwdUsable {
		cands = append(cands, wd)
	}
	cands = append(cands, recent...)
	if !cwdUsable && strings.TrimSpace(wd) != "" {
		cands = append(cands, wd) // 兜底：总比 "." 好
	}

	for _, c := range cands {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return "."
}

// exeDir 返回可执行文件所在目录；拿不到时返回空串（比较时视为不等）。
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// samePath 比较两个路径是否指向同一处（Windows 上大小写不敏感）。
// 空串一律判不等——"拿不到"不该被当成"相等"。
func samePath(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
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

	cwd := resolveWorkspace(o.Workspace, cfg.Workspace, store)

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

	// 审批规则：读坏了既不覆盖、也不假装没有——打出来，并按"每次都问"继续。
	// 静默当成空规则最危险：下一次 Remember 会把用户原来的规则整份覆盖掉，
	// 而他完全不知道，只会觉得"我设过的规则怎么没了"。
	if rules, rulesErr := kernel.LoadApprovalRules(kernel.ApprovalRulesPath()); rulesErr != nil {
		fmt.Fprintf(os.Stderr, "arkperf: %v（本次按「每次都问」处理）\n", rulesErr)
	} else {
		s.rules = rules
	}
	if !o.NoMCP {
		s.mcpSet = AttachMCP(ctx, cfg, s.reg, s.cwd)
		s.mcpBrief = "MCP " + s.mcpSet.Summary()
	}

	// 技能按工作目录解析——项目级技能就在工作区的 .arkperf/skills 下，
	// 所以必须在 cwd 定下来之后才解析。
	s.refreshSkills()
	WarnSkillDiagnostics(s.skillDiags)

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

// RenameSession 给会话起一个自定义名（侧栏"重命名"）。
//
// 名字存在会话文件里而不是单独的索引里：改名跟着会话走，
// 删掉会话名字自然也没了，不会留下悬空条目。
// 空串表示清除自定义名（回到"第一条提问"的自动标题）。
func (s *Session) RenameSession(id, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, err := s.store.Load(id)
	if err != nil {
		return err
	}
	sess.Rename(title)
	if err := s.store.Save(sess); err != nil {
		return err
	}
	// 改的正是当前会话：内存里这份也要跟上，否则侧栏刷新前
	// 还显示旧名字，而 Save 会把内存这份（带旧名字）写回去覆盖掉刚才的改名。
	if s.sess.ID == id {
		s.sess = sess
	}
	return nil
}

// DeleteSession 删掉一个会话（侧栏"从列表中移除"）。
//
// 只删会话记录，不碰它所在的工作目录。删的是**当前正在看的会话**时，
// 自动切到该目录下最近的一条；一条都没有了就开新会话——
// 否则界面会停在一个已经不存在的会话上，下一轮任务写不进任何文件。
func (s *Session) DeleteSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.store.Delete(id); err != nil {
		return err
	}
	if s.sess.ID != id {
		return nil
	}
	// 删的是当前会话：找同目录下最近的替代，找不到就开新的
	if next := s.store.LatestForCWD(s.cwd); next != nil {
		s.sess = next
	} else {
		s.sess = s.store.New(s.cwd)
	}
	s.conv = kernel.NewConversation()
	s.conv.Restore(s.sess.Messages)
	s.restored = s.sess
	return nil
}

// Transcript 返回当前会话的完整消息列表（切会话后回放用）。
func (s *Session) Transcript() []kernel.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conv.Messages()
}

// SetWorkspace 切换工作目录：换掉 cwd，并接上该目录下的最近一段会话（没有就新建）。
//
// 语义与启动时解析工作目录完全一致——切过去应该接着"上次在那个项目里说过的话"，
// 而不是把原来项目的上下文带过去（那会让模型拿着 A 项目的对话去改 B 项目）。
//
// 两件事必须做对：
//  1. 切走之前先把当前会话落盘，否则这段对话就丢了；
//  2. 就地 Reset + Restore 复用同一个 Conversation 对象，不换指针——
//     TUI 这类前端在启动时就持有 Conversation，换指针会让它继续写旧对象。
func (s *Session) SetWorkspace(dir string) error {
	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("工作目录不可用：%w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("不是目录：%s", abs)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 已经在这个目录里：什么都不做。否则会把当前会话重置掉，
	// 用户只是点了一下当前项而已。
	if strings.EqualFold(filepath.Clean(abs), filepath.Clean(s.cwd)) {
		return nil
	}

	s.sess.Messages = s.conv.Messages()
	if err := s.store.Save(s.sess); err != nil {
		return fmt.Errorf("切换前保存会话失败：%w", err)
	}

	s.cwd = abs
	s.conv.Reset()
	// 技能跟着工作区走：切过去该用那个工程自己的技能，而不是上一个工程的。
	s.refreshSkills()
	if prev := s.store.LatestForCWD(abs); prev != nil {
		s.sess = prev
		s.restored = prev
		s.conv.Restore(prev.Messages)
	} else {
		s.sess = s.store.New(abs)
		s.restored = nil
	}
	return nil
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
		CWD:          s.cwd, // 工具的工作目录＝当前工作区（不是进程 CWD）
		Approver:     ap,
		Events:       &ev,
		Conversation: s.conv, // 多轮会话的载体
		// 技能按次给（见下面的闭包）：工作区会变，取一次固定值会过期
		Rules: s.rules,
		// 前端自己渲染事件，Runner 的文本输出丢弃
		Out: io.Discard,
	}
	return func(ctx context.Context, task string) (kernel.LoopResult, error) {
		// CWD 与 Skills 都在每次调用时重新读：用户切了工作区，
		// 下一次任务就该用新目录下的工具基准与技能集。
		res, err := runner.Run(ctx, kernel.Task{
			Text:     task,
			MaxTurns: s.maxTurns,
			CWD:      s.cwd,
			Skills:   s.Skills(),
		})
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

// ApprovalRules 返回已加载的审批规则（文件读坏了时为 nil）。
func (s *Session) ApprovalRules() *kernel.ApprovalRules { return s.rules }

// ModelName 是当前模型名（展示用）。
func (s *Session) ModelName() string { return s.cfg.Provider.Model }

// ToolchainSummary 是工具链摘要。
func (s *Session) ToolchainSummary() string { return s.tc.Summary() }

// ToolchainCounts 返回工具链的（可用数, 总数），供界面上的状态点使用。
func (s *Session) ToolchainCounts() (int, int) {
	found, total, _ := s.tc.Counts()
	return found, total
}

// MCPSummary 是 MCP 装配摘要，如 "MCP 5/5 · 13 工具"。
func (s *Session) MCPSummary() string { return s.mcpBrief }

// Skills 返回当前工作目录下的技能集。
//
// 返回副本：调用方（前端渲染、报告）拿到之后会按自己的节奏用，
// 传出去的可变切片被改坏会影响到下一轮提示词。
func (s *Session) Skills() []skill.Skill {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]skill.Skill(nil), s.skills...)
}

// SkillDiagnostics 返回上次解析技能时记下的问题。
func (s *Session) SkillDiagnostics() []skill.Diagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]skill.Diagnostic(nil), s.skillDiags...)
}

// SkillSummary 是当前会话的技能摘要。
func (s *Session) SkillSummary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SkillSummary(s.skills)
}

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

// SkillReport 生成技能清单报告，供命令行 `arkperf skills` 与各前端共用。
//
// 与 ToolchainReport 同一个理由：命令行与界面各写一份，迟早出现
// "命令行说 3 个、界面说 2 个"这种分裂。
//
// 把**路径**打出来是刻意的：同名技能会被高优先级盖掉，用户看到路径
// 才能确认"我改的那一份到底生效了没有"，否则只能靠猜。
func SkillReport(cwd string) string {
	skills, diags := SkillsFor(cwd)

	var sb strings.Builder
	fmt.Fprintf(&sb, "技能目录（按优先级）：\n")
	for _, r := range skill.Roots(cwd, kernel.Home(), skill.ResolveBuiltinDir()) {
		fmt.Fprintf(&sb, "  [%s] %s\n", r.Source, r.Dir)
	}
	fmt.Fprintf(&sb, "\n%s\n", SkillSummary(skills))

	if len(skills) > 0 {
		sb.WriteString("\n")
		for _, s := range skills {
			fmt.Fprintf(&sb, "%-24s [%s] %s\n", s.Name, s.Source, oneLineDesc(s.Description))
			fmt.Fprintf(&sb, "%-24s        %s\n", "", s.FilePath)
		}
	}

	if len(diags) > 0 {
		sb.WriteString("\n已跳过（不影响其他技能）：\n")
		for _, d := range diags {
			fmt.Fprintf(&sb, "  %s\n    %s\n", d.Path, d.Message)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// SkillSummary 生成一行技能摘要，如 "技能 3 个 · 项目 1 用户 0 内置 2"。
//
// 同时供会话视图（Session.SkillSummary）与无会话场景（一次性执行、
// 报告）使用，避免两处各写一份而慢慢分叉。
func SkillSummary(skills []skill.Skill) string {
	if len(skills) == 0 {
		return "无技能"
	}
	var project, user, builtin int
	for _, s := range skills {
		switch s.Source {
		case skill.SourceProject:
			project++
		case skill.SourceUser:
			user++
		case skill.SourceBuiltin:
			builtin++
		}
	}
	return fmt.Sprintf("技能 %d 个 · 项目 %d 用户 %d 内置 %d", len(skills), project, user, builtin)
}

// oneLineDesc 把描述压成一行，避免把清单的行距撑乱。
func oneLineDesc(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ApprovalRulesSummary 生成审批规则的清单文本，供 TUI 的 /rules 与 `arkperf rules` 共用。
//
// 说明**怎么删**是必须的：规则只会减少询问，一条过宽的规则就是静默的陷阱。
// 现在还没有删除命令，用户得知道出路在哪个文件里。
func ApprovalRulesSummary(r *kernel.ApprovalRules) string {
	if r == nil {
		return "审批规则不可用（文件读不出来，本次按「每次都问」处理）"
	}
	rules := r.List()
	if len(rules) == 0 {
		return "没有已保存的审批规则：需要审批的工具每次都会问"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "已保存 %d 条审批规则（这些类别不再询问）：\n", len(rules))
	for _, rule := range rules {
		fmt.Fprintf(&sb, "  %-16s %s\n", rule.Tool, rule.Scope)
	}
	fmt.Fprintf(&sb, "\n要取消某一条：编辑 %s 后重启；或在审批卡上不再按 A", r.Path())
	return strings.TrimRight(sb.String(), "\n")
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
