"""冷启动耗时 —— MCP 薄封装（第三层）。

只做「参数透传 + 中文标签」，逻辑全在 cold_start_core.py。
启动：python cold_start_server.py
"""

import json
import os

try:                                    # 兼容 fastmcp / 官方 SDK v1 / v2
    from fastmcp import FastMCP
except ImportError:
    try:
        from mcp.server.fastmcp import FastMCP
    except ImportError:                 # mcp 2.x：FastMCP 改名为 MCPServer
        from mcp.server.mcpserver import MCPServer as FastMCP

import cold_start_core as core

mcp = FastMCP("harmony-cold-start")

DEFAULT_BASELINE = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                "cold_start_baseline.json")


def _load(path):
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def _dump(obj, path):
    with open(path, "w", encoding="utf-8") as f:
        json.dump(obj, f, ensure_ascii=False, indent=2)


@mcp.tool()
def cold_start_measure(bundle_name: str, ability_name: str = "",
                       runs: int = 10, anchor: bool = True,
                       baseline_path: str = "") -> dict:
    """测量应用冷启动耗时并存为基线，供改动后对比。

    Args:
        bundle_name: 应用包名，如 com.example.app
        ability_name: Ability 名；留空则自动识别
        runs: 计入统计的冷启动次数（建议 10~40，越大越灵敏）
        anchor: 是否启用锚点归一化（抵消模拟器环境漂移，建议开启）
        baseline_path: 基线保存路径，留空用默认文件
    """
    if not ability_name:
        ability_name = "EntryAbility"
    use_anchor = None
    if anchor and bundle_name != core.ANCHOR_BUNDLE:
        use_anchor = (core.ANCHOR_BUNDLE, core.ANCHOR_ABILITY)

    r = core.measure_cold_start(bundle_name, ability_name, runs=runs,
                                warmup=1, anchor=use_anchor)
    if "error" in r:
        return {"ok": False, "error": r["error"], "中文说明": "❌ 采集失败"}

    path = baseline_path or DEFAULT_BASELINE
    _dump(r, path)
    med = r["stats"]["median"]
    return {
        "ok": True,
        "中位耗时_ms": med,
        "P90_ms": r["stats"].get("p90"),
        "极差_ms": r["stats"].get("range"),
        "有效样本": r["stats"].get("n"),
        "非冷启动剔除": r.get("non_cold_count", 0),
        "中文判定": core.judge_cold_start(int(med)),
        "基线已保存": path,
        "raw": r,
    }


@mcp.tool()
def cold_start_compare(bundle_name: str, ability_name: str = "",
                       runs: int = 10, anchor: bool = True,
                       baseline_path: str = "") -> dict:
    """重新测量并与已保存的基线对比，给出是否「显著改善/退步」的结论。

    这是「优化前 → 优化后」对比的标准入口。
    """
    if not ability_name:
        ability_name = "EntryAbility"
    path = baseline_path or DEFAULT_BASELINE
    if not os.path.exists(path):
        return {"ok": False, "中文说明": "❌ 未找到基线，请先运行 cold_start_measure"}

    baseline = _load(path)
    use_anchor = None
    if anchor and bundle_name != core.ANCHOR_BUNDLE:
        use_anchor = (core.ANCHOR_BUNDLE, core.ANCHOR_ABILITY)

    r = core.measure_cold_start(bundle_name, ability_name, runs=runs,
                                warmup=1, anchor=use_anchor)
    if "error" in r:
        return {"ok": False, "error": r["error"], "中文说明": "❌ 采集失败"}

    cmp = core.compare_sessions(baseline, r)
    return {
        "ok": True,
        "基线中位_ms": cmp.get("baseline_median"),
        "当前中位_ms": cmp.get("current_median"),
        "差异_ms": cmp.get("delta"),
        "差异百分比": f"{cmp.get('pct')}%",
        "p值": cmp.get("p_value"),
        "最小可检测差异": cmp.get("mdd"),
        "中文判定": cmp.get("verdict"),
        "告警": cmp.get("warnings"),
        "raw": cmp,
    }


if __name__ == "__main__":
    mcp.run()
