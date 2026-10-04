package harmony

import (
	"strings"
)

// BuildDiagnosis 是对一次构建失败日志的判断。
type BuildDiagnosis struct {
	// Cause 是分类键；Confident=false 时是 "unknown"。
	Cause string
	// Title 是给人看的一句话结论。
	Title string
	// Advice 是具体可执行的建议。
	Advice []string
	// Evidence 是日志里支持这个判断的那几行原文。
	Evidence string
	// Confident 为 false 表示没匹配上任何已知类别。
	Confident bool
}

// 各类失败的特征串。
//
// 选特征的经验（踩过才知道）：
//   - **优先匹配错误码**，不要匹配提示文案。文案会随版本改，错误码不会。
//     典型就是 SDK 版本不匹配：提示语换过好几种，但
//     "Specification Limit Violation" 这个串一直在。
//   - 每条都要配"下一步做什么"。只说"签名失败"没有用，
//     用户不知道是该重签还是该改配置。
var buildCauses = []struct {
	cause  string
	title  string
	hints  []string
	advice []string
}{
	{
		cause: "sdk-home",
		title: "SDK 根目录没有注入（DEVECO_SDK_HOME 缺失或无效）",
		hints: []string{"DEVECO_SDK_HOME", "Invalid value of 'DEVECO_SDK_HOME'", "sdk directory does not exist"},
		advice: []string{
			"命令行构建必须显式设置 DEVECO_SDK_HOME（DevEco 的内置终端会自动设，裸 shell 不会）",
			"把它指向 DevEco 安装目录下的 sdk 目录，例如 D:\\DevEco Studio\\sdk",
			"ArkPerf 的 harmony_build 会自动补这个值；若你是手工跑 hvigorw，需要自己 export/set",
		},
	},
	{
		cause: "sdk-version",
		title: "SDK 版本与工程要求的 compatibleSdkVersion 不匹配",
		hints: []string{"Specification Limit Violation", "compatibleSdkVersion", "apiVersion", "not supported by the current SDK"},
		advice: []string{
			"用 harmony_project_profile 看工程的 compatibleSdkVersion",
			"在 DevEco 的 Settings → SDK 里装对应版本的 SDK，或把工程配置降到已装版本",
			"本机的 SDK 版本可用 harmony_toolchain_check 查看",
		},
	},
	{
		cause: "signing",
		title: "签名材料问题（证书 / profile / 密码）",
		hints: []string{"sign", "p7b", "profile", "certificate", "keystore", "keyAlias", "Signature"},
		advice: []string{
			"先用 harmony_build 产出未签名 hap，再用 harmony_sign 用本地调试身份签名",
			"注意：SDK 自带的 UnsgnedDebugProfileTemplate.json 有效期可能已过期，harmony_sign 会自动改写有效期",
			"签名别名必须是 profile debug 别名（release 别名会因自签证书链校验失败）",
		},
	},
	{
		cause: "ohpm-deps",
		title: "依赖问题（ohpm 装包失败或版本解析不到）",
		hints: []string{"ohpm", "Install failed", "dependency", "not found in registry", "oh-package"},
		advice: []string{
			"在工程根目录跑 ohpm install（ArkPerf 的构建不会替你装依赖）",
			"检查 oh-package.json5 里的版本号与私有源配置（.ohpmrc）",
			"网络不通时先确认能否访问 ohpm 仓库；公司网络可能需要配代理",
		},
	},
	{
		cause: "hvigor-env",
		title: "构建环境缺失（Node / hvigor 版本或内存不足）",
		hints: []string{"node", "hvigor", "OutOfMemory", "heap", "Cannot find module", "npm ERR"},
		advice: []string{
			"跑 harmony_toolchain_check 看 node / hvigorw 是否都可用",
			"Node 版本过低会让 hvigor 直接崩，DevEco 自带的 tools/node 通常可用",
			"内存不足时可给 Node 加大堆：NODE_OPTIONS=--max-old-space-size=8192",
		},
	},
	{
		cause: "arkts-source",
		title: "ArkTS / ETS 源码编译错误",
		hints: []string{".ets", ".ts:", "ArkTS", "SyntaxError", "Type mismatch", "is not assignable"},
		advice: []string{
			"看日志里第一个 ERROR 行，ArkTS 的错误通常带文件名与行列号",
			"ArkTS 比 TypeScript 严格（不允许动态类型、不支持部分语法），改法与 TS 不同",
			"可用 harmony_lint 先做一次静态检查",
		},
	},
	{
		cause: "config",
		title: "工程配置错误（build-profile / module.json5）",
		hints: []string{"build-profile", "module.json5", "main_pages", "Invalid value", "config"},
		advice: []string{
			"用 harmony_schema_check 做一次配置预检——它会直接指出是哪个文件的哪个字段",
			"重点看 module.json5 的 type / mainElement / deviceTypes 与模块 build-profile 的 targets",
		},
	},
	{
		cause: "network",
		title: "网络问题（下载依赖或组件超时）",
		hints: []string{"timeout", "ETIMEDOUT", "ECONNRESET", "network", "proxy", "ENOTFOUND"},
		advice: []string{
			"确认能访问 ohpm / npm 仓库；必要时配置代理或镜像",
			"重试一次：偶发超时很常见",
		},
	},
}

// diagnoseTail 是参与分类的日志长度上限。
//
// 构建日志动辄上万行，而"为什么失败"几乎总在最后。只取尾部既快又准——
// 从头扫会被前面的 WARN 干扰，命中一堆无关分类。
const diagnoseTail = 8000

// DiagnoseBuildLog 对构建日志做分类诊断。
//
// 判断不了时**不撒谎**：返回 Confident=false，并把首个 ERROR 块与日志尾部
// 原样带回去。宁可说"我没认出来"，也不要给一个看起来很像的错误归因——
// 后者会把人带到完全错误的方向。
func DiagnoseBuildLog(log string) BuildDiagnosis {
	raw := strings.TrimSpace(log)
	if raw == "" {
		return BuildDiagnosis{Cause: "empty", Title: "日志为空（构建可能没真正执行）",
			Advice: []string{"先跑 harmony_build 拿到输出，再把日志交给 harmony_build_doctor"}}
	}
	tail := raw
	if len(tail) > diagnoseTail {
		tail = tail[len(tail)-diagnoseTail:]
	}
	lower := strings.ToLower(tail)

	for _, c := range buildCauses {
		for _, hint := range c.hints {
			if !strings.Contains(lower, strings.ToLower(hint)) {
				continue
			}
			return BuildDiagnosis{
				Cause:     c.cause,
				Title:     c.title,
				Advice:    c.advice,
				Evidence:  evidenceAround(tail, hint),
				Confident: true,
			}
		}
	}
	return BuildDiagnosis{
		Cause:    "unknown",
		Title:    "没能归入已知类别——下面是原始证据",
		Evidence: firstErrorBlock(raw),
		Advice: []string{
			"看下面 tail 里的最后一段，hvigor 通常把真实原因放在最后",
			"如果想让 ArkPerf 以后能认出来，把这段日志发给我，我把它加进分类表",
		},
	}
}

// evidenceAround 取命中串附近的几行作为证据。
func evidenceAround(log, hint string) string {
	lower := strings.ToLower(log)
	idx := strings.Index(lower, strings.ToLower(hint))
	if idx < 0 {
		return ""
	}
	lines := strings.Split(log, "\n")
	// 命中行号
	lineNo := strings.Count(log[:idx], "\n")
	from := lineNo - 2
	if from < 0 {
		from = 0
	}
	to := lineNo + 3
	if to > len(lines) {
		to = len(lines)
	}
	var sb strings.Builder
	for i := from; i < to; i++ {
		if s := strings.TrimSpace(lines[i]); s != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(s)
		}
	}
	return clip(sb.String(), 1200)
}

// firstErrorBlock 取日志里第一个 ERROR 行及其后若干行。
func firstErrorBlock(log string) string {
	lines := strings.Split(log, "\n")
	for i, line := range lines {
		if !strings.Contains(strings.ToUpper(line), "ERROR") {
			continue
		}
		end := i + 6
		if end > len(lines) {
			end = len(lines)
		}
		var sb strings.Builder
		for j := i; j < end; j++ {
			if s := strings.TrimSpace(lines[j]); s != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(s)
			}
		}
		out := sb.String()
		if t := logTail(log, 1200); t != "" {
			out += "\n…\n" + t
		}
		return out
	}
	return logTail(log, 1200)
}

// logTail 取日志末尾若干字节，从整行开始。
func logTail(s string, limit int) string {
	s = strings.TrimRight(s, " \t\r\n")
	if len(s) <= limit {
		return s
	}
	cut := s[len(s)-limit:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[i+1:]
	}
	return "（日志尾部）\n" + cut
}

func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
