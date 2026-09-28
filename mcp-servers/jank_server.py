"""主线程卡顿 / 阻塞判定 —— MCP 薄封装（第三层）。

逻辑全在 jank_core.py。判定依据是**系统自身的卡顿判定**（hilog），只做计数。
启动：python jank_server.py
"""

try:                                    # 兼容 fastmcp / 官方 SDK v1 / v2
    from fastmcp import FastMCP
except ImportError:
    try:
        from mcp.server.fastmcp import FastMCP
    except ImportError:                 # mcp 2.x：FastMCP 改名为 MCPServer
        from mcp.server.mcpserver import MCPServer as FastMCP

import jank_core as core

mcp = FastMCP("harmony-jank")

_VERDICT_CN = {"clear": "✅ 未发现卡顿/阻塞",
               "blocked": "🔴 存在卡顿/阻塞",
               "unknown": "❓ 无法判定"}


@mcp.tool()
def blocking_check(bundle_name: str, rounds: int = 5) -> dict:
    """判定应用是否存在主线程卡顿 / 阻塞。

    做法：清空系统日志 → 驱动若干轮滑动制造负载 → 采集系统自身的卡顿判定
    （`JankFrameMonitor jank >= threshold`）并计数。

    注意：帧率类指标（SP_daemon -f）在部分模拟器镜像上恒为 0，本工具不依赖它。

    Args:
        bundle_name: 应用包名，如 com.example.app
        rounds: 滑动轮数（每轮上下各一次滑动，建议 5）
    """
    r = core.get_blocking_report(bundle_name, rounds=rounds)
    if "error" in r:
        return {"ok": False, "error": r["error"], "中文说明": "❌ 采集失败"}

    f = r.get("features", {})
    verdict = r.get("verdict", "unknown")
    return {
        "ok": True,
        "是否卡顿": verdict,
        "中文判定": _VERDICT_CN.get(verdict, ""),
        "严重程度": r.get("severity"),
        "结论详情": r.get("label"),
        "卡顿判定次数": r.get("count"),
        "卡顿频次_次每分": r.get("rate_per_min"),
        "卡顿帧峰值": f.get("peak_jank_frames"),
        "注入操作次数": r.get("input_events"),
        "告警": r.get("warnings"),
        "raw": r,
    }


@mcp.tool()
def blocking_evidence(bundle_name: str, rounds: int = 5,
                      keyword: str = "jank") -> dict:
    """返回卡顿判定的原始日志证据（便于人工复核结论）。

    Args:
        bundle_name: 应用包名
        rounds: 滑动轮数
        keyword: hilog 过滤关键字，默认 jank
    """
    r = core.get_blocking_report(bundle_name, rounds=rounds)
    if "error" in r:
        return {"ok": False, "error": r["error"], "中文说明": "❌ 采集失败"}
    return {
        "ok": True,
        "日志来源": r.get("log_filter"),
        "证据行": r.get("evidence_lines"),
        "中文判定": r.get("label"),
    }


if __name__ == "__main__":
    mcp.run()
