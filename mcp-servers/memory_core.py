"""内存测量与泄漏判定 —— 纯逻辑层。

设计要点（相对旧版的关键修正）：
  1. 泄漏判定改用「最小二乘斜率 + 单调性占比」，不再用首尾差；
  2. 阈值改为**速率**（kB/min），与采样时长脱钩；
  3. **以 ArkTS 堆为主判据**（应用自身对象），PSS 作辅助；
  4. 采样期间用 uitest **驱动操作**（反复滑动），否则空闲内存必然平、测不出泄漏；
  5. 采样失败显式计数，返回值区分「理论次数」与「有效样本数」；
  6. 解析取最后一条 Total 汇总行，并防御多 PID。

只依赖标准库，保持「纯逻辑 + 返回 dict」的接口契约。
"""

import re
import statistics
import subprocess
import time

# ============================================================
# 判定阈值：速率（kB/min）
# ============================================================
MEMORY_RATE_THRESHOLDS = {
    "stable": 512,      # < 0.5 MB/min 视为稳定
    "watch": 2048,      # < 2 MB/min   缓慢增长，关注
                        # >= 2 MB/min  疑似泄漏
}

MONO_RATIO_THRESHOLD = 0.7   # 上升点占比达到此值才算「持续增长」
MIN_SAMPLES = 3              # 少于 3 个有效采样无法判趋势

# 兼容旧常量名
MEMORY_GROWTH_THRESHOLDS = MEMORY_RATE_THRESHOLDS


# ============================================================
# 趋势统计
# ============================================================
def least_squares_slope(values: list) -> float:
    """最小二乘斜率（单位：每采样点）"""
    n = len(values)
    if n < 2:
        return 0.0
    xs = list(range(n))
    mx = sum(xs) / n
    my = sum(values) / n
    denom = sum((x - mx) ** 2 for x in xs)
    if denom == 0:
        return 0.0
    return sum((x - mx) * (y - my) for x, y in zip(xs, values)) / denom


def monotonic_ratio(values: list) -> float:
    """相邻点上升的比例。接近 1 表示持续单调增长。"""
    if len(values) < 2:
        return 0.0
    ups = sum(1 for a, b in zip(values, values[1:]) if b > a)
    return ups / (len(values) - 1)


def analyze_memory_series(values: list, interval_s: float = 1.0) -> dict:
    """对一组内存采样做趋势分析（只给统计量，不判定）。"""
    vs = [v for v in values if v is not None]
    if not vs:
        return {"n": 0}

    slope = least_squares_slope(vs)
    rate_per_min = slope * (60.0 / interval_s) if interval_s > 0 else 0.0

    return {
        "n": len(vs),
        "first": vs[0],
        "last": vs[-1],
        "min": min(vs),
        "max": max(vs),
        "range": max(vs) - min(vs),
        "net_change": vs[-1] - vs[0],
        "slope_per_sample": round(slope, 2),
        "rate_per_min": round(rate_per_min, 1),
        "mono_ratio": round(monotonic_ratio(vs), 3),
    }


def judge_memory_leak(values: list, interval_s: float = 1.0,
                      label: str = "") -> dict:
    """基于「斜率 + 单调性」判定是否疑似内存泄漏。返回 dict（含中文标签）。

    真泄漏的特征是**持续单调上升且速率显著**，而不是「首尾差大」。
    这样既能排除一次性分配，也能识别缓慢但持续的泄漏。
    """
    st = analyze_memory_series(values, interval_s)
    if st.get("n", 0) < MIN_SAMPLES:
        return {**st, "verdict": "unknown",
                "label": "❓ 样本不足（至少需要 3 个有效采样）"}

    rate = st["rate_per_min"]
    mono = st["mono_ratio"]
    pre = f"{label} " if label else ""
    suffix = f"（{rate:.0f} kB/min，单调性 {mono:.0%}）"

    if rate >= MEMORY_RATE_THRESHOLDS["watch"] and mono >= MONO_RATIO_THRESHOLD:
        return {**st, "verdict": "leak",
                "label": f"🔴 {pre}持续增长{suffix}，疑似内存泄漏"}
    if rate >= MEMORY_RATE_THRESHOLDS["stable"] and mono >= MONO_RATIO_THRESHOLD:
        return {**st, "verdict": "watch",
                "label": f"🟡 {pre}缓慢增长{suffix}，建议关注"}

    # 稳定：区分「整体下降」与「有涨有落（一次性分配/GC）」，避免措辞误导
    if rate <= -MEMORY_RATE_THRESHOLDS["watch"]:
        return {**st, "verdict": "stable",
                "label": f"✅ {pre}稳定（整体下降 {abs(rate):.0f} kB/min，无增长迹象）"}
    if rate >= MEMORY_RATE_THRESHOLDS["watch"]:
        return {**st, "verdict": "stable",
                "label": (f"✅ {pre}稳定（无持续增长趋势；整体斜率 {rate:+.0f} kB/min "
                          f"由一次性分配/GC 造成，单调性 {mono:.0%} 未达阈值）")}
    return {**st, "verdict": "stable",
            "label": f"✅ {pre}稳定（{rate:+.0f} kB/min）"}


def judge_memory_trend(pss_samples_kb: list, interval_s: float = 2.0) -> str:
    """兼容旧接口：返回纯文本判定标签（内部走新的斜率+单调性判定）。"""
    return judge_memory_leak(pss_samples_kb, interval_s)["label"]


# ============================================================
# 设备 IO
# ============================================================
def get_pid(bundle_name: str) -> str:
    """获取进程 PID。多 PID 时只取第一个，避免传给 hidumper 时出错。"""
    result = subprocess.run(
        ["hdc", "shell", "pidof", bundle_name],
        capture_output=True, text=True, timeout=5
    )
    pids = result.stdout.strip().split()
    return pids[0] if pids else ""


def _parse_memory(raw: str) -> dict:
    """从 hidumper --mem 输出中提取关键内存指标（单位 KB）。

    注意：表头行也是 `Total Clean Dirty...`，但后面不是数字，不会被匹配；
    汇总行 `Total  141949  164488 ...` 才是目标。取**最后一条**匹配，防止
    分节中出现多个 Total 行。
    """
    pss_total = None
    ark_ts_heap = None
    native_heap = None

    for line in raw.splitlines():
        m = re.match(r"\s*Total\s+(\d+)\s+\d+", line)
        if m:
            pss_total = int(m.group(1))      # 覆盖式赋值 → 最终保留最后一条
            continue
        m = re.match(r"\s*ark ts heap\s+(\d+)", line)
        if m:
            ark_ts_heap = int(m.group(1))
            continue
        m = re.match(r"\s*native heap\s+(\d+)", line)
        if m:
            native_heap = int(m.group(1))
            continue

    return {
        "pss_total_kb": pss_total,
        "ark_ts_heap_kb": ark_ts_heap,
        "native_heap_kb": native_heap,
    }


def _snapshot_by_pid(pid: str, timeout: int = 20) -> dict:
    """按 PID 直接取内存快照（内部用）"""
    try:
        result = subprocess.run(
            ["hdc", "shell", "hidumper", "--mem", pid],
            capture_output=True, text=True, timeout=timeout
        )
    except subprocess.TimeoutExpired:
        return {"pss_total_kb": None, "ark_ts_heap_kb": None, "native_heap_kb": None}
    return _parse_memory(result.stdout)


def get_memory_snapshot(bundle_name: str) -> dict:
    """纯逻辑：获取内存快照（单次）。"""
    pid = get_pid(bundle_name)
    if not pid:
        return {"error": f"未找到进程 {bundle_name}，请先启动应用"}

    metrics = _snapshot_by_pid(pid, timeout=30)
    if metrics["pss_total_kb"] is None:
        return {"error": "未能从 hidumper 输出中解析内存数据", "pid": pid}
    return {"pid": pid, **metrics}


def force_gc(pid: str, timeout: int = 30) -> dict:
    """用官方命令强制触发目标进程 GC。

    命令：hidumper --mem-jsheap <pid> --gc
    限制：**仅支持 debug 签名的应用**（release 应用会明确报错）。

    这一步很关键：不做 GC 时，内存里混着大量「还没被回收」的垃圾，
    无法区分「真泄漏」与「延迟回收」。官方分析法正是靠先 GC 再看是否回落。

    返回 {"ok": bool, "supported": bool|None, "reason": str}
    """
    try:
        r = subprocess.run(
            ["hdc", "shell", "hidumper", "--mem-jsheap", str(pid), "--gc"],
            capture_output=True, text=True, timeout=timeout,
        )
    except subprocess.TimeoutExpired:
        return {"ok": False, "supported": None, "reason": "强制 GC 超时"}

    out = (r.stdout or "") + (r.stderr or "")
    if "only supported for debug-signed" in out:
        return {"ok": False, "supported": False,
                "reason": "应用非 debug 签名，hidumper --mem-jsheap 不可用"}
    return {"ok": True, "supported": True, "reason": ""}


# ============================================================
# 操作驱动（用 uitest 制造负载，泄漏才会暴露）
# ============================================================
def _uitest_input(args, timeout: int = 15):
    return subprocess.run(
        ["hdc", "shell", "uitest", "uiInput", *args],
        capture_output=True, text=True, timeout=timeout
    )


def fling(direction: int, velocity: int = 800) -> bool:
    """方向滑动。direction: 0=左 1=右 2=上 3=下"""
    try:
        _uitest_input(["dircFling", str(direction), str(velocity)])
        return True
    except Exception:
        return False


def press_back() -> bool:
    """注入返回键"""
    try:
        _uitest_input(["keyEvent", "Back"])
        return True
    except Exception:
        return False


def default_workload(rounds: int = 1, on_progress=None) -> None:
    """默认负载：下滑→上滑循环，驱使应用反复布局/滚动。

    不做操作的「空闲采样」几乎必然得到平直序列 —— 那是假阴性。
    """
    for _ in range(rounds):
        fling(3)          # 下
        time.sleep(0.4)
        fling(2)          # 上
        time.sleep(0.4)
    if on_progress:
        on_progress(f"    已执行 {rounds} 轮滑动负载")


# ============================================================
# 采样序列
# ============================================================
def sample_memory_series(bundle_name: str, samples: int = 10,
                         interval: float = 1.0, warmup: int = 1,
                         workload: bool = True, settle_samples: int = 0,
                         on_progress=None) -> dict:
    """采样内存序列。

    参数：
      samples        —— 计入统计的有效采样次数
      interval       —— 采样间隔（秒），用于把斜率换算成 kB/min
      warmup         —— 预热次数，不计入统计（默认 1，等首次分配稳定）
      workload       —— 是否在每次采样前驱动操作（默认 True；False = 空闲采样）
      settle_samples —— **静置观察期**次数：停止操作后再采样这么多次。
                        用于区分「真泄漏」与「增长尚未被回收」：
                        静置后回落 → 不是泄漏；静置后仍高位 → 疑似泄漏。

    返回 dict；失败返回 {"error": "..."}
    """
    pid = get_pid(bundle_name)
    if not pid:
        return {"error": f"未找到进程 {bundle_name}，请先启动应用"}

    pss, ark, nat = [], [], []
    failed = 0
    total = max(0, warmup) + max(1, samples)

    for i in range(total):
        is_warmup = i < warmup
        if on_progress:
            on_progress(f"[{i + 1}/{total}]{' 预热' if is_warmup else ''} 采样内存 ...")

        if workload:
            default_workload(1)

        m = _snapshot_by_pid(pid)
        if m["pss_total_kb"] is None and m["ark_ts_heap_kb"] is None:
            failed += 1
            if on_progress:
                on_progress("    采样失败，跳过")
        else:
            if on_progress:
                on_progress(
                    f"    PSS={m['pss_total_kb']} kB  "
                    f"ArkTS={m['ark_ts_heap_kb']} kB  "
                    f"Native={m['native_heap_kb']} kB"
                )
            if not is_warmup:
                if m["pss_total_kb"] is not None:
                    pss.append(m["pss_total_kb"])
                if m["ark_ts_heap_kb"] is not None:
                    ark.append(m["ark_ts_heap_kb"])
                if m["native_heap_kb"] is not None:
                    nat.append(m["native_heap_kb"])

        if i < total - 1:
            time.sleep(interval)

    # ---- 静置观察期：停止操作，看内存是否回落 ----
    settle_pss, settle_ark, settle_nat = [], [], []
    for i in range(max(0, settle_samples)):
        if on_progress:
            on_progress(f"[静置 {i + 1}/{settle_samples}] 停止操作，观察内存是否回落 ...")
        time.sleep(interval)
        m = _snapshot_by_pid(pid)
        if on_progress:
            on_progress(f"    PSS={m['pss_total_kb']} kB  ArkTS={m['ark_ts_heap_kb']} kB")
        if m["pss_total_kb"] is not None:
            settle_pss.append(m["pss_total_kb"])
        if m["ark_ts_heap_kb"] is not None:
            settle_ark.append(m["ark_ts_heap_kb"])
        if m["native_heap_kb"] is not None:
            settle_nat.append(m["native_heap_kb"])

    return {
        "pid": pid,
        "requested_samples": samples,
        "valid_samples": len(pss),
        "failed": failed,
        "interval": interval,
        "workload": workload,
        "settle_samples": settle_samples,
        "pss_samples_kb": pss,
        "ark_ts_samples_kb": ark,
        "native_samples_kb": nat,
        "settle_pss_samples_kb": settle_pss,
        "settle_ark_ts_samples_kb": settle_ark,
        "settle_native_samples_kb": settle_nat,
    }


def get_memory_trend(bundle_name: str, duration: int = 10,
                     interval: int = 2, workload: bool = True,
                     on_progress=None) -> dict:
    """兼容旧接口：按 duration/interval 采样内存序列。"""
    samples = max(1, int(duration // interval))
    return sample_memory_series(
        bundle_name, samples=samples, interval=interval,
        warmup=1, workload=workload, on_progress=on_progress,
    )


# ============================================================
# 泄漏检测报告（组合：采样 + 判定）
# ============================================================
RESIDUAL_THRESHOLD_KB = 2048   # 静置后残留超过此值才算「未回收」


def analyze_residual(workload_values: list, settle_values: list) -> dict:
    """静置残留分析：操作期起点 → 静置后中位数。

    用于区分「真泄漏」与「尚未被 GC 回收」：
      静置后回落到起点附近 → 只是延迟回收，不是泄漏；
      静置后仍显著高于起点 → 未回收，疑似泄漏。
    """
    wl = [v for v in workload_values if v is not None]
    st = [v for v in settle_values if v is not None]
    if not wl or not st:
        return {}

    base = wl[0]
    settle_med = statistics.median(st)
    return {
        "baseline": base,
        "peak": max(wl),
        "settle_median": round(settle_med, 1),
        "residual": round(settle_med - base, 1),
        "released": round(base - settle_med, 1),
        "not_recycled": (settle_med - base) >= RESIDUAL_THRESHOLD_KB,
    }


def sample_memory_cycles(bundle_name: str, cycles: int = 5,
                         interval: float = 1.0, warmup: int = 1,
                         workload: bool = True, gc: bool = True,
                         on_progress=None) -> dict:
    """按「操作周期」采样，用于官方的「锯齿 vs 阶梯」判定。

    每个周期：强制 GC → 采样「回落点」 → 驱动操作 → 采样「操作后峰值」

    判定原理：
      锯齿 = 每周期回落点都回到基线附近 → 正常
      阶梯 = 回落点逐次抬升            → 泄漏

    同时记录真实周期时长，以便把斜率换算成 kB/min。
    """
    pid = get_pid(bundle_name)
    if not pid:
        return {"error": f"未找到进程 {bundle_name}，请先启动应用"}

    gc_enabled = bool(gc)
    gc_supported = None
    b_pss, b_ark, b_nat = [], [], []
    p_pss, p_ark, p_nat = [], [], []
    stamps = []

    total = max(0, warmup) + max(1, cycles)
    for i in range(total):
        is_warmup = i < warmup
        if on_progress:
            on_progress(f"[{i + 1}/{total}]{' 预热' if is_warmup else ''}周期："
                        f"{'强制GC → ' if gc_enabled else ''}回落点 → 操作 → 峰值")

        # 1) 先强制 GC，再取「回落点」
        if gc_enabled:
            g = force_gc(pid)
            if g["supported"] is False:
                gc_enabled = False
                gc_supported = False
                if on_progress:
                    on_progress(f"    ⚠ {g['reason']}，后续周期跳过 GC")
            elif g["supported"]:
                gc_supported = True

        m0 = _snapshot_by_pid(pid)

        # 2) 驱动操作
        if workload:
            default_workload(1)

        # 3) 取「操作后峰值」
        m1 = _snapshot_by_pid(pid)

        if on_progress:
            on_progress(
                f"    回落点 PSS={m0['pss_total_kb']} ArkTS={m0['ark_ts_heap_kb']}"
                f"  →  操作后 PSS={m1['pss_total_kb']} ArkTS={m1['ark_ts_heap_kb']}"
            )

        if not is_warmup:
            stamps.append(time.time())
            if m0["pss_total_kb"] is not None:
                b_pss.append(m0["pss_total_kb"])
            if m0["ark_ts_heap_kb"] is not None:
                b_ark.append(m0["ark_ts_heap_kb"])
            if m0["native_heap_kb"] is not None:
                b_nat.append(m0["native_heap_kb"])
            if m1["pss_total_kb"] is not None:
                p_pss.append(m1["pss_total_kb"])
            if m1["ark_ts_heap_kb"] is not None:
                p_ark.append(m1["ark_ts_heap_kb"])
            if m1["native_heap_kb"] is not None:
                p_nat.append(m1["native_heap_kb"])

        if i < total - 1:
            time.sleep(interval)

    periods = [stamps[k + 1] - stamps[k] for k in range(len(stamps) - 1)]
    cycle_period = round(statistics.median(periods), 2) if periods else round(interval + 2.0, 2)

    return {
        "pid": pid,
        "requested_cycles": cycles,
        "valid_cycles": len(b_ark) if b_ark else len(b_pss),
        "cycle_period_s": cycle_period,
        "interval": interval,
        "workload": workload,
        "gc": gc,
        "gc_supported": gc_supported,
        "baseline_pss": b_pss,
        "baseline_ark_ts": b_ark,
        "baseline_native": b_nat,
        "peak_pss": p_pss,
        "peak_ark_ts": p_ark,
        "peak_native": p_nat,
    }


def judge_staircase(baselines: list, cycle_period_s: float = 1.0,
                    label: str = "") -> dict:
    """官方「锯齿 vs 阶梯」判据：看每周期回落点是否逐次抬升。

      锯齿（sawtooth）  = 回落点稳定     → 正常
      阶梯（staircase） = 回落点逐次抬升 → 泄漏
    """
    base = judge_memory_leak(baselines, cycle_period_s, label)
    st = {k: v for k, v in base.items() if k not in ("verdict", "label")}

    if base.get("n", 0) < MIN_SAMPLES:
        return {**st, "pattern": "unknown",
                "label": f"❓ {label} 周期数不足（至少需要 {MIN_SAMPLES} 个）"}

    rate = base["rate_per_min"]
    if base["verdict"] == "leak":
        # 有界增长识别：回落点前段抬升、后段（后一半）饱和 → 缓存填充特征，非持续泄漏。
        # 真泄漏的后段斜率不会消失；有界缓存装满后斜率归零（或单调性瓦解）。
        tail = baselines[len(baselines) // 2:]
        if len(tail) >= MIN_SAMPLES:
            tail_st = analyze_memory_series(tail, cycle_period_s)
            tail_rate = tail_st.get("rate_per_min", 0.0)
            tail_mono = tail_st.get("mono_ratio", 0.0)
            if (tail_rate < MEMORY_RATE_THRESHOLDS["stable"]
                    or tail_mono < MONO_RATIO_THRESHOLD):
                return {**base, "verdict": "capped", "pattern": "capped",
                        "tail_rate_per_min": round(tail_rate, 1),
                        "label": (f"🟡 {label} 有界增长：回落点前段抬升后饱和"
                                  f"（全程 {rate:.0f} kB/min，后段 {tail_rate:+.0f} kB/min）"
                                  f"→ 符合有界缓存填充特征，非持续泄漏")}
        return {**base, "pattern": "staircase",
                "label": (f"🔴 {label} 阶梯式上升：每周期回落点持续抬升"
                          f"（{rate:.0f} kB/min）→ 疑似泄漏")}
    if base["verdict"] == "watch":
        return {**base, "pattern": "watch",
                "label": f"🟡 {label} 回落点缓慢抬升（{rate:.0f} kB/min）→ 建议关注"}
    return {**base, "pattern": "sawtooth",
            "label": f"✅ {label} 锯齿状回落：回落点稳定（{rate:+.0f} kB/min）→ 无泄漏迹象"}


def get_memory_leak_report(bundle_name: str, cycles: int = 5,
                           interval: float = 1.0, warmup: int = 1,
                           workload: bool = True, gc: bool = True,
                           on_progress=None) -> dict:
    """内存泄漏检测（官方口径：先强制 GC，再看「锯齿 vs 阶梯」）。

    判定链路：
      1. 每周期先 `--gc` 强制回收 → 取「回落点」；再驱动操作 → 取「峰值」；
      2. 对回落点序列做「锯齿 / 阶梯」判定（周期起点是否逐次抬升）；
      3. ArkTS 堆 / PSS / Native 堆 三维度取最严重者为主结论。
    """
    s = sample_memory_cycles(
        bundle_name, cycles=cycles, interval=interval,
        warmup=warmup, workload=workload, gc=gc, on_progress=on_progress,
    )
    if "error" in s:
        return s

    period = s["cycle_period_s"]
    ark = judge_staircase(s["baseline_ark_ts"], period, "ArkTS 堆")
    pss = judge_staircase(s["baseline_pss"], period, "PSS")
    native = judge_staircase(s["baseline_native"], period, "Native 堆")

    severity = {"leak": 3, "watch": 2, "capped": 2, "stable": 1, "unknown": 0}
    layers = [("ArkTS 堆", ark), ("PSS", pss), ("Native 堆", native)]
    usable = [l for l in layers if l[1].get("n", 0) >= MIN_SAMPLES]
    if usable:
        primary_name, primary = max(
            usable,
            key=lambda l: (severity.get(l[1].get("verdict", "unknown"), 0),
                           1 if l[0] == "ArkTS 堆" else 0),
        )
    else:
        primary_name, primary = layers[0]

    verdict = primary.get("verdict", "unknown")
    label = primary.get("label", "❓ 无法判定")
    if verdict in ("leak", "watch") and primary_name != "ArkTS 堆":
        label = (f"{label}（表现为 {primary_name} 增长、ArkTS 堆未同步增长，"
                 f"可能是原生内存或渲染缓存问题）")

    deltas = [p - b for p, b in zip(s["peak_ark_ts"], s["baseline_ark_ts"])]

    warnings = []
    if s["valid_cycles"] < cycles:
        warnings.append(f"有效周期 {s['valid_cycles']} 个，少于请求的 {cycles} 个")
    if not workload:
        warnings.append("本次为空闲采样（未驱动操作），可能掩盖真实泄漏")
    if gc and s["gc_supported"] is False:
        warnings.append(
            "应用非 debug 签名，无法强制 GC；回落点可能偏高（含未回收内存），结论偏保守"
        )
    if not gc:
        warnings.append("未启用强制 GC，无法区分「真泄漏」与「延迟回收」")

    return {
        "pid": s["pid"],
        "bundle_name": bundle_name,
        "method": "cycle/staircase",
        "cycles": cycles,
        "valid_cycles": s["valid_cycles"],
        "cycle_period_s": period,
        "workload": workload,
        "gc": gc,
        "gc_supported": s["gc_supported"],
        "primary": primary_name,
        "verdict": verdict,
        "pattern": primary.get("pattern", "unknown"),
        "label": label,
        "ark_ts": ark,
        "pss": pss,
        "native": native,
        "per_cycle_delta_ark_ts": deltas,
        "series": s,
        "warnings": warnings,
    }
