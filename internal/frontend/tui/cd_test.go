package tui

import (
	"fmt"
	"strings"
	"testing"
)

// /cd 成功时三件事要一起做：调注入的切换回调、更新状态行显示的目录、**清屏**。
//
// 清屏这条最容易被省掉，但省了就会出最危险的那种错位：
// 屏幕上是 A 项目的对话，模型却拿着 B 项目的历史在干活。
func TestCdSwitchesWorkspaceAndClearsTranscript(t *testing.T) {
	h := newHarness(t, false)

	// 先造一点"切换之前"的内容，用来验证它确实被清掉
	h.submit("/help")
	if !strings.Contains(h.m.View().Content, "命令") {
		t.Fatal("前置条件不成立：/help 应当有输出")
	}

	var gotPath string
	h.m.opts.SwitchWorkspace = func(path string) (string, error) {
		gotPath = path
		return `D:\ws\next`, nil
	}

	h.submit(`/cd D:\ws\next`)

	if gotPath != `D:\ws\next` {
		t.Fatalf("切换回调应收到目标路径，实际 %q", gotPath)
	}
	if h.m.opts.CWD != `D:\ws\next` {
		t.Fatalf("状态行的工作目录应更新，实际 %q", h.m.opts.CWD)
	}
	content := h.m.View().Content
	if !strings.Contains(content, "已切换工作目录") {
		t.Fatalf("应打印切换说明：\n%s", content)
	}
	if strings.Contains(content, "命令") {
		t.Fatal("切工作区必须清屏：旧转录还留在屏幕上")
	}
}

// 不带参数：给用法提示，并且**不该**调用切换（否则等于用空路径去切目录）
func TestCdWithoutArgShowsUsageOnly(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.SwitchWorkspace = func(string) (string, error) {
		t.Fatal("无参数时不该调用切换回调")
		return "", nil
	}

	h.submit("/cd")

	if !strings.Contains(h.m.View().Content, "用法：/cd") {
		t.Fatalf("应给出用法提示：\n%s", h.m.View().Content)
	}
}

// 失败：如实报错，且不改动状态行里的工作目录
func TestCdFailureKeepsCurrentWorkspace(t *testing.T) {
	h := newHarness(t, false)
	before := h.m.opts.CWD
	h.m.opts.SwitchWorkspace = func(string) (string, error) {
		return "", fmt.Errorf("工作目录不可用：目录不存在")
	}

	h.submit("/cd 并不存在的目录")

	if h.m.opts.CWD != before {
		t.Fatalf("切换失败不该改变显示的工作目录：%q → %q", before, h.m.opts.CWD)
	}
	if !strings.Contains(h.m.View().Content, "切换失败") {
		t.Fatalf("应显示失败原因：\n%s", h.m.View().Content)
	}
}
