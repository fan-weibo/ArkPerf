package kernel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// ChatRequest 是一次模型调用。
type ChatRequest struct {
	Messages []Message
	// Tools 为空表示不向模型暴露任何工具（纯对话）。
	Tools []ToolSpec
}

// ChatResponse 是一次模型调用的结果。
type ChatResponse struct {
	Message Message
	Model   string
	Usage   Usage
}

// Usage 是 token 用量。端点未上报时保持零值。
type Usage struct {
	PromptTokens     int `json:"promptTokens,omitzero"`
	CompletionTokens int `json:"completionTokens,omitzero"`
}

// Client 包装一个 OpenAI 兼容端点。
type Client struct {
	cfg     ProviderConfig
	timeout time.Duration
}

// NewClient 构造客户端，默认超时 120s。
func NewClient(cfg ProviderConfig) *Client {
	return &Client{cfg: cfg, timeout: 120 * time.Second}
}

// SetTimeout 覆盖单次请求超时。慢推理模型与长上下文需要放大。
func (c *Client) SetTimeout(d time.Duration) { c.timeout = d }

// headerTransport 在鉴权降级时把 Authorization 换成 X-Api-Key。
type headerTransport struct {
	base http.RoundTripper
	key  string
}

func (t *headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Del("Authorization")
	r.Header.Set("X-Api-Key", t.key)
	return t.base.RoundTrip(r)
}

func (c *Client) newClient(useXAPIKey bool) *openai.Client {
	oc := openai.DefaultConfig(c.cfg.APIKey)
	oc.BaseURL = strings.TrimRight(c.cfg.BaseURL, "/")
	hc := &http.Client{Timeout: c.timeout}
	if useXAPIKey {
		hc.Transport = &headerTransport{base: http.DefaultTransport, key: c.cfg.APIKey}
	}
	oc.HTTPClient = hc
	return openai.NewClientWithConfig(oc)
}

// Chat 发起一次非流式对话。
//
// 首次失败后，如果错误是 401 就用 X-Api-Key 重试一次：
// 部分网关拒绝 Bearer 鉴权，这不是"密钥错了"，重试才能区分两者。
func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	resp, err := c.chat(ctx, req, false)
	if err == nil {
		return resp, nil
	}

	if apiErr, ok := errors.AsType[*openai.APIError](err); ok && apiErr.HTTPStatusCode == http.StatusUnauthorized {
		if retried, err2 := c.chat(ctx, req, true); err2 == nil {
			return retried, nil
		}
	}
	return ChatResponse{}, err
}

func (c *Client) chat(ctx context.Context, req ChatRequest, useXAPIKey bool) (ChatResponse, error) {
	body := openai.ChatCompletionRequest{
		Model:    c.cfg.Model,
		Messages: toOpenAIMessages(req.Messages),
		Tools:    toOpenAITools(req.Tools),
	}

	resp, err := c.newClient(useXAPIKey).CreateChatCompletion(ctx, body)
	if err != nil {
		// 包一层端点信息：否则 404 与"网络不通"在报错里长得一模一样。
		return ChatResponse{}, fmt.Errorf("chat %s (model %s): %w", c.cfg.BaseURL, c.cfg.Model, err)
	}
	if len(resp.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("chat %s: empty choices in response", c.cfg.BaseURL)
	}

	return ChatResponse{
		Message: fromOpenAIMessage(resp.Choices[0].Message),
		Model:   resp.Model,
		Usage: Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
		},
	}, nil
}

func toOpenAI(t ToolCall) openai.ToolCall {
	return openai.ToolCall{
		ID:       t.ID,
		Type:     openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: t.Function.Name, Arguments: t.Function.Arguments},
	}
}

func fromOpenAI(t openai.ToolCall) ToolCall {
	return ToolCall{
		ID:       t.ID,
		Function: FunctionCall{Name: t.Function.Name, Arguments: t.Function.Arguments},
	}
}

func toOpenAIMessages(msgs []Message) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(msgs))
	for _, m := range msgs {
		om := openai.ChatCompletionMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, toOpenAI(tc))
		}
		out = append(out, om)
	}
	return out
}

func fromOpenAIMessage(m openai.ChatCompletionMessage) Message {
	out := Message{Role: m.Role, Content: m.Content}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, fromOpenAI(tc))
	}
	return out
}

// toOpenAITools 把注册表投影成请求里的 tools[]。
// json.RawMessage 实现了 json.Marshaler，会被原样嵌入，不会被二次编码成字符串。
func toOpenAITools(specs []ToolSpec) []openai.Tool {
	if len(specs) == 0 {
		return nil
	}
	out := make([]openai.Tool, 0, len(specs))
	for _, s := range specs {
		def := &openai.FunctionDefinition{Name: s.Name, Description: s.Description}
		if len(s.Parameters) > 0 {
			def.Parameters = s.Parameters
		}
		out = append(out, openai.Tool{Type: openai.ToolTypeFunction, Function: def})
	}
	return out
}
