package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func execEdit(t *testing.T, args map[string]any) (kernel.ToolResult, error) {
	t.Helper()
	return editFileTool{}.Execute(t.Context(), args, kernel.ToolCtx{CWD: t.TempDir()})
}

// ---------------------------------------------------------------- edit_file

func TestEditFileReplacesUniqueOccurrence(t *testing.T) {
	path := writeTemp(t, "a.txt", "line1\nline2\nline3\n")

	res, err := execEdit(t, map[string]any{
		"path": path, "old_string": "line2", "new_string": "LINE2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "line1\nLINE2\nline3\n" {
		t.Fatalf("文件内容不对：%q", got)
	}
	if !strings.Contains(res.Output, "第 2 行") {
		t.Fatalf("应该报出行号：%q", res.Output)
	}
}

// 不唯一就拒绝：宁可让模型多带几行上下文，也不要改错地方。
func TestEditFileRejectsAmbiguousMatch(t *testing.T) {
	path := writeTemp(t, "a.txt", "dup\nx\ndup\n")

	_, err := execEdit(t, map[string]any{
		"path": path, "old_string": "dup", "new_string": "y",
	})
	if err == nil {
		t.Fatal("重复匹配应被拒绝")
	}
	if !strings.Contains(err.Error(), "2 次") {
		t.Fatalf("应说明出现了几次：%v", err)
	}
	if !strings.Contains(err.Error(), "replaceAll") {
		t.Fatalf("应给出解决办法：%v", err)
	}
}

func TestEditFileReplaceAll(t *testing.T) {
	path := writeTemp(t, "a.txt", "dup\nx\ndup\n")

	if _, err := execEdit(t, map[string]any{
		"path": path, "old_string": "dup", "new_string": "y", "replaceAll": true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "y\nx\ny\n" {
		t.Fatalf("replaceAll 结果不对：%q", got)
	}
}

func TestEditFileRejectsMissingMatch(t *testing.T) {
	path := writeTemp(t, "a.txt", "hello\n")

	_, err := execEdit(t, map[string]any{
		"path": path, "old_string": "nope", "new_string": "x",
	})
	if err == nil {
		t.Fatal("找不到匹配应报错")
	}
	// 报错要指向解决办法：先用 read_file 看真实内容
	if !strings.Contains(err.Error(), "read_file") {
		t.Fatalf("应提示先用 read_file 确认：%v", err)
	}
}

func TestEditFileRejectsEmptyAndIdentical(t *testing.T) {
	path := writeTemp(t, "a.txt", "x\n")

	if _, err := execEdit(t, map[string]any{"path": path, "old_string": "", "new_string": "y"}); err == nil {
		t.Fatal("old_string 为空应报错")
	}
	if _, err := execEdit(t, map[string]any{"path": path, "old_string": "x", "new_string": "x"}); err == nil {
		t.Fatal("新旧相同应报错")
	}
}

// 缩进与首尾空行是内容本身：参数解码时绝不能 TrimSpace。
func TestEditFilePreservesIndentationAndBlankLines(t *testing.T) {
	content := "func a() {\n    return 1\n}\n"
	path := writeTemp(t, "a.ets", content)

	// 开头带两个空格——如果被 trim 掉就匹配不上
	if _, err := execEdit(t, map[string]any{
		"path": path, "old_string": "    return 1", "new_string": "    return 2",
	}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "func a() {\n    return 2\n}\n" {
		t.Fatalf("缩进被破坏：%q", got)
	}
}

// 行尾风格对齐：模型给 LF、文件是 CRLF 是最常见的 Windows 陷阱，
// 不做处理会永远匹配不上，而报错看起来像"old_string 不存在"，极难自查。
func TestEditFileMatchesAcrossLineEndings(t *testing.T) {
	path := writeTemp(t, "a.txt", "line1\r\nline2\r\nline3\r\n")

	if _, err := execEdit(t, map[string]any{
		"path": path, "old_string": "line2", "new_string": "LINE2",
	}); err != nil {
		t.Fatalf("CRLF 文件应当能匹配 LF 的 old_string：%v", err)
	}
	got := readFile(t, path)
	if got != "line1\r\nLINE2\r\nline3\r\n" {
		t.Fatalf("CRLF 应被保留：%q", got)
	}
}

func TestEditFileKeepsFileMode(t *testing.T) {
	// Windows 上没有 Unix 权限位（只有只读位），"沿用原权限"这件事
	// 在那里无法用文件系统的真实行为来验证——改测 fileModeOf 这个单元本身。
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 Unix 权限位，权限保留无法在此验证")
	}
	path := writeTemp(t, "a.sh", "#!/bin/sh\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := execEdit(t, map[string]any{"path": path, "old_string": "sh", "new_string": "bash"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("权限被改成 %v", info.Mode().Perm())
	}
}

// 覆盖写入时必须沿用文件原有的权限，而不是一律 0644：
// 把一个可执行脚本写成不可执行，是很容易被忽略的一类破坏。
func TestFileModeOfPrefersExistingFile(t *testing.T) {
	// Windows 上没有 Unix 权限位（只有只读位）：os.WriteFile 给的 0755
	// 在那里读回来就是 0666。这条断言只在不区分权限位的系统上才有意义。
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 Unix 权限位，权限沿用本质上是空操作")
	}
	dir := t.TempDir()

	executable := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := fileModeOf(executable); got != 0o755 {
		t.Fatalf("应沿用原权限 0755，实际 %v", got)
	}

	// 不存在的文件用默认 0644
	if got := fileModeOf(filepath.Join(dir, "nope.txt")); got != 0o644 {
		t.Fatalf("新文件应为 0644，实际 %v", got)
	}
}

func TestEditFileMissingFileReportsClearly(t *testing.T) {
	_, err := execEdit(t, map[string]any{
		"path": filepath.Join(t.TempDir(), "nope.txt"), "old_string": "a", "new_string": "b",
	})
	if err == nil {
		t.Fatal("文件不存在应报错")
	}
	if !strings.Contains(err.Error(), "读取") {
		t.Fatalf("应说明是读取失败：%v", err)
	}
}

// ---------------------------------------------------------------- write_file

func TestWriteFileCreatesWithParents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "new.txt")

	res, err := writeFileTool{}.Execute(t.Context(), map[string]any{
		"path": path, "content": "hello",
	}, kernel.ToolCtx{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "hello" {
		t.Fatalf("内容不对：%q", got)
	}
	if !strings.Contains(res.Output, "已创建") {
		t.Fatalf("应说明是新建：%q", res.Output)
	}
}

func TestWriteFileOverwrites(t *testing.T) {
	path := writeTemp(t, "a.txt", "old")

	res, err := writeFileTool{}.Execute(t.Context(), map[string]any{
		"path": path, "content": "new",
	}, kernel.ToolCtx{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "new" {
		t.Fatalf("未覆盖：%q", got)
	}
	if !strings.Contains(res.Output, "已覆盖") {
		t.Fatalf("应说明是覆盖：%q", res.Output)
	}
}

// 内容里的首尾空白必须原样保留，不能当成"多余空格"裁掉。
func TestWriteFilePreservesLeadingAndTrailingWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if _, err := (writeFileTool{}).Execute(t.Context(), map[string]any{
		"path": path, "content": "\n  indented  \n\n",
	}, kernel.ToolCtx{CWD: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "\n  indented  \n\n" {
		t.Fatalf("空白被破坏：%q", got)
	}
}

// 状态根内的文件被覆盖前必须留快照：配置改坏了 ArkPerf 直接起不来，
// 而那时用户连"用 agent 修回去"这条路都走不通。
func TestWriteFileBacksUpInsideHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ARKPERF_HOME", home)
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := writeFileTool{}.Execute(t.Context(), map[string]any{
		"path": path, "content": `{"new":true}`,
	}, kernel.ToolCtx{CWD: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "备份") {
		t.Fatalf("应说明备份位置：%q", res.Output)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), "config.json.bak-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("没有生成备份文件：%+v", entries)
	}
}

// 工作区内的文件不该撒 .bak：通常已在版本控制里，再留一堆副本只会碍事。
func TestWriteFileDoesNotBackUpOutsideHome(t *testing.T) {
	t.Setenv("ARKPERF_HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")

	if _, err := (writeFileTool{}).Execute(t.Context(), map[string]any{
		"path": path, "content": "x",
	}, kernel.ToolCtx{CWD: dir}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("不该产生额外文件：%+v", entries)
	}
}

// ---------------------------------------------------------------- 审批分级

func TestWriteApprovalTiers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ARKPERF_HOME", home)

	cwd := t.TempDir()
	// 切到工作区：writeNeedsApproval 用 os.Getwd 取当前目录
	t.Chdir(cwd)

	if writeNeedsApproval("entry/Index.ets") {
		t.Fatal("工作区内应免审批")
	}
	if !writeNeedsApproval(filepath.Join(home, "config.json")) {
		t.Fatal("状态根内应审批")
	}
	if !writeNeedsApproval(filepath.Join(t.TempDir(), "x.txt")) {
		t.Fatal("工作区外应审批")
	}
	if !writeNeedsApproval("") {
		t.Fatal("空路径宁可问一句")
	}
}

// ---------------------------------------------------------------- 小工具

func TestMatchLineEndingsOnlyWhenNeeded(t *testing.T) {
	// 文件是 LF：不动
	if a, _ := matchLineEndings("a\nb", "a\nb", "c\nd"); a != "a\nb" {
		t.Fatalf("LF 文件不该被转换：%q", a)
	}
	// old_string 自带 CRLF：尊重调用方，不转换
	if a, _ := matchLineEndings("a\r\nb", "a\r\nb", "c"); a != "a\r\nb" {
		t.Fatalf("自带 CRLF 时不应再转换：%q", a)
	}
	// 文件 CRLF + 输入 LF：转换
	newOld, newNew := matchLineEndings("a\r\nb", "a\nb", "x\ny")
	if newOld != "a\r\nb" || newNew != "x\r\ny" {
		t.Fatalf("转换结果不对：%q / %q", newOld, newNew)
	}
}

func TestCountLines(t *testing.T) {
	if countLines("") != 0 {
		t.Fatal("空串应为 0 行")
	}
	if countLines("a") != 1 {
		t.Fatal("无换行应为 1 行")
	}
	if countLines("a\nb\nc") != 3 {
		t.Fatal("两个换行应为 3 行")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
