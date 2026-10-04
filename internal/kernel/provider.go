package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// ChatRequest 是一次模型调用。
type ChatRequest struct {
	Messages []Message
	// Tools 为空表示不向模型暴露任何工具（纯对话）。
	Tools []ToolSpec
	// OnDelta 非空时改走流式：模型每产出一段就回调一次，最后仍然返回完整消息。
	//
	// 挂在请求上而不是改 ChatFunc 的签名：ChatFunc 是可注入的测试接缝
	// （见 loop_test.go 的 scriptedChat），加一个参数会让所有假实现跟着改；
	// 而"要不要看增量"本来就只有两种状态，适合放在请求里。
	//
	// 回调是**同步**的，在接收流的那条 goroutine 上执行：实现里不要做重活，
	// 也不要阻塞太久——那会拖慢整个流。
	OnDelta func(kind DeltaKind, text string)
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
	// retryBase 是退避基数，指数增长。测试会把它调小——
	// 否则一个"连续失败 6 次"的用例要真等半分钟。
	retryBase time.Duration

	// retryAfterMu / lastRetryAfter 见 retry.go 的 retryAfterRecorder：
	// 传输层在请求线程里写，重试决策在调用线程里读。
	retryAfterMu   sync.Mutex
	lastRetryAfter string
}

// NewClient 构造客户端，默认超时 120s。
func NewClient(cfg ProviderConfig) *Client {
	return &Client{cfg: cfg, timeout: 120 * time.Second, retryBase: defaultRetryBase}
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

	var rt http.RoundTripper = http.DefaultTransport
	if useXAPIKey {
		rt = &headerTransport{base: rt, key: c.cfg.APIKey}
	}
	// 最外层记 Retry-After：go-openai 的错误类型不带响应头，
	// 想尊重它只能在这一层截。
	rt = &retryAfterRecorder{base: rt, c: c}

	oc.HTTPClient = &http.Client{Timeout: c.timeout, Transport: rt}
	return openai.NewClientWithConfig(oc)
}

// Chat 发起一次对话，失败时按瞬时错误策略重试。
//
// 两条独立的补偿叠在一起，职责不重叠：
//   - **401 鉴权降级**：换 X-Api-Key 再试一次。这不是"瞬时错误"，
//     而是"这个网关不认 Bearer"，所以它不占退避次数，且降级后被记住，
//     后续重试不再重复走一遍 Bearer。
//   - **瞬时错误重试**：网络抖动、408、429、5xx，指数退避 + 尊重 Retry-After。
//
// 首次失败后，如果错误是 401 就用 X-Api-Key 重试一次：
// 部分网关拒绝 Bearer 鉴权，这不是"密钥错了"，重试才能区分两者。
func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	useXAPIKey := false
	var lastErr error

	for attempt := 1; attempt <= maxChatAttempts; attempt++ {
		c.beginAttempt()
		resp, err := c.chat(ctx, req, useXAPIKey)
		if err == nil {
			return resp, nil
		}

		// 鉴权降级只做一次：降级后被记住，不再来回换
		if !useXAPIKey {
			if apiErr, ok := errors.AsType[*openai.APIError](err); ok && apiErr.HTTPStatusCode == http.StatusUnauthorized {
				if retried, err2 := c.chat(ctx, req, true); err2 == nil {
					return retried, nil
				} else {
					err = err2
					useXAPIKey = true
				}
			}
		}

		lastErr = err
		if attempt == maxChatAttempts || !c.retryable(err) {
			break
		}
		if waitErr := sleep(ctx, c.backoff(attempt)); waitErr != nil {
			// 等待期间被取消：把取消讲清楚，而不是报"重试失败"
			return ChatResponse{}, waitErr
		}
	}
	return ChatResponse{}, lastErr
}

func (c *Client) chat(ctx context.Context, req ChatRequest, useXAPIKey bool) (ChatResponse, error) {
	if req.OnDelta != nil {
		return c.chatStream(ctx, req, useXAPIKey)
	}
	return c.chatOnce(ctx, req, useXAPIKey)
}

// chatOnce 是一次非流式调用：等模型把话说完再返回整条消息。
func (c *Client) chatOnce(ctx context.Context, req ChatRequest, useXAPIKey bool) (ChatResponse, error) {
	tools, err := toOpenAITools(req.Tools)
	if err != nil {
		return ChatResponse{}, err // 已经点名是哪个工具，不再包一层
	}
	body := openai.ChatCompletionRequest{
		Model:    c.cfg.Model,
		Messages: toOpenAIMessages(req.Messages),
		Tools:    tools,
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

// chatStream 是流式调用：边收边回调，收完再拼成与非流式完全相同的返回值。
//
// 两条路径的**返回值形状必须一致**：上游（循环）不该知道走的是哪条，
// 否则"流式下能跑、非流式下跑不通"这类差异会一直藏在细节里。
func (c *Client) chatStream(ctx context.Context, req ChatRequest, useXAPIKey bool) (ChatResponse, error) {
	tools, err := toOpenAITools(req.Tools)
	if err != nil {
		return ChatResponse{}, err // 已经点名是哪个工具，不再包一层
	}
	body := openai.ChatCompletionRequest{
		Model:    c.cfg.Model,
		Messages: toOpenAIMessages(req.Messages),
		Tools:    tools,
		// 不带 include_usage 就拿不到 token 用量：多数端点只在最后一个
		// chunk 里给，而那个 chunk 需要显式要求才发。
		StreamOptions: &openai.StreamOptions{IncludeUsage: true},
	}

	stream, err := c.newClient(useXAPIKey).CreateChatCompletionStream(ctx, body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("chat %s (model %s): %w", c.cfg.BaseURL, c.cfg.Model, err)
	}
	defer func() { _ = stream.Close() }()

	acc := newStreamAccumulator(req.OnDelta)
	for {
		chunk, recvErr := stream.Recv()
		// io.EOF 是正常收尾（[DONE] 之后 SDK 就是返回它）。
		// 服务端没发 [DONE] 就断开也会落在这里——那就保留已经收到的部分，
		// 而不是把半句话当成失败丢掉。
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			// 已经吐过字了：重试会让同一段话再打一遍，所以这一轮就认了。
			// 用专门的错误类型把它标出来，让重试策略能识别并放过。
			if acc.hasEmitted() {
				return acc.result(), &streamInterruptedError{cause: recvErr}
			}
			return ChatResponse{}, fmt.Errorf("chat %s (model %s): %w", c.cfg.BaseURL, c.cfg.Model, recvErr)
		}
		acc.consume(chunk)
	}
	return acc.result(), nil
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
func toOpenAITools(specs []ToolSpec) ([]openai.Tool, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]openai.Tool, 0, len(specs))
	for _, s := range specs {
		// schema 是原样嵌入的，坏一个就**整个请求**发不出去——而编码器的报错
		// 是"invalid character '\n' after object key value pair"这种天书，
		// 看不出是哪个工具、哪个字段。在这里点名，把天书变成一句能行动的话。
		// （实测踩过：grep 的 schema 里有一个没转义的引号，所有任务全部失败。）
		if len(s.Parameters) > 0 && !json.Valid(s.Parameters) {
			return nil, fmt.Errorf("工具 %s 的参数 schema 不是合法 JSON（代码缺陷，请报告）", s.Name)
		}
		def := &openai.FunctionDefinition{Name: s.Name, Description: s.Description}
		if len(s.Parameters) > 0 {
			def.Parameters = s.Parameters
		}
		out = append(out, openai.Tool{Type: openai.ToolTypeFunction, Function: def})
	}
	return out, nil
}
