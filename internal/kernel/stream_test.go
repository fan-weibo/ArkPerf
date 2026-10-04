package kernel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

// ---------------------------------------------------------------- 夹具

// sseReply 按 SSE 格式把若干 chunk 写出去，并以 [DONE] 收尾。
func sseReply(chunks ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeChunks(w, chunks...)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flush(w)
	}
}

// sseReplyCut 写完 chunk 就直接断开，**不发 [DONE]**——
// 用来验证"服务端中途断了"不会被当成整次失败。
func sseReplyCut(chunks ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeChunks(w, chunks...)
	}
}

func writeChunks(w http.ResponseWriter, chunks ...string) {
	for _, c := range chunks {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
		flush(w)
	}
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// deltaChunk 造一个带 delta 的 chunk。
func deltaChunk(t *testing.T, delta map[string]any) string {
	t.Helper()
	return marshal(t, map[string]any{
		"model":   "test-model",
		"choices": []map[string]any{{"index": 0, "delta": delta}},
	})
}

// toolCallDelta 造一个 tool_call 分片。index 传 nil 表示这个端点不带 index。
func toolCallDelta(t *testing.T, index *int, id, name, args string) map[string]any {
	t.Helper()
	fn := map[string]any{}
	if name != "" {
		fn["name"] = name
	}
	if args != "" {
		fn["arguments"] = args
	}
	tc := map[string]any{"type": "function", "function": fn}
	if id != "" {
		tc["id"] = id
	}
	if index != nil {
		tc["index"] = *index
	}
	return tc
}

// usageChunk 是带 usage 的**最后一个** chunk：choices 是空数组，这是合法的。
func usageChunk(prompt, completion int) string {
	b, _ := json.Marshal(map[string]any{
		"model":   "test-model",
		"choices": []any{},
		"usage":   map[string]int{"prompt_tokens": prompt, "completion_tokens": completion},
	})
	return string(b)
}

// collectDeltas 返回一个回调与它的记录。
func collectDeltas() (*[]DeltaKind, *[]string, func(DeltaKind, string)) {
	var kinds []DeltaKind
	var texts []string
	fn := func(k DeltaKind, t string) {
		kinds = append(kinds, k)
		texts = append(texts, t)
	}
	return &kinds, &texts, fn
}

// ---------------------------------------------------------------- 流式

func TestChatStreamEmitsDeltasAndReturnsFullMessage(t *testing.T) {
	srv := newServer(t, sseReply(
		deltaChunk(t, map[string]any{"role": "assistant"}),
		deltaChunk(t, map[string]any{"content": "你好"}),
		deltaChunk(t, map[string]any{"content": "，世界"}),
		usageChunk(7, 9),
	))

	kinds, texts, onDelta := collectDeltas()
	resp, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		OnDelta:  onDelta,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 返回值必须与非流式**完全一致**：上游不该知道走的是哪条路径
	if resp.Message.Content != "你好，世界" {
		t.Fatalf("content: %q", resp.Message.Content)
	}
	if resp.Message.Role != "assistant" {
		t.Fatalf("role: %q", resp.Message.Role)
	}
	// usage 藏在最后一个只有 usage 的 chunk 里，必须被接住
	if resp.Usage.PromptTokens != 7 || resp.Usage.CompletionTokens != 9 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	if len(*texts) != 2 || strings.Join(*texts, "") != "你好，世界" {
		t.Fatalf("deltas: %v", *texts)
	}
	for _, k := range *kinds {
		if k != DeltaContent {
			t.Fatalf("普通文字应当是 content，得到 %q", k)
		}
	}
}

// 请求体里必须带 stream 与 include_usage：不带后者就拿不到 token 用量。
func TestChatStreamRequestShape(t *testing.T) {
	var body map[string]any
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		sseReply(deltaChunk(t, map[string]any{"content": "x"}))(w, r)
	})

	_, _, onDelta := collectDeltas()
	if _, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		OnDelta:  onDelta,
	}); err != nil {
		t.Fatal(err)
	}

	if body["stream"] != true {
		t.Fatalf("stream 必须为 true：%#v", body["stream"])
	}
	so, ok := body["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("stream_options.include_usage 必须为 true：%#v", body["stream_options"])
	}
}

// 不传 OnDelta 时必须走非流式：否则每次都白付 SSE 的解析成本。
func TestChatWithoutOnDeltaStaysNonStreaming(t *testing.T) {
	var body map[string]any
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		jsonReply(200, okBody("ok"))(w, r)
	})

	if _, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if body["stream"] == true {
		t.Fatalf("不传 OnDelta 时不该要求流式：%#v", body["stream"])
	}
}

// tool_call 的参数是**逐片追加**的。见到一片就 append 一条，会把一次调用
// 拆成好几条、每条只有半截 JSON——而半截 JSON 在模型侧只表现为"参数写错了"。
func TestChatStreamAccumulatesToolCallFragments(t *testing.T) {
	zero := 0
	srv := newServer(t, sseReply(
		deltaChunk(t, map[string]any{"tool_calls": []any{
			toolCallDelta(t, &zero, "call_1", "read_file", ""),
		}}),
		deltaChunk(t, map[string]any{"tool_calls": []any{
			toolCallDelta(t, &zero, "", "", `{"path":`),
		}}),
		deltaChunk(t, map[string]any{"tool_calls": []any{
			toolCallDelta(t, &zero, "", "", `"a.go"}`),
		}}),
	))

	_, _, onDelta := collectDeltas()
	resp, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "read"}},
		OnDelta:  onDelta,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("必须归并成 1 次调用，实际 %d：%+v", len(resp.Message.ToolCalls), resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "read_file" {
		t.Fatalf("调用信息不全：%+v", tc)
	}
	if tc.Function.Arguments != `{"path":"a.go"}` {
		t.Fatalf("参数应当被拼回完整 JSON，得到 %q", tc.Function.Arguments)
	}
}

// 端点可能先给 index=1 再给 0（并行工具调用），直接按下标赋值会让先到的无处安放。
func TestChatStreamHandlesOutOfOrderToolCallIndex(t *testing.T) {
	zero, one := 0, 1
	srv := newServer(t, sseReply(
		deltaChunk(t, map[string]any{"tool_calls": []any{
			toolCallDelta(t, &one, "call_b", "grep", `{"pattern":"x"}`),
		}}),
		deltaChunk(t, map[string]any{"tool_calls": []any{
			toolCallDelta(t, &zero, "call_a", "read_file", `{"path":"a"}`),
		}}),
	))

	_, _, onDelta := collectDeltas()
	resp, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{OnDelta: onDelta})
	if err != nil {
		t.Fatal(err)
	}

	if len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("两次调用都该在，实际 %d：%+v", len(resp.Message.ToolCalls), resp.Message.ToolCalls)
	}
	// 顺序按 index 决定，与到达顺序无关
	if resp.Message.ToolCalls[0].Function.Name != "read_file" ||
		resp.Message.ToolCalls[1].Function.Name != "grep" {
		t.Fatalf("应当按 index 排序：%+v", resp.Message.ToolCalls)
	}
}

// 有些兼容端点不发 index，靠 id 判断后续分片属于哪一次调用。
func TestChatStreamWithoutIndexUsesID(t *testing.T) {
	srv := newServer(t, sseReply(
		deltaChunk(t, map[string]any{"tool_calls": []any{
			toolCallDelta(t, nil, "call_1", "read_file", ""),
		}}),
		deltaChunk(t, map[string]any{"tool_calls": []any{
			toolCallDelta(t, nil, "", "", `{"path":"a.go"}`),
		}}),
		deltaChunk(t, map[string]any{"tool_calls": []any{
			toolCallDelta(t, nil, "call_2", "grep", `{"pattern":"x"}`),
		}}),
	))

	_, _, onDelta := collectDeltas()
	resp, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{OnDelta: onDelta})
	if err != nil {
		t.Fatal(err)
	}

	if len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("应当是 2 次调用，实际 %d：%+v", len(resp.Message.ToolCalls), resp.Message.ToolCalls)
	}
	if resp.Message.ToolCalls[0].Function.Arguments != `{"path":"a.go"}` {
		t.Fatalf("第二片应当接在第一次调用上：%+v", resp.Message.ToolCalls[0])
	}
	if resp.Message.ToolCalls[1].Function.Name != "grep" {
		t.Fatalf("换了 id 就该算新的一次调用：%+v", resp.Message.ToolCalls)
	}
}

// 思考过程的增量必须与可见回答分开——它们要被区别对待（折叠/只显进度）。
func TestChatStreamSeparatesReasoningFromContent(t *testing.T) {
	srv := newServer(t, sseReply(
		deltaChunk(t, map[string]any{"reasoning_content": "先看内存"}),
		deltaChunk(t, map[string]any{"content": "结论：没泄漏"}),
	))

	kinds, texts, onDelta := collectDeltas()
	resp, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{OnDelta: onDelta})
	if err != nil {
		t.Fatal(err)
	}

	if len(*kinds) != 2 {
		t.Fatalf("deltas: %v", *texts)
	}
	if (*kinds)[0] != DeltaReasoning || (*kinds)[1] != DeltaContent {
		t.Fatalf("kind 顺序不对：%v", *kinds)
	}
	// 思考过程**不能**混进最终回答里，否则它会变成下一轮的上下文
	if resp.Message.Content != "结论：没泄漏" {
		t.Fatalf("content 不该包含思考过程：%q", resp.Message.Content)
	}
}

func TestChatStreamSkipsEmptyToolCallSlots(t *testing.T) {
	two := 2
	srv := newServer(t, sseReply(
		// 端点从 index=2 开始编号，0/1 是补出来的空槽
		deltaChunk(t, map[string]any{"tool_calls": []any{
			toolCallDelta(t, &two, "call_c", "find", `{"pattern":"*.ts"}`),
		}}),
	))

	_, _, onDelta := collectDeltas()
	resp, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{OnDelta: onDelta})
	if err != nil {
		t.Fatal(err)
	}

	// 空槽不能被当成"调用了无名工具"塞给循环，那只会换来一条 unknown tool 噪声
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("只该有一次真实调用，实际 %+v", resp.Message.ToolCalls)
	}
	if resp.Message.ToolCalls[0].Function.Name != "find" {
		t.Fatalf("调用信息不对：%+v", resp.Message.ToolCalls[0])
	}
}

// 服务端没发 [DONE] 就断开：保留已经收到的部分，而不是把半句话当失败丢掉。
func TestChatStreamKeepsPartialWhenCutWithoutDone(t *testing.T) {
	srv := newServer(t, sseReplyCut(
		deltaChunk(t, map[string]any{"content": "已经说了一半"}),
	))

	_, _, onDelta := collectDeltas()
	resp, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{OnDelta: onDelta})
	if err != nil {
		t.Fatalf("中途断开不该是整次失败：%v", err)
	}
	if resp.Message.Content != "已经说了一半" {
		t.Fatalf("应当保留已收到的部分：%q", resp.Message.Content)
	}
}

func TestChatStreamErrorNamesEndpointAndModel(t *testing.T) {
	srv := newServer(t, jsonReply(http.StatusNotFound, `{"error":{"message":"model not found"}}`))

	_, _, onDelta := collectDeltas()
	_, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{OnDelta: onDelta})
	if err == nil {
		t.Fatal("404 应当报错")
	}
	if !strings.Contains(err.Error(), srv.URL) || !strings.Contains(err.Error(), "model m") {
		t.Fatalf("报错必须能看出端点与模型：%v", err)
	}
}

// 401 降级是鉴权层的补丁，流式与非流式都得有——只修一条会留下"换个模型就坏"的坑。
func TestChatStreamFallsBackToXApiKeyOn401(t *testing.T) {
	var seen []string
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" && r.Header.Get("X-Api-Key") == "" {
			seen = append(seen, "bearer")
			jsonReply(http.StatusUnauthorized, `{"error":{"message":"unauthorized"}}`)(w, r)
			return
		}
		seen = append(seen, "x-api-key")
		sseReply(deltaChunk(t, map[string]any{"content": "ok"}))(w, r)
	})

	_, _, onDelta := collectDeltas()
	resp, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{OnDelta: onDelta})
	if err != nil {
		t.Fatalf("降级失败：%v", err)
	}
	if resp.Message.Content != "ok" {
		t.Fatalf("content: %q", resp.Message.Content)
	}
	if len(seen) != 2 || seen[0] != "bearer" || seen[1] != "x-api-key" {
		t.Fatalf("应当是 bearer 然后 x-api-key，实际 %v", seen)
	}
}

// ---------------------------------------------------------------- 纯逻辑

func openAIToolCall(index *int, id, name, args string) openai.ToolCall {
	tc := openai.ToolCall{ID: id, Function: openai.FunctionCall{Name: name, Arguments: args}}
	if index != nil {
		tc.Index = index
	}
	return tc
}

func TestStreamAccumulatorDropsUnusableFragments(t *testing.T) {
	two, neg := 2, -1

	t.Run("index 跳号不该产生幽灵调用", func(t *testing.T) {
		acc := newStreamAccumulator(nil)
		acc.mergeToolCall(openAIToolCall(&two, "c", "find", `{}`))
		// 0/1 是补出来的空槽，不能被当成"调用了无名工具"
		if got := len(acc.result().Message.ToolCalls); got != 1 {
			t.Fatalf("调用数 = %d，期望 1", got)
		}
	})

	t.Run("负 index 丢弃", func(t *testing.T) {
		acc := newStreamAccumulator(nil)
		acc.mergeToolCall(openAIToolCall(&neg, "c", "find", `{}`))
		if got := len(acc.result().Message.ToolCalls); got != 0 {
			t.Fatalf("调用数 = %d，期望 0", got)
		}
	})

	t.Run("完全空的碎片不开槽", func(t *testing.T) {
		acc := newStreamAccumulator(nil)
		acc.mergeToolCall(openAIToolCall(nil, "", "", ""))
		if len(acc.toolCalls) != 0 {
			t.Fatalf("空碎片不该开槽：%+v", acc.toolCalls)
		}
	})
}

func TestStreamAccumulatorIgnoresEmptyChunks(t *testing.T) {
	acc := newStreamAccumulator(nil)
	acc.consume(openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{}}},
	})
	if got := acc.result().Message.Content; got != "" {
		t.Fatalf("空 chunk 不该产生内容：%q", got)
	}
	if len(acc.result().Message.ToolCalls) != 0 {
		t.Fatalf("空 chunk 不该产生工具调用")
	}
}

// 端点没给 role 时要兜底：下游按 role 分派，空 role 会被端点拒绝。
func TestStreamAccumulatorDefaultsRoleToAssistant(t *testing.T) {
	acc := newStreamAccumulator(nil)
	if got := acc.result().Message.Role; got != "assistant" {
		t.Fatalf("应当兜底成 assistant，得到 %q", got)
	}
}

// onDelta 为空时不该 panic——非流式的 accumulator 复用同一条路径。
func TestStreamAccumulatorToleratesNilCallback(t *testing.T) {
	acc := newStreamAccumulator(nil)
	acc.consume(openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{
			{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "x", ReasoningContent: "y"}},
		},
	})
	if acc.result().Message.Content != "x" {
		t.Fatal("没有回调时也要正常累积")
	}
}
