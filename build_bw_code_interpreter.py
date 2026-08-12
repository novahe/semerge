#!/usr/bin/env python3
"""从 e2bdev/code-interpreter manifest 镜像一次产出 B/W 快照。

流程：
  create → commands.run（确认 envd 可执行命令）→ pause 默认策略 → export B
  → connect → commands.run 预热命令执行工作集 → pause 保留缓存 → export W

这里的 B/W 来自同一个 guest。B 使用节点默认的 ``drop_caches=true``，W 使用
``drop_caches=false, merge_ref=false``，因此 bench.py 的 ``--probe run`` 可以直接
比较 e2b SDK → node-proxy → envd → guest command 这条路径。

镜像不是上传 rootfs，而是 yyl 上传的 manifest：template ID 按现有流程由
``manifest://<key>`` 编码为 ``e2b-img-<base64url>``。
"""
import base64
import json
import os
import sys
import time

import e2b_env

e2b_env.load()
e2b_env.require("E2B_DOMAIN", "E2B_API_KEY", "SSL_CERT_FILE")
from e2b import Sandbox


PARENT = os.path.dirname(os.path.abspath(__file__))
MANIFEST_KEY = os.environ.get(
    "MANIFEST_KEY",
    "8aeffe688f1061d0826a9ec3d79b911a230bf0968c3648c8a403198fd7b3270a",
)
OUT_B = os.environ.get(
    "OUT_B", os.path.join(PARENT, "code-interpreter-bare-snp-template.id")
)
OUT_W = os.environ.get(
    "OUT_W", os.path.join(PARENT, "code-interpreter-e2b-ws-template.id")
)
SANDBOX_TIMEOUT = int(os.environ.get("SANDBOX_TIMEOUT", "600"))
RUN_TIMEOUT = int(os.environ.get("RUN_TIMEOUT", "60"))
RUN_USER = os.environ.get("RUN_USER", "root")
RUN_CWD = os.environ.get("RUN_CWD", "/")
WARM_RUNS = int(os.environ.get("WARM_RUNS", "4"))
READY_SETTLE = float(os.environ.get("READY_SETTLE", "2"))
RUN_COMMAND = os.environ.get(
    "RUN_COMMAND",
    "date",
)


def template_id(manifest_key):
    payload = ("manifest://" + manifest_key).encode()
    encoded = base64.urlsafe_b64encode(payload).decode().rstrip("=")
    return "e2b-img-" + encoded


def create_sandbox(tid):
    r = e2b_env.api_request(
        "POST",
        "/sandboxes",
        json={"templateID": tid, "timeout": SANDBOX_TIMEOUT},
        timeout=120,
    )
    if r.status_code != 201:
        sys.exit(f"✗ create HTTP {r.status_code}: {r.text[:400]}")
    body = r.json()
    return body["sandboxID"], body.get("forwardAccessToken", "")


def pause_sandbox(sid, policy=None):
    headers = {}
    if policy is not None:
        headers["X-Kuasar-Sandbox-Checkpoint"] = json.dumps(policy)
    r = e2b_env.api_request(
        "POST", f"/sandboxes/{sid}/pause", headers=headers, timeout=120
    )
    if r.status_code not in (200, 204):
        sys.exit(f"✗ pause HTTP {r.status_code}: {r.text[:300]}")
    time.sleep(3)


def connect_sandbox(sid):
    r = e2b_env.api_request(
        "POST",
        f"/sandboxes/{sid}/connect",
        json={"timeout": SANDBOX_TIMEOUT},
        timeout=120,
    )
    if r.status_code != 200:
        sys.exit(f"✗ connect HTTP {r.status_code}: {r.text[:300]}")


def export_sandbox(sid):
    r = e2b_env.api_request(
        "POST",
        f"/sandboxes/{sid}/export",
        json={"toTemplate": True},
        timeout=180,
    )
    if r.status_code != 200:
        sys.exit(f"✗ export HTTP {r.status_code}: {r.text[:300]}")
    template = r.json().get("result", "")
    if not template:
        sys.exit(f"✗ export 返回空 template: {r.text[:300]}")
    return template


def run_command(sandbox, command):
    t0 = time.time()
    result = sandbox.commands.run(
        command, user=RUN_USER, cwd=RUN_CWD, timeout=RUN_TIMEOUT
    )
    elapsed_ms = (time.time() - t0) * 1000
    stdout = getattr(result, "stdout", "") or ""
    stderr = getattr(result, "stderr", "") or ""
    output = (stdout or stderr).strip().replace("\n", " ")[:120]
    exit_code = getattr(result, "exit_code", 1)
    if exit_code != 0:
        raise RuntimeError(f"exit_code={exit_code} stdout={stdout[:200]!r} stderr={stderr[:200]!r}")
    return elapsed_ms, output


def write_template(path, value):
    with open(path, "w") as f:
        f.write(value + "\n")


def main():
    tid = template_id(MANIFEST_KEY)
    print(f"• code-interpreter B/W from manifest://{MANIFEST_KEY[:12]}…")
    print(f"  templateID={tid}")
    print(f"  command={RUN_COMMAND}  user={RUN_USER}  cwd={RUN_CWD}")
    sid = None
    try:
        sid, _ = create_sandbox(tid)
        sandbox = Sandbox.connect(sid, timeout=SANDBOX_TIMEOUT)
        print(f"  ✓ created sid={sid}")

        print("[1/5] commands.run 初始命令（envd readiness + baseline）…")
        elapsed, output = run_command(sandbox, RUN_COMMAND)
        print(f"  ✓ run {elapsed:.0f}ms output={output!r}")
        if READY_SETTLE > 0:
            time.sleep(READY_SETTLE)

        print("[2/5] pause#1（默认 drop_caches=true）→ export B …")
        t0 = time.time()
        pause_sandbox(sid)
        b = export_sandbox(sid)
        write_template(OUT_B, b)
        print(f"  ✓ B={OUT_B} ({(time.time() - t0) * 1000:.0f}ms)")

        print("[3/5] connect 恢复 …")
        t0 = time.time()
        connect_sandbox(sid)
        print(f"  ✓ resumed ({(time.time() - t0) * 1000:.0f}ms)")

        print(f"[4/5] commands.run 预热 ×{WARM_RUNS} …")
        for i in range(WARM_RUNS):
            elapsed, output = run_command(sandbox, RUN_COMMAND)
            print(f"  warm{i + 1}: {elapsed:.0f}ms output={output!r}")

        print("[5/5] pause#2（drop_caches=false, merge_ref=false）→ export W …")
        t0 = time.time()
        pause_sandbox(sid, {"drop_caches": False, "merge_ref": False})
        w = export_sandbox(sid)
        write_template(OUT_W, w)
        print(f"  ✓ W={OUT_W} ({(time.time() - t0) * 1000:.0f}ms)")

        print("\n=== done ===")
        print(f"B（bare/no working set）: {OUT_B}")
        print(f"W（working set）:         {OUT_W}")
        print(f"bench: python3 bench.py --b \"$(cat {OUT_B})\" --w \"$(cat {OUT_W})\" --probe run")
    finally:
        if sid:
            e2b_env.api_request("DELETE", f"/sandboxes/{sid}", timeout=30)


if __name__ == "__main__":
    main()
