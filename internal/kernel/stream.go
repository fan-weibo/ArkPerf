package kernel

import (
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// DeltaKind 区分流式片段的种类。
type DeltaKind string

const (
	// DeltaContent 是可见回答的增量。
	DeltaContent DeltaKind = "content"
	// DeltaReasoning 是思考过程的增量（deepseek-reasoner 一类模型的 reasoning_content）。
	//
	// 与 content 分开而不是混在一起：思考过程往往比回答长一个数量级，
	// 前端得能选择"展开看"、"只看进度"或"完全不显示"。揉成一坨就没得选了。
	DeltaReasoning DeltaKind = "reasoning"
)

// toolCallStream 是**一次**工具调用在流式过程中的累积状态。
type toolCallStream struct {
	id        string
	name      string
	arguments strings.Builder
}

// streamAccumulator 把 SSE 分片拼回一条完整的 assistant 消息。
//
// 三处必须做对，少一处都会得到"偶发丢工具调用 / 参数被截断"这类难查的故障：
//
//  1. **tool_calls 是按 Index 分片下发的**：第一个分片给 id 与 name，之后的
//     arguments 逐片追加。见到一片就 append 一条，会把一次调用拆成好几条，
//     每条都只有半截参数——而半截 JSON 在模型侧看起来只是"参数写错了"。
//     所以必须按 Index 归并。
//  2. **没有 choices 的 chunk 是合法的**：请求里带了 include_usage 时，
//     最后一个 chunk 只带 usage，choices 是空数组。
//  3. **usage 只在最后那一个 chunk 里出现**，前面的 chunk 即使有这个字段也是 null。
type streamAccumulator struct {
	onDelta func(DeltaKind, string)

	role      string
	model     string
	content   strings.Builder
	toolCalls []toolCallStream
	usage     Usage
	// emitted 记录是否已经往外吐过内容。中途断流时靠它决定"能不能重试"——
	// 吐过就不能重试，否则同一段话会再打一遍。
	emitted bool
}

func newStreamAccumulator(onDelta func(DeltaKind, string)) *streamAccumulator {
	return &streamAccumulator{onDelta: onDelta}
}

// consume 吃掉一个流式分片。
func (a *streamAccumulator) consume(chunk openai.ChatCompletionStreamResponse) {
	if chunk.Model != "" {
		a.model = chunk.Model
	}
	if chunk.Usage != nil {
		a.usage = Usage{
			PromptTokens:     chunk.Usage.PromptTokens,
			CompletionTokens: chunk.Usage.CompletionTokens,
		}
	}
	for _, choice := range chunk.Choices {
		d := choice.Delta
		if d.Role != "" {
			a.role = d.Role
		}
		if d.ReasoningContent != "" {
			a.emit(DeltaReasoning, d.ReasoningContent)
		}
		if d.Content != "" {
			a.content.WriteString(d.Content)
			a.emit(DeltaContent, d.Content)
		}
		for _, tc := range d.ToolCalls {
			a.mergeToolCall(tc)
		}
	}
}

// result 拼出最终消息。
func (a *streamAccumulator) result() ChatResponse {
	role := a.role
	if role == "" {
		role = "assistant"
	}
	msg := Message{Role: role, Content: a.content.String()}
	for _, c := range a.toolCalls {
		// 名字都没拼出来的槽位不算一次调用：模型不可能调用一个无名工具，
		// 塞给循环只会换来一条 "unknown tool \"\"" 的噪声结果。
		// 它只可能来自 Index 跳号产生的空槽（例如端点从 1 开始编号）。
		if c.name == "" {
			continue
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID:       c.id,
			Function: FunctionCall{Name: c.name, Arguments: c.arguments.String()},
		})
	}
	return ChatResponse{Message: msg, Model: a.model, Usage: a.usage}
}

// mergeToolCall 把一个 tool_call 分片并进对应的槽位。
func (a *streamAccumulator) mergeToolCall(tc openai.ToolCall) {
	idx := a.slotFor(tc)
	if idx < 0 {
		return
	}
	slot := &a.toolCalls[idx]
	if tc.ID != "" {
		slot.id = tc.ID
	}
	if tc.Function.Name != "" {
		slot.name = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		slot.arguments.WriteString(tc.Function.Arguments)
	}
}

// slotFor 找到（必要时新建）这个分片该归到哪一次调用。
func (a *streamAccumulator) slotFor(tc openai.ToolCall) int {
	if tc.Index != nil {
		i := *tc.Index
		if i < 0 {
			return -1
		}
		// 用补零的方式对齐下标，而不是直接赋值：端点可能先给 index=1 再给 0，
		// 直接赋值会让先到的那一片无处安放。
		for len(a.toolCalls) <= i {
			a.toolCalls = append(a.toolCalls, toolCallStream{})
		}
		return i
	}

	// 有些兼容端点不带 index。这时靠 id 判断是不是同一次调用：
	//   id 相同、或这一片根本没带 id → 接在上一次后面（arguments 的后续分片就是这种）
	//   带了一个不同的 id         → 新的一次
	if n := len(a.toolCalls); n > 0 {
		last := a.toolCalls[n-1]
		if tc.ID == "" || last.id == "" || last.id == tc.ID {
			return n - 1
		}
	}
	// 什么都没有的碎片（既无 index 也无 id）无处可归，丢掉而不是开一个空槽。
	// 开空槽会在 result() 里被滤掉，但空槽还会挡住后面同 index 的分片。
	if tc.ID == "" && tc.Function.Name == "" && tc.Function.Arguments == "" {
		return -1
	}
	a.toolCalls = append(a.toolCalls, toolCallStream{})
	return len(a.toolCalls) - 1
}

// emit 回调增量。onDelta 为空表示调用方不要流式，直接不问。
func (a *streamAccumulator) emit(kind DeltaKind, text string) {
	if a.onDelta == nil {
		return
	}
	a.emitted = true
	a.onDelta(kind, text)
}

// hasEmitted 返回是否已经往外吐过内容。
func (a *streamAccumulator) hasEmitted() bool { return a.emitted }
