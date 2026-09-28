package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// Edit 返回文件写工具（edit_file / write_file）。
//
// 审批按**路径分级**判定，由 kernel.ClassifyPath 统一负责：
//   - 工作区内 → 免审批（agent 的正常作业范围）
//   - 状态根内 → 永远审批（改自己的配置最危险，也最不该免卡）
//   - 工作区外 → 审批
//
// 这条规则刻意不写在工具里：写在工具里，迟早有一个工具漏掉。
func Edit() []kernel.Tool {
	return []kernel.Tool{editFileTool{}, writeFileTool{}}
}

// ---------------------------------------------------------------- edit_file

type editFileTool struct{}

func (editFileTool) Name() string { return "edit_file" }

func (editFileTool) Description() string {
	return "把文件中的 old_string 精确替换为 new_string。" +
		"old_string 必须在文件里唯一出现，否则会被拒绝——这是防止改错地方。" +
		"修改工作区以外的文件、或修改 ArkPerf 自己的配置时需要审批。"
}

func (editFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "文件路径，相对当前工作目录或绝对路径"},
    "old_string": {"type": "string", "description": "要被替换的原文，必须唯一匹配（包含缩进与换行）"},
    "new_string": {"type": "string", "description": "替换后的内容"},
    "replaceAll": {"type": "boolean", "description": "为 true 时替换全部匹配；默认 false，要求唯一匹配"}
  },
  "required": ["path", "old_string", "new_string"],
  "additionalProperties": false
}`)
}

func (editFileTool) NeedsApproval(args map[string]any) bool {
	return writeNeedsApproval(strArg(args, "path"))
}

// ApprovalNote 说明审批取决于路径，而不是"一律免审批"或"一律要审批"。
func (editFileTool) ApprovalNote() string {
	return "按路径分级：工作区内免审批，状态根与工作区外需审批"
}

func (editFileTool) Execute(_ context.Context, args map[string]any, tc kernel.ToolCtx) (kernel.ToolResult, error) {
	path, err := resolvePath(strArg(args, "path"), tc.CWD)
	if err != nil {
		return kernel.ToolResult{}, err
	}

	// 注意：这里不能 TrimSpace —— old_string 里的缩进与首尾空行就是内容本身
	oldStr, newStr := rawStrArg(args, "old_string"), rawStrArg(args, "new_string")
	if oldStr == "" {
		return kernel.ToolResult{}, errors.New("old_string 不能为空")
	}
	if oldStr == newStr {
		return kernel.ToolResult{}, errors.New("old_string 与 new_string 相同，没有可修改的内容")
	}
	replaceAll := boolArg(args, "replaceAll", false)

	orig, err := os.ReadFile(path)
	if err != nil {
		return kernel.ToolResult{}, fmt.Errorf("读取 %s：%w", path, err)
	}
	content := string(orig)

	// 行尾风格要对齐，否则匹配永远失败，而报错看起来像"old_string 不存在"——
	// 明明文件里就有，用户和模型都极难自查。
	oldPatched, newPatched := matchLineEndings(content, oldStr, newStr)

	count := strings.Count(content, oldPatched)
	switch {
	case count == 0:
		return kernel.ToolResult{}, fmt.Errorf(
			"在 %s 里找不到 old_string。请先用 read_file 确认真实原文（注意缩进、空格与换行）", path)
	case count > 1 && !replaceAll:
		return kernel.ToolResult{}, fmt.Errorf(
			"old_string 在 %s 里出现了 %d 次，不唯一。请多带几行上下文让它唯一，或显式设 replaceAll=true", path, count)
	}

	updated := strings.Replace(content, oldPatched, newPatched, 1)
	if replaceAll {
		updated = strings.ReplaceAll(content, oldPatched, newPatched)
	}

	if err := os.WriteFile(path, []byte(updated), fileModeOf(path)); err != nil {
		return kernel.ToolResult{}, fmt.Errorf("写入 %s：%w", path, err)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "已修改 %s\n", path)
	if replaceAll {
		fmt.Fprintf(&sb, "替换 %d 处\n", count)
	} else {
		fmt.Fprintf(&sb, "第 %d 行替换 1 处\n", lineOf(content, oldPatched))
	}
	fmt.Fprintf(&sb, "片段行数：%d → %d", countLines(oldPatched), countLines(newPatched))
	return kernel.ToolResult{Output: sb.String()}, nil
}

// ---------------------------------------------------------------- write_file

type writeFileTool struct{}

func (writeFileTool) Name() string { return "write_file" }

func (writeFileTool) Description() string {
	return "写入（或覆盖）文件，父目录会自动创建。" +
		"覆盖 ArkPerf 状态根内的文件前会自动留一份 .bak 快照。" +
		"写工作区以外的文件、或写 ArkPerf 自己的配置时需要审批。"
}

func (writeFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "文件路径，相对当前工作目录或绝对路径"},
    "content": {"type": "string", "description": "完整文件内容"}
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`)
}

func (writeFileTool) NeedsApproval(args map[string]any) bool {
	return writeNeedsApproval(strArg(args, "path"))
}

// ApprovalNote 说明审批取决于路径。
func (writeFileTool) ApprovalNote() string {
	return "按路径分级：工作区内免审批，状态根与工作区外需审批"
}

func (writeFileTool) Execute(_ context.Context, args map[string]any, tc kernel.ToolCtx) (kernel.ToolResult, error) {
	path, err := resolvePath(strArg(args, "path"), tc.CWD)
	if err != nil {
		return kernel.ToolResult{}, err
	}
	content := rawStrArg(args, "content")

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return kernel.ToolResult{}, fmt.Errorf("创建父目录：%w", err)
	}

	existed := fileExists(path)
	backup, err := backupIfNeeded(path)
	if err != nil {
		return kernel.ToolResult{}, err
	}

	if err := os.WriteFile(path, []byte(content), fileModeOf(path)); err != nil {
		return kernel.ToolResult{}, fmt.Errorf("写入 %s：%w", path, err)
	}

	action := "已创建"
	if existed {
		action = "已覆盖"
	}
	out := fmt.Sprintf("%s %s（%d 字节，%d 行）", action, path, len(content), countLines(content))
	if backup != "" {
		out += "\n原文件已备份到：" + backup
	}
	return kernel.ToolResult{Output: out}, nil
}

// ---------------------------------------------------------------- 公共辅助

// writeNeedsApproval 按路径分级判定写操作是否需要审批。
//
// NeedsApproval 拿不到 ToolCtx，所以这里现取工作目录与状态根。
// 来源与 Runner 一致（os.Getwd 与 kernel.Home），不会出现两套判断标准。
func writeNeedsApproval(path string) bool {
	if strings.TrimSpace(path) == "" {
		return true // 连路径都没给，宁可问一句
	}
	cwd, err := os.Getwd()
	if err != nil {
		return true
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return kernel.ClassifyPath(path, cwd, kernel.Home()) != kernel.TierWorkspace
}

// resolvePath 把工具收到的路径解析成绝对路径（不要求文件已存在）。
func resolvePath(p, cwd string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path 不能为空")
	}
	if !filepath.IsAbs(p) {
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p), nil
}

// matchLineEndings 让待匹配文本与文件的行尾风格一致。
//
// 模型给的几乎总是 LF，而 Windows 上的文件常是 CRLF。不做这一步，
// 匹配会失败，而失败原因（行尾不可见）是这类工具最难自查的一类问题。
// 只在文件确实含 CRLF 且待匹配文本是纯 LF 时转换，不做无差别替换。
func matchLineEndings(fileContent, oldStr, newStr string) (string, string) {
	if !strings.Contains(fileContent, "\r\n") || strings.Contains(oldStr, "\r\n") {
		return oldStr, newStr
	}
	toCRLF := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
	}
	return toCRLF(oldStr), toCRLF(newStr)
}

// backupIfNeeded 覆盖状态根内的文件前留一份快照。
//
// 只对状态根做：那里放的是配置与会话，改坏了 ArkPerf 直接起不来。
// 工作区内的文件通常已在版本控制里，再撒一地 .bak 只会碍事。
func backupIfNeeded(path string) (string, error) {
	if !kernel.InHome(path) || !fileExists(path) {
		return "", nil
	}
	backup := fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
	if err := copyFile(path, backup); err != nil {
		return "", fmt.Errorf("备份 %s：%w", path, err)
	}
	return backup, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// fileModeOf 返回保留用的权限：文件存在则沿用原权限，否则 0644。
func fileModeOf(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		if mode := info.Mode().Perm(); mode != 0 {
			return mode
		}
	}
	return 0o644
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func lineOf(content, needle string) int {
	idx := strings.Index(content, needle)
	if idx < 0 {
		return 0
	}
	return strings.Count(content[:idx], "\n") + 1
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// rawStrArg 与 strArg 的区别：不做 TrimSpace。
// old_string / new_string / content 这类参数里的首尾空白就是内容本身。
func rawStrArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}
