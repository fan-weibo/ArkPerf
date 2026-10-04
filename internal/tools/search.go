package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// grep / find 为什么必须是独立工具，而不是让模型用 run_command 凑：
//
//  1. ArkPerf 的 run_command **不经过 shell**，所以没有管道。搜一个大日志时
//     `grep xxx log | head` 这种自救路径根本不存在——而工具输出的截断是
//     "保留头尾、掐掉中间"，恰恰会把中间那些命中行丢掉。搜索必须有专用入口。
//  2. 噪声目录（node_modules / build / .git）必须在遍历时跳过。让模型自己拼
//     命令行参数做不到这件事，而扫进 node_modules 的结果既慢又没用。
//  3. 命中条数要有个上限。没有上限时一个大工程会把上下文一次性顶爆。
//
// 刻意**不 spawn ripgrep**：机器上装没装 rg 不确定，而"工具存在但跑不起来"
// 比"没有这个工具"更糟——模型会重试、会困惑。纯 Go 走 io/fs + 正则，
// 跨平台确定，零外部依赖。
const (
	// searchMaxFileBytes 是单个文件参与搜索的体量上限。
	// 超过它的基本是产物或日志归档，逐行扫过去只会拖慢整体。
	searchMaxFileBytes = 2 << 20 // 2 MB

	// searchLineBudget 是单条命中行的字符上限。
	// 命中展示的是"哪里匹配了"，不是"这一行有多长"。
	searchLineBudget = 300

	// searchDefaultMax 是命中条数的默认上限。
	searchDefaultMax = 100
)

// Search 返回搜索类工具（grep / find）。
func Search() []kernel.Tool {
	return []kernel.Tool{grepTool{}, findTool{}}
}

// ---------------------------------------------------------------- grep

type grepTool struct{}

func (grepTool) Name() string { return "grep" }

func (grepTool) Description() string {
	return "按正则搜索文件内容，返回 file:line: 内容。跳过 .git/node_modules/build 等噪声目录与二进制文件。" +
		"正则用 Go 语法（RE2，无回溯）。**要看日志或代码里的某个关键词时用这个工具**——" +
		"本机的 run_command 不经过 shell，没有管道可以用它过滤输出。"
}

func (grepTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Go 正则表达式（RE2），例如 \"ERROR|FATAL\" 或 \"[0-9]+ ms\""},
    "path": {"type": "string", "description": "要搜索的文件或目录，默认当前工作目录"},
    "glob": {"type": "string", "description": "只搜文件名匹配该 glob 的文件，例如 *.ts、*.log、module.json5"},
    "ignoreCase": {"type": "boolean", "description": "忽略大小写，默认 false"},
    "maxResults": {"type": "integer", "description": "最多返回多少条命中，默认 100"}
  },
  "required": ["pattern"],
  "additionalProperties": false
}`)
}

func (grepTool) NeedsApproval(map[string]any) bool { return false }

func (grepTool) Execute(ctx context.Context, args map[string]any, tc kernel.ToolCtx) (kernel.ToolResult, error) {
	pattern, err := requireString(args, "pattern")
	if err != nil {
		return kernel.ToolResult{}, fmt.Errorf("grep: %w", err)
	}
	// 原样留一份：回显给用户的是他写的那个 pattern，不是我们加了 (?i) 前缀的内部形式
	shown := pattern
	if optionalBool(args, "ignoreCase", false) {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		// 正则写错是模型常见的失误，如实说出错在哪，它通常能自己改
		return kernel.ToolResult{}, fmt.Errorf("grep: 正则无效：%w", err)
	}

	target := resolve(tc.CWD, optionalString(args, "path", "."))
	glob := optionalString(args, "glob", "")
	limit := optionalInt(args, "maxResults", searchDefaultMax)

	info, err := os.Stat(target)
	if err != nil {
		return kernel.ToolResult{}, fmt.Errorf("grep %s: %w", optionalString(args, "path", "."), err)
	}

	// 显式指定单个文件时忽略 glob：用户已经指名道姓了，
	// 再因为文件名不匹配而返回空结果只会让人困惑。
	stats := &searchStats{}
	var hits []string
	if !info.IsDir() {
		hits = append(hits, grepFile(ctx, target, re, tc.CWD, limit, stats)...)
	} else {
		err = walkFiles(ctx, target, glob, func(path string) bool {
			hits = append(hits, grepFile(ctx, path, re, tc.CWD, limit-len(hits), stats)...)
			return len(hits) < limit && ctx.Err() == nil // 够了就停
		})
		if err != nil {
			return kernel.ToolResult{}, fmt.Errorf("grep: %w", err)
		}
	}
	stats.hitLimit = len(hits) >= limit

	return kernel.ToolResult{Output: formatSearchOutput("grep", shown, stats, hits, limit)}, nil
}

// grepFile 扫一个文件，最多返回 budget 条命中。
func grepFile(ctx context.Context, path string, re *regexp.Regexp, cwd string, budget int, stats *searchStats) []string {
	data, ok := readSearchable(path, stats)
	if !ok {
		return nil
	}
	stats.files++

	var out []string
	for i, line := range strings.Split(string(data), "\n") {
		if budget <= 0 {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if !re.MatchString(line) {
			continue
		}
		// 行号从 1 起，与 read_file 的行号一致——两个工具的编号能对上，
		// 模型才能"grep 定位、read_file 细看"。
		out = append(out, fmt.Sprintf("%s:%d: %s", displayPath(cwd, path), i+1, clip(line, searchLineBudget)))
		budget--
	}
	return out
}

// ---------------------------------------------------------------- find

type findTool struct{}

func (findTool) Name() string { return "find" }

func (findTool) Description() string {
	return "按文件名查找文件，返回相对工作目录的路径（可直接交给 read_file）。" +
		"跳过 .git/node_modules/build 等噪声目录。pattern 含 * 或 ? 时按 glob 匹配，否则按子串匹配（忽略大小写）。"
}

func (findTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "文件名 glob（如 *.ts、module.json5、*_test.go）或子串（如 perflab）"},
    "path": {"type": "string", "description": "从哪个目录开始找，默认当前工作目录"},
    "maxResults": {"type": "integer", "description": "最多返回多少个，默认 100"}
  },
  "required": ["pattern"],
  "additionalProperties": false
}`)
}

func (findTool) NeedsApproval(map[string]any) bool { return false }

func (findTool) Execute(ctx context.Context, args map[string]any, tc kernel.ToolCtx) (kernel.ToolResult, error) {
	pattern, err := requireString(args, "pattern")
	if err != nil {
		return kernel.ToolResult{}, fmt.Errorf("find: %w", err)
	}
	target := resolve(tc.CWD, optionalString(args, "path", "."))
	limit := optionalInt(args, "maxResults", searchDefaultMax)

	info, err := os.Stat(target)
	if err != nil {
		return kernel.ToolResult{}, fmt.Errorf("find %s: %w", optionalString(args, "path", "."), err)
	}
	if !info.IsDir() {
		// 起点是文件时直接按文件名判定，别悄悄返回空——那看起来像"没找到"
		if matchName(filepath.Base(target), pattern) {
			return kernel.ToolResult{Output: displayPath(tc.CWD, target) + "\n"}, nil
		}
		return kernel.ToolResult{Output: fmt.Sprintf("%s 与 pattern %q 不匹配（它不是目录，find 只按文件名匹配）\n",
			displayPath(tc.CWD, target), pattern)}, nil
	}

	stats := &searchStats{}
	var hits []string
	err = walkFiles(ctx, target, "", func(path string) bool {
		// find 不读内容，所以"扫描"就是"看过多少个文件名"
		stats.files++
		if matchName(filepath.Base(path), pattern) {
			hits = append(hits, displayPath(tc.CWD, path))
		}
		return len(hits) < limit && ctx.Err() == nil
	})
	if err != nil {
		return kernel.ToolResult{}, fmt.Errorf("find: %w", err)
	}
	if len(hits) >= limit {
		stats.hitLimit = true
	}

	return kernel.ToolResult{Output: formatSearchOutput("find", pattern, stats, hits, limit)}, nil
}

// matchName 判断文件名是否匹配 pattern。
//
// 含 glob 元字符时走 filepath.Match，否则按子串匹配——模型写 "module.json5"
// 时想要的是"叫这个名字的文件"，而不是"字面量模式"。子串匹配忽略大小写：
// Windows 上文件名本来就大小写不敏感（搜 perflab 应该命中 PerfLab.json）。
func matchName(name, pattern string) bool {
	if strings.ContainsAny(pattern, "*?[") {
		if ok, err := filepath.Match(pattern, name); err == nil {
			return ok
		}
		return false
	}
	return strings.Contains(strings.ToLower(name), strings.ToLower(pattern))
}

// ---------------------------------------------------------------- 遍历与统计

// searchStats 记录一次搜索的规模，用来如实汇报"看了多少、跳过了什么"。
type searchStats struct {
	files    int // 实际扫描过的文件数
	binary   int // 因二进制而跳过
	oversize int // 因体量超限而跳过
	hitLimit bool
}

// walkFiles 遍历目录，对每个"值得搜的文件"回调 visit。
//
// 跳过规则集中在这里，grep 与 find 共用：
//   - 噪声目录（复用 list_dir 的 skipDirs，那是同一份判断）
//   - 隐藏目录（.git 已在 skipDirs 里，其余点目录同样不该进）
//   - 目录符号链接不跟随：WalkDir 的默认行为，避免环状链接把遍历卡死
//
// visit 返回 false 表示"够了，停"。
func walkFiles(ctx context.Context, root, glob string, visit func(path string) bool) error {
	errStop := fs.SkipAll
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// 单个目录读不动（权限等）不该让整次搜索失败：跳过它，
			// 用户从"扫描数偏少"能看出异常，比整个工具报错更有用。
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return errStop
		}
		if d.IsDir() {
			if path != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // 跳过设备文件、FIFO 等
		}
		if glob != "" {
			if ok, matchErr := filepath.Match(glob, d.Name()); matchErr != nil || !ok {
				return nil
			}
		}
		if !visit(path) {
			return errStop
		}
		return nil
	})
	if err != nil && err != errStop {
		return err
	}
	return nil
}

// readSearchable 读一个文件并判断它值不值得搜。
//
// 二进制必须跳过而不是硬扫：字节流里出现"命中"是常态，而那些结果对模型
// 毫无意义，只会挤掉真正的命中。
func readSearchable(path string, stats *searchStats) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	if info.Size() > searchMaxFileBytes {
		stats.oversize++
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	if isBinary(data) {
		stats.binary++
		return nil, false
	}
	return data, true
}

// isBinary 用"是否含 NUL 字节"判二进制。
//
// 只看开头 8KB：完整扫一遍大文件不值当，而 NUL 几乎总在开头出现。
func isBinary(data []byte) bool {
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	return strings.IndexByte(string(head), 0) >= 0
}

// formatSearchOutput 把结果排成模型好读的形状。
//
// 头部那行统计是刻意的：搜索最危险的失效模式是"返回空结果"——
// 它既可能是"真没有"，也可能是"路径错了 / 被跳过清单挡了"。
// 把扫描规模摆出来，这两种情况就能被区分开。
func formatSearchOutput(kind, pattern string, stats *searchStats, hits []string, limit int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %q — 扫描 %d 个文件，命中 %d 条", kind, pattern, stats.files, len(hits))
	if stats.binary > 0 || stats.oversize > 0 {
		fmt.Fprintf(&sb, "（跳过 二进制 %d · 超限 %d）", stats.binary, stats.oversize)
	}
	sb.WriteByte('\n')

	if len(hits) == 0 {
		sb.WriteString("没有命中。\n")
		if stats.files == 0 {
			// 一个文件都没扫到：多半是路径不对，而不是文件真没有内容
			sb.WriteString("一个文件都没扫到——先确认 path 指向的位置，或改用 list_dir 看一下结构。\n")
		}
		return sb.String()
	}

	sb.WriteString(strings.Join(hits, "\n"))
	sb.WriteByte('\n')

	if stats.hitLimit {
		// 说清"停在哪"和"怎么缩小"，而不是让模型以为这就是全部
		fmt.Fprintf(&sb, "... [已达上限 %d 条，可能还有更多命中。缩小 path、或用 glob 限定文件类型]\n", limit)
	}
	return sb.String()
}

// clip 按 rune 截断，避免切出半个汉字。
func clip(s string, limit int) string {
	s = strings.TrimRight(s, "\r")
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	rs := []rune(s)
	return string(rs[:limit]) + "…"
}

// displayPath 把绝对路径转成相对工作目录的形式；在目录之外时保持原样。
//
// 相对路径是给模型用的：它可以直接把结果粘进 read_file，
// 而 read_file 的相对路径就是按工作目录解析的。
func displayPath(cwd, p string) string {
	rel, err := filepath.Rel(cwd, p)
	if err != nil || rel == "" || strings.HasPrefix(rel, "..") {
		return p
	}
	return filepath.ToSlash(rel)
}

// optionalBool 读布尔参数。
//
// 与 optionalInt 同理：模型偶尔把 true 写成字符串 "true"，
// 只认 bool 会让这类参数被静默忽略掉。
func optionalBool(args map[string]any, key string, def bool) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	return def
}
