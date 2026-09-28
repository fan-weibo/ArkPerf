package kernel

import (
	"context"
	"encoding/json"
	"testing"
)

// fakeTool 是最小的 Tool 实现，用于注册表与循环的离线测试。
type fakeTool struct {
	name     string
	schema   json.RawMessage
	approval bool
	output   string
	err      error
}

func (f fakeTool) Name() string        { return f.name }
func (f fakeTool) Description() string { return "fake tool for tests" }

func (f fakeTool) Parameters() json.RawMessage {
	if f.schema != nil {
		return f.schema
	}
	return json.RawMessage(`{"type":"object"}`)
}

func (f fakeTool) NeedsApproval(map[string]any) bool { return f.approval }

func (f fakeTool) Execute(context.Context, map[string]any, ToolCtx) (ToolResult, error) {
	return ToolResult{Output: f.output}, f.err
}

func TestRegisterAndGet(t *testing.T) {
	reg := NewRegistry()
	reg.Register(fakeTool{name: "read_file", output: "ok"})

	got, ok := reg.Get("read_file")
	if !ok {
		t.Fatal("read_file not found")
	}
	res, err := got.Execute(t.Context(), nil, ToolCtx{})
	if err != nil || res.Output != "ok" {
		t.Fatalf("unexpected result: %+v err=%v", res, err)
	}
	if _, ok := reg.Get("nope"); ok {
		t.Fatal("missing tool must not be found")
	}
	if reg.Len() != 1 {
		t.Fatalf("Len: %d", reg.Len())
	}
}

func TestRegisterRejectsInvalidTools(t *testing.T) {
	reg := NewRegistry()
	reg.Register(fakeTool{name: "dup"})

	cases := []struct {
		name string
		tool Tool
	}{
		{"duplicate name", fakeTool{name: "dup"}},
		{"empty name", fakeTool{name: ""}},
		{"nil tool", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic for %s", tc.name)
				}
			}()
			reg.Register(tc.tool)
		})
	}
}

// 注册必须可逆：卸载/热替换不留悬空注册项。
func TestUnregisterRemovesTool(t *testing.T) {
	reg := NewRegistry()
	unregister := reg.Register(fakeTool{name: "temp"})
	if reg.Len() != 1 {
		t.Fatalf("Len before: %d", reg.Len())
	}

	unregister()
	if reg.Len() != 0 {
		t.Fatalf("Len after: %d", reg.Len())
	}
	if _, ok := reg.Get("temp"); ok {
		t.Fatal("tool must be gone after unregister")
	}

	// 反注册之后能重新注册同名工具，这就是热替换的路径
	reg.Register(fakeTool{name: "temp", output: "v2"})
	if got, _ := reg.Get("temp"); got == nil {
		t.Fatal("re-register should succeed after unregister")
	}
}

func TestSpecsAreSortedAndVerbatim(t *testing.T) {
	reg := NewRegistry()
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	reg.Register(fakeTool{name: "b"})
	reg.Register(fakeTool{name: "a", schema: schema})

	specs := reg.Specs()
	if len(specs) != 2 {
		t.Fatalf("want 2 specs, got %d", len(specs))
	}
	if specs[0].Name != "a" || specs[1].Name != "b" {
		t.Fatalf("specs must be name-sorted: %+v", specs)
	}
	if string(specs[0].Parameters) != string(schema) {
		t.Fatalf("schema must pass through verbatim: %s", specs[0].Parameters)
	}
}

func TestRegisterIsConcurrencySafe(t *testing.T) {
	reg := NewRegistry()
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h"}

	done := make(chan struct{})
	for _, n := range names {
		go func() {
			defer func() { done <- struct{}{} }()
			reg.Register(fakeTool{name: n})
		}()
	}
	for range names {
		<-done
	}

	if reg.Len() != len(names) {
		t.Fatalf("Len: got %d, want %d", reg.Len(), len(names))
	}
}
