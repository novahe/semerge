"""openclaw_net.py — 直连 openclaw gateway 的网络辅助（不经 envd / e2b 命令通道）。

sandbox 仍由 Sandbox.create() 恢复，但 run 阶段不再走 envd 的 commands.run，而是从 host
直接 curl gateway。链路：host 默认 netns → 路由 100.100.96.0/20 dev e2bdemom0 → eBPF
vswitch(e2bdemo) → 按 slot 做 1:1 NAT 到 guest inner IP → gateway（--bind lan，监听 0.0.0.0:18789）。

floating IP 推导（host-reachable，非 guest 内的 169.254.0.x inner IP）：
  <run_root>/<sid>/*.yaml 里 network.tapfd.exec 的 `--port=N` → floating IP = <base>.(N-1)
  base = e2bdemo vswitch 的 floating_ip_base（demo_e2b.sh --floating-ip-base=100.100.96.0）。
tap 设备同名规律：e2bdemo-tN（与 --port=N 对应，本辅助不需要 tap 名，仅作校对）。

提供：
  floating_ip(sid)        → 该 sandbox 的 host-reachable floating IP（str）或 None
  port_of(sid)            → vswitch port 号（int）或 None
  curl_gateway(ip, ...)   → curl http://<ip>:18789/<path>，返回计时 dict（curl -w 解析）
"""
import glob
import os
import re
import subprocess

# run_root 与 config/config.yaml 的 paths.run_root 一致；可用 E2B_RUN_ROOT 覆盖。
DEFAULT_RUN_ROOT = "/tmp/e2b-bench-file-opt/run"
GATEWAY_PORT = 18789
# e2bdemo vswitch floating_ip_base；可用 E2B_FIP_BASE 覆盖（端口 N → <base>.(N-1)）。
DEFAULT_FLOATING_IP_BASE = "100.100.96"
FLOATING_IP_BASE = os.environ.get("E2B_FIP_BASE", DEFAULT_FLOATING_IP_BASE)


def port_of(sid):
    """从 <run_root>/<sid>/*.yaml 的 network.tapfd.exec 取 `--port=N`（vswitch 1-based 端口号）。"""
    # Benchmark scripts call e2b_env.load() after importing this module, so read
    # the override at call time rather than freezing it during module import.
    run_root = os.environ.get("E2B_RUN_ROOT", DEFAULT_RUN_ROOT)
    for yml in glob.glob(os.path.join(run_root, sid, "*.yaml")):
        try:
            txt = open(yml).read()
        except OSError:
            continue
        m = re.search(r"--port=(\d+)", txt)
        if m:
            return int(m.group(1))
    return None


def floating_ip(sid):
    """host-reachable floating IP = <base>.(port-1)。port 来自 sandbox yaml 的 --port=N。"""
    port = port_of(sid)
    if port is None:
        return None
    base = os.environ.get("E2B_FIP_BASE", DEFAULT_FLOATING_IP_BASE)
    return f"{base}.{port - 1}"


def curl_gateway(ip, path="/health", timeout=10):
    """curl http://<ip>:18789/<path>，用 curl -w 取计时。返回 dict：
       {http, total, connect, startt}（秒，浮点）；失败 {http:0, err:<msg>}。
       - total    = 整个 curl 墙钟（用户要的「curl 耗时」）
       - connect  = TCP 握手
       - startt   = 连接 + 服务端处理到首字节；startt-connect ≈ gateway 处理（冷时含 page-in）
    """
    url = f"http://{ip}:{GATEWAY_PORT}{path}"
    fmt = "HTTP%{http_code} total=%{time_total} connect=%{time_connect} startt=%{time_starttransfer}"
    try:
        out = subprocess.run(
            ["curl", "--noproxy", "*", "-s", "-o", "/dev/null", "-w", fmt,
             "--max-time", str(timeout), url],
            capture_output=True, text=True, timeout=timeout + 2,
        )
    except subprocess.TimeoutExpired:
        return {"http": 0, "err": "curl-timeout"}
    s = out.stdout.strip()
    m = re.search(r"HTTP(\d+) total=([\d.]+) connect=([\d.]+) startt=([\d.]+)", s)
    if m:
        return {
            "http": int(m.group(1)),
            "total": float(m.group(2)),
            "connect": float(m.group(3)),
            "startt": float(m.group(4)),
        }
    return {"http": 0, "err": s or out.stderr.strip() or "no-match"}


def curl_exec(ip, command="date", timeout=15):
    """POST /tools/invoke {tool:exec} 在 gateway host 跑一条命令（需 patched image：HTTP exec 已开）。
    返回 {ok, http, total, exit_code, text}。total = curl 墙钟（含 bash-tools 模块懒加载 + spawn）。
    exec 首次会懒加载 bash-tools 运行时（重），是 working-set 探针的理想负载。"""
    import json as _json
    body = _json.dumps({"tool": "exec", "args": {"command": command, "host": "gateway"}})
    try:
        out = subprocess.run(
            ["curl", "--noproxy", "*", "-sS", "--max-time", str(timeout),
             "-H", "Authorization: Bearer fake-token",
             "-H", "Content-Type: application/json", "-d", body,
             "-w", "\n%{http_code} %{time_total}",
             f"http://{ip}:{GATEWAY_PORT}/tools/invoke"],
            capture_output=True, text=True, timeout=timeout + 2,
        )
    except subprocess.TimeoutExpired:
        return {"ok": False, "http": 0, "total": 0.0, "err": "curl-timeout"}
    s = out.stdout
    idx = s.rfind("\n")
    marker = s[idx + 1:] if idx != -1 else ""
    body_s = s[:idx] if idx != -1 else s
    m = re.search(r"(\d+)\s+([\d.]+)", marker)
    http = int(m.group(1)) if m else 0
    total = float(m.group(2)) if m else 0.0
    try:
        j = _json.loads(body_s)
    except Exception:
        j = {}
    ok = j.get("ok") is True
    details = j.get("details", {}) if ok else {}
    raw = (j.get("result", {}).get("content", [{}])[0].get("text", "") if ok
           else str(j.get("error", "")))
    return {"ok": ok, "http": http, "total": total,
            "exit_code": details.get("exitCode"), "text": raw[:80].replace("\n", " ")}


if __name__ == "__main__":
    # 手动诊断：python3 openclaw_net.py <sid>  → 打印 port / floating IP 并 curl 一次。
    import sys
    if len(sys.argv) < 2:
        sys.exit("用法: python3 openclaw_net.py <sid>")
    sid = sys.argv[1]
    port = port_of(sid)
    ip = floating_ip(sid)
    print(f"sid={sid}\n  port={port}\n  floating_ip={ip}")
    if ip:
        r = curl_gateway(ip)
        print(f"  curl /health → {r}")
