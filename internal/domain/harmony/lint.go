package harmony

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/fan-weibo/ArkPerf/internal/execx"
)

// lint 超时给到 5 分钟：codelinter 要先把整个工程解析一遍，
// 大工程慢是正常的；超时给小只会得到"检查失败"的假象。
const timeoutLint = 5 * time.Minute

// codelinterRel 是 DevEco 自带的官方检查工具入口。
//
// 它是 Node 写的，不是可执行程序——必须用 node 启动 index.js，
// 直接 exec 这个 .js 会失败（Windows 上会被当成文档打开）。
const codelinterRel = "plugins/codelinter/run/index.js"

// Lint 用官方 codelinter 检查工程。
//
// 三条定位路径按可靠性排序：环境变量 → PATH → DevEco 插件目录。
// 找不到时**返回错误并给出安装提示**，不假装"检查通过"——
// 一个永远说"没问题"的检查比没有检查更糟。
func Lint(ctx context.Context, tc *Toolchain, projectDir string) (execx.Result, error) {
	exe, args, err := codelinterCommand(tc)
	if err != nil {
		return execx.Result{}, err
	}
	dir, err := filepath.Abs(projectDir)
	if err != nil {
		return execx.Result{}, err
	}
	args = append(args, dir)
	res, err := execx.Run(ctx, exe, args, execx.Options{
		Timeout:   timeoutLint,
		MaxOutput: 16 << 20, // 16MB：大工程的问题列表可能很长
	})
	if err != nil && res.Output == "" {
		return res, fmt.Errorf("codelinter 执行失败：%w", err)
	}
	return res, err
}

// codelinterCommand 定位 codelinter 并给出命令与前置参数。
func codelinterCommand(tc *Toolchain) (exe string, prefix []string, err error) {
	// 1. PATH
	if p, lerr := exec.LookPath("codelinter"); lerr == nil && p != "" {
		return p, nil, nil
	}
	if p, lerr := exec.LookPath("codelinter.cmd"); lerr == nil && p != "" {
		return p, nil, nil
	}
	// 2. DevEco 插件目录里的 Node 入口
	if tc != nil && tc.DevEcoRoot != "" {
		index := filepath.Join(tc.DevEcoRoot, filepath.FromSlash(codelinterRel))
		if existsNoErr(index) {
			node := tc.Required(ToolNode)
			if !node.Found() {
				return "", nil, errors.New("找到 codelinter 但没找到 node（它是 Node 程序，需要 node 启动）")
			}
			return node.Path, []string{index}, nil
		}
	}
	return "", nil, errors.New(
		"没找到 codelinter：PATH 上没有，DevEco 的 plugins/codelinter/run/index.js 也不存在。" +
			"（可在 DevEco 里安装 Code Linter 插件，或用 harmony_toolchain_check 确认 DevEco 根目录是否正确）")
}
