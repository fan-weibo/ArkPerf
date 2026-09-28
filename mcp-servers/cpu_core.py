import subprocess
import time
import re
import statistics


# ============================================================
# 规则判断：CPU 阈值
# ============================================================
CPU_THRESHOLDS = {
    "idle": 5,      # < 5% 空闲
    "light": 30,    # < 30% 轻度
    "heavy": 70,    # < 70% 偏高
                    # >= 70% 严重
}


def judge_cpu(percent_str: str) -> str:
    """根据 CPU 使用率字符串（如 '0.04%'）返回评价标签"""
    try:
        value = float(percent_str.rstrip("%"))
    except (ValueError, AttributeError):
        return "❓ 无法解析"

    if value < CPU_THRESHOLDS["idle"]:
        return "✅ 空闲"
    if value < CPU_THRESHOLDS["light"]:
        return "🟡 轻度"
    if value < CPU_THRESHOLDS["heavy"]:
        return "⚠️ 偏高"
    return "🔴 严重"


def get_pid(bundle_name: str) -> str:
    result = subprocess.run(
        ["hdc", "shell", "pidof", bundle_name],
        capture_output=True, text=True, timeout=5
    )
    return result.stdout.strip()


def _parse_cpu_line(raw: str, bundle_name: str) -> dict:
    """从 hidumper --cpuusage 输出中解析指定进程的 CPU 数据"""
    for line in raw.splitlines():
        if bundle_name in line and "%" in line:
            parts = line.strip().split()
            if len(parts) >= 6:
                return {
                    "total": parts[1],
                    "user": parts[2],
                    "kernel": parts[3],
                    "fault_minor": parts[4],
                    "fault_major": parts[5],
                }
    return {}


def get_cpu_usage(bundle_name: str) -> dict:
    """纯逻辑：获取 CPU 使用率单次快照。"""
    pid = get_pid(bundle_name)
    if not pid:
        return {"error": f"未找到进程 {bundle_name}，请先启动应用"}

    result = subprocess.run(
        ["hdc", "shell", "hidumper", "--cpuusage", pid],
        capture_output=True, text=True, timeout=15
    )
    parsed = _parse_cpu_line(result.stdout, bundle_name)
    if not parsed:
        return {"error": "未能从 hidumper 输出中解析 CPU 数据", "pid": pid, "raw": result.stdout}

    return {"pid": pid, **parsed}


def get_cpu_trend(bundle_name: str, duration: int = 10,
                  interval: int = 2, on_progress=None) -> dict:
    """纯逻辑：多次采样 CPU 使用率，返回趋势序列。"""
    pid = get_pid(bundle_name)
    if not pid:
        return {"error": f"未找到进程 {bundle_name}，请先启动应用"}

    count = max(1, duration // interval)
    total_samples = []
    user_samples = []
    kernel_samples = []

    for i in range(count):
        if on_progress:
            on_progress(f"[{i+1}/{count}] 采样 CPU ...")

        result = subprocess.run(
            ["hdc", "shell", "hidumper", "--cpuusage", pid],
            capture_output=True, text=True, timeout=15
        )
        parsed = _parse_cpu_line(result.stdout, bundle_name)
        if parsed:
            total_samples.append(parsed["total"])
            user_samples.append(parsed["user"])
            kernel_samples.append(parsed["kernel"])

        if i < count - 1:
            time.sleep(interval)

    return {
        "pid": pid,
        "count": count,
        "interval": interval,
        "total_samples": total_samples,
        "user_samples": user_samples,
        "kernel_samples": kernel_samples,
    }


# ============================================================
# 判定（供 Agent 编排层使用，返回结构化结果）
# ============================================================
def to_percent(value):
    """把 '0.04%' / 0.04 / '0.04' 统一成 float 百分比；失败返回 None。"""
    try:
        return float(str(value).strip().rstrip("%"))
    except (TypeError, ValueError):
        return None


def judge_cpu_samples(samples: list) -> dict:
    """对 CPU 采样序列做判定：以**中位数**为口径，峰值作参考。

    verdict: ok / issue；severity: none / mild / moderate / heavy
    """
    vals = [v for v in (to_percent(s) for s in samples) if v is not None]
    if not vals:
        return {"n": 0, "verdict": "unknown", "severity": "unknown",
                "label": "❓ 无有效 CPU 样本"}

    vs = sorted(vals)
    median = round(statistics.median(vs), 2)
    peak = round(vs[-1], 2)

    if median < CPU_THRESHOLDS["idle"]:
        sev, tag = "none", "✅ 低"
    elif median < CPU_THRESHOLDS["light"]:
        sev, tag = "mild", "🟡 轻度"
    elif median < CPU_THRESHOLDS["heavy"]:
        sev, tag = "moderate", "⚠️ 偏高"
    else:
        sev, tag = "heavy", "🔴 严重"

    return {
        "n": len(vals),
        "median": median,
        "peak": peak,
        "min": round(vs[0], 2),
        "verdict": "issue" if sev in ("moderate", "heavy") else "ok",
        "severity": sev,
        "label": f"{tag} CPU 占用中位 {median}%（峰值 {peak}%，{len(vals)} 个样本）",
    }


def measure_cpu(bundle_name: str, samples: int = 3, interval: int = 2,
                on_progress=None) -> dict:
    """CPU 采集 + 判定。口径：应用**空闲**时的 CPU 占用（背景耗电/空转）。

    空闲占用偏高通常意味着常驻轮询、定时任务过密或后台线程未收敛。
    """
    samples = max(1, samples)
    trend = get_cpu_trend(bundle_name, duration=samples * interval,
                          interval=interval, on_progress=on_progress)
    if "error" in trend:
        return trend

    judged = judge_cpu_samples(trend["total_samples"])
    if not judged.get("n"):
        return {"error": "未采集到有效 CPU 样本", "pid": trend.get("pid")}

    return {**trend, **judged}
