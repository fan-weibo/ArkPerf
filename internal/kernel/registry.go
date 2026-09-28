package kernel

import (
	"fmt"
	"maps"
	"slices"
	"sync"
)

// ConditionalTool 表示"是否需要审批取决于具体参数"。
//
// `arkperf tools` 这类清单展示只能用空参数去问 NeedsApproval，
// 对路径/命令相关的工具会得到"免审批"这种误导性结论。
// 实现这个接口的工具会被标成"按需"，而不是被误报成只读。
type ConditionalTool interface {
	// ApprovalNote 用一句话说明审批依据（如"按路径分级"）。
	ApprovalNote() string
}

// Registry 是能力注册表：一个 map，重名即 panic。
//
// Register 返回反注册函数而不是无返回值——注册是可逆的。
// 这样插件卸载、工具热替换都不会留下悬空注册项：
//
//	unregister := reg.Register(myTool)
//	defer unregister()
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register 注册一个工具，返回反注册函数。
//
// 非法工具（nil、空名、重名）直接 panic：这些都是启动期就能发现的编码错误，
// 拖到运行期只会变成模型拿到一个诡异的工具列表。
func (r *Registry) Register(t Tool) func() {
	if t == nil {
		panic("kernel/registry: nil tool")
	}
	name := t.Name()
	if name == "" {
		panic("kernel/registry: empty tool name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[name]; dup {
		panic(fmt.Sprintf("kernel/registry: duplicate tool name %q", name))
	}
	r.tools[name] = t

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.tools, name)
	}
}

// Get 按名取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names 返回按名排序的工具名，输出稳定便于快照与测试。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Sorted(maps.Keys(r.tools))
}

// Specs 把工具投影成模型可用的描述，顺序与 Names 一致。
func (r *Registry) Specs() []ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]ToolSpec, 0, len(r.tools))
	for name := range slices.Values(slices.Sorted(maps.Keys(r.tools))) {
		t := r.tools[name]
		out = append(out, ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return out
}

// Len 返回已注册工具数量。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}
