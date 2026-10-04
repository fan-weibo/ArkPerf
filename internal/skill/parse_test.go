package skill

import (
	"strings"
	"testing"
)

func TestParseAcceptsMinimalSkill(t *testing.T) {
	data := []byte("---\nname: cold-start-baseline\ndescription: 建立冷启动基线并与改动后对比。\n---\n\n正文里的第一行\n第二行\n")

	s, err := Parse(data, "/x/cold-start-baseline/SKILL.md")
	if err != nil {
		t.Fatalf("应当解析成功，却报错：%v", err)
	}
	if s.Name != "cold-start-baseline" {
		t.Errorf("Name = %q", s.Name)
	}
	if s.Description != "建立冷启动基线并与改动后对比。" {
		t.Errorf("Description = %q", s.Description)
	}
	if s.FilePath != "/x/cold-start-baseline/SKILL.md" {
		t.Errorf("FilePath = %q", s.FilePath)
	}
	// Source 由 Discover 按来源填写，Parse 不该越权替它决定。
	if s.Source != "" {
		t.Errorf("Parse 不该设置 Source，得到 %q", s.Source)
	}
}

func TestParseToleratesRealWorldShapes(t *testing.T) {
	cases := []struct {
		name       string
		data       string
		wantName   string
		wantDesc   string
		wantInDesc string
	}{
		{
			name:     "CRLF 换行（Windows 上 git 自动转换过）",
			data:     "---\r\nname: a-b\r\ndescription: 描述\r\n---\r\n正文\r\n",
			wantName: "a-b",
			wantDesc: "描述",
		},
		{
			name:     "带 UTF-8 BOM",
			data:     "\ufeff---\nname: a-b\ndescription: 描述\n---\n",
			wantName: "a-b",
			wantDesc: "描述",
		},
		{
			name:     "值带双引号",
			data:     "---\nname: a-b\ndescription: \"含：冒号的描述\"\n---\n",
			wantName: "a-b",
			wantDesc: "含：冒号的描述",
		},
		{
			name:     "值带单引号",
			data:     "---\nname: a-b\ndescription: '带引号'\n---\n",
			wantName: "a-b",
			wantDesc: "带引号",
		},
		{
			name:     "规范里的其他字段一律忽略",
			data:     "---\nname: a-b\ndescription: 描述\nlicense: MIT\ncompatibility: 需要 node\nallowed-tools: read_file\n---\n",
			wantName: "a-b",
			wantDesc: "描述",
		},
		{
			name:     "frontmatter 后没有正文",
			data:     "---\nname: a-b\ndescription: 描述\n---",
			wantName: "a-b",
			wantDesc: "描述",
		},
		{
			name:     "字段顺序颠倒",
			data:     "---\ndescription: 描述\nname: a-b\n---\n",
			wantName: "a-b",
			wantDesc: "描述",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Parse([]byte(tc.data), "SKILL.md")
			if err != nil {
				t.Fatalf("应当解析成功：%v", err)
			}
			if s.Name != tc.wantName {
				t.Errorf("Name = %q，期望 %q", s.Name, tc.wantName)
			}
			if s.Description != tc.wantDesc {
				t.Errorf("Description = %q，期望 %q", s.Description, tc.wantDesc)
			}
		})
	}
}

func TestParseIgnoresFenceInsideBody(t *testing.T) {
	// 正文里出现 --- 是极常见的（Markdown 分隔线、代码块）。解析必须以
	// frontmatter 的第一对 --- 为准，不能被正文带偏。
	data := []byte("---\nname: a-b\ndescription: 描述\n---\n\n正文\n\n---\n\n更多正文\n")
	s, err := Parse(data, "SKILL.md")
	if err != nil {
		t.Fatalf("应当解析成功：%v", err)
	}
	if s.Name != "a-b" {
		t.Errorf("Name = %q", s.Name)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		wantSub string
	}{
		{"纯正文没有 frontmatter", "这不是技能\n", "缺少 frontmatter"},
		{"frontmatter 未闭合", "---\nname: a-b\ndescription: 描述\n", "没有闭合"},
		{"缺少 name", "---\ndescription: 描述\n---\n", "缺少 name"},
		{"缺少 description", "---\nname: a-b\n---\n", "缺少 description"},
		{"description 只有空白", "---\nname: a-b\ndescription:    \n---\n", "缺少 description"},
		{"name 含大写", "---\nname: ColdStart\ndescription: 描述\n---\n", "不合规"},
		{"name 含下划线", "---\nname: a_b\ndescription: 描述\n---\n", "不合规"},
		{"name 含空格", "---\nname: a b\ndescription: 描述\n---\n", "不合规"},
		{"name 以连字符开头", "---\nname: -ab\ndescription: 描述\n---\n", "不合规"},
		{"name 连续连字符", "---\nname: a--b\ndescription: 描述\n---\n", "不合规"},
		{"description 用 YAML 折叠写法", "---\nname: a-b\ndescription: >\n  多行描述\n---\n", "不支持 YAML 多行"},
		{"description 用 YAML 块写法", "---\nname: a-b\ndescription: |\n  多行描述\n---\n", "不支持 YAML 多行"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.data), "SKILL.md")
			if err == nil {
				t.Fatal("应当报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestParseRejectsOverlongFields(t *testing.T) {
	longName := strings.Repeat("a", maxNameLen+1)
	if _, err := Parse([]byte("---\nname: "+longName+"\ndescription: 描述\n---\n"), "SKILL.md"); err == nil {
		t.Error("超长 name 应当被拒")
	}

	longDesc := strings.Repeat("字", maxDescriptionLen+1)
	if _, err := Parse([]byte("---\nname: a-b\ndescription: "+longDesc+"\n---\n"), "SKILL.md"); err == nil {
		t.Error("超长 description 应当被拒")
	}
}

func TestParseAcceptsBoundaryLengths(t *testing.T) {
	// 恰好等于上限必须放行：给的是"上限"，不是"必须小于"。
	name := strings.Repeat("a", maxNameLen)
	desc := strings.Repeat("字", maxDescriptionLen)
	if _, err := Parse([]byte("---\nname: "+name+"\ndescription: "+desc+"\n---\n"), "SKILL.md"); err != nil {
		t.Errorf("正好等于上限应当放行：%v", err)
	}
}
