// Command arkperf —— 方舟智诊：OpenHarmony 应用性能体验分析与优化 Agent。
package main

import (
	"context"
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
				set := app.AttachMCP(cmd.Context(), cfg, reg)
				defer set.Close()
				reportMCP(out, set)
			}

			runner := &kernel.Runner{
				Cfg:      cfg,
				Registry: reg,
				Approver: cli.NewApprover(os.Stdin, out, flagAutoApprove),
				Out:      out,
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

	root.AddCommand(versionCmd(), initCmd(), configCmd(), toolsCmd(), mcpCmd(), checkCmd(), devicesCmd(), tuiCmd())
	root.Version = version
	return root
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
			set := app.AttachMCP(cmd.Context(), cfg, reg)
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
				ResetSession:     sess.Reset,
				StartupNotice:    notice,
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
