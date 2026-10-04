"""内存占用变化 / 泄漏检测 —— MCP 薄封装（第三层）。

逻辑全在 memory_core.py（官方口径：强制 GC + 锯齿/阶梯判定）。
启动：python memory_server.py
"""

try:                                    # 兼容 fastmcp / 官方 SDK v1 / v2
    from fastmcp import FastMCP
except ImportError:
    try:
        from mcp.server.fastmcp import FastMCP
    except ImportError:                 # mcp 2.x：FastMCP 改名为 MCPServer
        from mcp.server.mcpserver import MCPServer as FastMCP

import memory_core as core

mcp = FastMCP("harmony-memory")

_VERDICT_CN = {
    "leak": "🔴 疑似内存泄漏",
    "watch": "🟡 缓慢增长，建议关注",
    "stable": "✅ 未发现泄漏",
    "unknown": "❓ 无法判定",
}


@mcp.tool()
def memory_snapshot(bundle_name: str) -> dict:
    """获取应用内存快照（PSS 总量 / ArkTS 堆 / Native 堆）。

    Args:
        bundle_name: 应用包名，如 com.example.app
    """
    r = core.get_memory_snapshot(bundle_name)
    if "error" in r:
        return {"ok": False, "error": r["error"], "中文说明": "❌ 采集失败"}
    return {
        "ok": True,
        "PSS_kB": r.get("pss_total_kb"),
        "PSS_MB": round((r.get("pss_total_kb") or 0) / 1024, 1),
        "ArkTS堆_kB": r.get("ark_ts_heap_kb"),
        "Native堆_kB": r.get("native_heap_kb"),
        "raw": r,
    }


@mcp.tool()
def memory_leak_check(bundle_name: str, cycles: int = 5,
                      workload: bool = True, gc: bool = True) -> dict:
    """检测应用是否存在内存持续增长（泄漏）。

    做法：每周期先强制 GC 取「回落点」，再驱动操作取「峰值」，
    对回落点序列判定「锯齿（正常）/ 阶梯（泄漏）」。

    Args:
        bundle_name: 应用包名
        cycles: 采样周期数（建议 5，越多越稳但更慢）
        workload: 是否驱动操作制造负载（关闭会掩盖真实泄漏）
        gc: 是否强制 GC（需 debug 签名应用，非 debug 会自动降级并告警）
    """
    r = core.get_memory_leak_report(bundle_name, cycles=cycles,
                                    workload=workload, gc=gc)
    if "error" in r:
        return {"ok": False, "error": r["error"], "中文说明": "❌ 采集失败"}

    verdict = r.get("verdict", "unknown")
    return {
        "ok": True,
        "是否泄漏": verdict,
        "中文判定": _VERDICT_CN.get(verdict, r.get("label", "")),
        "结论详情": r.get("label"),
        "主判据层": r.get("primary"),
        "形态": r.get("pattern"),
        "有效周期": f"{r.get('valid_cycles')}/{r.get('cycles')}",
        "强制GC": "已启用" if r.get("gc_supported") else "不支持（结论偏保守）",
        "告警": r.get("warnings"),
        "raw": r,
    }


@mcp.tool()
def memory_trend(bundle_name: str, duration: int = 10,
                 interval: float = 1.0, workload: bool = True) -> dict:
    """采样应用内存变化趋势，返回 PSS / ArkTS 堆 / Native 堆序列。

    **本工具不触发 GC**，这一点必须写清楚，否则极易被误读：
    堆在两次 GC 之间上涨是垃圾回收器的正常工作方式（垃圾先堆着、攒够了一起收），
    所以这条曲线单调上升**只说明"产生了垃圾还没回收"，不是泄漏证据**。
    判定是否泄漏要用 memory_leak_check：它每个周期强制 GC，看的是
    GC 之后的「回落点」是否逐次抬升。

    典型误读：看到本工具 3822→4170 就报"内存持续上涨，疑似泄漏"。
    正确的读法是拿它做过程观察，结论由 memory_leak_check 给。

    Args:
        bundle_name: 应用包名
        duration: 采样总时长（秒）
        interval: 采样间隔（秒）
        workload: 每次采样前是否驱动一次操作
    """
    r = core.sample_memory_series(bundle_name, samples=max(2, int(duration)),
                                  interval=interval, workload=workload)
    if "error" in r:
        return {"ok": False, "error": r["error"], "中文说明": "❌ 采集失败"}
    return {
        "ok": True,
        "PSS序列_kB": r.get("pss_samples_kb"),
        "ArkTS序列_kB": r.get("ark_ts_samples_kb"),
        "Native序列_kB": r.get("native_samples_kb"),
        "有效样本": r.get("valid_samples"),
        # 防误读：结论随数据一起返回，而不是只写在文档里——
        # 模型（和人）看的是返回值，不是源码注释。
        "是否触发GC": False,
        "怎么读这条曲线": "本序列未触发 GC，单调上升属正常（垃圾尚未回收），"
                          "不能据此判泄漏；是否泄漏请用 memory_leak_check 看回落点是否逐次抬升",
        "raw": r,
    }


if __name__ == "__main__":
    mcp.run()
