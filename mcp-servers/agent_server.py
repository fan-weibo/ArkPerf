"""统一体验分析 Agent —— MCP 薄封装（第三层）。

逻辑全在 agent_core.py。这是**面向赛题第 3、4 条的主入口**：
一条调用完成「采集 → 分析 → 结构化诊断报告」。

启动：python agent_server.py
"""

try:                                    # 兼容 fastmcp / 官方 SDK v1 / v2
    from fastmcp import FastMCP
except ImportError:
    try:
        from mcp.server.fastmcp import FastMCP
    except ImportError:                 # mcp 2.x：FastMCP 改名为 MCPServer
        from mcp.server.mcpserver import MCPServer as FastMCP

import agent_core as core

mcp = FastMCP("harmony-experience-agent")


@mcp.tool()
def experience_analysis(bundle_name: str, ability_name: str = "",
                        modules: str = "", quick: bool = False,
                        scene: str = "") -> dict:
    """对指定 OpenHarmony 应用做完整体验分析，并生成结构化诊断报告。

    流程：前置检查 → 采集各指标 → 规则引擎归并问题 → 输出报告。
    报告包含：测试场景 / 关键指标 / 异常现象 / 可能原因 / 优化建议。

    Args:
        bundle_name: 应用包名，如 com.example.app
        ability_name: 指定 Ability，留空自动识别
        modules: 逗号分隔的模块，可选 cold_start/memory/cpu/jank；留空表示全部
        quick: 快速模式（减少采样次数，约 2 分钟；false 约 6 分钟）
        scene: 场景直达参数（透传 want.parameters.scene）。应用支持场景
            路由时（如 PerfLab 的 leak/block），内存/卡顿测量会直达对应
            页面，测出该场景下的真实表现；冷启动始终走默认入口。
    """
    mods = [m.strip() for m in modules.split(",") if m.strip()] or None
    r = core.run_analysis(bundle_name, ability_name=ability_name,
                          modules=mods, quick=quick, scene=scene)
    if "error" in r:
        return {"ok": False, "error": r["error"], "中文说明": "❌ 分析失败"}

    # MCP 返回里带上 markdown 全文，方便直接落盘成文档
    return {
        "ok": True,
        "包名": r["meta"]["bundle_name"],
        "Ability": r["meta"]["ability_name"],
        "耗时_s": r["meta"]["duration_s"],
        "健康度": {"good": "✅ 良好", "warning": "🟡 存在异常",
                   "bad": "🔴 问题严重"}.get(r["summary"]["health"], ""),
        "问题数": r["summary"]["issue_count"],
        "问题清单": [
            {"标题": i["title"], "严重度": i["severity"],
             "现象": i["evidence"], "可能原因": i["possible_causes"],
             "优化建议": i["suggestions"]}
            for i in r["issues"]
        ],
        "总体结论": r["summary"]["conclusion"],
        "报告Markdown": r["markdown"],
        "raw": {k: v for k, v in r.items() if k != "markdown"},
    }


@mcp.tool()
def save_experience_report(bundle_name: str, ability_name: str = "",
                           modules: str = "", quick: bool = False,
                           scene: str = "", out_dir: str = "reports",
                           tag: str = "") -> dict:
    """执行体验分析并把报告落盘为 Markdown + JSON（便于优化前后对比归档）。

    Args:
        bundle_name: 应用包名
        ability_name: 指定 Ability，留空自动识别
        modules: 逗号分隔的模块；留空表示全部
        quick: 快速模式
        scene: 场景直达参数（want.parameters.scene），如 PerfLab 的 leak/block
        out_dir: 输出目录（默认 reports/，相对当前工作区）
        tag: 文件名后缀，如 baseline / optimized
    """
    mods = [m.strip() for m in modules.split(",") if m.strip()] or None
    r = core.run_analysis(bundle_name, ability_name=ability_name,
                          modules=mods, quick=quick, scene=scene)
    if "error" in r:
        return {"ok": False, "error": r["error"], "中文说明": "❌ 分析失败"}

    paths = core.save_report(r, out_dir=out_dir, tag=tag)
    if not paths.get("ok"):
        # 分析跑完了但报告没落盘：必须说出来。否则用户以为有归档，
        # 下次做优化前后对比时才发现基线是空的。
        return {"ok": False, "error": paths.get("error"),
                "中文说明": "❌ 报告落盘失败（分析结果本身有效，可重跑归档）"}
    return {
        "ok": True,
        "报告Markdown": paths["markdown"],
        "报告JSON": paths["json"],
        "问题数": r["summary"]["issue_count"],
        "总体结论": r["summary"]["conclusion"],
    }


@mcp.tool()
def list_indicators() -> dict:
    """列出本 Agent 支持的体验指标与对应可识别的问题。"""
    return {
        "ok": True,
        "指标": [{"id": m, "名称": core.MODULE_LABELS[m]}
                 for m in core.MODULES],
        "可识别问题": [
            {"id": k, "名称": v["title"]} for k, v in core.ISSUE_RULES.items()
        ],
        "说明": "冷启动耗时为绝对时间量，受环境影响，建议结合前后对比使用；"
                "内存/卡顿为状态与模式判定，稳定性更好。",
    }


if __name__ == "__main__":
    mcp.run()
