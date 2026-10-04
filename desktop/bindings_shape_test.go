package main

import (
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/app"
)

// msgLines 是 app 层回放行到前端绑定形状的转换。回放逻辑本身在 app 层
// （TUI 与桌面端共用一份），这里只锁住**字段别错位**——前端按 Role 分发渲染，
// 错一个字段就是"工具行显示成文字"这类一眼能看出、却很难查回来的错。
func TestMsgLinesCarriesAllFields(t *testing.T) {
	in := []app.TranscriptLine{
		{Role: "user", Content: "建个文件"},
		{Role: "assistant", Content: "好的"},
		{Role: "tool", Name: "write_file", Content: `{"path":"a.txt"}`},
		{Role: "result", Name: "write_file", Content: "已创建", IsError: false},
		{Role: "result", Name: "read_file", Content: "ERROR: 不存在", IsError: true},
	}

	got := msgLines(in)
	if len(got) != len(in) {
		t.Fatalf("行数不对：%d != %d", len(got), len(in))
	}
	for i := range in {
		if got[i].Role != in[i].Role || got[i].Content != in[i].Content ||
			got[i].Name != in[i].Name || got[i].IsErr != in[i].IsError {
			t.Fatalf("第 %d 行字段错位：got %+v want %+v", i, got[i], in[i])
		}
	}
}

// 空输入不该产生非 nil 的奇怪结果（前端会拿到 null 或空数组）。
func TestMsgLinesEmpty(t *testing.T) {
	if got := msgLines(nil); len(got) != 0 {
		t.Fatalf("空输入应当是空输出，实际 %d 行", len(got))
	}
}
