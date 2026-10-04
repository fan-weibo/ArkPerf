package tools

import (
	"path/filepath"
	"strings"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// 审批类别（"以后别再问"的粒度）的计算。
//
// 只有这几个工具实现 kernel.Approvable，因此也只有它们支持"总是允许"。
// 粒度必须保守：**类别太宽等于把审批关掉**，而审批是 ArkPerf 唯一的入口
// 分类手段。所以路径只归到目录、命令只归到"程序 + 子命令"——
// 绝不会出现"允许 edit_file 这个工具"这种一整片放行。
//
// 静态审批的工具（harmony_build / install / launch / sign 等）刻意**不实现**它：
// 它们每次的差别在工程状态而不在参数，划不出一个安全的类别。
// 想给某个工具加，先回答清楚"这个类别有多宽"。
var (
	_ kernel.Approvable = editFileTool{}
	_ kernel.Approvable = writeFileTool{}
	_ kernel.Approvable = runCommandTool{}
	_ kernel.Approvable = shellTool{}
)

// pathScope 把路径归成"所在目录"。
//
// 归到目录而不是整文件：记一条"允许改 a.go"，下次模型改 b.go 时还得再问；
// 而"允许改这个目录"才是用户点那一项时真正的意思。
func pathScope(path, cwd string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	// 复用 fs.go 的 resolve：审批判定与规则匹配必须用同一个基准，
	// 两套解析规则会让规则"时灵时不灵"。
	return filepath.Dir(filepath.Clean(resolve(cwd, path)))
}

// commandScope 把命令归成"程序名 + 第一个非选项参数"。
//
// 带上第一个参数是必要的：`git status` 可以记（只读），
// 而 `git push` / `git reset` 绝不该被同一条规则放过。
func commandScope(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	verb := trimExe(filepath.Base(argv[0]))
	if len(argv) > 1 && !strings.HasPrefix(argv[1], "-") {
		verb += " " + argv[1]
	}
	return verb
}

// trimExe 去掉 Windows 上的 .exe 后缀。
//
// 不去的后果很隐蔽：`git status` 记下的规则，在模型写成
// `C:\...\git.exe status` 时不匹配——用户看到的是"我设过的规则没生效"，
// 而且换个写法又能生效，几乎不可能靠观察定位。
func trimExe(name string) string {
	if len(name) > 4 && strings.EqualFold(name[len(name)-4:], ".exe") {
		return name[:len(name)-4]
	}
	return name
}

// ---------------------------------------------------------------- 各工具的类别

func (editFileTool) ApprovalScope(args map[string]any, cwd string) string {
	return pathScope(strArg(args, "path"), cwd)
}

func (writeFileTool) ApprovalScope(args map[string]any, cwd string) string {
	return pathScope(strArg(args, "path"), cwd)
}

func (runCommandTool) ApprovalScope(args map[string]any, _ string) string {
	argv, err := splitCommand(args["command"])
	if err != nil {
		// 参数本身有问题时不给类别：Execute 会报错，
		// 而"记住一条对着坏参数的规则"没有任何意义。
		return ""
	}
	return commandScope(argv)
}

func (shellTool) ApprovalScope(args map[string]any, _ string) string {
	return commandScope(strSliceArg(args, "command"))
}
