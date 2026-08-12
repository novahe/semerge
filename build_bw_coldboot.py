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
KEY = os.environ.get("MANIFEST_KEY", "65c3599e5f52264c63549b5069cf313065a6255fad1c9190d5b55a3c809bfa3f")
PROFILE = os.environ.get("PROFILE", "e2b").strip().lower()  # e2b | bare
IMAGE_NAME = os.environ.get("IMAGE_NAME", "openclaw").strip()
if PROFILE == "bare":
    DEFAULT_OUT_B = f"{IMAGE_NAME}-bare-snp-template.id"
    DEFAULT_OUT_W = f"{IMAGE_NAME}-bare-ws-template.id"
else:
    DEFAULT_OUT_B = f"{IMAGE_NAME}-e2b-bare-snp-template.id"
    DEFAULT_OUT_W = f"{IMAGE_NAME}-e2b-ws-template.id"
OUT_B = os.environ.get("OUT_B", os.path.join(PARENT, DEFAULT_OUT_B))
OUT_W = os.environ.get("OUT_W", os.path.join(PARENT, DEFAULT_OUT_W))
SANDBOX_TIMEOUT = int(os.environ.get("SANDBOX_TIMEOUT", "600"))  # 两段式流程长，放宽空闲回收
READY_WAIT = int(os.environ.get("READY_WAIT", "120"))
WARM_CURLS = int(os.environ.get("WARM_CURLS", "4"))
WARM_HEALTH = os.environ.get("WARM_HEALTH", "1").lower() not in ("0", "false", "no")
QUIESCE_SECONDS = float(os.environ.get("QUIESCE_SECONDS", "0"))
READY_SETTLE = float(os.environ.get("READY_SETTLE", "2"))  # gateway ready 后、pause#1 前的 settle（让热页弄脏，避免 drop_caches 踢出快照；e2b-B 异位的根因）
DATAPLANE_PORTS = ("49983", "49999")

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
    lines = open("/etc/hosts").read().splitlines()
    add = [f"127.0.0.1 {p}-{sid}.{domain} #bwcb" for p in DATAPLANE_PORTS
           if not any(f"{p}-{sid}.{domain}" in l for l in lines)]
    if add:
        open("/etc/hosts", "a").write("\n".join(add) + "\n")


def del_hosts(sid, domain):
    kept = [l for l in open("/etc/hosts").read().splitlines()
            if not any(f"{p}-{sid}.{domain}" in l for p in DATAPLANE_PORTS)]
    open("/etc/hosts", "w").write("\n".join(kept) + "\n")


def export_sandbox(sid):
    r = e2b_env.api_request("POST", f"/sandboxes/{sid}/export",
                            json={"toTemplate": True}, timeout=180)
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
                                      "metadata": {"kuasar-sandbox.launch": json.dumps(LAUNCH)}})
        if r.status_code != 201:
            sys.exit(f"✗ create HTTP {r.status_code}: {r.text[:300]}")
        j = r.json()
        sid, fat = j["sandboxID"], j["forwardAccessToken"]
    else:
        # e2b：原始 API create 抓 forwardAccessToken；Sandbox.connect 拿 commands.run 起 gateway
        # （SDK 不暴露 forwardAccessToken，故 create 不走 Sandbox.create）。
        r = e2b_env.api_request("POST", "/sandboxes",
                                json={"templateID": tid, "timeout": SANDBOX_TIMEOUT})
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
            try:
                s.commands.run(GW_CMD, timeout=15)
            except Exception as e:
                print(f"  commands.run: {type(e).__name__}（setsid 后台不受影响）")
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

        # ④ 预热工作集
        profile = "health+exec" if WARM_HEALTH else "exec-only"
        print(f"[4/5] 预热 {profile} ×{WARM_CURLS}（聚焦 bash-tools 工作集）")
        for k in range(WARM_CURLS):
            rh = openclaw_net.curl_gateway_proxy(sid, fat, "/health") if WARM_HEALTH else {"http": 200}
            r = openclaw_net.curl_exec_proxy(sid, fat, "date")
            print(f"  warm{k+1}: health={rh.get('http')} exec_ok={r.get('ok')}")
            if rh.get("http") != 200 or not r.get("ok"):
                sys.exit(f"✗ warm 失败 health={rh} exec={r}")
            time.sleep(0.2)
        if QUIESCE_SECONDS > 0:
            print(f"  • quiesce {QUIESCE_SECONDS:g}s 后再 pause")
        # time.sleep(2)

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
        if sid is not None:
            e2b_env.api_request("DELETE", f"/sandboxes/{sid}")   # kill 就是它的封装
        if hosts_added:
            del_hosts(sid, domain)

if __name__ == "__main__":
    main()
