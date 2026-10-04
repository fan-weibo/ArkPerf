package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill 在 root/<name>/ 下写一份 SKILL.md，返回其路径。
func writeSkill(t *testing.T, root, name, desc string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, FileName)
	if err := os.WriteFile(p, []byte("---\nname: "+name+"\ndescription: "+desc+"\n---\n\n正文\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeRaw 在 path 写任意内容（用于造坏技能）。
func writeRaw(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(skills []Skill) []string {
	out := make([]string, 0, len(skills))
	for _, s := range skills {
		out = append(out, s.Name)
	}
	return out
}

func find(t *testing.T, skills []Skill, name string) Skill {
	t.Helper()
	for _, s := range skills {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("没有找到技能 %q，实际有：%v", name, names(skills))
	return Skill{}
}

func TestDiscoverMergesThreeTiersSorted(t *testing.T) {
	cwd, home, builtin := t.TempDir(), t.TempDir(), t.TempDir()
	writeSkill(t, ProjectDir(cwd), "zebra", "工程级")
	writeSkill(t, UserDir(home), "apple", "用户级")
	writeSkill(t, builtin, "mango", "内置级")

	skills, diags := Discover(cwd, home, builtin)
	if len(diags) != 0 {
		t.Fatalf("不该有诊断：%+v", diags)
	}
	// 排序必须是确定的，否则每轮提示词的技能顺序都会飘。
	got := strings.Join(names(skills), ",")
	if got != "apple,mango,zebra" {
		t.Errorf("顺序 = %s，期望 apple,mango,zebra", got)
	}
	if s := find(t, skills, "zebra"); s.Source != SourceProject {
		t.Errorf("zebra 的 Source = %q", s.Source)
	}
	if s := find(t, skills, "apple"); s.Source != SourceUser {
		t.Errorf("apple 的 Source = %q", s.Source)
	}
	if s := find(t, skills, "mango"); s.Source != SourceBuiltin {
		t.Errorf("mango 的 Source = %q", s.Source)
	}
}

func TestDiscoverPriorityProjectBeatsUserBeatsBuiltin(t *testing.T) {
	cwd, home, builtin := t.TempDir(), t.TempDir(), t.TempDir()
	pProject := writeSkill(t, ProjectDir(cwd), "shared", "工程版")
	writeSkill(t, UserDir(home), "shared", "用户版")
	writeSkill(t, builtin, "shared", "内置版")

	skills, diags := Discover(cwd, home, builtin)

	if len(skills) != 1 {
		t.Fatalf("同名应当只留一个，得到 %d 个：%v", len(skills), names(skills))
	}
	// 越具体越赢：用户在某个工程里专门写的，应当盖掉通用版本。
	if got := find(t, skills, "shared").FilePath; got != pProject {
		t.Errorf("胜出的是 %s，期望工程版 %s", got, pProject)
	}
	// 被盖掉的两份都要留痕，否则用户只能靠猜"为什么我改的那份没生效"。
	if len(diags) != 2 {
		t.Fatalf("应当有 2 条冲突诊断，得到 %d：%+v", len(diags), diags)
	}
	for _, d := range diags {
		if !strings.Contains(d.Message, "重复") {
			t.Errorf("诊断信息应当说明是重名：%q", d.Message)
		}
	}
}

func TestDiscoverCollisionDiagnosticPointsAtWinner(t *testing.T) {
	cwd, home, builtin := t.TempDir(), t.TempDir(), t.TempDir()
	winner := writeSkill(t, ProjectDir(cwd), "dup", "工程版")
	loser := writeSkill(t, UserDir(home), "dup", "用户版")

	_, diags := Discover(cwd, home, builtin)
	if len(diags) != 1 {
		t.Fatalf("诊断数 = %d", len(diags))
	}
	if diags[0].Path != loser {
		t.Errorf("诊断应当指向被忽略的那一份 %s，实际 %s", loser, diags[0].Path)
	}
	if !strings.Contains(diags[0].Message, winner) {
		t.Errorf("诊断应当指明胜出者路径，实际 %q", diags[0].Message)
	}
}

func TestDiscoverDoesNotRecurseIntoSkillDir(t *testing.T) {
	builtin := t.TempDir()
	writeSkill(t, builtin, "outer", "外层技能")
	// 技能目录下的 references/ 是材料，不是技能。
	writeRaw(t, filepath.Join(builtin, "outer", "references", FileName),
		"---\nname: should-not-load\ndescription: 这只是参考材料\n---\n")

	skills, _ := Discover("", "", builtin)
	if got := names(skills); len(got) != 1 || got[0] != "outer" {
		t.Errorf("应当只加载 outer，实际 %v", got)
	}
}

func TestDiscoverSkipsHiddenDirs(t *testing.T) {
	builtin := t.TempDir()
	writeSkill(t, builtin, "visible", "可见")
	writeSkill(t, filepath.Join(builtin, ".git"), "hidden", "藏在 .git 里")

	skills, _ := Discover("", "", builtin)
	if got := names(skills); len(got) != 1 || got[0] != "visible" {
		t.Errorf("应当跳过隐藏目录，实际 %v", got)
	}
}

func TestDiscoverSkipsBrokenSkillButKeepsOthers(t *testing.T) {
	builtin := t.TempDir()
	writeSkill(t, builtin, "good", "正常的")
	writeRaw(t, filepath.Join(builtin, "bad", FileName), "没有 frontmatter 的文件\n")
	writeRaw(t, filepath.Join(builtin, "nodesc", FileName), "---\nname: nodesc\n---\n")

	skills, diags := Discover("", "", builtin)

	// 一个写坏的技能不该让整个程序起不来，也不该影响别的技能。
	if got := names(skills); len(got) != 1 || got[0] != "good" {
		t.Fatalf("应当只加载 good，实际 %v", got)
	}
	if len(diags) != 2 {
		t.Fatalf("两个坏技能应当各留一条诊断，得到 %d：%+v", len(diags), diags)
	}
	joined := diags[0].Message + diags[1].Message
	if !strings.Contains(joined, "frontmatter") || !strings.Contains(joined, "description") {
		t.Errorf("诊断应当分别说明原因，实际 %q", joined)
	}
}

func TestDiscoverTreatsRootAsContainer(t *testing.T) {
	builtin := t.TempDir()
	// 根目录自己带 SKILL.md 是误放（约定：根是容器，与 mcp-servers/ 一致）。
	writeRaw(t, filepath.Join(builtin, FileName), "---\nname: at-root\ndescription: 放错位置\n---\n")
	writeSkill(t, builtin, "proper", "子目录里的技能")

	skills, _ := Discover("", "", builtin)
	if got := names(skills); len(got) != 1 || got[0] != "proper" {
		t.Errorf("根目录的 SKILL.md 不该被当成技能，实际 %v", got)
	}
}

func TestDiscoverMissingDirsProduceNoDiagnostics(t *testing.T) {
	// 用户可能从没建过工程级技能目录，那不是问题，不该每次启动都提醒。
	skills, diags := Discover(filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "nope"), "")
	if len(skills) != 0 || len(diags) != 0 {
		t.Errorf("目录不存在应当静默：skills=%v diags=%+v", names(skills), diags)
	}
}

func TestDiscoverIgnoresEmptyInputs(t *testing.T) {
	skills, diags := Discover("", "", "")
	if len(skills) != 0 || len(diags) != 0 {
		t.Errorf("三个来源都为空时应当没有结果：%v %+v", names(skills), diags)
	}
}

func TestDiscoverIsStableAcrossCalls(t *testing.T) {
	builtin := t.TempDir()
	for _, n := range []string{"c", "a", "b"} {
		writeSkill(t, builtin, n, "描述 "+n)
	}
	first, _ := Discover("", "", builtin)
	second, _ := Discover("", "", builtin)
	if strings.Join(names(first), ",") != strings.Join(names(second), ",") {
		t.Errorf("两次调用顺序不一致：%v vs %v", names(first), names(second))
	}
}

func TestRootsOrderIsProjectUserBuiltin(t *testing.T) {
	roots := Roots("/w", "/h", "/b")
	if len(roots) != 3 {
		t.Fatalf("roots 数 = %d", len(roots))
	}
	want := []Source{SourceProject, SourceUser, SourceBuiltin}
	for i, r := range roots {
		if r.Source != want[i] {
			t.Errorf("roots[%d].Source = %q，期望 %q", i, r.Source, want[i])
		}
	}
}

func TestResolveBuiltinDirHonorsEnv(t *testing.T) {
	custom := t.TempDir()
	t.Setenv(EnvSkillsDir, custom)

	got := ResolveBuiltinDir()
	want, err := filepath.Abs(custom)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("ResolveBuiltinDir() = %q，期望 %q", got, want)
	}
}
