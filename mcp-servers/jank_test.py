"""主线程卡顿 / 阻塞判定 —— 可执行入口（CLI）。

只回答一个问题：**有没有卡顿 / 阻塞**。

用法：
  python jank_test.py [bundle] [rounds] [--gap S] [--velocity V] [--no-clear]

示例：
  python jank_test.py                               # 默认应用 + 5 轮
  python jank_test.py com.example.app 8             # 指定应用 + 8 轮
  python jank_test.py com.example.app 8 --velocity 800
"""

import sys

from jank_core import (
    DEFAULT_GAP_S, DEFAULT_VELOCITY, DEFAULT_BUNDLE_FALLBACK,
    get_blocking_report,
)


def parse_args(argv):
    opts = {"rounds": None, "gap": DEFAULT_GAP_S,
            "velocity": DEFAULT_VELOCITY, "clear": True}
    positional = []
    i = 0
    while i < len(argv):
        a = argv[i]
        if a == "--no-clear":
            opts["clear"] = False
        elif a in ("--gap", "--velocity", "--rounds") and i + 1 < len(argv):
            i += 1
            try:
                opts[a.lstrip("-")] = (float(argv[i]) if a == "--gap"
                                       else int(float(argv[i])))
            except ValueError:
                pass
        elif a.startswith("-"):
            pass
        else:
            positional.append(a)
        i += 1
    return positional, opts


def main():
    positional, opts = parse_args(sys.argv[1:])

    bundle = positional[0] if positional else DEFAULT_BUNDLE_FALLBACK
    rounds = opts["rounds"]
    if rounds is None:
        rounds = int(float(positional[1])) if len(positional) > 1 else 5

    print(f"目标应用 : {bundle}")
    print(f"负载     : {rounds} 轮滑动（间隔 {opts['gap']}s，"
          f"velocity={opts['velocity']}）")
    print("-" * 60)

    rep = get_blocking_report(
        bundle, rounds=rounds, clear_log=opts["clear"],
        gap=opts["gap"], velocity=opts["velocity"],
        on_progress=lambda m: print("  " + m),
    )
    print("-" * 60)

    if "error" in rep:
        print(f"错误：{rep['error']}")
        return

    f = rep["features"]
    print(f"PID          : {rep['pid']}")
    print(f"测试时长     : {rep['window']['duration_s']} s")
    print(f"注入操作     : {rep['input_events']} 次")
    print(f"日志来源     : {rep['log_filter']}")
    print(f"卡顿判定     : jank≥threshold {f['threshold_events']} 条 / "
          f"jank frames 上报 {f['frame_reports']} 条")
    if f["peak_jank_frames"]:
        print(f"峰值卡顿帧   : {f['peak_jank_frames']} 帧")
    print()
    print(f">>> 判定：{rep['label']}")
    print(f"    结果码：verdict={rep['verdict']}  severity={rep['severity']}"
          f"  count={rep['count']}")
    if rep["basis"]:
        print(f"    依据  ：{rep['basis']}")

    for w in rep["warnings"]:
        print(f"    警告  ：{w}")

    if rep["evidence_lines"]:
        print()
        print(f"证据（最近 {len(rep['evidence_lines'])} 条）：")
        for ln in rep["evidence_lines"]:
            print("  " + ln.strip())


if __name__ == "__main__":
    main()
