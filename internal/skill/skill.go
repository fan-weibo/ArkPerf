// Package skill 是 ArkPerf 的技能层：发现「按需加载的说明书」，解析出元信息，
// 再交给系统提示词。
//
// 技能不是工具。工具给模型一双手（能做什么），技能给它一份手册（怎么把
// 那双手用对）。两者的成本结构因此完全不同：工具的参数 schema 每一轮请求
// 都要全量发给模型，技能只有名字与描述进提示词，正文等模型判定相关后
// 自己用 read_file 去读。
//
// 一个技能就是一个含 SKILL.md 的目录：
//
//	skills/cold-start-baseline/
//	├── SKILL.md
//	├── references/protocol.md
//	└── scripts/compare.py
//
// SKILL.md 的开头是 frontmatter，其后是正文：
//
//	---
//	name: cold-start-baseline
//	description: 建立冷启动基线并与改动后对比。当用户问启动耗时是否改善、需要优化前后对比时使用。
//	---
//
//	正文……
//
// 只支持**单行** frontmatter 值：需要多行时把内容放进正文，别指望 YAML 折叠。
//
// 本包只依赖标准库：它是 kernel 的叶子依赖（与 internal/execx 同一位置），
// 不反向依赖任何内部包。
package skill

import "path/filepath"

// Source 说明一个技能是从哪一层发现的。
//
// 三层的划分不是为了分类，而是为了作用域与生命周期：内置层随版本走、
// 项目层随仓库走、用户层随人走。它们的优先级不同（见 Discover）。
type Source string

const (
	// SourceProject 是工程自带技能：<工作目录>/.arkperf/skills。
	SourceProject Source = "project"
	// SourceUser 是跨工程的个人技能：<状态根>/skills（默认 ~/.arkperf/skills）。
	SourceUser Source = "user"
	// SourceBuiltin 是随 ArkPerf 分发的技能：仓库根的 skills/。
	SourceBuiltin Source = "builtin"
)

// Skill 是一个已解析的技能。
type Skill struct {
	Name        string
	Description string
	// FilePath 是 SKILL.md 的绝对路径。
	//
	// 必须是绝对路径：模型是靠 read_file 去读正文的，而技能可能来自
	// 用户目录或仓库随附目录，给相对路径它无从解析。
	FilePath string
	Source   Source
}

// Diagnostic 是发现过程中记录下来的问题。
//
// 技能是**外部输入**：目录可能是用户随手丢的，也可能整个是别人的仓库。
// 所以这里的策略与 kernel.Registry 恰好相反——那边重名直接 panic
// （工具全是编译进来的，重名是编码错误，越早炸越好），这边只记录并跳过。
// 一个写坏的 SKILL.md 不该让 ArkPerf 起不来。
type Diagnostic struct {
	// Path 出问题的那一项（目录或 SKILL.md）。
	Path string
	// Message 是一句人话，含该怎么改。
	Message string
}

// Roots 按优先级返回三层技能目录（项目 > 用户 > 内置）。
//
// 供 /skills 之类的展示使用：让用户看得到「这个技能是从哪加载的」，
// 否则同名技能谁盖了谁只能靠猜。
func Roots(cwd, home, builtinDir string) []Root {
	var out []Root
	add := func(dir string, src Source) {
		if dir == "" {
			return
		}
		out = append(out, Root{Dir: dir, Source: src})
	}
	// 顺序即优先级，Don't reorder：Discover 依赖「先到先得」。
	add(ProjectDir(cwd), SourceProject)
	add(UserDir(home), SourceUser)
	add(builtinDir, SourceBuiltin)
	return out
}

// Root 是一个技能扫描根。
type Root struct {
	Dir    string
	Source Source
}

// ProjectDir 返回工程级技能目录。
func ProjectDir(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Join(cwd, ".arkperf", "skills")
}

// UserDir 返回用户级技能目录。
func UserDir(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, "skills")
}
