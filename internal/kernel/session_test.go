package kernel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionStoreRoundTrip(t *testing.T) {
	store := NewSessionStore(t.TempDir())

	sess := store.New(`E:\proj\a`)
	sess.Messages = []Message{
		msg("system", "系统提示"),
		msg("user", "分析一下启动耗时"),
		msg("assistant", "冷启动中位数 320ms"),
	}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CWD != sess.CWD {
		t.Fatalf("工作目录丢了：%q", loaded.CWD)
	}
	if len(loaded.Messages) != 3 || loaded.Messages[1].Content != "分析一下启动耗时" {
		t.Fatalf("消息丢了：%+v", loaded.Messages)
	}
	if loaded.Turns() != 1 {
		t.Fatalf("轮数：%d", loaded.Turns())
	}
}

// 保存必须是原子的：中途失败不能留下半个 JSON 让下次启动直接读不动。
func TestSessionStoreSaveLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	store := NewSessionStore(dir)

	sess := store.New(`E:\proj\a`)
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("残留临时文件：%s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("应只有一个会话文件，实际 %d 个", len(entries))
	}
}

// 同一目录多次保存应覆盖同一个文件，而不是每次新建。
func TestSessionStoreSaveOverwritesSameSession(t *testing.T) {
	store := NewSessionStore(t.TempDir())
	sess := store.New(`E:\proj\a`)

	sess.Messages = []Message{msg("user", "q1"), msg("assistant", "a1")}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	sess.Messages = append(sess.Messages, msg("user", "q2"), msg("assistant", "a2"))
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	list := store.List()
	if len(list) != 1 {
		t.Fatalf("同一会话应只有一个文件，实际 %d", len(list))
	}
	if list[0].Turns() != 2 {
		t.Fatalf("应保存最新状态：%d 轮", list[0].Turns())
	}
}

// 按目录隔离：在 A 项目里的对话绝不能出现在 B 项目里——
// 那比"没有记忆"更糟，用户会看到完全无关的历史。
func TestSessionStoreLatestForCWDIsolatesProjects(t *testing.T) {
	store := NewSessionStore(t.TempDir())

	a := store.New(`E:\proj\a`)
	a.Messages = []Message{msg("user", "A 项目的问题"), msg("assistant", "A 的回答")}
	if err := store.Save(a); err != nil {
		t.Fatal(err)
	}

	b := store.New(`E:\proj\b`)
	b.Messages = []Message{msg("user", "B 项目的问题"), msg("assistant", "B 的回答")}
	if err := store.Save(b); err != nil {
		t.Fatal(err)
	}

	got := store.LatestForCWD(`E:\proj\a`)
	if got == nil || got.ID != a.ID {
		t.Fatalf("应取回 A 项目的会话：%+v", got)
	}
	if got := store.LatestForCWD(`E:\proj\c`); got != nil {
		t.Fatalf("没有会话的目录不该返回任何东西：%+v", got)
	}
}

// Windows 路径大小写不敏感；同一目录的不同写法必须认成同一个。
func TestSessionStoreSameDirIsCaseInsensitive(t *testing.T) {
	store := NewSessionStore(t.TempDir())
	sess := store.New(`E:\Proj\A`)
	sess.Messages = []Message{msg("user", "q"), msg("assistant", "a")}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	if got := store.LatestForCWD(`e:\proj\a`); got == nil {
		t.Fatal("大小写不同的同一目录应能恢复")
	}
}

// 空会话不值得恢复：恢复了也只是把用户带回一片空白。
func TestSessionStoreSkipsEmptySessions(t *testing.T) {
	store := NewSessionStore(t.TempDir())
	sess := store.New(`E:\proj\a`) // 没有任何消息
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	if got := store.LatestForCWD(`E:\proj\a`); got != nil {
		t.Fatalf("空会话不该被当成可恢复的：%+v", got)
	}
}

// 一个坏文件不该让"恢复上次会话"整体不可用。
func TestSessionStoreSkipsCorruptFile(t *testing.T) {
	store := NewSessionStore(t.TempDir())
	if err := os.MkdirAll(store.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	good := store.New(`E:\proj\a`)
	good.Messages = []Message{msg("user", "q"), msg("assistant", "a")}
	if err := store.Save(good); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), "broken.json"), []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := store.LatestForCWD(`E:\proj\a`); got == nil || got.ID != good.ID {
		t.Fatalf("坏文件不该影响好文件：%+v", got)
	}
}

// id 里的路径分隔符必须被消掉：将来若允许用户手输 id，这就是目录穿越。
func TestSessionStoreIDCannotEscape(t *testing.T) {
	store := NewSessionStore(t.TempDir())
	if got := store.path(`..\..\evil`); filepath.Dir(got) != store.Dir() {
		t.Fatalf("id 逃出了会话目录：%s", got)
	}
	if got := store.path("a/b"); filepath.Dir(got) != store.Dir() {
		t.Fatalf("id 逃出了会话目录：%s", got)
	}
}
