package kernel

import (
	"strings"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/skill"
)

// 技能在提示词里的角色是"目录"，不是内容：只有名字、描述与路径进提示词，
// 正文由模型判定相关后自己用 read_file 读。这一组测试守的就是这条边界——
// 一旦有人把正文塞进来，20 个技能乘以每轮请求的钱就白省了。
func TestSystemPromptListsSkillsWithoutBody(t *testing.T) {
	skills := []skill.Skill{{
		Name:        "cold-start-baseline",
		Description: "建立冷启动基线并在改动后判定是否显著改善。",
		FilePath:    `E:\ArkPerf\skills\cold-start-baseline\SKILL.md`,
		Source:      skill.SourceBuiltin,
	}}

	got := SystemPrompt(`E:\ArkPerf`, nil, skills)

	for _, want := range []string{
		"## 可用技能",
		"cold-start-baseline",
		"建立冷启动基线并在改动后判定是否显著改善。",
		`E:\ArkPerf\skills\cold-start-baseline\SKILL.md`,
		skill.ReadToolName, // 必须点名用哪个工具读正文，否则模型只能猜
	} {
		if !strings.Contains(got, want) {
			t.Errorf("提示词缺少 %q：\n%s", want, got)
		}
	}
}

func TestSystemPromptOmitsSkillsSectionWhenEmpty(t *testing.T) {
	got := SystemPrompt("E:\\ArkPerf", nil, nil)
	if strings.Contains(got, "可用技能") {
		t.Errorf("没有技能时不该出现技能小节：\n%s", got)
	}
}

func TestSystemPromptStillCarriesHostFactsAndTools(t *testing.T) {
	// 加技能参数不该影响原有内容——这一段是"我在哪、不能做什么"，
	// 少了它模型会在 Windows 上照 Unix 习惯写命令然后反复重试。
	got := SystemPrompt(`E:\proj`, []ToolSpec{{Name: "read_file", Description: "读文件"}}, nil)

	for _, want := range []string{"## 运行环境", `E:\proj`, "read_file", "## 纪律"} {
		if !strings.Contains(got, want) {
			t.Errorf("提示词缺少 %q：\n%s", want, got)
		}
	}
}
