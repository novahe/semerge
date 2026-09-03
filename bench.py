#!/usr/bin/env python3
"""bench.py — B（无工作集）vs W（工作集快照）冷启动对比。

run 阶段经 node proxy 按 sandbox id 转发到 gateway（template --bind lan 监听 0.0.0.0:18789），
走生产数据面（贴近实际使用）：
  host → orchestrator 反向 proxy（:4443，TLS）→ E2b-Sandbox-Id/Port + X-Access-Token
       → gateway
  X-Access-Token = POST /sandboxes 响应的 forwardAccessToken（kat1…，SDK 不暴露，故 create
  走原始 API 抓取）。比 floating-IP 直连多 ~18ms/次（TLS 握手 + proxy 一跳）。

三种主要探针（--probe）：
  exec   ★ POST /tools/invoke {tool:exec} 在 gateway host 跑命令。首 curl 懒加载 bash-tools
         运行时（重模块）+ spawn，是 working-set 的理想负载——W（drop-caches=false + 预热）应明显快于 B。
         需 patched openclaw image（HTTP exec 已开，见 README「exec 补丁」）。
  health   GET /health（27B，轻）。只碰 gateway 核心，B/W 差异很小，作 baseline。
  command  e2b SDK Sandbox.commands.run()，命令在 guest 的 envd 中执行；适用于
           e2bdev/code-interpreter 一类镜像，W 会预热 envd 命令执行工作集。
  code     e2b code-interpreter SDK Sandbox.run_code()，经 49999/execute → Jupyter；
           W 会预热 Python/Jupyter 执行工作集。

普通模式每轮：create → 立即第一次 probe → 等待 state=running → 第二次 probe → kill。
吞吐模式（--duration，支持 health/command）：持续执行 create → 首次 probe 成功，
首次 probe 返回即完成一个业务事务；warm probe、launch timing 和 kill 不计入事务成功条件。
- B = `*-snp-template.id`（drop-caches=true，无预热）
- W = `*-ws-template.id`（drop-caches=false + 预热）

计时：create(launchMs) 保留 conductor 的服务端启动耗时；没有 timing patch 时，
接口只提供 state=running，因此 create/cold/phase 指标不计入汇总；e2e1 是从发起 create
到 create 返回后立即执行的第一次 probe 完成的客户端端到端耗时，期间不插入
launchMs 轮询；run1(cold) = e2e1 - create(launchMs)。run2(warm) 仍只统计第二次
  probe 本身。e2b `command`/`code` probe 的 SDK 目标由
E2B_SANDBOX_URL 指向 node proxy，并携带 sandbox id/port/token。
"""
import e2b_env
import argparse, json, os, re, shutil, socket, subprocess, sys, threading, time, unicodedata
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime

PARENT = os.path.dirname(os.path.abspath(__file__))

e2b_env.load()
e2b_env.require("E2B_DOMAIN", "E2B_API_KEY", "SSL_CERT_FILE")
import openclaw_net

# restore 会把快照物化到节点 checkpoint.local_dir/<sid>/（~1GiB/次）；DELETE 不清理
# sandbox 的本地目录，会把磁盘写满并让回写脉冲污染后续样本（dispatch/pre_ch 毛刺）。
# 每样本删除 sandbox 后，顺手清掉当前 NS 的 run_root/<sid>、base_root/<sid> 和
# checkpoint.local_dir/<sid>。不能只从 E2B_ENV_FILE 的目录找 conductor：deploy-ns.sh
# 的冒烟流程会把 tenant.env 复制到 smoke-tmp；此时必须用 tenant 的 NS 回到对应
# <ns>-work/etc/node-ctl-<ns>/conductor.yaml，避免误清理另一个 NS。
def _namespace_from_path(path):
    """Infer an NS from ``node-ctl-<ns>`` or ``<ns>-work`` path components."""
    if not path:
        return None
    for component in reversed(os.path.abspath(path).split(os.sep)):
        if component.startswith("node-ctl-") and len(component) > len("node-ctl-"):
            return component[len("node-ctl-"):]
        if component.endswith("-work") and len(component) > len("-work"):
            return component[:-len("-work")]
    return None


def _namespace_from_endpoint(endpoint):
    """Infer an NS from domains such as ``sandboxes.nova.local``."""
    if not endpoint:
        return None
    match = re.search(
        r"(?:^|[./])sandboxes\.([A-Za-z0-9_-]+)(?:[.:/]|$)", endpoint
    )
    return match.group(1) if match else None


def _resolve_namespace(env_file):
    values = [
        ("E2B_NAMESPACE", os.environ.get("E2B_NAMESPACE")),
        ("E2B_ENV_FILE", _namespace_from_path(env_file)),
        ("E2B_DOMAIN", _namespace_from_endpoint(os.environ.get("E2B_DOMAIN"))),
        ("E2B_API_URL", _namespace_from_endpoint(os.environ.get("E2B_API_URL"))),
    ]
    discovered = [(source, value) for source, value in values if value]
    namespaces = {value for _source, value in discovered}
    if len(namespaces) > 1:
        detail = ", ".join(f"{source}={value}" for source, value in discovered)
        raise RuntimeError(f"E2B namespace mismatch: {detail}")
    return next(iter(namespaces), None)


def _resolve_workdir(env_file, namespace):
    explicit = os.environ.get("E2B_WORK_DIR")
    if explicit:
        return _namespace_checked_path(explicit, namespace, "E2B_WORK_DIR")
    if not namespace:
        return None
    current = os.path.dirname(os.path.abspath(env_file))
    target = f"{namespace}-work"
    while current != os.path.dirname(current):
        if os.path.basename(current) == target:
            return current
        current = os.path.dirname(current)
    return os.path.join(os.path.dirname(PARENT), target)


def _resolve_conductor_file(env_file, namespace, workdir):
    explicit = os.environ.get("E2B_CONDUCTOR_FILE")
    candidates = [explicit] if explicit else []
    candidates.append(os.path.join(os.path.dirname(os.path.abspath(env_file)), "conductor.yaml"))
    if workdir and namespace:
        candidates.append(os.path.join(
            workdir, "etc", f"node-ctl-{namespace}", "conductor.yaml"
        ))
    for candidate in candidates:
        if not candidate or not os.path.isfile(candidate):
            continue
        config_namespace = _namespace_from_path(candidate)
        if config_namespace and namespace and config_namespace != namespace:
            raise RuntimeError(
                f"conductor namespace mismatch: expected {namespace}, "
                f"got {config_namespace} ({candidate})"
            )
        return _namespace_checked_path(candidate, namespace, "conductor")
    return None


def _namespace_checked_path(path, namespace, source):
    resolved = os.path.abspath(path)
    path_namespace = _namespace_from_path(resolved)
    if path_namespace and namespace and path_namespace != namespace:
        raise RuntimeError(
            f"{source} namespace mismatch: expected {namespace}, "
            f"got {path_namespace} ({resolved})"
        )
    return resolved


ENV_FILE = os.path.abspath(
    os.environ.get("E2B_ENV_FILE") or os.path.join(PARENT, "e2b-env.sh")
)
RUNTIME_NAMESPACE = _resolve_namespace(ENV_FILE)
RUNTIME_WORKDIR = _resolve_workdir(ENV_FILE, RUNTIME_NAMESPACE)
CONDUCTOR_FILE = _resolve_conductor_file(
    ENV_FILE, RUNTIME_NAMESPACE, RUNTIME_WORKDIR
)


# 路径可用环境变量显式覆盖，否则从当前 NS 的 conductor.yaml 读取，再按当前
# <ns>-work 兜底。没有可靠的 NS/workdir 时返回 None，清理逻辑会报错而不会猜
# 到宿主机的共享目录。
def _resolve_runtime_dir(env_name, config_key, fallback):
    explicit = os.environ.get(env_name)
    if explicit:
        return _namespace_checked_path(explicit, RUNTIME_NAMESPACE, env_name)
    if CONDUCTOR_FILE:
        try:
            with open(CONDUCTOR_FILE, "r", encoding="utf-8") as f:
                for line in f:
                    # flow 映射形态：paths: { run_root: /path, base_root: /path, ... }
                    # 或 checkpoint: { mode: local, local_dir: /path, ... }。
                    m = re.search(rf"{re.escape(config_key)}\s*:\s*([^,}}]+)", line)
                    if m:
                        return _namespace_checked_path(
                            m.group(1).strip().strip("'\"").rstrip("\\"),
                            RUNTIME_NAMESPACE,
                            config_key,
                        )
        except OSError:
            pass
    if RUNTIME_WORKDIR:
        return _namespace_checked_path(
            os.path.join(RUNTIME_WORKDIR, fallback),
            RUNTIME_NAMESPACE,
            fallback,
        )
    return None

CHECKPOINT_DIR = _resolve_runtime_dir(
    "E2B_CHECKPOINT_DIR", "local_dir", os.path.join("var", "lib-saved")
)
BASE_DIR = _resolve_runtime_dir(
    "E2B_BASE_DIR", "base_root", os.path.join("var", "lib")
)
RUN_DIR = _resolve_runtime_dir("E2B_RUN_DIR", "run_root", "run")


def _remove_sandbox_directory(root, sid):
    """Remove exactly ``root/<sid>`` and return an error string on failure.

    The SID is validated as a single path component so a malformed API response
    can never turn this cleanup into a parent-directory deletion.
    """
    if not root or not sid:
        return None
    if not isinstance(sid, str) or sid in (".", "..") or os.path.basename(sid) != sid:
        return f"unsafe sandbox id for {root!r}: {sid!r}"
    directory = os.path.join(root, sid)
    last_error = None
    for attempt in range(3):
        try:
            if not os.path.lexists(directory):
                return None
            if os.path.islink(directory) or not os.path.isdir(directory):
                os.remove(directory)
            else:
                shutil.rmtree(directory)
            if not os.path.lexists(directory):
                return None
            last_error = "path still exists after removal"
        except FileNotFoundError:
            return None
        except OSError as exc:
            last_error = f"{type(exc).__name__}: {exc}"
        if attempt < 2:
            time.sleep(0.3 * (attempt + 1))
    return f"{directory}: {last_error}"


def cleanup_checkpoint_materialization(sid):
    """Best-effort 删除本样本的快照物化目录，返回可记录的错误。"""
    return _remove_sandbox_directory(CHECKPOINT_DIR, sid)


def cleanup_sandbox_directories(sid, namespace=None):
    """Remove all per-sandbox directories after the control-plane delete.

    Only the SID child is removed; runner pool directories and shared logs are
    intentionally left alone. The caller decides whether deletion of the
    sandbox itself completed successfully before invoking this function.
    """
    namespace = RUNTIME_NAMESPACE if namespace is None else namespace
    if namespace != RUNTIME_NAMESPACE:
        return [
            f"namespace mismatch: requested {namespace!r}, "
            f"configured {RUNTIME_NAMESPACE!r}"
        ]
    if not RUNTIME_NAMESPACE:
        return ["namespace unresolved; refusing local directory cleanup"]

    errors = []
    for name, root in (
        ("run_root", RUN_DIR),
        ("base_root", BASE_DIR),
        ("checkpoint.local_dir", CHECKPOINT_DIR),
    ):
        if not root:
            errors.append(f"ns={RUNTIME_NAMESPACE} {name}: path unresolved")
            continue
        error = _remove_sandbox_directory(root, sid)
        if error:
            errors.append(f"ns={RUNTIME_NAMESPACE} {name}: {error}")
    return errors


def _delete_sandbox(sid, sandbox):
    """Delete the sandbox and report whether its local files are safe to remove."""
    if not sid:
        return True, []

    errors = []
    if sandbox is not None:
        try:
            response = sandbox.kill()
            status = getattr(response, "status_code", None)
            if isinstance(status, int) and status != 404 and not 200 <= status < 300:
                raise RuntimeError(f"SDK kill HTTP {status}")
            return True, []
        except Exception as exc:
            errors.append(f"SDK kill failed: {type(exc).__name__}: {exc}")

    try:
        response = e2b_env.api_request("DELETE", f"/sandboxes/{sid}")
        status = getattr(response, "status_code", None)
        if isinstance(status, int) and status != 404 and not 200 <= status < 300:
            errors.append(f"API delete HTTP {status}")
            return False, errors
        return True, []
    except Exception as exc:
        errors.append(f"API delete failed: {type(exc).__name__}: {exc}")
        return False, errors
from e2b import Sandbox  # run 探针用 SDK sandbox 认证；exec/health 用 forwardAccessToken 走 node proxy
from e2b_code_interpreter import Sandbox as CodeSandbox  # code 探针走 CI SDK /execute（49999）

DEFAULT_RUN_COMMAND = "date"
DEFAULT_RUN_CODE = "print('hello kuasar-sandbox {index}')"



def create(template, tries=4, allocatable_memory=None, restore_prefetch=None):
    """POST /sandboxes 带重试，返回 (sid, forwardAccessToken)。
    走原始 API 而非 SDK：SDK 不暴露 forwardAccessToken（node proxy 鉴权必需），且能避开
    SDK 把 orchestrator 偶发 500 吞成 KeyError('code')。

    ``allocatable_memory`` 为空时保持旧行为；指定时与快照制作脚本一致，
    通过 kuasar-sandbox.resource 覆盖本次 sandbox 的 allocatable.memory。
    ``restore_prefetch`` 指定时通过 kuasar-sandbox.restore 注入 restore 预取策略。
    """
    last = None
    for _ in range(tries):
        try:
            body = {"templateID": template, "timeout": 120}
            metadata = {}
            if allocatable_memory:
                metadata["kuasar-sandbox.resource"] = json.dumps({
                    "allocatable": {"memory": allocatable_memory},
                })
            if restore_prefetch:
                metadata["kuasar-sandbox.restore"] = json.dumps({
                    "prefetch": restore_prefetch,
                })
            if metadata:
                body["metadata"] = metadata
            r = e2b_env.api_request("POST", "/sandboxes",
                                    json=body)
            if r.status_code == 201:
                j = r.json()
                return j["sandboxID"], j["forwardAccessToken"]
            last = RuntimeError(f"create HTTP {r.status_code}: {r.text[:200]}")
        except Exception as e:
            last = e
        time.sleep(1.0)
    raise last


class LaunchFailure(RuntimeError):
    """The control plane reached a terminal launch failure for this SID."""


def wait_launch_ms(sid, timeout=60):
    """node-ctl create 异步返回（admission 即返回）；轮询 GET /sandboxes/{sid}
    直到 launch terminal 成功且 state=running——这是 conductor 在 launch terminal
    写入的【服务端精确】boot 耗时（acceptedAt→terminal，含 restore/runtime-ready/
    envd-init，ms，亚毫秒精度）。失败样本也可能带 launchMs，因此不能只检查
    该字段；否则真正的 readiness EOF 会被下一次数据面 probe 伪装成 HTTP 404。
    轮询只为同步检测（值与日志 starting_total_duration 完全一致）。没有 timing patch
    时服务端不会公开 launchMs/launchTiming；此时 state=running 仍是成功信号，返回
    (None, None)，由调用方跳过依赖服务端计时的指标。成功返回 (float ms, launchTiming)
    或 (None, None)；失败/删除直接抛出 LaunchFailure。"""
    end = time.time() + timeout
    while time.time() < end:
        r = e2b_env.api_request("GET", f"/sandboxes/{sid}")
        if r.status_code == 404:
            raise LaunchFailure(f"launch failure: sandbox {sid} disappeared (GET HTTP 404)")
        try:
            body = r.json()
            launch_result = body.get("launchResult", body.get("launch_result"))
            if isinstance(launch_result, dict):
                result = launch_result.get("result")
                detail = launch_result.get("error") or launch_result.get("err")
            else:
                result = launch_result
                detail = body.get("launchError") or body.get("launch_error")
            if body.get("launchSuccess") is False or result in ("failure", "failed"):
                suffix = f": {detail}" if detail else ""
                raise LaunchFailure(f"launch failure for {sid}{suffix}")
            if body.get("state") == "dead":
                raise LaunchFailure(f"launch failure: sandbox {sid} state=dead{(': ' + str(detail)) if detail else ''}")
            ms = body.get("launchMs")
            # A failed launch can publish launchMs before rollback removes the
            # row. Accept it only once the authoritative state is running.
            if ms is not None and body.get("state") == "running":
                return float(ms), body.get("launchTiming")
            if ms is None and body.get("state") == "running":
                return None, None
        except LaunchFailure:
            raise
        except (ValueError, TypeError, KeyError):
            pass
        time.sleep(0.01)
    raise LaunchFailure(f"launch failure/timeout: sandbox {sid} did not reach state=running")


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
    "admission_to_node_prepare": "conductor 收到创建请求，到选好节点、完成准备",
    "runner_wait": "给沙箱分配执行器（runner），含排队等待",
    "runner_commit": "runner 登记接管这个沙箱",
    "runner_handoff": "runner 接管收尾、准备拉起沙箱进程",
    "sandbox_ctl_dispatch": "拉起 sandbox-ctl 进程（systemd 启动 + 进程自举 + 解析参数），到它开始干活",
    "sandbox_ctl_pre_ch": "sandbox-ctl 备料：打开快照、重建磁盘、申请内存、起后端服务，直到拉起虚拟机",
    "ch_start_to_resume": "虚拟机进程启动、装载快照状态，到『恢复运行』(/vm.resume) 接口返回",
    "resume_to_sandbox_init_ack": "虚拟机真正跑起来：客户机内核恢复运行，里面的 sandbox-init 初始化完成并向宿主机报到",
    "sandbox_init_ack_to_runtime_ready": "客户机报到传导到 node-ctl，标记为『运行时就绪』",
    "ch_start_to_runtime_ready": "虚拟机启动到 node-ctl 确认就绪（bare 镜像无 envd，走此路径）",
    "runtime_ready_to_envd_init_done": "虚拟机就绪后，向客户机里的 envd 服务下发初始化配置（要等 envd 起来、失败重试），直到成功",
    "envd_init_to_launch_done": "envd 配置完成，conductor 把沙箱标记为 running（e2b）",
    "runtime_ready_to_launch_done": "运行时就绪到 conductor 标记 running（bare 镜像路径）",
    "accountedMs": "上面各阶段耗时加总（校验用）：应≈ create(launchMs)，差值是未覆盖的缝隙（unaccountedMs）",
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


def run_code_probe(sandbox, code, timeout=30):
    """经 code-interpreter(/execute → FastAPI:49999 → Jupyter)执行代码。
    严格单次执行，不重试——cold 样本必须是真正的第一次。"""
    t0 = time.perf_counter()
    result = sandbox.run_code(code, timeout=timeout)
    total_ms = (time.perf_counter() - t0) * 1000
    ok = result.error is None
    text = ""
    if not ok:
        text = (result.error.value or "")[:80].replace("\n", " ")
    else:
        # print() 的输出走 logs.stdout（字符串列表），不是 results[].text
        # （那是富文本返回值 repr）。优先 stdout，落空再取 results 文本。
        stdout = getattr(result.logs, "stdout", None) or []
        if stdout:
            text = " ".join(str(s) for s in stdout)[:80].replace("\n", " ")
        elif result.results:
            text = (result.results[0].text or "")[:80].replace("\n", " ")
        if not text.strip():
            ok = False
            text = "empty execution stream"
    return ok, total_ms, text


def code_with_index(code, index):
    """Only the built-in smoke probe gets a changing payload.

    A caller-supplied --run-code remains verbatim: its braces may be valid
    Python and must not be interpreted as a formatter template.
    """
    if code == DEFAULT_RUN_CODE:
        return code.format(index=index)
    return code


def connect_code_sandbox(sid):
    """e2b 的 49999 经 ci.sock，必须使用 connect 返回的 envdAccessToken."""
    sandbox = CodeSandbox.connect(sid, timeout=120)
    if not sandbox._envd_access_token:
        raise RuntimeError("e2b connect returned no envdAccessToken for 49999")
    return sandbox


def probe(sid, token, mode, sandbox, command=DEFAULT_RUN_COMMAND,
          user="root", cwd="/", timeout=30):
    """返回 (ok, total_ms, text)。支持 node proxy 或 SDK/envd/CI。"""
    if mode == "command":
        if sandbox is None:
            raise RuntimeError("command probe 缺少已连接的 Sandbox")
        return run_command_probe(sandbox, command, user, cwd, timeout)
    if mode == "code":
        if sandbox is None:
            raise RuntimeError("code probe 缺少已连接的 CodeSandbox")
        return run_code_probe(sandbox, command, timeout)
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


def _show_time_progress(label, deadline, accepted, attempted, started=None,
                        refresh=0.25):
    """按时间窗口渲染 duration 模式进度条，返回停止函数。

    ``accepted``/``attempted`` 是每次渲染前重新求值的回调，这样即使当前
    事务很慢（create+probe 数秒），进度线仍按已流逝时间持续刷新。
    ``started`` 必须传入定义 deadline 的那个时间戳：进度条若在 worker
    启动后才取当前时钟，线程池 + 首笔 create 的 TLS 握手会推迟这里
    ~100ms，total 被截短，终帧永远到不了 20.0/20s。
    停止函数渲染最终状态并停止后台 ticker；worker 全部结束后调用。
    """
    if started is None:
        started = time.perf_counter()
    total = max(deadline - started, 1e-9)

    def render(final=False):
        now = time.perf_counter()
        elapsed = min(now - started, total)
        filled = min(PROGRESS_WIDTH, int(PROGRESS_WIDTH * elapsed / total))
        bar = "#" * filled + "." * (PROGRESS_WIDTH - filled)
        # deadline 后的尾巴显示为 +N.Ns；在途事务仍会拖高 measured_s。
        overtime = max(now - deadline, 0.0)
        tail = f" +{overtime:.1f}s" if overtime > 0.05 else ""
        line = (f"  {label} wall [{bar}] {elapsed:>5.1f}/{total:.0f}s"
                f" acc={accepted()} att={attempted()}{tail}")
        sys.stdout.write("\r" + line.ljust(100))
        if final:
            sys.stdout.write("\n")
        sys.stdout.flush()

    render()
    done = threading.Event()

    def tick():
        while not done.wait(refresh):
            render()

    ticker = threading.Thread(target=tick, daemon=True)
    ticker.start()

    def stop():
        done.set()
        ticker.join()
        render(final=True)

    return stop


def _new_results():
    res = {k: [] for k in ('create', 'create_api', 'e2e', 'cold', 'warm')}
    res["phases"] = {}
    res["accounted"] = []
    res["unaccounted"] = []
    res["samples"] = []
    res["discarded"] = 0
    # 吞吐模式使用单调时钟记录业务事务的开始和完成。普通模式保持这些
    # 字段为空，避免把两种统计口径混在同一个汇总里。
    res["qps_starts"] = []
    res["qps_completions"] = []
    res["qps_ends"] = []
    res["cleanup_failed"] = 0
    res["stop_worker"] = False
    return res


def _discard_result(res):
    """Remove a just-built sample when its post-probe cleanup did not finish."""
    for key in ('create', 'create_api', 'e2e', 'cold', 'warm', 'accounted', 'unaccounted'):
        res[key].clear()
    res["phases"].clear()
    res["samples"].clear()
    res["qps_starts"].clear()
    res["qps_completions"].clear()
    res["qps_ends"].clear()


def _merge_results(dst, src):
    for key in ('create', 'create_api', 'e2e', 'cold', 'warm', 'accounted', 'unaccounted'):
        dst[key].extend(src[key])
    for name, values in src["phases"].items():
        dst["phases"].setdefault(name, []).extend(values)
    dst["samples"].extend(src["samples"])
    dst["qps_starts"].extend(src["qps_starts"])
    dst["qps_completions"].extend(src["qps_completions"])
    dst["qps_ends"].extend(src["qps_ends"])
    dst["cleanup_failed"] += src["cleanup_failed"]
    dst["stop_worker"] = dst["stop_worker"] or src["stop_worker"]
    dst["discarded"] += src["discarded"]


def run_round(template, mode, command=DEFAULT_RUN_COMMAND,
              run_user="root", run_cwd="/", run_timeout=30,
              allocatable_memory=None, sample_index=0,
              transaction_only=False):
    """Run one isolated sandbox lifecycle and return (results, failure)."""
    if transaction_only and mode not in ("health", "command"):
        raise ValueError("transaction_only mode requires --probe health or command")
    res = _new_results()
    sid = token = None
    sandbox = None
    error = None
    cleanup_errors = []
    create_api_ms = e2e1 = tc = None
    try:
        e2e_start = time.perf_counter()             # 0.0 SDK/raw create start
        if transaction_only:
            res["qps_starts"].append(e2e_start)
        sid, token = create(template, allocatable_memory=allocatable_memory)
        create_return = time.perf_counter()         # 1.0 create interface return
        create_api_ms = (create_return - e2e_start) * 1000
        if mode == "command":
            sandbox = Sandbox.connect(sid, timeout=120)
        elif mode == "code":
            sandbox = connect_code_sandbox(sid)
        cold_command = code_with_index(
            command, f"{sample_index}-cold-{sid[-8:]}"
        ) if mode == "code" else command
        ok1, _probe1_ms, t1 = probe(sid, token, mode, sandbox, cold_command, run_user, run_cwd, run_timeout)  # cold
        probe_return = time.perf_counter()           # 2.0 probe return
        e2e1 = (probe_return - e2e_start) * 1000     # 0.0 -> 2.0
        if transaction_only:
            if not ok1:
                error = f"sid={sid} error={t1 or 'transaction probe failed'}"
            else:
                # 业务事务的完成边界就是首次 probe 成功；后续清理失败
                # 不能抹掉已经完成的 create+probe 事务，但会停止该 worker，
                # 防止清理失败时继续制造无法回收的 sandbox。
                res["qps_completions"].append(probe_return)
                res["create_api"].append(create_api_ms)
                res["e2e"].append(e2e1)
                res["samples"].append({
                    "sid": sid,
                    "create_api": create_api_ms,
                    "e2e": e2e1,
                    "probe_ms": _probe1_ms,
                    "output": (t1 or "")[:60],
                })
            # 吞吐模式不等待 launch timing，也不执行第二次 warm probe：
            # 首次成功 probe 已经是用户定义的事务完成点。
            tc = None
        else:
            # 首次 probe 的 e2e 计时已经结束；这里才轮询 launch 状态，避免轮询
            # wait_launch_ms 污染端到端耗时。未打 timing patch 时 tc/timing 均为 None，
            # 仍继续 warm probe，但不伪造服务端 create/cold/phase 指标。
            tc, timing = wait_launch_ms(sid)
            run1 = e2e1 - tc if ok1 and tc is not None else None
            if ok1:
                warm_command = code_with_index(
                    command, f"{sample_index}-warm-{sid[-8:]}"
                ) if mode == "code" else command
                ok2, c2, t2 = probe(sid, token, mode, sandbox, warm_command, run_user, run_cwd, run_timeout)  # warm
            else:
                ok2, c2, t2 = False, None, "probe1 failed"
            if not ok1:
                error = f"sid={sid} error={t1}"
            elif not ok2:
                error = f"sid={sid} error={t2 or 'warm probe failed'}"
            else:
                # A sample is atomic: only publish it after create, cold probe and
                # warm probe have succeeded. Server launch metrics are optional
                # because an unpatched control plane does not expose them.
                phase_values = dict(launch_timing_rows(timing, minimum_ms=0.0)) if timing else {}
                res['create_api'].append(create_api_ms)
                res['e2e'].append(e2e1)
                res['warm'].append(c2)
                if tc is not None:
                    res['create'].append(tc)
                if run1 is not None:
                    res['cold'].append(run1)
                for name, value in phase_values.items():
                    res["phases"].setdefault(name, []).append(value)
                if timing and timing.get("accountedMs") is not None:
                    res["accounted"].append(float(timing["accountedMs"]))
                if timing and timing.get("unaccountedMs") is not None:
                    res["unaccounted"].append(float(timing["unaccountedMs"]))
                res["samples"].append({
                    "sid": sid,
                    "create_api": create_api_ms,
                    "create": tc,
                    "e2e": e2e1,
                    "cold": run1,
                    "warm": c2,
                    "phases": phase_values,
                    "output": (t1 or "")[:60],
                })
    except Exception as e:
        error = f"sid={sid or '<none>'} error={type(e).__name__}: {e}"
    finally:
        if transaction_only:
            # This is before DELETE: successful transactions end at first
            # probe, while failed in-flight attempts still contribute their
            # real tail to measured_s without counting as accepted.
            res["qps_ends"].append(time.perf_counter())
        # SDK 没有公开 close()；kill() 是其 Sandbox 生命周期清理接口。
        # 只有控制面确认删除完成（或已是 404）后才删本地目录，避免仍在运行的
        # sandbox 继续使用 base_root/<sid>。目录清理失败必须进入 failure，不能静默。
        deleted, delete_errors = _delete_sandbox(sid, sandbox)
        cleanup_errors.extend(delete_errors)
        if sid and deleted:
            cleanup_errors.extend(
                cleanup_sandbox_directories(sid, namespace=RUNTIME_NAMESPACE)
            )
        elif sid:
            cleanup_errors.append(
                f"sid={sid}: local sandbox directories retained because delete failed"
            )
        if cleanup_errors:
            if transaction_only:
                # Keep qps_starts even when the probe transaction failed, so
                # attempted/accepted/discarded accounting remains truthful.
                # A cleanup failure always stops this worker to avoid creating
                # more sandboxes while a previous SID may still be live.
                res["stop_worker"] = True
                if res["qps_completions"]:
                    res["cleanup_failed"] += 1
            else:
                _discard_result(res)
            cleanup_error = "cleanup failed: " + "; ".join(cleanup_errors)
            error = f"{error}; {cleanup_error}" if error else (
                f"sid={sid} error={cleanup_error}"
            )
    failure = None
    if error:
        res["discarded"] = 1
        failure = {
            "sid": sid,
            "namespace": RUNTIME_NAMESPACE,
            "error": error,
            "create_api_ms": create_api_ms,
            "e2e_ms": e2e1,
            "launch_ms": tc,
        }
    return res, failure


def _run_throughput_worker(template, deadline, label, mode, command,
                           run_user, run_cwd, run_timeout, concurrency,
                           allocatable_memory, worker_index):
    """Run create+probe transactions until ``deadline``.

    A worker only starts a new transaction while inside the requested load
    window. An in-flight transaction may finish after the deadline and is
    counted using its actual first-probe completion timestamp.
    """
    result = _new_results()
    failures = []
    sample_index = worker_index
    while time.perf_counter() < deadline:
        sample_index += 1
        batch, failure = run_round(
            template, mode, command, run_user, run_cwd, run_timeout,
            allocatable_memory, sample_index, transaction_only=True)
        _merge_results(result, batch)
        if failure:
            failures.append(failure)
        if batch["stop_worker"]:
            break
    return result, failures


def run_throughput(template, duration, label, mode,
                   command=DEFAULT_RUN_COMMAND, run_user="root", run_cwd="/",
                   run_timeout=30, concurrency=1, failure_log=None,
                   allocatable_memory=None):
    """Measure completed create + first successful probe business transactions.

    ``duration`` controls the open window for starting new transactions;
    ``concurrency`` is the maximum number of lifecycle workers. The returned
    result contains monotonic start/completion timestamps so the caller can
    report business QPS without counting the optional warm probe.
    """
    if mode not in ("health", "command"):
        raise ValueError("throughput mode requires --probe health or command")
    if duration <= 0:
        raise ValueError("duration must be greater than zero")
    if concurrency < 1:
        raise ValueError("concurrency must be greater than zero")

    started_at = time.perf_counter()
    deadline = started_at + duration
    result = _new_results()
    failures = []
    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        futures = [pool.submit(
            _run_throughput_worker, template, deadline, label, mode, command,
            run_user, run_cwd, run_timeout, concurrency, allocatable_memory,
            worker_index)
            for worker_index in range(concurrency)]
        # 与普通模式不同，样本总数事先未知，进度条走时间维度：elapsed/duration。
        # 计数回调读 result 的实时长度；在途事务尚未合并，acc 会滞后于真实
        # 完成数，worker 收尾后由 stop() 渲染最终值。
        progress_stop = _show_time_progress(
            label, deadline,
            accepted=lambda: len(result["qps_completions"]),
            attempted=lambda: len(result["qps_starts"]),
            started=started_at)
        try:
            for future in as_completed(futures):
                worker_result, worker_failures = future.result()
                _merge_results(result, worker_result)
                failures.extend(worker_failures)
        finally:
            progress_stop()

    result["qps_started_at"] = started_at
    result["qps_deadline"] = deadline
    result["qps_finished_at"] = max(
        result["qps_ends"] or [deadline])
    for failure in failures:
        _clear_progress()
        print(f"  {failure['error']}")
        if failure_log:
            try:
                append_failure_log(
                    failure_log, label, concurrency, mode, template, failure)
            except OSError as e:
                print(f"  failure log 写入失败: {e}")
    return result


def throughput_metrics(result):
    """Return business-QPS counters for a duration-mode result.

    The requested duration is the offered-load window. The measurement window
    also includes an in-flight transaction that finishes after the deadline,
    so a short run cannot report an artificially high QPS from its tail.
    """
    starts = result.get("qps_starts", [])
    completions = result.get("qps_completions", [])
    attempted = len(starts)
    accepted = len(completions)
    discarded = max(0, attempted - accepted)
    started_at = result.get("qps_started_at")
    deadline = result.get("qps_deadline")
    finished_at = result.get("qps_finished_at")
    if started_at is None or deadline is None:
        return {
            "attempted": attempted,
            "accepted": accepted,
            "discarded": discarded,
            "measured_s": 0.0,
            "business_qps": 0.0,
            "success_rate": (accepted / attempted if attempted else 0.0),
            "cleanup_failed": result.get("cleanup_failed", 0),
        }
    measurement_end = max(deadline, finished_at or deadline)
    measured_s = max(measurement_end - started_at, 1e-9)
    return {
        "attempted": attempted,
        "accepted": accepted,
        "discarded": discarded,
        "measured_s": measured_s,
        "business_qps": accepted / measured_s,
        "success_rate": (accepted / attempted if attempted else 0.0),
        "cleanup_failed": result.get("cleanup_failed", 0),
    }


def _throughput_probe_label(mode):
    labels = {
        "health": "/health",
        "command": "commands.run",
    }
    try:
        return labels[mode]
    except KeyError as exc:
        raise ValueError(f"unsupported throughput probe: {mode}") from exc


def print_throughput_summary(concurrency, B, W, mode="health"):
    """Print counters and completed create+probe QPS for one concurrency."""
    b = throughput_metrics(B) if B else None
    w = throughput_metrics(W) if W else None
    print(f"\n=== create + {_throughput_probe_label(mode)} 业务吞吐（concurrency={concurrency}）===")
    print(f"  {'指标':<20} {'B':>12} | {'W':>12}")
    print(f"  {'-' * 20} {'-' * 12} | {'-' * 12}")
    for name, key, formatter in (
        ("attempted", "attempted", lambda value: str(value)),
        ("accepted", "accepted", lambda value: str(value)),
        ("discarded", "discarded", lambda value: str(value)),
        ("cleanup_failed", "cleanup_failed", lambda value: str(value)),
        ("measured_s", "measured_s", lambda value: f"{value:.2f}"),
        ("business_qps", "business_qps", lambda value: f"{value:.3f}"),
        ("success_rate", "success_rate", lambda value: f"{value * 100:.1f}%"),
    ):
        b_cell = formatter(b[key]) if b else "-"
        w_cell = formatter(w[key]) if w else "-"
        print(f"  {name:<20} {b_cell:>12} | {w_cell:>12}")


def print_throughput_details(label, result, mode="health"):
    """Print per-transaction rows for duration mode when --verbose is set."""
    probe_name = "health" if mode == "health" else "command"
    print(f"\n=== {label} create + {probe_name} 成功事务 ===")
    columns = (
        ("sandboxID", 36, "<"),
        ("e2e(ms)", 8, "<"),
        ("create-api", 10, "<"),
        (probe_name, 7, "<"),
        ("output", None, "<"),
    )
    _print_fixed_width_table(columns, (
        (
            sample["sid"],
            f"{sample['e2e']:.1f}",
            f"{sample['create_api']:.1f}",
            f"{sample['probe_ms']:.1f}",
            _single_line_output(sample.get("output", "")),
        )
        for sample in result.get("samples", [])
    ))


def _single_line_output(value):
    """Keep command/code output from breaking the per-sample table."""
    return str(value).replace("\r", " ").replace("\n", " ")


def _format_fixed_width_row(columns, values):
    cells = []
    for (_header, width, alignment), value in zip(columns, values):
        text = str(value)
        if width is None:
            cells.append(text)
        else:
            cells.append(f"{text:{alignment}{width}}")
    return "  " + " ".join(cells)


def _print_fixed_width_table(columns, rows):
    """Print a table whose header and data use identical column boundaries."""
    print(_format_fixed_width_row(columns, (header for header, _width, _alignment in columns)))
    print(_format_fixed_width_row(
        columns,
        ("-" * width if width is not None else "------"
         for _header, width, _alignment in columns),
    ))
    for values in rows:
        print(_format_fixed_width_row(columns, values))


def print_throughput_trend(label, runs):
    """Compare business QPS and latency across duration-mode concurrency."""
    available = [(concurrency, result) for concurrency, result in runs if result]
    if len(available) < 2:
        return
    print(f"\n=== {label} 业务 QPS 并发趋势 ===")
    print("  concurrency attempted accepted discarded business-qps e2e p50")
    print("  ----------- --------- -------- --------- ------------ -------")
    for concurrency, result in available:
        metrics = throughput_metrics(result)
        p50 = _p50(result.get("e2e", []))
        p50_cell = f"{p50:.1f}" if p50 is not None else "-"
        print(f"  {concurrency:>11} {metrics['attempted']:>9} "
              f"{metrics['accepted']:>8} {metrics['discarded']:>9} "
              f"{metrics['business_qps']:>12.3f} {p50_cell:>7}")


def append_failure_log(path, label, concurrency, mode, template, failure,
                       round_number=None):
    record = {
        "timestamp": datetime.now().astimezone().isoformat(),
        "variant": label,
        "concurrency": concurrency,
        "probe": mode,
        "template": template,
        **failure,
    }
    if round_number is not None:
        record["round"] = round_number
    directory = os.path.dirname(os.path.abspath(path))
    os.makedirs(directory, exist_ok=True)
    with open(path, "a", encoding="utf-8") as log:
        log.write(json.dumps(record, ensure_ascii=False) + "\n")


def _run_sample_batch(template, sample_count, label, mode, command,
                      run_user, run_cwd, run_timeout, concurrency,
                      failure_log, progress_offset=0, progress_total=None,
                      round_number=None, progress_label=None,
                      allocatable_memory=None):
    """Run one batch and return only after every sample in that batch ends."""
    if sample_count < 1:
        raise ValueError("sample_count must be greater than zero")
    progress_total = progress_total or sample_count
    progress_label = progress_label or label
    result = _new_results()
    _show_progress(progress_label, progress_offset, progress_total)
    with ThreadPoolExecutor(max_workers=min(concurrency, sample_count)) as pool:
        futures = [pool.submit(run_round, template, mode, command,
                               run_user, run_cwd, run_timeout,
                               allocatable_memory, progress_offset + index)
                   for index in range(1, sample_count + 1)]
        for completed, future in enumerate(as_completed(futures), 1):
            batch_result, failure = future.result()
            _merge_results(result, batch_result)
            if failure:
                _clear_progress()
                print(f"  {failure['error']}")
                if failure_log:
                    try:
                        append_failure_log(
                            failure_log, label, concurrency, mode, template,
                            failure, round_number=round_number)
                    except OSError as e:
                        print(f"  failure log 写入失败: {e}")
            _show_progress(progress_label, progress_offset + completed,
                           progress_total)
    return result


def run_rounds(template, n, label, mode, command=DEFAULT_RUN_COMMAND,
               run_user="root", run_cwd="/", run_timeout=30, concurrency=1,
               failure_log=None, rounds=None, allocatable_memory=None,
               round_interval=None):
    """Run samples using either legacy total-count or wave-based semantics.

    With ``rounds`` unset, ``n`` remains the legacy total number of samples and
    the worker pool continuously fills up to ``concurrency``.  With ``rounds``
    set, each round starts exactly ``concurrency`` samples and the next round
    waits until the complete batch has finished.  Thus ``rounds=5`` and
    ``concurrency=10`` attempts 50 samples in five waves of ten.
    """
    if concurrency < 1:
        raise ValueError("concurrency must be greater than zero")
    if rounds is None:
        if n is None or n < 1:
            raise ValueError("n must be greater than zero")
        return _run_sample_batch(
            template, n, label, mode, command, run_user, run_cwd,
            run_timeout, concurrency, failure_log,
            allocatable_memory=allocatable_memory)
    if rounds < 1:
        raise ValueError("rounds must be greater than zero")

    result = _new_results()
    total = rounds * concurrency
    completed = 0
    for round_number in range(1, rounds + 1):
        if round_number > 1:
            _clear_progress()
        print(f"  {label} round {round_number}/{rounds} "
              f"(concurrency={concurrency})")
        batch = _run_sample_batch(
            template, concurrency, label, mode, command, run_user, run_cwd,
            run_timeout, concurrency, failure_log,
            progress_offset=completed, progress_total=total,
            round_number=round_number,
            progress_label=f"{label} r{round_number}/{rounds}",
            allocatable_memory=allocatable_memory)
        for sample in batch["samples"]:
            sample["round"] = round_number
        _merge_results(result, batch)
        completed += concurrency
        if round_number < rounds:
            # 轮间隔随并发度 1-5s：让上一波的 teardown（停 unit、快照物化回写、
            # vsock 释放）落地，避免污染下一波样本的 launch/health 计时。
            interval = round_interval if round_interval is not None else min(5.0, max(1.0, concurrency / 10.0))
            if interval > 0:
                time.sleep(interval)
    return result


def print_sample_details(label, concurrency, result, verbose=False):
    """Print success counts, and optionally one row per successful sandbox."""
    accepted = len(result["samples"])
    if not verbose:
        print(f"  {label} attempted={accepted + result['discarded']} "
              f"accepted={accepted} discarded={result['discarded']}")
        return

    print(f"\n=== {label} 每个成功沙箱（concurrency={concurrency}）===")
    has_round = any("round" in sample for sample in result["samples"])
    columns = [("sandboxID", 36, "<")]
    if has_round:
        columns.append(("round", 5, "<"))
    columns.extend((
        ("e2e(ms)", 8, "<"),
        ("create-api", 10, "<"),
        ("launch", 6, "<"),
        ("cold", 4, "<"),
        ("warm", 4, "<"),
        ("output", None, "<"),
    ))

    def rows():
        for sample in result["samples"]:
            values = [sample["sid"]]
            if has_round:
                values.append(sample["round"])
            values.extend((
                f"{sample['e2e']:.1f}",
                f"{sample['create_api']:.1f}",
                "-" if sample.get("create") is None else f"{sample['create']:.1f}",
                "-" if sample.get("cold") is None else f"{sample['cold']:.1f}",
                "-" if sample.get("warm") is None else f"{sample['warm']:.1f}",
                _single_line_output(sample.get("output", "")),
            ))
            yield values

    _print_fixed_width_table(columns, rows())
    print(f"  attempted={accepted + result['discarded']}  accepted={accepted}  "
          f"discarded={result['discarded']}")


def _mean(values):
    return sum(values) / len(values) if values else None


def _percentile(values, percentile):
    if not values:
        return None
    if not 0 <= percentile <= 100:
        raise ValueError("percentile must be between 0 and 100")
    values = sorted(values)
    rank = (len(values) - 1) * percentile / 100
    lower = int(rank)
    upper = min(lower + 1, len(values) - 1)
    fraction = rank - lower
    return values[lower] + (values[upper] - values[lower]) * fraction


def _p50(values):
    if not values:
        return None
    values = sorted(values)
    middle = len(values) // 2
    return values[middle] if len(values) % 2 else (values[middle - 1] + values[middle]) / 2


def _p90(values):
    return _percentile(values, 90)


def _p95(values):
    return _percentile(values, 95)


def _p99(values):
    return _percentile(values, 99)


def _delta_cell(value, baseline):
    if value is None or baseline in (None, 0):
        return "-"
    return f"{(value / baseline - 1) * 100:+.1f}%"


def print_concurrency_trend(label, runs):
    """Compare aggregate and launch-stage latency across concurrency levels."""
    available = [(concurrency, result) for concurrency, result in runs if result]
    # A single concurrency value has no trend or baseline comparison to show;
    # the per-group summary above already contains the useful measurements.
    if len(available) < 2:
        return
    baseline = available[0][1]
    baseline_e2e = _mean(baseline["e2e"])
    print(f"\n=== {label} 并发趋势（相对 c={available[0][0]}）===")
    print("  concurrency attempted accepted discarded  e2e avg  e2e p50   delta  create-api  launch   cold   warm")
    print("  ----------- --------- -------- --------- -------- -------- ------- ----------- ------- ------ ------")
    for concurrency, result in available:
        e2e_avg = _mean(result["e2e"])
        values = [_mean(result[key]) for key in ("create_api", "create", "cold", "warm")]
        cells = [f"{value:.1f}" if value is not None else "-" for value in values]
        p50 = _p50(result["e2e"])
        e2e_cell = f"{e2e_avg:.1f}" if e2e_avg is not None else "-"
        p50_cell = f"{p50:.1f}" if p50 is not None else "-"
        accepted = len(result["samples"])
        attempted = accepted + result["discarded"]
        print(f"  {concurrency:>11} {attempted:>9} {accepted:>8} {result['discarded']:>9} "
              f"{e2e_cell:>8} {p50_cell:>8} {_delta_cell(e2e_avg, baseline_e2e):>7} "
              f"{cells[0]:>11} {cells[1]:>7} {cells[2]:>6} {cells[3]:>6}")

    phase_names = []
    for name, _start, _end in TIMING_PHASES:
        if any((_mean(result["phases"].get(name, [])) or 0) >= TIMING_DISPLAY_MIN_MS
               for _concurrency, result in available):
            phase_names.append(name)
    if not phase_names:
        return
    print(f"\n  {label} 启动节点趋势（单元格为 avg ms / 相对基线变化）")
    cell_width = 20
    print(f"  {'阶段':<{SUMMARY_LABEL_WIDTH}}" + "".join(
        f" {'c=' + str(concurrency):>{cell_width}}" for concurrency, _result in available))
    print(f"  {'-' * SUMMARY_LABEL_WIDTH}" + "".join(
        f" {'-' * cell_width}" for _concurrency, _result in available))
    for name in phase_names:
        base_value = _mean(baseline["phases"].get(name, []))
        cells = []
        for _concurrency, result in available:
            value = _mean(result["phases"].get(name, []))
            cell = "-" if value is None else f"{value:.1f} / {_delta_cell(value, base_value)}"
            cells.append(f" {cell:>{cell_width}}")
        print(f"  {name:<{SUMMARY_LABEL_WIDTH}}" + "".join(cells))


SUMMARY_LABEL_WIDTH = 38
SUMMARY_VALUE_WIDTH = 9
SUMMARY_N_WIDTH = 4


def _summary_label(value):
    """Pad a label to the terminal width used by the summary tables."""
    text = str(value)
    display_width = sum(
        2 if unicodedata.east_asian_width(char) in ("W", "F") else 1
        for char in text
    )
    return text + " " * max(0, SUMMARY_LABEL_WIDTH - display_width)


def stat_cells(xs):
    """Return min/avg/percentile/max/n cells without labels."""
    if not xs:
        return ("-", "-", "-", "-", "-", "-", "-", "-")
    xs = sorted(xs)
    n = len(xs)
    return (f"{min(xs):.1f}", f"{sum(xs) / n:.1f}", f"{_p50(xs):.1f}",
            f"{_p90(xs):.1f}", f"{_p95(xs):.1f}", f"{_p99(xs):.1f}",
            f"{max(xs):.1f}", str(n))


def _stat_group(cells):
    return " ".join(
        f"{cell:>{SUMMARY_N_WIDTH if index == len(cells) - 1 else SUMMARY_VALUE_WIDTH}}"
        for index, cell in enumerate(cells)
    )


def print_stat_table(title, rows):
    """Print a fixed-width B/W table with statistics in the column headers."""
    b_header = _stat_group((
        "min(ms)", "avg(ms)", "p50(ms)", "p90(ms)", "p95(ms)",
        "p99(ms)", "max(ms)", "n",
    ))
    w_header = b_header
    separator = f"  {'-' * SUMMARY_LABEL_WIDTH} {'-' * len(b_header)} | {'-' * len(w_header)}"
    print(f"\n=== {title} ===")
    print(f"  {_summary_label('指标')} {'B':^{len(b_header)}} | {'W':^{len(w_header)}}")
    print(f"  {_summary_label('')} {b_header} | {w_header}")
    print(separator)
    for label, b_values, w_values in rows:
        b = _stat_group(stat_cells(b_values))
        w = _stat_group(stat_cells(w_values))
        print(f"  {_summary_label(label)} {b} | {w}")


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
    print("  阶段说明（单位：ms；列为 min / avg / p50 / p90 / p95 / p99 / max / n）：")
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
    ap.add_argument("-n", "--iterations", type=int, default=None,
                    help="旧模式：每个并发档位的总样本数（与 --rounds 互斥，默认 10）")
    ap.add_argument("-r", "--rounds", type=int, default=None,
                    help="分轮模式：每个并发档位的轮数；每轮启动 concurrency 个样本，轮间等待全部结束")
    ap.add_argument("-d", "--duration", type=float, default=None,
                    help="吞吐模式：每个并发档位持续该秒数（如 -d 20）；支持 --probe health/command，"
                         "统计 create+首次成功 probe 的业务 QPS")
    ap.add_argument("-c", "--concurrency", type=int, nargs="+", default=[1],
                    metavar="N", help="并发档位，可传多个值，例如 -c 1 10 30 50（默认 1）")
    ap.add_argument("-v", "--verbose", action="store_true",
                    help="显示每个成功沙箱的逐样本耗时明细（失败始终显示）")
    ap.add_argument("--b", default=None,
                    help="B template（传入才跑 B；默认不跑）")
    ap.add_argument("--w", default=None,
                    help="W template（传入才跑 W；默认不跑）")
    ap.add_argument("--probe", choices=("exec", "health", "command", "code"), default="exec",
                    help="探针：exec/health 走 node proxy；command 走 SDK commands.run/envd；"
                         "code 走 CI SDK run_code（e2b 镜像 FastAPI:49999→Jupyter）")
    probe_args = ap.add_mutually_exclusive_group()
    probe_args.add_argument("--run-command", default=None,
                            help="--probe command 的 guest 命令（默认 date）")
    probe_args.add_argument("--run-code", default=None,
                            help="--probe code 的 Python 源码（默认 print('hello kuasar-sandbox {index}')，"
                                 "每次执行替换不同序号）")
    ap.add_argument("--run-user", default="root",
                    help="--probe command 的 guest 用户（code-interpreter manifest 默认 root）")
    ap.add_argument("--run-cwd", default="/",
                    help="--probe command 的 guest 工作目录（code-interpreter manifest 默认 /）")
    ap.add_argument("--run-timeout", type=int, default=30,
                    help="--probe command/code 的单次执行超时秒数")
    ap.add_argument("--allocatable-memory", default=None,
                    help="覆盖本次 sandbox 的 allocatable.memory，例如 256MiB；默认不覆盖")
    ap.add_argument("--failure-log", default=os.path.join(PARENT, "logs", "bench-failures.jsonl"),
                    help="失败样本 JSONL 日志（默认 logs/bench-failures.jsonl）")
    ap.add_argument("--round-interval", type=float, default=None,
                    help="轮间隔秒数；默认按并发度自动 1-5s（c=10→1s, c=25→2.5s, c=50→5s），0=不间隔")
    ap.add_argument("--docker", action="store_true",
                    help="额外重启 ws-docker-debug 并执行 Docker cold/warm 对照")
    args = ap.parse_args()
    if args.duration is not None:
        if args.iterations is not None or args.rounds is not None:
            ap.error("--duration 与 -n/--iterations、-r/--rounds 不能同时使用")
        if args.duration <= 0:
            ap.error("--duration 必须大于 0")
        if args.probe not in ("health", "command"):
            ap.error("--duration 吞吐模式仅支持 --probe health 或 command")
        if args.docker:
            ap.error("--duration 不能与 --docker 同时使用")
        if args.round_interval is not None:
            ap.error("--duration 不能与 --round-interval 同时使用")
    else:
        if args.iterations is not None and args.rounds is not None:
            ap.error("-n/--iterations 与 -r/--rounds 不能同时使用")
        if args.iterations is None and args.rounds is None:
            args.iterations = 10
        if args.iterations is not None and args.iterations < 1:
            ap.error("-n/--iterations 必须大于 0")
        if args.rounds is not None and args.rounds < 1:
            ap.error("-r/--rounds 必须大于 0")
    if any(value < 1 for value in args.concurrency):
        ap.error("-c/--concurrency 的值必须大于 0")
    if not args.b and not args.w:
        sys.exit("✗ 至少传一个 --b 或 --w")
    if args.docker and args.probe in ("command", "code"):
        sys.exit("✗ --docker 仅支持 exec/health 探针")
    if args.probe == "command":
        if args.run_code is not None:
            ap.error("--run-code 仅适用于 --probe code")
        probe_command = args.run_command or DEFAULT_RUN_COMMAND
    elif args.probe == "code":
        if args.run_command is not None:
            ap.error("--run-command 仅适用于 --probe command")
        probe_command = args.run_code or DEFAULT_RUN_CODE
    else:
        if args.run_command is not None or args.run_code is not None:
            ap.error("--run-command/--run-code 必须配合对应的 --probe command/code")
        probe_command = DEFAULT_RUN_COMMAND

    if args.duration is not None:
        throughput_probe = _throughput_probe_label(args.probe)
        count_header = (f"duration={args.duration:g}s/档（持续发起 create+{throughput_probe}，"
                        "到时不再启动新事务）")
    elif args.rounds is not None:
        count_header = (f"rounds={args.rounds}/档（每轮 concurrency 个样本，"
                        "轮间等待完成）")
    else:
        count_header = f"n={args.iterations}/档（总样本数）"
    if args.probe == "command":
        hdr = [f"# working-set 对比（e2b envd，probe=commands.run）",
               f"{count_header}  concurrency={','.join(map(str, args.concurrency))}  "
               f"command={probe_command}  user={args.run_user} cwd={args.run_cwd}"]
    elif args.probe == "code":
        hdr = [f"# working-set 对比（e2b code-interpreter，probe=run_code /execute）",
               f"{count_header}  concurrency={','.join(map(str, args.concurrency))}  "
               f"code={probe_command!r}  timeout={args.run_timeout}s（严格首次执行，无重试）"]
    elif args.probe == "health":
        hdr = ["# working-set 对比（openclaw + e2b/envd，node-proxy curl /health）",
               f"{count_header}  concurrency={','.join(map(str, args.concurrency))}  "
               f"proxy={openclaw_net.proxy_url()}"]
    else:
        hdr = [f"# working-set 对比（原生 openclaw，probe={args.probe}，经 node proxy 按 sid 转发）",
               f"{count_header}  concurrency={','.join(map(str, args.concurrency))}  "
               f"proxy={openclaw_net.proxy_url()}"]
    hdr.append(f"namespace={RUNTIME_NAMESPACE or '<unresolved>'}")
    if args.allocatable_memory:
        hdr.append(f"resource: allocatable.memory={args.allocatable_memory}")
    if args.b:
        hdr.append(f"B: {args.b[:48]}…")
    if args.w:
        hdr.append(f"W: {args.w[:48]}…")
    print("\n".join(hdr))

    runs = []
    for concurrency in args.concurrency:
        if args.duration is not None:
            run_header = f"duration={args.duration:g}s，create+{throughput_probe} 吞吐"
        elif args.rounds is not None:
            run_header = f"rounds={args.rounds}，attempted={args.rounds * concurrency}"
        else:
            run_header = f"n={args.iterations}"
        print(f"\n######## concurrency={concurrency}，{run_header} ########")
        B = W = None
        if args.b:
            print("\n=== B（无工作集）===")
            if args.duration is not None:
                B = run_throughput(
                    args.b, args.duration, "B", args.probe, probe_command,
                    args.run_user, args.run_cwd, args.run_timeout, concurrency,
                    args.failure_log, args.allocatable_memory)
            else:
                B = run_rounds(args.b, args.iterations, "B", args.probe, probe_command,
                               args.run_user, args.run_cwd, args.run_timeout, concurrency,
                               args.failure_log, rounds=args.rounds,
                               allocatable_memory=args.allocatable_memory,
                               round_interval=args.round_interval)
        if args.w:
            print("\n=== W（工作集）===")
            if args.duration is not None:
                W = run_throughput(
                    args.w, args.duration, "W", args.probe, probe_command,
                    args.run_user, args.run_cwd, args.run_timeout, concurrency,
                    args.failure_log, args.allocatable_memory)
            else:
                W = run_rounds(args.w, args.iterations, "W", args.probe, probe_command,
                               args.run_user, args.run_cwd, args.run_timeout, concurrency,
                               args.failure_log, rounds=args.rounds,
                               allocatable_memory=args.allocatable_memory,
                               round_interval=args.round_interval)

        runs.append((concurrency, B, W))

        if args.duration is not None:
            print_throughput_summary(concurrency, B, W, args.probe)
            print_stat_table(f"create + {throughput_probe} 延迟（concurrency={concurrency}）", [
                ("transaction e2e", B.get("e2e", []) if B else [],
                 W.get("e2e", []) if W else []),
                ("create-api", B.get("create_api", []) if B else [],
                 W.get("create_api", []) if W else []),
            ])
            if args.verbose:
                if B:
                    print_throughput_details("B", B, args.probe)
                if W:
                    print_throughput_details("W", W, args.probe)
        else:
            print_stat_table(f"汇总（concurrency={concurrency}）", [
                (f"{args.probe}1 e2e", B.get("e2e", []) if B else [], W.get("e2e", []) if W else []),
                ("create-api", B.get("create_api", []) if B else [], W.get("create_api", []) if W else []),
                ("create(launchMs)", B.get("create", []) if B else [], W.get("create", []) if W else []),
                (f"{args.probe}1 (cold)" + (" ★" if (B and W) else ""),
                 B.get("cold", []) if B else [], W.get("cold", []) if W else []),
                (f"{args.probe}2 (warm)", B.get("warm", []) if B else [], W.get("warm", []) if W else []),
            ])
            print_phase_summary(B, W)
            if B:
                print_sample_details("B", concurrency, B, verbose=args.verbose)
            if W:
                print_sample_details("W", concurrency, W, verbose=args.verbose)

    if args.duration is not None:
        print_throughput_trend("B", [(concurrency, B) for concurrency, B, _W in runs])
        print_throughput_trend("W", [(concurrency, W) for concurrency, _B, W in runs])
    else:
        print_concurrency_trend("B", [(concurrency, B) for concurrency, B, _W in runs])
        print_concurrency_trend("W", [(concurrency, W) for concurrency, _B, W in runs])
    if args.docker:
        print("\n=== Docker 对照 ===")
        run_docker_probe(args.probe)


if __name__ == "__main__":
    main()
