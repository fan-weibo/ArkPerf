"""统一体验分析 Agent —— 开发阶段测试程序（第一层）。

mcp-servers/ 分三层：
  *_core.py    公共后端逻辑
  *_server.py  给 agent 调的 MCP 薄封装（第三层，产品入口）
  *_test.py    开发阶段测试程序（本文件）

也就是说：**本文件不是产品交付路径**。给 agent 用的入口是 agent_server.py。
它存在的意义是开发时手工跑一遍、直接看终端输出——不用起 MCP 服务、不用走协议，
改了 core 之后能最快地验证采集和分析对不对。

一条命令完成：采集 → 分析 → 生成结构化报告。

用法：
  python agent_test.py <包名> [选项]

选项：
  --ability <名>       指定 Ability（默认自动识别，失败回退 EntryAbility）
  --modules a,b,c      只跑指定模块（cold_start/memory/cpu/jank）
  --quick              快速模式（减少采样次数，用于联调）
  --scene <名>         场景直达（透传 want.parameters.scene，如 leak/block）
  --out <目录>         报告输出目录（默认 reports/）
  --tag <名>           报告文件名后缀，如 baseline / optimized

示例：
  python agent_test.py com.example.app --quick
  python agent_test.py com.example.app --tag baseline
  python agent_test.py com.example.app --modules cold_start,memory
  python agent_test.py com.example.perflab --scene leak --tag baseline-leak
"""

import sys

from agent_core import MODULES, MODULE_LABELS, run_analysis, save_report


def parse_args(argv):
    opts = {"ability": "", "modules": list(MODULES), "quick": False,
            "scene": "", "out": "reports", "tag": ""}
    positional = []
    i = 0
    while i < len(argv):
        a = argv[i]
        if a == "--quick":
            opts["quick"] = True
        elif a in ("--ability", "--modules", "--scene", "--out", "--tag") \
                and i + 1 < len(argv):
            i += 1
            val = argv[i]
            if a == "--modules":
                opts["modules"] = [m.strip() for m in val.split(",") if m.strip()]
            else:
                opts[a.lstrip("-")] = val
        elif a.startswith("-"):
            pass
        else:
            positional.append(a)
        i += 1
    return positional, opts


def main():
    positional, opts = parse_args(sys.argv[1:])
    if not positional:
        print(__doc__)
        print("可选模块：",
              "、".join(f"{m}({MODULE_LABELS[m]})" for m in MODULES))
        return

    bundle = positional[0]
    print("=" * 72)
    print(f"OpenHarmony 应用体验分析  ——  {bundle}")
    print(f"模块：{'、'.join(opts['modules'])}    "
          f"模式：{'快速' if opts['quick'] else '标准'}    "
          f"场景：{opts['scene'] or '默认入口'}")
    print("=" * 72)

    report = run_analysis(
        bundle, ability_name=opts["ability"], modules=opts["modules"],
        quick=opts["quick"], scene=opts["scene"],
        on_progress=lambda m: print("  " + m),
    )
    print("-" * 72)

    if "error" in report:
        print("错误：", report["error"])
        return

    meta = report["meta"]
    print(f"Ability  : {meta['ability_name']}")
    print(f"耗时     : {meta['duration_s']} s")
    print()
    print("【场景指标】")
    for sc in report["scenarios"]:
        mark = {"ok": "✅", "issue": "⚠️", "error": "❌",
                "unknown": "❓"}.get(sc["status"], "?")
        print(f"  {mark} {sc['name']}")
        for k, v in sc["metrics"].items():
            print(f"      {k}: {v}")
        if sc["note"]:
            print(f"      备注: {sc['note']}")

    print()
    print("【问题清单】")
    if not report["issues"]:
        print("  未发现达到阈值的问题")
    for idx, iss in enumerate(report["issues"], 1):
        print(f"  {idx}. [{iss['severity']}] {iss['title']} —— {iss['evidence']}")
        for c in iss["possible_causes"][:2]:
            print(f"       可能原因：{c}")
        for s in iss["suggestions"][:2]:
            print(f"       优化建议：{s}")

    print()
    print("【总体结论】")
    print("  " + report["summary"]["conclusion"])

    paths = save_report(report, out_dir=opts["out"], tag=opts["tag"])
    print()
    if not paths.get("ok"):
        # 退出码非零：脚本化调用（CI / 批处理）时不能"看起来成功了"
        print(f"❌ 报告落盘失败：{paths.get('error')}")
        sys.exit(1)
    print(f"报告已保存：\n  {paths['markdown']}\n  {paths['json']}")


if __name__ == "__main__":
    main()
