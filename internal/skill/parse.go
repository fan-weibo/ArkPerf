package skill

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// FileName 是技能定义文件名。大小写敏感：规范里就是全大写。
	FileName = "SKILL.md"

	// fence 是 frontmatter 的分隔线。
	fence = "---"

	// maxNameLen / maxDescriptionLen 与 Agent Skills 规范一致。
	//
	// 上限不是洁癖：描述会进每一轮请求的系统提示词，一个 4KB 的描述
	// 乘以 20 个技能就是 8 万字符——那正好抵消掉"按需加载"省下来的钱。
	maxNameLen        = 64
	maxDescriptionLen = 1024
)

// nameRe 校验技能名。
//
// 限定字符集不只是为了规范：名字会拼进系统提示词，放开到任意字符串
// 就等于让一个 SKILL.md 能往提示词里塞任意内容。
var nameRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Parse 解析一份 SKILL.md 的元信息。
//
// 只读 frontmatter，正文一概不看——正文动辄几千字，而这里只需要知道
// 「这个技能叫什么、什么时候该用它」。正文等模型判定相关后自己读。
//
// 任何不合规都返回 error（由 Discover 记成诊断并跳过）：宁可少加载一个技能，
// 也不要让一个半残的技能进提示词——模型会照着半残的描述去猜。
func Parse(data []byte, path string) (Skill, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	lines := strings.Split(text, "\n")

	// 必须从第一行的 --- 开始，不做「向后搜索第一个 ---」的宽松解析：
	// 正文里出现分隔线很常见，宽松解析会把正文当元信息读进去。
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != fence {
		return Skill{}, errors.New("缺少 frontmatter：文件必须以单独一行 --- 开头")
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == fence {
			end = i
			break
		}
	}
	if end < 0 {
		return Skill{}, errors.New("frontmatter 没有闭合：缺少第二个 ---")
	}

	var name, desc string
	for _, raw := range lines[1:end] {
		key, value, ok := strings.Cut(raw, ":")
		if !ok {
			continue // 空行、注释、缩进续行：都不是我们支持的形状，忽略
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "name":
			name = unquote(value)
		case "description":
			// YAML 的多行写法（> 与 |）我们不支持，而且必须说出来。
			// 静默当成空值的话，用户看到的现象是「技能没被加载」，
			// 而原因（他用了折叠语法）完全看不出来。
			if value == ">" || value == "|" || value == ">-" || value == "|-" {
				return Skill{}, errors.New("description 不支持 YAML 多行写法（> / |）：请写成单行")
			}
			desc = unquote(value)
		}
	}

	if err := validateName(name); err != nil {
		return Skill{}, err
	}
	if desc == "" {
		// 描述是技能唯一的自动触发条件：没有它，模型永远不会知道该读这个技能。
		return Skill{}, errors.New("缺少 description：它是模型判断「何时该用这个技能」的唯一依据")
	}
	if n := utf8.RuneCountInString(desc); n > maxDescriptionLen {
		return Skill{}, fmt.Errorf("description 过长（%d 字符，上限 %d）：请精简到一句话", n, maxDescriptionLen)
	}

	return Skill{
		Name:        name,
		Description: desc,
		FilePath:    path,
	}, nil
}

// validateName 检查技能名是否合规。
func validateName(name string) error {
	if name == "" {
		return errors.New("缺少 name")
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return fmt.Errorf("name 过长（%d 字符，上限 %d）", utf8.RuneCountInString(name), maxNameLen)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("name %q 不合规：只允许小写字母、数字与连字符，且不能以连字符开头或结尾", name)
	}
	return nil
}

// unquote 去掉值两侧成对的引号。
//
// 用户从别处抄过来的 frontmatter 经常带引号（尤其描述里有冒号时），
// 而引号本身不是内容——不剥掉的话它会被显示给模型。
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return strings.TrimSpace(v[1 : len(v)-1])
		}
	}
	return v
}
