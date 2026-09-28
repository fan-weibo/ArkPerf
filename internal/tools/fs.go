// Package tools 提供 ArkPerf 的内置工具。
//
// 当前只有只读文件访问。写文件属于"改变世界"的操作，
// 要等审批门禁与 git 快照齐备之后才放开——先给模型看得见的能力，
// 再给它动手的能力。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// FS 返回内置的只读文件工具。
func FS() []kernel.Tool {
	return []kernel.Tool{readFileTool{}, listDirTool{}}
}

// skipDirs 是遍历时跳过的噪声目录：它们通常有成千上万个文件，
// 列出来只会淹没真正有用的结构信息。
var skipDirs = map[string]bool{
	".git": true, ".hvigor": true, "node_modules": true, "oh_modules": true,
	"build": true, "dist": true, ".idea": true, ".next": true, ".cache": true,
}

type readFileTool struct{}

func (readFileTool) Name() string { return "read_file" }

func (readFileTool) Description() string {
	return "读取文本文件，返回带行号的内容（便于引用 file:line）。超长文件按 maxBytes 截断并显式标注。"
}

func (readFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "文件路径；相对路径按工作目录解析"},
    "maxBytes": {"type": "integer", "description": "最多读取的字节数，默认 65536"}
  },
  "required": ["path"],
  "additionalProperties": false
}`)
}

// NeedsApproval 恒为 false：只读不改变任何状态，不值得打断用户。
func (readFileTool) NeedsApproval(map[string]any) bool { return false }

func (readFileTool) Execute(_ context.Context, args map[string]any, tc kernel.ToolCtx) (kernel.ToolResult, error) {
	path, err := requireString(args, "path")
	if err != nil {
		return kernel.ToolResult{}, fmt.Errorf("read_file: %w", err)
	}

	data, err := os.ReadFile(resolve(tc.CWD, path))
	if err != nil {
		return kernel.ToolResult{}, fmt.Errorf("read_file %s: %w", path, err)
	}

	total := len(data)
	limit := optionalInt(args, "maxBytes", 64*1024)
	truncated := total > limit
	if truncated {
		data = data[:limit]
	}
	// 按字节截断可能切断多字节字符，先修掉残缺尾字节再交给模型，
	// 否则下游 JSON 校验会因非法 UTF-8 报一个莫名其妙的错。
	text := strings.ToValidUTF8(string(data), "")

	var sb strings.Builder
	lineNo := 0
	for line := range strings.SplitSeq(text, "\n") {
		lineNo++
		fmt.Fprintf(&sb, "%5d| %s\n", lineNo, line)
	}
	if truncated {
		fmt.Fprintf(&sb, "... [truncated: showing %d of %d bytes]\n", limit, total)
	}
	return kernel.ToolResult{Output: sb.String()}, nil
}

type listDirTool struct{}

func (listDirTool) Name() string { return "list_dir" }

func (listDirTool) Description() string {
	return "列出一个目录的直接子项（目录名带 / 后缀），跳过 .git/node_modules/build 等噪声目录。用于快速摸清工程结构。"
}

func (listDirTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "目录路径，默认当前工作目录"},
    "maxEntries": {"type": "integer", "description": "最多返回多少项，默认 200"}
  },
  "additionalProperties": false
}`)
}

func (listDirTool) NeedsApproval(map[string]any) bool { return false }

func (listDirTool) Execute(_ context.Context, args map[string]any, tc kernel.ToolCtx) (kernel.ToolResult, error) {
	dir := optionalString(args, "path", ".")

	entries, err := os.ReadDir(resolve(tc.CWD, dir))
	if err != nil {
		return kernel.ToolResult{}, fmt.Errorf("list_dir %s: %w", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			if skipDirs[e.Name()] {
				continue
			}
			names = append(names, e.Name()+"/")
			continue
		}
		names = append(names, e.Name())
	}
	slices.Sort(names)

	total := len(names)
	limit := optionalInt(args, "maxEntries", 200)
	truncated := total > limit
	if truncated {
		names = names[:limit]
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s — %d entries%s\n", dir, total, map[bool]string{true: " (truncated)", false: ""}[truncated])
	for _, n := range names {
		sb.WriteString(n)
		sb.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&sb, "... [truncated: showing first %d of %d]\n", limit, total)
	}
	return kernel.ToolResult{Output: sb.String()}, nil
}

// resolve 把相对路径按工作目录展开。
func resolve(cwd, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(cwd, path)
}

func requireString(args map[string]any, key string) (string, error) {
	s, ok := args[key].(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("%s is required and must be a non-empty string", key)
	}
	return s, nil
}

func optionalString(args map[string]any, key, def string) string {
	if s, ok := args[key].(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	return def
}

// optionalInt 读数字参数。
//
// 必须同时容忍四种来源：JSON 解出来的 float64、Go 代码构造的 int/int64、
// 以及模型偶尔写成字符串的 "200"。只认 float64 会让直接调用工具的
// 代码路径（测试、未来的 UI）静默拿到默认值——这类"参数被忽略"的 bug
// 在 Agent 里极难察觉，因为工具照常返回结果，只是用的不是你给的值。
func optionalInt(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return def
}
