package harmony

import (
	"encoding/json"
	"os"
	"strings"
)

// OpenHarmony 的配置文件是 JSON5：带注释、单引号字符串、尾逗号。
// 直接用 encoding/json 读必然失败（实测第一个碰到的就是 module.json5 里的
// // 注释）。这里写一个够用的降级解析器：把 JSON5 规整成合法 JSON。
//
// 刻意不写成完整 JSON5 实现：我们只需要读懂 DevEco 生成的那几个配置文件，
// 完整实现（多行字符串、十六进制、加号数字…）带来的复杂度在这里换不来任何东西。

// stripJSON5 去掉注释、把单引号字符串转成双引号、删除尾逗号。
//
// 用状态机而不是正则：正则会把字符串里的 "//"（比如资源路径 "$media:app_icon"
// 之类不会，但 URL 会）当成注释删掉，把配置改坏。
func stripJSON5(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))

	runes := []rune(s)
	n := len(runes)
	for i := 0; i < n; {
		c := runes[i]

		// ---- 字符串 ----
		if c == '"' || c == '\'' {
			quote := c
			// 统一输出双引号
			sb.WriteByte('"')
			i++
			for i < n {
				ch := runes[i]
				if ch == '\\' && i+1 < n {
					// 转义原样保留；单引号串里的 \' 要变成 '
					if runes[i+1] == '\'' && quote == '\'' {
						sb.WriteByte('\'')
					} else {
						sb.WriteByte('\\')
						sb.WriteRune(runes[i+1])
					}
					i += 2
					continue
				}
				if ch == quote {
					sb.WriteByte('"')
					i++
					break
				}
				if ch == '"' && quote == '\'' {
					// 单引号串内部的双引号要转义，否则提前结束字符串
					sb.WriteString(`\"`)
					i++
					continue
				}
				sb.WriteRune(ch)
				i++
			}
			continue
		}

		// ---- 裸键（name: 形式）----
		//
		// JSON5 允许键不加引号，DevEco 生成的配置里就有这种写法。
		// 判据是"标识符后面紧跟冒号"——这样 true/false/null 这类裸值不会被误当成键。
		if isIdentStart(c) {
			j := i
			for j < n && isIdentChar(runes[j]) {
				j++
			}
			k := j
			for k < n && isSpaceRune(runes[k]) {
				k++
			}
			if k < n && runes[k] == ':' {
				sb.WriteByte('"')
				sb.WriteString(string(runes[i:j]))
				sb.WriteByte('"')
			} else {
				sb.WriteString(string(runes[i:j]))
			}
			i = j
			continue
		}

		// ---- 行注释 ----
		if c == '/' && i+1 < n && runes[i+1] == '/' {
			for i < n && runes[i] != '\n' {
				i++
			}
			continue
		}

		// ---- 块注释 ----
		if c == '/' && i+1 < n && runes[i+1] == '*' {
			i += 2
			for i+1 < n && !(runes[i] == '*' && runes[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2
			}
			continue
		}

		sb.WriteRune(c)
		i++
	}
	return stripTrailingCommas(sb.String())
}

// stripTrailingCommas 删除对象/数组末尾多余的逗号。
func stripTrailingCommas(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	runes := []rune(s)
	inString := false
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if c == '"' {
			// 跳过整个字符串（含转义）
			inString = !inString
			sb.WriteRune(c)
			continue
		}
		if inString {
			if c == '\\' && i+1 < len(runes) {
				sb.WriteRune(c)
				i++
				sb.WriteRune(runes[i])
				continue
			}
			sb.WriteRune(c)
			continue
		}
		if c == ',' {
			// 看后面是不是只有空白 + 结束符
			j := i + 1
			for j < len(runes) && isSpaceRune(runes[j]) {
				j++
			}
			if j < len(runes) && (runes[j] == '}' || runes[j] == ']') {
				continue // 丢掉这个逗号
			}
		}
		sb.WriteRune(c)
	}
	return sb.String()
}

func isIdentStart(r rune) bool {
	return r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isIdentChar(r rune) bool {
	return isIdentStart(r) || (r >= '0' && r <= '9')
}

func isSpaceRune(r rune) bool {
	switch r {
	case ' ', '\t', '\r', '\n':
		return true
	}
	return false
}

// parseJSON5 读一个 JSON5 文件并解析到 v。
//
// 文件不存在返回 os.ErrNotExist 语义的错误链，调用方可以据此区分
// "配置缺失" 与 "配置写错了" —— 前者是工程结构问题，后者是内容问题。
func parseJSON5(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	normalized := stripJSON5(string(data))
	if err := json.Unmarshal([]byte(normalized), v); err != nil {
		return &json5Error{path: path, err: err}
	}
	return nil
}

type json5Error struct {
	path string
	err  error
}

func (e *json5Error) Error() string {
	return "解析 " + e.path + " 失败：" + e.err.Error()
}

func (e *json5Error) Unwrap() error { return e.err }
