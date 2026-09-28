package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// 只读工具不该打断用户：审批一旦落在读文件上，Agent 就没法用了。
func TestReadOnlyToolsNeverRequireApproval(t *testing.T) {
	for _, tool := range FS() {
		if tool.NeedsApproval(map[string]any{"path": "anything"}) {
			t.Fatalf("%s must not require approval", tool.Name())
		}
	}
}

func TestFSExposesExpectedTools(t *testing.T) {
	names := make([]string, 0, 2)
	for _, tool := range FS() {
		names = append(names, tool.Name())
		if tool.Description() == "" {
			t.Fatalf("%s has no description, the model cannot choose it", tool.Name())
		}
		if len(tool.Parameters()) == 0 {
			t.Fatalf("%s has no parameter schema", tool.Name())
		}
	}
	got := strings.Join(names, ",")
	if got != "read_file,list_dir" {
		t.Fatalf("tools: %s", got)
	}
}

func TestReadFileReturnsNumberedLines(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello\nworld\n")

	res, err := readFileTool{}.Execute(t.Context(), map[string]any{"path": "a.txt"}, kernel.ToolCtx{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", res.Output)
	}
	// 行号是模型能引用 file:line 的前提
	if !strings.Contains(res.Output, "    1| hello") {
		t.Fatalf("missing numbered line: %q", res.Output)
	}
	if !strings.Contains(res.Output, "    2| world") {
		t.Fatalf("missing second line: %q", res.Output)
	}
}

func TestReadFileTruncatesAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "big.txt", strings.Repeat("x", 5000))

	res, err := readFileTool{}.Execute(t.Context(), map[string]any{"path": "big.txt", "maxBytes": 100}, kernel.ToolCtx{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	// 悄悄丢内容会让模型基于残缺信息自信地胡说，必须显式标注
	if !strings.Contains(res.Output, "truncated") {
		t.Fatalf("truncation must be labelled: %q", res.Output)
	}
	if !strings.Contains(res.Output, "of 5000 bytes") {
		t.Fatalf("original size must be reported: %q", res.Output)
	}
}

func TestReadFileDoesNotEmitBrokenUTF8(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "cn.txt", strings.Repeat("中", 100))

	// 按字节截断会切断多字节字符，工具必须自行修复而不是把非法 UTF-8 丢给模型
	res, err := readFileTool{}.Execute(t.Context(), map[string]any{"path": "cn.txt", "maxBytes": 7}, kernel.ToolCtx{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(res.Output, '\uFFFD') {
		t.Fatalf("output contains replacement chars: %q", res.Output)
	}
}

func TestReadFileMissingIsAnError(t *testing.T) {
	_, err := readFileTool{}.Execute(t.Context(), map[string]any{"path": "nope.txt"}, kernel.ToolCtx{CWD: t.TempDir()})
	if err == nil {
		t.Fatal("missing file must return an error")
	}
	if !strings.Contains(err.Error(), "nope.txt") {
		t.Fatalf("error must name the path: %v", err)
	}
}

func TestReadFileRequiresPath(t *testing.T) {
	for _, args := range []map[string]any{nil, {}, {"path": ""}, {"path": 42}} {
		if _, err := (readFileTool{}).Execute(t.Context(), args, kernel.ToolCtx{}); err == nil {
			t.Fatalf("args %v must be rejected", args)
		}
	}
}

func TestListDirSkipsNoiseDirectories(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "entry/src/main/ets/Index.ets", "// page")
	writeFile(t, dir, "readme.md", "hi")
	for _, noise := range []string{"node_modules", ".git", "build", "oh_modules"} {
		writeFile(t, dir, filepath.Join(noise, "junk.js"), "junk")
	}

	res, err := listDirTool{}.Execute(t.Context(), map[string]any{}, kernel.ToolCtx{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, noise := range []string{"node_modules", ".git", "build", "oh_modules"} {
		if strings.Contains(res.Output, noise) {
			t.Fatalf("%s should be skipped:\n%s", noise, res.Output)
		}
	}
	if !strings.Contains(res.Output, "entry/") || !strings.Contains(res.Output, "readme.md") {
		t.Fatalf("real entries missing:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "2 entries") {
		t.Fatalf("entry count wrong:\n%s", res.Output)
	}
}

func TestListDirCapsEntries(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.go", "b.go", "c.go", "d.go"} {
		writeFile(t, dir, n, "x")
	}

	res, err := listDirTool{}.Execute(t.Context(), map[string]any{"maxEntries": 2}, kernel.ToolCtx{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Fatalf("truncation must be labelled:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "d.go") {
		t.Fatalf("should only show the first 2 entries:\n%s", res.Output)
	}
}

func TestListDirMissingIsAnError(t *testing.T) {
	_, err := listDirTool{}.Execute(t.Context(), map[string]any{"path": "nope"}, kernel.ToolCtx{CWD: t.TempDir()})
	if err == nil {
		t.Fatal("missing directory must return an error")
	}
}

// 模型偶尔把数字写成字符串。这类小毛病不值得让整个调用失败。
//
// 同时覆盖 Go 原生 int/int64：直接调用工具的代码路径（测试、未来的 UI）
// 传的就是这些类型，只认 float64 会让参数被静默忽略。
func TestOptionalIntAcceptsAllSources(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{float64(50), 50}, // 来自 JSON
		{int(50), 50},     // 来自 Go 代码
		{int64(50), 50},   // 来自 Go 代码
		{"50", 50},        // 模型写成字符串
		{" 50 ", 50},
		{float64(0), 42},
		{0, 42},
		{-3, 42},
		{"abc", 42},
		{nil, 42},
		{true, 42},
	}
	for _, tc := range cases {
		if got := optionalInt(map[string]any{"n": tc.in}, "n", 42); got != tc.want {
			t.Fatalf("optionalInt(%#v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestResolveHandlesAbsoluteAndRelative(t *testing.T) {
	if got := resolve(`E:\work`, "a/b.go"); got != filepath.Join(`E:\work`, "a", "b.go") {
		t.Fatalf("relative: %q", got)
	}
	abs := filepath.Join(`E:\other`, "x.go")
	if got := resolve(`E:\work`, abs); got != abs {
		t.Fatalf("absolute must win: %q", got)
	}
}
