"""内存泄漏检测 —— 开发阶段测试程序（第一层）。
（官方口径：先强制 GC，再看「锯齿 vs 阶梯」）

mcp-servers/ 分三层：*_core.py 公共后端逻辑 / *_server.py 给 agent 调的
MCP 薄封装（第三层，产品入口）/ *_test.py 开发阶段测试程序（本文件）。

**本文件不是产品交付路径**：给 agent 用的入口是 memory_server.py。
它用来在开发时手工跑一遍、直接看终端输出，绕过 MCP 协议。

用法：
  python memory_test.py                                   # 默认：5 个操作周期
  python memory_test.py [bundle] [cycles] [interval]       # 指定包名/周期数/周期间隔(秒)
  python memory_test.py --no-workload                      # 空闲采样（不驱动操作，不推荐）
  python memory_test.py --no-gc                            # 不强制 GC（回退到旧逻辑）

流程（每个周期）：
  强制 GC → 采样「回落点」 → 驱动操作 → 采样「操作后峰值」

判定：
  锯齿（回落点稳定）   → 正常
  阶梯（回落点逐次抬升）→ 泄漏

注意：强制 GC 依赖 `hidumper --mem-jsheap --gc`，**仅对 debug 签名应用有效**。
"""

import sys

from memory_core import get_memory_leak_report, get_memory_snapshot

DEFAULT_BUNDLE = "com.huawei.hmos.settings"
DEFAULT_CYCLES = 5
DEFAULT_INTERVAL = 1.0


def fmt(kb):
    return f"{kb} kB ({kb / 1024:.1f} MB)" if kb is not None else "N/A"


def print_snapshot(bundle):
    print("=== 内存快照 ===")
    snap = get_memory_snapshot(bundle)
    if "error" in snap:
        print(snap["error"])
        return False
    print(f"PID       : {snap['pid']}")
    print(f"PSS 总量  : {fmt(snap['pss_total_kb'])}")
    print(f"ArkTS 堆  : {fmt(snap['ark_ts_heap_kb'])}")
    print(f"Native 堆 : {fmt(snap['native_heap_kb'])}")
    return True


def print_judge(tag, j):
    if not j.get("n"):
        print(f"  {tag:10s}: 无有效周期")
        return
    print(f"  {tag:10s}: {j['label']}")
    print(f"  {'':10s}  回落点速率 {j['rate_per_min']:+.0f} kB/min"
          f" | 单调性 {j['mono_ratio']:.0%}"
          f" | 区间跨度 {j['range']} kB")


def main():
    argv = sys.argv[1:]
    workload, gc = True, True
    if "--no-workload" in argv:
        workload = False
        argv = [a for a in argv if a != "--no-workload"]
    if "--no-gc" in argv:
        gc = False
        argv = [a for a in argv if a != "--no-gc"]

    bundle = argv[0] if len(argv) > 0 else DEFAULT_BUNDLE
    cycles = int(argv[1]) if len(argv) > 1 else DEFAULT_CYCLES
    interval = float(argv[2]) if len(argv) > 2 else DEFAULT_INTERVAL

    if not print_snapshot(bundle):
        return

    print(f"\n=== 操作周期采样（{'驱动操作' if workload else '空闲'}"
          f"{' + 强制GC' if gc else ''}，{cycles} 个周期）===")

    report = get_memory_leak_report(
        bundle, cycles=cycles, interval=interval,
        warmup=1, workload=workload, gc=gc, on_progress=print,
    )
    if "error" in report:
        print(f"\n{report['error']}")
        return

    s = report["series"]
    print(f"\n--- 回落点序列（每周期 GC 后，官方「锯齿/阶梯」判据就看它）---")
    print(f"  ArkTS 堆   : {s['baseline_ark_ts']}")
    print(f"  PSS        : {s['baseline_pss']}")
    print(f"  周期时长   : {s['cycle_period_s']} s（实测中位数）")

    print(f"\n--- 操作后峰值序列 ---")
    print(f"  ArkTS 堆   : {s['peak_ark_ts']}")
    print(f"  单周期增量 : {report['per_cycle_delta_ark_ts']}")

    print(f"\n--- 趋势判定 ---")
    print_judge("ArkTS 堆", report["ark_ts"])
    print_judge("PSS", report["pss"])
    print_judge("Native 堆", report["native"])

    pattern_txt = {"sawtooth": "锯齿状（正常）",
                   "staircase": "阶梯式（泄漏）",
                   "watch": "缓慢抬升（关注）",
                   "unknown": "无法判定"}
    print(f"\n>>> 结论：{report['label']}")
    print(f"    形态 = {pattern_txt.get(report['pattern'], report['pattern'])}"
          f" ｜ 主判据 = {report['primary']}"
          f" ｜ 有效周期 {report['valid_cycles']}/{report['cycles']}")
    if report.get("gc_supported") is False:
        print(f"    强制 GC = 不可用（应用非 debug 签名）")
    elif report.get("gc"):
        print(f"    强制 GC = 已启用")

    for w in report["warnings"]:
        print(f"    注意：{w}")


if __name__ == "__main__":
    main()
