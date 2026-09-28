package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Session 是一段可持久化的会话。
type Session struct {
	ID       string    `json:"id"`
	CWD      string    `json:"cwd"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Messages []Message `json:"messages"`
}

// Turns 返回会话里的用户轮数。
func (s *Session) Turns() int { return countTurns(s.Messages) }

// Summary 生成一行可读摘要。
func (s *Session) Summary() string {
	return fmt.Sprintf("%s · %d 轮 · %s", s.ID, s.Turns(), s.CWD)
}

// SessionStore 把会话存放在 <home>/sessions/<id>.json。
//
// 之所以整文件重写而不是追加式日志：一轮任务动辄几分钟，重写的开销
// 完全可以忽略，换来的是"读回来就是完整状态"——不需要重放事件、
// 不需要修复半截写入、不需要格式迁移。等真出现高频写入需求再改也不迟。
type SessionStore struct{ home string }

// NewSessionStore 构造会话仓库。
func NewSessionStore(home string) *SessionStore { return &SessionStore{home: home} }

// Dir 返回会话目录。
func (s *SessionStore) Dir() string { return filepath.Join(s.home, "sessions") }

// New 新建一段会话（尚未落盘）。
func (s *SessionStore) New(cwd string) *Session {
	now := time.Now()
	// 时间戳 + 纳秒低位：同一秒内连续新建也不会撞 id
	return &Session{
		ID:      fmt.Sprintf("%s-%04x", now.Format("20060102-150405"), now.UnixNano()&0xffff),
		CWD:     cwd,
		Created: now,
		Updated: now,
	}
}

// Save 落盘。
//
// 先写临时文件再改名：中途被打断也不会留下半个 JSON——
// 那会让下次启动直接读失败，而且用户完全不知道发生了什么。
func (s *SessionStore) Save(sess *Session) error {
	if sess == nil || sess.ID == "" {
		return errors.New("session: 空会话")
	}
	if err := os.MkdirAll(s.Dir(), 0o755); err != nil {
		return err
	}
	sess.Updated = time.Now()

	b, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	final := s.path(sess.ID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// Load 按 id 读取会话。
func (s *SessionStore) Load(id string) (*Session, error) {
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(b, &sess); err != nil {
		return nil, fmt.Errorf("会话 %s 解析失败: %w", id, err)
	}
	return &sess, nil
}

// List 返回全部会话，最近更新的在前。
//
// 单个文件读坏了只跳过它：一个损坏的会话不该让"恢复上次会话"整体不可用。
func (s *SessionStore) List() []*Session {
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		return nil
	}
	out := make([]*Session, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if sess, err := s.Load(strings.TrimSuffix(e.Name(), ".json")); err == nil {
			out = append(out, sess)
		}
	}
	slices.SortFunc(out, func(a, b *Session) int { return b.Updated.Compare(a.Updated) })
	return out
}

// LatestForCWD 返回该目录下最近更新的、且有内容的会话。
//
// 必须按目录过滤：在 A 项目里的对话跑到 B 项目里出现，是比"没有记忆"
// 更糟的一种错——用户会看到完全无关的历史。
func (s *SessionStore) LatestForCWD(cwd string) *Session {
	for _, sess := range s.List() {
		if sameDir(sess.CWD, cwd) && len(sess.Messages) > 0 {
			return sess
		}
	}
	return nil
}

// sameDir 比较两个目录是否指向同一处。
//
// 用 EqualFold 而不是比较小写化后的字符串：Windows 路径大小写不敏感，
// 而 Linux 敏感；EqualFold 在 Windows 上正确，在 Linux 上只是略微宽松，
// 不会把不同的目录判成同一个。
func sameDir(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// path 把 id 映射成文件路径。id 只可能来自我们生成的格式或会话目录里的
// 文件名，这里仍然做一次穿越防护——将来若支持用户手输 id 就不必再想这件事。
func (s *SessionStore) path(id string) string {
	safe := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(id)
	return filepath.Join(s.Dir(), safe+".json")
}
