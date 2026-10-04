"""冷启动测量 —— 开发阶段测试程序（第一层）。

mcp-servers/ 分三层：*_core.py 公共后端逻辑 / *_server.py 给 agent 调的
MCP 薄封装（第三层，产品入口）/ *_test.py 开发阶段测试程序（本文件）。

**本文件不是产品交付路径**：给 agent 用的入口是 cold_start_server.py。
它用来在开发时手工跑一遍、直接看终端输出，绕过 MCP 协议，改完 core 后最快验证。
基线存取现在也在 cold_start_core.py 里（load_baseline / save_baseline）。

用法：
  python cold_start_test.py                                    # 测量(默认包名/10次)，保存到默认基线
  python cold_start_test.py measure [bundle] [ability] [runs] [-o out.json] [--no-anchor]
                                                                # 测量并保存会话
  python cold_start_test.py compare [baseline.json] [bundle] [ability] [runs] [--no-anchor]
                                                                # 先测量当前，再与基线对比
  python cold_start_test.py sandwich <base1.json> <new.json> <base2.json>
                                                                # 三明治法：判定环境是否漂移

锚点归一化（默认开启）：
  默认用系统设置 App 作为「锚点」，每轮与目标应用一起测量，
  用 target/anchor 的比值作为观测单位。这样即使**不重启模拟器**，
  环境整体变快/变慢也会被约掉，跨会话对比依然可信。
  --no-anchor  关闭锚点（仅测目标，速度快一倍，但没有归一化能力）
  --anchor <bundle>:<ability>  自定义锚点应用

推荐工作流（模拟器常驻，不重启）：
  1) python cold_start_test.py measure -o base1.json     # 改动前
  2) 改代码 → 重新编译安装
  3) python cold_start_test.py compare base1.json -o new.json
  4) 若对结论存疑，再回滚 + 复测一次做三明治校验
"""

import json
import os
import sys

from cold_start_core import (
    compare_sessions,
    judge_cold_start,
    measure_cold_start,
    sandwich_check,
)

DEFAULT_BUNDLE = "com.huawei.hmos.settings"
DEFAULT_ABILITY = "com.huawei.hmos.settings.MainAbility"
DEFAULT_RUNS = 10
DEFAULT_BASELINE = "cold_start_baseline.json"

# 默认锚点：系统设置 App，长期稳定、随系统版本固定
DEFAULT_ANCHOR_BUNDLE = "com.huawei.hmos.settings"
DEFAULT_ANCHOR_ABILITY = "com.huawei.hmos.settings.MainAbility"


# ------------------------------------------------------------
# 打印
# ------------------------------------------------------------
def print_stats(result: dict) -> None:
    st = result.get("stats", {})
    dev = result.get("device", {})
    env = (result.get("env") or {}).get("start") or {}
    host = env.get("host") or {}

    print(f"\n=== 冷启动测量 ===")
    print(f"包名     : {result.get('bundle_name')}")
    print(f"Ability  : {result.get('ability_name')}")
    print(f"设备     : {dev.get('model')} / {dev.get('software')} / API {dev.get('api')}")
    print(f"环境     : 主机CPU {host.get('cpu_percent')}% / 内存 {host.get('mem_percent')}%"
          f" / 模拟器uptime {env.get('device_uptime_s')}s")
    print(f"\n原始序列 : {result.get('all')} ms")
    print(f"StartMode: {result.get('start_modes')}")

    if not st.get("n"):
        print("\n未采集到有效冷启动样本。")
        return

    print(f"\n有效样本 : n={st['n']}"
          f"（非冷启动被剔除 {result.get('non_cold_count', 0)} 次）")

    print(f"\n--- 统计 (ms) ---")
    print(f"  中位数 P50 : {st['median']:<10} {judge_cold_start(st['median'])}")
    print(f"  均值       : {st['mean']}")
    print(f"  P10 / P90  : {st['p10']} / {st['p90']}")
    print(f"  最小 / 最大: {st['min']} / {st['max']}")
    print(f"  极差       : {st['range']}")
    print(f"  标准差     : {st['stdev']}")
    print(f"  稳健噪声 σ : {st['robust_sigma']}  (1.4826*MAD，比标准差抗异常值)")

    wt = result.get("wait_stats", {})
    if wt.get("n"):
        print(f"  WaitTime P50: {wt['median']}")

    # 锚点与归一化
    anchor = result.get("anchor")
    norm = result.get("norm_stats") or {}
    if anchor and norm.get("n"):
        ast = result.get("anchor_stats") or {}
        print(f"\n--- 锚点归一化 ---")
        print(f"  锚点应用   : {anchor.get('bundle')}")
        print(f"  锚点 P50   : {ast.get('median')} ms")
        print(f"  比值 P50   : {norm.get('median')}  (target/anchor，跨会话可比)")
        print(f"  比值序列   : {result.get('ratios')}")


def print_comparison(cmp: dict) -> None:
    print(f"\n=== 改动前后对比 ===")
    if not cmp.get("ok"):
        print(f"无法对比：{cmp.get('reason')}")
        return

    if cmp.get("normalized"):
        print(f"对比口径 : 锚点归一化（比值，与环境整体快慢无关）")
        nb = (cmp.get("norm") or {}).get("baseline_ratio")
        nc = (cmp.get("norm") or {}).get("current_ratio")
        print(f"基线比值 : {nb}")
        print(f"当前比值 : {nc}")
    else:
        print(f"对比口径 : 原始耗时（未归一化）")

    print(f"基线 P50 : {cmp['baseline_median']} ms   (n={cmp['n_baseline']})")
    print(f"当前 P50 : {cmp['current_median']} ms   (n={cmp['n_current']})")
    print(f"差异     : {cmp['delta']} {cmp['unit']} ({cmp['pct']}%)")
    print(f"p 值     : {cmp['p_value']}   (Mann-Whitney U 检验, α={cmp['alpha']})")

    if cmp.get("normalized"):
        nmdd = (cmp.get("norm") or {}).get("mdd_pct")
        if nmdd is not None:
            print(f"可检测差异: {nmdd}%   (归一化口径下，小于此值的改动无法被可靠识别)")
    else:
        print(f"噪声下限 : {cmp['noise_floor']} ms")
        print(f"可检测差异: {cmp['mdd']} ms   (小于此值的改动无法被可靠识别)")

    if cmp.get("raw_pct") is not None and cmp.get("normalized"):
        print(f"（参考）原始耗时差异: {cmp['raw_delta']} ms ({cmp['raw_pct']}%)")

    if cmp.get("anchor_drift") is not None:
        print(f"锚点漂移 : {(cmp['anchor_drift'] - 1) * 100:+.1f}%   (环境整体快慢)")

    print(f"判定     : {cmp['verdict']}")

    ed = cmp.get("env_delta") or {}
    if ed:
        print(f"环境变化 : CPU {ed.get('cpu_percent')}pp / 内存 {ed.get('mem_percent')}pp"
              f" / 模拟器重启 {ed.get('emulator_restarted')}")

    if cmp.get("warnings"):
        print("注意     :")
        for w in cmp["warnings"]:
            print(f"  - {w}")


def print_sandwich(res: dict) -> None:
    print(f"\n=== 三明治判定（环境漂移自检）===")
    print(f"\n[1] 环境稳定性：base1 vs base2")
    st = res["stability"]
    if st.get("ok"):
        print(f"    Δ={st['delta']} {st.get('unit')}  p={st['p_value']}  "
              f"{'无显著差异 ✅ 环境稳定' if not st['significant'] else '有显著差异 ⛔ 环境漂移'}")
    else:
        print(f"    无法判定：{st.get('reason')}")

    for tag, key in (("[2] 改动效果：base1 vs new", "effect_a"),
                     ("[3] 改动效果：base2 vs new", "effect_b")):
        e = res[key]
        print(f"\n{tag}")
        if e.get("ok"):
            print(f"    Δ={e['delta']} {e.get('unit')}  p={e['p_value']}  {e['verdict']}")
        else:
            print(f"    无法判定：{e.get('reason')}")

    print(f"\n>>> 最终结论：{res['verdict']}")


# ------------------------------------------------------------
# 子命令
# ------------------------------------------------------------
def _run_measure(bundle, ability, runs, anchor) -> dict:
    result = measure_cold_start(
        bundle_name=bundle,
        ability_name=ability,
        runs=runs,
        anchor=anchor,
        on_progress=print,
    )
    if "error" in result:
        print(f"\n{result['error']}")
    return result


def do_measure(bundle, ability, runs, out_path=None, anchor=None) -> dict:
    if anchor and bundle == anchor[0]:
        print("⚠️ 目标应用与锚点应用相同，归一化比值将恒 ≈1（仅适用于锚点噪声自标定）。")
    result = _run_measure(bundle, ability, runs, anchor)
    if "error" in result:
        return result

    print_stats(result)

    out_path = out_path or DEFAULT_BASELINE
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump(result, f, ensure_ascii=False, indent=2)
    print(f"\n会话已保存 -> {os.path.abspath(out_path)}")
    return result


def do_compare(baseline_path, bundle, ability, runs, out_path=None, anchor=None) -> None:
    if not os.path.exists(baseline_path):
        print(f"找不到基线文件：{baseline_path}")
        print("请先执行一次 measure 生成基线。")
        return

    with open(baseline_path, "r", encoding="utf-8") as f:
        baseline = json.load(f)

    print(f"已载入基线：{baseline_path}")
    print(f"  基线 P50 = {baseline.get('stats', {}).get('median')} ms")

    bb, ba = baseline.get("bundle_name"), baseline.get("ability_name")
    if bb and (bundle, ability) != (bb, ba):
        print(f"\n⛔ 口径断裂：基线测量对象是 {bb} / {ba}，")
        print(f"   当前请求却是 {bundle} / {ability}。A/B 对比必须针对同一应用，已拒绝执行。")
        print("   （compare 不传包名时会自动沿用基线对象；如确需换对象，请先重新 measure 建基线）")
        return

    if anchor and bundle == anchor[0]:
        print("⚠️ 目标应用与锚点应用相同，归一化比值将恒 ≈1，归一化口径不适用（建议 --no-anchor）。")

    current = _run_measure(bundle, ability, runs, anchor)
    if "error" in current:
        return

    print_stats(current)
    print_comparison(compare_sessions(baseline, current))

    if out_path:
        with open(out_path, "w", encoding="utf-8") as f:
            json.dump(current, f, ensure_ascii=False, indent=2)
        print(f"\n当前会话已保存 -> {os.path.abspath(out_path)}")


def do_sandwich(base1_path, new_path, base2_path) -> None:
    for p in (base1_path, new_path, base2_path):
        if not os.path.exists(p):
            print(f"找不到会话文件：{p}")
            return

    def _load(p):
        with open(p, "r", encoding="utf-8") as f:
            return json.load(f)

    res = sandwich_check(_load(base1_path), _load(new_path), _load(base2_path))
    print_sandwich(res)


# ------------------------------------------------------------
# 参数解析
# ------------------------------------------------------------
def _pop_opt(argv, flag):
    """取出 `-o <value>`，返回 (value, 剩余 argv)"""
    if flag in argv:
        i = argv.index(flag)
        val = argv[i + 1] if i + 1 < len(argv) else None
        return val, argv[:i] + argv[i + 2:]
    return None, argv


def _parse_anchor(argv):
    """解析锚点参数，返回 (anchor, 剩余 argv)。

    --no-anchor            关闭锚点
    --anchor               使用默认锚点（系统设置）
    --anchor bundle:ability 自定义锚点
    """
    if "--no-anchor" in argv:
        return None, [a for a in argv if a != "--no-anchor"]

    if "--anchor" in argv:
        i = argv.index("--anchor")
        nxt = argv[i + 1] if i + 1 < len(argv) else None
        if nxt and not nxt.startswith("-"):
            rest = argv[:i] + argv[i + 2:]
            if ":" in nxt:
                ab, aa = nxt.split(":", 1)
                return (ab, aa), rest
            return (DEFAULT_ANCHOR_BUNDLE, DEFAULT_ANCHOR_ABILITY), rest
        return (DEFAULT_ANCHOR_BUNDLE, DEFAULT_ANCHOR_ABILITY), argv[:i] + argv[i + 1:]

    # 默认开启锚点
    return (DEFAULT_ANCHOR_BUNDLE, DEFAULT_ANCHOR_ABILITY), argv


def _bundle_from_baseline(baseline_path):
    """compare 未显式传包名时，从基线 JSON 恢复测量对象（A/B 对比必须与基线同口径）。"""
    try:
        with open(baseline_path, "r", encoding="utf-8") as f:
            d = json.load(f)
        bundle = d.get("bundle_name")
        ability = d.get("ability_name")
        if bundle and ability:
            print(f"未指定包名，沿用基线测量对象：{bundle} / {ability}")
            return bundle, ability
    except (OSError, ValueError):
        pass
    print(f"⚠️ 无法从 {baseline_path} 读取测量对象，回退默认：{DEFAULT_BUNDLE}")
    return DEFAULT_BUNDLE, DEFAULT_ABILITY


# ------------------------------------------------------------
# 入口
# ------------------------------------------------------------
def main():
    argv = sys.argv[1:]

    if not argv:
        anchor, _ = _parse_anchor([])
        do_measure(DEFAULT_BUNDLE, DEFAULT_ABILITY, DEFAULT_RUNS, anchor=anchor)
        return

    cmd = argv[0]
    rest = argv[1:]

    if cmd == "measure":
        out, rest = _pop_opt(rest, "-o")
        anchor, rest = _parse_anchor(rest)
        bundle = rest[0] if len(rest) > 0 else DEFAULT_BUNDLE
        ability = rest[1] if len(rest) > 1 else DEFAULT_ABILITY
        runs = int(rest[2]) if len(rest) > 2 else DEFAULT_RUNS
        do_measure(bundle, ability, runs, out_path=out, anchor=anchor)
        return

    if cmd == "compare":
        anchor, rest = _parse_anchor(rest)
        out, rest = _pop_opt(rest, "-o")
        baseline_path = rest[0] if len(rest) > 0 else DEFAULT_BASELINE
        bundle = rest[1] if len(rest) > 1 else None
        ability = rest[2] if len(rest) > 2 else None
        runs = int(rest[3]) if len(rest) > 3 else DEFAULT_RUNS
        if bundle is None or ability is None:
            bundle, ability = _bundle_from_baseline(baseline_path)
        do_compare(baseline_path, bundle, ability, runs, out_path=out, anchor=anchor)
        return

    if cmd == "sandwich":
        if len(rest) < 3:
            print("用法：python cold_start_test.py sandwich <base1.json> <new.json> <base2.json>")
            return
        do_sandwich(rest[0], rest[1], rest[2])
        return

    print(__doc__)


if __name__ == "__main__":
    main()
