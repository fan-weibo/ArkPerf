package skill

import (
	"fmt"
	"strings"
)

// ReadToolName 是读技能正文要用的工具名。
//
// 必须与注册表里那个读文件工具的名字一致（internal/tools 的 read_file）。
// 对不上的表现是"模型照着提示词去调一个不存在的工具"，然后卡住重试——
// 而提示词本身看起来完全正常，是最难查的一类错。改工具名时同步改这里。
const ReadToolName = "read_file"

// RenderBlock 把技能渲染成系统提示词里的一节。没有技能时返回空串。
//
// 只写名字、描述与路径，**正文一个字都不进**。
//
// 这就是技能存在的全部理由：一个技能正文动辄一两千字，20 个技能全量塞进
// 提示词，等于每一轮请求都为它们付一次钱；只放元信息则不到十分之一，
// 等模型判定相关再自己把那一份读进来。所以这一节必须保持短——
// 一旦有人往这里加正文，这个机制就白设计了。
func RenderBlock(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n## 可用技能\n")
	fmt.Fprintf(&sb, "下面是专项说明书，**正文不在这里**：任务与某个技能的描述相关时，先用 %s 读它的 SKILL.md 全文，再照里面的步骤做；不要凭描述去猜它的内容。\n", ReadToolName)
	sb.WriteString("技能里写的相对路径，相对于该 SKILL.md 所在目录解析。\n")

	for _, s := range skills {
		fmt.Fprintf(&sb, "- %s：%s（SKILL.md：%s）\n", s.Name, oneLine(s.Description), s.FilePath)
	}
	return sb.String()
}

// oneLine 把描述压成一行。
//
// 描述里的换行会把列表项拆散，后面的行看起来像不属于任何技能；
// 而 frontmatter 是用户手写的，换行属于正常输入。
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
