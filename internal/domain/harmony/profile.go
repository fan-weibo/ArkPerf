package harmony

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ModuleProfile 是一个模块的画像。
type ModuleProfile struct {
	Name        string
	Type        string
	DeviceTypes []string
	Abilities   []string
	Pages       []string
	SrcEntry    string
}

// ProjectProfile 是一次调用拿到的工程全貌。
//
// 为什么要做成"一次调用"：模型要理解一个陌生工程，通常需要
// 包名 + 模块 + 入口 ability + 页面 + 依赖 这几项；让它们分五次工具调用去拼，
// 每次都要重新定位工程根，既慢又容易拼错。
type ProjectProfile struct {
	Root       string
	BundleName string
	// SDKVersion 是 compatibleSdkVersion。
	SDKVersion string
	Modules    []ModuleProfile
	// Dependencies 是 entry 模块声明的依赖（har 与三方库）。
	Dependencies []string
	// ResourceCounts 按扩展名统计的资源文件数。
	ResourceCounts map[string]int
	// SourceFiles 是源码文件（.ets/.ts）总数。
	SourceFiles int
	// ConfigIssues 顺带带上配置校验发现的问题——反正配置都读过了，
	// 让调用方一次就能看到"结构"和"结构里的毛病"。
	ConfigIssues []SchemaIssue
}

// scanSkipDirs 是统计与遍历时要跳过的目录。
//
// oh_modules / node_modules 里的依赖代码动辄几万文件，统计它们只会污染
// "这个工程有多大"这个本来要给模型的信号。
var scanSkipDirs = map[string]bool{
	"node_modules": true,
	"oh_modules":   true,
	".hvigor":      true,
	".preview":     true,
	"build":        true,
	".git":         true,
	".idea":        true,
}

// Profile 生成一个工程的画像。
func Profile(root string) (ProjectProfile, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return ProjectProfile{}, err
	}
	if !isProjectRoot(root) {
		return ProjectProfile{}, &notProjectError{dir: root}
	}

	p := ProjectProfile{Root: root, ResourceCounts: map[string]int{}}

	if bundle, err := ReadBundleName(root); err == nil {
		p.BundleName = bundle
	}

	// 根配置：SDK 版本
	var rp struct {
		App struct {
			Products []struct {
				CompatibleSdkVersion string `json:"compatibleSdkVersion"`
			} `json:"products"`
		} `json:"app"`
	}
	if err := parseJSON5(filepath.Join(root, "build-profile.json5"), &rp); err == nil {
		if len(rp.App.Products) > 0 {
			p.SDKVersion = rp.App.Products[0].CompatibleSdkVersion
		}
	}

	// 模块
	for _, mod := range findModules(root) {
		modDir := filepath.Join(root, mod)
		var mm struct {
			Module struct {
				Name        string   `json:"name"`
				Type        string   `json:"type"`
				DeviceTypes []string `json:"deviceTypes"`
				MainElement string   `json:"mainElement"`
				Abilities   []struct {
					Name     string `json:"name"`
					SrcEntry string `json:"srcEntry"`
				} `json:"abilities"`
			} `json:"module"`
		}
		mp := ModuleProfile{Name: mod}
		if err := parseJSON5(filepath.Join(modDir, filepath.FromSlash(moduleMarker)), &mm); err == nil {
			mp.Type = mm.Module.Type
			mp.DeviceTypes = mm.Module.DeviceTypes
			mp.SrcEntry = mm.Module.MainElement
			for _, a := range mm.Module.Abilities {
				mp.Abilities = append(mp.Abilities, a.Name)
			}
		}
		mp.Pages = readMainPages(modDir)
		p.Modules = append(p.Modules, mp)
	}
	sort.SliceStable(p.Modules, func(i, j int) bool { return p.Modules[i].Name < p.Modules[j].Name })

	// entry 的依赖
	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := parseJSON5(filepath.Join(root, "entry", "oh-package.json5"), &pkg); err == nil {
		for name, ver := range pkg.Dependencies {
			p.Dependencies = append(p.Dependencies, name+"@"+ver)
		}
		sort.Strings(p.Dependencies)
	}

	// 文件统计
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if scanSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(d.Name())) {
		case ".ets", ".ts":
			p.SourceFiles++
		case ".json", ".json5", ".png", ".jpg", ".jpeg", ".svg", ".webp", ".mp4":
			p.ResourceCounts[strings.ToLower(strings.TrimPrefix(filepath.Ext(d.Name()), "."))]++
		}
		return nil
	})

	if issues, _, err := CheckProjectSchema(root); err == nil {
		p.ConfigIssues = issues
	}
	return p, nil
}

type notProjectError struct{ dir string }

func (e *notProjectError) Error() string {
	return e.dir + " 不是 OpenHarmony 工程根（缺 " + strings.Join(projectMarkers, " 或 ") + "）"
}

// Format 把画像渲染成给模型看的文本。
//
// 刻意不返回 JSON：这份东西是给模型读的上下文，分行罗列比嵌套结构
// 更省 token 也更不容易看漏。
func (p ProjectProfile) Format() string {
	var sb strings.Builder
	sb.WriteString("工程：" + p.Root + "\n")
	if p.BundleName != "" {
		sb.WriteString("包名：" + p.BundleName + "\n")
	}
	if p.SDKVersion != "" {
		sb.WriteString("目标 SDK：" + p.SDKVersion + "\n")
	}
	sb.WriteString("源码文件：" + itoa(p.SourceFiles) + " 个（.ets/.ts）\n")
	if len(p.ResourceCounts) > 0 {
		keys := make([]string, 0, len(p.ResourceCounts))
		for k := range p.ResourceCounts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+itoa(p.ResourceCounts[k]))
		}
		sb.WriteString("资源：" + strings.Join(parts, "  ") + "\n")
	}

	sb.WriteString("\n模块（" + itoa(len(p.Modules)) + "）：\n")
	for _, m := range p.Modules {
		sb.WriteString("- " + m.Name)
		if m.Type != "" {
			sb.WriteString(" [" + m.Type + "]")
		}
		if len(m.DeviceTypes) > 0 {
			sb.WriteString(" 设备类型：" + strings.Join(m.DeviceTypes, "/"))
		}
		sb.WriteString("\n")
		if len(m.Abilities) > 0 {
			sb.WriteString("    ability：" + strings.Join(m.Abilities, "、"))
			if m.SrcEntry != "" {
				sb.WriteString("（入口 " + m.SrcEntry + "）")
			}
			sb.WriteString("\n")
		}
		if len(m.Pages) > 0 {
			sb.WriteString("    页面：" + strings.Join(m.Pages, "、") + "\n")
		}
	}
	if len(p.Dependencies) > 0 {
		sb.WriteString("\n依赖（entry）：" + strings.Join(p.Dependencies, "、") + "\n")
	}
	if len(p.ConfigIssues) > 0 {
		sb.WriteString("\n配置问题（" + itoa(len(p.ConfigIssues)) + "）：\n")
		for _, i := range p.ConfigIssues {
			sb.WriteString("- " + i.String() + "\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func itoa(n int) string { return strconv.Itoa(n) }
