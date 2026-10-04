package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// openSession 造一个已打开的会话（工作目录固定在临时目录）。
func openSession(t *testing.T) *Session {
	t.Helper()
	setupHome(t)
	s, err := Open(context.Background(), Options{NoMCP: true})
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// currentOf 取当前会话（用现有接口拼出来，不给 Session 加新的对外口）。
func currentOf(t *testing.T, s *Session) *kernel.Session {
	t.Helper()
	id := s.CurrentID()
	for _, x := range s.Sessions() {
		if x.ID == id {
			return x
		}
	}
	t.Fatalf("找不到当前会话 %s", id)
	return nil
}

// seedSession 在磁盘上造一段有内容的历史会话，返回它的 ID。
func seedSession(t *testing.T, cwd, firstMsg string) string {
	t.Helper()
	store := kernel.NewSessionStore(kernel.Home())
	sess := store.New(cwd)
	sess.Messages = []kernel.Message{
		{Role: "user", Content: firstMsg},
		{Role: "assistant", Content: "好的"},
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("写历史会话失败：%v", err)
	}
	return sess.ID
}

// ---------------------------------------------------------------- 重命名

// 标题的取值顺序：用户起过的名字 > 第一条提问。
// 侧栏"重命名"依赖这个顺序，否则改了名却还显示旧标题。
func TestSessionTitlePrefersCustomName(t *testing.T) {
	sess := &kernel.Session{ID: "s1", Messages: []kernel.Message{
		{Role: "user", Content: "第一条提问"},
	}}

	// 没起名时回退到第一条提问
	if got := sess.DisplayName(); got != "第一条提问" {
		t.Fatalf("自动标题 = %q，期望 第一条提问", got)
	}

	sess.Title = "我的实验"
	if got := sess.DisplayName(); got != "我的实验" {
		t.Fatalf("自定义名应当优先，实际 %q", got)
	}
}

func TestRenameSessionPersistsAndAffectsCurrent(t *testing.T) {
	s := openSession(t)
	id := seedSession(t, s.CWD(), "第一条提问")

	// 先切过去，让它成为"当前会话"：改名必须同时改到内存里这份
	if err := s.LoadByID(id); err != nil {
		t.Fatalf("切换会话失败：%v", err)
	}
	if err := s.RenameSession(id, "冷启动基线"); err != nil {
		t.Fatalf("重命名失败：%v", err)
	}

	// 内存里那份要立刻跟上：Save 会把内存这份写回磁盘，
	// 不跟上的话，改名会被下一轮保存覆盖回去（表现为"改了又变回去"）
	if got := currentOf(t, s).DisplayName(); got != "冷启动基线" {
		t.Fatalf("当前会话的标题应当更新，实际 %q", got)
	}

	// 落盘了：重开一个会话读取也能看到
	reloaded, err := kernel.NewSessionStore(kernel.Home()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.DisplayName(); got != "冷启动基线" {
		t.Fatalf("改名应当已落盘，实际 %q", got)
	}
}

// 空标题表示"清除自定义名"，回到自动标题——侧栏需要一个撤回改名的办法。
func TestRenameSessionWithEmptyRestoresAutoTitle(t *testing.T) {
	s := openSession(t)
	id := seedSession(t, s.CWD(), "自动标题来源")

	if err := s.RenameSession(id, "随便起的名"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameSession(id, "   "); err != nil {
		t.Fatalf("空标题应当被接受（清除自定义名）：%v", err)
	}

	reloaded, err := kernel.NewSessionStore(kernel.Home()).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.DisplayName(); got != "自动标题来源" {
		t.Fatalf("清除后应当回到自动标题，实际 %q", got)
	}
}

func TestRenameSessionRejectsUnknownID(t *testing.T) {
	s := openSession(t)
	if err := s.RenameSession("no-such-id", "x"); err == nil {
		t.Fatal("不存在的会话应当报错")
	}
}

// ---------------------------------------------------------------- 移除

func TestDeleteSessionRemovesFileOnly(t *testing.T) {
	s := openSession(t)
	dir := s.CWD()
	a := seedSession(t, dir, "要删的会话")
	b := seedSession(t, dir, "要留的会话")
	store := kernel.NewSessionStore(kernel.Home())

	if err := s.DeleteSession(a); err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), a+".json")); !os.IsNotExist(err) {
		t.Fatalf("会话文件应当被删掉，err=%v", err)
	}
	// 工作目录必须原封不动："从列表中移除"指的是"不关心这段对话"，
	// 不是"把工程删了"
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("工作目录不该被动过：err=%v", err)
	}
	// 其他会话不受影响
	if _, err := store.Load(b); err != nil {
		t.Fatalf("其他会话不该被波及：%v", err)
	}
}

// 删掉当前正在看的会话时，必须自动切到该目录下最近的一条——
// 否则界面停在一个不存在的会话上，下一轮任务写不进任何文件。
func TestDeleteCurrentSessionFallsBackToLatest(t *testing.T) {
	s := openSession(t)
	dir := s.CWD()
	older := seedSession(t, dir, "较早的会话")
	newer := seedSession(t, dir, "较新的会话")

	if err := s.LoadByID(older); err != nil {
		t.Fatalf("切到较早会话失败：%v", err)
	}
	if err := s.DeleteSession(older); err != nil {
		t.Fatal(err)
	}

	// LatestForCWD 在同目录里找：只剩较新那条
	cur := currentOf(t, s)
	if cur.ID != newer {
		t.Fatalf("应当切到该目录下最近的一条，实际 %s", cur.ID)
	}
	if got := cur.DisplayName(); got != "较新的会话" {
		t.Fatalf("切过去的会话内容应当可用，实际标题 %q", got)
	}
}

// 删到一条都不剩：开新会话，而不是停在空状态。
// （空会话与 /new 一样，只有真正产生内容后才落盘——这是既有约定，
//
//	所以这里不断言"新会话有文件"。）
func TestDeleteLastSessionStartsFresh(t *testing.T) {
	s := openSession(t)
	id := seedSession(t, s.CWD(), "唯一的会话")

	if err := s.DeleteSession(id); err != nil {
		t.Fatal(err)
	}
	if s.CurrentID() == id {
		t.Fatal("应当自动换到一个新会话上，而不是停在已删除的那个")
	}
	// 侧栏列表里不再有它
	for _, x := range s.Sessions() {
		if x.ID == id {
			t.Fatal("列表里不该还有已删除的会话")
		}
	}
}

// 删一个不存在的会话要幂等：列表是异步刷新的，
// "菜单开着的时候它已经被别处删掉"不该报错。
func TestDeleteSessionIsIdempotent(t *testing.T) {
	s := openSession(t)
	if err := s.DeleteSession("no-such-id"); err != nil {
		t.Fatalf("删除不存在的会话不该报错：%v", err)
	}
	if strings.TrimSpace(s.CurrentID()) == "" {
		t.Fatal("当前会话不该被动掉")
	}
}
