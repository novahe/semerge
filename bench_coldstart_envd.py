#!/usr/bin/env python3
"""bench_coldstart_envd.py — openclaw 冷启动 → envd 首命令(true) 事务基准。

场景(2026-11-03 新增):e2b create 一个沙箱(**双盘模式**:vda 只读 base erofs
+ vdb RW overlay diff;默认 openclaw32-e2b-snp 冷模板,overlay 32GiB,与产线
overlay-32G 形态一致,drop-caches=true)+ 调用 envd 执行 `true` 返回成功
(exit 0)= 完成一次;后续冷启动耗时优化以此为准。开班先做一次不计时的
磁盘形态检查(lsblk 应见 vda RO=1 + vdb RW),非双盘直接报错退出。

每轮拆解(冒烟实测口径,2026-11-03):
  t0 → Sandbox.create(模板) → t1   create_ms = SDK 建沙箱耗时。本栈 POST /sandboxes
                                    在 admission 即返回,SDK 2.x create 不等 envd——
                                    实测仅 ~5ms,冷启动等待不在此段
  t1 → commands.run(cmd) → t2       exec_ms  = envd 首命令耗时,含 envd 就绪等待+
                                    connect 建立——冷启动大头落在此段(冒烟 ~190ms)
  e2e_ms = t2 - t0                  事务完成 = cmd 成功(exit 0)= 用户场景口径
  launch_ms = GET /sandboxes/{sid} 的服务端精确 boot 耗时(需 timing patch,
              拿不到记 null,不影响样本)
  → kill + 清本地残留目录(复用 bench.py,防残留污染后续样本与磁盘)

已知坑(与 soak 一致):e2b SDK create/connect 的 timeout 参数语义是【沙箱存活期
endAt 秒数】且缺省只发 300s——样本即起即删 300s 足够,勿当客户端超时理解;
commands.run 的 timeout 才是客户端超时。

用法:
  python3 bench_coldstart_envd.py                             # openclaw32-e2b-snp × 10
  python3 bench_coldstart_envd.py -n 50 -c 10                 # 50 样本,并发 10
  python3 bench_coldstart_envd.py --breakdown                 # 拆 launch_wait vs envd 首命令
  python3 bench_coldstart_envd.py --template openclaw32-e2b-ws-template.id # 暖模板对照
  python3 bench_coldstart_envd.py --cmd 'uname -a'            # 换首命令

结果:repro-results/coldstart-envd-<ts>/{samples.jsonl,result.json}

优化抓手提示(参照 nova-e2e-anatomy 实测):首次请求经 proxy→MUX 有 ~90ms 量级
固定成本(二次 6-12ms),落在 exec 段的 SDK envd connect 上;--breakdown 的
exec_after_launch_ms ≈ 该固定成本 + envd 真实执行,launch_wait_ms ≈ 服务端
启动/恢复耗时。两段分别对应数据面与 runtime 侧的优化方向。
"""
import argparse
import json
import os
import re
import statistics
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime

import e2b_env

e2b_env.load()
e2b_env.require("E2B_DOMAIN", "E2B_API_KEY", "SSL_CERT_FILE")

import bench  # noqa: E402  复用 _delete_sandbox/cleanup_sandbox_directories/wait_launch_ms
from e2b import Sandbox  # noqa: E402

PARENT = os.path.dirname(os.path.abspath(__file__))
DEFAULT_TEMPLATE_FILE = "openclaw32-e2b-snp-template.id"  # 双盘:vda RO base + vdb 32G RW overlay
CONDUCTOR_LOG = "/root/sandbox/nova-work/var/lib/logs/conductor.log"


def _conductor_sid_lines(sid):
    """grep 整个 conductor.log 取该 sid 的行。conductor 自身写盘有秒级缓冲,
    增量偏移读会漏段;整文件 grep(~14MB)每次仅几十 ms,稳。"""
    try:
        r = subprocess.run(["grep", "-F", sid, CONDUCTOR_LOG],
                           capture_output=True, text=True, timeout=30)
        return r.stdout.splitlines()
    except Exception:
        return []


def _dur_to_ms(value, unit):
    v = float(value)
    return v / 1000.0 if unit == "µs" else (v * 1000.0 if unit == "s" else v)


def server_phases(sid, since, until):
    """样本完成后只读抽取该 sid 的服务端分阶段耗时(不影响事务计时)。

    conductor.log『sandbox launch phase』给 node-ctl 侧各段与 runtime_ready/envd_init;
    journal 的 sandbox-ctl 微秒行给 VM resume 时点与『restore notify acked』(guest 侧
    sandbox-init 握手)。阶段口径:
      node_ctl_ms       = admission + runner_commit + prepare(含 artifact/网络)
      sandbox_init_ms   = restore notify ack 时长(VM resume→guest 握手完成)
      sandbox_ctl_ms    = runtime_ready − sandbox_init(runner 侧:进程起/manifest/
                          CH spawn/uffd/vhost;与 node-ctl prepare 部分并行)
      kernel_init       = 快照恢复路径下不存在(内核不重新引导,vCPU 直接 resume)
      envd_ms           = envd_init_duration
    """
    out = {}
    phases = {}
    for line in _conductor_sid_lines(sid):
        m = re.search(r'phase=(\w+) duration=([\d.]+)(µs|ms|s)', line)
        if m:
            phases[m.group(1)] = _dur_to_ms(m.group(2), m.group(3))
            continue
        m = re.search(r'artifact_prepare_handoff_duration=([\d.]+)(µs|ms|s)', line)
        if m:
            phases["artifact_prepare"] = _dur_to_ms(m.group(1), m.group(2))
            continue
        m = re.search(r'starting_total_duration=([\d.]+)(µs|ms|s)', line)
        if m:
            phases["terminal_total"] = _dur_to_ms(m.group(1), m.group(2))
    if phases:
        out["phases"] = {k: round(v, 1) for k, v in phases.items()}
        out["node_ctl_ms"] = round(phases.get("admission_duration", 0)
                                   + phases.get("runner_commit_duration", 0)
                                   + phases.get("prepare_duration", 0), 1)
        out["envd_ms"] = round(phases.get("envd_init_duration", 0), 1)
        out["server_total_ms"] = round(phases.get("terminal_total", 0), 1)

    try:
        # journald 有秒级可见性延迟:沙箱刚 kill 完立刻查可能拿不到,带重试
        for attempt in range(5):
            j = subprocess.run(["journalctl", "--since", since, "--until", until,
                                "--no-pager"], capture_output=True, text=True,
                               timeout=30).stdout
            pid = None
            for line in j.splitlines():
                if sid in line:
                    m = re.search(r'sandbox-ctl\[(\d+)\]', line)
                    if m:
                        pid = m.group(1)
                        break
            ack_us = None
            if pid:
                for line in j.splitlines():
                    if f"[{pid}]" not in line:
                        continue
                    m = re.search(r'restore notify acked in (\d+)µs', line)
                    if m:
                        ack_us = int(m.group(1))
                        break
            if ack_us is not None:
                out["sandbox_init_ms"] = round(ack_us / 1000.0, 1)
                rr = phases.get("runtime_ready_duration")
                if rr:
                    out["sandbox_ctl_ms"] = round(rr - ack_us / 1000.0, 1)
                break
            if attempt < 4:
                time.sleep(1.5)
    except Exception as exc:
        out["journal_error"] = f"{type(exc).__name__}: {str(exc)[:120]}"
    return out


def check_dual_disk(template):
    """开班检查(不计入样本):create → lsblk 确认双盘(vda RO + vdb RW)→ 立即删。
    注意本基准不启动 openclaw gateway,事务只到 envd 首命令为止。"""
    sandbox = None
    try:
        sandbox = Sandbox.create(template=template, timeout=300)
        sid = sandbox.sandbox_id
        out = sandbox.commands.run(
            "lsblk -ndo NAME,SIZE,RO,TYPE | grep -w disk",
            user="root", cwd="/", timeout=60)  # openclaw 镜像无 'user' 账号与 /home/user,需显式 root + cwd(同 bench.py)
        print(f"磁盘形态(sid={sid[:13]}):\n{out.stdout.strip()}")
        disks = [line.split() for line in out.stdout.strip().splitlines()]
        ro = {d[0]: d[2] for d in disks if len(d) >= 3}
        if not ("vda" in ro and "vdb" in ro):
            raise RuntimeError(f"非双盘:只见 {sorted(ro)}")
        if ro["vda"] != "1":
            raise RuntimeError(f"vda 应为只读(RO=1),实际 RO={ro['vda']}")
        if ro["vdb"] != "0":
            raise RuntimeError(f"vdb 应为可写(RO=0),实际 RO={ro['vdb']}")
        print("✓ 双盘模式确认:vda RO=1(只读 base)+ vdb RW(overlay)")
    finally:
        if sandbox is not None:
            bench._delete_sandbox(sandbox.sandbox_id, sandbox)
            bench.cleanup_sandbox_directories(sandbox.sandbox_id)


def resolve_template(name):
    """模板参数可以是 .id 文件名(仓库惯例)或完整模板 id。"""
    path = os.path.join(PARENT, name)
    if os.path.isfile(path):
        return open(path).read().strip()
    return name


def percentile(values, p):
    if not values:
        return None
    s = sorted(values)
    k = (len(s) - 1) * p
    lo, hi = int(k), min(int(k) + 1, len(s) - 1)
    return s[lo] + (s[hi] - s[lo]) * (k - lo)


def stats(values):
    if not values:
        return None
    return {
        "n": len(values),
        "min": round(min(values), 1),
        "p50": round(percentile(values, 0.50), 1),
        "p90": round(percentile(values, 0.90), 1),
        "avg": round(statistics.fmean(values), 1),
        "max": round(max(values), 1),
    }


def wait_state_running(sid, timeout=60, interval=0.01):
    """轮询 GET /sandboxes/{sid} 到 state=running(--breakdown 用)。
    返回 (elapsed_ms, state, polls);10ms 粒度,把 exec 段拆成
    『服务端 launch 完成』+『envd 首命令(含 SDK connect+数据面固定成本)』。"""
    start = time.perf_counter()
    polls, state = 0, None
    while time.perf_counter() - start < timeout:
        polls += 1
        try:
            r = e2b_env.api_request("GET", f"/sandboxes/{sid}", timeout=5)
            if r.status_code == 200:
                state = r.json().get("state")
                if state == "running":
                    break
        except Exception:
            pass
        time.sleep(interval)
    return round((time.perf_counter() - start) * 1000, 1), state, polls


def one_sample(index, template, cmd, breakdown=False):
    """一次完整事务:create → envd cmd → (launchMs) → kill+清理。返回样本 dict。"""
    sample = {"i": index, "ok": False, "sid": None, "error": None,
              "wall_start": datetime.now().isoformat(timespec="seconds")}
    sandbox = None
    try:
        t0 = time.perf_counter()
        sandbox = Sandbox.create(template=template, timeout=300)
        t1 = time.perf_counter()
        sample["sid"] = sandbox.sandbox_id
        sample["create_ms"] = round((t1 - t0) * 1000, 1)

        if breakdown:
            wait_ms, state, polls = wait_state_running(sample["sid"])
            t1b = time.perf_counter()
            sample["launch_wait_ms"] = wait_ms
            sample["launch_polls"] = polls
            if state != "running":
                sample["error"] = f"launch 未达 running(state={state})"
                return sample

        result = sandbox.commands.run(cmd, user="root", cwd="/", timeout=60)
        t2 = time.perf_counter()
        sample["exec_ms"] = round((t2 - t1) * 1000, 1)
        sample["e2e_ms"] = round((t2 - t0) * 1000, 1)
        sample["exit_code"] = result.exit_code
        if result.exit_code != 0:
            sample["error"] = f"exit {result.exit_code}: {result.stderr[:200]}"
            return sample
        sample["ok"] = True

        try:
            launch = bench.wait_launch_ms(sample["sid"], timeout=5)
            if isinstance(launch, (int, float)):
                sample["launch_ms"] = round(launch, 1)
        except Exception:
            pass  # 无 timing patch / 拿不到就缺省
    except Exception as exc:
        sample["error"] = f"{type(exc).__name__}: {str(exc)[:300]}"
    finally:
        if sample["sid"]:
            ok_del, errs = bench._delete_sandbox(sample["sid"], sandbox)
            bench.cleanup_sandbox_directories(sample["sid"])
            if not ok_del:
                sample.setdefault("error", f"cleanup: {'; '.join(errs)}")
    return sample


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("-n", "--samples", type=int, default=10, help="样本数(默认 10)")
    ap.add_argument("-c", "--concurrency", type=int, default=1, help="并发(默认 1)")
    ap.add_argument("--template", default=DEFAULT_TEMPLATE_FILE,
                    help=f"模板 .id 文件或完整 id(默认 {DEFAULT_TEMPLATE_FILE})")
    ap.add_argument("--cmd", default="true", help="envd 首命令(默认 true)")
    ap.add_argument("--no-check", action="store_true",
                    help="跳过开班双盘检查(默认先做一次不计时的 create+lsblk 验证)")
    ap.add_argument("--breakdown", action="store_true",
                    help="拆解 exec 段:admission 后 10ms 轮询 state=running,"
                         "分出 launch_wait_ms(服务端启动)与剩余(envd connect+首命令)")
    ap.add_argument("--no-server", action="store_true",
                    help="跳过服务端阶段抽取(默认每样本后只读解析 conductor.log"
                         "+journal,输出 node_ctl/sandbox_ctl/sandbox_init/envd 分段)")
    args = ap.parse_args()

    template = resolve_template(args.template)
    print(f"template={template[:40]}… cmd={args.cmd!r} n={args.samples} c={args.concurrency}")

    if not args.no_check:
        check_dual_disk(template)

    ts = datetime.now().strftime("%Y%m%d-%H%M%S")
    outdir = os.path.join(PARENT, "repro-results", f"coldstart-envd-{ts}")
    os.makedirs(outdir, exist_ok=True)

    samples = []
    with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        futures = [pool.submit(one_sample, i, template, args.cmd, args.breakdown)
                   for i in range(args.samples)]
        for fut in as_completed(futures):
            s = fut.result()
            if not args.no_server and s["sid"]:
                s["server"] = server_phases(
                    s["sid"], s["wall_start"],
                    datetime.now().isoformat(timespec="seconds"))
            samples.append(s)
            with open(os.path.join(outdir, "samples.jsonl"), "a") as f:
                f.write(json.dumps(s, ensure_ascii=False) + "\n")
            mark = "✓" if s["ok"] else "✗"
            line = (f"[{len(samples):>3}/{args.samples}] {mark} i={s['i']}"
                    f" e2e={s.get('e2e_ms')}ms create={s.get('create_ms')}ms"
                    f" exec={s.get('exec_ms')}ms")
            if "launch_wait_ms" in s:
                line += f" launch_wait={s['launch_wait_ms']}ms"
            if "launch_ms" in s:
                line += f" server_launch={s['launch_ms']}ms"
            if s["error"]:
                line += f" err={s['error'][:120]}"
            print(line, flush=True)
            srv = s.get("server") or {}
            if srv:
                print(f"      server: node_ctl={srv.get('node_ctl_ms')}"
                      f" sandbox_ctl={srv.get('sandbox_ctl_ms')}"
                      f" sandbox_init={srv.get('sandbox_init_ms')}"
                      f" envd={srv.get('envd_ms')}"
                      f" total={srv.get('server_total_ms')}", flush=True)

    # 收尾补齐:conductor 写缓冲/journald 延迟可能导致个别样本缺键,统一重抽后重写 jsonl
    if not args.no_server:
        changed = False
        for s in samples:
            sp = s.get("server") or {}
            if s["sid"] and (sp.get("node_ctl_ms") is None
                             or sp.get("sandbox_init_ms") is None):
                fixed = server_phases(
                    s["sid"], s["wall_start"],
                    datetime.now().isoformat(timespec="seconds"))
                merged = dict(sp)
                merged.update({k: v for k, v in fixed.items() if v is not None})
                s["server"] = merged
                changed = True
        if changed:
            with open(os.path.join(outdir, "samples.jsonl"), "w") as f:
                for s in samples:
                    f.write(json.dumps(s, ensure_ascii=False) + "\n")

    ok = [s for s in samples if s["ok"]]
    result = {
        "ts": ts,
        "template": template,
        "cmd": args.cmd,
        "samples": args.samples,
        "concurrency": args.concurrency,
        "breakdown": args.breakdown,
        "ok": len(ok),
        "fail": args.samples - len(ok),
        "e2e_ms": stats([s["e2e_ms"] for s in ok]),
        "create_ms": stats([s["create_ms"] for s in ok]),
        "exec_ms": stats([s["exec_ms"] for s in ok]),
        "launch_ms": stats([s["launch_ms"] for s in ok if "launch_ms" in s]),
    }
    if args.breakdown:
        waits = [s["launch_wait_ms"] for s in ok if "launch_wait_ms" in s]
        result["launch_wait_ms"] = stats(waits)
        result["exec_after_launch_ms"] = stats(
            [round(s["exec_ms"] - s["launch_wait_ms"], 1)
             for s in ok if "launch_wait_ms" in s])
    if not args.no_server:
        for key in ("node_ctl_ms", "sandbox_ctl_ms", "sandbox_init_ms",
                    "envd_ms", "server_total_ms"):
            vals = [s["server"][key] for s in ok
                    if s.get("server") and s["server"].get(key) is not None]
            if vals:
                result[key] = stats(vals)
    with open(os.path.join(outdir, "result.json"), "w") as f:
        json.dump(result, f, ensure_ascii=False, indent=2)

    print("=== summary ===")
    print(json.dumps(result, ensure_ascii=False, indent=2))
    print(f"结果目录: {outdir}")
    if result["fail"]:
        sys.exit(1)


if __name__ == "__main__":
    main()
