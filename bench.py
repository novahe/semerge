#!/usr/bin/env python3
"""bench.py — B（无工作集）vs W（工作集快照）冷启动对比（原生 openclaw，不经 envd）。

run 阶段经 node proxy 按 sandbox id 转发到 gateway（template --bind lan 监听 0.0.0.0:18789），
走生产数据面（贴近实际使用）：
  host → orchestrator 反向 proxy（:4443，TLS）→ E2b-Sandbox-Id/Port + X-Access-Token
       → gateway
  X-Access-Token = POST /sandboxes 响应的 forwardAccessToken（kat1…，SDK 不暴露，故 create
  走原始 API 抓取）。比 floating-IP 直连多 ~18ms/次（TLS 握手 + proxy 一跳）。

两种探针（--probe）：
  exec   ★ POST /tools/invoke {tool:exec} 在 gateway host 跑命令。首 curl 懒加载 bash-tools
         运行时（重模块）+ spawn，是 working-set 的理想负载——W（drop-caches=false + 预热）应明显快于 B。
         需 patched openclaw image（HTTP exec 已开，见 README「exec 补丁」）。
  health   GET /health（27B，轻）。只碰 gateway 核心，B/W 差异很小，作 baseline。

每轮：create（restore wall）→ 探针 cold → 探针 warm → kill。
- B = ./paused-template.id（drop-caches=true，无预热）
- W = ws-template.id（drop-caches=false + 预热；build-ws-template.py 产出）
"""
import e2b_env
import argparse, json, os, re, socket, subprocess, sys, time

e2b_env.load()
e2b_env.require("E2B_DOMAIN", "E2B_API_KEY", "SSL_CERT_FILE")
import openclaw_net

PARENT = os.path.dirname(os.path.abspath(__file__))


def create(template, tries=4):
    """POST /sandboxes 带重试，返回 (sid, forwardAccessToken)。
    走原始 API 而非 SDK：SDK 不暴露 forwardAccessToken（node proxy 鉴权必需），且能避开
    SDK 把 orchestrator 偶发 500 吞成 KeyError('code')。"""
    last = None
    for _ in range(tries):
        try:
            r = e2b_env.api_request("POST", "/sandboxes",
                                    json={"templateID": template, "timeout": 120})
            if r.status_code == 201:
                j = r.json()
                return j["sandboxID"], j["forwardAccessToken"]
            last = RuntimeError(f"create HTTP {r.status_code}: {r.text[:200]}")
        except Exception as e:
            last = e
        time.sleep(1.0)
    raise last


def wait_launch_ms(sid, timeout=60):
    """node-ctl create 异步返回（admission 即返回）；轮询 GET /sandboxes/{sid}
    直到出现 launchMs——这是 conductor 在 launch terminal 写入的【服务端精确】boot
    耗时（acceptedAt→terminal，含 restore/runtime-ready/envd-init，ms，亚毫秒精度）。
    轮询只为同步检测（值与日志 starting_total_duration 完全一致），不引入客户端计时偏差。
    返回 float ms 或 None（launch 失败/超时）。"""
    end = time.time() + timeout
    while time.time() < end:
        r = e2b_env.api_request("GET", f"/sandboxes/{sid}")
        if r.status_code == 404:
            return None
        try:
            ms = r.json().get("launchMs")
            if ms is not None:
                return float(ms)
        except Exception:
            pass
        time.sleep(0.01)
    return None


def probe(sid, token, mode):
    """返回 (ok, total_ms, text)。经 node proxy 按 sid 转发到 gateway。"""
    if mode == "exec":
        r = openclaw_net.curl_exec_proxy(sid, token, "date")
        return r.get("ok") is True, r.get("total", 0) * 1000, r.get("text", "")[:30]
    r = openclaw_net.curl_gateway_proxy(sid, token)
    ok = r.get("http") == 200
    return ok, r.get("total", 0) * 1000, ""


def run_rounds(template, n, label, mode):
    res = {k: [] for k in ('create', 'cold', 'warm')}
    for i in range(n):
        sid = token = None
        try:
            sid, token = create(template)
            # create 异步返回（admission）；tc 用 conductor 服务端精确值 launchMs
            # （轮询只为检测其出现，值本身精确、无客户端计时偏差）。
            tc = wait_launch_ms(sid)
            if tc is None:
                raise RuntimeError(f"no launchMs for {sid}（launch 失败/超时）")
            ok1, c1, t1 = probe(sid, token, mode)      # cold
            ok2, c2, t2 = probe(sid, token, mode)      # warm
            if ok1:
                res['create'].append(tc); res['cold'].append(c1)
            if ok2:
                res['warm'].append(c2)
            print(f"  [{label} {i+1}/{n}] sid={sid[:8]}.. create {tc:5.0f}  "
                  f"{mode}1(cold)={c1:5.0f}  {mode}2(warm)={c2:4.0f}  "
                  + ('✓ ' + t1[:24] if ok1 else f"✗ {t1}"))
        except Exception as e:
            print(f"  [{label} {i+1}] ✗ {type(e).__name__}: {e}")
        finally:
            if sid:
                try: e2b_env.api_request("DELETE", f"/sandboxes/{sid}")
                except Exception: pass
    return res


def stats(xs):
    if not xs:
        return "(无样本)"
    xs = sorted(xs); n = len(xs)
    return f"avg={sum(xs)/n:.0f}ms p50={xs[n//2]:.0f}ms min={min(xs):.0f}ms max={max(xs):.0f}ms (n={n})"


def docker_curl(mode, port=18790):
    """Docker gateway 的单次 probe，返回 (ok, milliseconds)。"""
    if mode == "exec":
        body = json.dumps({"tool": "exec", "args": {"command": "date", "host": "gateway"}})
        cmd = ["curl", "--noproxy", "*", "-sS", "-H", "Authorization: Bearer fake-token",
               "-H", "Content-Type: application/json", "-d", body, "-o", "/dev/null",
               "-w", "HTTP%{http_code} total=%{time_total}", f"http://127.0.0.1:{port}/tools/invoke"]
    else:
        cmd = ["curl", "--noproxy", "*", "-sS", "-o", "/dev/null",
               "-w", "HTTP%{http_code} total=%{time_total}", f"http://127.0.0.1:{port}/health"]
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=20)
    except (OSError, subprocess.TimeoutExpired):
        return False, 0.0
    m = re.fullmatch(r"HTTP(\d+) total=([\d.]+)", out.stdout.strip())
    if not m:
        return False, 0.0
    return m.group(1) == "200", float(m.group(2)) * 1000


def run_docker_probe(mode, container="ws-docker-debug", port=18790):
    """重启 Docker gateway 后执行一次 cold 和一次 warm probe。"""
    try:
        subprocess.run(["docker", "restart", container], check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as e:
        print(f"  Docker restart 失败: {e}")
        return None
    deadline = time.time() + 30
    while time.time() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=1):
                break
        except OSError:
            time.sleep(0.25)
    else:
        print("  Docker 端口未就绪")
        return None
    # TCP listener 先于 gateway handler 就绪；重试只吸收启动期 connection reset，
    # 第一个 HTTP 200 才记为 cold 样本，不依赖容器 healthcheck 的实现。
    time.sleep(2)
    ok1, cold = False, 0.0
    while time.time() < deadline and not ok1:
        ok1, cold = docker_curl(mode, port)
        if not ok1:
            time.sleep(0.5)
    if not ok1:
        print("  Docker gateway 未返回 HTTP 200")
        return None
    ok2, warm = docker_curl(mode, port)
    print(f"  [Docker] {mode} cold={cold:.0f}ms warm={warm:.0f}ms "
          f"{'✓' if ok1 and ok2 else '✗'}")
    return cold, warm


def main():
    ap = argparse.ArgumentParser(description="B vs W 冷启动对比（原生 openclaw，host 经 tap 直 curl/exec）")
    ap.add_argument("-n", "--iterations", type=int, default=10)
    ap.add_argument("--b", default=None,
                    help="B template（传入才跑 B；默认不跑）")
    ap.add_argument("--w", default=None,
                    help="W template（传入才跑 W；默认不跑）")
    ap.add_argument("--probe", choices=("exec", "health"), default="exec",
                    help="探针：exec（/tools/invoke，重，需 patched image）或 health（/health，轻 baseline）")
    ap.add_argument("--docker", action="store_true",
                    help="额外重启 ws-docker-debug 并执行 Docker cold/warm 对照")
    args = ap.parse_args()
    if not args.b and not args.w:
        sys.exit("✗ 至少传一个 --b 或 --w")

    hdr = [f"# working-set 对比（原生 openclaw，probe={args.probe}，经 node proxy 按 sid 转发）",
           f"n={args.iterations}  proxy={openclaw_net.proxy_url()}"]
    if args.b:
        hdr.append(f"B: {args.b[:48]}…")
    if args.w:
        hdr.append(f"W: {args.w[:48]}…")
    print("\n".join(hdr))

    B = W = None
    if args.b:
        print("\n=== B（无工作集，drop-caches=true）===")
        B = run_rounds(args.b, args.iterations, "B", args.probe)
    if args.w:
        print("\n=== W（工作集，drop-caches=false）===")
        W = run_rounds(args.w, args.iterations, "W", args.probe)

    print("\n=== 汇总 ===")
    def col(label, key):
        parts = []
        if B:
            parts.append(f"B {stats(B[key])}")
        if W:
            parts.append(f"W {stats(W[key])}")
        print(f"  {label:<16} " + " | ".join(parts))
    col("create", "create")
    col(f"{args.probe}1 (cold)" + (" ★" if (B and W) else ""), "cold")
    col(f"{args.probe}2 (warm)", "warm")

    if B and W:
        def sp(key, name):
            if B[key] and W[key]:
                b, w = sum(B[key]) / len(B[key]), sum(W[key]) / len(W[key])
                if w > 0:
                    print(f"  {name} 提速: {b/w:.2f}×  ({b:.0f}→{w:.0f}ms)")
        print()
        sp('cold', f"{args.probe} cold")
        if B['create'] and W['create']:
            cb, cw = sum(B['create']) / len(B['create']), sum(W['create']) / len(W['create'])
            print(f"  create 差异: {cb-cw:+.0f}ms（预期 ≈0）")
    if args.docker:
        print("\n=== Docker 对照 ===")
        run_docker_probe(args.probe)


if __name__ == "__main__":
    main()
