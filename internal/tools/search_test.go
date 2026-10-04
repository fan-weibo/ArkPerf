package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

func runGrep(t *testing.T, cwd string, args map[string]any) string {
	t.Helper()
	res, err := grepTool{}.Execute(t.Context(), args, kernel.ToolCtx{CWD: cwd})
	if err != nil {
		t.Fatalf("grep 返回错误：%v", err)
	}
	if res.IsError {
		t.Fatalf("grep 报业务失败：%s", res.Output)
	}
	return res.Output
}

func runFind(t *testing.T, cwd string, args map[string]any) string {
	t.Helper()
	res, err := findTool{}.Execute(t.Context(), args, kernel.ToolCtx{CWD: cwd})
	if err != nil {
		t.Fatalf("find 返回错误：%v", err)
	}
	return res.Output
}

// ---------------------------------------------------------------- 元信息

func TestSearchExposesExpectedTools(t *testing.T) {
	names := make([]string, 0, 2)
	for _, tool := range Search() {
		names = append(names, tool.Name())
		if tool.Description() == "" {
			t.Fatalf("%s 没有描述，模型无从选择", tool.Name())
		}
		if len(tool.Parameters()) == 0 {
			t.Fatalf("%s 没有参数 schema", tool.Name())
		}
	}
	if got := strings.Join(names, ","); got != "grep,find" {
		t.Fatalf("tools: %s", got)
	}
}

// 搜索是只读的。一旦要求审批，模型每查一次日志都要打断用户，Agent 就没法用了。
func TestSearchToolsNeverRequireApproval(t *testing.T) {
	for _, tool := range Search() {
		if tool.NeedsApproval(map[string]any{"pattern": "anything"}) {
			t.Fatalf("%s 不该要求审批", tool.Name())
		}
	}
}

// ---------------------------------------------------------------- grep

func TestGrepFindsMatchesWithFileAndLine(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.log", "第一行\nERROR: boom\n第三行\n")

	out := runGrep(t, dir, map[string]any{"pattern": "ERROR"})
	// 行号必须与 read_file 对齐，模型才能"grep 定位、read_file 细看"
	if !strings.Contains(out, "a.log:2: ERROR: boom") {
		t.Fatalf("缺少 file:line 形式的命中：%q", out)
	}
	if !strings.Contains(out, "命中 1 条") {
		t.Fatalf("缺少命中统计：%q", out)
	}
}

func TestGrepSkipsNoiseDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/a.txt", "needle\n")
	writeFile(t, dir, "node_modules/pkg/b.txt", "needle\n")
	writeFile(t, dir, "build/c.txt", "needle\n")
	writeFile(t, dir, ".git/d.txt", "needle\n")

	out := runGrep(t, dir, map[string]any{"pattern": "needle"})

	if !strings.Contains(out, "src/a.txt") {
		t.Fatalf("应当命中 src/a.txt：%q", out)
	}
	for _, noise := range []string{"node_modules", "build/", ".git"} {
		if strings.Contains(out, noise) {
			t.Fatalf("不该进 %s：%q", noise, out)
		}
	}
	if !strings.Contains(out, "命中 1 条") {
		t.Fatalf("命中数应当只有 1：%q", out)
	}
}

func TestGrepSkipsHiddenDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".hidden/a.txt", "needle\n")
	writeFile(t, dir, "visible.txt", "needle\n")

	out := runGrep(t, dir, map[string]any{"pattern": "needle"})
	if strings.Contains(out, ".hidden") {
		t.Fatalf("隐藏目录不该被搜：%q", out)
	}
	if !strings.Contains(out, "visible.txt") {
		t.Fatalf("应当命中 visible.txt：%q", out)
	}
}

func TestGrepGlobLimitsFileTypes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "needle\n")
	writeFile(t, dir, "b.log", "needle\n")

	out := runGrep(t, dir, map[string]any{"pattern": "needle", "glob": "*.log"})
	if strings.Contains(out, "a.txt") {
		t.Fatalf("glob 应当把 a.txt 排除掉：%q", out)
	}
	if !strings.Contains(out, "b.log") {
		t.Fatalf("应当命中 b.log：%q", out)
	}
}

// 二进制文件里出现"命中"是常态，而那些结果对模型毫无意义，只会挤掉真正的命中。
func TestGrepSkipsBinaryAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bin.dat", "needle\x00more\n")
	writeFile(t, dir, "text.txt", "needle\n")

	out := runGrep(t, dir, map[string]any{"pattern": "needle"})
	if strings.Contains(out, "bin.dat") {
		t.Fatalf("二进制文件不该被搜：%q", out)
	}
	if !strings.Contains(out, "跳过 二进制 1") {
		t.Fatalf("跳过了什么必须说出来：%q", out)
	}
}

func TestGrepSkipsOversizeFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "huge.log", strings.Repeat("x", searchMaxFileBytes+1))
	writeFile(t, dir, "small.txt", "needle\n")

	out := runGrep(t, dir, map[string]any{"pattern": "needle"})
	if !strings.Contains(out, "跳过 二进制 0 · 超限 1") {
		t.Fatalf("超限文件应当被计入统计：%q", out)
	}
	if !strings.Contains(out, "small.txt") {
		t.Fatalf("小文件应当照常搜：%q", out)
	}
}

func TestGrepRespectsMaxResults(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", strings.Repeat("needle\n", 10))

	out := runGrep(t, dir, map[string]any{"pattern": "needle", "maxResults": 3})
	if got := strings.Count(out, "a.txt:"); got != 3 {
		t.Fatalf("命中数 = %d，期望 3：%q", got, out)
	}
	// 到上限必须说清"可能还有更多"，否则模型会以为这就是全部
	if !strings.Contains(out, "已达上限 3 条") {
		t.Fatalf("上限提示缺失：%q", out)
	}
}

func TestGrepIgnoreCase(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "Needle\n")

	if out := runGrep(t, dir, map[string]any{"pattern": "needle"}); !strings.Contains(out, "命中 0 条") {
		t.Fatalf("默认应当区分大小写：%q", out)
	}
	out := runGrep(t, dir, map[string]any{"pattern": "needle", "ignoreCase": true})
	if !strings.Contains(out, "命中 1 条") {
		t.Fatalf("ignoreCase 应当生效：%q", out)
	}
	// 回显的是用户写的 pattern，不是内部加了 (?i) 的形式
	if strings.Contains(out, "(?i)") {
		t.Fatalf("不该把内部前缀暴露出来：%q", out)
	}
}

func TestGrepRejectsBadRegex(t *testing.T) {
	dir := t.TempDir()
	_, err := grepTool{}.Execute(t.Context(), map[string]any{"pattern": "([unclosed"}, kernel.ToolCtx{CWD: dir})
	if err == nil {
		t.Fatal("非法正则应当报错")
	}
	if !strings.Contains(err.Error(), "正则无效") {
		t.Fatalf("错误信息应当说明原因：%v", err)
	}
}

func TestGrepRequiresPattern(t *testing.T) {
	dir := t.TempDir()
	if _, err := (grepTool{}).Execute(t.Context(), map[string]any{}, kernel.ToolCtx{CWD: dir}); err == nil {
		t.Fatal("缺 pattern 应当报错")
	}
}

func TestGrepMissingPathIsError(t *testing.T) {
	dir := t.TempDir()
	if _, err := (grepTool{}).Execute(t.Context(), map[string]any{"pattern": "x", "path": "nope"}, kernel.ToolCtx{CWD: dir}); err == nil {
		t.Fatal("路径不存在应当报错")
	}
}

// 指名道姓给了一个文件时，不该再因为文件名不匹配 glob 而返回空。
func TestGrepSingleFileIgnoresGlob(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "needle\n")

	out := runGrep(t, dir, map[string]any{"pattern": "needle", "path": "a.txt", "glob": "*.log"})
	if !strings.Contains(out, "命中 1 条") {
		t.Fatalf("单文件目标应当忽略 glob：%q", out)
	}
}

// 搜索最危险的失效模式是"返回空结果"：既可能是真没有，也可能是路径错了。
func TestGrepExplainsEmptyResult(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "hello\n")

	out := runGrep(t, dir, map[string]any{"pattern": "absent"})
	if !strings.Contains(out, "没有命中") {
		t.Fatalf("空结果要明说：%q", out)
	}
	if !strings.Contains(out, "扫描 1 个文件") {
		t.Fatalf("要报告扫描规模：%q", out)
	}
	if strings.Contains(out, "一个文件都没扫到") {
		t.Fatalf("扫到了文件时不该给路径提示：%q", out)
	}
}

func TestGrepHintsWhenNothingWasScanned(t *testing.T) {
	dir := t.TempDir()
	// 目录里只有二进制文件：扫了 0 个文件，这与"扫了但没命中"是两件事
	writeFile(t, dir, "bin.dat", "\x00\x01\x02")

	out := runGrep(t, dir, map[string]any{"pattern": "x"})
	if !strings.Contains(out, "扫描 0 个文件") {
		t.Fatalf("应当报告扫描 0 个：%q", out)
	}
	if !strings.Contains(out, "一个文件都没扫到") {
		t.Fatalf("一个文件都没扫到时要给出排查方向：%q", out)
	}
}

func TestGrepTruncatesLongLines(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "needle"+strings.Repeat("x", 1000)+"\n")

	out := runGrep(t, dir, map[string]any{"pattern": "needle"})
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) > searchLineBudget+64 {
			t.Fatalf("命中行应当被截断，实际 %d 字符：%q", len([]rune(line)), line)
		}
	}
}

// 结果里的路径要能直接喂给 read_file，所以给相对工作目录的形式。
func TestGrepReturnsRelativePaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "sub/a.txt", "needle\n")

	out := runGrep(t, dir, map[string]any{"pattern": "needle"})
	if !strings.Contains(out, "sub/a.txt:1:") {
		t.Fatalf("应当是相对路径：%q", out)
	}
	if strings.Contains(out, dir) {
		t.Fatalf("不该出现绝对路径：%q", out)
	}
}

func TestGrepStopsOnCancelledContext(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "needle\n")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res, err := grepTool{}.Execute(ctx, map[string]any{"pattern": "needle"}, kernel.ToolCtx{CWD: dir})
	// 中断不是故障：如实返回"什么都没搜到"，不冒泡成错误
	if err != nil {
		t.Fatalf("取消不该报错：%v", err)
	}
	if strings.Contains(res.Output, "a.txt:") {
		t.Fatalf("已取消时不该继续搜：%q", res.Output)
	}
}

// ---------------------------------------------------------------- find

func TestFindMatchesGlob(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/a.ts", "")
	writeFile(t, dir, "src/b.js", "")

	out := runFind(t, dir, map[string]any{"pattern": "*.ts"})
	if !strings.Contains(out, "src/a.ts") {
		t.Fatalf("应当找到 a.ts：%q", out)
	}
	if strings.Contains(out, "b.js") {
		t.Fatalf("不该返回 b.js：%q", out)
	}
}

func TestFindMatchesSubstringCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "PerfLab.json", "")

	// Windows 上文件名大小写不敏感，搜 perflab 就该命中 PerfLab.json
	out := runFind(t, dir, map[string]any{"pattern": "perflab"})
	if !strings.Contains(out, "PerfLab.json") {
		t.Fatalf("子串匹配应当忽略大小写：%q", out)
	}
}

func TestFindSkipsNoiseDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/keep.ts", "")
	writeFile(t, dir, "node_modules/dep/drop.ts", "")
	writeFile(t, dir, ".hidden/also-drop.ts", "")

	out := runFind(t, dir, map[string]any{"pattern": "*.ts"})
	if !strings.Contains(out, "src/keep.ts") {
		t.Fatalf("应当找到 src/keep.ts：%q", out)
	}
	for _, noise := range []string{"node_modules", ".hidden"} {
		if strings.Contains(out, noise) {
			t.Fatalf("不该进 %s：%q", noise, out)
		}
	}
}

func TestFindRespectsMaxResults(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.ts", "b.ts", "c.ts", "d.ts"} {
		writeFile(t, dir, n, "")
	}

	out := runFind(t, dir, map[string]any{"pattern": "*.ts", "maxResults": 2})
	// 头部会回显 pattern，所以只数结果行（文件名以 .ts 结尾的行）
	var got int
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, ".ts") {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("返回数 = %d，期望 2：%q", got, out)
	}
	if !strings.Contains(out, "已达上限 2 条") {
		t.Fatalf("上限提示缺失：%q", out)
	}
}

func TestFindOnFileTarget(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.ts", "")

	if out := runFind(t, dir, map[string]any{"pattern": "*.ts", "path": "a.ts"}); !strings.Contains(out, "a.ts") {
		t.Fatalf("文件目标匹配时应当返回它：%q", out)
	}
	// 不匹配时要说明"find 只按文件名匹配"，而不是静默返回空
	out := runFind(t, dir, map[string]any{"pattern": "*.log", "path": "a.ts"})
	if !strings.Contains(out, "不匹配") {
		t.Fatalf("不匹配应当被解释：%q", out)
	}
}

func TestFindReportsNoMatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "")

	if out := runFind(t, dir, map[string]any{"pattern": "*.ts"}); !strings.Contains(out, "没有命中") {
		t.Fatalf("无命中要明说：%q", out)
	}
}

func TestFindRequiresPattern(t *testing.T) {
	dir := t.TempDir()
	if _, err := (findTool{}).Execute(t.Context(), map[string]any{}, kernel.ToolCtx{CWD: dir}); err == nil {
		t.Fatal("缺 pattern 应当报错")
	}
}

func TestFindReturnsForwardSlashes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sub", "deep"), "a.ts", "")

	out := runFind(t, dir, map[string]any{"pattern": "*.ts"})
	if !strings.Contains(out, "sub/deep/a.ts") {
		t.Fatalf("路径应当用正斜杠以便跨平台粘贴：%q", out)
	}
}

// 参数从 Go 代码直接构造时是 int / string，从 JSON 来的是 float64。
// 只认 float64 会让"直接调用工具"这条路径静默拿到默认值。
func TestSearchAcceptsNonJSONParameterTypes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", strings.Repeat("needle\n", 5))

	out := runGrep(t, dir, map[string]any{"pattern": "needle", "maxResults": int64(2)})
	if strings.Count(out, "a.txt:") != 2 {
		t.Fatalf("int64 形式的 maxResults 应当生效：%q", out)
	}
	out = runGrep(t, dir, map[string]any{"pattern": "needle", "ignoreCase": "true"})
	if !strings.Contains(out, "命中 5 条") {
		t.Fatalf("字符串形式的 ignoreCase 不应当被静默忽略：%q", out)
	}
}

func TestOptionalBoolDefaultsToFalse(t *testing.T) {
	if optionalBool(map[string]any{}, "k", false) {
		t.Fatal("缺省应当是 false")
	}
	if !optionalBool(map[string]any{"k": "YES"}, "k", false) {
		t.Fatal("应当接受 yes")
	}
	if optionalBool(map[string]any{"k": "0"}, "k", true) {
		t.Fatal("应当接受 0")
	}
}

func TestDisplayPathKeepsOutsidersAbsolute(t *testing.T) {
	cwd := t.TempDir()
	outside := filepath.Join(t.TempDir(), "x.txt")
	// 工作区之外的路径保持原样：强行相对化会得到一堆 ../
	if got := displayPath(cwd, outside); got != outside {
		t.Fatalf("工作区外应当保持绝对路径，得到 %q", got)
	}
	inside := filepath.Join(cwd, "a", "b.txt")
	if got := displayPath(cwd, inside); got != "a/b.txt" {
		t.Fatalf("工作区内应当是相对路径，得到 %q", got)
	}
}

func TestIsBinaryDetectsNulByte(t *testing.T) {
	if isBinary([]byte("plain text\n")) {
		t.Fatal("纯文本不该判成二进制")
	}
	if !isBinary([]byte("has\x00nul")) {
		t.Fatal("含 NUL 应当判成二进制")
	}
}

func TestSearchDoesNotTouchRealWorkingDirectory(t *testing.T) {
	// 起点的目录不存在时必须是错误，而不是悄悄退化成"搜工作目录"
	dir := t.TempDir()
	if _, err := (findTool{}).Execute(t.Context(), map[string]any{"pattern": "x", "path": "missing"}, kernel.ToolCtx{CWD: dir}); err == nil {
		t.Fatal("起点不存在应当报错")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("测试目录不该被动过：%v", err)
	}
}
