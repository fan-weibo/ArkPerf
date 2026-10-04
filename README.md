# ArkPerf

方舟智诊——OpenHarmony 应用性能体验分析与优化 Agent

## 它做什么

一个 CLI / TUI / 桌面端三形态的 Agent：用自然语言描述任务，它调用 `mcp-servers/` 里的
5 个测量服务（冷启动、内存、CPU、卡顿、全量体验）在真机或模拟器上采集数据，
再把"指标 → 现象 → 原因 → 优化建议"整理成报告。

性能测量能力全部来自本仓库的 `mcp-servers/`，因此**跑起来需要先把 Python 依赖装好**
（标准库之外只依赖 `mcp` 一个包）。

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
go run ./cmd/arkperf skills       # 技能清单（三层目录 + 来源路径）
go run ./cmd/arkperf rules        # 已保存的审批规则
go run ./cmd/arkperf devices      # 已连接设备
go run ./cmd/arkperf tool grep '{"pattern":"TODO","glob":"*.go"}'   # 直接调单个工具
go test ./...                     # 全量测试（438+129 项）
```

> 注意：`arkperf` **不带参数只会打印帮助**，交互式界面要写 `arkperf tui`。
> 一次性执行**不接续会话历史**：每次都是全新上下文，也不会把这次对话存进侧栏——
> 要多轮与历史回放，用 TUI 或桌面端。
>
> 进入 TUI 后按 **`/help`** 可列出全部命令与按键（命令表与补全菜单同源，不会出现"help 里写了但没有"）。
> 常用：`Enter` 发送 · `Ctrl+J` 换行 · `Esc` 中断 · `PgUp/PgDn` 回看历史 ·
> `/new` 新会话 · `/yes` 切自动批准 · `/skills` 技能清单 · `/rules` 审批规则。

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
cmd/arkperf/            CLI 入口（init / check / tools / skills / rules / mcp / devices / tui …）
internal/kernel/        内核：模型调用循环、工具注册表、会话、配置
internal/app/           装配层：把配置、注册表、MCP 连接与会话组装成各前端共用的 Session
internal/tools/         本地工具：读文件 / 写文件 / 搜索 / 执行命令 / 鸿蒙域
internal/skill/         技能层：发现 / 解析 / 渲染「按需加载的说明书」
internal/mcp/           MCP 客户端：把远端服务投影成本地工具
internal/domain/harmony/ 鸿蒙工具链探测与工程结构解析
internal/frontend/tui/  TUI 前端
desktop/                桌面端外壳（Wails3 + Vue3）
mcp-servers/            5 个 Python 测量服务（本项目的测量能力来源）
skills/                 内置技能：与 mcp-servers/ 平级的随仓库分发数据
docs/                   设计与现状文档
```

## 技能（skill）

技能是**按需加载的说明书**，与工具的分工是：工具给模型一双手（能做什么），
技能给它一份手册（怎么把那双手用对）。

只有技能的名字与描述进系统提示词，正文在模型判定任务相关后自己用 `read_file` 读。
一个技能就是一个含 `SKILL.md` 的目录：

```
skills/cold-start-baseline/
├── SKILL.md            # frontmatter + 正文，正文里可引用同目录的其他文件
└── references/protocol.md
```

```markdown
---
name: cold-start-baseline
description: 建立冷启动耗时基线，并在改动后判定是否显著改善或退步。当用户问启动速度、冷启动优化效果时使用。
---

正文：具体步骤与纪律……
```

- `name`：小写字母 / 数字 / 连字符，≤64 字符
- `description`：≤1024 字符，**同时说清「做什么」与「什么时候用」**——
  它是模型判断「要不要读这份技能」的唯一依据，写得含糊就等于这个技能不存在
- frontmatter 只支持**单行值**；需要多行内容就放进正文

三层目录，**越具体越赢**（同名时高优先级的那份生效）：

| 层 | 位置 | 用途 |
|---|---|---|
| 项目级 | `<工作区>/.arkperf/skills/<名字>/SKILL.md` | 某个工程专有的流程 |
| 用户级 | `<状态根>/skills/<名字>/SKILL.md` | 跨工程复用自己的技能 |
| 内置 | 仓库根的 `skills/` | 随版本分发 |

自己放的技能没生效时，先跑 `arkperf skills`：它会打出三层目录、每个技能的来源路径，
以及被跳过的坏文件与原因（坏技能只跳过、不阻断，但会打一行 stderr）。

## 运行时的三个默认行为

**流式输出。** 模型的字边产边出，不是等整段说完。请求上带 `OnDelta` 就走流式，
不带就走一次性路径——两种路径的返回值完全一致，上游不需要知道走的是哪条。
思考过程（`reasoning_content`）与可见回答**分开回调**：TUI 只把思考字数显示在状态行，
不进转录（思考往往比回答长一个数量级，全留下来会把结论冲得看不见）。

**瞬时错误重试。** 网络层故障、408、429、5xx 按指数退避重试，最多 6 次；
服务端给了 `Retry-After` 就听它的（钳制到 120 秒）。**其他 4xx 与本地超时不重试**——
密钥错、模型名错这类确定性错误，重试一百次还是同样的错，只会让用户多等半分钟。
流已经开始吐字之后中断**也不重试**，否则同一段话会再打一遍。

**审批规则。** 危险工具默认每次询问。「这一类以后都不问」三个入口等价：
TUI 审批卡上按 **A**（Shift+a）、桌面端审批卡上点**「这一类以后都不问」**按钮、
CLI 加 `--yes`（后者是全自动批准，不落规则）。
规则落在 `<状态根>/approved-rules.json`，下次启动继续生效。

粒度是**保守**的：路径只归到目录、命令只归到「程序 + 子命令」，
静态审批的域工具（build / install / launch 等）不参与记忆。

> 规则只**减少询问**，不放宽任何红线：被硬拒的命令压根不进入审批路径
> （`NeedsApproval` 恒为 false），所以规则也放行不了它们。
>
> `arkperf rules` 或 TUI 里的 `/rules` 能看到自己放行了什么。删除目前只能手改文件。

## 换机器 / 换克隆位置

MCP 目录、Python 解释器与技能目录**不写死在代码里**，按以下顺序解析：

| 目标 | 解析顺序 |
|---|---|
| MCP 目录 | `ARKPERF_MCP_DIR` → 可执行文件所在目录及其上两级下的 `mcp-servers` → 当前目录下的 `mcp-servers` |
| Python | `ARKPERF_PYTHON` → 仓库根或 `mcp-servers` 下的 `.venv` → PATH 上的 `python` / `python3` / `py` |
| 内置技能目录 | `ARKPERF_SKILLS_DIR` → 可执行文件所在目录及其上两级下的 `skills` → 当前目录下的 `skills` |

解析结果会在 `arkperf init` 时打印出来；连不上 MCP 时先看那两行，
技能没加载到就 `arkperf skills` 看它找了哪三个目录。
