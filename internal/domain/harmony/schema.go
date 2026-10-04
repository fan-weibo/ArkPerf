package harmony

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SchemaIssue 是一处配置问题。
type SchemaIssue struct {
	File    string
	Field   string
	Problem string
}

// String 输出 "文件: 字段 — 问题"。
func (i SchemaIssue) String() string { return i.File + "：" + i.Field + " — " + i.Problem }

// moduleTypes 是 module.json5 里合法的 module.type。
//
// 实测 DevEco 新建的工程只会出现这四种；写成自由文本会让"把 entry 拼错成 entr"
// 这种错误一路带到构建阶段，报错还完全不提 module.json5。
var moduleTypes = []string{"entry", "feature", "har", "shared"}

// CheckProjectSchema 在构建之前校验工程配置结构。
//
// 为什么要单独做这一步：hvigor 的配置错误往往要等构建跑几十秒才暴露，
// 而且报错是 "Invalid value" 之类不含文件名的话。提前校验能把"配置写错"
// 和"代码写错"分开，省掉一轮无效构建。
//
// 只读三个文件——这也是 DevEco 真正会读的那三个，多校验不存在的规则只会产生噪音。
func CheckProjectSchema(root string) ([]SchemaIssue, int, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, 0, err
	}
	var issues []SchemaIssue
	checked := 0

	// ---- 根 build-profile.json5 ----
	rootProfile := filepath.Join(root, "build-profile.json5")
	var rp struct {
		App struct {
			Products []struct {
				Name                 string `json:"name"`
				CompatibleSdkVersion string `json:"compatibleSdkVersion"`
				RuntimeOS            string `json:"runtimeOS"`
			} `json:"products"`
			SigningConfigs []any `json:"signingConfigs"`
		} `json:"app"`
		Modules []struct {
			Name    string `json:"name"`
			SrcPath string `json:"srcPath"`
		} `json:"modules"`
	}
	if err := parseJSON5(rootProfile, &rp); err != nil {
		issues = append(issues, SchemaIssue{"build-profile.json5", "(整体)", "无法解析：" + err.Error()})
	} else {
		checked++
		if len(rp.App.Products) == 0 {
			issues = append(issues, SchemaIssue{"build-profile.json5", "app.products", "缺少 products，构建时无法确定目标 SDK 版本"})
		}
		for _, p := range rp.App.Products {
			if strings.TrimSpace(p.CompatibleSdkVersion) == "" {
				issues = append(issues, SchemaIssue{"build-profile.json5", "app.products[" + p.Name + "].compatibleSdkVersion",
					"为空；hvigor 会以 'Invalid value' 失败且不说清是哪个字段"})
			}
		}
		if len(rp.Modules) == 0 {
			issues = append(issues, SchemaIssue{"build-profile.json5", "modules", "为空；工程里没有任何模块可构建"})
		}
		for _, m := range rp.Modules {
			if strings.TrimSpace(m.SrcPath) == "" {
				issues = append(issues, SchemaIssue{"build-profile.json5", "modules[" + m.Name + "].srcPath", "为空"})
			} else if !isDirNoErr(filepath.Join(root, filepath.FromSlash(m.SrcPath))) {
				issues = append(issues, SchemaIssue{"build-profile.json5", "modules[" + m.Name + "].srcPath",
					"指向的目录不存在：" + m.SrcPath})
			}
		}
	}

	// ---- 各模块 ----
	mods := findModules(root)
	for _, mod := range mods {
		modDir := filepath.Join(root, mod)

		var mp struct {
			APIType string `json:"apiType"`
			Targets []struct {
				Name string `json:"name"`
			} `json:"targets"`
		}
		modProfile := filepath.Join(modDir, "build-profile.json5")
		if err := parseJSON5(modProfile, &mp); err != nil {
			issues = append(issues, SchemaIssue{mod + "/build-profile.json5", "(整体)", "无法解析：" + err.Error()})
		} else {
			checked++
			if mp.APIType != "" && mp.APIType != "stageMode" {
				issues = append(issues, SchemaIssue{mod + "/build-profile.json5", "apiType",
					"当前值为 " + mp.APIType + "，FA 模型（faMode）在新 SDK 上已不受支持，应为 stageMode"})
			}
			if len(mp.Targets) == 0 {
				issues = append(issues, SchemaIssue{mod + "/build-profile.json5", "targets", "为空，模块无法产出"})
			}
		}

		var mm struct {
			Module struct {
				Name        string   `json:"name"`
				Type        string   `json:"type"`
				MainElement string   `json:"mainElement"`
				DeviceTypes []string `json:"deviceTypes"`
				Pages       string   `json:"pages"`
				Abilities   []struct {
					Name     string `json:"name"`
					SrcEntry string `json:"srcEntry"`
				} `json:"abilities"`
			} `json:"module"`
		}
		moduleJSON := filepath.Join(modDir, filepath.FromSlash(moduleMarker))
		if err := parseJSON5(moduleJSON, &mm); err != nil {
			issues = append(issues, SchemaIssue{mod + "/src/main/module.json5", "(整体)", "无法解析：" + err.Error()})
			continue
		}
		checked++
		m := mm.Module
		if m.Type != "" && !containsString(moduleTypes, m.Type) {
			issues = append(issues, SchemaIssue{mod + "/src/main/module.json5", "module.type",
				"未知类型 " + m.Type + "，合法值：" + strings.Join(moduleTypes, " / ")})
		}
		if m.Type == "entry" {
			if strings.TrimSpace(m.MainElement) == "" {
				issues = append(issues, SchemaIssue{mod + "/src/main/module.json5", "module.mainElement",
					"entry 模块必须指定入口 ability（通常是 EntryAbility）"})
			}
			if len(m.DeviceTypes) == 0 {
				issues = append(issues, SchemaIssue{mod + "/src/main/module.json5", "module.deviceTypes",
					"为空；构建会失败，且不会提示是这里缺的"})
			}
		}
		if m.Pages != "" && !strings.HasPrefix(m.Pages, "$profile:") {
			issues = append(issues, SchemaIssue{mod + "/src/main/module.json5", "module.pages",
				"应为 $profile:<文件名> 形式，当前为 " + m.Pages})
		}
		for _, a := range m.Abilities {
			if strings.TrimSpace(a.Name) == "" {
				issues = append(issues, SchemaIssue{mod + "/src/main/module.json5", "module.abilities[].name", "存在未命名的 ability"})
			}
			if strings.TrimSpace(a.SrcEntry) == "" {
				issues = append(issues, SchemaIssue{mod + "/src/main/module.json5", "module.abilities[" + a.Name + "].srcEntry",
					"为空，无法定位 ability 源码"})
			}
		}
	}

	// 问题按文件名排序：同一次输出里同一个文件的问题聚在一起，
	// 模型（和人）不用在列表里来回跳。
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].File == issues[j].File {
			return issues[i].Field < issues[j].Field
		}
		return issues[i].File < issues[j].File
	})
	return issues, checked, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// appJSONRel 是全局应用配置的位置。
const appJSONRel = "AppScope/app.json5"

// ReadBundleName 从 AppScope/app.json5 读应用包名。
//
// 包名在很多地方都要用（装机、启动、卸载、冒烟测试），每次都让模型传
// 既容易出错也没必要——工程里本来就写着。
func ReadBundleName(root string) (string, error) {
	var app struct {
		App struct {
			BundleName string `json:"bundleName"`
		} `json:"app"`
	}
	path := filepath.Join(root, filepath.FromSlash(appJSONRel))
	if err := parseJSON5(path, &app); err != nil {
		return "", err
	}
	if strings.TrimSpace(app.App.BundleName) == "" {
		return "", &missingFieldError{file: appJSONRel, field: "app.bundleName"}
	}
	return app.App.BundleName, nil
}

type missingFieldError struct {
	file  string
	field string
}

func (e *missingFieldError) Error() string {
	return e.file + " 里没有 " + e.field
}

// mainPagesRel 是页面清单相对模块源码根的位置。
const mainPagesRel = "src/main/resources/base/profile/main_pages.json"

// readMainPages 读模块的页面列表。
func readMainPages(modDir string) []string {
	var pages struct {
		Src []string `json:"src"`
	}
	if err := parseJSON5(filepath.Join(modDir, filepath.FromSlash(mainPagesRel)), &pages); err != nil {
		return nil // har 模块本来就没有页面清单，缺文件不算问题
	}
	return pages.Src
}

// ensureDirExists 是给调用方做存在性判断的小工具（避免到处 os.Stat）。
func ensureDirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
