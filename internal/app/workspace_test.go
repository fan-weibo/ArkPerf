package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// SetWorkspace 必须做对四件事：
// 换 cwd、接上目标目录自己的最近会话、切走前把旧会话落盘、坏输入不动状态。
//
// 为什么这些要一条条锁住：工作区切换是"看起来简单但很容易把用户数据弄丢"
// 的功能——少存一次会话，用户那轮对话就没了；接错会话，模型会拿 A 项目的
// 上下文去改 B 项目。
func TestSetWorkspaceSwitchesAndRestoresTargetSession(t *testing.T) {
	setupHome(t)

	dirA := t.TempDir()
	dirB := t.TempDir()

	// 两个目录各留一段历史
	store := kernel.NewSessionStore(kernel.Home())
	sessA := store.New(dirA)
	sessA.Messages = []kernel.Message{
		{Role: "user", Content: "A 的问题"},
		{Role: "assistant", Content: "A 的回答"},
	}
	if err := store.Save(sessA); err != nil {
		t.Fatalf("写入 A 会话失败：%v", err)
	}
	sessB := store.New(dirB)
	// 必须是完整的一轮（user + assistant）：Conversation 会丢弃"没跑完的半截轮"，
	// 只留一条 user 消息的历史恢复出来是空的——这一点在 run.go 里有同样的约定。
	sessB.Messages = []kernel.Message{
		{Role: "user", Content: "B 的问题"},
		{Role: "assistant", Content: "B 的回答"},
	}
	if err := store.Save(sessB); err != nil {
		t.Fatalf("写入 B 会话失败：%v", err)
	}

	s, err := Open(context.Background(), Options{NoMCP: true, Workspace: dirA})
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	defer s.Close()

	if !samePath(s.CWD(), dirA) {
		t.Fatalf("起始工作区应为 %s，实际 %s", dirA, s.CWD())
	}
	if s.Restored() == nil || !samePath(s.Restored().CWD, dirA) {
		t.Fatal("应接上 A 目录自己的会话")
	}

	// 在 A 里接着说一轮（模拟跑完一次任务），切走后这轮必须已经落盘。
	// 注意必须是**完整的一轮**：半截轮（只有 user）会被 Conversation 丢掉，
	// 那样测的就不是"落盘"而是"半截轮被丢"了。
	s.mu.Lock()
	msgs := append(s.conv.Messages(),
		kernel.Message{Role: "user", Content: "A 这边新说的话"},
		kernel.Message{Role: "assistant", Content: "A 的新回答"},
	)
	s.conv.Restore(msgs)
	s.mu.Unlock()

	if err := s.SetWorkspace(dirB); err != nil {
		t.Fatalf("切到 B 失败：%v", err)
	}
	if !samePath(s.CWD(), dirB) {
		t.Fatalf("cwd 应变成 %s，实际 %s", dirB, s.CWD())
	}
	if s.Restored() == nil || !samePath(s.Restored().CWD, dirB) {
		t.Fatal("应接上 B 目录自己的会话")
	}
	if got := s.Transcript(); len(got) != 2 || got[0].Content != "B 的问题" {
		t.Fatalf("B 的上下文不对（不该带着 A 的对话过来）：%+v", got)
	}

	reloaded, err := store.Load(sessA.ID)
	if err != nil {
		t.Fatalf("A 会话应已保存：%v", err)
	}
	found := false
	for _, m := range reloaded.Messages {
		if m.Content == "A 这边新说的话" {
			found = true
		}
	}
	if !found {
		t.Fatal("切换前的会话没落盘——那段对话丢了")
	}

	// 切到没有历史的目录：应当是干净的新会话
	dirC := t.TempDir()
	if err := s.SetWorkspace(dirC); err != nil {
		t.Fatalf("切到 C 失败：%v", err)
	}
	if s.Restored() != nil {
		t.Fatal("C 目录没有历史，不该声称恢复了会话")
	}
	if got := s.Transcript(); len(got) != 0 {
		t.Fatalf("新会话应当是空的，实际 %d 条", len(got))
	}

	// 坏输入：明确报错，且不改动当前状态
	if err := s.SetWorkspace(filepath.Join(dirC, "并不存在")); err == nil {
		t.Fatal("不存在的目录应当报错")
	}
	if !samePath(s.CWD(), dirC) {
		t.Fatal("失败的切换不该改变当前工作区")
	}

	// 点自己：幂等，不该把当前会话重置掉
	if err := s.SetWorkspace(dirA); err != nil {
		t.Fatalf("切回 A 失败：%v", err)
	}
	before := len(s.Transcript())
	if err := s.SetWorkspace(dirA); err != nil {
		t.Fatalf("重复切 A 失败：%v", err)
	}
	if after := len(s.Transcript()); after != before {
		t.Fatalf("切到当前目录应是空操作，上下文不该变：%d → %d", before, after)
	}
}
