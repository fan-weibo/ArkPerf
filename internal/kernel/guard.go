package kernel

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// 这个文件是 ArkPerf 的安全策略层：**路径分级**与**破坏性命令护栏**。
//
// 两者合在一处是有意的——它们是同一条底线的两面：
// 分级决定"要不要问用户"，护栏决定"无论问不问都不许做"。
// 分散到各工具里，迟早会出现某个工具漏掉其中一条。

// ---------------------------------------------------------------- 路径分级

// PathTier 描述一个路径相对"工作区"与"状态根"的位置。
type PathTier int

const (
	// TierWorkspace 工作区内：agent 的正常作业范围，可以免审批。
	TierWorkspace PathTier = iota
	// TierHome 状态根内（~/.arkperf）：存放配置、会话与规则，**永远要审批**。
	TierHome
	// TierOutside 工作区外：要审批。
	TierOutside
)

func (t PathTier) String() string {
	switch t {
	case TierWorkspace:
		return "工作区内"
	case TierHome:
		return "状态根内"
	default:
		return "工作区外"
	}
}

// ClassifyPath 判定路径属于哪一层。
//
// **状态根优先于工作区**：如果用户在 ~/.arkperf 里工作，工作区规则不能把
// "改 ArkPerf 自己的配置"变成免审批——那正是 agent 最容易把自己（以及用户的
// 审批策略）弄坏的地方。
func ClassifyPath(path, cwd, home string) PathTier {
	if insideRoot(home, path) {
		return TierHome
	}
	if insideRoot(cwd, path) {
		return TierWorkspace
	}
	return TierOutside
}

// insideRoot 判断 child 是否落在 parent 之内。
//
// 三个坑：
//  1. 前缀比较会把 /foo 和 /foobar 判成同一处 → 必须按路径分隔符定边界
//  2. Windows 路径大小写不敏感，不统一大小写会让 C:\Proj 与 c:\proj 被判成两地
//  3. 相对路径要先转绝对，否则 "" 与 "." 之类的输入会得出错误结论
//
// 已知取舍：**不解析符号链接**。工作区内的软链可以指向外部，
// 这里看不出来。要真正隔离得靠操作系统级沙箱，正则与路径比较做不到。
func insideRoot(parent, child string) bool {
	if strings.TrimSpace(parent) == "" {
		return false
	}
	absParent, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	absChild, err := filepath.Abs(child)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		absParent = strings.ToLower(absParent)
		absChild = strings.ToLower(absChild)
	}

	rel, err := filepath.Rel(absParent, absChild)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// rel 以 .. 开头（且不是 "..foo" 这种同名前缀）说明在外部
	return !(rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// InHome 判断路径是否落在状态根（~/.arkperf）内。
//
// 单独提供一个是因为"这是不是 ArkPerf 自己的文件"在很多地方要用
// （写前备份、审批分级、将来的规则文件保护），而它们不必都去构造 cwd。
func InHome(path string) bool { return insideRoot(Home(), path) }

// ---------------------------------------------------------------- 命令护栏

// ErrDenied 是"被安全护栏拒绝"的哨兵错误。
var ErrDenied = errors.New("命令被安全护栏拒绝")

// DeniedError 指明命中了哪条规则。
//
// 必须说清原因：只说"被拒绝"会让用户以为工具坏了，
// 进而想方设法绕过——那比不设护栏更糟。
type DeniedError struct {
	Rule   string
	Reason string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("%s：%s（这条规则不受审批与 --yes 影响）", e.Rule, e.Reason)
}

// Is 让 errors.Is(err, ErrDenied) 成立。
func (e *DeniedError) Is(target error) bool { return target == ErrDenied }

// deniedPattern 是一条硬拒规则。
type deniedPattern struct {
	rule   string
	reason string
	re     *regexp.Regexp
}

// cmdStart 匹配"命令起始位置"：字符串开头，或跟在 ; & | 换行之后。
//
// 为什么需要它：护栏认的必须是"命令位置"，而不是"文本里出现过"。
// 没有这个锚点，`git commit -m "别跑 rm -rf /"` 这种把危险命令**当文字提到**
// 的提交信息也会被拦——那是纯粹误伤，而误伤多了用户就会把护栏关掉。
//
// 同时允许 sudo / doas 前缀：`sudo rm -rf /` 显然也是要拦的。
const cmdStart = `(?i)(?:^|[;&|\n]\s*)\s*(?:sudo\s+|doas\s+)?`

// deniedPatterns 是**凌驾于审批之上**的硬拒清单。
//
// 与审批的区别必须说清楚：审批是"用户可能知道自己在做什么"，
// 硬拒是"这件事没有任何正当理由由 agent 代劳"。因此这里匹配到的命令
// 连问都不问，直接拒——`--yes` 与 approval:auto 都放行不了。
//
// 诚实声明：**这不是沙箱**。正则黑名单天然可被绕过（加引号拆词、
// 命令替换、换成等价命令）。它的作用是拦住"手滑"和"模型想当然"，
// 把破坏性操作的门槛抬高到需要刻意为之。真正的隔离需要 OS 级沙箱。
var deniedPatterns = []deniedPattern{
	{
		rule:   "递归删除根或家目录",
		reason: "会清空整个文件系统或用户主目录",
		re:     regexp.MustCompile(cmdStart + `rm\s+(?:-\w*\s+)*-\w*[rf]\w*[rf]\w*\s+(?:/|~|\$HOME|/\*)(?:\s|$)`),
	},
	{
		rule:   "递归删除 Windows 盘根",
		reason: "会清空整个磁盘分区",
		re:     regexp.MustCompile(cmdStart + `(?:rd|rmdir|del)\b[^\n]*\s/[sq]\b[^\n]*\b[a-z]:\\?(?:\s|$)`),
	},
	{
		rule:   "递归强删系统目录",
		reason: "会破坏操作系统",
		re:     regexp.MustCompile(cmdStart + `remove-item[^\n]*-recurse[^\n]*(?:-force)?[^\n]*(?:[a-z]:\\(?:windows|program files|programdata|users)|\$env:systemroot)`),
	},
	{
		rule:   "格式化磁盘",
		reason: "会清空磁盘分区",
		re:     regexp.MustCompile(cmdStart + `format\s+[a-z]:`),
	},
	{
		rule:   "关机或重启宿主",
		reason: "会中断用户正在做的一切（含正在跑的测量）",
		re:     regexp.MustCompile(cmdStart + `(?:shutdown|reboot)\b\s+(?:/[srp]|-h|-r|-s)`),
	},
	{
		rule:   "通配符杀进程",
		reason: "会终止大量无关进程",
		re:     regexp.MustCompile(cmdStart + `taskkill\b[^\n]*/im\s+\*`),
	},
	{
		rule:   "写注册表自启动项",
		reason: "会留下持久化后门",
		re:     regexp.MustCompile(cmdStart + `reg\s+add\b[^\n]*\\run\b`),
	},
	{
		rule:   "管道下载并执行脚本",
		reason: "在未审查的情况下执行远端代码",
		re:     regexp.MustCompile(cmdStart + `(?:curl|wget|iwr|invoke-webrequest)\b[^\n|]*\|\s*(?:sudo\s+)?(?:ba|z|da)?sh\b`),
	},
	{
		rule:   "下载内容直接求值",
		reason: "在未审查的情况下执行远端代码",
		re:     regexp.MustCompile(cmdStart + `(?:iwr|invoke-webrequest)\b[^\n|]*\|\s*(?:iex|invoke-expression)\b`),
	},
	{
		rule:   "放开脚本执行策略",
		reason: "会解除 Windows 对脚本执行的限制",
		re:     regexp.MustCompile(cmdStart + `set-executionpolicy[^\n]*(?:unrestricted|bypass)`),
	},
	{
		rule:   "低层磁盘操作",
		reason: "写错目标会直接损坏磁盘数据",
		re:     regexp.MustCompile(cmdStart + `(?:(?:mkfs(?:\.\w+)?|fdisk|diskpart)\b|dd\b[^\n]*of=/dev/)`),
	},
	{
		rule:   "抹除设备系统分区",
		reason: "会让设备无法启动",
		re:     regexp.MustCompile(cmdStart + `rm\s+(?:-\w*\s+)*-\w*r\w*f\w*\s+/(?:data|system|vendor|storage)(?:\s|$|\*)`),
	},
}

// CheckCommand 检查一条命令是否触犯硬拒规则。
//
// 三个执行入口（run_command、harmony_shell，将来还有别的）都必须先过这里，
// 而且要在**问用户之前**过——被拒的命令不该先弹一个审批框再告诉用户"其实不行"。
func CheckCommand(command string) error {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return nil
	}
	for _, p := range deniedPatterns {
		if p.re.MatchString(cmd) {
			return &DeniedError{Rule: p.rule, Reason: p.reason}
		}
	}
	return nil
}

// DeniedRules 返回全部规则名与理由，供 `/rules` 之类的展示使用。
func DeniedRules() [][2]string {
	out := make([][2]string, 0, len(deniedPatterns))
	for _, p := range deniedPatterns {
		out = append(out, [2]string{p.rule, p.reason})
	}
	return out
}

// ---------------------------------------------------------------- 只读快路径

// 无可读的字符：出现任何一个都说明这条命令不只是"读一下"。
// 特别地 `>` 与 `<` 是重定向，本质是写。
const shellMetaChars = "|&;<>$`(){}\n\r\t!*?[]"

// bareProbeVerbs 是"裸探测"白名单：命令本身只读、不接受危险子命令。
var bareProbeVerbs = map[string]bool{
	"pwd": true, "ls": true, "dir": true, "cat": true, "type": true,
	"head": true, "tail": true, "wc": true, "file": true, "stat": true,
	"whoami": true, "hostname": true, "date": true, "env": true,
}

// bareProbeSubcommands 是需要限定子命令的动词：只有这些子命令是只读的。
var bareProbeSubcommands = map[string]map[string]bool{
	"git":  {"status": true, "log": true, "diff": true, "show": true, "branch": true, "remote": true, "rev-parse": true, "describe": true},
	"go":   {"version": true, "env": true, "list": true, "vet": true},
	"node": {"--version": true, "-v": true},
	"java": {"-version": true, "--version": true},
	"hdc":  {"list": true},
	"adb":  {"devices": true},
	"npm":  {"ls": true, "view": true},
}

// IsBareProbe 判断命令是否属于"只读裸探测"，从而可以免审批。
//
// 判定刻意收得很紧：单个命令、无 shell 元字符、动词在白名单内、
// 需要限定子命令的动词还必须在只读子命令里。
//
// 为什么值得开这个口子：什么都问，用户会疲劳，最后无脑按 y——
// "审批疲劳"本身就是安全风险。让真正无害的读操作静默通过，
// 用户才会对**真正需要判断**的那几次认真看。
//
// 诚实声明：只读不等于无害。读出来的内容会进入模型上下文，
// 这正是模型的职责所在；这里只解决"要不要打断用户"。
func IsBareProbe(command string) bool {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return false
	}
	if strings.ContainsAny(cmd, shellMetaChars) {
		return false
	}

	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}

	verb := strings.ToLower(strings.TrimSuffix(filepath.Base(fields[0]), ".exe"))
	if bareProbeVerbs[verb] {
		return true
	}

	subs, ok := bareProbeSubcommands[verb]
	if !ok {
		return false
	}
	if len(fields) < 2 {
		// 只给了动词（例如 `git`），不认为无害
		return false
	}
	return subs[strings.ToLower(fields[1])]
}
