"""CPU 占用变化 —— MCP 薄封装（第三层）。

逻辑全在 cpu_core.py。
启动：python cpu_server.py
"""

try:                                    # 兼容 fastmcp / 官方 SDK v1 / v2
    from fastmcp import FastMCP
except ImportError:
    try:
        from mcp.server.fastmcp import FastMCP
    except ImportError:                 # mcp 2.x：FastMCP 改名为 MCPServer
        from mcp.server.mcpserver import MCPServer as FastMCP

import cpu_core as core

mcp = FastMCP("harmony-cpu")


@mcp.tool()
def cpu_usage(bundle_name: str) -> dict:
    """获取应用 CPU 使用率单次快照（总量 / 用户态 / 内核态）。

    Args:
        bundle_name: 应用包名，如 com.example.app
    """
    r = core.get_cpu_usage(bundle_name)
    if "error" in r:
        return {"ok": False, "error": r["error"], "中文说明": "❌ 采集失败"}
    return {
        "ok": True,
        "总使用率": r.get("total"),
        "用户态": r.get("user"),
        "内核态": r.get("kernel"),
        "中文判定": core.judge_cpu(r.get("total", "")),
        "raw": r,
    }


@mcp.tool()
def cpu_trend(bundle_name: str, samples: int = 5, interval: int = 2) -> dict:
    """按间隔多次采样应用 CPU 使用率，返回序列与判定。

    Args:
        bundle_name: 应用包名
        samples: 采样次数
        interval: 采样间隔（秒）
    """
    r = core.get_cpu_trend(bundle_name, duration=max(1, samples) * interval,
                           interval=interval)
    if "error" in r:
        return {"ok": False, "error": r["error"], "中文说明": "❌ 采集失败"}
    judged = core.judge_cpu_samples(r["total_samples"])
    return {
        "ok": True,
        "总使用率序列": r.get("total_samples"),
        "用户态序列": r.get("user_samples"),
        "内核态序列": r.get("kernel_samples"),
        "中位占用_pct": judged.get("median"),
        "中文判定": judged.get("label"),
        "raw": {**r, **judged},
    }


@mcp.tool()
def cpu_assess(bundle_name: str, samples: int = 3, interval: int = 2) -> dict:
    """评估应用空闲时的 CPU 占用是否偏高（识别常驻轮询 / 空转）。

    Args:
        bundle_name: 应用包名
        samples: 采样次数（建议 3）
        interval: 采样间隔（秒）
    """
    r = core.measure_cpu(bundle_name, samples=samples, interval=interval)
    if "error" in r:
        return {"ok": False, "error": r["error"], "中文说明": "❌ 采集失败"}
    return {
        "ok": True,
        "中位占用_pct": r.get("median"),
        "峰值占用_pct": r.get("peak"),
        "severity": r.get("severity"),
        "中文判定": r.get("label"),
        "原始序列": r.get("total_samples"),
        "raw": r,
    }


if __name__ == "__main__":
    mcp.run()
