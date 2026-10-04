package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// ApprovalDecision 是用户对一次审批的回答。
type ApprovalDecision int

const (
	// ApprovalDeny 拒绝。安全缺省：前端认不出来的回答一律按它处理。
	ApprovalDeny ApprovalDecision = iota
	// ApprovalOnce 只放行这一次。
	ApprovalOnce
	// ApprovalAlways 这一类以后都别再问。
	//
	// 只对**能划出类别**的调用有意义：scope 为空时它退化成 ApprovalOnce。
	// 前端在没有 scope 时不该给出这个选项——那会让人以为记住了，
	// 下次却被再问一遍。
	ApprovalAlways
)

// Granted 返回是否放行。
func (d ApprovalDecision) Granted() bool { return d != ApprovalDeny }

// Approvable 是工具的可选扩展：把一次需要审批的调用归成一个"可记忆的类别"。
//
// 只有实现了它的工具才可能被"以后别再问我"。**分类必须保守**：
// 一个类别太宽，等于把审批关掉——而入口分类是 ArkPerf 唯一的审批依据
// （见 guard.go 顶部对"分级决定要不要问、护栏决定问不问都不许做"的说明）。
// 所以宁可返回空串（意思是"这次值得问、但不值得记"），也不要给一个宽类别。
type Approvable interface {
	// ApprovalScope 返回这次调用的类别标识。空串表示不可记忆。
	//
	// cwd 是本次任务的工作目录：相对路径必须按它解析，否则同一份规则
	// 在换个目录启动后会算出另一个类别，表现为"规则时灵时不灵"。
	ApprovalScope(args map[string]any, cwd string) string
}

// ApprovalRuleNote 说明一次审批规则的来龙去脉，供前端如实告知用户。
//
// 三种情况都要能区分出来，因为它们对用户的含义完全不同：
//   - Hit：命中了已有规则，所以**这次没问你**；
//   - Saved：你刚选了"以后别再问"，规则已写盘；
//   - Err：规则没写进磁盘，**但这次已经放行了**——不说的话，用户会以为
//     已经生效，下次却被再问一遍，只会觉得"这功能坏了"。
type ApprovalRuleNote struct {
	Name  string
	Scope string
	Hit   bool
	Saved bool
	Err   error
}

// ApprovalRule 是一条"以后别再问"的记忆。
type ApprovalRule struct {
	Tool  string    `json:"tool"`
	Scope string    `json:"scope"`
	Added time.Time `json:"added"`
}

// rulesFileVersion 是落盘格式的版本。
//
// 带上它是为了将来能改格式：读到不认识的版本宁可报"读不出来"，
// 也不要按新格式去解释旧文件——那会把用户的规则读成别的意思，
// 表现为"我没加过这条规则，它怎么直接放行了"。
const rulesFileVersion = 1

type rulesFile struct {
	Version int            `json:"version"`
	Rules   []ApprovalRule `json:"rules"`
}

// ApprovalRules 是落盘的审批记忆：tool -> scope -> 加进来的时间。
type ApprovalRules struct {
	path  string
	mu    sync.Mutex
	rules map[string]map[string]time.Time
}

// ApprovalRulesPath 返回审批规则文件的位置（与配置、会话同在状态根下）。
func ApprovalRulesPath() string { return filepath.Join(Home(), "approved-rules.json") }

// LoadApprovalRules 读规则文件。
//
// 文件不存在不算错（绝大多数用户不会用到这个功能）。
// 但**读坏了必须报错**而不是当成空规则：后者会让下一次写入把用户原来的
// 规则整份覆盖掉，而用户完全不知道。
func LoadApprovalRules(path string) (*ApprovalRules, error) {
	r := &ApprovalRules{path: path, rules: make(map[string]map[string]time.Time)}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读审批规则 %s: %w", path, err)
	}

	var f rulesFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("审批规则 %s 解析失败（修好或删掉它再试）: %w", path, err)
	}
	if f.Version != rulesFileVersion {
		return nil, fmt.Errorf("审批规则 %s 的版本是 %d，本版本只认 %d（删掉它可重新开始）",
			path, f.Version, rulesFileVersion)
	}
	for _, rule := range f.Rules {
		// 缺 tool 或 scope 的条目是坏数据：记着它没有意义，而且会让
		// "这条规则是怎么来的"永远查不清。
		if strings.TrimSpace(rule.Tool) == "" || strings.TrimSpace(rule.Scope) == "" {
			continue
		}
		if r.rules[rule.Tool] == nil {
			r.rules[rule.Tool] = make(map[string]time.Time)
		}
		r.rules[rule.Tool][rule.Scope] = rule.Added
	}
	return r, nil
}

// Allowed 判断某个工具的这个类别是否已被记住。
func (r *ApprovalRules) Allowed(tool, scope string) bool {
	if r == nil || scope == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.rules[tool][scope]
	return ok
}

// Remember 记住"这个工具的这个类别以后别再问"，并立刻落盘。
//
// 立即落盘而不是退出时统一写：用户选这一项的心智是"从现在起别再问了"，
// 万一程序随后崩了或被 Ctrl+C 掉，规则却丢了，他会更不信任这个功能。
func (r *ApprovalRules) Remember(tool, scope string) error {
	if r == nil {
		return errors.New("审批规则不可用")
	}
	if strings.TrimSpace(tool) == "" || strings.TrimSpace(scope) == "" {
		return errors.New("tool 与 scope 都不能为空")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.rules[tool] == nil {
		r.rules[tool] = make(map[string]time.Time)
	}
	if _, ok := r.rules[tool][scope]; ok {
		return nil // 已经有了，不必重写文件
	}
	r.rules[tool][scope] = time.Now()
	return r.writeLocked()
}

// List 返回全部规则，按工具名与类别排序（输出稳定，便于展示与测试）。
func (r *ApprovalRules) List() []ApprovalRule {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]ApprovalRule, 0, len(r.rules))
	for tool, scopes := range r.rules {
		for scope, added := range scopes {
			out = append(out, ApprovalRule{Tool: tool, Scope: scope, Added: added})
		}
	}
	slicesSortRules(out)
	return out
}

// Path 返回规则文件位置（展示用）。
func (r *ApprovalRules) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

// writeLocked 落盘。调用方必须已持锁。
//
// 先写临时文件再改名：中途被打断也不会留下半个 JSON——
// 那会让下次启动直接读失败，而用户完全不知道发生了什么。
func (r *ApprovalRules) writeLocked() error {
	f := rulesFile{Version: rulesFileVersion}
	for tool, scopes := range r.rules {
		for scope, added := range scopes {
			f.Rules = append(f.Rules, ApprovalRule{Tool: tool, Scope: scope, Added: added})
		}
	}
	slicesSortRules(f.Rules)

	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// slicesSortRules 按 tool 再按 scope 排序。抽出来是为了 List 与落盘用同一个顺序，
// 否则文件里的顺序会随 map 遍历随机变化，每次写盘都产生无意义的 diff。
func slicesSortRules(rules []ApprovalRule) {
	slices.SortFunc(rules, func(a, b ApprovalRule) int {
		if c := strings.Compare(a.Tool, b.Tool); c != 0 {
			return c
		}
		return strings.Compare(a.Scope, b.Scope)
	})
}

// ApprovalScope 问工具要这次调用的类别标识。没实现 Approvable 的工具返回空串。
//
// 导出是为了让绕开循环、直接调工具的路径（`arkperf tool`）也能得到
// **同一个类别**：两条路径算出来的类别不一致的话，规则会"时灵时不灵"。
func ApprovalScope(t Tool, args map[string]any, cwd string) string {
	a, ok := t.(Approvable)
	if !ok {
		return ""
	}
	return strings.TrimSpace(a.ApprovalScope(args, cwd))
}
