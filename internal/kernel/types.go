package kernel

import (
	"context"
	"encoding/json"
)

// ToolResult 是工具执行的统一结果。
//
// IsError 用 omitzero 而不是 omitempty：bool 的零值 false 就是"成功"，
// 序列化时省略它才符合"字段缺席=未出错"的语义。
type ToolResult struct {
	Output  string `json:"output"`
	IsError bool   `json:"isError,omitzero"`
}

// ToolCtx 是每次工具调用拿到的环境。
type ToolCtx struct {
	// CWD 是相对路径的解析基准。
	CWD string
	// Home 是 ArkPerf 的状态根，例如 ~/.arkperf。
	Home string
}

// Tool 是系统唯一的能力扩展面：内置工具、MCP 远端工具、域工具
// 都投影成这个形状，内核只认这个接口。
//
// Execute 返回 error 表示"工具坏了"（基础设施故障）；
// 业务上的失败（文件不存在、命令非零退出）应当返回 ToolResult{IsError: true}，
// 因为模型需要看到失败原因才能换策略。
type Tool interface {
	Name() string
	Description() string
	// Parameters 是 JSON Schema，直接投影给模型。
	Parameters() json.RawMessage
	// NeedsApproval 返回 true 时，循环必须先拿到用户批准才能执行。
	// 只读工具返回 false。
	NeedsApproval(args map[string]any) bool
	Execute(ctx context.Context, args map[string]any, tc ToolCtx) (ToolResult, error)
}

// ToolSpec 是投影给模型的工具描述（Registry 的对外形状）。
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall 是模型请求调用某个工具。
type ToolCall struct {
	ID       string       `json:"id"`
	Function FunctionCall `json:"function"`
}

// FunctionCall 是函数名与 JSON 编码的参数。
//
// Arguments 保持字符串形态而不是解析后的对象：模型有时会给出非法 JSON，
// 保持原文才能把"它到底写了什么"如实反馈回去。
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message 是会话消息，工具调用与工具结果都建在它上面。
//
// 刻意不做 `Name` 字段：OpenAI 规范里 name 属于已废弃的 function 角色，
// 现代端点在 tool 消息上收到它会直接 400。
type Message struct {
	Role       string     `json:"role"` // system | user | assistant | tool
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
}
