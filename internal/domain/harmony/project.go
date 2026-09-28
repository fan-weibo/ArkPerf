package harmony

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/fan-weibo/ArkPerf/internal/execx"
)

// timeoutBuild 是构建超时。
//
// 给到 20 分钟不是保守：首次构建要解析依赖、编译 ArkTS、跑打包链路，
// 实测分钟级很常见；把超时设小只会得到"构建失败"的假象，而实际上
// 编译器还在正常工作。
const timeoutBuild = 20 * time.Minute

// 工程根的判据。
//
// 不能用"存在 build-profile.json5"单独判定：实测本机每个真实工程的
// **根目录和每个模块目录都有** build-profile.json5，从模块内部向上找时
// 会在模块目录就停下，把 entry/ 当成工程根。
//
// 两条可靠判据：
//  1. hvigor/hvigor-config.json5 —— 只有根目录有
//  2. build-profile.json5 且自身不是模块（不含 src/main/module.json5）
var projectMarkers = []string{"hvigor/hvigor-config.json5", "build-profile.json5"}

// 模块的判据文件（相对模块目录）。
const moduleMarker = "src/main/module.json5"

// hvigorConfigRel 是根目录独有标记。
const hvigorConfigRel = "hvigor/hvigor-config.json5"

// isProjectRoot 判断一个目录是不是工程根。
func isProjectRoot(dir string) bool {
	if existsNoErr(filepath.Join(dir, filepath.FromSlash(hvigorConfigRel))) {
		return true
	}
	isModule := existsNoErr(filepath.Join(dir, filepath.FromSlash(moduleMarker)))
	return existsNoErr(filepath.Join(dir, "build-profile.json5")) && !isModule
}

// Project 是一个 OpenHarmony 工程。
type Project struct {
	Root string
	// Hvigorw 是实际使用的构建入口；为空表示没有可用的构建器。
	Hvigorw string
	// HvigorwSource 说明构建入口来自哪里，用于如实展示。
	HvigorwSource string
	// Modules 是模块目录名（含 src/main/module.json5 的直接子目录）。
	Modules []string
	// SDKDir 是 SDK 根目录，用于在命令行环境里补 DEVECO_SDK_HOME。
	SDKDir string
}

// FindProject 从 start 开始向上寻找工程根。
//
// 判据用 build-profile.json5 / hvigor/hvigor-config.json5，而不是"存在 hvigorw 脚本"：
// 实测本机三个 OpenHarmony 工程根目录都没有 hvigorw（它们靠 DevEco 的全局
// hvigorw 构建），用脚本来判会一个都找不到。
func FindProject(start string) (*Project, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}

	for range 12 {
		if isProjectRoot(dir) {
			return newProject(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil, fmt.Errorf("在 %s 及其上级目录中没有找到 OpenHarmony 工程（判据：%s）",
		start, strings.Join(projectMarkers, " / "))
}

func newProject(root string) (*Project, error) {
	if !isProjectRoot(root) {
		return nil, fmt.Errorf("%s 看起来不是 OpenHarmony 工程：缺少 %s", root, strings.Join(projectMarkers, " 或 "))
	}

	p := &Project{Root: root, Modules: findModules(root)}

	// 工程自带的构建脚本优先；没有就用 DevEco 的全局 hvigorw
	for _, name := range []string{"hvigorw.bat", "hvigorw", "hvigorw.cmd"} {
		cand := filepath.Join(root, name)
		if existsNoErr(cand) {
			p.Hvigorw, p.HvigorwSource = cand, "工程自带"
			break
		}
	}
	return p, nil
}

// findModules 找出工程内的模块目录。
func findModules(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var mods []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if existsNoErr(filepath.Join(root, e.Name(), filepath.FromSlash(moduleMarker))) {
			mods = append(mods, e.Name())
		}
	}
	slices.Sort(mods)
	return mods
}

// UseToolchain 在没有工程自带构建脚本时，回退到工具链里的 hvigorw，
// 并把工具链探测到的 SDK 位置带上。
//
// 返回 false 表示两边都没有——这时不要假装能构建。
func (p *Project) UseToolchain(tc *Toolchain) bool {
	if tc == nil {
		return p.Hvigorw != ""
	}
	if p.SDKDir == "" {
		p.SDKDir = tc.SDKDir
	}
	if p.Hvigorw != "" {
		return true
	}
	hvigorw := tc.Required(ToolHvigorw)
	if !hvigorw.Found() {
		return false
	}
	p.Hvigorw, p.HvigorwSource = hvigorw.Path, "DevEco 全局"
	return true
}

// DefaultBuildArgs 是构建的默认参数。
//
// 刻意保持最小：只有任务名与 --no-daemon。加 --mode module 之类需要配套的
// -p module=xx@yy，缺了就报错；先跑最朴素的形态，需要精细控制时由调用方
// 用 extra 追加参数。
//
// --no-daemon 是必须的：hvigor 的常驻守护进程会持有工程锁，对"跑完就退出"
// 的 CLI 来说，留着它之后只会得到"文件被占用""增量状态不一致"这类怪问题。
func DefaultBuildArgs(task string) []string {
	task = strings.TrimSpace(task)
	if task == "" {
		task = "assembleHap"
	}
	return []string{task, "--no-daemon"}
}

// Build 在工程根目录执行构建。
func (p *Project) Build(ctx context.Context, task string, extra []string) (execx.Result, error) {
	if p.Hvigorw == "" {
		return execx.Result{}, errors.New("没有可用的 hvigorw：工程内无构建脚本，DevEco 的 tools/hvigor/bin 也不存在")
	}
	args := append(DefaultBuildArgs(task), extra...)

	env := map[string]string{}
	// 补 DEVECO_SDK_HOME：实测命令行构建不设它必然失败，报错是
	// "Invalid value of 'DEVECO_SDK_HOME' in the system environment path" ——
	// 既没说是空的还是错的，也不提示该设成什么，极难自查。
	// 已设且路径有效时不动它（用户可能把 SDK 装在别处）。
	if p.SDKDir != "" {
		if v := strings.TrimSpace(os.Getenv(EnvDevEcoSDKHome)); v == "" || !existsNoErr(v) {
			env[EnvDevEcoSDKHome] = p.SDKDir
		}
	}

	return execx.Run(ctx, p.Hvigorw, args, execx.Options{
		Dir:     p.Root, // hvigorw 用 cwd 定位工程，必须切到根目录
		Env:     env,
		Timeout: timeoutBuild,
	})
}

// FindHap 在构建产物目录中找出最新的 hap 文件。
//
// 构建成功不等于知道产物在哪：hvigor 的输出路径随产品名与模块名变化，
// 与其猜路径，不如按修改时间挑最新的——这正是"刚构建出来的那个"。
func (p *Project) FindHap() (string, error) {
	var found []string
	for _, mod := range p.Modules {
		base := filepath.Join(p.Root, mod, "build")
		if !existsNoErr(base) {
			continue
		}
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // 单个目录读不了不该中断整个查找
			}
			if !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".hap") {
				found = append(found, path)
			}
			return nil
		})
	}
	if len(found) == 0 {
		return "", fmt.Errorf("在 %s 下没有找到 .hap 产物；构建可能没成功，或产物在别处", p.Root)
	}

	slices.SortFunc(found, func(a, b string) int {
		ai, aerr := os.Stat(a)
		bi, berr := os.Stat(b)
		switch {
		case aerr != nil:
			return 1
		case berr != nil:
			return -1
		default:
			return bi.ModTime().Compare(ai.ModTime()) // 新的在前
		}
	})
	return found[0], nil
}

func existsNoErr(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isDirNoErr(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
