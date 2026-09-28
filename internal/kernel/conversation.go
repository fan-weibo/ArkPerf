package kernel

import (
	"fmt"
	"strings"
	"sync"
)

// defaultHistoryTokens 是历史部分的 token 预算（近似）。
//
// 给 30K 不是"上下文只有这么大"，而是因为什么都留着最后一定会撞上模型
// 窗口上限，而撞上时的表现是任务直接失败。宁可早一点丢最旧的整轮，
// 也要保证会话能一直用下去。
//
// 单位是**近似 token**而不是字节：一个汉字 3 字节但只占约 1 个 token，
// 按字节算预算会让中文历史被过早丢弃（差 3 倍）。
const defaultHistoryTokens = 30_000

// Conversation 是一段跨任务的对话上下文。
//
// 为什么需要它：单次 RunLoop 只处理一个任务，任务之间没有记忆，
// 于是"改完代码再测一次"——ArkPerf 最核心的用法——必须反复交代全部背景。
// 会话把上一轮的转录留下来，作为下一轮 History 的前缀。
// 这才是 agent，而不是一问一答。
type Conversation struct {
	mu       sync.Mutex
	messages []Message
	// turns 是单调递增的"已完成轮数"。
	// 它不等于保留的历史轮数：历史被裁剪后仍然如实显示做过多少轮。
	turns     int
	maxTokens int
	// compacted 是已被压缩的工具输出条数。
	compacted int
}

// NewConversation 构造空会话。
func NewConversation() *Conversation {
	return &Conversation{maxTokens: defaultHistoryTokens}
}

// SetMaxTokens 调整历史预算（0 或负值恢复默认）。
func (c *Conversation) SetMaxTokens(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n <= 0 {
		n = defaultHistoryTokens
	}
	c.maxTokens = n
	c.messages = trimHistory(c.messages, c.maxTokens)
}

// History 返回可交给下一轮的对话前缀（不含 system，且以 user 开头）。
func (c *Conversation) History() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Message(nil), c.messages...)
}

// Messages 返回完整对话，供持久化与展示。
func (c *Conversation) Messages() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Message(nil), c.messages...)
}

// Turns 返回本会话已完成的轮数（单调递增，不受裁剪影响）。
func (c *Conversation) Turns() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turns
}

// Retained 返回当前仍保留在上下文里的轮数。
//
// 它可能小于 Turns：超出预算的旧轮次会被丢掉。两个数都摆出来，
// 用户才不会把"agent 忘了"当成 bug。
func (c *Conversation) Retained() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return countTurns(c.messages)
}

// Budget 返回当前的 token 预算（近似）。
func (c *Conversation) Budget() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxTokens
}

// Set 用一轮结束后的**完整转录**更新会话（含历史，不是只有新增部分）。
//
// 语义是"替换"而不是"追加"：LoopResult.Messages 本来就是完整转录，
// 替换让 Set 天然幂等——重复调用不会把历史累积两遍。
//
// system 会被丢掉：它包含当前工具清单与工作目录，必须每轮按实际情况重建，
// 从历史里带一个过期的 system 只会让模型看到不存在的工具。
func (c *Conversation) Set(transcript []Message) {
	kept := withoutSystem(transcript)
	// 丢掉尾部的半截轮：历史必须以 assistant 结尾，否则下一轮会拼出
	// 连续两条 user 消息，部分端点直接拒绝。
	kept = trimDanglingTail(kept)
	if len(kept) == 0 {
		// 这一轮没有任何可用内容（例如模型调用直接失败），什么都不改——
		// 空转也算一轮会让状态行上的轮数骗人。
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = compactHistory(kept, c.maxTokens)
	c.turns++
	c.compacted = countCompacted(c.messages)
}

// Reset 开始一段新会话。
func (c *Conversation) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = nil
	c.turns = 0
	c.compacted = 0
}

// Restore 用已保存的历史重建会话（用于恢复上次会话）。
func (c *Conversation) Restore(messages []Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = compactHistory(trimDanglingTail(withoutSystem(messages)), c.maxTokens)
	c.turns = countTurns(c.messages)
	c.compacted = countCompacted(c.messages)
}

// Compacted 返回已被压缩的工具输出条数。
//
// 把它摆出来是因为"agent 说的内容变少了"必须可归因：
// 是历史被裁剪了，还是被压缩了，用户看一眼就知道该不该重说一遍。
func (c *Conversation) Compacted() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.compacted
}

// trimDanglingTail 丢掉尾部的半截轮（没有 assistant 收尾的部分）。
//
// 历史必须以 assistant 结尾：否则下一轮会在末尾再拼一条 user 消息，
// 形成连续两条 user——多数端点能忍，但有些会直接 400，而且报错跟会话
// 结构有关，看不出是"上次任务没跑完"导致的。
func trimDanglingTail(msgs []Message) []Message {
	for len(msgs) > 0 && msgs[len(msgs)-1].Role != "assistant" {
		msgs = msgs[:len(msgs)-1]
	}
	return msgs
}

func withoutSystem(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		out = append(out, m)
	}
	return out
}

func countTurns(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "user" {
			n++
		}
	}
	return n
}

// trimHistory 从头丢掉整轮，直到历史低于预算。
//
// 边界规则：保留下来的历史必须以 user 消息开头。若在 assistant（带 tool_calls）
// 与它后面的 tool 结果之间切断，就会留下一个没有对应结果的工具调用——
// 多数 OpenAI 兼容端点会直接 400，而且报错信息完全看不出跟会话有关，
// 排查成本极高。
func trimHistory(msgs []Message, maxTokens int) []Message {
	if len(msgs) == 0 {
		return nil
	}
	if maxTokens <= 0 {
		maxTokens = defaultHistoryTokens
	}

	// 起点落在第一条 user 消息上（防御外部传入的畸形历史）
	start := 0
	for start < len(msgs) && msgs[start].Role != "user" {
		start++
	}

	total := historyCost(msgs[start:])
	for total > maxTokens && start < len(msgs) {
		next := nextTurnStart(msgs, start)
		total -= historyCost(msgs[start:next])
		start = next
	}
	if start >= len(msgs) {
		// 一轮都留不下：宁可从零开始，也不要带着超预算的历史去请求
		return nil
	}
	return msgs[start:]
}

// 压缩参数。
const (
	// tombstoneExcerptChars 墓碑里首尾各保留多少字符。
	//
	// 首尾都留是因为关键信息的位置很分裂：
	// 构建失败原因在末尾，测量结论与表头常在开头。
	//
	// 但摘录本身也要小：留太多的话墓碑只是"稍微变短"，压缩压不到预算以内，
	// 最后还是得靠丢整轮——那就白压了。
	tombstoneExcerptChars = 160
	// tombstonePrefix 是墓碑标记，也是幂等判断的依据。
	tombstonePrefix = "〔工具"
)

// compactHistory 两级压缩：先把最老的工具输出墓碑化，仍然超预算才丢整轮。
//
// 为什么必须先墓碑化：工具输出（构建日志、测量报告）是历史里最大块、
// 也是最先过时的部分，但**不能整块删掉**——上一轮测出的数字往往就在里面。
// 整轮丢弃相当于让 agent 失忆；墓碑化则把"发生了什么"压缩成可引用的摘要，
// 结构（user / assistant / tool 的配对）完全保留。
func compactHistory(msgs []Message, maxTokens int) []Message {
	if maxTokens <= 0 {
		maxTokens = defaultHistoryTokens
	}
	if historyCost(msgs) <= maxTokens {
		return msgs
	}

	out := tombstoneOldToolOutput(msgs, maxTokens)
	if historyCost(out) > maxTokens {
		// 墓碑化还不够，才用最后的手段：丢最旧的整轮
		out = trimHistory(out, maxTokens)
	}
	return out
}

// tombstoneOldToolOutput 从最老的工具输出开始墓碑化，直到低于预算。
//
// **当前这一轮（最后一条 user 消息之后）的工具输出一律不动**：
// 模型刚拿到它们、正在据此推理，压缩它们会直接影响这一轮的结论。
// 更早轮次的输出才是"已经被消化过"的信息，可以先压。
func tombstoneOldToolOutput(msgs []Message, maxTokens int) []Message {
	lastUser := -1
	for i, m := range msgs {
		if m.Role == "user" {
			lastUser = i
		}
	}

	out := append([]Message(nil), msgs...)
	for i, m := range out {
		if historyCost(out) <= maxTokens {
			break
		}
		if m.Role != "tool" || i > lastUser {
			continue
		}
		if strings.HasPrefix(m.Content, tombstonePrefix) {
			continue // 已经压过，不重复压
		}
		out[i].Content = tombstoneContent(toolNameFor(out, i), m.Content)
	}
	return out
}

// tombstoneContent 生成墓碑文本：标注省略量，并保留首尾摘录。
//
// 短内容直接整段保留——压缩它省不了多少，却会让人怀疑"是不是丢了什么"。
func tombstoneContent(name, content string) string {
	chars, tokens := len(content), EstimateTokens(content)
	if chars <= 2*tombstoneExcerptChars {
		return fmt.Sprintf("%s %s 的输出已压缩：原 %d 字符〕\n%s", tombstonePrefix, name, chars, content)
	}
	return fmt.Sprintf(
		"%s %s 的输出已压缩：原 %d 字符 / 约 %d token，以下仅首尾摘录〕\n%s\n…〔中间省略 %d 字符〕…\n%s",
		tombstonePrefix, name, chars, tokens,
		headRunes(content, tombstoneExcerptChars),
		chars-2*tombstoneExcerptChars,
		tailRunes(content, tombstoneExcerptChars))
}

// toolNameFor 反查这条工具输出对应的工具名（从前面的 assistant 消息里找）。
// 找不到就返回空：墓碑里没有名字也比猜一个错名字好。
func toolNameFor(msgs []Message, idx int) string {
	// 工具输出通常紧跟在发起调用的 assistant 之后，往前看几条足够
	for i := idx - 1; i >= 0 && i >= idx-3; i-- {
		for _, tc := range msgs[i].ToolCalls {
			if tc.ID != "" && tc.ID == msgs[idx].ToolCallID {
				return tc.Function.Name
			}
		}
	}
	return ""
}

func countCompacted(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, tombstonePrefix) {
			n++
		}
	}
	return n
}

// headRunes / tailRunes 按 rune 切片。
// 按字节切会切出半个汉字，那样进模型的就是非法 UTF-8。
func headRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

func tailRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[len(rs)-n:])
}

// historyCost 估算一段历史的模型侧成本（近似 token 数）。
//
// 工具调用的名字与参数也进上下文，必须一起算——构建参数动辄上百字符，
// 漏算会明显低估。
func historyCost(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += EstimateTokens(m.Content)
		for _, tc := range m.ToolCalls {
			n += EstimateTokens(tc.Function.Name) + EstimateTokens(tc.Function.Arguments)
		}
	}
	return n
}

// nextTurnStart 返回从 start 出发的下一轮起点（下一个 user 消息的下标）。
func nextTurnStart(msgs []Message, start int) int {
	for i := start + 1; i < len(msgs); i++ {
		if msgs[i].Role == "user" {
			return i
		}
	}
	return len(msgs)
}

// EstimateTokens 估算文本的 token 数。
//
// 口径：ASCII 约 4 字符/token；CJK 约 1 字符/token（一个汉字通常就是一个词）。
// 刻意不引 tokenizer：这里只需要一个量级正确的预算闸门，
// 引入分词器会给零依赖的内核带来一份不小的依赖树。
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk := 0
	for _, r := range s {
		if isWide(r) {
			cjk++
		}
	}
	// 每个 CJK 字符在 UTF-8 里占 3 字节，其余按 ASCII 的 4 字符/token 估
	asciiBytes := len(s) - cjk*3
	if asciiBytes < 0 {
		asciiBytes = 0
	}
	n := asciiBytes/4 + cjk
	if n == 0 {
		n = 1 // 非空文本至少占 1
	}
	return n
}

// isWide 判断是否为"CJK 类"字符（按 1 字符约 1 token 计）。
func isWide(r rune) bool {
	switch {
	case r >= 0x3040 && r <= 0x30ff: // 日文假名
		return true
	case r >= 0x3400 && r <= 0x4dbf: // CJK 扩展 A
		return true
	case r >= 0x4e00 && r <= 0x9fff: // CJK 基本区
		return true
	case r >= 0xf900 && r <= 0xfaff: // CJK 兼容表意
		return true
	case r >= 0xff00 && r <= 0xffef: // 全角形式
		return true
	default:
		return false
	}
}
