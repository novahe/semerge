"""openclaw_net.py — 经 node proxy 按 sandbox id 直达 openclaw gateway 的网络辅助。

sandbox 由 POST /sandboxes 恢复，run 阶段直接打 gateway HTTP，而非走 envd commands.run。
链路（生产数据面）：host → orchestrator 反向 proxy（:4443，TLS）→ 按 E2b-Sandbox-Id +
E2b-Sandbox-Port 转发到 gateway（guest 内 --bind lan，监听 0.0.0.0:18789）。header 方式，
无需改 /etc/hosts。

鉴权：X-Access-Token = POST /sandboxes 响应里的 forwardAccessToken（kat1…，仅 create 时下发；
GET /sandboxes 不返回；e2b SDK 也不暴露 → 必须用原始 API 抓，见 bench.create / build_bw）。

提供：
  proxy_url()                 → 数据面 base URL（E2B_SANDBOX_URL 或 https://<E2B_DOMAIN>）
  curl_gateway_proxy(sid,t,...) → 经 proxy GET gateway，返回计时 dict（curl -w 解析）
  curl_exec_proxy(sid,t,...)    → 经 proxy POST /tools/invoke {tool:exec}
"""
import json as _json
import os
import re
import subprocess

GATEWAY_PORT = 18789   # gateway 在 guest 内监听端口（proxy 经 E2b-Sandbox-Port 转发到此）


def proxy_url():
    """数据面 base URL：优先 E2B_SANDBOX_URL，否则 https://<E2B_DOMAIN>。"""
    return (os.environ.get("E2B_SANDBOX_URL")
            or f"https://{os.environ.get('E2B_DOMAIN', 'sandboxes.demo.local')}").rstrip("/")


def _proxy_headers(sid, token):
    """node proxy 路由 + 鉴权 header（header 方式，无需改 /etc/hosts）。"""
    return ["-H", f"E2b-Sandbox-Id: {sid}",
            "-H", f"E2b-Sandbox-Port: {GATEWAY_PORT}",
            "-H", f"X-Access-Token: {token}"]


def curl_gateway_proxy(sid, token, path="/health", timeout=None):
    """经 node proxy GET gateway（E2b-Sandbox-Id/Port + X-Access-Token=forwardAccessToken）。
    返回 {http,total,connect,appconnect,pretransfer,startt}（秒，浮点）；失败 {http:0,err:<msg>}。
    - total   = 整个 curl 墙钟（含 TLS 握手 + proxy 转发，比 floating-IP 直连多 ~18ms）
    - connect = TCP 连接；appconnect = TLS 握手完成；pretransfer = 请求可发送
    - startt  = 到首字节；startt-pretransfer 更接近 proxy/backend 处理（冷时含 page-in）"""
    if timeout is None:
        timeout = int(os.environ.get("HEALTH_PROBE_TIMEOUT", "10"))
    url = f"{proxy_url()}{path}"
    fmt = ("HTTP%{http_code} total=%{time_total} connect=%{time_connect} "
           "appconnect=%{time_appconnect} pretransfer=%{time_pretransfer} "
           "startt=%{time_starttransfer}")
    cmd = ["curl", "--noproxy", "*", "-s", "-o", "/dev/null", "-w", fmt,
           "--max-time", str(timeout)] + _proxy_headers(sid, token)
    cacert = os.environ.get("SSL_CERT_FILE")
    if cacert:
        cmd += ["--cacert", cacert]
    cmd.append(url)
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout + 2)
    except subprocess.TimeoutExpired:
        return {"http": 0, "err": "curl-timeout"}
    s = out.stdout.strip()
    m = re.search(
        r"HTTP(\d+) total=([\d.]+) connect=([\d.]+) "
        r"appconnect=([\d.]+) pretransfer=([\d.]+) startt=([\d.]+)", s
    )
    if m:
        return {"http": int(m.group(1)), "total": float(m.group(2)),
                "connect": float(m.group(3)), "appconnect": float(m.group(4)),
                "pretransfer": float(m.group(5)), "startt": float(m.group(6))}
    return {"http": 0, "err": s or out.stderr.strip() or "no-match"}


def curl_exec_proxy(sid, token, command="date", timeout=None):
    """经 node proxy POST /tools/invoke {tool:exec}（路由+鉴权同 curl_gateway_proxy）。
    返回 {ok,http,total,connect,startt,exit_code,text}。total = curl 墙钟（含
    bash-tools 模块懒加载 + spawn）；startt-connect 近似 gateway/proxy 首字节前的服务端处理。
    exec 首次会懒加载 bash-tools 运行时（重），是 working-set 探针的理想负载。
    gateway 自身还要求 --auth token（fake-token），故另带 Authorization: Bearer。"""
    if timeout is None:
        timeout = int(os.environ.get("EXEC_PROBE_TIMEOUT", "15"))
    body = _json.dumps({"tool": "exec", "args": {"command": command, "host": "gateway"}})
    fmt = "\n%{http_code} %{time_total} %{time_connect} %{time_starttransfer}"
    cmd = ["curl", "--noproxy", "*", "-sS", "--max-time", str(timeout),
           "-H", "Authorization: Bearer fake-token",
           "-H", "Content-Type: application/json", "-d", body,
           "-w", fmt] + _proxy_headers(sid, token)
    cacert = os.environ.get("SSL_CERT_FILE")
    if cacert:
        cmd += ["--cacert", cacert]
    cmd.append(f"{proxy_url()}/tools/invoke")
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout + 2)
    except subprocess.TimeoutExpired:
        return {"ok": False, "http": 0, "total": 0.0, "err": "curl-timeout"}
    s = out.stdout
    idx = s.rfind("\n")
    marker = s[idx + 1:] if idx != -1 else ""
    body_s = s[:idx] if idx != -1 else s
    m = re.search(r"(\d+)\s+([\d.]+)\s+([\d.]+)\s+([\d.]+)", marker)
    http = int(m.group(1)) if m else 0
    total = float(m.group(2)) if m else 0.0
    connect = float(m.group(3)) if m else 0.0
    startt = float(m.group(4)) if m else 0.0
    try:
        j = _json.loads(body_s)
    except Exception:
        j = {}
    ok = j.get("ok") is True
    details = j.get("details", {}) if ok else {}
    raw = (j.get("result", {}).get("content", [{}])[0].get("text", "") if ok
           else str(j.get("error", "")))
    return {"ok": ok, "http": http, "total": total,
            "connect": connect, "startt": startt,
            "exit_code": details.get("exitCode"), "text": raw[:80].replace("\n", " ")}


if __name__ == "__main__":
    # 手动诊断：python3 openclaw_net.py <sid> <forwardAccessToken>  → 经 node proxy curl 一次。
    import sys
    if len(sys.argv) < 3:
        sys.exit("用法: python3 openclaw_net.py <sid> <forwardAccessToken>")
    sid, token = sys.argv[1], sys.argv[2]
    print(f"proxy={proxy_url()}\n  sid={sid}\n  token={token[:12]}…")
    r = curl_gateway_proxy(sid, token)
    print(f"  curl /health → {r}")
