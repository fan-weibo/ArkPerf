package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/skill"
)

// writeSkillDir 在 dir/<name>/ 下写一份 SKILL.md。
func writeSkillDir(t *testing.T, dir, name, desc string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n正文\n"
	if err := os.WriteFile(filepath.Join(d, skill.FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// isolatedSkills 把三层技能目录全部指到测试自己的临时目录，
// 避免读到仓库里真实的 skills/ 或用户 ~/.arkperf/skills。
//
// 用户级也要隔离：SkillsFor 走 kernel.Home()，不指开就会读到开发机
// 真实的 ~/.arkperf/skills，测试结果随机器而变。
func isolatedSkills(t *testing.T) (cwd, home, builtin string) {
	t.Helper()
	cwd, home, builtin = t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv(skill.EnvSkillsDir, builtin)
	t.Setenv("ARKPERF_HOME", home)
	return cwd, home, builtin
}

func TestSkillsForMergesProjectAndBuiltin(t *testing.T) {
	cwd, _, builtin := isolatedSkills(t)
	writeSkillDir(t, skill.ProjectDir(cwd), "proj-only", "工程级")
	writeSkillDir(t, builtin, "builtin-only", "内置级")

	skills, diags := skill.Discover(cwd, t.TempDir(), builtin)
	if len(diags) != 0 {
		t.Fatalf("不该有诊断：%+v", diags)
	}
	if len(skills) != 2 {
		t.Fatalf("技能数 = %d，期望 2", len(skills))
	}
}

// 技能跟着工作目录走：项目级技能就在 <工作区>/.arkperf/skills 下，
// 所以装配顺序必须是"先定 cwd、再解析技能"。
func TestOpenLoadsSkillsForWorkspace(t *testing.T) {
	setupHome(t)
	cwd, _, _ := isolatedSkills(t)
	writeSkillDir(t, skill.ProjectDir(cwd), "proj-skill", "工程自带")

	s, err := Open(context.Background(), Options{NoMCP: true, Workspace: cwd})
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	defer s.Close()

	got := s.Skills()
	if len(got) != 1 || got[0].Name != "proj-skill" {
		t.Fatalf("技能 = %+v，期望只有 proj-skill", got)
	}
	if got[0].Source != skill.SourceProject {
		t.Errorf("Source = %q，期望 project", got[0].Source)
	}
	if !strings.Contains(s.SkillSummary(), "技能 1 个") {
		t.Errorf("摘要 = %q", s.SkillSummary())
	}
}

// 切工作区必须换技能集：否则会拿 A 工程的说明书去改 B 工程。
func TestSetWorkspaceRefreshesSkills(t *testing.T) {
	setupHome(t)
	dirA, _, _ := isolatedSkills(t)
	dirB := t.TempDir()
	writeSkillDir(t, skill.ProjectDir(dirA), "from-a", "A 的技能")
	writeSkillDir(t, skill.ProjectDir(dirB), "from-b", "B 的技能")

	s, err := Open(context.Background(), Options{NoMCP: true, Workspace: dirA})
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	defer s.Close()

	if got := s.Skills(); len(got) != 1 || got[0].Name != "from-a" {
		t.Fatalf("切之前技能 = %+v", got)
	}

	if err := s.SetWorkspace(dirB); err != nil {
		t.Fatalf("SetWorkspace 失败：%v", err)
	}

	got := s.Skills()
	if len(got) != 1 || got[0].Name != "from-b" {
		t.Fatalf("切之后技能 = %+v，期望只剩 from-b", got)
	}
}

// 坏技能只跳过、不阻断，但必须留下痕迹——否则用户唯一的观察窗口
// 是模型的回答，而"技能没生效"在回答里看不出来。
func TestSkillsForKeepsDiagnostics(t *testing.T) {
	cwd, _, builtin := isolatedSkills(t)
	writeSkillDir(t, builtin, "good", "正常")
	bad := filepath.Join(builtin, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, skill.FileName), []byte("没有 frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills, diags := SkillsFor(cwd)
	if len(skills) != 1 {
		t.Fatalf("好技能应当照常加载，实际 %d 个", len(skills))
	}
	if len(diags) != 1 {
		t.Fatalf("坏技能应当留一条诊断，实际 %+v", diags)
	}

	report := SkillReport(cwd)
	for _, want := range []string{"技能目录", "good", "已跳过", "frontmatter", builtin} {
		if !strings.Contains(report, want) {
			t.Errorf("报告缺少 %q：\n%s", want, report)
		}
	}
}

// 同名技能必须能被看见谁盖了谁：用户改的那份没生效时，
// 唯一的线索就是报告里的路径。
func TestSkillReportShowsWinnerPath(t *testing.T) {
	cwd, _, builtin := isolatedSkills(t)
	writeSkillDir(t, skill.ProjectDir(cwd), "dup", "工程版")
	writeSkillDir(t, builtin, "dup", "内置版")

	skills, _ := SkillsFor(cwd)
	if len(skills) != 1 {
		t.Fatalf("同名应当只留一个，实际 %d", len(skills))
	}
	if skills[0].Source != skill.SourceProject {
		t.Errorf("应当是工程版胜出，实际 %q", skills[0].Source)
	}

	report := SkillReport(cwd)
	if !strings.Contains(report, skill.ProjectDir(cwd)) {
		t.Errorf("报告应当打出胜出者路径：\n%s", report)
	}
	if !strings.Contains(report, "已跳过") {
		t.Errorf("报告应当说明被盖掉的那份：\n%s", report)
	}
}

func TestSkillSummaryCountsBySource(t *testing.T) {
	got := SkillSummary([]skill.Skill{
		{Name: "a", Source: skill.SourceProject},
		{Name: "b", Source: skill.SourceBuiltin},
		{Name: "c", Source: skill.SourceBuiltin},
	})
	if !strings.Contains(got, "技能 3 个") || !strings.Contains(got, "项目 1") || !strings.Contains(got, "内置 2") {
		t.Errorf("摘要 = %q", got)
	}
	if got := SkillSummary(nil); got != "无技能" {
		t.Errorf("空集摘要 = %q", got)
	}
}
