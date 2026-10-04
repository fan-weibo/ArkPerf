package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 工作目录的解析优先级。这里踩过一个真实缺陷：终端里 `cd E:\.OpenHarmony`
// 启动 TUI，工作区却停在历史里的另一个目录——因为"进程 CWD"被排在
// "最近一次会话的目录"之后，终端用户的明确意图被无视了（实测被当场抓到）。
//
// 规则：显式 > 配置 > **进程 CWD（非 exe 目录时）** > 最近会话 > 进程 CWD（兜底）。
func TestPickWorkspace(t *testing.T) {
	dir := func(name string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("造目录失败：%v", err)
		}
		return p
	}
	missing := filepath.Join("E:", "definitely", "not", "here")
	exeDir := filepath.Join("E:", "ArkPerf", "desktop", "bin")

	t.Run("显式指定最高优先", func(t *testing.T) {
		exp, cfg, recent, wd := dir("exp"), dir("cfg"), dir("recent"), dir("wd")
		if got := pickWorkspace(exp, cfg, []string{recent}, wd, exeDir); got != exp {
			t.Fatalf("got %q, want %q", got, exp)
		}
	})

	t.Run("没有显式时用配置的 workspace", func(t *testing.T) {
		cfg, wd := dir("cfg"), dir("wd")
		if got := pickWorkspace("", cfg, nil, wd, exeDir); got != cfg {
			t.Fatalf("got %q, want %q", got, cfg)
		}
	})

	// 这一条就是被用户抓到的那个 bug：终端里 cd 过去应当胜出
	t.Run("终端 cd 过去时 CWD 压过最近会话", func(t *testing.T) {
		wd, recent := dir("cd到的地方"), dir("上次聊天的目录")
		if got := pickWorkspace("", "", []string{recent}, wd, exeDir); got != wd {
			t.Fatalf("CWD 应当胜出：got %q, want %q", got, wd)
		}
	})

	t.Run("双击启动（CWD=exe 目录）时退回最近会话", func(t *testing.T) {
		recent := dir("上次聊天的目录")
		exeDir := dir("bin")
		// 双击启动的真实形态：进程 CWD 就是 exe 所在的那个目录
		if got := pickWorkspace("", "", []string{recent}, exeDir, exeDir); got != recent {
			t.Fatalf("应退回最近会话：got %q, want %q", got, recent)
		}
	})

	t.Run("大小写不同也算同一个 exe 目录", func(t *testing.T) {
		recent := dir("history")
		exeDir := dir("bin")
		if got := pickWorkspace("", "", []string{recent}, exeDir, strings.ToUpper(exeDir)); got != recent {
			t.Fatalf("got %q, want %q", got, recent)
		}
	})

	t.Run("不存在的候选被跳过", func(t *testing.T) {
		wd := dir("真实目录")
		if got := pickWorkspace(missing, missing, []string{missing}, wd, exeDir); got != wd {
			t.Fatalf("got %q, want %q", got, wd)
		}
	})

	t.Run("最近会话里不存在的目录也跳过", func(t *testing.T) {
		wd := dir("真实目录")
		if got := pickWorkspace("", "", []string{missing}, wd, exeDir); got != wd {
			t.Fatalf("got %q, want %q", got, wd)
		}
	})

	t.Run("全都拿不到时退回当前目录", func(t *testing.T) {
		if got := pickWorkspace(missing, "", nil, "", ""); got != "." {
			t.Fatalf("got %q, want \".\"", got)
		}
	})
}

// samePath：空串一律判不等——"拿不到路径"不该被当成"两条路径相同"。
func TestSamePath(t *testing.T) {
	if samePath("", "") {
		t.Fatal("两个空串不该判为同一条路径")
	}
	if samePath(`E:\a`, "") {
		t.Fatal("空串与实路径不该判为相同")
	}
	if !samePath(`E:\Proj\A`, `e:\proj\a`) {
		t.Fatal("Windows 路径应大小写不敏感")
	}
	if samePath(`E:\a`, `E:\b`) {
		t.Fatal("不同路径不该判为相同")
	}
}
