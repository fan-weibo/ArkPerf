package kernel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	// Title 是用户自己起的名字，可空。
	//
	// 空时显示名回退到"第一条用户消息的首行"（见 app 层的 sessionTitle）。
	// 不做成单独的索引文件而放进会话本身：改名跟着会话走，
	// 删掉会话名字自然也没了，不会留下指向已删除会话的悬空条目。
	Title string `json:"title,omitzero"`
}

// Rename 给会话起名。空串表示清除自定义名（回到自动标题）。
func (s *Session) Rename(title string) {
	s.Title = strings.TrimSpace(title)
	if s.Title != "" {
		s.Updated = time.Now()
	}
}

// DisplayName 返回侧栏要显示的标题：用户起过的名字优先，
// 否则回退到第一条用户消息的首行（截 40 字）。
//
// 放在内核而不是某个前端：三个前端都要显示它，放一处才不会出现
// "桌面端改了名、TUI 还显示旧标题"这种分叉。
func (s *Session) DisplayName() string {
	if t := strings.TrimSpace(s.Title); t != "" {
		return t
	}
	for _, m := range s.Messages {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			t := strings.SplitN(strings.TrimSpace(m.Content), "\n", 2)[0]
			if r := []rune(t); len(r) > 40 {
				t = string(r[:40]) + "…"
			}
			return t
		}
	}
	return "（空会话）"
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
//
// ID = 秒级时间戳 + **4 字节随机**十六进制。
//
// 后缀不能用时间的低位：老实现是 `UnixNano()&0xffff`（只有 16 位），
// 注释还写着"同一秒内连续新建也不会撞 id"——那句是错的。Windows 时钟精度
// 是毫秒级的，同一秒内两次创建很容易落在相同的低位上；一旦撞 id，
// 后一次 Save 会**覆盖**前一段会话，用户静默丢掉整段对话
// （实测：在测试里 3 次能复现 1 次，生产路径上 `/new` 后紧接着切工作区同样中招）。
//
// 随机后缀顺带解决另一个隐患：ID 不该能从创建时间推出来。
func (s *SessionStore) New(cwd string) *Session {
	now := time.Now()
	return &Session{
		ID:      fmt.Sprintf("%s-%s", now.Format("20060102-150405"), randomSuffix()),
		CWD:     cwd,
		Created: now,
		Updated: now,
	}
}

// randomSuffix 返回 4 字节随机数的十六进制（8 位）。
//
// 取不到随机源时退回纳秒低位——降级之后仍然能用，
// 但不能假装它足够安全（32 位随机撞的概率约 1/43 亿，16 位则高得多）。
func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
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

// Delete 从磁盘上删掉一个会话。
//
// 只删会话文件本身：它所在的目录、目录里的其他会话都不动。
// "从列表中移除"指的是"我不再关心这段对话"，不是"把工程删了"。
func (s *SessionStore) Delete(id string) error {
	err := os.Remove(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil // 已经不在了：幂等，调用方不必区分
	}
	return err
}

// path 把 id 映射成文件路径。id 只可能来自我们生成的格式或会话目录里的
// 文件名，这里仍然做一次穿越防护——将来若支持用户手输 id 就不必再想这件事。
func (s *SessionStore) path(id string) string {
	safe := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(id)
	return filepath.Join(s.Dir(), safe+".json")
}
