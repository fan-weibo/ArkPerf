# ArkPerf 前端（Wails3 + Vue 3 + TypeScript）

桌面端的**渲染层**，由 Wails 嵌入 Go 二进制。设计 token 在 `src/theme.css`，
界面在 `src/App.vue` 与 `src/components/`。项目总览看仓库根目录的 README——
这里只讲这个目录怎么构建与调试。

## 构建：走 wails3，不要单独 pnpm build

```bash
cd ..                      # 回到 desktop/
wails3 task build          # → bin/arkperf-desktop.exe（原生窗口）
wails3 task build:server   # → bin/arkperf-desktop-server.exe（浏览器访问 127.0.0.1:9090）
```

`wails3 task build` 会替你做三件单独做不到的事：

1. `pnpm install` —— 装前端依赖
2. `wails3 generate bindings` —— 把 Go 服务导出成 `bindings/` 下的 TS 类型与调用
3. `go build` —— 把 `dist/` 嵌进二进制

**只跑 `pnpm build` 只会得到一份没有后端绑定的 `dist/`。**
`bindings/` 与 `dist/` 都是生成物，已在 `.gitignore` 里排除。

## 开发模式

```bash
wails3 task dev            # = wails3 dev：开窗口 + Vite 热更新，前端改动即时生效
```

只想调样式、不碰后端时也可以 `pnpm dev`（只起 Vite），
但那样调不到 Go 绑定与真实事件，页面上会出现错误提示。
