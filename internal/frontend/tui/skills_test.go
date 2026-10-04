package tui

import (
	"strings"
	"testing"
)

const fakeSkillReport = "技能目录（按优先级）：\n  [builtin] C:\\proj\\skills\n\n技能 2 个 · 内置 2"

func TestSlashSkillsPrintsReport(t *testing.T) {
	h := newHarness(t, false)
	called := false
	h.m.opts.SkillReport = func() string {
		called = true
		return fakeSkillReport
	}

	h.submit("/skills")

	if !called {
		t.Fatal("/skills 应当调用注入的报告函数")
	}
	out := h.allTranscript()
	for _, want := range []string{"技能目录", "builtin", "技能 2 个"} {
		if !strings.Contains(out, want) {
			t.Fatalf("转录里缺少 %q：%q", want, out)
		}
	}
}

// 报告函数为空时要说"不可用"，而不是静默什么都不做——
// 静默的表现是"敲了命令但屏幕没反应"，用户只会以为界面卡了。
func TestSlashSkillsReportsUnavailable(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.SkillReport = nil

	h.submit("/skills")

	if out := h.allTranscript(); !strings.Contains(out, "不可用") {
		t.Fatalf("应当提示不可用：%q", out)
	}
}

func TestSlashHelpListsSkills(t *testing.T) {
	h := newHarness(t, false)
	h.submit("/help")

	if out := h.allTranscript(); !strings.Contains(out, "/skills") {
		t.Fatalf("帮助里应当列出 /skills：%q", out)
	}
}

func TestSlashStatusShowsSkillSummary(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.SkillSummary = func() string { return "技能 2 个 · 内置 2" }

	h.submit("/status")

	out := h.allTranscript()
	if !strings.Contains(out, "技能") || !strings.Contains(out, "技能 2 个 · 内置 2") {
		t.Fatalf("/status 应当显示技能摘要：%q", out)
	}
}

// 摘要为空时不该在状态里留一行空标签。
func TestSlashStatusSkipsEmptySkillSummary(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.SkillSummary = func() string { return "" }

	h.submit("/status")

	if out := h.allTranscript(); strings.Contains(out, "技能：") {
		t.Fatalf("空摘要不该留空标签：%q", out)
	}
}

func TestSlashSkillsDoesNotStartTask(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.SkillReport = func() string { return fakeSkillReport }

	h.submit("/skills")

	select {
	case task := <-h.task:
		t.Fatalf("斜杠命令不该被当成任务发给模型，实际收到 %q", task)
	default:
	}
}

// 命令表同时驱动 /help 与补全菜单，重名或残缺会让两者对不上。
func TestCommandsTableIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range commands() {
		if c.Name == "" || c.Help == "" || c.Run == nil {
			t.Fatalf("命令表有不完整项：%+v", c)
		}
		if seen[c.Name] {
			t.Fatalf("命令名重复：%s", c.Name)
		}
		seen[c.Name] = true
	}
	// quit 是 exit 的别名，两条都必须在，否则用户按习惯敲 /quit 会得到"未知命令"
	if !seen["exit"] || !seen["quit"] {
		t.Fatal("exit 与 quit 都必须存在")
	}
	if !seen["skills"] {
		t.Fatal("技能清单是纯数据能力，必须有对应命令")
	}
	if !seen["rules"] {
		t.Fatal("审批规则只减少询问，必须有个地方能看见自己放行了什么")
	}
}
