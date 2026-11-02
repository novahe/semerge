#!/usr/bin/env python3
"""probe_balloon_remove.py — 动态验证"启动期 balloon 回收风暴饿死 uffd fault"结论。

结论链(代码级):开机 balloon 充入 ~7GiB → guest 释放产生海量 uffd EVENT_REMOVE
→ sandbox-ctl reader 在全局 installMu 内逐批处理(handler.go drain/dispatchRelease)
→ 首命令 zeropage fault 在 installMu 上排队(fault_queue_wait=纯排队时间)。

本探针取三份 /sandboxes/{sid}/stats/resource 快照对照:
  A = running 时刻;B = 首命令结束;C = 再静置 8s 后。
看 remove_events 在各窗口的增量,与 fault_queue_wait_p50 的量级。
"""
import json
import os
import sys
import time

NOVA_TENANT_ENV = "/root/sandbox/nova-work/etc/node-ctl-nova/tenant.env"
if not os.environ.get("E2B_ENV_FILE") and os.path.isfile(NOVA_TENANT_ENV):
    os.environ["E2B_ENV_FILE"] = NOVA_TENANT_ENV

import e2b_env  # noqa: E402

e2b_env.load()

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from bench_coldstart_envd import DEFAULT_MANIFEST_KEY, resolve_template  # noqa: E402

import bench  # noqa: E402
from e2b import Sandbox  # noqa: E402

WATCH = ("remove_events", "remove_events_batched", "remove_q_dropped", "madvise_calls",
         "fault_queue_wait_ns", "fault_queue_wait_p50", "fault_queue_wait_p95",
         "fault_queue_wait_p99", "faults", "fault_pages", "fault_queue_depth_hwm")


def get_stats(sid):
    r = e2b_env.api_request("GET", f"/sandboxes/{sid}/stats/resource", timeout=10)
    if r.status_code != 200:
        return {"_http": r.status_code, "_body": r.text[:120]}
    return r.json()


def uffd_view(stats):
    """stats 结构未知,递归找含 uffd 键的子对象再过滤 WATCH。"""
    def walk(o):
        if isinstance(o, dict):
            hits = {k: o[k] for k in o if k in WATCH or k.startswith("uffd")}
            if hits and any(k in WATCH for k in o):
                return hits
            for v in o.values():
                got = walk(v)
                if got:
                    return got
        elif isinstance(o, list):
            for v in o:
                got = walk(v)
                if got:
                    return got
        return None
    return walk(stats) or {}


def wait_running(sid, deadline=60):
    start = time.perf_counter()
    while time.perf_counter() - start < deadline:
        try:
            r = e2b_env.api_request("GET", f"/sandboxes/{sid}", timeout=3)
            if r.status_code == 200 and r.json().get("state") == "running":
                return round((time.perf_counter() - start) * 1000, 1)
        except Exception:
            pass
        time.sleep(0.005)
    return None


def main():
    template = resolve_template(DEFAULT_MANIFEST_KEY)
    sb = Sandbox.create(template=template, timeout=300)
    sid = sb.sandbox_id
    try:
        t_r = wait_running(sid)
        print(f"sid={sid[:13]} running@{t_r}ms")
        spec = e2b_env.api_request("GET", f"/sandboxes/{sid}", timeout=5).json()
        for k in ("memory", "spec", "config", "allocatable", "template"):
            if k in spec:
                print(f"  {k}: {json.dumps(spec[k], ensure_ascii=False)[:200]}")

        a = uffd_view(get_stats(sid))
        t1 = time.perf_counter()
        r1 = sb.commands.run("true", user="root", cwd="/", timeout=60)
        t_cmd1 = round((time.perf_counter() - t1) * 1000, 1)
        b = uffd_view(get_stats(sid))
        time.sleep(8)
        c = uffd_view(get_stats(sid))
        t2 = time.perf_counter()
        r2 = sb.commands.run("true", user="root", cwd="/", timeout=60)
        t_cmd2 = round((time.perf_counter() - t2) * 1000, 1)

        for label, snap in (("A@running", a), ("B@cmd1后", b), ("C@+8s", c)):
            print(f"[{label}] " + json.dumps({k: snap.get(k) for k in WATCH
                                              if k in snap}, ensure_ascii=False))
        print(f"cmd1={t_cmd1}ms rc={r1.exit_code}  cmd2={t_cmd2}ms rc={r2.exit_code}")
        if a and b:
            d = {k: b.get(k, 0) - a.get(k, 0) for k in
                 ("remove_events", "madvise_calls", "faults") if k in a and k in b}
            print(f"A→B(首命令窗口)增量: {d}")
        if b and c:
            d = {k: c.get(k, 0) - b.get(k, 0) for k in
                 ("remove_events", "madvise_calls", "faults") if k in b and k in c}
            print(f"B→C(静置8s)增量: {d}")
    finally:
        bench._delete_sandbox(sid, sb)
        bench.cleanup_sandbox_directories(sid)


if __name__ == "__main__":
    main()
