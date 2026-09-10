#!/usr/bin/env python3
"""hold.py — 从模板起一个沙箱并挂住，Ctrl+C 清理。调试用。

用法：
  ./hold.py <模板ID 或 .id 文件名>          # 挂住，Ctrl+C 删除沙箱退出
  ./hold.py openclaw-e2b-ws-balloon-q2-template.id
  ./hold.py e2b-snp-ZmlsZTovL...

输出 sid 与现成的 sandbox-ctl TTY exec / proxy curl / SDK 连接命令，方便另开终端观测（debug_cold7s.py、
runner.log、guest 内 /tmp/gw.log 等）。沙箱空闲回收放宽到 1h；
create 成功后任何路径退出（含崩溃）都会清理。
"""
import os
import shlex
import signal
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import e2b_env
import openclaw_net

e2b_env.load(os.environ.get("E2B_ENV_FILE")
             or "/root/sandbox/nova-work/etc/node-ctl-nova/tenant.env")

HOLD_TIMEOUT = 3600  # 沙箱空闲回收上限（秒）


def resolve_run_root():
    """Resolve the node-local run root used by sandbox-ctl's ctl.sock."""
    # Prefer an explicit CLI-compatible override.  E2B_RUN_ROOT is retained
    # for older/local deployments that already export it in their env file.
    configured = os.environ.get("SANDBOX_RUN_ROOT") or os.environ.get("E2B_RUN_ROOT")
    if configured:
        return configured

    # Namespace deployments commonly put per-sandbox state under a nested
    # `sandboxes` directory (older deployments used the namespace root
    # directly).  The tenant domain is available after e2b_env.load().
    domain = os.environ.get("E2B_DOMAIN", "")
    if domain.startswith("sandboxes.") and domain.endswith(".local"):
        namespace = domain[len("sandboxes."):-len(".local")]
        if namespace:
            nested = f"/run/sandbox-{namespace}/sandboxes"
            if os.path.isdir(nested):
                return nested
            return f"/run/sandbox-{namespace}"
    return "/run/sandbox"


def resolve_template(arg):
    """参数可以是模板 ID 本身，也可以是（本目录下的）.id 文件名/路径。"""
    for p in (arg, os.path.join(os.path.dirname(os.path.abspath(__file__)), arg)):
        if os.path.isfile(p):
            return open(p).read().strip()
    return arg


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    tpl = resolve_template(sys.argv[1])

    r = e2b_env.api_request("POST", "/sandboxes",
                            json={"templateID": tpl, "timeout": HOLD_TIMEOUT})
    if r.status_code != 201:
        sys.exit(f"✗ create HTTP {r.status_code}: {r.text[:200]}")
    j = r.json()
    sid, token = j["sandboxID"], j["forwardAccessToken"]
    print(f"✓ sid={sid}")
    sys.stdout.flush()

    try:
        run_root = resolve_run_root()
        ctl = os.environ.get("SANDBOX_CTL", "sandbox-ctl")
        exec_cmd = [ctl, "exec", "--sandbox-id", sid,
                    "--run-root", run_root, "--tty", "--", "/bin/bash"]
        print(f"  TTY exec : {shlex.join(exec_cmd)}")
        print("              （需在承载该 sandbox 的 node 上执行；退出 shell 不会删除 sandbox）")
        print(f"  经 proxy : curl --noproxy '*' -s -o /dev/null "
              f"-w '%{{http_code}} %{{time_total}}s' --cacert $SSL_CERT_FILE \\")
        print(f"      -H 'E2b-Sandbox-Id: {sid}' -H 'E2b-Sandbox-Port: 18789' "
              f"-H 'X-Access-Token: {token}' {openclaw_net.proxy_url()}/health")
        print(f"  SDK     : Sandbox.connect('{sid}')")
        print("挂住中… Ctrl+C 清理并退出")
        sys.stdout.flush()

        stop = {"flag": False}

        def bye(signum, frame):
            stop["flag"] = True

        signal.signal(signal.SIGINT, bye)
        signal.signal(signal.SIGTERM, bye)
        while not stop["flag"]:
            time.sleep(0.5)
    finally:
        try:
            e2b_env.api_request("DELETE", f"/sandboxes/{sid}", timeout=30)
            print(f"\n✓ 已删除 {sid}")
        except Exception as e:
            print(f"\n✗ 删除失败 {sid}: {e}（手动: DELETE /sandboxes/{sid}）",
                  file=sys.stderr)
            sys.exit(1)


if __name__ == "__main__":
    main()
