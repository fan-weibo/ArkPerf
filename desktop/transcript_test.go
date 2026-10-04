package main

import (
	"context"
	"strings"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/app"
	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// openTestSession 打开一个隔离的状态根，并造一段带工具调用的历史。
func openTestSession(t *testing.T) *app.Session {
	t.Helper()
	t.Setenv("ARKPERF_HOME", t.TempDir())
	t.Setenv("ARKPERF_BASE_URL", "http://127.0.0.1:1/v1")
	t.Setenv("ARKPERF_API_KEY", "k")
	t.Setenv("ARKPERF_MODEL", "m")

	s, err := app.Open(context.Background(), app.Options{NoMCP: true})
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	t.Cleanup(func() { s.Close() })

	store := kernel.NewSessionStore(kernel.Home())
	sess := store.New(s.CWD())
	sess.Messages = []kernel.Message{
		{Role: "user", Content: "建个文件"},
		{Role: "assistant", ToolCalls: []kernel.ToolCall{
			{ID: "c1", Function: kernel.FunctionCall{Name: "write_file", Arguments: `{"path":"a.txt"}`}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "已创建"},
		{Role: "assistant", Content: "建好了"},
		{Role: "user", Content: "再建一个"},
		{Role: "assistant", ToolCalls: []kernel.ToolCall{
			{ID: "c2", Function: kernel.FunctionCall{Name: "write_file", Arguments: `{"path":"b.txt"}`}},
		}},
		{Role: "tool", ToolCallID: "c2", Content: "已创建"},
		{Role: "assistant", Content: "第二个也建好了"},
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("造历史失败：%v", err)
	}
	if err := s.LoadByID(sess.ID); err != nil {
		t.Fatalf("切到测试会话失败：%v", err)
	}
	return s
}

// 回放必须包含工具调用：之前只回放 user/assistant，
// 用户切个会话回来，工具调用全没了，看到的对话是残缺的。
func TestTranscriptLinesIncludeToolCalls(t *testing.T) {
	s := openTestSession(t)
	lines := transcriptLines(s)

	joined := ""
	for _, l := range lines {
		joined += l.Role + ":" + l.Content + " | "
	}
	for _, want := range []string{`{"path":"a.txt"}`, "已创建"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("回放里缺少 %q（工具调用被丢掉了）：%s", want, joined)
		}
	}
}

// 工具名要从 tool_call_id 反查：结果行显示的应当是工具名而不是裸 id。
func TestTranscriptLinesResolvesToolNames(t *testing.T) {
	s := openTestSession(t)
	lines := transcriptLines(s)

	for _, l := range lines {
		if l.Role != "result" {
			continue
		}
		if strings.Contains(l.Name, "call_") {
			t.Fatalf("结果行应当显示工具名而不是裸 id：%q", l.Name)
		}
	}
}
