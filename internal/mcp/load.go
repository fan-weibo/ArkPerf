package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/fan-weibo/ArkPerf/internal/kernel"
)

// ServerSpec 是配置里的一条服务器定义。
//
// 独立于 kernel.MCPServerConfig：配置文件的形状（JSON 标签、三态布尔）
// 不该渗进这个包，映射放在 FromConfig 一处完成。
type ServerSpec struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
	Trusted bool
	Timeout time.Duration
}

// Result 是单个服务器的连接结果。
type Result struct {
	Name  string
	Tools int
	Err   error
}

// OK 表示该服务器连接成功。
func (r Result) OK() bool { return r.Err == nil }

// Set 是一次装配出来的全部 MCP 连接。
type Set struct {
	conns   []*Conn
	Results []Result
}

// ToolCount 返回注册成功的远端工具总数。
func (s *Set) ToolCount() int {
	if s == nil {
		return 0
	}
	n := 0
	for _, r := range s.Results {
		n += r.Tools
	}
	return n
}

// Connected 返回连接成功的服务器数量。
func (s *Set) Connected() int {
	if s == nil {
		return 0
	}
	n := 0
	for _, r := range s.Results {
		if r.OK() {
			n++
		}
	}
	return n
}

// Failed 返回失败服务器的错误摘要，供 CLI 展示。
func (s *Set) Failed() []Result {
	if s == nil {
		return nil
	}
	var out []Result
	for _, r := range s.Results {
		if !r.OK() {
			out = append(out, r)
		}
	}
	return out
}

// Close 回收所有连接（反注册工具 + 结束子进程）。
func (s *Set) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	for _, c := range s.conns {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	s.conns = nil
	return errors.Join(errs...)
}

// LoadAll 并发连接所有服务器，并把各自的工具注册进 reg。
//
// 刻意"部分成功也返回"：测量能力应当是部分可用的——CPU 服务起不来，
// 不该让内存和卡顿也一起用不了。每个服务器的成败都留在 Results 里
// 如实上报，由调用方决定怎么展示，而不是在这里吞掉。
func LoadAll(ctx context.Context, reg *kernel.Registry, specs []ServerSpec) *Set {
	set := &Set{Results: make([]Result, len(specs))}

	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for i, spec := range specs {
		wg.Go(func() {
			conn, err := Connect(ctx, spec.Name, ServerConfig{
				Command: spec.Command,
				Args:    spec.Args,
				Env:     spec.Env,
				Trusted: spec.Trusted,
				Timeout: spec.Timeout,
			})
			if err != nil {
				set.Results[i] = Result{Name: spec.Name, Err: err}
				return
			}

			n, err := conn.RegisterInto(ctx, reg)
			if err != nil {
				_ = conn.Close()
				set.Results[i] = Result{Name: spec.Name, Err: err}
				return
			}

			mu.Lock()
			set.conns = append(set.conns, conn)
			mu.Unlock()

			set.Results[i] = Result{Name: spec.Name, Tools: n}
		})
	}
	wg.Wait()
	return set
}

// FromConfig 把配置映射成规格列表：跳过停用的服务器，按名排序保证装配顺序稳定。
func FromConfig(cfg *kernel.Config) []ServerSpec {
	specs := make([]ServerSpec, 0, len(cfg.MCPServers))
	for name, srv := range cfg.MCPServers {
		if !srv.IsEnabled() {
			continue
		}
		var timeout time.Duration
		if srv.TimeoutSeconds > 0 {
			timeout = time.Duration(srv.TimeoutSeconds) * time.Second
		}
		specs = append(specs, ServerSpec{
			Name:    name,
			Command: srv.Command,
			Args:    srv.Args,
			Env:     srv.Env,
			Trusted: srv.Trusted,
			Timeout: timeout,
		})
	}
	slices.SortFunc(specs, func(a, b ServerSpec) int { return cmp.Compare(a.Name, b.Name) })
	return specs
}

// Summary 生成一行装配摘要。
func (s *Set) Summary() string {
	if s == nil || len(s.Results) == 0 {
		return "未配置 MCP 服务器"
	}
	failed := s.Failed()
	out := fmt.Sprintf("%d/%d 服务器已连接 · %d 个工具", s.Connected(), len(s.Results), s.ToolCount())
	if len(failed) > 0 {
		out += fmt.Sprintf(" · %d 个失败", len(failed))
	}
	return out
}
