"""统一体验分析 Agent —— 编排 + 规则引擎 + 结构化报告（纯逻辑层）。

对应赛题要求：
  1. 采集 ≥3 类基础体验指标：冷启动耗时 / 内存占用变化 / CPU 占用变化 / 主线程卡顿
  2. 识别 ≥3 类典型体验问题：启动耗时过长 / 内存持续增长 / CPU 占用过高 / 主线程阻塞
  3. 完整 Agent 工作流：输入包名（或工程路径）→ 执行采集 → 归并分析 → 输出诊断
  4. 规则引擎生成结构化报告：测试场景 / 关键指标 / 异常现象 / 可能原因 / 优化建议

设计约定（与其它模块一致）：
  - 只依赖标准库 + 同目录的 *_core 模块，不 import fastmcp；
  - 入参简单、返回 dict，便于最后套 *_server.py 封装成 MCP；
  - 判定阈值集中在各 *_core 里，本层只做编排与归并。
"""

import datetime
import json
import os
import re
import subprocess
import time

import cold_start_core as cs
import cpu_core as cpu
import jank_core as jank
import memory_core as mem

# ============================================================
# 指标模块
# ============================================================
MODULES = ("cold_start", "memory", "cpu", "jank")

MODULE_LABELS = {
    "cold_start": "冷启动耗时",
    "memory": "内存占用变化",
    "cpu": "CPU 占用变化",
    "jank": "主线程卡顿/阻塞",
}

# 冷启动环境归一化用的锚点应用（系统设置，与被测应用无关）
DEFAULT_ANCHOR = (cs.ANCHOR_BUNDLE, cs.ANCHOR_ABILITY)

SEVERITY_TAGS = {"none": "✅", "mild": "🟡", "moderate": "🟠", "heavy": "🔴"}
SEVERITY_WEIGHT = {"none": 0, "mild": 1, "moderate": 2, "heavy": 3,
                   "unknown": 0}

# ============================================================
# 规则引擎：问题模板（异常现象 → 可能原因 → 优化建议）
# ============================================================
ISSUE_RULES = {
    "slow_cold_start": {
        "title": "启动耗时过长",
        "causes": [
            "Ability.onCreate / onWindowStageCreate 中执行了同步耗时操作"
            "（数据库、首选项、网络请求、第三方 SDK 初始化）",
            "首页组件树层级过深，首帧需要构建的节点过多",
            "启动阶段加载了非首屏必需的资源或页面",
            "主线程被同步 IO 阻塞，未能与渲染并行",
        ],
        "suggestions": [
            "把非首屏必需的初始化改为异步或延迟到首帧之后执行（TaskPool / setTimeout）",
            "首页按需渲染、减少布局层级；图片改懒加载并压缩尺寸",
            "预加载/缓存热点数据，避免启动时同步读盘",
            "用 hiTraceMeter 打点定位耗时最长的阶段，优先治理占比最大的那一段",
        ],
    },
    "memory_growth": {
        "title": "内存持续增长 / 疑似泄漏",
        "causes": [
            "全局集合或缓存只增不减（Map / Array 累积）",
            "事件监听、订阅、回调注册后未在 aboutToDisappear 中注销",
            "定时器 / 异步任务持有页面或组件引用，页面销毁后无法释放",
            "Native 层或渲染缓存未释放（表现为 PSS / Native 堆增长而 ArkTS 堆平稳）",
        ],
        "suggestions": [
            "为缓存设置上限与淘汰策略（LRU），避免无界增长",
            "在 aboutToDisappear 中成对调用 off / unregister，确保监听解绑",
            "定时器与长任务在页面销毁时主动取消",
            "用 hidumper --mem-jsheap <pid> --leakobj / 堆快照对比定位具体泄漏对象",
        ],
    },
    "memory_capped": {
        "title": "内存有界增长（缓存填充特征）",
        "causes": [
            "存在带上限的缓存 / 对象池，使用初期持续填充直至上限后饱和",
            "页面或模块加载后保留了一定规模的常驻数据（图片缓存、数据列表）",
        ],
        "suggestions": [
            "确认缓存上限符合设计预期；上限本身过大时下调",
            "若填充段集中在冷启动后，可将缓存预热移出启动路径",
            "持续观察：后段已饱和则无需处理；若饱和平台继续抬升，按泄漏重新评估",
        ],
    },
    "cpu_high": {
        "title": "CPU 占用过高",
        "causes": [
            "存在常驻轮询或定时任务，间隔过密",
            "后台线程未收敛，空闲时仍在计算",
            "高频状态刷新触发无关组件重复渲染",
        ],
        "suggestions": [
            "降低轮询频率或改为事件驱动，避免固定间隔空转",
            "空闲时挂起非必要线程与定时器",
            "收敛状态更新粒度，减少无谓的 UI 重渲染",
        ],
    },
    "main_thread_block": {
        "title": "主线程卡顿 / 阻塞",
        "causes": [
            "主线程上执行了耗时同步任务（复杂计算、大文件读写、网络等待）",
            "单帧渲染工作量过大（一次创建大量组件、超大图片解码）",
            "频繁深拷贝 / 序列化阻塞主线程",
        ],
        "suggestions": [
            "把耗时计算搬离主线程（TaskPool / Worker）",
            "拆分长任务，避免单帧内堆积过多渲染工作",
            "图片按显示尺寸解码，避免整图加载",
            "用 hitrace（app / ace 标签）定位主线程上的长耗时切片",
        ],
    },
}


# ============================================================
# 工具
# ============================================================
def _now_iso() -> str:
    return datetime.datetime.now().strftime("%Y-%m-%d %H:%M:%S")


def device_online() -> bool:
    """hdc 是否有在线设备。"""
    try:
        out = subprocess.run(["hdc", "list", "targets"],
                             capture_output=True, text=True,
                             timeout=15).stdout
    except Exception:
        return False
    for line in out.splitlines():
        s = line.strip()
        if s and not s.startswith("[") and "Empty" not in s:
            return True
    return False


def guess_ability(bundle_name: str) -> str:
    """尽力从 bundle 信息里猜主 Ability 名；失败返回空串。"""
    try:
        raw = cs._shell(["bm", "dump", "-n", bundle_name], timeout=30).stdout
    except Exception:
        return ""
    names = re.findall(r'"name"\s*:\s*"([A-Za-z0-9_.]*Ability[A-Za-z0-9_]*)"', raw)
    for n in names:
        if "Entry" in n:
            return n
    return names[0] if names else ""


def ensure_running(bundle_name: str, ability_name: str = "",
                   on_progress=None, scene: str = "") -> str:
    """确保应用在运行，返回 pid（空串表示失败）。

    scene 非空时：force-stop 后带 want.parameters.scene 重启，
    直达应用内对应场景页面（如 PerfLab 的 leak/block 实验页）。
    """
    if scene:
        if on_progress:
            on_progress(f"以 scene={scene} 重启 {bundle_name} ...")
        try:
            cs._shell(["aa", "force-stop", bundle_name], timeout=15)
        except Exception:
            pass
        args = ["aa", "start", "-b", bundle_name]
        if ability_name:
            args += ["-a", ability_name]
        args += ["--ps", "scene", scene]
        try:
            cs._shell(args, timeout=30)
        except Exception:
            return ""
        time.sleep(3)
        return cs.get_pid(bundle_name)

    pid = cs.get_pid(bundle_name)
    if pid:
        return pid
    if on_progress:
        on_progress(f"启动应用 {bundle_name} ...")
    args = ["aa", "start", "-b", bundle_name]
    if ability_name:
        args += ["-a", ability_name]
    try:
        cs._shell(args, timeout=30)
    except Exception:
        return ""
    time.sleep(3)
    return cs.get_pid(bundle_name)


# ============================================================
# 各指标的采集 + 判定（统一成 scenario 结构）
# ============================================================
def _empty(module, status, note=""):
    return {"module": module, "name": MODULE_LABELS[module], "status": status,
            "metrics": {}, "issues": [], "note": note}


def _cold_start_severity(median_ms: float):
    if median_ms < 1000:
        return "none", "✅ 优秀"
    if median_ms < 1500:
        return "mild", "🟡 良好"
    if median_ms < 3000:
        return "moderate", "⚠️ 偏慢"
    return "heavy", "🔴 严重偏慢"


def scenario_cold_start(bundle_name: str, ability_name: str,
                        quick: bool = False, on_progress=None) -> dict:
    runs = 5 if quick else 10
    anchor = DEFAULT_ANCHOR if not quick else None
    # 锚点不能等于被测应用（否则等于空标定）
    if anchor and anchor[0] == bundle_name:
        anchor = None

    r = cs.measure_cold_start(bundle_name, ability_name, runs=runs, warmup=1,
                              anchor=anchor, on_progress=on_progress)
    if "error" in r:
        return _empty("cold_start", "error", r["error"])

    st = r["stats"]
    median = st["median"]
    sev, tag = _cold_start_severity(median)

    metrics = {
        "中位耗时(ms)": median,
        "P10(ms)": st.get("p10"),
        "P90(ms)": st.get("p90"),
        "离散度σ(ms)": st.get("robust_sigma"),
        "极大-极小(ms)": st.get("range"),
        "有效样本数": st.get("n"),
        "非冷启动剔除": r.get("non_cold_count", 0),
        "判定": f"{tag}（{cs.judge_cold_start(int(median))}）",
    }
    if r.get("anchor"):
        metrics["锚点漂移"] = f"{r['anchor'].get('bundle', '')}"

    issues = []
    if sev in ("moderate", "heavy"):
        issues.append({
            "id": "slow_cold_start", "severity": sev,
            "evidence": (f"冷启动中位 {median} ms，P90 {st.get('p90')} ms，"
                         f"极差 {st.get('range')} ms（{st.get('n')} 次有效冷启动）"),
        })

    return {"module": "cold_start", "name": MODULE_LABELS["cold_start"],
            "status": "issue" if issues else "ok",
            "metrics": metrics, "issues": issues, "note": ""}


def scenario_memory(bundle_name: str, quick: bool = False,
                    on_progress=None) -> dict:
    cycles = 3 if quick else 5
    r = mem.get_memory_leak_report(bundle_name, cycles=cycles,
                                   on_progress=on_progress)
    if "error" in r:
        return _empty("memory", "error", r["error"])

    verdict = r.get("verdict", "unknown")
    metrics = {
        "主判据": r.get("primary"),
        "形态": {"sawtooth": "锯齿（正常）", "staircase": "阶梯（异常）",
                "capped": "饱和（有界增长）"}
                .get(r.get("pattern"), r.get("pattern")),
        "结论": r.get("label"),
        "有效周期": f"{r.get('valid_cycles')}/{r.get('cycles')}",
        "强制GC": "已启用" if r.get("gc_supported") else "不支持（结论偏保守）",
        "周期时长(s)": r.get("cycle_period_s"),
    }
    series = r.get("series") or {}
    if series.get("baseline_ark_ts"):
        metrics["ArkTS回落点(kB)"] = series["baseline_ark_ts"]
    for key, name in (("ark_ts", "ArkTS 堆"), ("pss", "PSS"),
                      ("native", "Native 堆")):
        d = r.get(key) or {}
        if d.get("n"):
            metrics[f"{name}判定"] = d.get("label")

    issues = []
    if verdict in ("leak", "watch", "capped"):
        issues.append({
            "id": "memory_growth" if verdict in ("leak", "watch") else "memory_capped",
            "severity": "heavy" if verdict == "leak" else "moderate",
            "evidence": r.get("label", ""),
        })

    note = "；".join(r.get("warnings", []))
    return {"module": "memory", "name": MODULE_LABELS["memory"],
            "status": "issue" if issues else "ok",
            "metrics": metrics, "issues": issues, "note": note}


def scenario_cpu(bundle_name: str, quick: bool = False,
                 on_progress=None) -> dict:
    samples = 2 if quick else 3
    r = cpu.measure_cpu(bundle_name, samples=samples, interval=2,
                        on_progress=on_progress)
    if "error" in r:
        return _empty("cpu", "error", r["error"])

    metrics = {
        "中位占用(%)": r.get("median"),
        "峰值占用(%)": r.get("peak"),
        "最低占用(%)": r.get("min"),
        "样本数": r.get("n"),
        "原始序列": r.get("total_samples"),
        "判定": r.get("label"),
    }
    issues = []
    if r.get("verdict") == "issue":
        issues.append({
            "id": "cpu_high", "severity": r.get("severity", "moderate"),
            "evidence": (f"空闲 CPU 占用中位 {r.get('median')}%、"
                         f"峰值 {r.get('peak')}%"),
        })

    return {"module": "cpu", "name": MODULE_LABELS["cpu"],
            "status": "issue" if issues else "ok",
            "metrics": metrics, "issues": issues, "note": ""}


def scenario_jank(bundle_name: str, quick: bool = False,
                  on_progress=None) -> dict:
    rounds = 3 if quick else 5
    r = jank.get_blocking_report(bundle_name, rounds=rounds,
                                 on_progress=on_progress)
    if "error" in r:
        return _empty("jank", "error", r["error"])

    f = r.get("features", {})
    metrics = {
        "判定": r.get("label"),
        "卡顿判定次数": r.get("count"),
        "卡顿频次(次/分)": r.get("rate_per_min"),
        "jank≥threshold": f.get("threshold_events"),
        "卡顿帧峰值": f.get("peak_jank_frames"),
        "注入操作次数": r.get("input_events"),
        "日志来源": r.get("log_filter"),
    }
    issues = []
    if r.get("verdict") == "blocked":
        issues.append({
            "id": "main_thread_block",
            "severity": r.get("severity", "moderate"),
            "evidence": r.get("label", ""),
        })

    note = "；".join(r.get("warnings", []))
    return {"module": "jank", "name": MODULE_LABELS["jank"],
            "status": "issue" if issues else (
                "unknown" if r.get("verdict") == "unknown" else "ok"),
            "metrics": metrics, "issues": issues, "note": note}


# ============================================================
# 编排
# ============================================================
def run_analysis(bundle_name: str, ability_name: str = "",
                 modules=None, quick: bool = False, scene: str = "",
                 on_progress=None) -> dict:
    """完整 Agent 工作流：前置检查 → 逐模块采集 → 规则引擎归并 → 结构化报告。

    scene：可选的场景直达参数（透传给 want.parameters.scene）。
    应用支持场景路由时（如 PerfLab 的 leak/block），内存/卡顿测量
    会重启应用直达对应页面，测出该场景下的真实表现。
    """
    modules = [m for m in (modules or MODULES) if m in MODULES]
    if not modules:
        return {"error": f"未指定有效模块，可选：{'/'.join(MODULES)}"}

    log = on_progress or (lambda m: None)

    # ---- 前置检查 ----
    log("检查设备与应用 ...")
    if not device_online():
        return {"error": "未检测到在线设备，请确认 hdc 可用且设备已连接"}

    if not ability_name:
        ability_name = guess_ability(bundle_name)
        if ability_name:
            log(f"自动识别 Ability：{ability_name}")
    if not ability_name:
        ability_name = "EntryAbility"
        log(f"未识别到 Ability，回退为 {ability_name}")

    started = _now_iso()
    t0 = time.time()
    device = cs.device_info()

    # ---- 逐模块采集 ----
    # scene 只作用于 memory / cpu / jank（需应用在前台运行并驱动操作）；
    # 冷启动保持默认入口，保证与基线同口径可比。
    runners = {
        "cold_start": lambda: scenario_cold_start(
            bundle_name, ability_name, quick, log),
        "cpu": lambda: scenario_cpu(
            bundle_name, quick,
            (lambda m: log(m)) if ensure_running(bundle_name, ability_name, log, scene)
            else None),
        "memory": lambda: scenario_memory(
            bundle_name, quick,
            (lambda m: log(m)) if ensure_running(bundle_name, ability_name, log, scene)
            else None),
        "jank": lambda: scenario_jank(
            bundle_name, quick,
            (lambda m: log(m)) if ensure_running(bundle_name, ability_name, log, scene)
            else None),
    }

    scenarios = []
    for m in modules:
        log(f"—— 采集「{MODULE_LABELS[m]}」——")
        try:
            scenarios.append(runners[m]())
        except Exception as e:                      # 单模块失败不影响整体
            scenarios.append(_empty(m, "error", f"{type(e).__name__}: {e}"))

    duration_s = round(time.time() - t0, 1)

    # ---- 规则引擎：归并问题 ----
    issues = collect_issues(scenarios)

    health = ("bad" if any(i["severity"] == "heavy" for i in issues)
              else "warning" if issues else "good")

    report = {
        "meta": {
            "bundle_name": bundle_name,
            "ability_name": ability_name,
            "scene": scene or "默认入口",
            "device": device,
            "started_at": started,
            "duration_s": duration_s,
            "modules": modules,
            "mode": "快速" if quick else "标准",
        },
        "scenarios": scenarios,
        "issues": issues,
        "summary": {
            "health": health,
            "issue_count": len(issues),
            "conclusion": _conclusion(health, issues, scenarios),
        },
    }
    report["markdown"] = render_markdown(report)
    return report


def collect_issues(scenarios: list) -> list:
    """规则引擎：把各场景的异常现象归并成问题清单（附原因与建议）。"""
    issues = []
    for sc in scenarios:
        for raw in sc.get("issues", []):
            rule = ISSUE_RULES.get(raw["id"], {})
            issues.append({
                "id": raw["id"],
                "title": rule.get("title", raw["id"]),
                "severity": raw.get("severity", "moderate"),
                "scenario": sc["name"],
                "evidence": raw.get("evidence", ""),
                "possible_causes": list(rule.get("causes", [])),
                "suggestions": list(rule.get("suggestions", [])),
            })
    issues.sort(key=lambda i: -SEVERITY_WEIGHT.get(i["severity"], 0))
    return issues


def _conclusion(health, issues, scenarios) -> str:
    ok = sum(1 for s in scenarios if s["status"] == "ok")
    bad = sum(1 for s in scenarios if s["status"] == "error")
    if health == "good":
        head = f"共采集 {len(scenarios)} 类指标，未发现明显体验问题。"
    else:
        names = "、".join(i["title"] for i in issues)
        head = f"共采集 {len(scenarios)} 类指标，发现 {len(issues)} 个体验问题：{names}。"
    tail = ""
    if ok:
        tail += f" 其中 {ok} 项指标正常。"
    if bad:
        tail += f" {bad} 项指标采集失败，见各场景备注。"
    return head + tail


# ============================================================
# 报告渲染
# ============================================================
def render_markdown(report: dict) -> str:
    meta = report["meta"]
    lines = []
    lines.append("# OpenHarmony 应用体验分析报告")
    lines.append("")
    lines.append("## 一、测试信息")
    lines.append("")
    lines.append(f"- 应用包名：`{meta['bundle_name']}`")
    lines.append(f"- Ability：`{meta['ability_name']}`")
    lines.append(f"- 测试场景：{meta.get('scene') or '默认入口'}")
    dev = meta.get("device") or {}
    dev_desc = " / ".join(str(v) for v in (
        dev.get("model"), dev.get("software"), dev.get("api")) if v)
    lines.append(f"- 设备：{dev_desc or '（未获取）'}")
    lines.append(f"- 采集时间：{meta['started_at']}（耗时 {meta['duration_s']} s，"
                 f"{meta['mode']}模式）")
    lines.append(f"- 采集模块：{'、'.join(MODULE_LABELS[m] for m in meta['modules'])}")
    lines.append("")

    lines.append("## 二、测试场景与关键指标")
    lines.append("")
    lines.append("| 场景 | 状态 | 关键指标 | 说明 |")
    lines.append("|---|---|---|---|")
    status_desc = {"ok": "✅ 正常", "issue": "⚠️ 异常",
                   "error": "❌ 采集失败", "unknown": "❓ 无法判定"}
    for sc in report["scenarios"]:
        kv = "；".join(f"{k}={v}" for k, v in list(sc["metrics"].items())[:4])
        kv = kv.replace("|", "/").replace("\n", " ")
        note = (sc.get("note") or "").replace("|", "/").replace("\n", " ")
        lines.append(f"| {sc['name']} | {status_desc.get(sc['status'], sc['status'])} "
                     f"| {kv or '—'} | {note[:60] or '—'} |")
    lines.append("")

    lines.append("### 指标明细")
    lines.append("")
    for sc in report["scenarios"]:
        lines.append(f"**{sc['name']}**")
        lines.append("")
        if sc["metrics"]:
            for k, v in sc["metrics"].items():
                lines.append(f"- {k}：{v}")
        else:
            lines.append(f"- （无数据）{sc.get('note', '')}")
        if sc.get("note") and sc["metrics"]:
            lines.append(f"- 备注：{sc['note']}")
        lines.append("")

    lines.append("## 三、异常现象、可能原因与优化建议")
    lines.append("")
    if not report["issues"]:
        lines.append("本次采集未发现达到阈值的问题。")
        lines.append("")
    for idx, iss in enumerate(report["issues"], 1):
        tag = SEVERITY_TAGS.get(iss["severity"], "")
        lines.append(f"### 问题 {idx}：{iss['title']}  {tag}")
        lines.append("")
        lines.append(f"- **异常现象**：{iss['evidence']}")
        lines.append(f"- **所属场景**：{iss['scenario']}")
        lines.append("- **可能原因**：")
        for c in iss["possible_causes"]:
            lines.append(f"  {iss['possible_causes'].index(c) + 1}. {c}")
        lines.append("- **优化建议**：")
        for s in iss["suggestions"]:
            lines.append(f"  {iss['suggestions'].index(s) + 1}. {s}")
        lines.append("")

    lines.append("## 四、总体结论")
    lines.append("")
    lines.append(report["summary"]["conclusion"])
    lines.append("")
    lines.append("---")
    lines.append("")
    lines.append("> 说明：绝对耗时类指标受测量环境影响，结论以「多次采样的稳健统计量 + "
                 "判定阈值」为准；同一设备、同一口径下的前后对比才有意义。")
    lines.append("")
    return "\n".join(lines)


def save_report(report: dict, out_dir: str = ".", tag: str = "") -> dict:
    """把报告落盘为 JSON + Markdown，返回文件路径。"""
    stamp = datetime.datetime.now().strftime("%Y%m%d-%H%M%S")
    base = f"experience-report-{stamp}" + (f"-{tag}" if tag else "")
    json_path = os.path.join(out_dir, base + ".json")
    md_path = os.path.join(out_dir, base + ".md")

    with open(json_path, "w", encoding="utf-8") as f:
        payload = {k: v for k, v in report.items() if k != "markdown"}
        json.dump(payload, f, ensure_ascii=False, indent=2)
    with open(md_path, "w", encoding="utf-8") as f:
        f.write(report.get("markdown", ""))

    return {"json": json_path, "markdown": md_path}
