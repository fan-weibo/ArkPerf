package kernel

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/skill"
)

// testRunner 造一个不会碰网络、不打印任何东西的 Runner。
// 技能是否送达模型只能从"模型收到了什么"看，所以必须走一次真实循环。
func testRunner(chat ChatFunc, skills []skill.Skill) *Runner {
	return &Runner{
		Cfg: &Config{
			Provider: ProviderConfig{BaseURL: "http://localhost", APIKey: "k", Model: "m"},
			MaxTurns: 1,
			Approval: "auto",
		},
		Registry: NewRegistry(),
		Chat:     chat,
		Events:   &LoopEvents{}, // 非空即覆盖默认文本渲染，测试不会往 stdout 写
		Skills:   skills,
	}
}

func systemMessage(t *testing.T, req ChatRequest) string {
	t.Helper()
	for _, m := range req.Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	t.Fatal("请求里没有 system 消息")
	return ""
}

// 交互模式走流式（字边产边出），静默模式**不**流式（等说完一次打出来）。
//
// 这两条不能混：静默模式的契约是"只输出最终回答"，一旦也流式，
// 中间过程会跟着进 stdout，脚本拿到的结果就被污染了。
func TestRunnerStreamsOnlyWhenNotSilent(t *testing.T) {
	cases := []struct {
		name       string
		silent     bool
		wantStream bool
	}{
		{"交互模式走流式", false, true},
		{"静默模式不流式", true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var streamed bool
			var out bytes.Buffer

			ch := &scriptedChat{t: t, steps: []ChatResponse{assistantText("回答")}}
			r := testRunner(func(ctx context.Context, req ChatRequest) (ChatResponse, error) {
				streamed = req.OnDelta != nil
				if req.OnDelta != nil {
					// 冒充 provider：流式时按增量回调，最后仍返回完整消息
					req.OnDelta(DeltaContent, "回答")
					req.OnDelta(DeltaReasoning, "想了想")
				}
				return ch.chat(ctx, req)
			}, nil)
			r.Out = &out
			r.SilenceProgress = tc.silent
			r.Events = nil // 用默认渲染，正是被测对象

			if _, err := r.Run(t.Context(), Task{Text: "任务"}); err != nil {
				t.Fatalf("Run 失败：%v", err)
			}
			if streamed != tc.wantStream {
				t.Fatalf("是否流式 = %v，期望 %v", streamed, tc.wantStream)
			}

			// 两条路径下"回答"都只能出现一次——流式最典型的错就是打两遍
			if n := strings.Count(out.String(), "回答"); n != 1 {
				t.Fatalf("回答出现 %d 次，应当只有 1 次：%q", n, out.String())
			}
			// 思考过程不该进 stdout：这条路径的输出常被脚本接走
			if strings.Contains(out.String(), "想了想") {
				t.Fatalf("思考过程不该混进输出：%q", out.String())
			}
		})
	}
}

func TestRunnerDeliversDefaultSkillsToModel(t *testing.T) {
	ch := &scriptedChat{
		t:     t,
		steps: []ChatResponse{assistantText("done")},
	}
	skills := []skill.Skill{{
		Name:        "memory-leak-triage",
		Description: "判定是否存在内存泄漏。",
		FilePath:    `E:\s\memory-leak-triage\SKILL.md`,
		Source:      skill.SourceBuiltin,
	}}

	r := testRunner(ch.chat, skills)
	if _, err := r.Run(t.Context(), Task{Text: "看看内存"}); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	if len(ch.requests) != 1 {
		t.Fatalf("模型被调用了 %d 次", len(ch.requests))
	}
	sys := systemMessage(t, ch.requests[0])
	if !strings.Contains(sys, "memory-leak-triage") {
		t.Errorf("Runner.Skills 没有进提示词：\n%s", sys)
	}
}

// 同一个 Runner 会被前端复用；用户切了工作区之后，下一次任务必须换成
// 新目录下的技能集。这条覆盖顺序与 CWD 一致，靠的是 Task.Skills。
func TestTaskSkillsOverrideRunnerDefault(t *testing.T) {
	ch := &scriptedChat{
		t:     t,
		steps: []ChatResponse{assistantText("done")},
	}
	fallback := []skill.Skill{{
		Name: "should-not-appear", Description: "默认技能",
		FilePath: "/x/should-not-appear/SKILL.md",
	}}
	override := []skill.Skill{{
		Name: "from-new-workspace", Description: "新工作区的技能",
		FilePath: "/y/from-new-workspace/SKILL.md",
	}}

	r := testRunner(ch.chat, fallback)
	if _, err := r.Run(t.Context(), Task{Text: "任务", Skills: override}); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	sys := systemMessage(t, ch.requests[0])
	if !strings.Contains(sys, "from-new-workspace") {
		t.Errorf("Task.Skills 没有覆盖 Runner.Skills：\n%s", sys)
	}
	if strings.Contains(sys, "should-not-appear") {
		t.Errorf("默认技能不该同时出现：\n%s", sys)
	}
}

// 传非 nil 的空切片表示"这次明确不用技能"，必须能与"没指定"区分开——
// 否则前端就没法实现"这个任务不要技能"。
func TestTaskSkillsEmptySliceDisablesSkills(t *testing.T) {
	ch := &scriptedChat{
		t:     t,
		steps: []ChatResponse{assistantText("done")},
	}
	fallback := []skill.Skill{{
		Name: "should-not-appear", Description: "默认技能",
		FilePath: "/x/should-not-appear/SKILL.md",
	}}

	r := testRunner(ch.chat, fallback)
	if _, err := r.Run(t.Context(), Task{Text: "任务", Skills: []skill.Skill{}}); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	sys := systemMessage(t, ch.requests[0])
	if strings.Contains(sys, "should-not-appear") {
		t.Errorf("空切片应当关掉技能：\n%s", sys)
	}
	if strings.Contains(sys, "可用技能") {
		t.Errorf("没有技能时不该出现技能小节：\n%s", sys)
	}
}

func TestRunnerWithoutSkillsKeepsPromptClean(t *testing.T) {
	ch := &scriptedChat{
		t:     t,
		steps: []ChatResponse{assistantText("done")},
	}
	r := testRunner(ch.chat, nil)
	if _, err := r.Run(t.Context(), Task{Text: "任务"}); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if sys := systemMessage(t, ch.requests[0]); strings.Contains(sys, "可用技能") {
		t.Errorf("没有技能时提示词不该有技能小节：\n%s", sys)
	}
}
