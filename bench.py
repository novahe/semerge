#!/usr/bin/env python3
"""bench.py — B（无工作集）vs W（工作集快照）冷启动对比。

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
  run      e2b SDK Sandbox.commands.run()，命令在 guest 的 envd 中执行；适用于
           e2bdev/code-interpreter 一类镜像，W 会预热 envd 命令执行工作集。

每轮：create → 立即第一次 probe → 读取 launchMs → 第二次 probe → kill。
- B = `*-bare-snp-template.id`（drop-caches=true，无预热）
- W = `*-e2b-ws-template.id` 或 `*-bare-ws-template.id`（drop-caches=false + 预热）

计时：create(launchMs) 保留 conductor 的服务端启动耗时；e2e1 是从发起 create
到 create 返回后立即执行的第一次 probe 完成的客户端端到端耗时，期间不插入
launchMs 轮询；run1(cold) = e2e1 - create(launchMs)。run2(warm) 仍只统计第二次
probe 本身。e2b `run` probe 的 SDK 目标由
E2B_SANDBOX_URL 指向 node proxy，并携带 sandbox id/port/token。
"""
import e2b_env
import argparse, json, os, re, socket, subprocess, sys, time
from datetime import datetime

e2b_env.load()
e2b_env.require("E2B_DOMAIN", "E2B_API_KEY", "SSL_CERT_FILE")
import openclaw_net
from e2b import Sandbox  # run 探针用 SDK sandbox 认证；exec/health 用 forwardAccessToken 走 node proxy

DEFAULT_RUN_COMMAND = "date"

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
    返回 (float ms, launchTiming) 或 (None, None)（launch 失败/超时）。"""
    end = time.time() + timeout
    while time.time() < end:
        r = e2b_env.api_request("GET", f"/sandboxes/{sid}")
        if r.status_code == 404:
            return None, None
        try:
            body = r.json()
            ms = body.get("launchMs")
            if ms is not None:
                return float(ms), body.get("launchTiming")
        except Exception:
            pass
        time.sleep(0.01)
    return None, None


TIMING_PHASES = (
    ("admission_to_node_prepare", "accepted", "node_prepare_done"),
    ("runner_wait", "runner_assign_start", "runner_commit_start"),
    ("runner_commit", "runner_commit_start", "runner_commit_done"),
    ("runner_handoff", "runner_commit_done", "runner_handoff_done"),
    ("sandbox_ctl_dispatch", "runner_handoff_done", "sandbox_ctl_start"),
    ("sandbox_ctl_pre_ch", "sandbox_ctl_start", "ch_start"),
    ("ch_start_to_resume", "ch_start", "ch_resume_done"),
    ("resume_to_sandbox_init_ack", "ch_resume_done", "sandbox_init_ack"),
    ("sandbox_init_ack_to_runtime_ready", "sandbox_init_ack", "runtime_ready_received"),
    ("ch_start_to_runtime_ready", "ch_start", "runtime_ready_received"),
    ("runtime_ready_to_envd_init_done", "runtime_ready_received", "envd_init_done"),
    ("envd_init_to_launch_done", "envd_init_done", "launch_done"),
    ("runtime_ready_to_launch_done", "runtime_ready_received", "launch_done"),
)

PHASE_DESCRIPTIONS = {
    "admission_to_node_prepare": "conductor 接收 create 到 node-ctl 完成节点准备",
    "runner_wait": "node-ctl 开始分配 runner 到 runner commit 开始",
    "runner_commit": "runner commit 开始到 commit 完成",
    "runner_handoff": "runner commit 完成到 sandbox runner 接手",
    "sandbox_ctl_dispatch": "runner 接手到 sandbox-ctl 开始处理",
    "sandbox_ctl_pre_ch": "sandbox-ctl 开始处理到 Cloud Hypervisor 启动",
    "ch_start_to_resume": "Cloud Hypervisor 启动到 resume 接口返回",
    "resume_to_sandbox_init_ack": "resume 返回到 sandbox-init 初始化完成并 ACK",
    "sandbox_init_ack_to_runtime_ready": "sandbox-init ACK 到 node-ctl 收到 runtime ready",
    "ch_start_to_runtime_ready": "Cloud Hypervisor 启动到 node-ctl 收到 runtime ready（bare）",
    "runtime_ready_to_envd_init_done": "node-ctl 收到 runtime ready 到 envd /init 完成（e2b）",
    "envd_init_to_launch_done": "envd /init 完成到 launch terminal/state running（e2b）",
    "runtime_ready_to_launch_done": "runtime ready 到 launch terminal/state running（bare）",
    "accountedMs": "已纳入阶段统计的耗时总和，应接近 create(launchMs)",
}

TIMING_DISPLAY_MIN_MS = 3.0
PROGRESS_WIDTH = 24


def _timing_point_key(points, name, fallback):
    """Sort a phase by its RFC3339Nano start point, with a stable fallback."""
    value = points.get(name)
    if not value:
        return (1, fallback)
    try:
        # node-ctl emits UTC RFC3339Nano values. fromisoformat handles the
        # variable fractional-second width after converting Z to an offset.
        return (0, datetime.fromisoformat(value.replace("Z", "+00:00")))
    except (TypeError, ValueError):
        return (1, fallback)


def launch_timing_rows(timing, minimum_ms=TIMING_DISPLAY_MIN_MS):
    """Return chronological phases, optionally hiding sub-3ms display noise."""
    if not timing:
        return []
    phases = timing.get("phasesMs") or {}
    points = timing.get("points") or {}
    visible = []
    for index, (name, start, _end) in enumerate(TIMING_PHASES):
        value = phases.get(name)
        if value is None or value < minimum_ms:
            continue
        visible.append((_timing_point_key(points, start, index), index, name, value))
    visible.sort(key=lambda row: (row[0], row[1]))
    return [(row[2], row[3]) for row in visible]


def run_command_probe(sandbox, command, user="root", cwd="/", timeout=30):
    """通过 SDK/envd 执行 guest 命令，返回 (ok, total_ms, text)。"""
    t0 = time.time()
    result = sandbox.commands.run(command, user=user, cwd=cwd, timeout=timeout)
    total_ms = (time.time() - t0) * 1000
    text = (getattr(result, "stdout", "") or getattr(result, "stderr", "") or "")
    ok = getattr(result, "exit_code", 1) == 0
    return ok, total_ms, text[:80].replace("\n", " ")


def probe(sid, token, mode, sandbox, command=DEFAULT_RUN_COMMAND,
          user="root", cwd="/", timeout=30):
    """返回 (ok, total_ms, text)。支持 node proxy 或 SDK/envd。"""
    if mode == "run":
        if sandbox is None:
            raise RuntimeError("run probe 缺少已连接的 Sandbox")
        return run_command_probe(sandbox, command, user, cwd, timeout)
    if mode == "exec":
        r = openclaw_net.curl_exec_proxy(sid, token, "date")
        return r.get("ok") is True, r.get("total", 0) * 1000, r.get("text", "")[:30]
    r = openclaw_net.curl_gateway_proxy(sid, token)
    ok = r.get("http") == 200
    text = "" if ok else f"HTTP {r.get('http', 0)} {r.get('err', '')}".strip()
    return ok, r.get("total", 0) * 1000, text


def _clear_progress():
    sys.stdout.write("\r" + (" " * 120) + "\r")
    sys.stdout.flush()


def _show_progress(label, completed, total):
    """Update one in-place progress line; no per-round success details."""
    total = max(total, 1)
    filled = min(PROGRESS_WIDTH, int(PROGRESS_WIDTH * completed / total))
    bar = "#" * filled + "." * (PROGRESS_WIDTH - filled)
    line = f"  {label} progress [{bar}] {completed:>{len(str(total))}}/{total}"
    sys.stdout.write("\r" + line.ljust(90))
    if completed >= total:
        sys.stdout.write("\n")
    sys.stdout.flush()


def run_rounds(template, n, label, mode, command=DEFAULT_RUN_COMMAND,
               run_user="root", run_cwd="/", run_timeout=30):
    res = {k: [] for k in ('create', 'create_api', 'e2e', 'cold', 'warm')}
    res["phases"] = {}
    res["accounted"] = []
    res["unaccounted"] = []
    _show_progress(label, 0, n)
    for i in range(n):
        sid = token = None
        sandbox = None
        try:
            e2e_start = time.perf_counter()             # 0.0 SDK/raw create start
            sid, token = create(template)
            create_return = time.perf_counter()         # 1.0 create interface return
            create_api_ms = (create_return - e2e_start) * 1000
            res['create_api'].append(create_api_ms)
            if mode == "run":
                sandbox = Sandbox.connect(sid, timeout=120)
            ok1, _probe1_ms, t1 = probe(sid, token, mode, sandbox, command, run_user, run_cwd, run_timeout)  # cold
            probe_return = time.perf_counter()           # 2.0 probe return
            e2e1 = (probe_return - e2e_start) * 1000     # 0.0 -> 2.0
            probe_e2e_ms = (probe_return - create_return) * 1000
            # 首次 probe 的 e2e 计时已经结束；这里才轮询 conductor 的精确 launchMs，
            # 避免轮询 wait_launch_ms 污染端到端耗时。
            tc, timing = wait_launch_ms(sid)
            if tc is None:
                raise RuntimeError(f"no launchMs for {sid}（launch 失败/超时）")
            run1 = e2e1 - tc if ok1 else None
            if ok1:
                ok2, c2, t2 = probe(sid, token, mode, sandbox, command, run_user, run_cwd, run_timeout)  # warm
            else:
                ok2, c2, t2 = False, None, "probe1 failed"
            if ok1:
                res['create'].append(tc); res['e2e'].append(e2e1); res['cold'].append(run1)
                if timing:
                    for name, value in launch_timing_rows(timing, minimum_ms=0.0):
                        res["phases"].setdefault(name, []).append(value)
                    if timing.get("accountedMs") is not None:
                        res["accounted"].append(float(timing["accountedMs"]))
                    if timing.get("unaccountedMs") is not None:
                        res["unaccounted"].append(float(timing["unaccountedMs"]))
            if ok2:
                res['warm'].append(c2)
            if not ok1:
                _clear_progress()
                print(f"  sid={sid} error={t1}")
            elif not ok2:
                _clear_progress()
                print(f"  sid={sid} error={t2 or 'warm probe failed'}")
        except Exception as e:
            _clear_progress()
            print(f"  sid={sid or '<none>'} error={type(e).__name__}: {e}")
        finally:
            if sandbox is not None:
                # SDK 没有公开 close()；kill() 是其 Sandbox 生命周期清理接口，
                # 同时避免只 DELETE sid 而遗留本轮 Sandbox 对象的连接状态。
                try:
                    sandbox.kill()
                except Exception:
                    try: e2b_env.api_request("DELETE", f"/sandboxes/{sid}")
                    except Exception: pass
                sandbox = None
            elif sid:
                try: e2b_env.api_request("DELETE", f"/sandboxes/{sid}")
                except Exception: pass
            _show_progress(label, i + 1, n)
    return res


SUMMARY_LABEL_WIDTH = 38
SUMMARY_VALUE_WIDTH = 9
SUMMARY_N_WIDTH = 4


def stat_cells(xs):
    """Return fixed cells for avg/p50/min/max/n without embedded labels."""
    if not xs:
        return ("-", "-", "-", "-", "-")
    xs = sorted(xs)
    n = len(xs)
    mid = n // 2
    p50 = xs[mid] if n % 2 else (xs[mid - 1] + xs[mid]) / 2
    return (f"{sum(xs) / n:.1f}", f"{p50:.1f}", f"{min(xs):.1f}",
            f"{max(xs):.1f}", str(n))


def _stat_group(cells):
    return " ".join([
        f"{cells[0]:>{SUMMARY_VALUE_WIDTH}}",
        f"{cells[1]:>{SUMMARY_VALUE_WIDTH}}",
        f"{cells[2]:>{SUMMARY_VALUE_WIDTH}}",
        f"{cells[3]:>{SUMMARY_VALUE_WIDTH}}",
        f"{cells[4]:>{SUMMARY_N_WIDTH}}",
    ])


def print_stat_table(title, rows):
    """Print a fixed-width B/W table with statistics in the column headers."""
    b_header = _stat_group(("avg(ms)", "p50(ms)", "min(ms)", "max(ms)", "n"))
    w_header = b_header
    separator = f"  {'-' * SUMMARY_LABEL_WIDTH} {'-' * len(b_header)} | {'-' * len(w_header)}"
    print(f"\n=== {title} ===")
    print(f"  {'指标':<{SUMMARY_LABEL_WIDTH}} {'B':^{len(b_header)}} | {'W':^{len(w_header)}}")
    print(f"  {'':<{SUMMARY_LABEL_WIDTH}} {b_header} | {w_header}")
    print(separator)
    for label, b_values, w_values in rows:
        b = _stat_group(stat_cells(b_values))
        w = _stat_group(stat_cells(w_values))
        print(f"  {label:<{SUMMARY_LABEL_WIDTH}} {b} | {w}")


def print_phase_summary(B, W):
    """Print a separate fixed-width B/W summary for launch phases."""
    b_phases = B.get("phases", {}) if B else {}
    w_phases = W.get("phases", {}) if W else {}

    rows = []
    for name, _start, _end in TIMING_PHASES:
        b_values = b_phases.get(name, [])
        w_values = w_phases.get(name, [])
        values = b_values + w_values
        # Hide phases whose aggregate duration is below the display threshold
        # for both B and W. Filtering by max(value) would expose a mostly
        # sub-3ms phase merely because of one outlier.
        means = [sum(group) / len(group) for group in (b_values, w_values) if group]
        if values and max(means, default=0.0) >= TIMING_DISPLAY_MIN_MS:
            rows.append((name, b_values, w_values))

    # accountedMs is a useful total beside the phase aggregate. unaccountedMs
    # is deliberately omitted here when it is only sub-3ms display noise.
    for name, key in (("accountedMs", "accounted"),):
        b_values = B.get(key, []) if B else []
        w_values = W.get(key, []) if W else []
        if b_values or w_values:
            rows.append((name, b_values, w_values))

    print_stat_table("启动阶段汇总（平均 >= 3ms）", rows)
    print("  阶段说明（单位：ms；列为 avg / p50 / min / max / n）：")
    for name, _b_values, _w_values in rows:
        description = PHASE_DESCRIPTIONS.get(name)
        if description:
            print(f"    {name:<38} {description}")


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
    ap = argparse.ArgumentParser(description="B vs W 冷启动对比（openclaw proxy 或 envd commands.run）")
    ap.add_argument("-n", "--iterations", type=int, default=10)
    ap.add_argument("--b", default=None,
                    help="B template（传入才跑 B；默认不跑）")
    ap.add_argument("--w", default=None,
                    help="W template（传入才跑 W；默认不跑）")
    ap.add_argument("--probe", choices=("exec", "health", "run"), default="exec",
                    help="探针：exec/health 走 node proxy；run 走 SDK Sandbox.commands.run/envd")
    ap.add_argument("--command", default=DEFAULT_RUN_COMMAND,
                    help="--probe run 时在 guest 中执行的命令（默认 date）")
    ap.add_argument("--run-user", default="root",
                    help="--probe run 的 guest 用户（code-interpreter manifest 默认 root）")
    ap.add_argument("--run-cwd", default="/",
                    help="--probe run 的 guest 工作目录（code-interpreter manifest 默认 /）")
    ap.add_argument("--run-timeout", type=int, default=30,
                    help="--probe run 的单次 commands.run 超时秒数")
    ap.add_argument("--docker", action="store_true",
                    help="额外重启 ws-docker-debug 并执行 Docker cold/warm 对照")
    args = ap.parse_args()
    if not args.b and not args.w:
        sys.exit("✗ 至少传一个 --b 或 --w")
    if args.docker and args.probe == "run":
        sys.exit("✗ --docker 仅支持 exec/health 探针")

    if args.probe == "run":
        hdr = [f"# working-set 对比（e2b envd，probe=commands.run）",
               f"n={args.iterations}  command={args.command}  user={args.run_user} cwd={args.run_cwd}"]
    elif args.probe == "health":
        hdr = ["# working-set 对比（openclaw + e2b/envd，node-proxy curl /health）",
               f"n={args.iterations}  proxy={openclaw_net.proxy_url()}"]
    else:
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
        B = run_rounds(args.b, args.iterations, "B", args.probe, args.command,
                       args.run_user, args.run_cwd, args.run_timeout)
    if args.w:
        print("\n=== W（工作集，drop-caches=false）===")
        W = run_rounds(args.w, args.iterations, "W", args.probe, args.command,
                       args.run_user, args.run_cwd, args.run_timeout)

    print_stat_table("汇总", [
        (f"{args.probe}1 e2e", B.get("e2e", []) if B else [], W.get("e2e", []) if W else []),
        ("create-api", B.get("create_api", []) if B else [], W.get("create_api", []) if W else []),
        ("create(launchMs)", B.get("create", []) if B else [], W.get("create", []) if W else []),
        (f"{args.probe}1 (cold)" + (" ★" if (B and W) else ""),
         B.get("cold", []) if B else [], W.get("cold", []) if W else []),
        (f"{args.probe}2 (warm)", B.get("warm", []) if B else [], W.get("warm", []) if W else []),
    ])
    print_phase_summary(B, W)

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
