// Package mcp 把 MCP 服务器上的工具投影成 ArkPerf 内核的 kernel.Tool。
//
// ArkPerf 的测量能力全部来自 MCP（本仓库 mcp-servers/ 下的 Python 服务），
// 所以本包是产品与外部能力之间唯一的桥：桥断了，Agent 就只剩下读文件的本事。
package mcp

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fan-weibo/ArkPerf/internal/execx"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

const (
	// DefaultTimeout 是单次工具调用超时。
	//
	// 360s 不是拍脑袋：测量工具要驱动模拟器跑完整采集，全量分析约 4 分钟。
	// 沿用模型请求那套 120s 会在服务端还在工作时把连接掐掉，白跑一次测量。
	DefaultTimeout = 360 * time.Second

	// toolPrefix 让远端工具在模型眼里与本地工具一眼可分。
	toolPrefix = "mcp_"

	// maxToolNameLen 是 OpenAI 工具名的长度上限。
	maxToolNameLen = 64

	// stderrTailBytes 是保留的服务器 stderr 尾巴长度。
	stderrTailBytes = 4096

	// shutdownGrace 是关闭时等待子进程自行退出的宽限期。
	shutdownGrace = 2 * time.Second

	clientName    = "arkperf"
	clientVersion = "0.1.0"
)

// ServerConfig 描述一个以子进程方式启动的 stdio MCP 服务器。
type ServerConfig struct {
	// Command 是解释器或可执行文件的绝对路径。
	Command string
	Args    []string
	Env     map[string]string
	// Trusted 为 true 时该服务器的工具免审批。
	Trusted bool
	// Timeout 覆盖单次调用超时。零值用 DefaultTimeout。
	Timeout time.Duration
}

// Conn 是一个已完成握手的 MCP 服务器连接。
type Conn struct {
	name    string
	cfg     ServerConfig
	session *sdk.ClientSession

	cmd    *exec.Cmd
	stderr *tailBuffer

	mu    sync.Mutex
	tools []kernel.Tool
	undo  []func()
	once  sync.Once
}

// Connect 启动子进程并完成 MCP 握手。
//
// 握手失败时会把子进程的 stderr 尾巴拼进错误：Python 服务的启动失败
// （缺包、路径错、解释器不对）几乎全靠这段输出才能定位。
func Connect(ctx context.Context, name string, cfg ServerConfig) (*Conn, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("mcp %s: command is empty", name)
	}

	cmd := exec.Command(cfg.Command, cfg.Args...)
	// 桌面版没有控制台：不设这个，每个 MCP 服务器都会弹出一个终端窗口
	// （实测启动时连 5 个服务器 → 弹 5 个终端）。详见 execx.HideWindow。
	execx.HideWindow(cmd)
	// Python 默认对 stdout 做块缓冲，而 stdio 上的 JSON-RPC 靠行分隔。
	// 不关掉缓冲，客户端会一直等一条永远不 flush 的响应——表现为"握手卡死"，
	// 且没有任何报错。这是接 Python MCP 服务器最常见的坑。
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1", "PYTHONIOENCODING=utf-8")
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp %s: stdin pipe: %w", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp %s: stdout pipe: %w", name, err)
	}
	tail := newTailBuffer(stderrTailBytes)
	// stderr 必须持续读走。不读的话管道缓冲区写满后子进程会阻塞在写日志上，
	// 症状是"工具偶尔莫名卡死"，且日志越多越容易复现。
	cmd.Stderr = tail

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp %s: start %s: %w", name, cfg.Command, err)
	}

	conn, err := Attach(ctx, name, cfg, &sdk.IOTransport{Reader: stdout, Writer: stdin})
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("mcp %s: %w%s", name, err, tail.suffix())
	}
	conn.cmd = cmd
	conn.stderr = tail
	return conn, nil
}

// Attach 用给定的 transport 完成握手。
//
// 单独导出是为了可测：测试用 sdk.NewInMemoryTransports() 在同一进程里
// 起一个假服务器，整条 MCP 链路（列工具、调工具、内容渲染、错误折叠）就
// 都能离线验证，不需要 Python、不需要设备、不需要网络。
func Attach(ctx context.Context, name string, cfg ServerConfig, t sdk.Transport) (*Conn, error) {
	client := sdk.NewClient(&sdk.Implementation{Name: clientName, Version: clientVersion}, nil)
	session, err := client.Connect(ctx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("handshake: %w", err)
	}
	return &Conn{name: name, cfg: cfg, session: session}, nil
}

// Name 返回服务器名。
func (c *Conn) Name() string { return c.name }

// Tools 返回已投影并注册的工具。
func (c *Conn) Tools() []kernel.Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]kernel.Tool(nil), c.tools...)
}

// RegisterInto 拉取远端工具列表并注册进 reg，返回注册数量。
//
// 反注册由 Close 统一负责，而不是在这里返回一堆清理函数：工具的生命周期
// 与进程的生命周期是同一件事，分开管理迟早会漏掉一半。
func (c *Conn) RegisterInto(ctx context.Context, reg *kernel.Registry) (int, error) {
	listed, err := c.session.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		return 0, fmt.Errorf("list tools: %w", err)
	}

	var (
		tools []kernel.Tool
		undo  []func()
	)
	for _, t := range listed.Tools {
		if t == nil || strings.TrimSpace(t.Name) == "" {
			continue
		}
		tool := c.project(t)
		tools = append(tools, tool)
		undo = append(undo, reg.Register(tool))
	}

	c.mu.Lock()
	c.tools = tools
	c.undo = undo
	c.mu.Unlock()
	return len(tools), nil
}

// project 把一个远端工具投影成本地 Tool。
//
// 命名规则刻意做得无聊：mcp_<server>_<tool>。看着有点重复
// （mcp_cpu_cpu_usage），但规则唯一、无歧义，也不会因为"聪明地去掉
// 重复前缀"而在两个服务器工具重名时撞车。
func (c *Conn) project(t *sdk.Tool) *remoteTool {
	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	if t.InputSchema != nil {
		if b, err := json.Marshal(t.InputSchema); err == nil && len(b) > 0 {
			schema = b
		}
	}

	desc := strings.TrimSpace(t.Description)
	if desc == "" {
		desc = "（远端工具未提供描述）"
	}

	return &remoteTool{
		conn:     c,
		name:     sanitizeToolName(toolPrefix + c.name + "_" + t.Name),
		original: t.Name,
		desc:     fmt.Sprintf("%s（MCP 服务器 %s）", desc, c.name),
		schema:   schema,
		trusted:  c.cfg.Trusted,
		timeout:  cmp.Or(c.cfg.Timeout, DefaultTimeout),
	}
}

// Stderr 返回服务器 stderr 的尾部内容，供状态命令诊断使用。
func (c *Conn) Stderr() string {
	if c.stderr == nil {
		return ""
	}
	return c.stderr.tail()
}

// Close 逆序反注册工具、关闭会话并回收子进程。可重复调用。
func (c *Conn) Close() error {
	var err error
	c.once.Do(func() { err = c.close() })
	return err
}

func (c *Conn) close() error {
	c.mu.Lock()
	undo := c.undo
	c.tools = nil
	c.undo = nil
	c.mu.Unlock()

	// 逆序反注册：后注册的先撤，与注册顺序对称。
	for i := len(undo) - 1; i >= 0; i-- {
		undo[i]()
	}

	var errs []error
	if c.session != nil {
		if err := c.session.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if c.cmd != nil && c.cmd.Process != nil {
		done := make(chan struct{})
		go func() {
			_ = c.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(shutdownGrace):
			// 关掉 stdin 后多数服务会自行退出；赖着不走的才强杀。
			_ = c.cmd.Process.Kill()
			<-done
		}
	}
	return errors.Join(errs...)
}

// remoteTool 是一个投影后的远端工具。
type remoteTool struct {
	conn     *Conn
	name     string // 投影名（模型看到的）
	original string // 服务器上的原名
	desc     string
	schema   json.RawMessage
	trusted  bool
	timeout  time.Duration
}

func (t *remoteTool) Name() string                      { return t.name }
func (t *remoteTool) Description() string               { return t.desc }
func (t *remoteTool) Parameters() json.RawMessage       { return t.schema }
func (t *remoteTool) Original() string                  { return t.original }
func (t *remoteTool) NeedsApproval(map[string]any) bool { return !t.trusted }

func (t *remoteTool) Execute(ctx context.Context, args map[string]any, _ kernel.ToolCtx) (kernel.ToolResult, error) {
	// 每次调用单独设超时：测量工具动辄跑几分钟，需要比模型请求宽松得多。
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	res, err := t.conn.session.CallTool(callCtx, &sdk.CallToolParams{
		Name:      t.original,
		Arguments: args,
	})
	if err != nil {
		return kernel.ToolResult{}, fmt.Errorf("mcp %s/%s: %w", t.conn.name, t.original, err)
	}
	return kernel.ToolResult{Output: renderResult(res), IsError: res.IsError}, nil
}

// renderResult 把 MCP 返回的内容折成一段文本。
//
// 优先用文本块（FastMCP 返回 dict 时正文就是 JSON 文本，最可读）；
// 非文本块（图片等）显式标注省略而不是静默丢弃——模型有权知道自己没看到什么。
func renderResult(res *sdk.CallToolResult) string {
	if res == nil {
		return "(empty result)"
	}

	var (
		sb    strings.Builder
		texts int
	)
	for _, c := range res.Content {
		switch v := c.(type) {
		case *sdk.TextContent:
			sb.WriteString(v.Text)
			texts++
			if !strings.HasSuffix(v.Text, "\n") {
				sb.WriteByte('\n')
			}
		default:
			fmt.Fprintf(&sb, "[非文本内容已省略：%T]\n", v)
		}
	}

	if texts == 0 && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			sb.Write(b)
		}
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		if res.IsError {
			return "(工具失败，但没有返回任何说明)"
		}
		return "(工具未返回内容)"
	}
	return out
}

// sanitizeToolName 把任意字符串压成合法的 OpenAI 工具名：
// 只保留 [A-Za-z0-9_]，超长则截断并附短哈希（避免截断后撞名）。
func sanitizeToolName(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	out := strings.Trim(sb.String(), "_")
	if out == "" {
		return toolPrefix + "tool"
	}
	if len(out) > maxToolNameLen {
		sum := sha256.Sum256([]byte(out))
		out = out[:maxToolNameLen-9] + "_" + hex.EncodeToString(sum[:])[:8]
	}
	return out
}

// tailBuffer 保留写入内容的最后 N 字节。
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *tailBuffer) tail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

// suffix 把 stderr 尾巴拼成错误后缀。
func (b *tailBuffer) suffix() string {
	t := b.tail()
	if t == "" {
		return ""
	}
	return "\n--- 服务器 stderr（尾部）---\n" + t
}
