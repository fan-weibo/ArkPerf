"""主线程卡顿 / 阻塞判定 —— 纯逻辑层。

目标只有一个：**判定「有没有卡顿 / 阻塞」**，不输出精确时延数值。

判定依据是**系统自身的判定结果**，我们只做计数（不自造"长任务阈值"）：

  hilog · Hiview-PerfMonitor / RSJankStats
    - `ProcessJank: JankFrameMonitor::ProcessJank jank >= threshold`
        → 系统认定该帧卡顿超标（出现一次 = 一次超额卡顿）
    - `RSJankStats::SetJankStats jank frames N`
        → 上报窗口内的卡顿帧数

为什么不采 hisysevent 的 INTERACTION_*_LATENCY：
  那类事件产出的是**时延数值**，会被负载类型（滑动惯性动画）和环境漂移放大，
  实测中位数可被抬到数千毫秒，无法作为可靠判据。而「有没有卡顿」这件事
  系统已经判好了，直接采信更稳、更快、失败面更小。

接口契约：仅依赖标准库；函数返回 dict；judge_* 判定函数留在本层。
"""

import re
import subprocess
import time

# ============================================================
# 判定阈值（官方未公开统一阈值，此处为经验参考值，可按项目调整）
# ============================================================
# 卡顿判定出现的频次（次/分钟）。
# 参考：滑动过程中，行为正常的应用应当**接近 0 次**；故阈值取得较紧。
# ⚠️ 官方未公开统一阈值，这里是经验值，待有"已知坏样本"后再标定。
JANK_RATE_THRESHOLDS = {"mild": 10, "moderate": 30}

# 噪声底数：整个测试窗口内 ≤ 此次数的零星判定视为系统噪声，不触发 blocked。
# 依据实测：同一应用同条件复测，jank>=threshold 计数在 0~2 次间波动
# （HarmonyStudy / open_neteasy_cloud 均出现 0↔1 翻转），单次事件无判别力。
JANK_NOISE_FLOOR = 1

# 负载参数
DEFAULT_VELOCITY = 400
DEFAULT_GAP_S = 1.5

# CLI 未指定包名时的回退值
DEFAULT_BUNDLE_FALLBACK = "com.wandroid.harmonyStudy"

# 卡顿日志的关键串
_K_PAT_THRESHOLD = "jank >= threshold"
_K_PAT_PROCESS = "processjank"
_K_PAT_FRAMES = "jank frames"


# ============================================================
# 设备 IO
# ============================================================
def _shell(args, timeout=60):
    return subprocess.run(
        ["hdc", "shell", *args],
        capture_output=True, text=True, timeout=timeout,
    )


def get_pid(bundle_name: str) -> str:
    try:
        pids = _shell(["pidof", bundle_name], timeout=5).stdout.strip().split()
        return pids[0] if pids else ""
    except Exception:
        return ""


def clear_hilog() -> bool:
    """清空 hilog 缓冲区，保证测试窗口内只有本次产生的日志。"""
    try:
        return _shell(["hilog", "-r"], timeout=15).returncode == 0
    except Exception:
        return False


# ============================================================
# 负载驱动（没有负载就不会有卡顿）
# ============================================================
def fling(direction: int, velocity: int = DEFAULT_VELOCITY,
          timeout: int = 15) -> bool:
    """方向滑动。direction: 0=左 1=右 2=上 3=下。返回是否注入成功。"""
    try:
        subprocess.run(
            ["hdc", "shell", "uitest", "uiInput", "dircFling",
             str(direction), str(velocity)],
            capture_output=True, text=True, timeout=timeout,
        )
        return True
    except Exception:
        return False


def drive_interactions(rounds: int = 5, gap: float = DEFAULT_GAP_S,
                       velocity: int = DEFAULT_VELOCITY,
                       on_progress=None) -> int:
    """反复上下滑动制造负载，返回**成功注入的输入次数**。

    返回注入次数是有意义的：输出"未发现卡顿"之前，必须先确认
    负载真的送进去了，否则等于什么都没测。

    gap 需大于一次滑动动画的时长，否则输入排队会掩盖真实间隔。
    """
    ok = 0
    for _ in range(max(1, rounds)):
        if fling(3, velocity):
            ok += 1
        time.sleep(gap)
        if fling(2, velocity):
            ok += 1
        time.sleep(gap)
    if on_progress:
        on_progress(f"    已注入 {ok} 次滑动（{rounds} 轮，间隔 {gap}s）")
    return ok


# ============================================================
# 卡顿日志采集
# ============================================================
def _pick_lines(raw: str, keyword: str) -> list:
    return [ln for ln in raw.splitlines()
            if keyword.lower() in ln.lower() and ln.strip()]


def query_jank_logs(pid: str, keyword: str = "jank",
                    timeout: int = 60) -> dict:
    """取指定进程的卡顿日志。

    优先按进程过滤；若为空则回退为全局检索后按 PID 筛选
    （部分卡顿判定由系统服务上报，日志行可能不带应用 PID）。

    返回 {"lines": [...], "count": n, "filtered_by": "pid|global+pid|none"}
    """
    if not pid:
        return {"lines": [], "count": 0, "filtered_by": "none"}

    try:
        r = _shell(["hilog", "-x", "-P", str(pid), "-e", keyword],
                   timeout=timeout)
        lines = _pick_lines(r.stdout, keyword)
    except subprocess.TimeoutExpired:
        lines = []

    if lines:
        return {"lines": lines, "count": len(lines), "filtered_by": "pid"}

    # 回退：全局检索 + 按 PID 二次筛选
    try:
        r2 = _shell(["hilog", "-x", "-e", keyword], timeout=timeout)
    except subprocess.TimeoutExpired:
        return {"lines": [], "count": 0, "filtered_by": "pid"}

    pat = re.compile(rf"\s{re.escape(str(pid))}\s")
    lines2 = [ln for ln in _pick_lines(r2.stdout, keyword) if pat.search(ln)]
    return {"lines": lines2, "count": len(lines2), "filtered_by": "global+pid"}


def parse_jank_logs(lines: list) -> dict:
    """从卡顿日志行里提取计数特征。

    两类行分别计数，避免混为一谈：
      threshold_events —— "jank >= threshold" 判定出现次数（主判据）
      frame_reports    —— "jank frames N" 上报次数（辅证）+ 峰值帧数
    """
    threshold_events = 0
    frame_reports = 0
    peaks = []

    for ln in lines:
        low = ln.lower()
        if _K_PAT_THRESHOLD in low or _K_PAT_PROCESS in low:
            threshold_events += 1
        if _K_PAT_FRAMES in low:
            frame_reports += 1
            seg = low.split(_K_PAT_FRAMES, 1)[-1]
            nums = [int(x) for x in re.findall(r"\d+", seg)]
            if nums:
                peaks.append(max(nums))

    return {
        "threshold_events": threshold_events,
        "frame_reports": frame_reports,
        "peak_jank_frames": max(peaks) if peaks else 0,
        "max_report_frames": sum(peaks) if peaks else 0,
    }


# ============================================================
# 判定
# ============================================================
def judge_blocking(features: dict, duration_s: float) -> dict:
    """按系统卡顿判定的出现频次，判定「有没有卡顿/阻塞」。

    返回 verdict ∈ {clear, blocked}、severity ∈ {none, mild, moderate, heavy}。
    主判据用 threshold_events；为零时退回 frame_reports。
    """
    ev = features.get("threshold_events", 0)
    rep = features.get("frame_reports", 0)

    if ev > 0:
        count, basis = ev, "hilog 中 jank ≥ threshold 判定次数"
    elif rep > 0:
        count, basis = rep, "hilog 中 jank frames 上报次数"
    else:
        count, basis = 0, ""

    if count == 0:
        return {
            "verdict": "clear", "severity": "none", "count": 0,
            "rate_per_min": 0.0, "basis": "",
            "label": "✅ 未发现卡顿/阻塞（系统在测试窗口内未产生卡顿判定）",
        }

    rate = round(count * 60.0 / duration_s, 1) if duration_s > 0 else 0.0

    # 噪声带：零星 1 次判定在系统噪声范围内（同条件复测 0~1 波动），不判 blocked
    if count <= JANK_NOISE_FLOOR:
        return {
            "verdict": "clear", "severity": "none", "count": count,
            "rate_per_min": rate, "basis": basis,
            "label": (f"✅ 未发现卡顿/阻塞（窗口内 {count} 次零星判定，"
                      f"处于系统噪声范围内，不构成卡顿结论）"),
        }

    if rate <= JANK_RATE_THRESHOLDS["mild"]:
        sev, tag = "mild", "🟡 轻度卡顿"
    elif rate <= JANK_RATE_THRESHOLDS["moderate"]:
        sev, tag = "moderate", "🟠 中等卡顿"
    else:
        sev, tag = "heavy", "🔴 频繁卡顿"

    peak = features.get("peak_jank_frames", 0)
    peak_note = f"，单次峰值 {peak} 帧" if peak else ""

    return {
        "verdict": "blocked", "severity": sev, "count": count,
        "rate_per_min": rate, "basis": basis,
        "label": (f"{tag}：检测到卡顿/阻塞 {count} 次"
                  f"（{rate} 次/分钟{peak_note}）"),
    }


# ============================================================
# 完整报告
# ============================================================
def get_blocking_report(bundle_name: str, rounds: int = 5,
                        clear_log: bool = True, gap: float = DEFAULT_GAP_S,
                        velocity: int = DEFAULT_VELOCITY,
                        on_progress=None) -> dict:
    """主线程卡顿 / 阻塞判定。

    流程：清缓冲 → 注入负载 → 采集系统卡顿日志 → 计数判定。
    """
    pid = get_pid(bundle_name)
    if not pid:
        return {"error": f"未找到进程 {bundle_name}，请先启动应用并保持前台"}

    if clear_log:
        if on_progress:
            on_progress("清空 hilog 缓冲区 ...")
        clear_hilog()

    if on_progress:
        on_progress(f"驱动 {rounds} 轮滑动制造负载 ...")
    t0 = time.time()
    input_events = drive_interactions(
        rounds, gap=gap, velocity=velocity, on_progress=on_progress)
    time.sleep(1.0)                     # 等日志刷出
    duration_s = max(0.001, time.time() - t0)

    if on_progress:
        on_progress("采集系统卡顿日志 ...")
    logs = query_jank_logs(pid)
    features = parse_jank_logs(logs["lines"])
    judged = judge_blocking(features, duration_s)

    verdict = judged["verdict"]
    label = judged["label"]
    severity = judged["severity"]
    warnings = []

    # 没有负载 => "未发现卡顿"没有意义
    if input_events == 0:
        verdict, severity = "unknown", "none"
        label = "❓ 未能注入任何操作，本次无有效负载，无法判定"
        warnings.append("uitest 注入失败：确认应用在前台、屏幕亮起、设备在线")

    if judged["count"] == 0 and input_events > 0:
        warnings.append("窗口内未出现卡顿判定；若体感确实卡顿，"
                        "可加大负载（--rounds / --velocity）后重测")

    if logs["filtered_by"] == "global+pid":
        warnings.append("进程级过滤无结果，已回退为全局日志并按 PID 筛选，"
                        "存在混入旧日志的可能")

    return {
        "pid": pid,
        "bundle_name": bundle_name,
        "window": {"duration_s": round(duration_s, 2)},
        "input_events": input_events,
        "log_filter": logs["filtered_by"],
        "features": features,
        "verdict": verdict,          # clear | blocked | unknown
        "severity": severity,        # none | mild | moderate | heavy
        "count": judged["count"],
        "rate_per_min": judged.get("rate_per_min", 0.0),
        "basis": judged.get("basis", ""),
        "label": label,
        "evidence_lines": logs["lines"][-5:],
        "warnings": warnings,
    }
