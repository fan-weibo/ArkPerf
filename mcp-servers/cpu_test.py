from cpu_core import (
    get_cpu_usage,
    judge_cpu,
    measure_cpu,
    get_pid,
)
import subprocess
import sys
import time


def ensure_running(bundle_name: str) -> str:
    """应用未运行时自动拉起，返回 pid。"""
    pid = get_pid(bundle_name)
    if pid:
        return pid
    subprocess.run(["hdc", "shell", "aa", "start", "-b", bundle_name],
                   capture_output=True, text=True, timeout=30)
    time.sleep(3)
    return get_pid(bundle_name)


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("-")]
    bundle = args[0] if args else "com.wandroid.harmonyStudy"
    samples = int(args[1]) if len(args) > 1 else 3

    print("=== CPU 使用率快照 ===")
    if not ensure_running(bundle):
        print(f"未找到进程 {bundle}，且自动拉起失败")
        return
    result = get_cpu_usage(bundle)
    if "error" in result:
        print(result["error"])
        return
    print(f"PID: {result['pid']}")
    print(f"总使用率: {result['total']}")
    print(f"  用户空间: {result['user']}")
    print(f"  内核空间: {result['kernel']}")
    print(f"  判定    : {judge_cpu(result['total'])}")

    print(f"\n=== CPU 使用率趋势（{samples} 次采样） ===")
    trend = measure_cpu(bundle, samples=samples, interval=2, on_progress=print)
    if "error" in trend:
        print(trend["error"])
        return
    print(f"总使用率序列: {trend['total_samples']}")
    print(f"用户空间序列: {trend['user_samples']}")
    print(f"内核空间序列: {trend['kernel_samples']}")
    print(f"中位占用    : {trend['median']}%   峰值: {trend['peak']}%")
    print(f"判定        : {trend['label']}")


if __name__ == "__main__":
    main()
