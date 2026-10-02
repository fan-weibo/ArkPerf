// Command ArkPerf —— ArkPerf 的桌面 / Web 外壳（Wails 3）。
//
// 这一层只做三件事：建窗口、注册服务、把前端资源交给 Wails。
// 业务全在 internal/app（前端无关的装配层）与 internal/kernel 里，
// 桌面端**不复制一份**——这是"加第二个前端"不产生分叉的前提。
//
// 同一份 main 还能编出 Web 版：
//
//	wails3 task build:server    # 纯 HTTP 服务，浏览器访问，无窗口、无 GUI 依赖
//	wails3 task run:server
//
// 构建前必须先产出 frontend/dist —— 走 Taskfile 的 build:frontend，
// 不要手工 go build（那样 dist 可能是旧的，甚至不存在）。
package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend/dist
var assets embed.FS

// 注册事件载荷类型，供绑定生成器产出**带类型**的前端事件 API。
// 不注册的话前端只能拿到 any，那几个 payload 的结构就白定义了。
func init() {
	application.RegisterEvent[string](evAssistant)
	application.RegisterEvent[ToolCallInfo](evToolCall)
	application.RegisterEvent[ToolResultInfo](evToolResult)
	application.RegisterEvent[ApprovalInfo](evApprovalRequest)
	application.RegisterEvent[ApprovalResolved](evApprovalResolved)
	application.RegisterEvent[DoneInfo](evDone)
}

func main() {
	svc := NewService()

	app := application.New(application.Options{
		Name:        "ArkPerf",
		Description: "方舟智诊：OpenHarmony 应用性能体验分析与优化 Agent",
		Services: []application.Service{
			application.NewService(svc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		// 只在 -tags server 构建里生效；桌面构建忽略。
		//
		// 端口用 9090 而不是文档默认的 8080：本机 8080 绑定会失败
		// （WSAEACCES "以一种访问权限不允许的方式做了访问套接字的尝试"，
		// 而实测 9090 可绑定）。系统保留区间里并没有 8080，所以原因另在
		// （可能是 HTTP.sys 预留或别的安全策略）——换端口比深挖它划算。
		Server: application.ServerOptions{
			Host: "127.0.0.1",
			Port: 9090,
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})
	svc.setApp(app)

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "ArkPerf · 方舟智诊",
		Width:            1180,
		Height:           760,
		BackgroundColour: application.NewRGB(20, 22, 28),
		URL:              "/",
	})

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
