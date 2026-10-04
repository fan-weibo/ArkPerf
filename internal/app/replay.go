package app

import (
	"fmt"
	"sort"
	"strings"
)

// TranscriptLine 是回放用的一行，与前端无关。
//
// 工具调用也在内：只回放 user/assistant 的话，"切个会话回来工具调用全没了"，
// 用户看到的是残缺的对话，也无法理解当时的结论是怎么来的（实测被指出）。
type TranscriptLine struct {
	Role    string // user | assistant | tool | result
	Content string
	// Name 只对 tool / result 有意义：前者是参数串，后者是工具名。
	// result 的工具名要从前面最近一条带 tool_calls 的 assistant 里反查，
	// 因为 tool 消息本身只存了 tool_call_id。
	Name string
	// IsError 标记这是一次失败的工具结果。
	IsError bool
}

// TranscriptLines 把当前会话压成可回放的行。
//
// 顺序与实时观看时一致：assistant 文字 → 工具调用 → 工具结果。
// 文字必须排在工具行**之前**：前端会把连续的 tool/result 行聚成可折叠块，
// 文字夹在中间会把同一轮的调用切成两截。
func (s *Session) TranscriptLines() []TranscriptLine {
	msgs := s.Transcript()
	out := make([]TranscriptLine, 0, len(msgs))
	names := make(map[string]string, len(msgs)) // tool_call_id → 工具名
	for _, m := range msgs {
		switch m.Role {
		case "user":
			out = append(out, TranscriptLine{Role: "user", Content: m.Content})
		case "assistant":
			// 纯工具调用的 assistant 没有文字可显示，跳过，
			// 否则界面上会出现一个只有标题的空节点
			if strings.TrimSpace(m.Content) != "" {
				out = append(out, TranscriptLine{Role: "assistant", Content: m.Content})
			}
			for _, c := range m.ToolCalls {
				names[c.ID] = c.Function.Name
				out = append(out, TranscriptLine{Role: "tool", Name: c.Function.Name, Content: c.Function.Arguments})
			}
		case "tool":
			name := names[m.ToolCallID]
			if name == "" {
				name = m.ToolCallID
			}
			// 失败标记从正文前缀恢复：Message 里没有独立的错误位，
			// 而 kernel.caption() 对失败结果固定写 "ERROR: "（见 loop.go）。
			out = append(out, TranscriptLine{
				Role:    "result",
				Name:    name,
				Content: m.Content,
				IsError: strings.HasPrefix(m.Content, "ERROR: "),
			})
		}
	}
	return out
}

// SessionBrief 是会话列表里的一项。
type SessionBrief struct {
	ID      string
	Title   string
	Turns   int
	Updated string
	Current bool
}

// WorkspaceSessions 列出**当前工作区**的会话。
//
// 只列当前工作区：切到别的工作区的会话要连工作区一起切走，那是 OpenSession 的事；
// 列表里混着别目录的会话，只会让人分不清"选它会不会把工作区也切了"。
//
// 排序按**创建时间倒序**（稳定）。不能按 Updated：切一次会话就会 Save 一次，
// 次序跟着变，用户刚点过的条目会在他眼皮底下换位置——桌面端已经踩过这个坑。
func (s *Session) WorkspaceSessions() []SessionBrief {
	cur := s.CurrentID()
	cwd := s.CWD()

	sorted := s.Sessions()
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Created.After(sorted[j].Created) })

	out := make([]SessionBrief, 0, len(sorted))
	for _, x := range sorted {
		if !samePath(x.CWD, cwd) || len(x.Messages) == 0 {
			continue
		}
		out = append(out, SessionBrief{
			ID:      x.ID,
			Title:   x.DisplayName(),
			Turns:   x.Turns(),
			Updated: x.Updated.Format("01-02 15:04"),
			Current: x.ID == cur,
		})
	}
	return out
}

// OpenSession 切到指定会话（不属于当前工作区时，连工作区一起切过去）。
//
// 返回切换后的工作目录：前端要拿它更新状态行——否则屏幕上写着旧目录、
// 模型却在另一个目录里干活，这是最危险的一种错位。
func (s *Session) OpenSession(id string) (string, error) {
	var cwd string
	found := false
	for _, x := range s.Sessions() {
		if x.ID == id {
			cwd, found = x.CWD, true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("找不到会话：%s", id)
	}

	if !samePath(cwd, s.CWD()) {
		if err := s.SetWorkspace(cwd); err != nil {
			return "", err
		}
	}
	if err := s.LoadByID(id); err != nil {
		return "", err
	}
	return s.CWD(), nil
}

// NewSession 开始新会话：清空上下文并新建持久化记录。
//
// 与 Reset 的差别只在于**先把当前会话落盘**。Reset 依赖"每轮任务结束都会 Save"，
// 所以平常是安全的；但用户可能刚切过工作区或刚恢复过历史，
// 这时内存里的内容未必和磁盘一致——显式存一次，成本极低。
func (s *Session) NewSession() error {
	if err := s.Save(); err != nil {
		return err
	}
	return s.Reset()
}
