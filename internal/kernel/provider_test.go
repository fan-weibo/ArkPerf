package kernel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newServer 起一个假 OpenAI 端点。有了它，请求形状、鉴权、错误处理
// 全都能离线验证——不必连真实厂商，也不必烧钱。
func newServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func jsonReply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func okBody(content string) string {
	b, _ := json.Marshal(map[string]any{
		"model":   "test-model",
		"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": content}}},
		"usage":   map[string]int{"prompt_tokens": 11, "completion_tokens": 22},
	})
	return string(b)
}

func testClient(baseURL string) *Client {
	return NewClient(ProviderConfig{BaseURL: baseURL, APIKey: "sk-test", Model: "m"})
}

func TestChatRequestShape(t *testing.T) {
	var got *http.Request
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		jsonReply(200, okBody("hello"))(w, r)
	})

	resp, err := testClient(srv.URL+"/v1").Chat(t.Context(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if resp.Message.Content != "hello" {
		t.Fatalf("content: %q", resp.Message.Content)
	}
	if resp.Model != "test-model" {
		t.Fatalf("model: %q", resp.Model)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 22 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	if !strings.HasSuffix(got.URL.Path, "/chat/completions") {
		t.Fatalf("path: %q", got.URL.Path)
	}
	if got.Header.Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("auth: %q", got.Header.Get("Authorization"))
	}
}

// json.RawMessage 必须被原样嵌入 tools[].function.parameters，
// 一旦被二次编码成字符串，端点多半会静默忽略工具定义。
func TestChatSendsToolSchemaVerbatim(t *testing.T) {
	var body map[string]any
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		jsonReply(200, okBody("ok"))(w, r)
	})

	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	_, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools: []ToolSpec{{
			Name:        "read_file",
			Description: "读取文件",
			Parameters:  schema,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools not sent: %#v", body["tools"])
	}
	fn, ok := tools[0].(map[string]any)["function"].(map[string]any)
	if !ok {
		t.Fatalf("malformed tool entry: %#v", tools[0])
	}
	if fn["name"] != "read_file" {
		t.Fatalf("tool name: %v", fn["name"])
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters must be a JSON object, got %T (%v)", fn["parameters"], fn["parameters"])
	}
	if params["type"] != "object" {
		t.Fatalf("parameters lost content: %#v", params)
	}
}

func TestChatMapsToolCallsFromResponse(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
      {"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]}}]}`
	srv := newServer(t, jsonReply(200, body))

	resp, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "read a.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls: %+v", resp.Message.ToolCalls)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "read_file" {
		t.Fatalf("tool call mapping: %+v", tc)
	}
	// arguments 保持字符串原文，由循环负责解析
	if !strings.Contains(tc.Function.Arguments, "a.go") {
		t.Fatalf("arguments lost: %q", tc.Function.Arguments)
	}
}

func TestChatFallsBackToXApiKeyOn401(t *testing.T) {
	var seen []string
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" && r.Header.Get("X-Api-Key") == "" {
			seen = append(seen, "bearer")
			jsonReply(http.StatusUnauthorized, `{"error":{"message":"unauthorized"}}`)(w, r)
			return
		}
		seen = append(seen, "x-api-key")
		jsonReply(200, okBody("ok"))(w, r)
	})

	resp, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if resp.Message.Content != "ok" {
		t.Fatalf("content: %q", resp.Message.Content)
	}
	if len(seen) != 2 || seen[0] != "bearer" || seen[1] != "x-api-key" {
		t.Fatalf("expected bearer then x-api-key, got %v", seen)
	}
}

func TestChatErrorNamesEndpointAndModel(t *testing.T) {
	srv := newServer(t, jsonReply(http.StatusNotFound, `{"error":{"message":"model not found"}}`))

	_, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	// 报错里必须能看出"打的是哪个端点哪个模型"，否则 404 与网络故障无法区分
	if !strings.Contains(err.Error(), srv.URL) || !strings.Contains(err.Error(), "model m") {
		t.Fatalf("error must name endpoint and model: %v", err)
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Fatalf("error must carry the server message: %v", err)
	}
}

func TestChatEmptyChoicesIsAnError(t *testing.T) {
	srv := newServer(t, jsonReply(200, `{"choices":[]}`))

	if _, err := testClient(srv.URL).Chat(t.Context(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("empty choices must error, not return an empty answer")
	}
}

func TestChatRespectsContextCancellation(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := testClient(srv.URL).Chat(ctx, ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("cancelled context must abort the call")
	}
}
