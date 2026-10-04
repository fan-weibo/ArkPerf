"""冷启动测量 —— 纯逻辑层。

设计目标：让「改动前后」的对比结论可信，而不只是给出一个绝对值。
要点：
  1. 每次测量都校验 StartMode==Cold，剔除热启动/温启动等污染样本；
  2. 预热 warmup 次不计入统计（默认 1，剔除系统缓存未预热的首轮）；
  3. 用中位数 + 稳健噪声(1.4826*MAD) 表达结果，并给出离散度；
  4. 提供 compare_sessions()，自动判断两次会话的差异是否超出噪声。

本模块只依赖标准库，保持「纯逻辑 + 返回 dict」的接口契约，
便于后续 _server.py 薄封装。
"""

import json
import math
import os
import re
import statistics
import subprocess
import sys
import time

# ============================================================
# 规则判断：冷启动阈值（保持不变）
# ============================================================
COLD_START_THRESHOLDS = {
    "excellent": 1000,   # < 1000ms 优秀
    "good": 1500,        # < 1500ms 良好
    "slow": 3000,        # < 3000ms 偏慢
                         # >= 3000ms 严重偏慢
}

# 锚点应用（系统设置）：每轮同时测它，用「目标/锚点」比值抵消环境整体漂移。
ANCHOR_BUNDLE = "com.huawei.hmos.settings"
ANCHOR_ABILITY = "com.huawei.hmos.settings.MainAbility"


def judge_cold_start(ms: int) -> str:
    """根据冷启动耗时返回评价标签"""
    if ms < COLD_START_THRESHOLDS["excellent"]:
        return "✅ 优秀"
    if ms < COLD_START_THRESHOLDS["good"]:
        return "🟡 良好"
    if ms < COLD_START_THRESHOLDS["slow"]:
        return "⚠️ 偏慢"
    return "🔴 严重偏慢"


# ============================================================
# 基础 IO
# ============================================================
def _shell(args, timeout=30):
    """执行 hdc shell <args>，返回 CompletedProcess"""
    return subprocess.run(
        ["hdc", "shell", *args],
        capture_output=True, text=True, timeout=timeout,
    )


def device_info() -> dict:
    """采集设备指纹，用于校验两次测量是否在同一条件下进行"""
    info = {}
    for key, param in (
        ("model", "const.product.model"),
        ("software", "const.product.software.version"),
        ("api", "const.ohos.apiversion"),
    ):
        try:
            info[key] = _shell(["param", "get", param], timeout=5).stdout.strip()
        except Exception:
            info[key] = ""
    return info


def device_uptime() -> float:
    """设备已运行秒数（用于判断两次会话之间是否重启过模拟器）。

    注意：非 root 下读取 /proc/uptime 会 Permission denied，
    因此改用 uptime 命令的输出解析。
    """
    try:
        raw = _shell(["uptime"], timeout=5).stdout.strip()
    except Exception:
        return -1.0

    # "up 2 days,  3:21"
    m = re.search(r"up\s+(\d+)\s+days?,\s*(\d+):(\d+)", raw)
    if m:
        return float(int(m.group(1)) * 86400
                     + int(m.group(2)) * 3600
                     + int(m.group(3)) * 60)

    # "up 33 min"
    m = re.search(r"up\s+(\d+)\s+min", raw)
    if m:
        return float(int(m.group(1)) * 60)

    # "up 1:23"
    m = re.search(r"up\s+(\d+):(\d+)", raw)
    if m:
        return float(int(m.group(1)) * 3600 + int(m.group(2)) * 60)

    return -1.0


def host_snapshot() -> dict:
    """主机侧负载快照（CPU 使用率%、内存使用率%）。

    无第三方依赖：Windows 走 ctypes 调 Win32；其他平台退化为 loadavg。
    模拟器跑在本机，主机负载会直接传导到被测对象，因此必须记录。
    """
    snap = {"cpu_percent": None, "mem_percent": None}

    if sys.platform == "win32":
        try:
            import ctypes
            from ctypes import wintypes

            class FILETIME(ctypes.Structure):
                _fields_ = [("dwLowDateTime", wintypes.DWORD),
                            ("dwHighDateTime", wintypes.DWORD)]

            def _ft(v):
                return (v.dwHighDateTime << 32) | v.dwLowDateTime

            k32 = ctypes.windll.kernel32
            idle, kern, user = FILETIME(), FILETIME(), FILETIME()

            if k32.GetSystemTimes(ctypes.byref(idle), ctypes.byref(kern), ctypes.byref(user)):
                i0, k0, u0 = _ft(idle), _ft(kern), _ft(user)
                time.sleep(0.3)
                k32.GetSystemTimes(ctypes.byref(idle), ctypes.byref(kern), ctypes.byref(user))
                i1, k1, u1 = _ft(idle), _ft(kern), _ft(user)
                d_idle = i1 - i0
                d_total = (k1 - k0) + (u1 - u0)   # kernel 已包含 idle
                if d_total > 0:
                    snap["cpu_percent"] = round((1 - d_idle / d_total) * 100, 1)

            class MEMORYSTATUSEX(ctypes.Structure):
                _fields_ = [("dwLength", wintypes.DWORD),
                            ("dwMemoryLoad", wintypes.DWORD),
                            ("ullTotalPhys", ctypes.c_ulonglong),
                            ("ullAvailPhys", ctypes.c_ulonglong),
                            ("ullTotalPageFile", ctypes.c_ulonglong),
                            ("ullAvailPageFile", ctypes.c_ulonglong),
                            ("ullTotalVirtual", ctypes.c_ulonglong),
                            ("ullAvailVirtual", ctypes.c_ulonglong),
                            ("ullAvailExtendedVirtual", ctypes.c_ulonglong)]

            m = MEMORYSTATUSEX()
            m.dwLength = ctypes.sizeof(MEMORYSTATUSEX)
            if k32.GlobalMemoryStatusEx(ctypes.byref(m)):
                snap["mem_percent"] = float(m.dwMemoryLoad)
        except Exception:
            pass
    else:
        try:
            load = os.getloadavg()[0]
            snap["cpu_percent"] = round(load / (os.cpu_count() or 1) * 100, 1)
        except Exception:
            pass

    return snap


def environment_snapshot() -> dict:
    """一次完整的测量环境快照：主机负载 + 设备 uptime + 时间戳。"""
    return {
        "host": host_snapshot(),
        "device_uptime_s": device_uptime(),
        "timestamp": time.strftime("%Y-%m-%d %H:%M:%S"),
    }


def get_pid(bundle_name: str) -> str:
    """获取进程 PID，空串表示未运行"""
    try:
        return _shell(["pidof", bundle_name], timeout=5).stdout.strip()
    except Exception:
        return ""


def force_stop(bundle_name: str) -> None:
    try:
        _shell(["aa", "force-stop", bundle_name], timeout=15)
    except Exception:
        pass


def wait_until_dead(bundle_name: str, timeout: int = 10) -> bool:
    """轮询等待进程完全退出"""
    for _ in range(timeout):
        if not get_pid(bundle_name):
            return True
        time.sleep(1)
    return False


# ============================================================
# 单次冷启动
# ============================================================
def parse_start_output(raw: str) -> dict:
    """解析 `aa start -W` 输出。

    典型输出：
        StartMode: Cold
        BundleName: com.xxx
        AbilityName: com.xxx.MainAbility
        TotalTime: 1152
        WaitTime: 1160
    """
    out = {"start_mode": None, "total_time": None, "wait_time": None}
    m = re.search(r"StartMode:\s*(\w+)", raw)
    if m:
        out["start_mode"] = m.group(1)
    m = re.search(r"TotalTime:\s*(\d+)", raw)
    if m:
        out["total_time"] = int(m.group(1))
    m = re.search(r"WaitTime:\s*(\d+)", raw)
    if m:
        out["wait_time"] = int(m.group(1))
    return out


def start_once(bundle_name: str, ability_name: str,
               settle: float = 1.0, timeout: int = 30) -> dict:
    """单次冷启动测量。

    流程：force-stop -> 等进程真死 -> 稳定等待 -> aa start -W -> 解析
    返回 {"ok": True, start_mode, total_time, wait_time}
    或   {"ok": False, "reason": "..."}
    """
    force_stop(bundle_name)
    if not wait_until_dead(bundle_name):
        return {"ok": False, "reason": "force-stop 后进程未退出"}

    if settle > 0:
        time.sleep(settle)

    try:
        result = _shell(
            ["aa", "start", "-a", ability_name, "-b", bundle_name, "-W"],
            timeout=timeout,
        )
    except subprocess.TimeoutExpired:
        return {"ok": False, "reason": "启动命令超时"}

    parsed = parse_start_output(result.stdout)
    if parsed["total_time"] is None:
        return {"ok": False, "reason": "未解析到 TotalTime", "raw": result.stdout}

    return {"ok": True, **parsed}


# ============================================================
# 统计
# ============================================================
def compute_stats(values: list, digits: int = 2) -> dict:
    """对一组耗时做稳健统计。1.4826*MAD 用于估计真实噪声。

    digits —— 保留小数位（耗时用 2；锚点比值需要更高精度，用 6）
    """
    if not values:
        return {"n": 0}

    vs = sorted(values)
    n = len(vs)

    def percentile(p):
        if n == 1:
            return float(vs[0])
        k = (n - 1) * p
        lo = int(k)
        hi = min(lo + 1, n - 1)
        return vs[lo] + (vs[hi] - vs[lo]) * (k - lo)

    median = statistics.median(vs)
    mad = statistics.median([abs(v - median) for v in vs]) if n > 1 else 0.0

    return {
        "n": n,
        "min": vs[0],
        "max": vs[-1],
        "median": round(median, digits),
        "mean": round(statistics.fmean(vs), digits),
        "p10": round(percentile(0.10), digits),
        "p90": round(percentile(0.90), digits),
        "stdev": round(statistics.stdev(vs), digits) if n > 1 else 0.0,
        "mad": round(mad, digits),
        "robust_sigma": round(1.4826 * mad, digits),
        "range": round(vs[-1] - vs[0], digits),
    }


# ============================================================
# 多次测量
# ============================================================
def measure_cold_start(bundle_name: str, ability_name: str,
                       runs: int = 10, warmup: int = 1,
                       settle: float = 1.0, anchor=None,
                       on_progress=None) -> dict:
    """多次冷启动测量。

    参数：
      runs     —— 计入统计的有效次数
      warmup   —— 预热次数，不计入统计（默认 1，剔除系统缓存未预热的首轮）
      settle   —— 每次启动前的稳定等待（秒）
      anchor   —— 可选锚点应用 (bundle, ability)。

                  设置后，**每一轮都会紧接着再测一次锚点**，并记录
                  ratio = 目标耗时 / 锚点耗时。

                  意义：模拟器等设备常驻时，环境会随时间整体变快/变慢。
                  用比值作为观测单位可以把这个整体漂移约掉，
                  **无需重启模拟器**就能做跨会话对比。
      on_progress —— 进度回调，接收字符串

    返回 dict（保留旧键 all/first_launch/steady_median/steady_count 以兼容）。
    未采集到数据时返回 {"error": "..."}。
    """
    device = device_info()
    env_start = environment_snapshot()
    total_rounds = max(0, warmup) + max(1, runs)
    records = []

    for i in range(total_rounds):
        is_warmup = i < warmup
        tag = "预热" if is_warmup else "测量"
        if on_progress:
            on_progress(f"[{i + 1}/{total_rounds}] {tag}冷启动 ...")

        rec = start_once(bundle_name, ability_name, settle=settle)
        rec["warmup"] = is_warmup
        rec["round"] = i + 1

        # 锚点：紧接着测一次，与目标同处一个时间窗口，才能抵消漂移
        if anchor and rec.get("ok"):
            a_bundle, a_ability = anchor
            a_rec = start_once(a_bundle, a_ability, settle=settle)
            if (a_rec.get("ok") and a_rec.get("start_mode") == "Cold"
                    and a_rec.get("total_time")):
                rec["anchor_total"] = a_rec["total_time"]
                rec["ratio"] = round(rec["total_time"] / a_rec["total_time"], 4)
            force_stop(a_bundle)

        if rec.get("ok"):
            records.append(rec)
            if on_progress:
                extra = f" ratio={rec['ratio']}" if "ratio" in rec else ""
                on_progress(
                    f"    StartMode={rec['start_mode']} "
                    f"TotalTime={rec['total_time']}ms "
                    f"WaitTime={rec['wait_time']}ms{extra}"
                )
        elif on_progress:
            on_progress(f"    跳过：{rec.get('reason')}")

        force_stop(bundle_name)

    env_end = environment_snapshot()

    measured = [r for r in records if not r["warmup"]]
    cold = [r for r in measured if r["start_mode"] == "Cold"]
    non_cold = [r for r in measured if r["start_mode"] != "Cold"]

    totals = [r["total_time"] for r in cold]
    waits = [r["wait_time"] for r in cold if r["wait_time"] is not None]

    stats = compute_stats(totals)
    wait_stats = compute_stats(waits)
    all_totals = [r["total_time"] for r in measured]

    # 锚点归一化指标
    anchor_totals = [r["anchor_total"] for r in cold if "anchor_total" in r]
    ratios = [r["ratio"] for r in cold if "ratio" in r]
    anchor_stats = compute_stats(anchor_totals)
    norm_stats = compute_stats(ratios, digits=6)

    if not totals:
        return {
            "error": "未采集到有效冷启动数据，请检查设备连接、包名与 Ability 名称"
        }

    return {
        # ---- 兼容旧接口 ----
        "all": all_totals,
        "first_launch": all_totals[0] if all_totals else 0,
        "steady_median": stats.get("median", 0),
        "steady_count": len(totals),
        # ---- 新增 ----
        "cold_totals": totals,
        "bundle_name": bundle_name,
        "ability_name": ability_name,
        "device": device,
        "env": {"start": env_start, "end": env_end},
        "warmup": warmup,
        "requested_runs": runs,
        "start_modes": [r["start_mode"] for r in measured],
        "non_cold_count": len(non_cold),
        "wait_times": waits,
        "stats": stats,
        "wait_stats": wait_stats,
        "runs": measured,
        # ---- 锚点归一化 ----
        "anchor": ({"bundle": anchor[0], "ability": anchor[1]} if anchor else None),
        "anchor_totals": anchor_totals,
        "anchor_stats": anchor_stats,
        "ratios": ratios,
        "norm_stats": norm_stats,
    }


# ============================================================
# 会话对比 —— 判断「改动前后」的差异是否超出噪声
# ============================================================
def _mannwhitney(a: list, b: list):
    """Mann-Whitney U 检验（含并列秩校正与连续性校正），返回 (U, z, p_two_sided)。

    非参数检验，不假设正态分布，适合冷启动这种带抖动的耗时数据。
    """
    n1, n2 = len(a), len(b)
    if n1 == 0 or n2 == 0:
        return None

    combined = sorted([(v, 0) for v in a] + [(v, 1) for v in b])
    N = n1 + n2

    ranks = [0.0] * N
    tie_term = 0.0
    i = 0
    while i < N:
        j = i
        while j + 1 < N and combined[j + 1][0] == combined[i][0]:
            j += 1
        avg_rank = (i + j + 2) / 2.0          # 秩从 1 开始
        for k in range(i, j + 1):
            ranks[k] = avg_rank
        t = j - i + 1
        if t > 1:
            tie_term += t ** 3 - t
        i = j + 1

    r1 = sum(ranks[k] for k in range(N) if combined[k][1] == 0)
    u1 = r1 - n1 * (n1 + 1) / 2.0
    u2 = n1 * n2 - u1
    u = min(u1, u2)

    mu = n1 * n2 / 2.0
    if N > 1:
        sigma_sq = (n1 * n2 / 12.0) * ((N + 1) - tie_term / (N * (N - 1)))
    else:
        sigma_sq = 0.0
    sigma = sigma_sq ** 0.5
    if sigma <= 0:
        return (u, 0.0, 1.0)

    z = (u - mu + 0.5) / sigma                 # 连续性校正
    p = 2 * (1 - 0.5 * (1 + math.erf(abs(z) / math.sqrt(2))))
    return (u, z, p)


def compare_sessions(baseline: dict, current: dict, alpha: float = 0.05) -> dict:
    """对比两次测量会话，判断差异是否显著。

    判据：对两组的冷启动样本做 Mann-Whitney U 检验（非参数），p < alpha 判为显著。
    同时报告噪声下限与「最小可检测差异(MDD)」，便于判断本次对比的灵敏度。

    返回 dict：含 delta / pct / p_value / significant / mdd / verdict / warnings
    """
    b = baseline.get("stats", {}) or {}
    c = current.get("stats", {}) or {}
    warnings = []

    if not b.get("n") or not c.get("n"):
        return {"ok": False, "reason": "任一会话没有有效的冷启动样本，无法对比"}

    if b.get("n", 0) < 5:
        warnings.append(f"基线样本偏少（n={b['n']}），结论置信度下降")
    if c.get("n", 0) < 5:
        warnings.append(f"当前样本偏少（n={c['n']}），结论置信度下降")

    b_dev = baseline.get("device", {}) or {}
    c_dev = current.get("device", {}) or {}
    same_device = (b_dev.get("model") == c_dev.get("model")
                   and b_dev.get("software") == c_dev.get("software"))
    if not same_device:
        warnings.append("两次会话的设备/系统版本不一致，绝对数值不可直接比较")

    if baseline.get("non_cold_count", 0) or current.get("non_cold_count", 0):
        warnings.append(
            f"存在非冷启动样本被剔除（基线 {baseline.get('non_cold_count', 0)} 次、"
            f"当前 {current.get('non_cold_count', 0)} 次）"
        )

    # ---- 环境漂移检查 ----
    b_env = (baseline.get("env") or {}).get("start") or {}
    c_env = (current.get("env") or {}).get("start") or {}
    b_host = b_env.get("host") or {}
    c_host = c_env.get("host") or {}
    env_delta = {}

    b_cpu, c_cpu = b_host.get("cpu_percent"), c_host.get("cpu_percent")
    if b_cpu is not None and c_cpu is not None:
        env_delta["cpu_percent"] = round(c_cpu - b_cpu, 1)
        if abs(c_cpu - b_cpu) > 15:
            warnings.append(
                f"主机 CPU 负载差异较大（基线 {b_cpu}% → 当前 {c_cpu}%），可能影响结论"
            )

    b_mem, c_mem = b_host.get("mem_percent"), c_host.get("mem_percent")
    if b_mem is not None and c_mem is not None:
        env_delta["mem_percent"] = round(c_mem - b_mem, 1)
        if abs(c_mem - b_mem) > 10:
            warnings.append(f"主机内存占用差异较大（{b_mem}% → {c_mem}%）")

    b_up, c_up = b_env.get("device_uptime_s"), c_env.get("device_uptime_s")
    if b_up and c_up and b_up > 0 and c_up > 0:
        env_delta["emulator_restarted"] = c_up < b_up

    # ---- 锚点归一化（模拟器常驻时抵消环境整体漂移）----
    b_anchor_stats = baseline.get("anchor_stats") or {}
    c_anchor_stats = current.get("anchor_stats") or {}
    b_ratios = baseline.get("ratios") or []
    c_ratios = current.get("ratios") or []

    normalized = bool(b_ratios and c_ratios)
    norm = {}
    anchor_drift = None

    if (baseline.get("anchor") or {}) != (current.get("anchor") or {}):
        warnings.append("两次会话的锚点应用不一致，归一化结果不可比")

    if b_anchor_stats.get("median") and c_anchor_stats.get("median"):
        anchor_drift = round(c_anchor_stats["median"] / b_anchor_stats["median"], 4)
        if abs(anchor_drift - 1.0) > 0.10:
            warnings.append(
                f"锚点自身耗时变化 {(anchor_drift - 1) * 100:+.1f}%，环境已漂移；"
                f"已按锚点归一化，请以归一化结果为准"
            )

    if normalized:
        nb = compute_stats(b_ratios, digits=6)
        nc = compute_stats(c_ratios, digits=6)
        mw_n = _mannwhitney(b_ratios, c_ratios)
        np_value = round(mw_n[2], 4) if mw_n else None
        n_delta = round(nc["median"] - nb["median"], 6)
        n_pct = round(n_delta / nb["median"] * 100, 2) if nb["median"] else 0.0
        norm = {
            "baseline_ratio": nb["median"],
            "current_ratio": nc["median"],
            "delta_ratio": n_delta,
            "pct": n_pct,
            "p_value": np_value,
            "significant": (np_value is not None) and (np_value < alpha),
            "n_baseline": nb["n"],
            "n_current": nc["n"],
        }
        # 归一化口径下的最小可检测差异（换算成百分比）
        if nb["median"]:
            se_nb = 1.253 * (nb.get("robust_sigma") or 0) / math.sqrt(nb["n"])
            se_nc = 1.253 * (nc.get("robust_sigma") or 0) / math.sqrt(nc["n"])
            norm["mdd_pct"] = round(
                2.0 * (se_nb ** 2 + se_nc ** 2) ** 0.5 / nb["median"] * 100, 2
            )

    # 取两次会话的冷启动样本（兼容旧基线文件：回退到 all）
    a = baseline.get("cold_totals") or baseline.get("all") or []
    b_samp = current.get("cold_totals") or current.get("all") or []

    mw = _mannwhitney(a, b_samp)
    p_value = round(mw[2], 4) if mw else None

    b_sigma = b.get("robust_sigma", 0) or 0
    c_sigma = c.get("robust_sigma", 0) or 0
    noise = (b_sigma ** 2 + c_sigma ** 2) ** 0.5

    # 最小可检测差异(MDD)：中位数标准误的 2 倍（近似 95% 置信）
    se_b = 1.253 * b_sigma / math.sqrt(b["n"]) if b["n"] else 0.0
    se_c = 1.253 * c_sigma / math.sqrt(c["n"]) if c["n"] else 0.0
    mdd = round(2.0 * (se_b ** 2 + se_c ** 2) ** 0.5, 2)

    raw_delta = round(c["median"] - b["median"], 2)
    raw_pct = round(raw_delta / b["median"] * 100, 2) if b["median"] else 0.0

    # 有锚点时，以归一化结果作为主结论
    if normalized and norm:
        delta = norm["delta_ratio"]
        pct = norm["pct"]
        significant = norm["significant"]
        p_main = norm["p_value"]
        unit = "ratio"
        suffix = "（锚点归一化）"
    else:
        delta = raw_delta
        pct = raw_pct
        significant = (p_value is not None) and (p_value < alpha)
        p_main = p_value
        unit = "ms"
        suffix = ""

    if not significant:
        verdict = f"无显著差异（p={p_main} ≥ {alpha}）{suffix}"
    elif pct < 0:
        verdict = f"显著下降 ✅（{pct}%，p={p_main} < {alpha}）{suffix}"
    else:
        verdict = f"显著上升 🔴（{pct}%，p={p_main} < {alpha}）{suffix}"

    if not normalized and mdd and abs(delta) < mdd:
        warnings.append(
            f"差异 {abs(delta)}ms 小于最小可检测差异 {mdd}ms；"
            f"建议增加采样次数，或加锚点归一化后重测"
        )

    return {
        "ok": True,
        "bundle_name": current.get("bundle_name") or baseline.get("bundle_name"),
        "baseline_median": b["median"],
        "current_median": c["median"],
        # ---- 主结论（有锚点时 = 归一化结果）----
        "delta": delta,
        "pct": pct,
        "unit": unit,
        "p_value": p_main,
        "significant": significant,
        # ---- 原始（未归一化）----
        "raw_delta": raw_delta,
        "raw_pct": raw_pct,
        "raw_p_value": p_value,
        # ---- 锚点 ----
        "normalized": normalized,
        "norm": norm,
        "anchor_drift": anchor_drift,
        # ---- 其他 ----
        "alpha": alpha,
        "noise_floor": round(noise, 2),
        "mdd": mdd,
        "baseline_band": [b.get("p10"), b.get("p90")],
        "current_band": [c.get("p10"), c.get("p90")],
        "n_baseline": b["n"],
        "n_current": c["n"],
        "same_device": same_device,
        "env_delta": env_delta,
        "verdict": verdict,
        "warnings": warnings,
    }


# ============================================================
# 三明治判定（ABBA）—— 用两次基线自证环境是否漂移
# ============================================================
# ============================================================
# 基线存取
#
# 持久化属于本层：_server.py 只做透传。与 agent_core 自己负责归档报告是同一个口径。
# 曾一度把这段写在 cold_start_server.py 里，而那个文件的 docstring 同时写着
# "逻辑全在 cold_start_core.py"——自相矛盾的契约比没有契约更害人。
# ============================================================

def default_baseline_path() -> str:
    """默认基线文件位置（与本模块同目录）。"""
    return os.path.join(os.path.dirname(os.path.abspath(__file__)),
                        "cold_start_baseline.json")


def load_baseline(path: str = "") -> dict:
    """读取基线。

    统一返回 dict，**失败时带 "error" 键而不是抛异常**：
    "文件在不在""读坏了算什么"是本层该定的规则，不该让 server 层
    自己写 try/except 去猜。
    """
    p = path or default_baseline_path()
    if not os.path.exists(p):
        return {"error": f"未找到基线文件：{p}"}
    try:
        with open(p, encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError) as e:
        return {"error": f"基线读取失败（{p}）：{e}"}


def save_baseline(result: dict, path: str = "") -> dict:
    """把一次测量结果存为基线，返回 {"ok":..., "path":...} 或 {"ok":False, "error":...}。

    写盘失败（磁盘满、没权限）必须能报出来——否则界面上写着"基线已保存"，
    实际什么都没留下，等用户要做对比时才发现基线是空的。
    """
    p = path or default_baseline_path()
    try:
        with open(p, "w", encoding="utf-8") as f:
            json.dump(result, f, ensure_ascii=False, indent=2)
    except OSError as e:
        return {"ok": False, "error": f"基线写入失败（{p}）：{e}"}
    return {"ok": True, "path": p}


def sandwich_check(base1: dict, new: dict, base2: dict, alpha: float = 0.05) -> dict:
    """三明治法：base1 → 改动 → new → 回滚 → base2。

    逻辑：
      1. 先看 base1 vs base2：若本身就有显著差异 → 环境漂移，本轮结果作废；
      2. 再看 base1 vs new 与 base2 vs new：两者都显著且方向一致 → 结论可信。

    返回 dict：stability / effect_a / effect_b / stable / consistent / verdict
    """
    stability = compare_sessions(base1, base2, alpha)
    effect_a = compare_sessions(base1, new, alpha)
    effect_b = compare_sessions(base2, new, alpha)

    stable = bool(stability.get("ok")) and not stability.get("significant")

    dirs = []
    for e in (effect_a, effect_b):
        if e.get("ok") and e.get("significant"):
            dirs.append(e["delta"] < 0)
    consistent = len(dirs) == 2 and len(set(dirs)) == 1

    if not stable:
        verdict = ("⛔ 环境漂移：两次基线自身就有显著差异，本轮结果作废，请重测"
                   "（重点排查主机负载与模拟器是否重启）")
    elif len(dirs) < 2:
        verdict = "⚪ 无显著差异：改动效果未超出噪声，无法判定"
    elif consistent:
        direction = "下降" if dirs[0] else "上升"
        verdict = f"✅ 结论可信：改动效果显著，且在两次基线对比中方向一致（{direction}）"
    else:
        verdict = "⚠️ 结果矛盾：两次基线给出的方向不一致，建议增加采样次数"

    return {
        "ok": True,
        "alpha": alpha,
        "stable": stable,
        "consistent": consistent,
        "stability": stability,
        "effect_a": effect_a,
        "effect_b": effect_b,
        "verdict": verdict,
    }
