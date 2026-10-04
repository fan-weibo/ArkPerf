package skill

import (
	"strings"
	"testing"
)

func TestRenderBlockEmptyWhenNoSkills(t *testing.T) {
	// 没技能时连标题都不该出现：一个空小节只是白占提示词位置。
	if got := RenderBlock(nil); got != "" {
		t.Errorf("无技能时应当返回空串，实际 %q", got)
	}
	if got := RenderBlock([]Skill{}); got != "" {
		t.Errorf("空切片时应当返回空串，实际 %q", got)
	}
}

func TestRenderBlockCarriesNameDescriptionAndPath(t *testing.T) {
	got := RenderBlock([]Skill{
		{
			Name:        "cold-start-baseline",
			Description: "建立冷启动基线并与改动后对比。",
			FilePath:    `E:\ArkPerf\skills\cold-start-baseline\SKILL.md`,
			Source:      SourceBuiltin,
		},
	})

	for _, want := range []string{
		"## 可用技能",
		"cold-start-baseline",
		"建立冷启动基线并与改动后对比。",
		`E:\ArkPerf\skills\cold-start-baseline\SKILL.md`,
		ReadToolName, // 提示词必须点名用哪个工具去读正文
	} {
		if !strings.Contains(got, want) {
			t.Errorf("渲染结果缺少 %q：\n%s", want, got)
		}
	}
}

func TestRenderBlockNeverCarriesBody(t *testing.T) {
	// 这条是技能机制的全部意义所在：正文不进提示词。一旦有人往
	// RenderBlock 里加正文，机制就退化成"全量塞进去"，白设计。
	body := "正文里的机密步骤：第一步先做这个，第二步再做那个。"
	s := Skill{Name: "a-b", Description: "描述", FilePath: "/x/a-b/SKILL.md"}

	if got := RenderBlock([]Skill{s}); strings.Contains(got, body) {
		t.Errorf("正文不该出现在提示词里：\n%s", got)
	}
}

func TestRenderBlockFlattensMultilineDescription(t *testing.T) {
	// 描述里的换行会把列表项拆散，后面的行看起来像不属于任何技能。
	got := RenderBlock([]Skill{{
		Name:        "a-b",
		Description: "第一行\n第二行",
		FilePath:    "/x/SKILL.md",
	}})

	if !strings.Contains(got, "第一行 第二行") {
		t.Errorf("描述里的换行应当被压成空格：\n%s", got)
	}
	if strings.Contains(got, "\n第二行") {
		t.Errorf("描述不该换行：\n%s", got)
	}
}

func TestRenderBlockListsEverySkillOnce(t *testing.T) {
	skills := []Skill{
		{Name: "a", Description: "描述 a", FilePath: "/a/SKILL.md"},
		{Name: "b", Description: "描述 b", FilePath: "/b/SKILL.md"},
	}
	got := RenderBlock(skills)

	if n := strings.Count(got, "- a："); n != 1 {
		t.Errorf("技能 a 出现了 %d 次", n)
	}
	if n := strings.Count(got, "- b："); n != 1 {
		t.Errorf("技能 b 出现了 %d 次", n)
	}
}
