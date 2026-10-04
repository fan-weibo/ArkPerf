package tui

import (
	"errors"
	"strings"
	"testing"
)

// 注：harness（app_test.go）已经提供了不接终端的 Model 与事件通道，
// 这里只补 /sessions 与 /new 两条命令的行为。

// 会话列表要能看出来：有哪些会话、哪个是当前、怎么切过去。
func TestSessionsListsAndMarksCurrent(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.Sessions = func() []SessionBrief {
		return []SessionBrief{
			{ID: "s1", Title: "体检基线", Turns: 3, Updated: "10-05 06:04", Current: true},
			{ID: "s2", Title: "冷启动专项", Turns: 1, Updated: "10-05 05:12"},
		}
	}

	h.submit("/sessions")

	out := h.allTranscript()
	for _, want := range []string{"体检基线", "冷启动专项", "(3 轮", "(1 轮", "/sessions <序号>"} {
		if !strings.Contains(out, want) {
			t.Fatalf("列表里缺少 %q：\n%s", want, out)
		}
	}
}

// 没有会话时要说清楚原因，而不是给一片空白。
func TestSessionsEmptyWorkspace(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.Sessions = func() []SessionBrief { return nil }

	h.submit("/sessions")

	if out := h.allTranscript(); !strings.Contains(out, "还没有会话") {
		t.Fatalf("空列表应当给出说明：%q", out)
	}
}

// 按序号切会话：要回放历史、更新工作目录、并说明切到了哪。
func TestSessionsSwitchReplaysAndUpdatesCWD(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.Sessions = func() []SessionBrief {
		return []SessionBrief{
			{ID: "s1", Title: "当前会话", Current: true},
			{ID: "s2", Title: "目标会话", Turns: 2, Updated: "10-05 05:12"},
		}
	}

	var gotID string
	h.m.opts.OpenSession = func(id string) ([]ReplayLine, string, error) {
		gotID = id
		return []ReplayLine{
			{Role: "user", Content: "之前问的问题"},
			{Role: "assistant", Content: "之前的回答"},
			{Role: "tool", Name: "read_file", Content: `{"path":"a.txt"}`},
			{Role: "result", Name: "read_file", Content: "文件内容"},
			{Role: "result", Name: "read_file", Content: "ERROR: 打不开", IsError: true},
		}, `E:\other-proj`, nil
	}

	h.submit("/sessions 2")

	if gotID != "s2" {
		t.Fatalf("应当切到第 2 项（s2），实际 %q", gotID)
	}
	out := h.allTranscript()
	for _, want := range []string{"之前问的问题", "之前的回答", "read_file", "文件内容", "✗"} {
		if !strings.Contains(out, want) {
			t.Fatalf("回放里缺少 %q：\n%s", want, out)
		}
	}
	if h.m.opts.CWD != `E:\other-proj` {
		t.Fatalf("工作目录没跟着更新：%q", h.m.opts.CWD)
	}
}

// 序号越界 / 非数字：给用法提示，不要静默什么都不做。
func TestSessionsRejectsBadIndex(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.Sessions = func() []SessionBrief {
		return []SessionBrief{{ID: "s1", Title: "唯一", Current: true}}
	}
	h.m.opts.OpenSession = func(string) ([]ReplayLine, string, error) {
		t.Fatal("非法序号不该真的去切会话")
		return nil, "", nil
	}

	for _, bad := range []string{"9", "abc", "-1"} {
		h.m.transcript = nil
		h.submit("/sessions " + bad)
		if out := h.allTranscript(); !strings.Contains(out, "序号无效") {
			t.Fatalf("%q 应当被拒绝：%q", bad, out)
		}
	}
}

// 切换失败要如实显示原因（例如会话文件被手工删了）。
func TestSessionsSwitchErrorIsShown(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.Sessions = func() []SessionBrief {
		return []SessionBrief{
			{ID: "s1", Title: "当前", Current: true},
			{ID: "s2", Title: "目标"},
		}
	}
	h.m.opts.OpenSession = func(string) ([]ReplayLine, string, error) {
		return nil, "", errors.New("找不到会话：s2")
	}

	h.submit("/sessions 2")

	if out := h.allTranscript(); !strings.Contains(out, "找不到会话") {
		t.Fatalf("失败原因应当显示出来：%q", out)
	}
}

// /new 之后屏幕要清空——"开了新会话却还挂着旧对话"看起来不像新的，
// 也容易出现"屏幕上写着 A、模型手里是空的"这种错位。
func TestNewClearsScreen(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.ResetSession = func() error { return nil }

	// 先让屏幕上有点东西
	h.m.transcript = []string{"旧会话的内容"}
	h.submit("/new")

	out := h.allTranscript()
	if strings.Contains(out, "旧会话的内容") {
		t.Fatalf("/new 应当清掉屏幕上的旧内容：\n%s", out)
	}
	if !strings.Contains(out, "上下文与屏幕都已清空") {
		t.Fatalf("应当说明清空了什么：\n%s", out)
	}
	// 并且要告诉用户旧会话没丢、去哪找
	if !strings.Contains(out, "/sessions") {
		t.Fatalf("应当提示旧会话可以用 /sessions 找回：\n%s", out)
	}
}

// /new 失败（装配层报错）时不能清屏——否则用户以为成功了。
func TestNewKeepsScreenWhenResetFails(t *testing.T) {
	h := newHarness(t, false)
	h.m.opts.ResetSession = func() error { return errors.New("磁盘只读") }

	h.m.transcript = []string{"旧会话的内容"}
	h.submit("/new")

	if out := h.allTranscript(); !strings.Contains(out, "旧会话的内容") {
		t.Fatalf("失败时不该清屏：\n%s", out)
	}
}
