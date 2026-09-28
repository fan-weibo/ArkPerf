package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// setupHome 把状态根指到临时目录，并用环境变量供上必填的 provider 字段。
//
// 这样测的是真实装配路径（配置加载 + 校验 + 会话存取），不是一份玩具实现；
// 又因为 ARKPERF_HOME 被隔离，不会碰到用户真实的 ~/.arkperf。
func setupHome(t *testing.T) {
	t.Helper()
	t.Setenv("ARKPERF_HOME", t.TempDir())
	t.Setenv("ARKPERF_BASE_URL", "http://127.0.0.1:1/v1")
	t.Setenv("ARKPERF_API_KEY", "test-key")
	t.Setenv("ARKPERF_MODEL", "test-model")
}

// 没有历史时开新会话：横幅要能反映身份，且不该谎称"已恢复"。
func TestOpenCreatesNewSessionWithoutHistory(t *testing.T) {
	setupHome(t)

	s, err := Open(context.Background(), Options{NoMCP: true})
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	defer s.Close()

	if s.Restored() != nil {
		t.Fatal("没有历史时不该有 Restored")
	}
	if got := s.Turns(); got != 0 {
		t.Fatalf("新会话轮数应为 0，实际 %d", got)
	}
	if b := s.Banner(); !strings.Contains(b, "test-model") || !strings.Contains(b, "arkperf") {
		t.Fatalf("横幅应含名字与模型：%q", b)
	}
	// 未启用 MCP 时必须如实说"未启用"，而不是假装 0/0
	if got := s.MCPSummary(); got != "MCP 未启用" {
		t.Fatalf("MCPSummary = %q，应为「MCP 未启用」", got)
	}
	if s.NewRunner(nil, kernel.LoopEvents{}) == nil {
		t.Fatal("NewRunner 应返回可用的执行器")
	}
	if s.Registry() == nil || s.Config() == nil {
		t.Fatal("注册表与配置都应可用")
	}
}

// 按工作目录接上最近一段会话；/new（Reset）要真的清掉上下文。
func TestOpenRestoresLatestSessionAndResetClearsIt(t *testing.T) {
	setupHome(t)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败：%v", err)
	}

	// 先造一段已落盘的历史
	store := kernel.NewSessionStore(kernel.Home())
	prev := store.New(cwd)
	prev.Messages = []kernel.Message{
		{Role: "user", Content: "上一轮的问题"},
		{Role: "assistant", Content: "上一轮的回答"},
	}
	if err := store.Save(prev); err != nil {
		t.Fatalf("写入历史会话失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), prev.ID+".json")); err != nil {
		t.Fatalf("会话文件应已落盘：%v", err)
	}

	s, err := Open(context.Background(), Options{NoMCP: true})
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	defer s.Close()

	if s.Restored() == nil {
		t.Fatal("有历史时 Restored 不该为空")
	}
	if got := s.Turns(); got != 1 {
		t.Fatalf("恢复后轮数应为 1，实际 %d", got)
	}
	if s.Restored().ID != prev.ID {
		t.Fatalf("应恢复同一段会话：%s != %s", s.Restored().ID, prev.ID)
	}

	// /new：上下文清空、不再声称恢复
	if err := s.Reset(); err != nil {
		t.Fatalf("Reset 失败：%v", err)
	}
	if s.Restored() != nil {
		t.Fatal("Reset 之后不该还自称已恢复旧会话")
	}
	if got := s.Turns(); got != 0 {
		t.Fatalf("Reset 之后轮数应为 0，实际 %d", got)
	}

	// 保存应写到新建的那段会话，而不是旧的那段
	if err := s.Save(); err != nil {
		t.Fatalf("Save 失败：%v", err)
	}
}
