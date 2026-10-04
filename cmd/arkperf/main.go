// Command arkperf —— 方舟智诊：OpenHarmony 应用性能体验分析与优化 Agent。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/fan-weibo/ArkPerf/internal/app"
	"github.com/fan-weibo/ArkPerf/internal/frontend/cli"
	"github.com/fan-weibo/ArkPerf/internal/frontend/tui"
	"github.com/fan-weibo/ArkPerf/internal/kernel"
	"github.com/fan-weibo/ArkPerf/internal/mcp"
	"github.com/fan-weibo/ArkPerf/internal/skill"
	"github.com/spf13/cobra"
)

// version 由构建注入：
//
//	go build -ldflags "-X main.version=v0.0.1" ./cmd/arkperf
var version = "dev"

// 全局旗标绑在包级变量上，而不是 newRootCmd 的局部变量。
//
// 子命令（tui）也需要读到同样的值，而根闭包里的局部变量它看不见。
// 早先在 tui 上重复定义同名局部 flag，结果是同名 flag 遮蔽：
// 两处各绑一个变量，值最终落在谁身上全看 cobra 的内部规则——
// 这种"看起来能用、边界上不可预测"的写法必须避免。
var (
	flagAutoApprove bool
	flagMaxTurns    int
	flagNoMCP       bool
)

func main() {
	// Ctrl+C 取消整个任务：循环在每个轮次边界检查 ctx，
	// 已发出的请求与 MCP 子进程都会随 ctx 一起中断。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "arkperf: "+err.Error())
		os.Exit(1)
	}
}

// 装配（配置、注册表、MCP、会话）全部在 internal/app 里，前端只管渲染。
// 早先这段逻辑直接写在各子命令里，加第二个前端时必然要复制一份——
// 那正是"命令行说 5/5、界面说 4/5"这类分裂的来源。

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "arkperf [task]",
		Short: "方舟智诊：OpenHarmony 应用性能体验分析与优化 Agent",
		Long: "arkperf 是一个面向 OpenHarmony 应用性能体验分析与优化的 Agent。" +
			"\n直接给出任务即可，例如：arkperf \"分析 com.example.app 的内存有没有泄漏\"",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			out := cmd.OutOrStdout()

			cfg, err := kernel.Load(kernel.ConfigPath())
			if err != nil {
				return err
			}
			// 先校验配置再拉子进程：配置本身有问题时不该先起一堆服务。
			if err := cfg.Validate(); err != nil {
				return err
			}

			reg := app.NewRegistry()
			reportToolchain(out)
			if !flagNoMCP {
				set := app.AttachMCP(cmd.Context(), cfg, reg, workDir())
				defer set.Close()
				reportMCP(out, set)
			}

			// 技能按工作目录解析。一次性执行没有显式工作区，
			// 就用进程 CWD——与 Runner 回退到进程 CWD 的口径一致，
			// 否则会出现"提示词里的技能来自 A 目录、工具却跑在 B 目录"。
			cwd, wdErr := os.Getwd()
			if wdErr != nil {
				cwd = "."
			}
			skills, skillDiags := app.SkillsFor(cwd)
			app.WarnSkillDiagnostics(skillDiags)
			reportSkills(out, skills)

			// 审批规则：与 TUI 那条路径同源。读坏了不覆盖、不假装没有。
			rules, rulesErr := kernel.LoadApprovalRules(kernel.ApprovalRulesPath())
			if rulesErr != nil {
				fmt.Fprintf(os.Stderr, "arkperf: %v（本次按「每次都问」处理）\n", rulesErr)
				rules = nil
			}

			runner := &kernel.Runner{
				Cfg:      cfg,
				Registry: reg,
				Approver: cli.NewApprover(os.Stdin, out, flagAutoApprove),
				Out:      out,
				Skills:   skills,
				Rules:    rules,
			}

			res, err := runner.Run(cmd.Context(), kernel.Task{
				Text:     strings.Join(args, " "),
				MaxTurns: flagMaxTurns,
			})
			if err != nil {
				return err
			}
			// 非正常收尾要显式告知：静默停下会让用户以为任务做完了。
			if res.Reason != kernel.StopFinal {
				fmt.Fprintf(out, "\n[停止：%s · %d 轮 · %d 次工具调用]\n",
					res.Reason, res.Turns, res.ToolUses)
			}
			return nil
		},
	}

	root.PersistentFlags().BoolVar(&flagAutoApprove, "yes", false, "自动批准所有需要审批的工具（危险）")
	root.PersistentFlags().IntVar(&flagMaxTurns, "max-turns", 0, "覆盖配置里的轮次上限")
	root.PersistentFlags().BoolVar(&flagNoMCP, "no-mcp", false, "不连接 MCP 服务器（只用本地工具）")

	root.AddCommand(versionCmd(), initCmd(), configCmd(), toolsCmd(), toolCmd(), skillsCmd(), rulesCmd(), mcpCmd(), checkCmd(), devicesCmd(), tuiCmd())
	root.Version = version
	return root
}

// workDir 返回进程当前目录，作为 MCP 子进程的工作目录。
//
// 一次性执行没有显式工作区，进程 CWD 就是它（与工具执行的口径一致）；
// 拿不到时返回空串，子进程将继承父进程 CWD——不为此报错。
func workDir() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

// reportToolchain 打印工具链状态。
//
// 缺什么必须说出来：静默缺失会让模型在"设备操作失败"上反复试错，
// 而根因只是本机没装 hvigorw。
func reportToolchain(out io.Writer) {
	tc := app.Toolchain()
	if tc == nil {
		return
	}
	fmt.Fprintf(out, "[toolchain] %s\n", tc.Summary())
}

// reportMCP 把装配结果如实打出来：失败的服务器必须可见，
// 否则用户会把"根本没测成"误读成"没有发现性能问题"。
func reportMCP(out io.Writer, set *mcp.Set) {
	if set == nil {
		return
	}
	fmt.Fprintf(out, "[mcp] %s\n", set.Summary())
	for _, r := range set.Failed() {
		fmt.Fprintf(out, "[mcp] ✗ %s: %v\n", r.Name, r.Err)
	}
}

// reportSkills 打印技能装载情况。
//
// 与 toolchain 同理：技能是外部输入，写坏了只跳过不报错，所以必须有一个
// 地方能让人看见"到底加载了几个、从哪加载的"。不打印的话，
// 用户唯一的观察窗口是模型的回答，而"技能没生效"在回答里看不出来。
func reportSkills(out io.Writer, skills []skill.Skill) {
	fmt.Fprintf(out, "[skill] %s\n", app.SkillSummary(skills))
	// 只有内置技能时提醒一句：用户往工程里放了技能却发现没生效，
	// 最常见的原因就是放错了目录（放在工程根而不是 .arkperf/skills 下）。
	if len(skills) > 0 && allBuiltin(skills) {
		fmt.Fprintf(out, "[skill] 以上均来自内置目录；自定义技能放 <工作区>/.arkperf/skills/<名字>/SKILL.md\n")
	}
}

// allBuiltin 判断是否全部来自内置目录。
func allBuiltin(skills []skill.Skill) bool {
	for _, s := range skills {
		if s.Source != skill.SourceBuiltin {
			return false
		}
	}
	return true
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "打印版本",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "arkperf %s\n", version)
		},
	}
}

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "初始化状态目录与配置模板",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := os.MkdirAll(kernel.Home(), 0o755); err != nil {
				return err
			}
			path := kernel.ConfigPath()
			if _, err := os.Stat(path); err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "已存在：%s\n", path)
				return nil
			}

			// 先把解析出来的两条路径打印出来：换机器/换克隆位置后连不上 MCP 时，
			// 这两行是唯一能立刻说明"它到底在找哪儿"的信息。
			// 同时把解析结果传给写配置的函数，避免打印与写盘两次解析出现分歧。
			paths := kernel.ResolveTemplatePaths()
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "MCP 目录 : %s\n", paths.MCPDir)
			fmt.Fprintf(out, "Python   : %s\n", paths.Python)
			if probs := kernel.CheckTemplatePaths(paths); len(probs) > 0 {
				fmt.Fprintln(out, "⚠ 注意：")
				for _, p := range probs {
					fmt.Fprintf(out, "  - %s\n", p)
				}
				fmt.Fprintln(out, "  （不影响写出配置；修好后用 arkperf mcp 逐个验证）")
			}

			if err := kernel.SaveDefaultFor(path, paths); err != nil {
				return err
			}
			fmt.Fprintf(out, "已创建 %s，请填入 provider.baseUrl / apiKey / model\n", path)
			return nil
		},
	}
}

func configCmd() *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "查看与校验配置"}

	c.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "打印配置（密钥脱敏）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := kernel.Load(kernel.ConfigPath())
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), cfg.String())
			return nil
		},
	})

	c.AddCommand(&cobra.Command{
		Use:   "check",
		Short: "校验必填项与字段名",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := kernel.ConfigPath()
			cfg, err := kernel.Load(path)
			if err != nil {
				return err
			}
			// 未知字段只警告不阻断：多写一个字段不该让整个工具不可用，
			// 但必须让人看见——静默忽略才是真正的坑。
			data, err := os.ReadFile(path)
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			for _, f := range kernel.UnknownFields(data) {
				fmt.Fprintf(cmd.OutOrStdout(), "⚠ 未知字段：%s（拼写错误？）\n", f)
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "✅ 配置可用")
			return nil
		},
	})

	return c
}

// skillsCmd 列出三层技能目录与各自解析出的技能。
//
// 为什么要有这个入口：技能是纯数据，加一个技能不用改代码，所以"我放进去
// 为什么没生效"是最高频的问题。这里把三层目录、每个技能的来源路径、
// 以及被跳过的坏文件连同原因一起打出来——用户自己就能定位，
// 不必反过来问我们。
func skillsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "skills",
		Short: "列出三层技能目录与解析出的技能",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				cwd = "."
			}
			fmt.Fprintln(cmd.OutOrStdout(), app.SkillReport(cwd))
			return nil
		},
	}
}

// rulesCmd 列出已保存的审批规则。
//
// 规则只减少询问，一条过宽的规则是静默的陷阱，所以要有地方能看见。
// **删除只能手改文件**：现在没有删除命令——等真的有人需要"按类别撤销"
// 再加，而不是先造一个自己也不确定语义的子命令。
func rulesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rules",
		Short: "列出已保存的审批规则（哪些类别不再询问）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := kernel.ApprovalRulesPath()
			rules, err := kernel.LoadApprovalRules(path)
			if err != nil {
				// 读坏了就把原因原样打出来：它比"没有规则"重要得多
				fmt.Fprintf(cmd.OutOrStdout(), "%v\n", err)
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), app.ApprovalRulesSummary(rules))
			return nil
		},
	}
}

func toolsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tools",
		Short: "列出本地工具（不含 MCP）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			reg := app.NewRegistry()

			for _, name := range reg.Names() {
				tool, ok := reg.Get(name)
				if !ok {
					continue
				}
				// 审批标注：取决于参数的工具标成"按需"，而不是拿空参数
				// 判定出的"免审批"——那会让人以为它只读。
				approval := "只读"
				if cond, ok := tool.(kernel.ConditionalTool); ok {
					approval = "按需 · " + cond.ApprovalNote()
				} else if tool.NeedsApproval(map[string]any{}) {
					approval = "需审批"
				}
				fmt.Fprintf(out, "%-28s [%s] %s\n", name, approval, tool.Description())
			}
			fmt.Fprintln(out, "\n（MCP 工具见 `arkperf mcp --tools`）")
			return nil
		},
	}
}

// toolCmd 直接调用一个工具。
//
// 为什么要有这个入口：工具本来是给模型调的，但"这个工具在本机到底跑不跑得通"
// 只能靠真跑一次来回答。以前要么写 Go 测试、要么在 TUI 里让模型去调
// （还得先配好模型）——排障成本高到实际上没人去做，于是"工具坏了"往往
// 直到 Agent 卡住才被发现。
//
// 审批**不绕过**：走的是与正常运行同一个 cli.Approver，所以不带 --yes 时
// 会在终端询问，非交互环境（管道 / CI）一律按拒绝处理。
func toolCmd() *cobra.Command {
	var argsJSON string

	c := &cobra.Command{
		Use:   "tool <名字> [参数JSON]",
		Short: "直接调用一个本地工具（调试用，参数以 JSON 给出）",
		Long: "不经过模型，直接执行一个工具并打印结果。用于验证工具在本机是否可用。\n\n" +
			"  arkperf tool harmony_emulator_list\n" +
			"  arkperf tool harmony_api_lookup '{\"symbol\":\"UIAbility\"}'\n" +
			"  arkperf tool harmony_emulator_start '{\"name\":\"Pura 90\"}' --yes\n\n" +
			"需要审批的工具会照常询问（非交互环境一律拒绝）；只读工具直接执行。",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			name := strings.TrimSpace(args[0])

			raw := argsJSON
			if len(args) > 1 {
				raw = args[1] // 位置参数优先于 --args，命令行上更顺手
			}
			params := map[string]any{}
			if strings.TrimSpace(raw) != "" {
				if err := json.Unmarshal([]byte(raw), &params); err != nil {
					return fmt.Errorf("参数不是合法 JSON 对象：%w\n示例：arkperf tool %s '{\"key\":\"value\"}'", err, name)
				}
			}

			reg := app.NewRegistry()
			tool, ok := reg.Get(name)
			if !ok {
				return fmt.Errorf("没有名为 %q 的本地工具。%s（用 `arkperf tools` 看全部；MCP 工具不在其中）",
					name, suggestTool(reg, name))
			}

			if tool.NeedsApproval(params) {
				// 这条路径绕开了循环、直接调工具，所以审批与"规则记忆"都得自己做一遍。
				// 关键是**用同一个类别算法**：两条路径算出不同类别的话，
				// 用户在 TUI 里记住的规则到这里就不生效，而现象是
				// "我明明设过了，它怎么还问我"。
				cwd, _ := os.Getwd()
				scope := kernel.ApprovalScope(tool, params, cwd)
				rules, rulesErr := kernel.LoadApprovalRules(kernel.ApprovalRulesPath())
				if rulesErr != nil {
					// 读坏了不覆盖、也不假装没有：说清楚，并按"每次都问"继续
					fmt.Fprintf(os.Stderr, "arkperf: %v（本次按「每次都问」处理）\n", rulesErr)
					rules = nil
				}

				if rules.Allowed(name, scope) {
					fmt.Fprintf(out, "按已保存的规则放行 %s（%s）\n", name, scope)
				} else {
					approver := cli.NewApprover(cmd.InOrStdin(), out, flagAutoApprove)
					decision, err := approver.Ask(cmd.Context(), name, params, scope)
					if err != nil {
						// 管道 / CI / 重定向时读不到回答，审批器会返回 EOF 之类的原始错误。
						// 直接把它抛出去，用户看到的是 "arkperf: EOF"——完全看不懂。
						// 必须翻译成"发生了什么 + 怎么办"。
						return fmt.Errorf("%s 需要审批，但当前不是交互式终端（%v），已按拒绝处理；"+
							"确认要执行请显式加 --yes", name, err)
					}
					if !decision.Granted() {
						return fmt.Errorf("%s 未获批准，未执行", name)
					}
					if decision == kernel.ApprovalAlways && scope != "" {
						if err := rules.Remember(name, scope); err != nil {
							// 这次已经放行了，只是没记住；必须说出来，
							// 否则用户以为已经生效，下次却被再问一遍
							fmt.Fprintf(os.Stderr, "arkperf: 规则没能保存：%v\n", err)
						} else {
							fmt.Fprintf(out, "已记住：%s 的「%s」以后不再询问\n", name, scope)
						}
					}
				}
			}

			cwd, err := os.Getwd()
			if err != nil {
				cwd = ""
			}
			res, err := tool.Execute(cmd.Context(), params, kernel.ToolCtx{CWD: cwd, Home: kernel.Home()})
			if err != nil {
				return err // 这是"工具坏了"，由上层统一打印
			}
			fmt.Fprintln(out, res.Output)
			if res.IsError {
				// 业务失败也要有非 0 退出码：否则脚本里看不出成败
				return fmt.Errorf("%s 返回错误（输出见上）", name)
			}
			return nil
		},
	}
	c.Flags().StringVar(&argsJSON, "args", "", `参数 JSON，如 '{"symbol":"UIAbility"}'`)
	return c
}

// 名字里的通用词：它们出现在几乎每个工具名里，拿来做匹配会得到一堆无用候选。
var toolNameStopWords = map[string]bool{"harmony": true, "file": true, "list": true}

// suggestTool 给拼错的工具名一个提示。
//
// 匹配用"名字片段的前 6 个字符"而不是整段相等：这样少写一个字母
// （emulater → emulator）也能命中，又不至于退化成"所有 harmony_ 工具都算候选"。
//
// 不实现编辑距离：工具名长且规律，前缀匹配已经够用，
// 而且不会给出"看起来很像其实不是"的错误建议。
func suggestTool(reg *kernel.Registry, name string) string {
	parts := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == '_' || r == '-' || r == ' ' || r == '.'
	})
	var hits []string
	for _, n := range reg.Names() {
		if n == name {
			continue
		}
		low := strings.ToLower(n)
		for _, p := range parts {
			if len(p) < 4 || toolNameStopWords[p] {
				continue
			}
			stem := p
			if len(stem) > 6 {
				stem = stem[:6]
			}
			if strings.Contains(low, stem) {
				hits = append(hits, n)
				break
			}
		}
		if len(hits) >= 3 {
			break
		}
	}
	if len(hits) == 0 {
		return ""
	}
	return "是不是想找：" + strings.Join(hits, " / ")
}

func mcpCmd() *cobra.Command {
	var showTools bool

	c := &cobra.Command{
		Use:   "mcp",
		Short: "连接 MCP 服务器并报告状态",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			cfg, err := kernel.Load(kernel.ConfigPath())
			if err != nil {
				return err
			}
			if len(cfg.MCPServers) == 0 {
				fmt.Fprintln(out, "配置里没有 mcpServers。运行 `arkperf init` 可生成默认模板。")
				return nil
			}

			reg := app.NewRegistry()
			set := app.AttachMCP(cmd.Context(), cfg, reg, workDir())
			defer set.Close()

			for _, name := range slices.Sorted(maps.Keys(cfg.MCPServers)) {
				srv := cfg.MCPServers[name]
				if !srv.IsEnabled() {
					fmt.Fprintf(out, "○ %-16s 已停用\n", name)
					continue
				}
				mark, detail := "✗", "未连接"
				for _, r := range set.Results {
					if r.Name != name {
						continue
					}
					if r.OK() {
						mark, detail = "✓", fmt.Sprintf("%d 个工具", r.Tools)
					} else {
						detail = r.Err.Error()
					}
				}
				fmt.Fprintf(out, "%s %-16s %s\n", mark, name, detail)
			}
			fmt.Fprintf(out, "\n%s\n", set.Summary())

			if showTools {
				fmt.Fprintln(out, "\n--- 投影后的工具 ---")
				for _, spec := range reg.Specs() {
					fmt.Fprintf(out, "%-42s %s\n", spec.Name, spec.Description)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&showTools, "tools", false, "同时列出投影后的工具名")
	return c
}

func checkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "探测 OpenHarmony 工具链",
		RunE: func(cmd *cobra.Command, _ []string) error {
			report, err := app.ToolchainReport(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
}

func devicesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "devices",
		Short: "列出已连接的设备",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// "没有设备"是一次成功的查询（答案是"没有"），所以退出码为 0；
			// 无法查询（hdc 缺失/失败）才是错误。
			report, err := app.DeviceReport(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
}

func tuiCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "tui",
		Short: "全屏终端界面（原生滚动区转录 + 输入框 + 状态行）",
		Long: "启动交互式终端界面：转录在上方视口，输入框钉在屏幕底边，agent 名字常驻左上。\n" +
			"PageUp/PageDown 回看历史；输入 /help 看命令与按键。",
		RunE: func(cmd *cobra.Command, args []string) error {
			sess, err := app.Open(cmd.Context(), app.Options{
				NoMCP:       flagNoMCP,
				AutoApprove: flagAutoApprove,
				MaxTurns:    flagMaxTurns,
			})
			if err != nil {
				return err
			}
			defer sess.Close()

			// 提示语是**前端的措辞**，所以留在这里而不是塞进装配层：
			// 桌面端这里该说的是"点关闭按钮"，不是"Ctrl+D 退出"。
			notice := sess.Banner()
			if prev := sess.Restored(); prev == nil {
				notice += "\n新会话。上下文跨轮保留；/help 看命令与按键，Ctrl+D 退出。"
			} else {
				notice += fmt.Sprintf("\n已恢复上次会话：%d 轮 · %s；/new 开始新会话，Ctrl+D 退出。",
					prev.Turns(), prev.Updated.Format("01-02 15:04"))
			}

			// 恢复出来的历史要在启动时回放到屏幕上：否则提示语说"已恢复了"、
			// 屏幕却空着，看起来像数据丢了。只有真恢复了才填。
			var initial []tui.ReplayLine
			if sess.Restored() != nil {
				lines := sess.TranscriptLines()
				initial = make([]tui.ReplayLine, 0, len(lines))
				for _, l := range lines {
					initial = append(initial, tui.ReplayLine{
						Role: l.Role, Content: l.Content, Name: l.Name, IsError: l.IsError,
					})
				}
			}

			return tui.Run(cmd.Context(), tui.Options{
				CWD:              sess.CWD(),
				ModelName:        sess.ModelName(),
				Registry:         sess.Registry(),
				ToolCount:        sess.Registry().Len(),
				MCP:              sess.MCPSummary(),
				Toolchain:        sess.ToolchainSummary(),
				AutoApprove:      sess.AutoApprove(),
				SessionTurns:     sess.Turns,
				SessionRetained:  sess.Retained,
				SessionCompacted: sess.Compacted,
				ResetSession:     sess.NewSession,
				StartupNotice:    notice,
				InitialReplay:    initial,
				// /sessions：列当前工作区的会话。TUI 不 import app，
				// 所以在这一层把 app 的行/简报映射成 TUI 自己的类型。
				Sessions: func() []tui.SessionBrief {
					briefs := sess.WorkspaceSessions()
					out := make([]tui.SessionBrief, 0, len(briefs))
					for _, b := range briefs {
						out = append(out, tui.SessionBrief{
							ID: b.ID, Title: b.Title, Turns: b.Turns,
							Updated: b.Updated, Current: b.Current,
						})
					}
					return out
				},
				// /sessions <序号>：切过去并回放历史。
				// 跨工作区的会话由 app 层连工作区一起切，返回的目录要写回 opts.CWD，
				// 否则状态行会停在旧目录。
				OpenSession: func(id string) ([]tui.ReplayLine, string, error) {
					dir, err := sess.OpenSession(id)
					if err != nil {
						return nil, "", err
					}
					lines := sess.TranscriptLines()
					out := make([]tui.ReplayLine, 0, len(lines))
					for _, l := range lines {
						out = append(out, tui.ReplayLine{
							Role: l.Role, Content: l.Content, Name: l.Name, IsError: l.IsError,
						})
					}
					return out, dir, nil
				},
				// /skills：用闭包而不是取一次固定值——工作目录会随 /cd 变，
				// 而项目级技能就挂在 <工作目录>/.arkperf/skills 下
				SkillReport:  func() string { return app.SkillReport(sess.CWD()) },
				SkillSummary: sess.SkillSummary,
				// /rules：审批规则的清单（含"怎么删"）
				RulesReport: func() string { return app.ApprovalRulesSummary(sess.ApprovalRules()) },
				// /cd：切工作目录，返回切换后的目录（失败如实抛出，让 TUI 显示原因）
				SwitchWorkspace: func(path string) (string, error) {
					if err := sess.SetWorkspace(path); err != nil {
						return "", err
					}
					return sess.CWD(), nil
				},
				NewAgent: func(ap kernel.Approver, ev kernel.LoopEvents) tui.RunFunc {
					return tui.RunFunc(sess.NewRunner(ap, ev))
				},
				ToolchainReport: app.ToolchainReport,
				DeviceReport:    app.DeviceReport,
			})
		},
	}

	return c
}

// 工具链报告 / 设备报告已移到 internal/app：它们同时服务命令行子命令与
// 各前端的 /check 与 /devices，放在装配层才能保证只有一份实现。
