# ArkPerf

方舟智诊——OpenHarmony 应用性能体验分析与优化 Agent

## 它做什么

一个 **TUI 与桌面端两个形态**的 Agent：用自然语言描述任务，它调用 `mcp-servers/` 里的
5 个服务——**四类指标的采集器**（冷启动耗时 / 内存占用 / CPU 占用 / 主线程卡顿）各一个，
外加一个把它们一次跑完并落报告的**聚合服务**——在真机或模拟器上采集数据，
再把"指标 → 现象 → 原因 → 优化建议"整理成报告。

性能测量能力全部来自本仓库的 `mcp-servers/`（标准库之外只依赖 `mcp` 一个包）。
Release 包里已带便携 Python，解压即用；从源码运行才需要自己装。

## 快速开始

**只需要 Windows 10 / 11（64 位）**。要连真机或模拟器采集数据才需要 DevEco Studio，
只看界面、跑工具不需要。

1. 从 [Releases](https://github.com/fan-weibo/ArkPerf/releases) 下载
   `ArkPerf-<版本>-windows-x64.zip`，**完整解压**到任意目录。

2. 生成配置(在项目根目录终端运行命令)：

   ```bash
   arkperf.exe init     # 首次运行，生成配置（配置文件在 %USERPROFILE%\.arkperf\config.json）
   ```

3. 填模型信息（编辑上面那个 config.json 的 provider 段）：

   ```bash
   baseUrl OpenAI 兼容端点，例如 https://api.deepseek.com/v1
   apiKey 你的密钥
   model 模型名，例如 deepseek-chat
   ```

4. 自检：

   ```bash
   arkperf.exe check     # 工具链：hdc / hvigorw / ohpm / node / java
   arkperf.exe mcp       # 应输出 5/5 服务器已连接 · 13 个工具
   ```

   `arkperf mcp` 那一步是分界线：**5/5 才算装好了**。某一个连不上不影响其余几个，
   报错会说明原因。

5. 使用：

   ```bash
   未将项目路径加入 PATH 时:
      "包路径"/arkperf.exe tui

   已将项目路径加入 PATH 时:
      arkperf tui
   ```

   建议把 `arkperf.exe` 所在目录加入 PATH 环境变量，便于使用。

## 从源码构建与运行

| 依赖                  | 用途                            | 参考版本                             |
| --------------------- | ------------------------------- | ------------------------------------ |
| Go                    | 构建内核与命令行入口            | **1.26.5+**（`go.mod` 里写的就是它） |
| **Python + `mcp` 包** | **跑测量服务（必需）**          | 3.13                                 |
| DevEco Studio         | 提供 `hdc` / `hvigorw` / `ohpm` | 6.1                                  |
| Node + pnpm           | 仅构建桌面端时需要              | 22 / pnpm 12                         |
| `wails3` CLI          | 仅构建桌面端时需要              | 3.0.0-beta.25                        |

### Go 环境配置

```bash
winget install GoLang.Go     # Windows
brew install go              # macOS
# 其他平台：https://go.dev/dl/
```

装完 `go version` 应 ≥ **1.26.5**。

### 步骤

```bash
# 1) 装测量服务的 Python 依赖（在仓库根建虚拟环境，之后会被自动识别）
python -m venv .venv
.venv\Scripts\pip install mcp

# 2) 生成配置（写 ~/.arkperf/config.json）
go run ./cmd/arkperf init

# 3) 填模型 API Key
#    编辑 ~/.arkperf/config.json 的 provider.baseUrl / apiKey / model

# 4) 自检
go run ./cmd/arkperf check        # 工具链：hdc / hvigorw / ohpm / node / java
go run ./cmd/arkperf mcp          # 应输出：5/5 服务器已连接 · 13 个工具

# 5) 使用
go run ./cmd/arkperf tui          # TUI；进入后 /help 看全部命令与按键

# 其余是自检/查看类子命令
go run ./cmd/arkperf tools        # 工具清单（含审批标注）
go run ./cmd/arkperf skills       # 技能清单（三层目录 + 来源路径）
go run ./cmd/arkperf rules        # 已保存的审批规则
go run ./cmd/arkperf devices      # 已连接设备
go test ./...                     # 全量测试（497 顶层 + 143 子测试）
```

> 注意：`arkperf` **不带参数只会打印帮助**，交互式界面要写 `arkperf tui`。

## 桌面端（暂不推荐，需要者自行构建完善）

```bash
# 还没装 wails3 CLI 时（版本要与 go.mod 里的 wails 一致）
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.25

cd desktop
wails3 task build          # → bin/ArkPerf.exe（原生窗口）
wails3 task build:server   # → bin/ArkPerf-server.exe（浏览器访问 127.0.0.1:9090）
```

三栏界面（功能选择栏 / 聊天 / 工作区文件）跟随 Reasonix 的桌面端设计语言；
TUI 现在是**同一取向**（会话 / 对话 / 文件树）。两者共用内核、技能与 MCP 装配，
差别只在渲染层（Wails + Vue3 对 Bubble Tea）。
首次 `wails3 task build` 会自动装前端依赖并生成绑定，需要能访问 npm 源。

## 目录结构

```
cmd/arkperf/            CLI 入口（init / check / tools / skills / rules / mcp / devices / tui …）
internal/kernel/        内核：模型调用循环、工具注册表、会话、配置
internal/app/           装配层：把配置、注册表、MCP 连接与会话组装成各前端共用的 Session
internal/tools/         本地工具：读文件 / 写文件 / 搜索 / 执行命令 / 鸿蒙域
internal/skill/         技能层：发现 / 解析 / 渲染「按需加载的说明书」
internal/mcp/           MCP 客户端：把远端服务投影成本地工具
internal/domain/harmony/ 鸿蒙工具链探测与工程结构解析
internal/frontend/tui/  TUI 前端
desktop/                桌面端外壳（Wails3 + Vue3）
mcp-servers/            5 个 Python 服务：四类指标采集 + 一个聚合出报告
skills/                 内置技能：与 mcp-servers/ 平级的随仓库分发数据
```
