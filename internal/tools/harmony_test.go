package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/domain/harmony"
	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

func names(tools []kernel.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name())
	}
	return out
}

func TestHarmonyExposesExpectedTools(t *testing.T) {
	got := strings.Join(names(Harmony(nil)), ",")
	want := strings.Join([]string{
		// 基础链路
		"harmony_toolchain_check", "harmony_devices", "harmony_build",
		"harmony_install", "harmony_launch", "harmony_logs", "harmony_shell",
		// 模拟器组
		"harmony_emulator_list", "harmony_emulator_catalog", "harmony_image_check",
		"harmony_emulator_start", "harmony_emulator_stop",
		"harmony_emulator_create", "harmony_emulator_delete",
		// 工程与设备组
		"harmony_uninstall", "harmony_project_profile", "harmony_schema_check",
		"harmony_build_doctor", "harmony_api_lookup", "harmony_sign", "harmony_lint",
		"harmony_device_test", "harmony_ui_regression",
	}, ",")
	if got != want {
		t.Fatalf("tools:\n got %s\nwant %s", got, want)
	}

	for _, tool := range Harmony(nil) {
		if strings.TrimSpace(tool.Description()) == "" {
			t.Fatalf("%s: 空描述，模型无法判断何时用它", tool.Name())
		}
		if len(tool.Parameters()) == 0 {
			t.Fatalf("%s: 缺少参数 schema", tool.Name())
		}
		if !strings.HasPrefix(tool.Name(), "harmony_") {
			t.Fatalf("%s: 鸿蒙域工具应以 harmony_ 前缀命名", tool.Name())
		}
	}
}

// 审批矩阵：只读探测免审批；会执行工程代码或改变设备状态的一律要审批。
// 这条规则必须锁住——它是"改第三方工程"场景下的安全底线。
func TestApprovalMatrix(t *testing.T) {
	want := map[string]bool{
		"harmony_toolchain_check": false,
		"harmony_devices":         false,
		"harmony_logs":            false,
		"harmony_build":           true, // 会执行工程自带的 hvigorfile.ts
		"harmony_install":         true, // 改变设备状态
		"harmony_launch":          true,
		"harmony_shell":           true,
		// 模拟器：查与看免审批，动进程/改文件要审批
		"harmony_emulator_list":    false,
		"harmony_emulator_catalog": false,
		"harmony_image_check":      false,
		"harmony_emulator_start":   true, // 拉起长期运行的进程
		"harmony_emulator_stop":    true, // 结束进程
		"harmony_emulator_create":  true, // 写模拟器目录与清单
		"harmony_emulator_delete":  true, // 破坏性
		// 工程与设备：读工程免审批，改设备/产文件要审批
		"harmony_uninstall":       true, // 删除设备上的应用
		"harmony_project_profile": false,
		"harmony_schema_check":    false,
		"harmony_build_doctor":    true, // 空参数下会自己跑一次构建（矩阵用空参数测）
		"harmony_api_lookup":      false,
		"harmony_sign":            true, // 生成新文件
		"harmony_lint":            false,
		"harmony_device_test":     true, // 装机+启动+卸载
		"harmony_ui_regression":   true, // 启动应用
	}

	for _, tool := range Harmony(nil) {
		expect, ok := want[tool.Name()]
		if !ok {
			t.Fatalf("未在审批矩阵中登记的工具：%s", tool.Name())
		}
		if got := tool.NeedsApproval(map[string]any{}); got != expect {
			t.Fatalf("%s: NeedsApproval=%v, want %v", tool.Name(), got, expect)
		}
	}
}

func TestToolchainCheckWithoutDiscoveryIsHonest(t *testing.T) {
	tool := toolchainCheckTool{tc: nil}
	res, err := tool.Execute(t.Context(), nil, kernel.ToolCtx{})
	if err != nil {
		t.Fatalf("没探测不算失败，不该报错：%v", err)
	}
	if !strings.Contains(res.Output, "未探测") {
		t.Fatalf("output should say it was not probed: %q", res.Output)
	}
}

func TestDevicesWithoutToolchainIsAnError(t *testing.T) {
	tool := devicesTool{tc: nil}
	if _, err := tool.Execute(t.Context(), nil, kernel.ToolCtx{}); err == nil {
		t.Fatal("没有工具链时查询设备应当报错")
	}
}

// 空目录里调用构建：要明确说"这不是 OpenHarmony 工程"，而不是抛一个
// exec 层的"命令不存在"——后者会把排查方向带偏。
func TestBuildOutsideProjectExplainsTheRealProblem(t *testing.T) {
	tool := buildTool{tc: nil}
	_, err := tool.Execute(t.Context(), nil, kernel.ToolCtx{CWD: t.TempDir()})
	if err == nil {
		t.Fatal("expected an error outside a project")
	}
	if !strings.Contains(err.Error(), "build-profile.json5") {
		t.Fatalf("error should name the project marker: %v", err)
	}
}

func TestBuildWithoutHvigorwPointsAtTheMissingTool(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "build-profile.json5"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool := buildTool{tc: &harmony.Toolchain{}}
	_, err := tool.Execute(t.Context(), nil, kernel.ToolCtx{CWD: dir})
	if err == nil {
		t.Fatal("expected an error without a builder")
	}
	if !strings.Contains(err.Error(), "hvigorw") {
		t.Fatalf("error must name the missing builder: %v", err)
	}
}

func TestLaunchRequiresBundle(t *testing.T) {
	tool := launchTool{tc: &harmony.Toolchain{}}
	if _, err := tool.Execute(t.Context(), map[string]any{}, kernel.ToolCtx{}); err == nil {
		t.Fatal("bundle 缺失时应当报错")
	}
}

func TestShellRequiresCommand(t *testing.T) {
	tool := shellTool{tc: &harmony.Toolchain{}}
	if _, err := tool.Execute(t.Context(), map[string]any{}, kernel.ToolCtx{}); err == nil {
		t.Fatal("command 缺失时应当报错")
	}
}

// 模型偶尔把数组写成字符串（"a b c"）或把数字写成字符串，
// 这类小毛病不值得让整个调用失败。
func TestStrSliceArgToleratesShapes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"native slice", []string{"-T", "tag"}, []string{"-T", "tag"}},
		{"json slice", []any{"-T", "tag"}, []string{"-T", "tag"}},
		{"single string", "-T tag", []string{"-T", "tag"}},
		{"blank string", "   ", nil},
		{"missing", nil, nil},
		{"wrong type", 42, nil},
		{"mixed json slice", []any{"-T", 42, "tag"}, []string{"-T", "tag"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strSliceArg(map[string]any{"k": tc.in}, "k")
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// 构建失败的原因几乎总在输出的末尾，所以 tail 必须保尾。
func TestTailKeepsTheEndAtLineBoundary(t *testing.T) {
	long := strings.Repeat("progress line\n", 100) + "ERROR: 找不到模块 entry"
	got := tail(long, 60)
	if !strings.Contains(got, "ERROR") {
		t.Fatalf("tail must keep the end: %q", got)
	}
	if !strings.Contains(got, "前文省略") {
		t.Fatalf("tail should mark the omission: %q", got)
	}
	if strings.Contains(got, "progress li\n") {
		t.Fatal("tail should cut at a line boundary, not mid-line")
	}
	if got := tail("short", 60); got != "short" {
		t.Fatalf("under limit must be verbatim: %q", got)
	}
}

func TestStartDirPrefersExplicitArg(t *testing.T) {
	tc := kernel.ToolCtx{CWD: `C:\cwd`}
	if got := startDir(map[string]any{}, tc); got != `C:\cwd` {
		t.Fatalf("fallback to CWD: %q", got)
	}
	if got := startDir(map[string]any{"projectDir": `D:\proj`}, tc); got != `D:\proj` {
		t.Fatalf("explicit dir should win: %q", got)
	}
}
