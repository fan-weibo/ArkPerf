package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

func TestSlashRulesPrintsReport(t *testing.T) {
	h := newHarness(t, false)
	called := false
	h.m.opts.RulesReport = func() string {
		called = true
		return "已保存 1 条审批规则（这些类别不再询问）：\n  run_command      git status"
	}

	h.submit("/rules")

	if !called {
		t.Fatal("/rules 应当调用注入的报告函数")
	}
	out := h.allTranscript()
	for _, want := range []string{"已保存 1 条", "run_command", "git status"} {
		if !strings.Contains(out, want) {
			t.Fatalf("转录里缺少 %q：%q", want, out)
		}
	}
}

func TestSlashRulesReportsUnavailable(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.RulesReport = nil

	h.submit("/rules")

	if out := h.allTranscript(); !strings.Contains(out, "不可用") {
		t.Fatalf("应当提示不可用：%q", out)
	}
}

// 审批卡必须把"这一类"原样显示出来：只给工具名的话，用户会把
// "以后都不问"理解成"永远允许这个工具"，而实际范围可能只是某个目录。
func TestApprovalCardShowsScope(t *testing.T) {
	h := newHarness(t, false)
	reply := make(chan kernel.ApprovalDecision, 1)
	h.m.Update(event{
		kind:  evApproval,
		name:  "run_command",
		args:  map[string]any{"command": []any{"git", "status"}},
		scope: "git status",
		reply: reply,
	})

	card := h.m.View().Content
	if !strings.Contains(card, "git status") {
		t.Fatalf("审批卡应当显示类别：%q", card)
	}
	if !strings.Contains(card, "以后都不问") {
		t.Fatalf("有类别时应当给出「以后都不问」的选项：%q", card)
	}
	reply <- kernel.ApprovalDeny
}

// 没有类别时不该出现「以后都不问」：给了也只能退化成一次放行，
// 用户会以为已经一劳永逸。
func TestApprovalCardHidesAlwaysWhenNoScope(t *testing.T) {
	h := newHarness(t, false)
	reply := make(chan kernel.ApprovalDecision, 1)
	h.m.Update(event{kind: evApproval, name: "harmony_build", reply: reply})

	if card := h.m.View().Content; strings.Contains(card, "以后都不问") {
		t.Fatalf("没有类别时不该给出该选项：%q", card)
	}
	reply <- kernel.ApprovalDeny
}

// Shift+a 才是"以后都不问"；小写 a 被忽略（见 TestApprovalIgnoresUnrelatedKeys）。
func TestApprovalShiftAChoosesAlways(t *testing.T) {
	h := newHarness(t, false)
	reply := make(chan kernel.ApprovalDecision, 1)
	h.m.Update(event{kind: evApproval, name: "run_command", scope: "git status", reply: reply})

	h.m.Update(tea.KeyPressMsg{Code: 'A', Text: "A"})

	select {
	case got := <-reply:
		if got != kernel.ApprovalAlways {
			t.Fatalf("应当选择「以后都不问」，得到 %v", got)
		}
	default:
		t.Fatal("应当回传决定")
	}
	// 确认信息要说清楚记住了什么，用户才知道"这一类"到底有多宽
	if out := h.allTranscript(); !strings.Contains(out, "以后不再询问") {
		t.Fatalf("应当确认已记住：%q", out)
	}
}

// 没有类别却按了 A：只放行本次，并且**要说出来**。
func TestApprovalShiftAWithoutScopeDegradesToOnce(t *testing.T) {
	h := newHarness(t, false)
	reply := make(chan kernel.ApprovalDecision, 1)
	h.m.Update(event{kind: evApproval, name: "harmony_build", reply: reply})

	h.m.Update(tea.KeyPressMsg{Code: 'A', Text: "A"})

	select {
	case got := <-reply:
		if got != kernel.ApprovalOnce {
			t.Fatalf("没有类别时应当只放行本次，得到 %v", got)
		}
	default:
		t.Fatal("应当回传决定")
	}
	if out := h.allTranscript(); !strings.Contains(out, "只放行了本次") {
		t.Fatalf("退化必须说出来：%q", out)
	}
}

// 三种规则通知的措辞都要能把事情说清楚——尤其是"没记住"，
// 不说的话用户以为已经生效，下次被再问一遍只会觉得功能坏了。
func TestApprovalRuleText(t *testing.T) {
	cases := []struct {
		name string
		note kernel.ApprovalRuleNote
		want string
	}{
		{"命中规则", kernel.ApprovalRuleNote{Name: "run_command", Scope: "git status", Hit: true}, "按已保存的规则放行"},
		{"已记住", kernel.ApprovalRuleNote{Name: "run_command", Scope: "git status", Saved: true}, "以后不再询问"},
		{"没记住", kernel.ApprovalRuleNote{
			Name: "run_command", Scope: "git status",
			Err: errors.New("磁盘满了"),
		}, "下次还会问你"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := approvalRuleText(tc.note); !strings.Contains(got, tc.want) {
				t.Fatalf("措辞 = %q，应当包含 %q", got, tc.want)
			}
		})
	}
}

// /status 里不显示规则，但 /help 必须有它——否则用户不知道有这个入口。
func TestSlashHelpListsRules(t *testing.T) {
	h := newHarness(t, false)
	h.submit("/help")

	if out := h.allTranscript(); !strings.Contains(out, "/rules") {
		t.Fatalf("帮助里应当列出 /rules：%q", out)
	}
}
