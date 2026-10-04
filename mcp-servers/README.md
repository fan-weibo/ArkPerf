# OpenHarmony 应用体验分析工具集

对指定 OpenHarmony 应用采集基础体验指标、识别典型体验问题、输出结构化诊断报告。

## 一、目录结构（三层）

每个指标一个模块，三层分离。**给 Agent 调用的产品入口是 `*_server.py`**；
`*_test.py` 是开发阶段测试程序，供改完 core 后手工跑一遍、直接看终端输出。

| 层 | 文件 | 职责 |
|---|---|---|
| 公共后端逻辑 | `*_core.py` | 纯逻辑，只依赖标准库，返回 dict |
| MCP 薄封装 | `*_server.py` | 参数透传 + 中文标签，供 Agent 调用（产品入口） |
| 开发阶段测试程序 | `*_test.py` | 命令行直跑，绕过 MCP 协议，开发时手工验证用 |

| 模块 | 指标 |
|---|---|
| `cold_start_*` | 冷启动耗时 |
| `memory_*` | 内存占用变化 / 泄漏 |
| `cpu_*` | CPU 占用变化 |
| `jank_*` | 主线程卡顿 / 阻塞 |
| `agent_*` | **统一编排 + 规则引擎 + 报告生成** |

## 二、快速开始

前置：`hdc` 在 PATH、设备/模拟器在线、目标应用已安装。

```bash
cd E:\ArkPerf\mcp-servers        # 本仓库的位置（按实际克隆路径调整）

# 一条命令跑完整分析（约 2 分钟，快速模式）
python agent_test.py <包名> --quick

# 标准模式（约 6 分钟，采样更多、结论更稳）
python agent_test.py <包名> --tag baseline

# 只跑部分指标
python agent_test.py <包名> --modules cold_start,memory

# 指定 Ability / 输出目录
python agent_test.py <包名> --ability EntryAbility --out ./reports
```

输出：终端结论 + `experience-report-<时间>-<tag>.md` / `.json`。

单独使用某个指标：

```bash
python cold_start_test.py measure -o base1.json   # 改动前存基线
python cold_start_test.py compare base1.json      # 改动后对比
python memory_test.py <包名> 5 1.5                # 内存泄漏检测
python cpu_test.py                                # CPU 快照 + 趋势
python jank_test.py <包名> 5                      # 主线程卡顿判定
```

## 三、指标与可识别问题

| 指标 | 判定口径 | 对应问题 |
|---|---|---|
| 冷启动耗时 | 中位数 + P90 + 离散度；校验 `StartMode==Cold` | 启动耗时过长 |
| 内存占用变化 | **强制 GC → 锯齿 / 阶梯**（官方口径） | 内存持续增长 / 泄漏 |
| CPU 占用变化 | 空闲占用中位数（阈值 5 / 30 / 70 %） | CPU 占用过高 |
| 主线程卡顿 | 系统卡顿判定计数（`jank >= threshold`） | 主线程阻塞 |

报告固定包含五部分：测试场景、关键指标、异常现象、可能原因、优化建议。

## 四、MCP 接入

**依赖**：`mcp` SDK，装在虚拟环境里即可（见仓库根 README 的快速开始；
`arkperf init` 会自动找仓库内或本目录下的 `.venv`，也可以用环境变量
`ARKPERF_PYTHON` 直接指定解释器）。

**配置**：由 `arkperf init` 写入 `~/.arkperf/config.json`，注册 5 个服务：

| 服务 | 工具 |
|---|---|
| `harmony-experience-agent` | `experience_analysis`、`save_experience_report`、`list_indicators` |
| `harmony-cold-start` | `cold_start_measure`、`cold_start_compare` |
| `harmony-memory` | `memory_snapshot`、`memory_leak_check`、`memory_trend` |
| `harmony-cpu` | `cpu_usage`、`cpu_trend`、`cpu_assess` |
| `harmony-jank` | `blocking_check`、`blocking_evidence` |

若客户端未加载项目级配置（部分版本的 MCP 可视化编辑器只写用户级），
把同一份 `mcpServers` 合并进该客户端的用户级 `mcp.json` 即可。

## 五、已知限制（重要）

1. **帧率 / 掉帧不可用**：`SP_daemon -f`、`-ohtestfps`、`-editor fpsohtest`
   在当前模拟器镜像上**恒返回 `fps=0`**（截图对比已证明滑动确实在渲染），
   属镜像能力缺失，故未纳入指标集。真机或完整镜像可再启用。
2. **冷启动是绝对时间量**：受主机负载与模拟器状态影响，同代码重复测 P50 可摆动约 10~16%。
   模块内置锚点归一化与显著性检验，**只报「差异 + p 值 + 最小可检测差异」**，不报单次绝对值；
   低于约 20% 的差异在当前环境下无法可靠识别（可提高采样数改善）。
3. **主线程卡顿信号偏弱**：`JankFrameMonitor` 判定在行为正常的应用上仅 0~4 次，
   结论存在波动风险；系统另有的 `RSJankStats jank frames` 由渲染服务（非应用 PID）输出，
   当前采集口径会漏掉，待改进。
4. **内存检测需 debug 签名应用**才能强制 GC；非 debug 会自动降级并告警，结论偏保守。
5. 所有判定阈值为经验参考值，官方未公开统一阈值，集中在各 `*_core.py` 顶部便于调整。
