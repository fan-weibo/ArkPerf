package mcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// startFakeServer 在同一进程里起一个假 MCP 服务器，并返回客户端侧 transport。
//
// 有了它，MCP 这一层可以完全离线验证：列工具、投影、调用、内容渲染、
// 失败折叠——不需要 Python、不需要设备、不需要网络，而且结果是确定的。
func startFakeServer(t *testing.T) sdk.Transport {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "fake-harmony", Version: "1.0"}, nil)

	echo := func(_ context.Context, _ *sdk.CallToolRequest, args map[string]any) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.TextContent{Text: fmt.Sprintf("echo:%v", args["text"])},
		}}, nil, nil
	}
	explicit := func(context.Context, *sdk.CallToolRequest, map[string]any) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{
			IsError: true,
			Content: []sdk.Content{&sdk.TextContent{Text: "内存泄漏检测需要 debug 签名应用"}},
		}, nil, nil
	}
	boom := func(context.Context, *sdk.CallToolRequest, map[string]any) (*sdk.CallToolResult, any, error) {
		return nil, nil, errors.New("device offline")
	}
	// 探针：把收到的 out_dir 原样回显，用来验证"相对路径按工作区补绝对"这一环
	// 真的作用到了**发往服务端**的参数上，而不只是函数级单测过了。
	outdirProbe := func(_ context.Context, _ *sdk.CallToolRequest, args map[string]any) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{
			&sdk.TextContent{Text: fmt.Sprintf("out_dir=%v", args["out_dir"])},
		}}, nil, nil
	}

	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"text": map[string]any{"type": "string"}},
		"required":   []string{"text"},
	}
	sdk.AddTool[map[string]any, any](server, &sdk.Tool{Name: "cpu_usage", Description: "CPU 占用快照", InputSchema: schema}, echo)
	sdk.AddTool[map[string]any, any](server, &sdk.Tool{Name: "memory-leak-check", Description: "内存泄漏判定"}, explicit)
	sdk.AddTool[map[string]any, any](server, &sdk.Tool{Name: "boom", Description: "总是失败"}, boom)
	sdk.AddTool[map[string]any, any](server, &sdk.Tool{Name: "outdir_probe", Description: "回显 out_dir"}, outdirProbe)

	ct, st := sdk.NewInMemoryTransports()
	session, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatalf("connect fake server: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return ct
}

func attachFake(t *testing.T, name string, trusted bool) (*Conn, *kernel.Registry) {
	t.Helper()
	conn, err := Attach(t.Context(), name, ServerConfig{Trusted: trusted}, startFakeServer(t))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	reg := kernel.NewRegistry()
	n, err := conn.RegisterInto(t.Context(), reg)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if n != 4 {
		t.Fatalf("expected 4 tools, registered %d", n)
	}
	return conn, reg
}

func TestRegisterIntoProjectsRemoteTools(t *testing.T) {
	_, reg := attachFake(t, "fake-harmony", true)

	want := []string{"mcp_fake_harmony_boom", "mcp_fake_harmony_cpu_usage", "mcp_fake_harmony_memory_leak_check", "mcp_fake_harmony_outdir_probe"}
	if got := reg.Names(); !slices.Equal(got, want) {
		t.Fatalf("tool names:\n got %v\nwant %v", got, want)
	}

	tool, ok := reg.Get("mcp_fake_harmony_cpu_usage")
	if !ok {
		t.Fatal("projected tool missing")
	}
	if !strings.Contains(tool.Description(), "CPU 占用快照") {
		t.Fatalf("description lost: %q", tool.Description())
	}
	// 描述里带服务器名，模型才能知道这份数据是谁给的
	if !strings.Contains(tool.Description(), "fake-harmony") {
		t.Fatalf("description should name the server: %q", tool.Description())
	}
	// schema 必须原样透传，否则模型不知道参数怎么填
	if !strings.Contains(string(tool.Parameters()), `"required":["text"]`) {
		t.Fatalf("schema not passed through: %s", tool.Parameters())
	}
}

func TestExecuteRoundTrip(t *testing.T) {
	_, reg := attachFake(t, "fake-harmony", true)

	tool, _ := reg.Get("mcp_fake_harmony_cpu_usage")
	res, err := tool.Execute(t.Context(), map[string]any{"text": "hello"}, kernel.ToolCtx{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Output)
	}
	if res.Output != "echo:hello" {
		t.Fatalf("output: %q", res.Output)
	}
}

// 工具自己声明失败时必须原样传成 IsError，而不是被当成成功的结果。
func TestExecutePassesThroughToolDeclaredFailure(t *testing.T) {
	_, reg := attachFake(t, "fake-harmony", true)

	tool, _ := reg.Get("mcp_fake_harmony_memory_leak_check")
	res, err := tool.Execute(t.Context(), map[string]any{}, kernel.ToolCtx{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("isError must be preserved")
	}
	if !strings.Contains(res.Output, "debug 签名") {
		t.Fatalf("failure explanation lost: %q", res.Output)
	}
}

// 服务器侧报错（协议层错误）必须冒泡成 Go error，
// 由循环折叠成一次可恢复的工具失败——不能静默变成"空结果"。
func TestExecuteSurfacesServerFailure(t *testing.T) {
	_, reg := attachFake(t, "fake-harmony", true)

	tool, _ := reg.Get("mcp_fake_harmony_boom")
	res, err := tool.Execute(t.Context(), map[string]any{}, kernel.ToolCtx{})
	if err == nil && !res.IsError {
		t.Fatalf("a server-side failure must not look like success: %+v", res)
	}
	if err != nil && !strings.Contains(err.Error(), "mcp fake-harmony/boom") {
		t.Fatalf("error must name the server and tool: %v", err)
	}
}

func TestApprovalDependsOnTrustedFlag(t *testing.T) {
	for _, tc := range []struct {
		trusted bool
		want    bool
	}{
		{trusted: true, want: false},
		{trusted: false, want: true},
	} {
		_, reg := attachFake(t, "fake-harmony", tc.trusted)
		tool, _ := reg.Get("mcp_fake_harmony_cpu_usage")
		if got := tool.NeedsApproval(map[string]any{}); got != tc.want {
			t.Fatalf("trusted=%v: NeedsApproval=%v, want %v", tc.trusted, got, tc.want)
		}
	}
}

func TestCloseUnregistersTools(t *testing.T) {
	conn, reg := attachFake(t, "fake-harmony", true)
	if reg.Len() != 4 {
		t.Fatalf("before close: %d tools", reg.Len())
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if reg.Len() != 0 {
		t.Fatalf("tools must be unregistered on close, %d left: %v", reg.Len(), reg.Names())
	}
	// 可重复关闭：CLI 里 defer 与显式关闭都可能触发
	if err := conn.Close(); err != nil {
		t.Fatalf("second close must be a no-op: %v", err)
	}
}

// 远端没给 schema 时要给模型一个合法的空对象 schema，而不是 nil。
func TestProjectFallsBackToEmptyObjectSchema(t *testing.T) {
	c := &Conn{name: "cpu", cfg: ServerConfig{}}
	tool := c.project(&sdk.Tool{Name: "no_schema"})

	if got := string(tool.Parameters()); got != `{"type":"object","properties":{}}` {
		t.Fatalf("fallback schema: %s", got)
	}
	if tool.Description() == "" {
		t.Fatal("a missing description must still yield something the model can read")
	}
}

func TestSanitizeToolName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"mcp_cpu_cpu_usage", "mcp_cpu_cpu_usage"},
		{"harmony-experience-agent.run", "harmony_experience_agent_run"},
		{"", "mcp_tool"},
		{"___", "mcp_tool"},
		{strings.Repeat("a", 100), strings.Repeat("a", 55) + "_" + hash8(strings.Repeat("a", 100))},
	}
	for _, tc := range cases {
		if got := sanitizeToolName(tc.in); got != tc.want {
			t.Fatalf("sanitizeToolName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// 超长名截断后必须仍唯一
	a := sanitizeToolName(strings.Repeat("a", 99) + "x")
	b := sanitizeToolName(strings.Repeat("a", 99) + "y")
	if a == b {
		t.Fatal("truncation must keep distinct names distinct")
	}
	if len(a) > maxToolNameLen {
		t.Fatalf("name too long: %d", len(a))
	}
}

func TestFromConfigSkipsDisabledAndSorts(t *testing.T) {
	no, yes := false, true
	cfg := &kernel.Config{MCPServers: map[string]kernel.MCPServerConfig{
		"z-cpu":  {Command: "python", Enabled: &yes},
		"a-jank": {Command: "python", Enabled: &yes},
		"off":    {Command: "python", Enabled: &no},
		"plain":  {Command: "python"}, // 未写 enabled = 启用
	}}

	got := make([]string, 0, 3)
	for _, s := range FromConfig(cfg) {
		got = append(got, s.Name)
	}
	want := []string{"a-jank", "plain", "z-cpu"}
	if !slices.Equal(got, want) {
		t.Fatalf("specs:\n got %v\nwant %v", got, want)
	}
}

// 单个服务器起不来不能让整体装配失败——测量能力应当部分可用。
func TestLoadAllReportsFailuresWithoutAborting(t *testing.T) {
	reg := kernel.NewRegistry()
	set := LoadAll(t.Context(), reg, []ServerSpec{
		{Name: "broken-a", Command: "definitely-not-a-real-binary-arkperf"},
		{Name: "broken-b", Command: "definitely-not-a-real-binary-arkperf"},
	}, "")
	t.Cleanup(func() { _ = set.Close() })

	if set.Connected() != 0 {
		t.Fatalf("nothing should connect: %+v", set.Results)
	}
	if len(set.Failed()) != 2 {
		t.Fatalf("both failures must be reported: %+v", set.Results)
	}
	for _, r := range set.Failed() {
		if r.Err == nil || !strings.Contains(r.Err.Error(), r.Name) {
			t.Fatalf("error must name the server %q: %v", r.Name, r.Err)
		}
	}
	if reg.Len() != 0 {
		t.Fatalf("no tools should be registered: %v", reg.Names())
	}
	if !strings.Contains(set.Summary(), "失败") {
		t.Fatalf("summary must mention failures: %q", set.Summary())
	}
}

func TestLoadAllEmptyConfig(t *testing.T) {
	set := LoadAll(t.Context(), kernel.NewRegistry(), nil, "")
	t.Cleanup(func() { _ = set.Close() })

	if got := set.Summary(); got != "未配置 MCP 服务器" {
		t.Fatalf("summary: %q", got)
	}
}

func TestConnectRejectsEmptyCommand(t *testing.T) {
	if _, err := Connect(t.Context(), "cpu", ServerConfig{}, ""); err == nil {
		t.Fatal("empty command must be rejected before spawning anything")
	}
}

func TestTailBufferKeepsLastBytes(t *testing.T) {
	b := newTailBuffer(8)
	if _, err := b.Write([]byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if got := b.tail(); got != "89abcdef" {
		t.Fatalf("tail: %q", got)
	}
	if !strings.Contains(b.suffix(), "stderr") {
		t.Fatalf("suffix must label the source: %q", b.suffix())
	}
	// 空尾巴不该污染错误信息
	if got := newTailBuffer(8).suffix(); got != "" {
		t.Fatalf("empty stderr must yield no suffix: %q", got)
	}
}

func hash8(s string) string {
	full := sanitizeToolName(s)
	return full[len(full)-8:]
}

// 超时是每个工具自己的事：测量工具要 4~6 分钟，不能被默认值卡住。
func TestTimeoutDefaultsAndOverride(t *testing.T) {
	c := &Conn{name: "cpu", cfg: ServerConfig{}}
	if got := c.project(&sdk.Tool{Name: "x"}).timeout; got != DefaultTimeout {
		t.Fatalf("default timeout: %v", got)
	}

	c2 := &Conn{name: "cpu", cfg: ServerConfig{Timeout: 30 * time.Second}}
	if got := c2.project(&sdk.Tool{Name: "x"}).timeout; got != 30*time.Second {
		t.Fatalf("override timeout: %v", got)
	}
}
