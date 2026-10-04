package skill

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// EnvSkillsDir 覆盖内置技能目录。
const EnvSkillsDir = "ARKPERF_SKILLS_DIR"

// builtinDirName 是随仓库分发的技能目录名（与 mcp-servers 平级）。
const builtinDirName = "skills"

// Discover 按三层顺序发现技能并合并。
//
// 顺序即优先级：**项目级 > 用户级 > 内置**，先到先得。
//
// 为什么是「越具体越赢」：用户在某个工程里专门写的技能，应当盖掉他自己的
// 通用版本——否则他没法为单个工程做例外。这与 .gitignore 的层叠方向一致。
//
// 同名冲突只记诊断、不报错。技能是外部输入：三个来源同时装了东西是常态，
// 用户可能本来就想要「用工程里的版本盖掉自己那份」，那不是错误。
//
// 目录不存在不算问题（用户可能从没建过工程级技能），因此不产生诊断。
func Discover(cwd, home, builtinDir string) ([]Skill, []Diagnostic) {
	var (
		skills []Skill
		diags  []Diagnostic
	)
	byName := make(map[string]Skill)

	for _, root := range Roots(cwd, home, builtinDir) {
		scanRoot(root, func(s Skill) {
			if prev, dup := byName[s.Name]; dup {
				diags = append(diags, Diagnostic{
					Path: s.FilePath,
					Message: fmt.Sprintf("技能名 %q 与 %s 重复，已忽略这一个（优先级 项目 > 用户 > 内置）",
						s.Name, prev.FilePath),
				})
				return
			}
			byName[s.Name] = s
			skills = append(skills, s)
		}, func(d Diagnostic) {
			diags = append(diags, d)
		})
	}

	// 排序只为**输出稳定**：技能列表会进系统提示词，顺序飘忽会让
	// 每一轮的提示词都不一样，缓存命中与测试快照都跟着乱。
	slices.SortFunc(skills, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(diags, func(a, b Diagnostic) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Message, b.Message)
	})
	return skills, diags
}

// scanRoot 扫描一个技能根。
//
// 遍历规则与 Agent Skills 规范一致：
//   - 根目录本身是**容器**，不是技能（与 mcp-servers/ 的约定相同）
//   - 某个目录里有 SKILL.md，就当它是技能根，**不再往下递归**
//     （技能目录下的 references/ scripts/ assets/ 都是材料，不是技能）
//   - 隐藏目录一律不进：.git / .venv / .idea 下面不可能有技能
func scanRoot(root Root, add func(Skill), diag func(Diagnostic)) {
	info, err := os.Stat(root.Dir)
	if err != nil || !info.IsDir() {
		// 目录不存在是常态。不记诊断：一个从没建过技能的工程
		// 每次启动都被提醒一次，只会让人学会忽略警告。
		return
	}

	seenDir := make(map[string]bool)

	// WalkDir 保证按字典序访问，因此发现顺序是确定的。
	_ = filepath.WalkDir(root.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				diag(Diagnostic{Path: p, Message: "目录读取失败，已跳过：" + err.Error()})
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() || p == root.Dir {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}

		mdPath := filepath.Join(p, FileName)
		data, readErr := os.ReadFile(mdPath)
		if readErr != nil {
			return nil // 本层没有 SKILL.md，继续往下找
		}

		parsed, parseErr := Parse(data, mdPath)
		if parseErr != nil {
			diag(Diagnostic{Path: mdPath, Message: parseErr.Error()})
			return fs.SkipDir
		}
		// 同一份技能可能被符号链接进多个来源（monorepo 共享技能的常见做法）。
		// 按真实目录去重，否则同一个技能会被加载两次、并在提示词里重复出现。
		real := canonicalDir(p)
		if !seenDir[real] {
			seenDir[real] = true
			parsed.Source = root.Source
			add(parsed)
		}
		return fs.SkipDir
	})
}

// canonicalDir 解析符号链接后的真实路径；解析不了就用原路径。
func canonicalDir(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		if abs, err := filepath.Abs(real); err == nil {
			return abs
		}
		return real
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// ResolveBuiltinDir 解析随 ArkPerf 分发的技能目录。
//
// 与 kernel.ResolveMCPDir 是同一套办法与同一套理由：仓库被克隆到哪里
// 因人而异，写死绝对路径会让队友一个内置技能都加载不到，而现象是
// 「Agent 好像不知道该怎么用这些测量工具」——比报错更难查。
//
// 解析顺序（每个候选都要求真实存在，先命中先用）：
//
//	ARKPERF_SKILLS_DIR
//	→ 可执行文件所在目录及其上两级下的 skills（覆盖仓库根 / desktop/bin 两种启动位置）
//	→ 当前工作目录下的 skills
//
// 都找不到时返回当前目录下的 skills 作为最合理的猜测，让下游"扫不到技能"
// 这个安静的结果自己说明问题，而不是在这里 panic——这个函数会被每个
// 前端在装配时调用，崩了就是整个程序起不来。
func ResolveBuiltinDir() string {
	// 显式指定就照用，不做存在性检查：用户说了算，指错了应当表现为
	// 「技能是空的」，而不是悄悄换到别的目录（那更难看懂）。
	if v := os.Getenv(EnvSkillsDir); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
		return v
	}

	var cands []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for range 3 {
			cands = append(cands, filepath.Join(dir, builtinDirName))
			dir = filepath.Dir(dir)
		}
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(wd, builtinDirName))
	}
	for _, c := range cands {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			if abs, err := filepath.Abs(c); err == nil {
				return abs
			}
			return c
		}
	}

	wd, err := os.Getwd()
	if err != nil {
		return builtinDirName
	}
	return filepath.Join(wd, builtinDirName)
}
