package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fan-weibo/ArkPerf/internal/execx"
	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// 执行类工具的超时。默认值与上限都刻意留得宽：
// 鸿蒙构建动辄十几分钟，超时设紧了只会得到"构建失败"的假象，
// 而实际上编译器还在正常工作。
const (
	defaultRunTimeout = 120 * time.Second
	maxRunTimeout     = 20 * time.Minute
)

// shellControlTokens 是 shell 的控制操作符。
//
// ArkPerf **不通过 shell 执行命令**：直接起进程，参数按数组传递。
// 好处是完全没有引号解析、变量展开、管道串联带来的意外——
// 安全护栏因此从"尽力而为的正则"变成真正有效的闸门。
// 代价是不能用一行 `a && b` 写完，得分成两次调用。
var shellControlTokens = map[string]bool{
	"|": true, "||": true, "&": true, "&&": true, ";": true,
	">": true, ">>": true, "<": true, "<<": true, "2>": true, "2>&1": true,
}

// Exec 返回宿主命令执行工具。
func Exec() []kernel.Tool { return []kernel.Tool{runCommandTool{}} }

type runCommandTool struct{}

func (runCommandTool) Name() string { return "run_command" }

func (runCommandTool) Description() string {
	return "在本机执行一条命令（直接起进程，不经过 shell，因而不能用管道/重定向/串联）。" +
		"只读的简单探测（如 git status、ls、go version）免审批；其余需要审批；" +
		"破坏性命令会被安全护栏直接拒绝，审批与 --yes 都无法放行。"
}

func (runCommandTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {
      "description": "要执行的命令。推荐传数组（如 [\"git\",\"status\"]），避免路径含空格时的引号歧义；也可传字符串，会按引号规则切分",
      "oneOf": [
        {"type": "array", "items": {"type": "string"}},
        {"type": "string"}
      ]
    },
    "cwd": {"type": "string", "description": "工作目录，留空用当前工作目录"},
    "timeoutSeconds": {"type": "integer", "description": "超时秒数，默认 120，上限 1200"}
  },
  "required": ["command"],
  "additionalProperties": false
}`)
}

// NeedsApproval 按命令内容判定。
//
// 硬拒的命令返回 false：它不需要用户做决定——护栏已经决定了。
// 若返回 true，会先弹一个审批框，用户点同意后才告诉他"其实不允许"，
// 那对用户是纯粹的打扰。
func (runCommandTool) NeedsApproval(args map[string]any) bool {
	argv, err := splitCommand(args["command"])
	if err != nil {
		return false // 参数本身有问题，直接让 Execute 报错
	}
	joined := strings.Join(argv, " ")
	if kernel.CheckCommand(joined) != nil {
		return false
	}
	return !kernel.IsBareProbe(joined)
}

// ApprovalNote 说明审批取决于命令内容。
func (runCommandTool) ApprovalNote() string {
	return "按命令判定：只读探测免审批，其余需审批；破坏性命令一律拒绝"
}

func (runCommandTool) Execute(ctx context.Context, args map[string]any, tc kernel.ToolCtx) (kernel.ToolResult, error) {
	argv, err := splitCommand(args["command"])
	if err != nil {
		return kernel.ToolResult{}, err
	}
	if len(argv) == 0 {
		return kernel.ToolResult{}, errors.New("command 不能为空")
	}

	// 1. 拒绝 shell 语法：说明清楚为什么，而不是让它悄悄失败
	for _, tok := range argv {
		if shellControlTokens[tok] {
			return kernel.ToolResult{}, fmt.Errorf(
				"不支持 shell 语法（%q）：ArkPerf 直接起进程、不经过 shell，因此没有管道/重定向/串联。"+
					"请拆成多条命令分别调用", tok)
		}
	}

	// 2. 破坏性命令硬拒（凌驾于审批之上，放在审批之前）
	joined := strings.Join(argv, " ")
	if err := kernel.CheckCommand(joined); err != nil {
		return kernel.ToolResult{}, err
	}

	// 3. 执行
	dir := strArg(args, "cwd")
	if dir == "" {
		dir = tc.CWD
	}
	timeout := time.Duration(intArg(args, "timeoutSeconds", 0)) * time.Second
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	if timeout > maxRunTimeout {
		timeout = maxRunTimeout
	}

	res, err := execx.Run(ctx, argv[0], argv[1:], execx.Options{Dir: dir, Timeout: timeout})

	var sb strings.Builder
	fmt.Fprintf(&sb, "命令：%s\n工作目录：%s\n耗时：%s\n",
		execx.Display(argv[0], argv[1:]), dir, res.Duration.Round(time.Millisecond))
	// 进程根本没启动时退出码的零值 0 是假的，如实区分——
	// 否则"退出码：0"配上 ERROR 标签，连模型都会被搞糊涂（实测发生过）。
	if res.Started {
		fmt.Fprintf(&sb, "退出码：%d\n", res.ExitCode)
	} else {
		sb.WriteString("退出码：—（进程未启动）\n")
	}
	if res.Output != "" {
		sb.WriteString("\n")
		sb.WriteString(res.Output)
	}

	// 命令失败了：把**原因**一并交回模型。只给退出码等于什么都没说，
	// 模型会基于残缺信息瞎猜（实测它确实猜了）。
	if err != nil {
		sb.WriteString("\n失败原因：" + err.Error())
		return kernel.ToolResult{Output: sb.String(), IsError: true}, nil
	}
	return kernel.ToolResult{Output: sb.String()}, nil
}

// ---------------------------------------------------------------- 命令切分

// splitCommand 把命令参数解析成 argv。
//
// 首选数组形式：那是唯一没有歧义的表示，路径含空格时也不用猜引号规则。
// 字符串形式是给模型的便利，按 shell 的引号习惯切分——
// 但**不展开变量、不处理通配符、不认转义以外的特殊语义**。
func splitCommand(v any) ([]string, error) {
	switch cmd := v.(type) {
	case []string:
		return trimTokens(cmd), nil
	case []any:
		out := make([]string, 0, len(cmd))
		for _, item := range cmd {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("command 数组里出现了非字符串元素：%T", item)
			}
			out = append(out, s)
		}
		return trimTokens(out), nil
	case string:
		// 模型经常把数组整段序列化成字符串塞进来：`["git","status"]`。
		// 这不是罕见错误而是常见写法（实测就撞到过），兜住它比报错有用得多。
		if t := strings.TrimSpace(cmd); strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			var arr []string
			if err := json.Unmarshal([]byte(t), &arr); err == nil && len(arr) > 0 {
				return trimTokens(arr), nil
			}
		}
		return tokenize(cmd)
	default:
		return nil, errors.New("command 必须是字符串或字符串数组")
	}
}

func trimTokens(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// tokenize 按空白切分，并处理成对的单/双引号。
//
// 只做这一件事：不展开 $VAR、不做通配符、不处理反斜杠转义以外的语法。
// 少即是好——每多一条 shell 语义，就多一类"看起来能跑、实际跑错"的坑。
func tokenize(s string) ([]string, error) {
	var (
		tokens  []string
		current strings.Builder
		quote   rune // 0 表示不在引号内
		started bool
	)
	flush := func() {
		if started {
			tokens = append(tokens, current.String())
			current.Reset()
			started = false
		}
	}

	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
			started = true
		case r == '\'' || r == '"':
			quote = r
			started = true // 空引号 "" 也要算一个空参数
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, errors.New("命令里的引号没有闭合")
	}
	flush()
	return tokens, nil
}

func intArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err == nil {
			return n
		}
	}
	return def
}
