package kernel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

const (
	// maxChatAttempts 是一次对话的总尝试次数（含首次）。
	//
	// 6 次与 hmharness 对齐。瞬时故障通常一两次内自愈；而"服务端真的挂了"
	// 多试几次也没用，不如早点把错误摆给用户看。
	maxChatAttempts = 6

	// defaultRetryBase 是退避基数，按 1s、2s、4s… 指数增长。
	defaultRetryBase = time.Second

	// maxBackoff 是退避上限。没有它，第 6 次退避会到 32s，
	// 用户会觉得"卡死了"而不是"在重试"。
	maxBackoff = 30 * time.Second

	// maxRetryAfter 是听服务端话的上限。
	//
	// 有的服务端会给出一个荒唐的值（几分钟），照单全收等于挂死在这里。
	// 超过上限就按上限走——重试本来就是为了自愈，不是为了无限等。
	maxRetryAfter = 120 * time.Second
)

// streamInterruptedError 表示流在**已经开始产出内容之后**断掉。
//
// 它同时承担两件事：把真实原因带给用户，以及阻止重试——
// 重试会把同一段话从头再打一遍。宁可承认"这一轮没说完"。
type streamInterruptedError struct{ cause error }

func (e *streamInterruptedError) Error() string {
	return fmt.Sprintf("流式输出中断，回答不完整：%v", e.cause)
}

func (e *streamInterruptedError) Unwrap() error { return e.cause }

// retryable 判断一次失败值不值得重试。
//
// **默认不重试**——这一点是刻意的，而且起初写反过：先把"认不出来的错误"
// 当成可重试，结果"响应里没有 choices"这种**确定性**错误也会退避重试 6 次，
// 白等半分钟才把真正的原因摆给用户。确定性错误重试一百次还是同样的错。
//
// 只有明确的三类才重试：
//   - 网络层故障（连不上、DNS 失败、TCP 断了）
//   - 408 请求超时 / 429 限流
//   - 5xx 服务端故障
func (c *Client) retryable(err error) bool {
	// 流已经吐过字了：重试会让同一段话出现两遍
	if _, ok := errors.AsType[*streamInterruptedError](err); ok {
		return false
	}
	// 上下文被取消/超时：这是调用方要停，不是服务端抖动
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// 本地超时（连接超时、读超时）也不重试：那是"等太久了"，
	// 再等一轮只是把 120 秒变成 240 秒，不如让用户看到"太慢"这个事实。
	// 注意 *url.Error 的 Timeout() 会透传到内层错误，所以这一条同时覆盖
	// http.Client.Timeout 与连接超时两种。
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return false
	}
	if apiErr, ok := errors.AsType[*openai.APIError](err); ok {
		return retryableStatus(apiErr.HTTPStatusCode)
	}
	if reqErr, ok := errors.AsType[*openai.RequestError](err); ok {
		// SDK 只在**拿到响应之后**才构造 RequestError，所以状态码是真实的
		// HTTP 状态，不可能是 0。
		return retryableStatus(reqErr.HTTPStatusCode)
	}
	// 剩下的按网络层判：go-openai 对 client.Do 的失败是**原样返回**的
	// （不包成 RequestError），所以这里看到的是 *url.Error / net.OpError。
	return isNetworkError(err)
}

// isNetworkError 判断是否是网络层故障。
func isNetworkError(err error) bool {
	if _, ok := errors.AsType[*url.Error](err); ok {
		return true
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return true
	}
	_, ok := errors.AsType[net.Error](err)
	return ok
}

func retryableStatus(code int) bool {
	switch {
	case code == http.StatusRequestTimeout, code == http.StatusTooManyRequests:
		return true
	case code >= 500 && code <= 599:
		return true
	default:
		return false
	}
}

// backoff 返回第 attempt 次失败之后该等多久。
//
// 服务端给了 Retry-After 就听它的（钳制上限），否则指数退避。
// 用指数而不是固定值：429 往往是"你打太快了"，固定间隔会一直撞在限流上。
func (c *Client) backoff(attempt int) time.Duration {
	if v := c.consumeRetryAfter(); v != "" {
		if d := parseRetryAfter(v, time.Now()); d > 0 {
			return d
		}
	}
	base := c.retryBase
	if base <= 0 {
		base = defaultRetryBase
	}
	if attempt < 1 {
		attempt = 1
	}
	// 上限前先算，避免移位溢出：attempt 很大时 base<<attempt 会变成 0 或负数
	if attempt > 16 {
		return maxBackoff
	}
	d := base << (attempt - 1)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}

// parseRetryAfter 解析 Retry-After。按 RFC 9110 有两种合法形式：
// 秒数（"120"）与 HTTP-date（"Wed, 21 Oct 2026 07:28:00 GMT"）。
//
// 看不懂就当没给（返回 0，走指数退避）：宁可退避得保守一点，
// 也不要因为一个畸形头而把请求卡住。
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return clampRetryAfter(time.Duration(secs) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return clampRetryAfter(t.Sub(now))
	}
	return 0
}

func clampRetryAfter(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// sleep 等待退避时长，但 ctx 一取消就立刻返回。
//
// 少了 ctx 分支的话，用户按了 Ctrl+C 还得等完最后一次退避才停得下来——
// 而退避上限是 30 秒，那段时间界面看起来就是卡住了。
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryAfterRecorder 从响应头里抄下 Retry-After。
//
// 为什么需要它：go-openai 的 APIError / RequestError **都不带响应头**，
// 所以想尊重 Retry-After 就只能自己在 RoundTripper 这一层截下来。
//
// 它假定同一个 Client 同时只有一个请求在跑——内核的循环是串行的，
// 所以这个假定成立。真被并发使用的话，归属会错乱，后果只是退避时长不准，
// 不会影响返回结果。
type retryAfterRecorder struct {
	base http.RoundTripper
	c    *Client
}

func (t *retryAfterRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if resp != nil {
		if v := resp.Header.Get("Retry-After"); v != "" {
			t.c.retryAfterMu.Lock()
			t.c.lastRetryAfter = v
			t.c.retryAfterMu.Unlock()
		}
	}
	return resp, err
}

// beginAttempt 清掉上一轮记下的 Retry-After。
//
// 不清的话，第 1 次失败给出的 Retry-After 会被第 2、3 次继续沿用，
// 而那时服务端的建议早就过期了。
func (c *Client) beginAttempt() {
	c.retryAfterMu.Lock()
	c.lastRetryAfter = ""
	c.retryAfterMu.Unlock()
}

func (c *Client) consumeRetryAfter() string {
	c.retryAfterMu.Lock()
	defer c.retryAfterMu.Unlock()
	v := c.lastRetryAfter
	c.lastRetryAfter = ""
	return v
}
