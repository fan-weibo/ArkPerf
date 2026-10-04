package mcp

import (
	"path/filepath"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// 远端工具里的相对 out_dir 必须按**当前工作区**解析，不能交给子进程。
//
// 实测原因：MCP 子进程的 CWD 只在启动那一刻定下来——桌面端从 exe 目录启动
// （不是工作区），而且切了工作区也不会跟着变。不在这里补绝对路径，
// 报告就会落到 exe 旁边，用户按预期去工程里找会找不到。
func TestAbsolutizeOutDir(t *testing.T) {
	ws := filepath.Join("E:", "proj", "demo")

	t.Run("相对路径按工作区补全", func(t *testing.T) {
		args := map[string]any{"out_dir": "reports", "bundle_name": "x"}
		absolutizeOutDir(args, ws)
		if got := args["out_dir"]; got != filepath.Join(ws, "reports") {
			t.Fatalf("out_dir = %v，期望 %v", got, filepath.Join(ws, "reports"))
		}
		// 其他参数不能被碰
		if args["bundle_name"] != "x" {
			t.Fatalf("不该动其他参数：%+v", args)
		}
	})

	t.Run("嵌套相对路径", func(t *testing.T) {
		args := map[string]any{"out_dir": filepath.Join("out", "2026")}
		absolutizeOutDir(args, ws)
		if got := args["out_dir"]; got != filepath.Join(ws, "out", "2026") {
			t.Fatalf("out_dir = %v", got)
		}
	})

	t.Run("绝对路径原样不动", func(t *testing.T) {
		// 用 TempDir 构造真正的绝对路径：手写 `filepath.Join("D:", "x")` 在 Windows 上
		// 会得到 "D:x"——那是**驱动器相对**路径，IsAbs 判 false（本测试第一版就栽在这）。
		abs := filepath.Join(t.TempDir(), "elsewhere")
		args := map[string]any{"out_dir": abs}
		absolutizeOutDir(args, ws)
		if got := args["out_dir"]; got != abs {
			t.Fatalf("模型明确指定位置时不该被纠正：%v", got)
		}
	})

	t.Run("没有 out_dir 就不管", func(t *testing.T) {
		args := map[string]any{"pattern": "TODO"}
		absolutizeOutDir(args, ws)
		if len(args) != 1 {
			t.Fatalf("不该凭空加参数：%+v", args)
		}
	})

	t.Run("类型不对或为空时不动", func(t *testing.T) {
		for _, bad := range []any{42, "", "   ", nil} {
			args := map[string]any{"out_dir": bad}
			absolutizeOutDir(args, ws)
			if args["out_dir"] != bad {
				t.Fatalf("out_dir=%v 不该被改写，实际 %v", bad, args["out_dir"])
			}
		}
	})

	t.Run("拿不到工作区时什么都不做", func(t *testing.T) {
		args := map[string]any{"out_dir": "reports"}
		absolutizeOutDir(args, "")
		if args["out_dir"] != "reports" {
			t.Fatalf("没有工作区时应保持原样（子进程继承父进程 CWD），实际 %v", args["out_dir"])
		}
	})
}

// 端到端（内存传输，无子进程）：相对 out_dir 必须以**绝对路径**抵达服务端。
//
// 这是"报告落在哪"的最后一环。函数级单测只能证明 absolutizeOutDir 算得对，
// 证明不了 Execute 真的调了它、也没证明改的是**发出去的那份参数**。
// 链路正确性对 TUI / 桌面端同样成立：三者共用 Runner → LoopConfig.Ctx.CWD。
func TestExecuteSendsAbsolutizedOutDir(t *testing.T) {
	_, reg := attachFake(t, "fake-harmony", true)

	tool, ok := reg.Get("mcp_fake_harmony_outdir_probe")
	if !ok {
		t.Fatal("探针工具没有注册上")
	}

	ws := filepath.Join(t.TempDir(), "workspace")
	res, err := tool.Execute(t.Context(), map[string]any{"out_dir": "reports"},
		kernel.ToolCtx{CWD: ws})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := "out_dir=" + filepath.Join(ws, "reports")
	if res.Output != want {
		t.Fatalf("服务端收到的 out_dir 不是工作区下的绝对路径：\n got %q\nwant %q", res.Output, want)
	}

	// 绝对路径要原样送达（模型明确指定位置时不该被改）
	abs := filepath.Join(t.TempDir(), "elsewhere")
	res, err = tool.Execute(t.Context(), map[string]any{"out_dir": abs}, kernel.ToolCtx{CWD: ws})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Output != "out_dir="+abs {
		t.Fatalf("绝对路径被改动了：%q", res.Output)
	}
}
