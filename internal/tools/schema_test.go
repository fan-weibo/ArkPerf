package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// collectAll 拿到全部本地工具（含鸿蒙域）。
func collectAll() []kernel.Tool {
	out := make([]kernel.Tool, 0, 32)
	out = append(out, FS()...)
	out = append(out, Exec()...)
	out = append(out, Search()...)
	out = append(out, Harmony(nil)...)
	return out
}

// 每个工具的参数 schema 必须是**合法 JSON**。
//
// 它会被原样嵌进发给模型的请求里（json.RawMessage），坏一个就
// 整个请求都发不出去——而且报错是编码器吐的"invalid character '\n'
// after object key value pair"，完全看不出是哪个工具、哪个字段，
// 用户看到的只是一句天书。所以必须在源头把它拦下来。
//
// 这条测试就是干这个的：哪家的 schema 坏了，直接点名。
// 全部 30 个工具都在内——鸿蒙域的 schema 一样会被嵌进请求，
// 坏一个同样能让所有任务发不出去。
func TestEveryToolSchemaIsValidJSON(t *testing.T) {
	for _, tool := range collectAll() {
		raw := tool.Parameters()
		if len(raw) == 0 {
			t.Errorf("%s：没有参数 schema", tool.Name())
			continue
		}
		if !json.Valid(raw) {
			// 报出出错位置附近的内容，定位不用再翻源码
			snippet := string(raw)
			if len(snippet) > 400 {
				snippet = snippet[:400] + "…"
			}
			t.Errorf("%s：参数 schema 不是合法 JSON\n---\n%s\n---", tool.Name(), snippet)
		}
	}
}

// schema 顶层必须是 object：模型按 JSON Schema 校验参数，
// 顶层不是 object 时有的端点会静默忽略整个工具定义。
func TestEveryToolSchemaTopLevelIsObject(t *testing.T) {
	for _, tool := range collectAll() {
		var top struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(tool.Parameters(), &top); err != nil {
			t.Errorf("%s：schema 解析失败（先修合法性）：%v", tool.Name(), err)
			continue
		}
		if !strings.EqualFold(top.Type, "object") {
			t.Errorf("%s：顶层 type = %q，期望 object", tool.Name(), top.Type)
		}
	}
}
