# ArkPerf

方舟智诊——OpenHarmony 应用性能体验分析与优化 Agent

## 它做什么

一个 CLI / TUI / 桌面端三形态的 Agent：用自然语言描述任务，它调用 `mcp-servers/` 里的
5 个测量服务（冷启动、内存、CPU、卡顿、全量体验）在真机或模拟器上采集数据，
再把"指标 → 现象 → 原因 → 优化建议"整理成报告。

性能测量能力全部来自本仓库的 `mcp-servers/`，因此**跑起来需要先把 Python 依赖装好**。

## 环境要求

| 依赖 | 用途 | 参考版本 |
|---|---|---|
| Go | 构建 CLI 与内核 | **1.26.5+**（`go.mod` 里写的就是它） |
| **Python + `mcp` 包** | **跑测量服务（必需）** | 3.13 |
| DevEco Studio | 提供 `hdc` / `hvigorw` / `ohpm` | 6.1 |
| Node + pnpm | 仅构建桌面端时需要 | 22 / pnpm 12 |
| `wails3` CLI | 仅构建桌面端时需要 | 3.0.0-beta.25 |

### 没装 Go？先装它

```bash
winget install GoLang.Go     # Windows
brew install go              # macOS
# 其他平台：https://go.dev/dl/
```

装完 `go version` 应 ≥ **1.26.5**。1.27 也能用（已核对：本项目没用 1.27 里会变行为的那几处 API）。

### 国内网络：建议先配代理 / 镜像

不是必须，但校园网、公司网下能省掉一堆超时。**这两个都只影响"第一次拉依赖"**：

```bash
# Go 模块代理：第一次 go build 要拉几十个模块（Wails、bubbletea 等），
# 默认走 proxy.golang.org，国内经常卡住或超时
go env -w GOPROXY=https://goproxy.cn,direct

# pip 镜像：装 mcp 包时用（写法见下面第 1 步的注释）
```

核对是否生效：`go env GOPROXY`、`pip config list`。

## 快速开始

```bash
# 1) 装测量服务的 Python 依赖（在仓库根建虚拟环境，之后会被自动识别）
python -m venv .venv
.venv\Scripts\pip install mcp            # macOS / Linux: .venv/bin/pip install mcp
#    校园网 / 公司网络下访问 pypi.org 慢或连不上时，改用镜像（一次性指定即可）：
#      .venv\Scripts\pip install -i https://pypi.tuna.tsinghua.edu.cn/simple mcp

# 2) 生成配置（写 ~/.arkperf/config.json）
go run ./cmd/arkperf init
#    它会打印解析到的 MCP 目录与 Python 解释器，并对缺失项给出提示。
#    没找对时用环境变量指定：
#      Windows:      set ARKPERF_PYTHON=<python.exe 绝对路径>
#      macOS/Linux:  export ARKPERF_PYTHON=<python 绝对路径>
#    MCP 目录被挪走时同理：ARKPERF_MCP_DIR

# 3) 填模型 API Key
#    编辑 ~/.arkperf/config.json 的 provider.baseUrl / apiKey / model

# 4) 自检
go run ./cmd/arkperf check        # 工具链：hdc / hvigorw / ohpm / node / java
go run ./cmd/arkperf mcp          # 应输出：5/5 服务器已连接 · 13 个工具

# 5) 用起来
go run ./cmd/arkperf tui          # TUI（交互式；键位与命令见表下）
go run ./cmd/arkperf "分析 com.example.app 的冷启动耗时"   # 一次性执行（不进入交互界面）
go run ./cmd/arkperf tools        # 工具清单（含审批标注）
go run ./cmd/arkperf devices      # 已连接设备
```

> 注意：`arkperf` **不带参数只会打印帮助**，交互式界面要写 `arkperf tui`。
> 进入 TUI 后按 **`/help`** 可列出全部命令与按键（命令表与补全菜单同源，不会出现"help 里写了但没有"）。
> 常用：`Enter` 发送 · `Ctrl+J` 换行 · `Esc` 中断 · `PgUp/PgDn` 回看历史 · `/new` 新会话 · `/yes` 切自动批准。

`arkperf mcp` 那一步是分界线：**5/5 才算装好了**。某一个连不上不影响其余几个，
报错里会给出具体是哪个服务、什么原因（常见原因：python 里没装 `mcp` 包）。

## 桌面端（可选）

```bash
# 还没装 wails3 CLI 时（版本要与 go.mod 里的 wails 一致）
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.25

cd desktop
wails3 task build          # → bin/ArkPerf.exe（原生窗口）
wails3 task build:server   # → bin/ArkPerf-server.exe（浏览器访问 127.0.0.1:9090）
```

三栏界面（功能选择栏 / 聊天 / 工作区文件）跟随 Reasonix 的桌面端设计语言；
首次 `wails3 task build` 会自动装前端依赖并生成绑定，需要能访问 npm 源。

## 目录结构

```
cmd/arkperf/            CLI 入口（init / check / tools / mcp / devices / tui …）
internal/kernel/        内核：模型调用循环、工具注册表、会话、配置
internal/tools/         本地工具：读文件 / 写文件 / 执行命令 / 鸿蒙域
internal/mcp/           MCP 客户端：把远端服务投影成本地工具
internal/domain/harmony/ 鸿蒙工具链探测与工程结构解析
internal/frontend/tui/  TUI 前端
desktop/                桌面端外壳（Wails3 + Vue3）
mcp-servers/            5 个 Python 测量服务（本项目的测量能力来源）
```

## 换机器 / 换克隆位置

MCP 目录与 Python 解释器**不写死在代码里**，按以下顺序解析：

| 目标 | 解析顺序 |
|---|---|
| MCP 目录 | `ARKPERF_MCP_DIR` → 可执行文件所在目录及其上两级下的 `mcp-servers` → 当前目录下的 `mcp-servers` |
| Python | `ARKPERF_PYTHON` → 仓库根或 `mcp-servers` 下的 `.venv` → PATH 上的 `python` / `python3` / `py` |

解析结果会在 `arkperf init` 时打印出来；连不上 MCP 时先看那两行。
