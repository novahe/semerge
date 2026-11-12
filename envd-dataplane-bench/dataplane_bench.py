#!/usr/bin/env python3
"""envd 数据面全家桶 bench（修复验证 + 场景矩阵 A-G）.

场景:
  P0 构建(模板ID由 manifest 现场拼装) + 3 沙箱并发 + RSS 采样器启动
  PA 单箱 6 路并发 200MiB 下载(h2/h1.1 各半) + 重载 exec ping 延迟(无卡顿)
  PB 1GiB 大文件 ×4 轮下载 sha 校验
  PC pause → 自动 resume → proxy 恢复探测 → 200MiB 传输验证(恢复后数据面)
  PD MUX 对照: commands.run 流式拉 200MiB stdout, sha 校验
  PE 1KiB 小文件 ×200 高频上传+下载 sha 校验
  PH gzip 压缩传输: 文本/随机 Accept-Encoding 下载 + Content-Encoding 上传, sha 校验
  PG RSS 全程采样(host sandbox-ctl/CH + guest envd 边界采样)

用法: source <节点>/tenant.env && (export BENCH_TPL=… 或同目录 bench.env) && python3 dataplane_bench.py
"""
import base64, gzip, hashlib, os, subprocess, sys, threading, time
from concurrent.futures import ThreadPoolExecutor
from urllib.parse import urlencode, urlsplit

from e2b import Sandbox

# ---- 可移植配置: 环境变量 > 脚本同目录 bench.env(gitignored, 本节点覆盖) ----
# 三种跑法: ① source 目标节点 tenant.env(SDK 鉴权+E2B_DOMAIN+E2B_SANDBOX_URL+
# CURL_CA_BUNDLE 全在里面)后直跑 ② export BENCH_* 覆盖 ③ 同目录 bench.env 写节点相关值.
# 不内置任何节点信息; 缺关键配置直接报错退出并列出缺什么.
_HERE = os.path.dirname(os.path.abspath(__file__))
for _kv in (l.strip() for l in open(f"{_HERE}/bench.env", errors="ignore")
            if l.strip() and not l.lstrip().startswith("#")):
    if "=" in _kv:
        _k, _v = _kv.split("=", 1)
        os.environ.setdefault(_k.strip(), _v.strip())  # 已有环境变量优先


def _base(u):
    return urlsplit(u)._replace(query="", fragment="").geturl()


E2B_DOMAIN = os.environ.get("E2B_DOMAIN", "")
SANDBOX_URL = os.environ.get("E2B_SANDBOX_URL", "")
PROXY = (os.environ.get("BENCH_PROXY")
         or (_base(SANDBOX_URL) if SANDBOX_URL else "")
         or (f"https://{E2B_DOMAIN}:{os.environ.get('BENCH_PROXY_PORT', '32591')}" if E2B_DOMAIN else ""))
CACERT = (os.environ.get("BENCH_CACERT") or os.environ.get("CURL_CA_BUNDLE")
          or os.environ.get("SSL_CERT_FILE") or "")
NS_TAG = (os.environ.get("BENCH_NS")
          or (E2B_DOMAIN.split(".")[0].removeprefix("sandboxes-") if E2B_DOMAIN else ""))


def _tpl_from_manifest(m):
    if not m.startswith(("manifest://", "registry://")):
        m = "manifest://" + m  # 裸 sha 也接受
    # conductor 的 ref 解码用 raw base64, 自带 padding(=) 的会报 illegal base64 → 500
    return "e2b-img-" + base64.b64encode(m.encode()).decode().rstrip("=")


TPL = (os.environ.get("BENCH_TPL")
       or (os.environ.get("BENCH_MANIFEST") and _tpl_from_manifest(os.environ["BENCH_MANIFEST"]))
       or "")
WORK = os.environ.get("BENCH_WORK") or _HERE + "/work"
USER = os.environ.get("BENCH_USER", "root")
NBOX = int(os.environ.get("BENCH_NBOX", "3"))
ENVD_PORT = os.environ.get("BENCH_ENVD_PORT", "49983")
TIMEOUT_S = int(os.environ.get("BENCH_TIMEOUT", "1800"))  # 防 300s 空闲自动 pause
_missing = [n for n, v in (("BENCH_PROXY 或 E2B_SANDBOX_URL/E2B_DOMAIN", PROXY),
                           ("BENCH_NS 或 E2B_DOMAIN", NS_TAG),
                           ("BENCH_TPL 或 BENCH_MANIFEST", TPL)) if not v]
if _missing:
    print("缺少配置: " + "; ".join(_missing)
          + "\n  跑法: source <节点>/tenant.env 后直跑, 或 export BENCH_*, "
            "或在本目录 bench.env(gitignored)写节点相关值; 详见 README 跨节点移植",
          file=sys.stderr)
    sys.exit(2)
PRINT = threading.Lock()
T0 = time.time()
FAILS = []          # (phase, box, detail)
EVENTS = []         # 关键事件时间线


def log(msg):
    with PRINT:
        print(f"[{time.time()-T0:7.1f}s] {msg}", flush=True)


def fail(phase, box, detail):
    FAILS.append((phase, box, detail))
    log(f"✗ [{phase}] box{box} {detail}")


def event(msg):
    EVENTS.append((time.time() - T0, msg))
    log(f"◆ {msg}")


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(len(xs) * p / 100))] if xs else float("nan")


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for c in iter(lambda: f.read(1 << 20), b""):
            h.update(c)
    return h.hexdigest()


def sgn(path, op, tok):
    return "v1_" + base64.b64encode(hashlib.sha256(
        f"{path}:{op}:{USER}:{tok}".encode()).digest()).decode().rstrip("=")


def wline(p):
    """解析 curl -w 输出: (code, n_bytes, t_sec); 解析失败返回 (None, 0, 0.0)."""
    parts = p.stdout.split()
    if len(parts) == 3:
        try:
            return parts[0], int(parts[1]), float(parts[2])
        except ValueError:
            pass
    return None, 0, 0.0


class Box:
    def __init__(self, bid):
        self.bid = bid
        self.p200, self.p1g = f"/tmp/bench200_{bid}.bin", "/tmp/bench1g.bin"
        self.src200 = f"{WORK}/src200_{bid}.bin"
        self.dl = f"{WORK}/dl_{bid}.bin"
        self.sbx = None

    def urls(self, path, tok):
        q = lambda op: urlencode({"path": path, "username": USER, "signature": sgn(path, op, tok)})
        return f"{PROXY}/files?{q('write')}", f"{PROXY}/files?{q('read')}"

    def curl(self, sid, url, out=None, infile=None, http1=False, maxtime=180, post_body=False,
             headers=(), hdr_out=None):
        cmd = ["curl", "-sS", *(["--cacert", CACERT] if CACERT else []),
               *(["--http1.1"] if http1 else []),
               "-H", f"E2b-Sandbox-Id: {sid}", "-H", f"E2b-Sandbox-Port: {ENVD_PORT}",
               "--max-time", str(maxtime), "-w", "%{http_code} %{size_download} %{time_total}"]
        for h in headers:
            cmd += ["-H", h]
        if hdr_out:  # 响应头落盘(PH 校验 Content-Encoding 用)
            cmd += ["-D", hdr_out]
        if post_body:
            # -T 流式上传: --data-binary @file 会整文件读进内存, 1GiB 直接 OOM(rc=2)
            cmd += ["-X", "POST", "-H", "Content-Type: application/octet-stream",
                    "-T", infile, "-o", "/dev/null"]
        elif infile:
            cmd += ["--data-binary", "@" + infile]
        else:
            cmd += ["-o", out]
        cmd.append(url)
        return subprocess.run(cmd, capture_output=True, text=True)


# ---------------- PG: RSS 采样 ----------------
class RssSampler(threading.Thread):
    def __init__(self):
        super().__init__(daemon=True)
        self.stop_flag = threading.Event()
        self.samples = {}   # pid -> [(t, rss_kb, name)]

    def _procs(self):
        out = {}
        for pid in os.listdir("/proc"):
            if not pid.isdigit():
                continue
            try:
                with open(f"/proc/{pid}/cmdline", "rb") as f:
                    cl = f.read().decode(errors="ignore").replace("\0", " ")
                if "sandbox-ctl run" not in cl and "cloud-hypervisor" not in cl:
                    continue
                if NS_TAG not in cl and "cloud-hypervisor" not in cl:
                    continue  # 只盯本 NS 的沙箱进程(跨节点可移植)
                with open(f"/proc/{pid}/status") as f:
                    rss = next(int(l.split()[1]) for l in f if l.startswith("VmRSS"))
                out[int(pid)] = (rss, ("CH" if "cloud-hypervisor" in cl else "sbxctl") + pid)
            except (OSError, StopIteration):
                continue
        return out

    def run(self):
        while not self.stop_flag.is_set():
            t = time.time() - T0
            for pid, (rss, name) in self._procs().items():
                self.samples.setdefault(pid, []).append((t, rss, name))
            self.stop_flag.wait(2)

    def report(self):
        lines = []
        for pid, xs in sorted(self.samples.items()):
            name, growth = xs[0][2], xs[-1][1] - xs[0][1]
            lines.append(f"    {name}: max={max(r for _, r, _ in xs)/1024:.0f}MiB "
                         f"增长={growth/1024:+.0f}MiB (样本{len(xs)})")
        return "\n".join(lines)


# ---------------- 各 phase ----------------
def _create_sandbox():
    """create 带重试: 瞬时 5xx(SDK 解析成 KeyError 等)不应带崩整场."""
    last = None
    for attempt in (1, 2, 3):
        try:
            sbx = Sandbox.create(TPL, timeout=TIMEOUT_S)
            sbx.set_timeout(TIMEOUT_S)
            return sbx
        except Exception as e:
            last = e
            log(f"  create 第{attempt}次失败 {type(e).__name__}: {str(e)[:80]}")
            time.sleep(3 * attempt)
    raise last


def setup(box, size_mib):
    t = time.time()
    box.sbx = _create_sandbox()
    box.sid = box.sbx.sandbox_id
    box.tok = box.sbx._envd_access_token
    subprocess.run(["dd", "if=/dev/urandom", f"of={box.src200}", "bs=4M",
                    f"count={size_mib//4}", "status=none"], check=True)
    box.sha200 = sha256_file(box.src200)
    up_url, _ = box.urls(box.p200, box.tok)
    p = box.curl(box.sid, up_url, infile=box.src200, post_body=True, maxtime=300)
    ok = wline(p)[0] == "200"
    if not ok:
        fail("P0", box.bid, f"初始上传失败 {p.stdout} {p.stderr[:60]}")
    event(f"box{box.bid} 就绪 sid={box.sid[:13]} 初始上传{'✓' if ok else '✗'} "
          f"create+upload {time.time()-t:.1f}s")
    return ok


def ping_loop(sbx, stop_flag, latencies):
    while not stop_flag.is_set():
        t = time.time()
        try:
            sbx.commands.run("true", cwd="/app", user=USER, timeout=60)
            latencies.append(time.time() - t)
        except Exception as e:
            latencies.append(float("nan"))
            with PRINT:
                print(f"    ping 异常: {type(e).__name__} {str(e)[:60]}", flush=True)
        stop_flag.wait(0.5)


def phase_a(box):
    """6 路并发下载(h2×3+h1.1×3) + 重载 ping。"""
    _, dl_url = box.urls(box.p200, box.tok)
    base = []
    for _ in range(15):
        t = time.time(); box.sbx.commands.run("true", cwd="/app", user=USER, timeout=60)
        base.append(time.time() - t)
    lat, stop = [], threading.Event()
    pinger = threading.Thread(target=ping_loop, args=(box.sbx, stop, lat), daemon=True)
    pinger.start()
    results = [None] * 6

    def dl(i):
        http1 = i % 2 == 1
        try:
            p = box.curl(box.sid, dl_url, out=box.dl, http1=http1, maxtime=180)
            code, nb, ts = wline(p)
            good = (p.returncode == 0 and code == "200" and nb == 200 * 2**20
                    and sha256_file(box.dl) == box.sha200)
            results[i] = (good, http1, ts, p.returncode,
                          (p.stderr or p.stdout).strip()[:40])
        except Exception as e:  # 下载线程不允许带崩整箱
            results[i] = (False, http1, -1, -1, f"{type(e).__name__}: {str(e)[:40]}")

    with ThreadPoolExecutor(6) as ex:
        list(ex.map(dl, range(6)))
    time.sleep(1)
    stop.set(); pinger.join(timeout=5)
    bad = [r for r in results if not r[0]]
    for r in bad:
        fail("PA", box.bid, f"并发下载失败 http1={r[1]} rc={r[3]} {r[4]}")
    lat_ok = [x for x in lat if x == x]
    nan_n = len(lat) - len(lat_ok)
    stat = (f"box{box.bid} PA: 下载 {'6/6✓' if not bad else f'{6-len(bad)}/6'} | "
            f"ping 重载 p50={pct(lat_ok,50)*1000:.0f}ms p99={pct(lat_ok,99)*1000:.0f}ms "
            f"max={max(lat_ok)*1000:.0f}ms (n={len(lat_ok)}"
            + (f", {nan_n}次异常" if nan_n else "") + ")")
    log(f"  {stat}")
    box.pa_ping = (pct(base, 50), pct(lat_ok, 50), pct(lat_ok, 99), max(lat_ok) if lat_ok else -1)
    return not bad and nan_n == 0


def phase_b(box):
    """1GiB ×4 轮。源文件由 main 预生成(三箱共享, 避免并发建/删竞态)。"""
    src1g = f"{WORK}/src1g.bin"
    sha1g = sha256_file(src1g)
    up_url, dl_url = box.urls(box.p1g, box.tok)
    p = box.curl(box.sid, up_url, infile=src1g, post_body=True, maxtime=300)
    if wline(p)[0] != "200":
        fail("PB", box.bid, f"1GiB 上传失败 rc={p.returncode} out={p.stdout[:50]!r} err={p.stderr.strip()[:80]!r}")
        return False
    ok = True
    times = []
    for i in range(4):
        p = box.curl(box.sid, dl_url, out=box.dl, http1=(i % 2 == 1), maxtime=300)
        code, nb, ts = wline(p)
        good = code == "200" and nb == 2**30 and sha256_file(box.dl) == sha1g
        times.append(ts if good else -1)
        if not good:
            fail("PB", box.bid, f"1GiB 第{i+1}轮失败 {p.stdout[:40]} {p.stderr[:40]}")
            ok = False
    log(f"  box{box.bid} PB: 1GiB×4 {'✓' if ok else '✗'} "
        f"下载耗时={['%.2f' % t for t in times]}")
    return ok


def phase_c(box):
    """pause → 自动 resume → proxy 恢复探测 → 传输验证。"""
    t = time.time()
    try:
        if not box.sbx.pause():
            fail("PC", box.bid, "pause() 返回 False")
            return False
    except Exception as e:
        fail("PC", box.bid, f"pause 异常 {type(e).__name__}: {str(e)[:60]}")
        return False
    t_pause = time.time() - t
    # 触发自动 resume: exec 到恢复或超时
    t2 = time.time()
    resumed = False
    try:
        box.sbx.commands.run("true", cwd="/app", user=USER, timeout=180)
        resumed = True
    except Exception as e:
        fail("PC", box.bid, f"resume exec 失败 {type(e).__name__}: {str(e)[:60]}")
    t_resume = time.time() - t2
    # proxy 数据面恢复探测: 每2s GET, 最多120s
    _, dl_url = box.urls(box.p200, box.tok)
    t3, recover_s, dl_ok = time.time(), None, False
    while time.time() - t3 < 120:
        p = box.curl(box.sid, dl_url, out=box.dl, maxtime=30)
        code, nb, _ = wline(p)
        if code == "200" and sha256_file(box.dl) == box.sha200:
            recover_s = time.time() - t3
            dl_ok = True
            break
        time.sleep(2)
    if not dl_ok:
        fail("PC", box.bid, "恢复后 proxy 数据面 120s 未恢复(持续502)")
        # 重建沙箱让后续 phase 可继续
        try:
            box.sbx.kill()
        except Exception:
            pass
        setup(box, 200)
    else:
        # 再来一发 200MiB 全量确认
        p = box.curl(box.sid, dl_url, out=box.dl, maxtime=180)
        code, nb, ts = wline(p)
        full_ok = code == "200" and nb == 200 * 2**20 and sha256_file(box.dl) == box.sha200
        if not full_ok:
            fail("PC", box.bid, f"恢复后 200MiB 全量失败 {p.stdout[:40]}")
        log(f"  box{box.bid} PC: pause={t_pause:.1f}s resume={t_resume:.1f}s "
            f"proxy恢复={recover_s:.1f}s 全量={'✓' if full_ok else '✗'}")
    return dl_ok


def phase_d(box):
    """MUX 大输出对照: 200MiB 经 commands.run stdout 流回(host 解码).
    随机二进制不能直接走 stdout(SDK 按 UTF-8 文本解码会失真), guest 侧 base64 装甲."""
    t = time.time()
    try:
        r = box.sbx.commands.run(f"base64 -w0 {box.p200}", cwd="/app", user=USER, timeout=300)
    except Exception as e:
        fail("PD", box.bid, f"exec 异常 {type(e).__name__}: {str(e)[:60]}")
        return False
    if r.exit_code != 0:
        fail("PD", box.bid, f"guest exit={r.exit_code} {str(r.stderr)[:60]}")
        return False
    try:
        got = hashlib.sha256(base64.b64decode(r.stdout)).hexdigest()
    except Exception as e:
        fail("PD", box.bid, f"base64 解码失败 {type(e).__name__} stdout_len={len(r.stdout)}")
        return False
    ok = got == box.sha200
    if not ok:
        fail("PD", box.bid, f"stdout sha 不匹配 stdout_len={len(r.stdout)}")
    log(f"  box{box.bid} PD: 200MiB(b64) stdout {'✓' if ok else '✗'} {time.time()-t:.1f}s")
    return ok


def phase_e(box):
    """1KiB ×200 高频上传+下载。"""
    up_url, dl_url = box.urls("/tmp/small.bin", box.tok)
    bad = 0
    t = time.time()
    for i in range(200):
        payload = f"bench-{box.bid}-{i:04d}-".encode() * 64  # 1088B ≈ 1KiB
        want = hashlib.sha256(payload).hexdigest()
        open(box.dl, "wb").write(payload)
        p = box.curl(box.sid, up_url, infile=box.dl, post_body=True, maxtime=30)
        if wline(p)[0] != "200":
            bad += 1; fail("PE", box.bid, f"#{i} 上传 {p.stdout[:30]}"); continue
        p = box.curl(box.sid, dl_url, out=box.dl, maxtime=30)
        if wline(p)[0] != "200" or sha256_file(box.dl) != want:
            bad += 1; fail("PE", box.bid, f"#{i} 下载 {p.stdout[:30]}")
    log(f"  box{box.bid} PE: 1KiB×200 {'✓' if bad == 0 else f'✗ {bad}失败'} "
        f"{(time.time()-t)/200*1000:.0f}ms/轮")
    return bad == 0


def _gunzip_sha(body_path, out_path):
    """解压 curl 落盘的 gzip 响应体并算 sha; 非法 gzip 流返回 None."""
    try:
        with gzip.open(body_path, "rb") as f, open(out_path, "wb") as o:
            h = hashlib.sha256()
            for c in iter(lambda: f.read(4 << 20), b""):
                o.write(c)
                h.update(c)
        return h.hexdigest()
    except OSError:
        return None


def phase_h(box):
    """gzip 压缩传输(envd 原生双向 gzip, proxy 透传两 header — probe_gzip.py 已证):
    文本下载看线上字节节省, 随机下载看最差情况(CPU 开销无压缩收益), 上传走 envd 服务端解压."""
    ok = True
    mib = 200
    text = f"{WORK}/gztext_{box.bid}.bin"
    body = f"{WORK}/gzbody_{box.bid}.bin"
    out = f"{WORK}/gzout_{box.bid}.bin"
    hdr = f"{WORK}/gzhdr_{box.bid}.txt"
    # 可压缩文本源(重复行, gzip ~1000:1); 保留复用, main 结束统一清 WORK
    if not os.path.exists(text):
        line = ("the quick brown fox jumps over the lazy dog 0123456789 "
                "envd gzip transfer bench\n").encode()
        block = line * ((4 << 20) // len(line))
        with open(text, "wb") as f:
            for _ in range(mib // 4):
                f.write(block)
    sha_t = sha256_file(text)
    gp = f"/tmp/benchgz_{box.bid}.bin"
    up_url, _ = box.urls(gp, box.tok)
    p = box.curl(box.sid, up_url, infile=text, post_body=True, maxtime=300)
    if wline(p)[0] != "200":
        fail("PH", box.bid, f"文本 identity 上传失败 {p.stdout[:40]} {p.stderr[:40]}")
        return False
    rep = []
    # gzip 下载: 文本 + 随机(p200 已就绪), 响应须 Content-Encoding: gzip
    for tag, path, want in (("文本", gp, sha_t), ("随机", box.p200, box.sha200)):
        _, dl_url = box.urls(path, box.tok)
        p = box.curl(box.sid, dl_url, out=body, headers=("Accept-Encoding: gzip",),
                     hdr_out=hdr, maxtime=300)
        code, wire, ts = wline(p)
        try:
            enc = next(l.split(":", 1)[1].strip().lower() for l in open(hdr, errors="ignore")
                       if l.lower().startswith("content-encoding:"))
        except (StopIteration, OSError):
            enc = ""
        dsha = _gunzip_sha(body, out) if enc == "gzip" else None
        good = code == "200" and enc == "gzip" and dsha == want
        ok = ok and good
        if not good:
            fail("PH", box.bid, f"gzip 下载({tag}) http={code} enc={enc or '无'} "
                 f"wire={wire/2**20:.1f}MiB sha{'✓' if dsha == want else '✗'} {p.stderr[:40]}")
        rep.append(f"{tag}线上={wire/2**20:.2f}MiB/{ts:.1f}s{'✓' if good else '✗'}")
    # gzip 上传: host 压缩(级6) → envd 服务端流式解压落盘 → guest sha=原文本
    t = time.time()
    with open(text, "rb") as fi, gzip.open(text + ".gz", "wb", compresslevel=6) as fo:
        for c in iter(lambda: fi.read(4 << 20), b""):
            fo.write(c)
    t_gz = time.time() - t
    gzs = os.path.getsize(text + ".gz")
    p = box.curl(box.sid, up_url, infile=text + ".gz", post_body=True,
                 headers=("Content-Encoding: gzip",), maxtime=300)
    code, _, uts = wline(p)  # 上传线上字节=gzs(-w 的 size_download 是响应体大小, 不是上传量)
    good = code == "200"
    if good:
        try:
            r = box.sbx.commands.run(f"sha256sum {gp}", cwd="/app", user=USER, timeout=120)
            good = r.stdout.split()[0] == sha_t
        except Exception as e:
            good = False
            fail("PH", box.bid, f"guest sha 异常 {type(e).__name__}: {str(e)[:60]}")
    ok = ok and good
    if not good:
        fail("PH", box.bid, f"gzip 上传 http={code} {p.stderr[:40]}")
    rep.append(f"上传线上={gzs/2**20:.2f}MiB/{uts:.1f}s{'✓' if good else '✗'}")
    log(f"  box{box.bid} PH: gzip({' | '.join(rep)}) 压缩比≈{mib*2**20/max(gzs, 1):.0f}:1 "
        f"host压缩{t_gz:.1f}s")
    return ok


def guest_envd_rss(box):
    try:
        r = box.sbx.commands.run("ps -o rss= -p $(pidof envd)", cwd="/app",
                                 user=USER, timeout=30)
        return int(r.stdout.split()[0]) // 1024
    except Exception:
        return -1


def run_phase(name, fn, box):
    """单 phase 单箱兜底: 未捕获异常记失败, 不带崩整场 bench."""
    try:
        return fn(box)
    except Exception as e:
        fail(name, box.bid, f"未捕获异常 {type(e).__name__}: {str(e)[:80]}")
        return False


def main():
    os.makedirs(WORK, exist_ok=True)
    if CACERT and not os.path.exists(CACERT):
        print(f"CA 证书不存在: {CACERT} (export BENCH_CACERT=<tls.crt> 修正)", file=sys.stderr)
        sys.exit(2)
    event(f"bench 开始 {NBOX}箱并发 | PROXY={PROXY} NS={NS_TAG} "
          f"CA={CACERT or '(curl env)'} TPL=…{TPL[-16:]} timeout={TIMEOUT_S}s")
    # PB 共享 1GiB 源: 预生成一次, 结束统一清理
    if not os.path.exists(f"{WORK}/src1g.bin"):
        subprocess.run(["dd", "if=/dev/urandom", f"of={WORK}/src1g.bin", "bs=4M",
                        "count=256", "status=none"], check=True)
    sampler = RssSampler()
    sampler.start()
    boxes = [Box(i) for i in range(NBOX)]
    with ThreadPoolExecutor(NBOX) as ex:
        setup_ok = list(ex.map(lambda b: run_phase("P0", lambda bb: setup(bb, 200), b), boxes))
    if not all(setup_ok):
        log("P0 存在失败，继续可执行部分")

    results = {}
    for name, fn in (("PA", phase_a), ("PB", phase_b), ("PC", phase_c),
                     ("PD", phase_d), ("PE", phase_e), ("PH", phase_h)):
        t = time.time()
        with ThreadPoolExecutor(NBOX) as ex:
            results[name] = list(ex.map(lambda b: run_phase(name, fn, b), boxes))
        log(f"{name} 完成 {time.time()-t:.1f}s")
        for b in boxes:
            r = guest_envd_rss(b)
            log(f"  [envd RSS] box{b.bid}: {r}MiB")

    sampler.stop_flag.set(); sampler.join(timeout=3)
    for b in boxes:
        try:
            b.sbx.kill()
        except Exception:
            pass
    for f in os.listdir(WORK):
        try:
            os.remove(f"{WORK}/{f}")
        except OSError:
            pass

    print("\n" + "=" * 72)
    verdict = all(all(v) for v in results.values()) and all(setup_ok)
    print(f"总结: {'✓ 全部通过' if verdict else '✗ 存在失败'} | 墙钟 {time.time()-T0:.0f}s")
    for name, v in results.items():
        print(f"  {name}: {'✓' if all(v) else '✗'} {[('box%d' % i, '✓' if x else '✗') for i, x in enumerate(v)]}")
    if hasattr(boxes[0], "pa_ping") and boxes[0].pa_ping:
        b50, l50, l99, lmax = boxes[0].pa_ping
        print(f"  无卡顿: 空载 ping p50={b50*1000:.0f}ms → 重载 p50={l50*1000:.0f}ms "
              f"p99={l99*1000:.0f}ms max={lmax*1000:.0f}ms")
    print("  RSS(host 侧):")
    print(sampler.report())
    if FAILS:
        print(f"  失败明细({len(FAILS)}):")
        for ph, bx, d in FAILS[:20]:
            print(f"    [{ph}] box{bx}: {d}")
    print(f"\n事件时间线: {[(round(t,1), m) for t, m in EVENTS]}")


if __name__ == "__main__":
    main()
