package tools

import (
	"os"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/domain/harmony"
	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// 本机冒烟测试：拿真实工具链与真实工程跑一遍**只读**工具。
//
// 为什么要这种测试：单测覆盖的是合成夹具（能验证解析逻辑），但验证不了
// "在本机这个 DevEco / 这套 SDK 下，路径解析是不是真的对"。
// 这种"环境依赖"的测试默认跳过，只有显式给出工程目录时才跑：
//
//	ARKPERF_SMOKE_PROJECT=E:\.HuaWei\PerfLab go test ./internal/tools -run TestSmoke -v
//
// 刻意只跑只读工具：冒烟测试不该启模拟器、装机或改文件——
// 那些会改变机器状态，留给人手工验证。
func TestSmokeReadOnlyTools(t *testing.T) {
	project := os.Getenv("ARKPERF_SMOKE_PROJECT")
	if project == "" {
		t.Skip("未设置 ARKPERF_SMOKE_PROJECT，跳过本机冒烟（设置成一个真实 OpenHarmony 工程目录即可开启）")
	}
	if _, err := os.Stat(project); err != nil {
		t.Fatalf("ARKPERF_SMOKE_PROJECT=%s 不存在：%v", project, err)
	}

	tc := harmony.Discover(t.Context(), harmony.DiscoverOptions{})
	var all []kernel.Tool
	for _, tool := range Harmony(tc) {
		all = append(all, tool)
	}
	byName := map[string]kernel.Tool{}
	for _, tool := range all {
		byName[tool.Name()] = tool
	}

	// 这些工具在本机任何环境下都应该能跑通（不依赖设备在线、不依赖 hap）
	cases := []struct {
		name string
		args map[string]any
		// mustContain 为空时只要求"没报错"
		mustContain string
	}{
		{"harmony_toolchain_check", nil, ""},
		{"harmony_devices", nil, ""},
		{"harmony_emulator_list", nil, ""},
		{"harmony_emulator_catalog", nil, ""},
		{"harmony_image_check", nil, ""},
		{"harmony_project_profile", nil, ""},
		{"harmony_schema_check", nil, ""},
		{"harmony_api_lookup", map[string]any{"symbol": "UIAbility"}, "UIAbility"},
		{"harmony_build_doctor", map[string]any{"log": "Invalid value of 'DEVECO_SDK_HOME'"}, "sdk-home"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool, ok := byName[c.name]
			if !ok {
				t.Fatalf("工具未注册：%s", c.name)
			}
			res, err := tool.Execute(t.Context(), c.args, kernel.ToolCtx{CWD: project})
			t.Logf("%s：\n%s", c.name, clipOut(res.Output, 800))
			if err != nil {
				t.Fatalf("执行出错：%v", err)
			}
			if res.IsError {
				// 免审批的只读工具里，只有"确实没查到东西"才允许 IsError
				// （例如没装任何镜像）；但这里给的都是本机应有内容的场景
				t.Fatalf("返回 IsError=true，输出：%s", res.Output)
			}
			if c.mustContain != "" && !contains(res.Output, c.mustContain) {
				t.Fatalf("输出里应包含 %q", c.mustContain)
			}
		})
	}
}

// TestSmokeNonProjectIsRejected 确认"不是工程"会报错而不是给出空画像。
//
// 这条最容易退化：一旦画像函数改成"读不到就返回空结构"，
// 模型会拿到一份看起来正常、实际全是空的工程描述，然后基于它做错误判断。
func TestSmokeNonProjectIsRejected(t *testing.T) {
	tc := harmony.Discover(t.Context(), harmony.DiscoverOptions{})
	for _, tool := range Harmony(tc) {
		if tool.Name() != "harmony_project_profile" {
			continue
		}
		_, err := tool.Execute(t.Context(), nil, kernel.ToolCtx{CWD: t.TempDir()})
		if err == nil {
			t.Fatal("非工程目录必须报错，不能返回空画像")
		}
		return
	}
	t.Fatal("harmony_project_profile 未注册")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func clipOut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…（截断）"
}
