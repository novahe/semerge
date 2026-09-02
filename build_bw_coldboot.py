#!/usr/bin/env python3
"""build_bw_coldboot.py — 一次冷启动顺序产出 B（冷）+ W（工作集）两个 1-layer 快照。

流程（B/W 出自同一 guest 进程树，变量最少）：
  cold boot → 起 gateway → 等 ready
  → pause#1（不带 X-Kuasar-Sandbox-Checkpoint，默认 drop_caches=true）→ export → B（冷快照）
  → connect 恢复 → 等 ready → 预热 health+exec ×WARM_CURLS（打 bash-tools 工作集）
  → pause#2（header {"drop_caches":false,"merge_ref":false}，同 build_ws_coldboot.py）→ export → W

  gateway 访问经 node proxy 按 sandbox id 转发（同 bench.py；forwardAccessToken 来自 create
  响应，鉴权用 X-Access-Token）。e2b profile 用 Sandbox.connect 拿 commands.run 起 gateway。

  - 层结构（实测）：B 无父层 → 1-layer。W 从 B 恢复而来且 pause 传 merge_ref=false
    （不 flatten 父层）→ W 链式引用 B，restore 为 2 memory layers。本 build 上
    2-layer restore 工作正常（uffd64 老 bug 未触发，bench 120+ 次零失败）；
    若需要独立 1-layer W，需要使用服务端支持的 flatten/export 流程；toTemplate 导出
    不读取导出后的源保留参数，源 sandbox 由本脚本最后的 DELETE 清理。
  - PROFILE=bare（无 envd）：gateway 由 create 时的 kuasar-sandbox.launch metadata 注入
    （标准用法，launch spec 同 make_bare_snapshot.py）。

用法：
  PROFILE=bare python3 build_bw_coldboot.py
  bench: python3 bench.py --b "$(cat openclaw-bare-snp-template.id)" --w "$(cat openclaw-bare-ws-template.id)" --probe health
"""
import base64, json, os, sys, time

import e2b_env
e2b_env.load()
e2b_env.require("E2B_DOMAIN", "E2B_API_KEY", "SSL_CERT_FILE")
from e2b import Sandbox
import openclaw_net

PARENT = os.path.dirname(os.path.abspath(__file__))
KEY = os.environ.get("MANIFEST_KEY", "085b6760c6f3309ab0acf2b2ad2cf173a3ec272c73a26e8da69c60dfecc468d3")
PROFILE = os.environ.get("PROFILE", "e2b").strip().lower()  # e2b | bare
IMAGE_NAME = os.environ.get("IMAGE_NAME", "openclaw").strip()
if PROFILE == "bare":
    DEFAULT_OUT_B = f"{IMAGE_NAME}-bare-snp-template.id"
    DEFAULT_OUT_W = f"{IMAGE_NAME}-bare-ws-template.id"
else:
    # e2b profile：命名不再重复 "bare"（那修饰镜像形态而非 profile）
    DEFAULT_OUT_B = f"{IMAGE_NAME}-e2b-snp-template.id"
    DEFAULT_OUT_W = f"{IMAGE_NAME}-e2b-ws-template.id"
OUT_B = os.environ.get("OUT_B", os.path.join(PARENT, DEFAULT_OUT_B))
OUT_W = os.environ.get("OUT_W", os.path.join(PARENT, DEFAULT_OUT_W))
SANDBOX_TIMEOUT = int(os.environ.get("SANDBOX_TIMEOUT", "600"))  # 两段式流程长，放宽空闲回收
READY_WAIT = int(os.environ.get("READY_WAIT", "120"))
WARM_CURLS = int(os.environ.get("WARM_CURLS", "4"))
WARM_HEALTH = os.environ.get("WARM_HEALTH", "1").lower() not in ("0", "false", "no")
EXEC_PROBE_TRIES = int(os.environ.get("EXEC_PROBE_TRIES", "2"))  # exec 支持探测次数：首调懒加载 bash-tools 偶发超时给一次重试；404/405 直接判不支持
READY_SETTLE = float(os.environ.get("READY_SETTLE", "20"))  # gateway ready 后、pause#1 前的 settle（让热页弄脏，避免 drop_caches 踢出快照；e2b-B 异位的根因）
DATAPLANE_PORTS = ("49983", "49999")
# 覆盖本次（及产出的快照所记录）的 allocatable.memory；=capacity（如 8GiB）时
# alloc==cap → CH 不加气球设备，用于产"无气球"对照快照。
ALLOCATABLE_MEMORY = os.environ.get("ALLOCATABLE_MEMORY", "").strip() or None
def _resource_meta(extra=None):
    meta = dict(extra or {})
    if ALLOCATABLE_MEMORY:
        meta["kuasar-sandbox.resource"] = json.dumps({"allocatable": {"memory": ALLOCATABLE_MEMORY}})
    return meta

GW_CMD = ("cd /app && setsid sh -c 'exec openclaw gateway --allow-unconfigured "
          "--verbose --bind lan --auth token --token fake-token' "
          "</dev/null >/tmp/gw.log 2>&1 & echo started")
# bare profile 的 launch（标准用法，同 make_bare_snapshot.py）：user=0:0 跳过
# sandbox-init 解析 USER=node 时读 /etc/group（vda 间歇 I/O error 会卡这）。
LAUNCH = {"exec": "/bin/sh",
          "args": ["-c", "cd /app && exec openclaw gateway --allow-unconfigured "
                   "--verbose --bind lan --auth token --token fake-token"],
          "workdir": "/app", "restart": "always", "user": "0:0"}


def add_hosts(sid, domain):
    with open("/etc/hosts") as hosts:
        lines = hosts.read().splitlines()
    add = [f"127.0.0.1 {p}-{sid}.{domain} #bwcb" for p in DATAPLANE_PORTS
           if not any(f"{p}-{sid}.{domain}" in l for l in lines)]
    if add:
        with open("/etc/hosts", "a") as hosts:
            hosts.write("\n".join(add) + "\n")


def del_hosts(sid, domain):
    with open("/etc/hosts") as hosts:
        kept = [l for l in hosts.read().splitlines()
                if not any(f"{p}-{sid}.{domain}" in l for p in DATAPLANE_PORTS)]
    with open("/etc/hosts", "w") as hosts:
        hosts.write("\n".join(kept) + "\n")


def export_sandbox(sid):
    # keepSource=True：B 导出后保留源沙箱，供 connect 恢复继续做 W
    # （#197 起 toTemplate 默认删除源，不保留则步骤③ connect 404）。
    r = e2b_env.api_request("POST", f"/sandboxes/{sid}/export",
                            json={"toTemplate": True, "keepSource": True}, timeout=180)
    if r.status_code != 200:
        sys.exit(f"✗ export HTTP {r.status_code}: {r.text[:300]}")
    return r.json().get("result", "")


def pause_sandbox(sid, policy):
    """policy=None → 不带 header（节点默认 drop_caches=true，冷快照 B）；
    否则带 X-Kuasar-Sandbox-Checkpoint（W：drop_caches=false, merge_ref=false）。"""
    headers = {"X-Kuasar-Sandbox-Checkpoint": json.dumps(policy)} if policy else {}
    r = e2b_env.api_request("POST", f"/sandboxes/{sid}/pause", headers=headers, timeout=120)
    if r.status_code not in (200, 204):
        sys.exit(f"✗ pause HTTP {r.status_code}: {r.text[:200]}")
    time.sleep(3)  # checkpoint 落盘


def connect_sandbox(sid):
    """connect 恢复 paused 沙箱（bare/e2b 同一端点；恢复后 gateway 随 guest 内存回来）。"""
    r = e2b_env.api_request("POST", f"/sandboxes/{sid}/connect",
                            json={"timeout": SANDBOX_TIMEOUT}, timeout=120)
    if r.status_code != 200:
        sys.exit(f"✗ connect HTTP {r.status_code}: {r.text[:200]}")


def wait_ready(sid, fat):
    """等 gateway /health 200（经 node proxy 按 sid 转发）。"""
    end = time.time() + READY_WAIT
    while time.time() < end:
        if openclaw_net.curl_gateway_proxy(sid, fat, "/health").get("http") == 200:
            return True
        time.sleep(2)
    return False


def probe_exec_support(sid, fat):
    """探测 gateway 是否启用 HTTP exec（exec 补丁镜像才有 /tools/invoke exec）。

    返回 (supported, note)。404/405 = 镜像没打 exec 补丁，属预期降级而非失败；
    其余失败（含首调懒加载 bash-tools 偶发超时）重试 EXEC_PROBE_TRIES 次，
    全部失败也只降级（W 少预热 exec 工作集），不让 build 失败。
    """
    last = "no attempt"
    for attempt in range(1, EXEC_PROBE_TRIES + 1):
        r = openclaw_net.curl_exec_proxy(sid, fat, "date")
        if r.get("ok") is True:
            return True, f"exec ok ({r.get('total', 0):.3f}s, attempt {attempt})"
        if r.get("http") in (404, 405):
            return False, f"HTTP {r.get('http')}：镜像未启用 HTTP exec，跳过 exec 预热"
        last = f"attempt {attempt}: http={r.get('http', 0)} err={r.get('err', '') or (r.get('text') or '')[:60]}"
        if attempt < EXEC_PROBE_TRIES:
            time.sleep(1)
    return False, f"exec 探测未通过（{last}），按不支持降级，跳过 exec 预热"


def main():
    if PROFILE not in ("e2b", "bare"):
        sys.exit(f"✗ PROFILE={PROFILE!r}（仅支持 e2b | bare）")
    tid = f"{PROFILE}-img-" + base64.urlsafe_b64encode(("manifest://" + KEY).encode()).decode().rstrip("=")
    print(f"• cold-boot [{PROFILE}] from manifest://{KEY[:12]}… 一次产出 B（冷）+ W（工作集）")
    domain = os.environ["E2B_DOMAIN"]
    s = None        # e2b 分支的 SDK Sandbox（commands.run 起 gateway）；bare 走 launch metadata
    sid = None
    fat = None      # forwardAccessToken（node proxy 鉴权，来自 create 响应）
    hosts_added = False
    if PROFILE == "bare":
        # bare：原始 API create（抓 forwardAccessToken）+ launch metadata 注入 gateway（随 boot 自起）
        r = e2b_env.api_request("POST", "/sandboxes",
                                json={"templateID": tid, "timeout": SANDBOX_TIMEOUT,
                                      "metadata": _resource_meta({"kuasar-sandbox.launch": json.dumps(LAUNCH)})})
        if r.status_code != 201:
            sys.exit(f"✗ create HTTP {r.status_code}: {r.text[:300]}")
        j = r.json()
        sid, fat = j["sandboxID"], j["forwardAccessToken"]
    else:
        # e2b：原始 API create 抓 forwardAccessToken；Sandbox.connect 拿 commands.run 起 gateway
        # （SDK 不暴露 forwardAccessToken，故 create 不走 Sandbox.create）。
        r = e2b_env.api_request("POST", "/sandboxes",
                                json={"templateID": tid, "timeout": SANDBOX_TIMEOUT,
                                      "metadata": _resource_meta()})
        if r.status_code != 201:
            sys.exit(f"✗ create HTTP {r.status_code}: {r.text[:300]}")
        j = r.json()
        sid, fat = j["sandboxID"], j["forwardAccessToken"]
        s = Sandbox.connect(sid, timeout=SANDBOX_TIMEOUT)
        add_hosts(sid, domain)   # 仅 e2b：envd 数据面域名 → 127.0.0.1
        hosts_added = True
    print(f"  ✓ created  sid={sid}  proxy={openclaw_net.proxy_url()}")
    try:
        # ① 起 gateway + 等 ready（bare：launch 已在 create 时注入，随 boot 自起）
        if PROFILE != "bare":
            # The gateway is deliberately long-lived.  A foreground
            # commands.run waits for the shell/process stream to close even
            # though the gateway was backgrounded inside that shell, which
            # turns a successful launch into a client-side timeout.  Start it
            # through envd and disconnect the output stream immediately; the
            # guest process keeps running and readiness is verified below via
            # the node-proxy health request.
            handle = s.commands.run(GW_CMD, background=True, timeout=15)
            handle.disconnect()
            print(f"  commands.run(background) started pid={handle.pid}")
        print("[1/5] 等 gateway /health 200 …")
        if not wait_ready(sid, fat):
            sys.exit(f"✗ gateway {READY_WAIT}s 未 ready")
        print("  ✓ gateway ready")
        if READY_SETTLE > 0:
            print(f"  • settle {READY_SETTLE:g}s 再 pause#1")
            time.sleep(READY_SETTLE)

        # ② B：默认策略 pause（drop_caches=true → 冷）+ export
        print("[2/5] pause#1（默认 drop_caches=true）→ export B …")
        t0 = time.time()
        pause_sandbox(sid, None)
        print(f"  ✓ paused (cold)  ({(time.time()-t0)*1000:.0f}ms)")
        t0 = time.time()
        b = export_sandbox(sid)
        print(f"  ✓ B template: {b[:60]}…  ({(time.time()-t0)*1000:.0f}ms)")
        open(OUT_B, "w").write(b + "\n")
        print(f"  ✓ wrote {OUT_B}")

        # ③ connect 恢复（gateway 随内存回来；toTemplate export 不会删除源 sandbox）
        print("[3/5] connect 恢复 …")
        t0 = time.time()
        connect_sandbox(sid)
        print(f"  ✓ resumed  ({(time.time()-t0)*1000:.0f}ms)")
        if not wait_ready(sid, fat):
            sys.exit("✗ resume 后 gateway 未 ready")
        print("  ✓ gateway ready (post-resume)")

        # ④ 预热工作集。exec 先探测支持（openclaw 镜像可能没打 HTTP exec 补丁），
        # 不支持则降级为 health-only 预热并继续 build，不失败。
        exec_supported, note = probe_exec_support(sid, fat)
        print(f"  • exec 探测: {note}")
        if not exec_supported:
            print("  ⚠ 未启用 HTTP exec：W 只预热 health 工作集（bash-tools 页不会进快照）")
        profile = ("health+exec" if WARM_HEALTH else "exec-only") if exec_supported else "health-only"
        print(f"[4/5] 预热 {profile} ×{WARM_CURLS}")
        for k in range(WARM_CURLS):
            # exec 不支持时强制 health 预热（原 WARM_HEALTH=0 的 exec-only 组合随之失效）
            rh = openclaw_net.curl_gateway_proxy(sid, fat, "/health") if (WARM_HEALTH or not exec_supported) else {"http": 200}
            if not exec_supported:
                print(f"  warm{k+1}: health={rh.get('http')}({rh.get('total', 0):.3f}s) exec=skipped")
            else:
                r = openclaw_net.curl_exec_proxy(sid, fat, "date")
                print(f"  warm{k+1}: health={rh.get('http')}({rh.get('total', 0):.3f}s) "
                      f"exec={r.get('ok')}({r.get('total', 0):.3f}s)"
                      f"{'  <<< 文件页缓存已空，这张 W 快照会是坏的' if (r.get('total') or 0) > 2 else ''}")
                if rh.get("http") != 200 or not r.get("ok"):
                    sys.exit(f"✗ warm 失败 health={rh} exec={r}")
            if rh.get("http") != 200:
                sys.exit(f"✗ warm 失败 health={rh}")
            time.sleep(0.2)
        if READY_SETTLE > 0:
            print(f"  • quiesce {READY_SETTLE:g}s 后再 pause")
            time.sleep(READY_SETTLE)

        # ⑤ W：带 header pause（drop_caches=false, merge_ref=false）+ export
        print("[5/5] pause#2（drop_caches=false, merge_ref=false）→ export W …")
        t0 = time.time()
        pause_sandbox(sid, {"drop_caches": False, "merge_ref": False})
        print(f"  ✓ paused (working-set)  ({(time.time()-t0)*1000:.0f}ms)")
        t0 = time.time()
        w = export_sandbox(sid)
        print(f"  ✓ W template: {w[:60]}…  ({(time.time()-t0)*1000:.0f}ms)")
        open(OUT_W, "w").write(w + "\n")
        print(f"  ✓ wrote {OUT_W}")

        print("\n=== done ===")
        print(f"B（冷）:     {OUT_B}")
        print(f"W（工作集）: {OUT_W}")
        print(f"bench: python3 bench.py --b \"$(cat {OUT_B})\" --w \"$(cat {OUT_W})\" --probe health")
    finally:
        # Keep temporary local aliases independent from control-plane cleanup:
        # a failed DELETE must not accumulate #bwci entries in /etc/hosts.
        if hosts_added:
            try:
                del_hosts(sid, domain)
            except Exception as e:
                print(f"⚠ cleanup hosts for {sid}: {e}", file=sys.stderr)
        if sid is not None:
            try:
                e2b_env.api_request("DELETE", f"/sandboxes/{sid}")   # kill 就是它的封装
            except Exception as e:
                print(f"⚠ cleanup sandbox {sid}: {e}", file=sys.stderr)

if __name__ == "__main__":
    main()
