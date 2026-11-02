#!/usr/bin/env python3
"""probe_exec_gap.py v2 — 冷启 exec_after_launch(~2.9s)归因。

v1 教训:envd /health 答的是 204(非 200),判定错导致 2×30s 假超时;
晚发 commands.run 只要 ~1s。本版:
  1) running 时刻立即 commands.run → t_cmd1(复现 ~2.9s?)
  2) 同沙箱紧接第二条 → t_cmd2(暖命令对照)
  3) 期间 5ms 轮询 envd.sock /health 的状态码序列,看 204 之外有没有跳变
  4) 第二个沙箱:running 后先 sleep 3s 再发命令 → 区分"envd 慢热"vs"首命令固定成本"
"""
import os
import socket
import sys
import time
from collections import Counter

import e2b_env

NOVA_TENANT_ENV = "/root/sandbox/nova-work/etc/node-ctl-nova/tenant.env"
if not os.environ.get("E2B_ENV_FILE") and os.path.isfile(NOVA_TENANT_ENV):
    os.environ["E2B_ENV_FILE"] = NOVA_TENANT_ENV
e2b_env.load()

import bench  # noqa: E402
import e2b_env as env_mod  # noqa: E402
from e2b import Sandbox  # noqa: E402

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from bench_coldstart_envd import DEFAULT_MANIFEST_KEY, resolve_template  # noqa: E402


def http_unix_status(sock_path, path="/health", timeout=1):
    try:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(timeout)
        s.connect(sock_path)
        s.sendall((f"GET {path} HTTP/1.1\r\nHost: localhost\r\n"
                   "Connection: close\r\n\r\n").encode())
        data = b""
        while len(data) < 4096:
            chunk = s.recv(4096)
            if not chunk:
                break
            data += chunk
        s.close()
        line = data.split(b"\r\n", 1)[0].decode(errors="replace")
        return line.split(" ", 1)[1] if " " in line else line or "(空)"
    except Exception as e:
        return f"err:{type(e).__name__}"


def wait_running(sid, deadline=30):
    start = time.perf_counter()
    while time.perf_counter() - start < deadline:
        try:
            r = env_mod.api_request("GET", f"/sandboxes/{sid}", timeout=3)
            if r.status_code == 200 and r.json().get("state") == "running":
                return round((time.perf_counter() - start) * 1000, 1)
        except Exception:
            pass
        time.sleep(0.005)
    return None


def one_round(label, pre_sleep=0.0, track_envd=True):
    template = resolve_template(DEFAULT_MANIFEST_KEY)
    t0 = time.perf_counter()
    sb = Sandbox.create(template=template, timeout=300)
    sid = sb.sandbox_id
    t_r = wait_running(sid)
    print(f"[{label}] sid={sid[:13]} running@{t_r}ms")
    if pre_sleep:
        time.sleep(pre_sleep)
    sock = f"/run/sandbox-nova/sandboxes/{sid}/envd.sock"
    seq = Counter()

    class Timer:
        pass

    # 边发命令边轮询 envd 状态码(线程简单地串行混采:先采一次,命令后再采)
    st_before = http_unix_status(sock) if track_envd else "-"
    t1 = time.perf_counter()
    r1 = sb.commands.run("true", user="root", cwd="/", timeout=60)
    t_cmd1 = round((time.perf_counter() - t1) * 1000, 1)
    st_mid = http_unix_status(sock) if track_envd else "-"
    t2 = time.perf_counter()
    r2 = sb.commands.run("true", user="root", cwd="/", timeout=60)
    t_cmd2 = round((time.perf_counter() - t2) * 1000, 1)
    print(f"[{label}] cmd1={t_cmd1}ms rc={r1.exit_code} envd状态 cmd前={st_before} cmd后={st_mid}; cmd2={t_cmd2}ms rc={r2.exit_code}")
    bench._delete_sandbox(sid, sb)
    bench.cleanup_sandbox_directories(sid)
    return t_cmd1, t_cmd2


if __name__ == "__main__":
    one_round("A_immediate")          # running 即发
    one_round("B_sleep3s", pre_sleep=3.0)  # running 后等 3s 再发
