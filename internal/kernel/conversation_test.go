package kernel

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func msg(role, content string) Message { return Message{Role: role, Content: content} }

func rolesOf(msgs []Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Role)
	}
	return out
}

func TestConversationStartsEmpty(t *testing.T) {
	c := NewConversation()
	if len(c.History()) != 0 {
		t.Fatalf("新会话不该有历史：%+v", c.History())
	}
	if c.Turns() != 0 || c.Retained() != 0 {
		t.Fatalf("轮数应为 0：turns=%d retained=%d", c.Turns(), c.Retained())
	}
}

func TestConversationSetAndHistory(t *testing.T) {
	c := NewConversation()
	c.Set([]Message{
		msg("system", "系统提示"),
		msg("user", "第一个问题"),
		msg("assistant", "第一个回答"),
	})

	if c.Turns() != 1 {
		t.Fatalf("轮数：%d", c.Turns())
	}
	h := c.History()
	if got := rolesOf(h); len(got) != 2 || got[0] != "user" || got[1] != "assistant" {
		t.Fatalf("历史应只含 user+assistant：%v", got)
	}
}

// system 必须每轮重建，不能从历史里带——它带着当前工具清单与工作目录，
// 带一份过期的 system 会让模型看到不存在的工具。
func TestConversationDropsSystemFromHistory(t *testing.T) {
	c := NewConversation()
	c.Set([]Message{msg("system", "旧的系统提示"), msg("user", "q"), msg("assistant", "a")})
	for _, m := range c.History() {
		if m.Role == "system" {
			t.Fatalf("历史里不该有 system：%+v", m)
		}
	}
}

// 尾部半截轮（没有 assistant 收尾）必须丢掉。
// 留着的话下一轮会拼出连续两条 user 消息，部分端点直接 400，
// 而报错完全看不出跟会话结构有关。
func TestConversationDropsDanglingTail(t *testing.T) {
	c := NewConversation()

	// 只有一条 user：什么都留不下，也不该记一轮
	c.Set([]Message{msg("user", "跑了一半")})
	if c.Turns() != 0 || len(c.History()) != 0 {
		t.Fatalf("半截轮不该算数：turns=%d hist=%d", c.Turns(), len(c.History()))
	}

	// 完整轮 + 尾部半截：只丢半截
	c.Set([]Message{
		msg("user", "第一轮"), msg("assistant", "答"),
		msg("user", "第二轮没答完"),
	})
	h := c.History()
	if got := rolesOf(h); len(got) != 2 || got[len(got)-1] != "assistant" {
		t.Fatalf("历史必须以 assistant 结尾：%v", got)
	}
	if c.Turns() != 1 {
		t.Fatalf("只该记一轮：%d", c.Turns())
	}
}

// Set 是"替换"语义：重复喂同一份完整转录不应累积两遍。
func TestConversationSetIsIdempotent(t *testing.T) {
	c := NewConversation()
	full := []Message{msg("user", "q"), msg("assistant", "a")}
	c.Set(full)
	c.Set(full)

	if got := len(c.History()); got != 2 {
		t.Fatalf("重复设置不应重复累积：%d 条", got)
	}
}

// 保留的历史必须以 user 开头：在 assistant(带 tool_calls) 与 tool 结果之间
// 切断会留下没有结果的工具调用。
func TestConversationTrimsWholeTurnsFromTheFront(t *testing.T) {
	c := NewConversation()
	// 一轮中文约 40+40 token，预算设成只装得下两轮
	c.SetMaxTokens(200)

	// 模拟循环：每轮的转录都是"累积的完整转录"
	var transcript []Message
	for i := range 4 {
		transcript = append(transcript,
			msg("user", strings.Repeat("问题", 20)+string(rune('A'+i))),
			msg("assistant", strings.Repeat("回答", 20)+string(rune('A'+i))),
		)
		c.Set(transcript)
	}

	h := c.History()
	if len(h) == 0 {
		t.Fatal("不该把历史清空")
	}
	if h[0].Role != "user" {
		t.Fatalf("历史必须以 user 开头，实际 %q", h[0].Role)
	}
	if got := historyCost(h); got > 200 {
		t.Fatalf("裁剪后仍超预算：%d tokens", got)
	}

	// 总轮数单调递增，不因裁剪而变小；保留轮数变少
	if c.Turns() != 4 {
		t.Fatalf("总轮数应保持 4，实际 %d", c.Turns())
	}
	if c.Retained() >= c.Turns() {
		t.Fatalf("应有轮次被裁剪：retained=%d turns=%d", c.Retained(), c.Turns())
	}
}

// 预算按 token 而不是字节算：一个汉字 3 字节但只占约 1 个 token，
// 用字节数会让中文历史被过早丢弃（差 3 倍）。
func TestEstimateTokensIsCJKAware(t *testing.T) {
	cn := strings.Repeat("中", 100) // 300 字节，但只约 100 token
	en := strings.Repeat("a", 300) // 300 字节，约 75 token

	if got := EstimateTokens(cn); got < 90 || got > 110 {
		t.Fatalf("100 个汉字应估成约 100 token，实际 %d", got)
	}
	if got := EstimateTokens(en); got < 70 || got > 80 {
		t.Fatalf("300 个 ASCII 应估成约 75 token，实际 %d", got)
	}
	if EstimateTokens("") != 0 {
		t.Fatal("空串应为 0")
	}
	if EstimateTokens(" ") == 0 {
		t.Fatal("非空文本至少占 1 token")
	}
}

// 预算极小的时间点也要保持结构合法，而不是留半条消息。
func TestConversationTrimNeverBreaksPairing(t *testing.T) {
	c := NewConversation()
	c.SetMaxTokens(1)

	c.Set([]Message{
		msg("user", "q1"), msg("assistant", "a1"),
		msg("user", "q2"), msg("assistant", "a2"),
	})

	h := c.History()
	if len(h) > 0 && h[0].Role != "user" {
		t.Fatalf("裁剪后仍必须以 user 开头：%v", rolesOf(h))
	}
	if len(h) > 0 && h[len(h)-1].Role != "assistant" {
		t.Fatalf("裁剪后仍必须以 assistant 结尾：%v", rolesOf(h))
	}
}

func TestConversationReset(t *testing.T) {
	c := NewConversation()
	c.Set([]Message{msg("user", "q"), msg("assistant", "a")})
	c.Reset()

	if c.Turns() != 0 || len(c.History()) != 0 {
		t.Fatalf("重置后应回到空：turns=%d hist=%d", c.Turns(), len(c.History()))
	}
}

func TestConversationRestore(t *testing.T) {
	c := NewConversation()
	c.Restore([]Message{
		msg("system", "旧 system"),
		msg("user", "q1"), msg("assistant", "a1"),
		msg("user", "q2"), msg("assistant", "a2"),
	})

	if c.Turns() != 2 || c.Retained() != 2 {
		t.Fatalf("恢复：turns=%d retained=%d", c.Turns(), c.Retained())
	}
	if got := rolesOf(c.History()); got[0] != "user" {
		t.Fatalf("恢复后历史结构不对：%v", got)
	}
}

// ---------------------------------------------------------------- 上下文压缩

// bigToolOutput 造一条"典型的大工具输出"：开头有结论、结尾有失败原因。
func bigToolOutput(tag string) string {
	return "HEAD-" + tag + "\n" + strings.Repeat("a", 1900) + "\nTAIL-" + tag
}

// multiTurnTranscript 造一份多轮转录：每轮 = 提问 → 调工具 → 工具输出 → 回答。
func multiTurnTranscript(turns int) []Message {
	var out []Message
	for i := range turns {
		tag := fmt.Sprintf("T%d", i)
		out = append(out,
			Message{Role: "user", Content: "问题" + tag},
			Message{Role: "assistant", Content: "调用工具" + tag, ToolCalls: []ToolCall{
				{ID: "call-" + tag, Function: FunctionCall{Name: "harmony_build", Arguments: "{}"}},
			}},
			Message{Role: "tool", ToolCallID: "call-" + tag, Content: bigToolOutput(tag)},
			Message{Role: "assistant", Content: "回答" + tag},
		)
	}
	return out
}

// 这条是本次压缩的核心价值：**优先墓碑化，而不是丢整轮**。
//
// 整轮丢弃会让 agent 失忆——上一轮测出的数字可能就在被丢掉的那轮里；
// 墓碑化保留结构与首尾摘录，"发生了什么、结论是什么"仍然可引用。
func TestCompactionTombstonesInsteadOfDroppingTurns(t *testing.T) {
	msgs := multiTurnTranscript(4)

	// 预算自校准：取"原样成本的一半"。
	// 不写死数字是因为成本估算是近似值，写死会在改动估算口径时变得脆弱。
	budget := historyCost(msgs) / 2

	c := NewConversation()
	c.SetMaxTokens(budget)
	c.Set(msgs)

	if c.Retained() != 4 {
		t.Fatalf("墓碑化足够时不应丢轮：保留 %d 轮（总 %d）", c.Retained(), c.Turns())
	}
	if got := historyCost(c.History()); got > budget {
		t.Fatalf("压缩后仍超预算：%d > %d tokens", got, budget)
	}
	if c.Compacted() != 3 {
		t.Fatalf("应压缩 3 条（不含本轮），实际 %d", c.Compacted())
	}

	// 对照：同样预算下"只丢整轮"（压缩前的行为）会丢掉更多。
	// 这个对照才是本测试真正要证明的东西。
	if dropped := countTurns(trimHistory(msgs, budget)); dropped >= 4 {
		t.Fatalf("该预算下整轮丢弃也不丢轮，测试失去对比意义：%d", dropped)
	}

	h := c.History()
	// 本轮的工具输出必须原样保留：模型正在用它
	lastTool := h[len(h)-2]
	if strings.Contains(lastTool.Content, tombstonePrefix) {
		t.Fatal("本轮工具输出不该被压缩")
	}
	if !strings.Contains(lastTool.Content, "TAIL-T3") {
		t.Fatalf("本轮工具输出应保持完整：%.80q", lastTool.Content)
	}
	// 早期输出被压成墓碑，但首尾摘录仍在——信息没有丢光
	first := h[2].Content
	for _, want := range []string{tombstonePrefix, "HEAD-T0", "TAIL-T0", "harmony_build"} {
		if !strings.Contains(first, want) {
			t.Fatalf("墓碑缺少 %q：%.120q", want, first)
		}
	}
}

// 墓碑化不够时才用最后的手段：丢整轮。
func TestCompactionDropsTurnsOnlyAsLastResort(t *testing.T) {
	c := NewConversation()
	c.SetMaxTokens(150) // 小到墓碑化也救不回来
	c.Set(multiTurnTranscript(4))

	if c.Retained() >= 4 {
		t.Fatalf("预算极小时应丢掉旧轮：保留 %d", c.Retained())
	}
	if got := historyCost(c.History()); got > 150 {
		t.Fatalf("仍超预算：%d tokens", got)
	}
	if len(c.History()) > 0 && c.History()[0].Role != "user" {
		t.Fatal("丢轮之后仍必须以 user 开头")
	}
}

// 重复压缩不能把内容越压越小（否则最后只剩一串标记）。
func TestCompactionIsIdempotent(t *testing.T) {
	msgs := multiTurnTranscript(3)
	first := compactHistory(msgs, 500)
	second := compactHistory(first, 500)

	if historyCost(first) != historyCost(second) {
		t.Fatalf("第二次压缩又变小了：%d → %d", historyCost(first), historyCost(second))
	}
	if countCompacted(first) != countCompacted(second) {
		t.Fatalf("压缩条数变了：%d → %d", countCompacted(first), countCompacted(second))
	}
}

func TestTombstoneContentShortKeepsEverything(t *testing.T) {
	// 短内容压缩省不了多少，却会让人怀疑"是不是丢了什么"——直接整段保留
	short := "BUILD SUCCESSFUL in 2 s 467 ms"
	if got := tombstoneContent("harmony_build", short); !strings.Contains(got, short) {
		t.Fatalf("短内容应整段保留：%q", got)
	}
}

func TestTombstoneContentReportsOmission(t *testing.T) {
	long := strings.Repeat("x", 1000)
	got := tombstoneContent("harmony_launch", long)
	for _, want := range []string{tombstonePrefix, "harmony_launch", "省略", "1000"} {
		if !strings.Contains(got, want) {
			t.Fatalf("墓碑缺少 %q：%.160q", want, got)
		}
	}
	// 墓碑本身必须比原文小得多，否则压缩没有意义
	if EstimateTokens(got) >= EstimateTokens(long) {
		t.Fatalf("墓碑没有变小：%d → %d", EstimateTokens(long), EstimateTokens(got))
	}
}

// 按字节切会切出半个汉字——进模型就是非法 UTF-8。
func TestHeadTailRunesAreUTF8Safe(t *testing.T) {
	s := strings.Repeat("中", 100) + strings.Repeat("文", 100)
	head, tail := headRunes(s, 30), tailRunes(s, 30)
	if !utf8.ValidString(head) || !utf8.ValidString(tail) {
		t.Fatalf("切出了非法 UTF-8：%q / %q", head, tail)
	}
	if head != strings.Repeat("中", 30) {
		t.Fatalf("头部不对：%q", head)
	}
	if tail != strings.Repeat("文", 30) {
		t.Fatalf("尾部不对：%q", tail)
	}
	// 比上限短时原样返回
	if headRunes("abc", 10) != "abc" || tailRunes("abc", 10) != "abc" {
		t.Fatal("短于上限时应原样返回")
	}
}

func TestToolNameFor(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: FunctionCall{Name: "harmony_build"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "out"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Function: FunctionCall{Name: "harmony_launch"}}}},
		{Role: "tool", ToolCallID: "c2", Content: "out"},
		{Role: "tool", ToolCallID: "", Content: "out"},
	}
	if got := toolNameFor(msgs, 1); got != "harmony_build" {
		t.Fatalf("应找到 harmony_build，实际 %q", got)
	}
	if got := toolNameFor(msgs, 3); got != "harmony_launch" {
		t.Fatalf("应找到 harmony_launch，实际 %q", got)
	}
	if got := toolNameFor(msgs, 4); got != "" {
		t.Fatalf("找不到时应返回空而不是乱猜，实际 %q", got)
	}
}

// ---------------------------------------------------------------- 循环层

// 循环必须接受历史、并把完整转录交回：这是多轮会话的地基。
func TestRunLoopAcceptsHistoryAndReturnsTranscript(t *testing.T) {
	reg := NewRegistry()
	chat := &scriptedChat{t: t, steps: []ChatResponse{assistantText("这一轮的回答")}}

	history := []Message{
		{Role: "user", Content: "上一轮的问题"},
		{Role: "assistant", Content: "上一轮的回答"},
	}
	res, err := RunLoop(t.Context(), LoopConfig{
		Registry: reg,
		Chat:     chat.chat,
		History:  history,
	}, "这一轮的问题")
	if err != nil {
		t.Fatal(err)
	}

	// 请求里的顺序：system → 历史 → 本轮 user
	got := rolesOf(chat.requests[0].Messages)
	want := []string{"system", "user", "assistant", "user"}
	if len(got) != len(want) {
		t.Fatalf("请求消息数 %d，期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条角色 %q，期望 %q（全部：%v）", i, got[i], want[i], got)
		}
	}
	req := chat.requests[0].Messages
	if req[1].Content != "上一轮的问题" || req[3].Content != "这一轮的问题" {
		t.Fatalf("历史与本轮内容错位：%+v", req)
	}

	// 返回的转录要能被下一轮直接拿去当 History
	if len(res.Messages) != 5 {
		t.Fatalf("返回转录应有 5 条（system+历史2+user+assistant），实际 %d", len(res.Messages))
	}
	if res.Messages[len(res.Messages)-1].Role != "assistant" {
		t.Fatalf("转录应以 assistant 结尾：%v", rolesOf(res.Messages))
	}
}

// 多轮会话的端到端硬指标：第二轮的请求里必须看得到第一轮说过的话。
func TestRunnerKeepsConversationAcrossTasks(t *testing.T) {
	reg := NewRegistry()
	chat := &scriptedChat{t: t, steps: []ChatResponse{
		assistantText("第一次回答"),
		assistantText("第二次回答"),
	}}

	conv := NewConversation()
	runner := &Runner{
		Cfg:          &Config{Provider: ProviderConfig{BaseURL: "http://x", APIKey: "k", Model: "m"}},
		Registry:     reg,
		Chat:         chat.chat,
		Conversation: conv,
	}

	if _, err := runner.Run(t.Context(), Task{Text: "第一个问题"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(t.Context(), Task{Text: "第二个问题"}); err != nil {
		t.Fatal(err)
	}

	second := chat.requests[1].Messages
	var contents []string
	for _, m := range second {
		contents = append(contents, m.Content)
	}
	joined := strings.Join(contents, "|")
	for _, want := range []string{"第一个问题", "第一次回答", "第二个问题"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("第二轮请求里应包含 %q，实际：%s", want, joined)
		}
	}
	if conv.Turns() != 2 {
		t.Fatalf("会话轮数：%d", conv.Turns())
	}
}

// 没有 Conversation 时保持单轮语义（命令行一次性调用的行为不该被改掉）。
func TestRunnerWithoutConversationIsSingleTurn(t *testing.T) {
	reg := NewRegistry()
	chat := &scriptedChat{t: t, steps: []ChatResponse{
		assistantText("答一"), assistantText("答二"),
	}}
	runner := &Runner{
		Cfg:      &Config{Provider: ProviderConfig{BaseURL: "http://x", APIKey: "k", Model: "m"}},
		Registry: reg,
		Chat:     chat.chat,
	}

	runner.Run(t.Context(), Task{Text: "第一问"})
	runner.Run(t.Context(), Task{Text: "第二问"})

	second := chat.requests[1].Messages
	if got := rolesOf(second); len(got) != 2 || got[0] != "system" || got[1] != "user" {
		t.Fatalf("单轮模式下第二轮不该带上历史：%v", got)
	}
	if strings.Contains(second[1].Content, "第一问") {
		t.Fatalf("单轮模式泄露了上一轮内容：%q", second[1].Content)
	}
}
