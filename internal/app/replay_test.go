package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// replaySession 造一个带工具调用的会话，供回放相关测试用。
//
// 形状刻意覆盖三种情况：正常结果、失败结果（ERROR: 前缀）、
// 以及"只调工具不说话"的 assistant（这类不该在回放里留下一行空白）。
func replaySession(t *testing.T) *Session {
	t.Helper()
	s := openSession(t)

	store := kernel.NewSessionStore(kernel.Home())
	sess := store.New(s.CWD())
	sess.Messages = []kernel.Message{
		{Role: "user", Content: "建个文件"},
		{Role: "assistant", Content: "我先建文件", ToolCalls: []kernel.ToolCall{
			{ID: "c1", Function: kernel.FunctionCall{Name: "write_file", Arguments: `{"path":"a.txt"}`}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "已创建"},
		// 只调工具、没有说话内容
		{Role: "assistant", ToolCalls: []kernel.ToolCall{
			{ID: "c2", Function: kernel.FunctionCall{Name: "read_file", Arguments: `{"path":"a.txt"}`}},
		}},
		{Role: "tool", ToolCallID: "c2", Content: "ERROR: 文件不存在"},
		{Role: "assistant", Content: "建好了"},
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("造历史失败：%v", err)
	}
	if err := s.LoadByID(sess.ID); err != nil {
		t.Fatalf("切到测试会话失败：%v", err)
	}
	return s
}

// 回放必须包含工具调用：只回放 user/assistant 的话，
// 用户切个会话回来会觉得"工具调用全没了"，也就看不懂结论是怎么来的。
func TestTranscriptLinesIncludeToolCalls(t *testing.T) {
	lines := replaySession(t).TranscriptLines()

	var toolLines, resultLines int
	for _, l := range lines {
		switch l.Role {
		case "tool":
			toolLines++
			if l.Name == "" || l.Content == "" {
				t.Fatalf("tool 行缺名字或参数：%+v", l)
			}
		case "result":
			resultLines++
			if l.Name == "" {
				t.Fatalf("result 行缺工具名：%+v", l)
			}
		}
	}
	if toolLines != 2 || resultLines != 2 {
		t.Fatalf("工具行应当 2 调 2 结，实际 %d / %d", toolLines, resultLines)
	}
}

// 失败结果的标记要从正文前缀恢复——Message 里没有独立的错误位。
func TestTranscriptLinesMarkFailedResults(t *testing.T) {
	lines := replaySession(t).TranscriptLines()

	var failed, ok int
	for _, l := range lines {
		if l.Role != "result" {
			continue
		}
		if l.IsError {
			failed++
			if !strings.Contains(l.Content, "ERROR:") {
				t.Fatalf("失败行应保留原始正文：%+v", l)
			}
		} else {
			ok++
		}
	}
	if failed != 1 || ok != 1 {
		t.Fatalf("应有 1 失败 1 成功，实际 %d / %d", failed, ok)
	}
}

// 「只调工具不说话」的 assistant 不该在回放里留下一个空节点。
func TestTranscriptLinesSkipEmptyAssistant(t *testing.T) {
	lines := replaySession(t).TranscriptLines()

	for _, l := range lines {
		if l.Role == "assistant" && strings.TrimSpace(l.Content) == "" {
			t.Fatal("回放里不该出现空的 assistant 行")
		}
	}
}

// 工具结果应该紧跟在它对应的调用之后（顺序错了，用户读起来就对不上）。
func TestTranscriptLinesOrderToolsBeforeResults(t *testing.T) {
	lines := replaySession(t).TranscriptLines()

	seenTool := map[string]bool{}
	for _, l := range lines {
		if l.Role == "tool" {
			seenTool[l.Name] = true
		}
		if l.Role == "result" && !seenTool[l.Name] {
			t.Fatalf("结果行 %q 出现在它的调用行之前：%+v", l.Name, lines)
		}
	}
}

// 会话列表只含当前工作区，且按**创建时间**倒序（不能按 Updated：
// 切一次会话就会 Save 一次，次序会跟着变，用户刚点的条目会换位置）。
func TestWorkspaceSessionsOnlyCurrentWorkspace(t *testing.T) {
	s := openSession(t)
	here, elsewhere := s.CWD(), t.TempDir()

	_ = seedSession(t, here, "本目录第一条")
	_ = seedSession(t, here, "本目录第二条")
	_ = seedSession(t, elsewhere, "别的目录")

	list := s.WorkspaceSessions()
	if len(list) != 2 {
		t.Fatalf("只应列出当前工作区的 2 条，实际 %d：%+v", len(list), list)
	}
	for _, b := range list {
		if strings.TrimSpace(b.Title) == "" || b.ID == "" {
			t.Fatalf("列表项缺标题或 ID：%+v", b)
		}
	}
}

// 空会话不进列表：点进去等于没点。
func TestWorkspaceSessionsSkipsEmpty(t *testing.T) {
	s := openSession(t)

	store := kernel.NewSessionStore(kernel.Home())
	if err := store.Save(store.New(s.CWD())); err != nil { // 没有任何消息
		t.Fatal(err)
	}
	if got := len(s.WorkspaceSessions()); got != 0 {
		t.Fatalf("空会话不该进列表，实际 %d 条", got)
	}
}

// 同工作区切换：直接换，不动工作区。
func TestOpenSessionSameWorkspace(t *testing.T) {
	s := openSession(t)
	id := seedSession(t, s.CWD(), "要被切过去的会话")

	dir, err := s.OpenSession(id)
	if err != nil {
		t.Fatalf("切换失败：%v", err)
	}
	if !samePath(dir, s.CWD()) {
		t.Fatalf("同目录切换不该改变工作区：%q", dir)
	}
	if s.CurrentID() != id {
		t.Fatalf("当前会话没换过去：%s", s.CurrentID())
	}
}

// 跨工作区切换：连同工作区一起切过去。不切的话，
// "看到的历史"和"实际操作的目录"会错开（比没有历史更糟）。
func TestOpenSessionSwitchesWorkspace(t *testing.T) {
	s := openSession(t)
	other := t.TempDir()
	id := seedSession(t, other, "别的项目里的会话")

	dir, err := s.OpenSession(id)
	if err != nil {
		t.Fatalf("跨目录切换失败：%v", err)
	}
	if !samePath(dir, other) {
		t.Fatalf("工作区没跟着切：got %q want %q", dir, other)
	}
	if !samePath(s.CWD(), other) {
		t.Fatalf("Session.CWD 没更新：%q", s.CWD())
	}
	if s.CurrentID() != id {
		t.Fatalf("当前会话没换过去：%s", s.CurrentID())
	}
}

func TestOpenSessionUnknownID(t *testing.T) {
	s := openSession(t)
	if _, err := s.OpenSession("no-such-session"); err == nil {
		t.Fatal("不存在的会话应当报错")
	}
}

// NewSession 之后旧会话必须还在磁盘上、并且能被 OpenSession 找回来——
// 这是"/new 会不会把旧会话弄丢"这个问题的回归测试。
func TestNewSessionKeepsOldRecoverable(t *testing.T) {
	s := openSession(t)
	old := seedSession(t, s.CWD(), "旧会话的内容")

	// 让旧会话成为当前会话（否则 NewSession 存的是另一条）
	if _, err := s.OpenSession(old); err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession(); err != nil {
		t.Fatalf("NewSession 失败：%v", err)
	}
	if s.CurrentID() == old {
		t.Fatal("NewSession 之后当前会话应当换了新的")
	}

	// 旧会话仍然可以切回去
	dir, err := s.OpenSession(old)
	if err != nil {
		t.Fatalf("旧会话应当还能切回去：%v", err)
	}
	if dir == "" || s.CurrentID() != old {
		t.Fatalf("切回去失败：dir=%q id=%s", dir, s.CurrentID())
	}
	if len(s.Transcript()) == 0 {
		t.Fatal("切回去之后应当能看到旧会话的内容")
	}
}

// 路径比较：空串不对等，大小写不敏感。
func TestSamePathIncludingEmpty(t *testing.T) {
	if !samePath(filepath.Clean(`E:\A`), `e:\a`) {
		t.Fatal("大小写不同的同一路径应当相等")
	}
	if samePath("", "") {
		t.Fatal("两个空串不该判为同一路径")
	}
}
