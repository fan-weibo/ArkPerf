package kernel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scopedTool 是一个"有记忆类别"的需要审批工具。
type scopedTool struct {
	fakeTool
	scope   string
	execErr error
}

func (s scopedTool) ApprovalScope(map[string]any, string) string { return s.scope }

func (s scopedTool) Execute(ctx context.Context, args map[string]any, tc ToolCtx) (ToolResult, error) {
	return s.fakeTool.Execute(ctx, args, tc)
}

// plainApprovalTool 是需要审批但**没有类别**的工具（模拟静态审批的域工具）。
type plainApprovalTool struct{ fakeTool }

// ---------------------------------------------------------------- 规则的存取

func TestApprovalRulesRememberAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "approved-rules.json")
	r, err := LoadApprovalRules(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.Allowed("run_command", "git status") {
		t.Fatal("空规则不该放行任何东西")
	}
	if err := r.Remember("run_command", "git status"); err != nil {
		t.Fatal(err)
	}
	// 立刻落盘：用户点这一项的心智是"从现在起别再问了"，
	// 万一随后程序被 Ctrl+C，规则丢了会让他更不信任这个功能。
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("应当立刻写盘：%v", err)
	}

	again, err := LoadApprovalRules(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Allowed("run_command", "git status") {
		t.Fatal("重载后应当仍然命中")
	}
}

// 规则必须**同时**匹配工具与类别：只匹配其中一个是危险的宽放行。
func TestApprovalRulesMatchToolAndScopeTogether(t *testing.T) {
	r := &ApprovalRules{path: filepath.Join(t.TempDir(), "r.json"), rules: map[string]map[string]time.Time{}}
	if err := r.Remember("run_command", "git status"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		tool, scope string
		want        bool
	}{
		{"run_command", "git status", true},
		{"run_command", "git push", false},
		{"run_command", "", false},
		{"harmony_shell", "git status", false},
		{"", "git status", false},
	}
	for _, tc := range cases {
		if got := r.Allowed(tc.tool, tc.scope); got != tc.want {
			t.Errorf("Allowed(%q, %q) = %v，期望 %v", tc.tool, tc.scope, got, tc.want)
		}
	}
}

func TestApprovalRulesRejectEmptyScope(t *testing.T) {
	r := &ApprovalRules{path: filepath.Join(t.TempDir(), "r.json"), rules: map[string]map[string]time.Time{}}
	// 空类别意味着"这次没有可记忆的类别"，绝不能存成一条万能规则
	if err := r.Remember("edit_file", ""); err == nil {
		t.Fatal("空类别应当被拒")
	}
	if err := r.Remember("", "x"); err == nil {
		t.Fatal("空工具名应当被拒")
	}
}

func TestApprovalRulesRememberIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	r, _ := LoadApprovalRules(path)
	for range 3 {
		if err := r.Remember("edit_file", "E:\\proj\\src"); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(r.List()); got != 1 {
		t.Fatalf("重复记住应当只有一条，实际 %d", got)
	}
}

// 落盘顺序必须稳定：否则每次写盘都产生无意义的 diff。
func TestApprovalRulesFileOrderIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	r, _ := LoadApprovalRules(path)
	for _, s := range []string{"z", "a", "m"} {
		if err := r.Remember("t", s); err != nil {
			t.Fatal(err)
		}
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Remember("t", "b"); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)

	var f1, f2 rulesFile
	_ = json.Unmarshal(first, &f1)
	_ = json.Unmarshal(second, &f2)
	// 已有的三条必须保持同样的相对顺序（新加的那条按字典序插进去）
	var got []string
	for _, rule := range f2.Rules {
		got = append(got, rule.Scope)
	}
	if strings.Join(got, ",") != "a,b,m,z" {
		t.Fatalf("落盘顺序应当稳定有序，实际 %v", got)
	}
}

func TestLoadApprovalRulesMissingFileIsEmpty(t *testing.T) {
	r, err := LoadApprovalRules(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("文件不存在不该报错：%v", err)
	}
	if len(r.List()) != 0 {
		t.Fatal("应当是空规则集")
	}
}

// 读坏了必须**报错**而不是当成空规则：后者会让下一次写入
// 把用户原来的规则整份覆盖掉，而他完全不知道。
func TestLoadApprovalRulesCorruptFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	if err := os.WriteFile(path, []byte("{ 这不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApprovalRules(path); err == nil {
		t.Fatal("坏文件应当报错")
	}
	// 关键：报错之后原文件必须原封不动
	if b, _ := os.ReadFile(path); string(b) != "{ 这不是 JSON" {
		t.Fatalf("原文件不该被动过：%q", b)
	}
}

// 不认识的版本宁可报"读不出来"：按新格式解释旧文件会把规则读成别的意思，
// 表现为"我没加过这条规则，它怎么直接放行了"。
func TestLoadApprovalRulesRejectsUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	body := `{"version":99,"rules":[{"tool":"run_command","scope":"git status"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadApprovalRules(path)
	if err == nil {
		t.Fatal("不认识的版本应当报错")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Fatalf("错误里应当说明读到的是哪个版本：%v", err)
	}
}

func TestLoadApprovalRulesSkipsIncompleteEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	body := `{"version":1,"rules":[
	  {"tool":"run_command","scope":"git status"},
	  {"tool":"","scope":"x"},
	  {"tool":"edit_file","scope":""}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadApprovalRules(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(r.List()); got != 1 {
		t.Fatalf("缺字段的条目应当被跳过，实际留下 %d 条", got)
	}
}

// ---------------------------------------------------------------- 循环的行为

// scopedLoopConfig 造一个只有 scopedTool 的最小配置。
func scopedLoopConfig(t *testing.T, tool Tool, ap Approver, rules *ApprovalRules, ch *scriptedChat, notes *[]ApprovalRuleNote) LoopConfig {
	t.Helper()
	events := LoopEvents{}
	if notes != nil {
		events.OnApprovalRule = func(n ApprovalRuleNote) { *notes = append(*notes, n) }
	}
	reg := NewRegistry()
	reg.Register(tool)
	return LoopConfig{
		Registry: reg,
		Ctx:      ToolCtx{CWD: `E:\proj`},
		Chat:     ch.chat,
		Approval: "ask",
		Approver: ap,
		Rules:    rules,
		Events:   events,
	}
}

func TestLoopAsksWhenNoRuleSaved(t *testing.T) {
	ch := &scriptedChat{t: t, steps: []ChatResponse{
		assistantToolCall("c1", "scoped", `{}`),
		assistantText("done"),
	}}
	ap := &yesApprover{}
	rules, _ := LoadApprovalRules(filepath.Join(t.TempDir(), "r.json"))

	if _, err := RunLoop(t.Context(), scopedLoopConfig(t, scopedTool{fakeTool: fakeTool{name: "scoped", approval: true}, scope: "git status"}, ap, rules, ch, nil), "任务"); err != nil {
		t.Fatal(err)
	}
	if ap.asked != 1 {
		t.Fatalf("没有规则时应当问一次，实际 %d", ap.asked)
	}
}

// 命中规则就不问，但**必须说出来**：用户看到工具直接跑了却没被问，
// 唯一能解释它的就是这条通知。
func TestLoopSkipsAskWhenRuleExistsAndSaysSo(t *testing.T) {
	ch := &scriptedChat{t: t, steps: []ChatResponse{
		assistantToolCall("c1", "scoped", `{}`),
		assistantText("done"),
	}}
	ap := &yesApprover{}
	rules, _ := LoadApprovalRules(filepath.Join(t.TempDir(), "r.json"))
	if err := rules.Remember("scoped", "git status"); err != nil {
		t.Fatal(err)
	}

	var notes []ApprovalRuleNote
	cfg := scopedLoopConfig(t, scopedTool{fakeTool: fakeTool{name: "scoped", approval: true}, scope: "git status"}, ap, rules, ch, &notes)
	if _, err := RunLoop(t.Context(), cfg, "任务"); err != nil {
		t.Fatal(err)
	}

	if ap.asked != 0 {
		t.Fatalf("命中规则不该再问，实际问了 %d 次", ap.asked)
	}
	if len(notes) != 1 || !notes[0].Hit {
		t.Fatalf("应当有一条「命中规则」的通知：%+v", notes)
	}
}

// 用户选了"以后都不问"之后，第二次必须不再问。
func TestLoopRemembersAlwaysAndStopsAsking(t *testing.T) {
	rules, _ := LoadApprovalRules(filepath.Join(t.TempDir(), "r.json"))
	ap := &alwaysApprover{}
	tool := scopedTool{fakeTool: fakeTool{name: "scoped", approval: true}, scope: "git status"}

	var notes []ApprovalRuleNote
	for range 2 {
		ch := &scriptedChat{t: t, steps: []ChatResponse{
			assistantToolCall("c1", "scoped", `{}`),
			assistantText("done"),
		}}
		if _, err := RunLoop(t.Context(), scopedLoopConfig(t, tool, ap, rules, ch, &notes), "任务"); err != nil {
			t.Fatal(err)
		}
	}

	if ap.asked != 1 {
		t.Fatalf("第二次不该再问，实际问了 %d 次", ap.asked)
	}
	if !rules.Allowed("scoped", "git status") {
		t.Fatal("规则应当已被记住")
	}
	var saved bool
	for _, n := range notes {
		if n.Saved {
			saved = true
		}
	}
	if !saved {
		t.Fatalf("应当有一条「已记住」的通知：%+v", notes)
	}
}

// 没有类别的工具即使被选"总是允许"也只放行这一次，并且**不留规则**。
func TestLoopDoesNotRememberWhenNoScope(t *testing.T) {
	rules, _ := LoadApprovalRules(filepath.Join(t.TempDir(), "r.json"))
	ap := &alwaysApprover{}
	tool := plainApprovalTool{fakeTool{name: "plain", approval: true}}

	var notes []ApprovalRuleNote
	for range 2 {
		ch := &scriptedChat{t: t, steps: []ChatResponse{
			assistantToolCall("c1", "plain", `{}`),
			assistantText("done"),
		}}
		if _, err := RunLoop(t.Context(), scopedLoopConfig(t, tool, ap, rules, ch, &notes), "任务"); err != nil {
			t.Fatal(err)
		}
	}

	// 两次都得问：没有类别就没有可记的东西
	if ap.asked != 2 {
		t.Fatalf("没有类别时每次都要问，实际问了 %d 次", ap.asked)
	}
	if len(rules.List()) != 0 {
		t.Fatalf("不该留下任何规则：%+v", rules.List())
	}
	for _, n := range notes {
		if n.Saved || n.Hit {
			t.Fatalf("没有类别时不该有命中/保存通知：%+v", notes)
		}
	}
}

// 规则没写进磁盘时，这次调用仍然放行，但必须留一条带 Err 的通知——
// 否则用户以为已经生效，下次却被再问一遍。
func TestLoopReportsRememberFailure(t *testing.T) {
	ch := &scriptedChat{t: t, steps: []ChatResponse{
		assistantToolCall("c1", "scoped", `{}`),
		assistantText("done"),
	}}
	ap := &alwaysApprover{}
	tool := scopedTool{fakeTool: fakeTool{name: "scoped", approval: true}, scope: "git status"}

	var notes []ApprovalRuleNote
	// Rules 为 nil：模拟"规则文件读坏了、功能整体降级"
	cfg := scopedLoopConfig(t, tool, ap, nil, ch, &notes)
	if _, err := RunLoop(t.Context(), cfg, "任务"); err != nil {
		t.Fatalf("记不住规则不该让任务失败：%v", err)
	}

	if len(notes) != 1 || notes[0].Err == nil {
		t.Fatalf("应当有一条带错误的通知：%+v", notes)
	}
	if notes[0].Saved {
		t.Fatal("没能保存就不该标成 Saved")
	}
}
