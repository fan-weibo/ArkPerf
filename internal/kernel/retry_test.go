package kernel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// fastClient 把退避压到毫秒级：一个"连续失败 6 次"的用例不该真等半分钟。
func fastClient(baseURL string) *Client {
	c := testClient(baseURL)
	c.retryBase = time.Millisecond
	return c
}

// flakyServer 前 fail 次返回 status，之后返回 200。
func flakyServer(t *testing.T, fail int, status int, body string) (*int, http.HandlerFunc) {
	t.Helper()
	calls := new(int)
	return calls, func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		if *calls <= fail {
			jsonReply(status, body)(w, nil)
			return
		}
		jsonReply(200, okBody("ok"))(w, nil)
	}
}

// ---------------------------------------------------------------- 重试行为

func TestChatRetriesTransientThenSucceeds(t *testing.T) {
	calls, handler := flakyServer(t, 2, http.StatusServiceUnavailable, `{"error":{"message":"upstream busy"}}`)
	srv := newServer(t, handler)

	resp, err := fastClient(srv.URL).Chat(t.Context(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("第三次应当成功：%v", err)
	}
	if resp.Message.Content != "ok" {
		t.Fatalf("content: %q", resp.Message.Content)
	}
	if *calls != 3 {
		t.Fatalf("应当请求 3 次，实际 %d 次", *calls)
	}
}

// 429 是"打太快了"，也是最该重试的一类。
func TestChatRetriesTooManyRequests(t *testing.T) {
	calls, handler := flakyServer(t, 1, http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`)
	srv := newServer(t, handler)

	if _, err := fastClient(srv.URL).Chat(t.Context(), ChatRequest{}); err != nil {
		t.Fatalf("限流应当被重试：%v", err)
	}
	if *calls != 2 {
		t.Fatalf("请求次数 = %d", *calls)
	}
}

// 确定性错误**不能**重试：密钥错、模型名错、参数错，重试一百次还是同样的错，
// 只会让用户多等半分钟才看到真正的原因。
func TestChatDoesNotRetryDeterministicErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"400 参数错", http.StatusBadRequest},
		{"404 模型名错", http.StatusNotFound},
		{"422 请求体错", http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := new(int)
			srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				*calls++
				jsonReply(tc.status, `{"error":{"message":"nope"}}`)(w, nil)
			})

			if _, err := fastClient(srv.URL).Chat(t.Context(), ChatRequest{}); err == nil {
				t.Fatal("应当报错")
			}
			if *calls != 1 {
				t.Fatalf("%d 不该被重试，实际请求 %d 次", tc.status, *calls)
			}
		})
	}
}

// 响应里没有 choices 是我们自己的代码报的确定性错误，同样不该重试。
// （这条曾经真的退避重试了 6 次，白等半分钟。）
func TestChatDoesNotRetryEmptyChoices(t *testing.T) {
	calls := new(int)
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		jsonReply(200, `{"choices":[]}`)(w, nil)
	})

	if _, err := fastClient(srv.URL).Chat(t.Context(), ChatRequest{}); err == nil {
		t.Fatal("空 choices 应当报错")
	}
	if *calls != 1 {
		t.Fatalf("空 choices 不该被重试，实际请求 %d 次", *calls)
	}
}

func TestChatGivesUpAfterMaxAttempts(t *testing.T) {
	calls := new(int)
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		jsonReply(http.StatusServiceUnavailable, `{"error":{"message":"down"}}`)(w, nil)
	})

	if _, err := fastClient(srv.URL).Chat(t.Context(), ChatRequest{}); err == nil {
		t.Fatal("一直失败应当最终报错")
	}
	if *calls != maxChatAttempts {
		t.Fatalf("应当在 %d 次后放弃，实际 %d 次", maxChatAttempts, *calls)
	}
}

// 401 只降级一次：降级之后若还是 401，那是密钥真不对，不该再试 6 遍。
func TestChatDoesNotLoopOn401(t *testing.T) {
	calls := new(int)
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		jsonReply(http.StatusUnauthorized, `{"error":{"message":"bad key"}}`)(w, nil)
	})

	if _, err := fastClient(srv.URL).Chat(t.Context(), ChatRequest{}); err == nil {
		t.Fatal("401 应当报错")
	}
	// 一次 Bearer + 一次 X-Api-Key，到此为止
	if *calls != 2 {
		t.Fatalf("401 只该尝试两次（Bearer + X-Api-Key），实际 %d 次", *calls)
	}
}

// 等待退避期间被取消，要立刻返回，而不是把最后一次退避也等完。
func TestChatAbortsRetryOnContextCancel(t *testing.T) {
	srv := newServer(t, jsonReply(http.StatusServiceUnavailable, `{"error":{"message":"down"}}`))

	c := testClient(srv.URL)
	c.retryBase = 10 * time.Second // 远超测试能接受的时间：能返回就说明没真等
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := c.Chat(ctx, ChatRequest{})
	if err == nil {
		t.Fatal("应当报错")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应当把取消原因带出来，得到 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("取消后应当立刻返回，实际用了 %v", elapsed)
	}
}

// ---------------------------------------------------------------- Retry-After

// go-openai 的错误类型不带响应头，所以我们自己在传输层截。这条守的是"截到了"。
func TestRetryAfterHeaderIsCaptured(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		jsonReply(http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`)(w, nil)
	})

	c := testClient(srv.URL)
	// 直接开一次请求，不走 Chat 的重试循环，免得值被消费掉
	_, _ = c.newClient(false).CreateChatCompletion(t.Context(), openai.ChatCompletionRequest{
		Model:    "m",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
	})

	if got := c.consumeRetryAfter(); got != "3" {
		t.Fatalf("应当截到 Retry-After，得到 %q", got)
	}
}

func TestBackoffPrefersRetryAfterThenClearsIt(t *testing.T) {
	c := testClient("http://localhost")
	c.retryBase = time.Millisecond
	c.lastRetryAfter = "7"

	if d := c.backoff(1); d != 7*time.Second {
		t.Fatalf("应当听服务端的 Retry-After，得到 %v", d)
	}
	// 消费掉之后回到指数退避：不清的话，第 1 次给的 7 秒会被后续每次沿用
	if d := c.backoff(1); d != time.Millisecond {
		t.Fatalf("Retry-After 应当只生效一次，得到 %v", d)
	}
}

func TestBackoffIsExponentialAndCapped(t *testing.T) {
	c := testClient("http://localhost")
	c.retryBase = time.Second

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, maxBackoff}
	for i, w := range want {
		if got := c.backoff(i + 1); got != w {
			t.Fatalf("第 %d 次退避 = %v，期望 %v", i+1, got, w)
		}
	}
	// 次数极大时不能溢出成 0 或负数
	if got := c.backoff(999); got != maxBackoff {
		t.Fatalf("极大次数应当钳到上限，得到 %v", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 4, 3, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"秒数", "120", 120 * time.Second},
		{"零秒", "0", 0},
		{"负数按 0", "-5", 0},
		{"HTTP-date", "Sat, 04 Oct 2026 03:00:30 GMT", 30 * time.Second},
		{"已经过期的日期按 0", "Sat, 04 Oct 2026 02:59:00 GMT", 0},
		{"超过上限被钳制", "99999", maxRetryAfter},
		{"看不懂就当没给", "soon", 0},
		{"空值", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.in, now); got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = %v，期望 %v", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- 判定表

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"网络：连接被拒（url.Error 包着）", &url.Error{Op: "Post", Err: errors.New("connection refused")}, true},
		{"网络：DNS 失败", &net.DNSError{Err: "no such host", Name: "x"}, true},
		{"网络：本地超时不重试", &url.Error{Op: "Post", Err: timeoutErr{}}, false},
		{"上下文取消", context.Canceled, false},
		{"上下文超时", context.DeadlineExceeded, false},
		{"503 服务端故障", &openai.APIError{HTTPStatusCode: 503}, true},
		{"429 限流", &openai.APIError{HTTPStatusCode: 429}, true},
		{"408 请求超时", &openai.APIError{HTTPStatusCode: 408}, true},
		{"400 参数错", &openai.APIError{HTTPStatusCode: 400}, false},
		{"401 密钥错", &openai.APIError{HTTPStatusCode: 401}, false},
		{"RequestError 502", &openai.RequestError{HTTPStatusCode: 502}, true},
		{"RequestError 404", &openai.RequestError{HTTPStatusCode: 404}, false},
		{"流已吐字：不重试", &streamInterruptedError{cause: errors.New("unexpected EOF")}, false},
		// 这条是**默认不重试**的守门：认不出来的错误多半是自己代码报的确定性错误
		{"认不出来的错误不重试", errors.New("empty choices in response"), false},
		{"被包过的确定性错误", fmt.Errorf("chat x: %w", errors.New("boom")), false},
	}

	c := testClient("http://localhost")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.retryable(tc.err); got != tc.want {
				t.Fatalf("retryable = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- 流式

// 流已经开始吐字之后断掉：**不能**重试，否则同一段话会再打一遍。
func TestChatStreamDoesNotRetryAfterOutputStarted(t *testing.T) {
	calls := new(int)
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		*calls++
		// 声明一个远大于实际写入的长度：客户端会在读完后得到 unexpected EOF，
		// 模拟"流中途断了"而不是正常收尾。
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		writeChunks(w, deltaChunk(t, map[string]any{"content": "说到一半"}))
	})

	var got []string
	_, err := fastClient(srv.URL).Chat(t.Context(), ChatRequest{
		OnDelta: func(_ DeltaKind, text string) { got = append(got, text) },
	})
	if err == nil {
		t.Fatal("流中断应当报错")
	}
	if _, ok := errors.AsType[*streamInterruptedError](err); !ok {
		t.Fatalf("应当标成「流已开始」以便阻止重试，得到 %v", err)
	}
	if *calls != 1 {
		t.Fatalf("已经吐过字就不能重试，实际请求 %d 次", *calls)
	}
	if strings.Join(got, "") != "说到一半" {
		t.Fatalf("已收到的内容应当保留：%v", got)
	}
}

// 什么都还没吐出来就失败：可以安全重试。
func TestChatStreamRetriesWhenNothingEmittedYet(t *testing.T) {
	calls := new(int)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if *calls == 1 {
			jsonReply(http.StatusServiceUnavailable, `{"error":{"message":"busy"}}`)(w, r)
			return
		}
		sseReply(deltaChunk(t, map[string]any{"content": "好了"}))(w, r)
	})

	var got []string
	resp, err := fastClient(srv.URL).Chat(t.Context(), ChatRequest{
		OnDelta: func(_ DeltaKind, text string) { got = append(got, text) },
	})
	if err != nil {
		t.Fatalf("第二次应当成功：%v", err)
	}
	if resp.Message.Content != "好了" || strings.Join(got, "") != "好了" {
		t.Fatalf("content=%q deltas=%v", resp.Message.Content, got)
	}
	if *calls != 2 {
		t.Fatalf("请求次数 = %d", *calls)
	}
}
