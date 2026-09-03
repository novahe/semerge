#!/usr/bin/env python3
"""从 e2bdev/code-interpreter manifest 镜像一次产出 B/W 快照。

流程：
  create → run_code + commands.run("date")（确认两条执行路径可用）
  → pause 默认策略 → export B
  → connect → run_code + commands.run("date") 预热两类执行工作集
  → pause 保留缓存 → export W

这里的 B/W 来自同一个 guest。B 使用节点默认的 ``drop_caches=true``，W 使用
``drop_caches=false, merge_ref=false``。W 同时记录 ``run_code`` 和
``commands.run("date")`` 两类新增工作集；bench.py 再通过 ``--probe command`` 或
``--probe code`` 选择其中一条路径进行基准测试，不会在同一轮 bench 中同时执行。

code-interpreter manifest 依赖 envd，不能使用 ``bare`` profile。模板使用带 envd
的 ``e2b-img-`` 前缀；构建流程不修改 guest 文件，只验证一次有输出的
``run_code`` 和一次成功的 ``commands.run``。命令默认是 ``date``，可用
``RUN_COMMAND`` 覆盖。
"""
import base64
import json
import os
import shlex
import sys
import time

import e2b_env

e2b_env.load()
e2b_env.require("E2B_DOMAIN", "E2B_API_KEY", "SSL_CERT_FILE")
from e2b import Sandbox as EnvdSandbox
from e2b_code_interpreter import Sandbox as CodeSandbox


PARENT = os.path.dirname(os.path.abspath(__file__))
MANIFEST_KEY = os.environ.get(
    "MANIFEST_KEY",
    "4410ffb653b2eb4a67d421531a4852c7f2ddacddbf7cccebe664a85c571a3990",
)
OUT_B = os.environ.get(
    "OUT_B", os.path.join(PARENT, "code-interpreter-e2b-snp-template.id")
)
OUT_W = os.environ.get(
    "OUT_W", os.path.join(PARENT, "code-interpreter-e2b-ws-template.id")
)
SANDBOX_TIMEOUT = int(os.environ.get("SANDBOX_TIMEOUT", "600"))
RUN_TIMEOUT = int(os.environ.get("RUN_TIMEOUT", "60"))
BRIDGE_READY_TIMEOUT = int(os.environ.get("BRIDGE_READY_TIMEOUT", "120"))
WARM_RUNS = int(os.environ.get("WARM_RUNS", "4"))
READY_SETTLE = float(os.environ.get("READY_SETTLE", "20"))
RUN_USER = os.environ.get("RUN_USER", "root")
RUN_CWD = os.environ.get("RUN_CWD", "/")
RUN_COMMAND = os.environ.get("RUN_COMMAND", "date")
DEFAULT_RUN_CODE = "print('hello kuasar-sandbox {index}')"
RUN_CODE = os.environ.get("RUN_CODE", DEFAULT_RUN_CODE)
STARTUP_COMMAND = (
    "cd /root/.jupyter && E2B_LOCAL=true "
    "setsid sh -c 'exec ./start-up.sh' "
    "</dev/null >/tmp/code-interpreter-start.log 2>&1 & echo started"
)
DATAPLANE_PORTS = ("49983", "49999")


def template_id(manifest_key):
    payload = ("manifest://" + manifest_key).encode()
    encoded = base64.urlsafe_b64encode(payload).decode().rstrip("=")
    return "e2b-img-" + encoded


def add_hosts(sid, domain):
    """Match build_bw_coldboot.py: pin envd/data-plane hosts to node proxy."""
    with open("/etc/hosts") as hosts:
        lines = hosts.read().splitlines()
    add = [
        f"127.0.0.1 {port}-{sid}.{domain} #bwci"
        for port in DATAPLANE_PORTS
        if not any(f"{port}-{sid}.{domain}" in line for line in lines)
    ]
    if add:
        with open("/etc/hosts", "a") as hosts:
            hosts.write("\n".join(add) + "\n")


def del_hosts(sid, domain):
    """Remove only this build's temporary host aliases."""
    with open("/etc/hosts") as hosts:
        kept = [
            line
            for line in hosts.read().splitlines()
            if not any(f"{port}-{sid}.{domain}" in line for port in DATAPLANE_PORTS)
        ]
    with open("/etc/hosts", "w") as hosts:
        hosts.write("\n".join(kept) + "\n")


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
    return body["sandboxID"]


def connect_code_sandbox(sid):
    sandbox = CodeSandbox.connect(sid, timeout=SANDBOX_TIMEOUT)
    if not getattr(sandbox, "_envd_access_token", None):
        raise RuntimeError("e2b connect returned no envdAccessToken for 49999")
    return sandbox


def start_bridge(envd_sandbox):
    handle = envd_sandbox.commands.run(
        STARTUP_COMMAND,
        user="root",
        cwd="/",
        background=True,
        timeout=15,
    )
    if hasattr(handle, "disconnect"):
        handle.disconnect()
    return getattr(handle, "pid", None)


def wait_for_bridge(sandbox):
    """Wait for the guest FastAPI bridge, not just envd launch readiness."""
    token = sandbox._envd_access_token
    headers = {
        "E2b-Sandbox-Id": sandbox.sandbox_id,
        "E2b-Sandbox-Port": "49999",
        "X-Access-Token": token,
    }
    deadline = time.monotonic() + BRIDGE_READY_TIMEOUT
    last = "not probed"
    started = time.monotonic()
    while time.monotonic() < deadline:
        try:
            client = sandbox._client
            with client:
                response = client.get(
                    f"{sandbox._jupyter_url}/health",
                    headers=headers,
                    timeout=5,
                )
            if response.status_code == 200:
                return (time.monotonic() - started) * 1000
            last = f"HTTP {response.status_code}: {response.text[:120]!r}"
        except Exception as e:
            last = f"{type(e).__name__}: {e}"
        time.sleep(1)
    raise RuntimeError(
        f"code-interpreter bridge /health did not become ready in "
        f"{BRIDGE_READY_TIMEOUT}s ({last})"
    )


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
    # keepSource=True：B/W 出自同一 guest，第一次 export 后必须保留源沙箱
    # （默认 keepSource=false 会 teardown 销毁源）。B/W 都导完后由 finally
    # DELETE 清理。
    r = e2b_env.api_request(
        "POST",
        f"/sandboxes/{sid}/export",
        json={"toTemplate": True, "keepSource": True},
        timeout=180,
    )
    if r.status_code != 200:
        sys.exit(f"✗ export HTTP {r.status_code}: {r.text[:300]}")
    template = r.json().get("result", "")
    if not template:
        sys.exit(f"✗ export 返回空 template: {r.text[:300]}")
    return template


def run_code(sandbox, code):
    t0 = time.time()
    result = sandbox.run_code(code, timeout=RUN_TIMEOUT)
    elapsed_ms = (time.time() - t0) * 1000
    if result.error is not None:
        traceback = getattr(result.error, "traceback", "") or ""
        if "unauthorized access" in traceback.lower():
            raise RuntimeError(
                "run_code rejected: the guest bridge did not forward "
                "X-Access-Token to EnvD; use an official image with the "
                "native bridge fix"
            )
        raise RuntimeError(f"run_code error={result.error.value!r}")
    stdout = getattr(result.logs, "stdout", None) or []
    text = " ".join(str(s) for s in stdout) if stdout else (
        result.results[0].text if result.results else "")
    output = (text or "").strip().replace("\n", " ")[:120]
    if not output:
        raise RuntimeError(
            "run_code returned no stdout/result; code-interpreter /execute "
            "bridge is missing or did not return an execution stream"
        )
    return elapsed_ms, output


def run_command(sandbox, command):
    """通过 e2b/envd 执行 guest 命令，返回耗时和 stdout/stderr。"""
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
        raise RuntimeError(
            f"commands.run exit_code={exit_code} "
            f"stdout={stdout[:200]!r} stderr={stderr[:200]!r}"
        )
    return elapsed_ms, output


def code_with_index(code, index):
    """Vary only the built-in smoke code; preserve caller-supplied Python."""
    if code == DEFAULT_RUN_CODE:
        return code.format(index=index)
    return code


def write_template(path, value):
    with open(path, "w") as f:
        f.write(value + "\n")


def main():
    tid = template_id(MANIFEST_KEY)
    print(f"• code-interpreter B/W from manifest://{MANIFEST_KEY[:12]}…")
    print(f"  templateID={tid}")
    print(f"  command={RUN_COMMAND!r}  user={RUN_USER}  cwd={RUN_CWD}")
    domain = os.environ["E2B_DOMAIN"]
    sid = None
    hosts_added = False
    try:
        sid = create_sandbox(tid)
        # Keep the startup path identical to build_bw_coldboot.py: connect to
        # the envd SDK and use its data-plane aliases for 49983/49999.
        add_hosts(sid, domain)
        hosts_added = True
        envd_sandbox = EnvdSandbox.connect(sid, timeout=SANDBOX_TIMEOUT)
        print(f"  ✓ created sid={sid}")

        print("[1/5] 启动镜像自带 start-up.sh / Jupyter/49999 + readiness …")
        pid = start_bridge(envd_sandbox)
        print(f"  ✓ start-up.sh started pid={pid}")
        code_sandbox = connect_code_sandbox(sid)
        ready_ms = wait_for_bridge(code_sandbox)
        print(f"  ✓ 49999/health ready {ready_ms:.0f}ms")
        elapsed, output = run_code(
            code_sandbox, code_with_index(RUN_CODE, "create-check")
        )
        print(f"  ✓ run_code {elapsed:.0f}ms output={output!r}")
        elapsed, output = run_command(envd_sandbox, RUN_COMMAND)
        print(f"  ✓ commands.run {elapsed:.0f}ms output={output!r}")
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
        envd_sandbox = EnvdSandbox.connect(sid, timeout=SANDBOX_TIMEOUT)
        code_sandbox = connect_code_sandbox(sid)
        ready_ms = wait_for_bridge(code_sandbox)
        print(f"  ✓ resumed ({(time.time() - t0) * 1000:.0f}ms), "
              f"49999/health ready {ready_ms:.0f}ms")

        print(f"[4/5] run_code + commands.run 双预热 ×{WARM_RUNS} …")
        for i in range(WARM_RUNS):
            code_elapsed, code_output = run_code(
                code_sandbox, code_with_index(RUN_CODE, f"warm-{i + 1}")
            )
            command_elapsed, command_output = run_command(envd_sandbox, RUN_COMMAND)
            print(f"  warm{i + 1}: run_code={code_elapsed:.0f}ms "
                  f"{code_output!r}; commands.run={command_elapsed:.0f}ms "
                  f"{command_output!r}")

        print("[5/5] pause#2（drop_caches=false, merge_ref=false）→ export W …")
        t0 = time.time()
        pause_sandbox(sid, {"drop_caches": False, "merge_ref": False})
        w = export_sandbox(sid)
        write_template(OUT_W, w)
        print(f"  ✓ W={OUT_W} ({(time.time() - t0) * 1000:.0f}ms)")

        print("\n=== done ===")
        print(f"B（e2b/cold baseline）:      {OUT_B}")
        print(f"W（working set）:        {OUT_W}")
        command = shlex.quote(RUN_COMMAND)
        print(f"bench (commands.run): python3 bench.py --b \"$(cat {OUT_B})\" "
              f"--w \"$(cat {OUT_W})\" --probe command --run-command {command}")
        print(f"bench (run_code):     python3 bench.py --b \"$(cat {OUT_B})\" "
              f"--w \"$(cat {OUT_W})\" --probe code --run-code "
              f"{shlex.quote(RUN_CODE)}")
    finally:
        # Host aliases are only needed while this process is talking to the
        # data plane. Clean them independently: a control-plane DELETE timeout
        # must not leave #bwci entries in the shared host file.
        if hosts_added:
            try:
                del_hosts(sid, domain)
            except Exception as e:
                print(f"⚠ cleanup hosts for {sid}: {e}", file=sys.stderr)
        if sid:
            try:
                e2b_env.api_request("DELETE", f"/sandboxes/{sid}", timeout=30)
            except Exception as e:
                print(f"⚠ cleanup sandbox {sid}: {e}", file=sys.stderr)


if __name__ == "__main__":
    main()
