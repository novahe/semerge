#!/usr/bin/env python3
"""envd 上传/下载 soak：2 沙箱并发 × N 轮(默认每箱 500, 共 1000), 单文件 200MiB.

每轮: 签名 curl 上传(octet-stream) → guest sha256 校验 → 签名 curl 下载 → host sha256 校验.
失败不重试(零容忍计数); 单轮 >90s 记 stall; 每 50 轮 SDK stream 读抽查.
用法: source tenant.env && python3 envd_soak_updown.py [rounds_per_box] [size_mib]
"""
import base64, hashlib, os, subprocess, sys, threading, time
from urllib.parse import urlencode, urlsplit

from e2b import Sandbox

# ---- 可移植配置: 环境变量 > 同目录 bench.env(gitignored, 本节点覆盖) ----
# 跨节点: source 对应节点 tenant.env 即可(E2B_SANDBOX_URL/CURL_CA_BUNDLE 自动推导),
# 模板 ID 用 BENCH_TPL/BENCH_MANIFEST 或 bench.env 提供; 不内置任何节点信息.
_HERE = os.path.dirname(os.path.abspath(__file__))
for _kv in (l.strip() for l in open(f"{_HERE}/bench.env", errors="ignore")
            if l.strip() and not l.lstrip().startswith("#")):
    if "=" in _kv:
        _k, _v = _kv.split("=", 1)
        os.environ.setdefault(_k.strip(), _v.strip())  # 已有环境变量优先

_E2B_DOMAIN = os.environ.get("E2B_DOMAIN", "")
_SANDBOX_URL = os.environ.get("E2B_SANDBOX_URL", "")
PROXY = (os.environ.get("BENCH_PROXY")
         or (urlsplit(_SANDBOX_URL)._replace(query="", fragment="").geturl() if _SANDBOX_URL else "")
         or (f"https://{_E2B_DOMAIN}:{os.environ.get('BENCH_PROXY_PORT', '32591')}" if _E2B_DOMAIN else ""))
CACERT = (os.environ.get("BENCH_CACERT") or os.environ.get("CURL_CA_BUNDLE")
          or os.environ.get("SSL_CERT_FILE") or "")


def _tpl_from_manifest(m):
    if not m.startswith(("manifest://", "registry://")):
        m = "manifest://" + m
    # conductor ref 解码用 raw base64, padding(=) 会报 illegal base64 → 500
    return "e2b-img-" + base64.b64encode(m.encode()).decode().rstrip("=")


TPL = (os.environ.get("BENCH_TPL")
       or (os.environ.get("BENCH_MANIFEST") and _tpl_from_manifest(os.environ["BENCH_MANIFEST"]))
       or "")
if not PROXY or not TPL:
    print("缺少配置: " + "; ".join(n for n, v in
          (("BENCH_PROXY 或 E2B_SANDBOX_URL/E2B_DOMAIN", PROXY),
           ("BENCH_TPL 或 BENCH_MANIFEST", TPL)) if not v)
          + "\n  跑法: source <节点>/tenant.env 后直跑, 或 export BENCH_*, "
            "或在本目录 bench.env(gitignored)写节点相关值",
          file=sys.stderr)
    sys.exit(2)
USER = os.environ.get("BENCH_USER", "root")
TIMEOUT_S = int(os.environ.get("BENCH_TIMEOUT", "1800"))  # ≥数据面空闲窗口, 防 300s 自动 pause
PRINT = threading.Lock()
T0 = time.time()


def log(msg):
    with PRINT:
        print(f"[{time.time()-T0:7.1f}s] {msg}", flush=True)


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(len(xs) * p / 100))]


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def sgn(path, op, tok):
    return "v1_" + base64.b64encode(hashlib.sha256(
        f"{path}:{op}:{USER}:{tok}".encode()).digest()).decode().rstrip("=")


def run_box(bid, rounds, size_mib):
    guest_path = f"/tmp/soak_rw_{bid}.bin"
    src, dst = f"/tmp/soak_src_{bid}.bin", f"/tmp/soak_dl_{bid}.bin"
    # 每箱独立随机源文件
    subprocess.run(["dd", "if=/dev/urandom", "of="+src, "bs=4M",
                    f"count={size_mib//4}", "status=none"], check=True)
    src_sha = sha256_file(src)
    log(f"box{bid}: 源文件就绪 {size_mib}MiB sha={src_sha[:16]}…")

    sbx = Sandbox.create(TPL, timeout=TIMEOUT_S)
    sbx.set_timeout(TIMEOUT_S)
    sid = sbx.sandbox_id
    tok = sbx._envd_access_token
    up_url = f"{PROXY}/files?" + urlencode({"path": guest_path, "username": USER, "signature": sgn(guest_path, "write", tok)})
    dl_url = f"{PROXY}/files?" + urlencode({"path": guest_path, "username": USER, "signature": sgn(guest_path, "read", tok)})
    hdrs = ["-H", f"E2b-Sandbox-Id: {sid}", "-H", "E2b-Sandbox-Port: 49983"]

    st = {"rounds": 0, "up_fail": 0, "guest_sha_fail": 0, "dl_fail": 0,
          "host_sha_fail": 0, "sdk_fail": 0, "stalls": 0, "rt": [], "up": [], "dl": []}
    try:
        for i in range(1, rounds + 1):
            t = time.time()
            fail = None
            # 1) 上传
            p = subprocess.run(["curl", "-sS", *(["--cacert", CACERT] if CACERT else []),
                                "-X", "POST", *hdrs,
                                "-H", "Content-Type: application/octet-stream",
                                "-T", src, "--max-time", "120",
                                "-w", "%{http_code} %{size_upload} %{time_total}",
                                "-o", "/dev/null", up_url], capture_output=True, text=True)
            try:
                code, upb, ups = p.stdout.split()
                st["up"].append(float(ups))
                if p.returncode or code != "200" or int(upb) != size_mib * 2**20:
                    fail = f"upload rc={p.returncode} http={code} {int(upb)/2**20:.1f}MiB {p.stderr.strip()[:60]}"
            except ValueError:
                fail = f"upload parse: {p.stdout[:60]}"
            # 2) guest sha
            if not fail:
                r = sbx.commands.run(f"sha256sum {guest_path}", cwd="/app", user=USER, timeout=120)
                if r.stdout.split()[0] != src_sha:
                    fail = f"guest sha mismatch @{r.stdout.split()[0][:12]}"
            if fail:
                st["up_fail"] += 1 if fail.startswith("upload") else st["up_fail"]
                if "guest sha" in fail:
                    st["guest_sha_fail"] += 1
                else:
                    st["up_fail"] += 1
                log(f"box{bid} #{i} ✗ {fail}")
                continue
            # 3) 下载
            p = subprocess.run(["curl", "-sS", *(["--cacert", CACERT] if CACERT else []),
                                *hdrs, "--max-time", "120",
                                "-o", dst, "-w", "%{http_code} %{size_download} %{time_total}",
                                dl_url], capture_output=True, text=True)
            try:
                code, dlb, dls = p.stdout.split()
                st["dl"].append(float(dls))
                if p.returncode or code != "200" or int(dlb) != size_mib * 2**20:
                    fail = f"download rc={p.returncode} http={code} {int(dlb)/2**20:.1f}MiB {p.stderr.strip()[:60]}"
            except ValueError:
                fail = f"download parse: {p.stdout[:60]}"
            # 4) host sha
            if not fail and sha256_file(dst) != src_sha:
                fail = "host sha mismatch"
            if fail:
                st["dl_fail"] += 1 if "download" in fail else 0
                if "host sha" in fail:
                    st["host_sha_fail"] += 1
                else:
                    st["dl_fail"] += 1
                log(f"box{bid} #{i} ✗ {fail}")
                continue
            rt = time.time() - t
            st["rounds"] += 1
            st["rt"].append(rt)
            if rt > 90:
                st["stalls"] += 1
                log(f"box{bid} #{i} ⚠ stall {rt:.1f}s")
            # 5) SDK stream 抽查(每 50 轮)
            if i % 50 == 0:
                try:
                    got, t1 = 0, time.time()
                    with sbx.files.read(guest_path, format="stream", user=USER, request_timeout=120) as f:
                        for chunk in f:
                            got += len(chunk)
                    assert got == size_mib * 2**20
                    log(f"box{bid} #{i} SDK抽查 ok {got/2**20/(time.time()-t1):.0f}MiB/s")
                except Exception as e:
                    st["sdk_fail"] += 1
                    log(f"box{bid} #{i} SDK抽查 ✗ {type(e).__name__}: {str(e)[:80]}")
            if i % 25 == 0:
                log(f"box{bid} #{i}/{rounds} 累计失败={st['up_fail']+st['guest_sha_fail']+st['dl_fail']+st['host_sha_fail']+st['sdk_fail']} "
                    f"轮时p50={pct(st['rt'],50):.1f}s max={max(st['rt']):.1f}s")
    finally:
        try:
            sbx.kill()
        except Exception:
            pass
    log(f"box{bid} 完成: 成功轮={st['rounds']}/{rounds} 上传失败={st['up_fail']} guest_sha失败={st['guest_sha_fail']} "
        f"下载失败={st['dl_fail']} host_sha失败={st['host_sha_fail']} SDK失败={st['sdk_fail']} stall={st['stalls']}")
    return st, src_sha


def main():
    rounds = int(sys.argv[1]) if len(sys.argv) > 1 else 500
    size_mib = int(sys.argv[2]) if len(sys.argv) > 2 else 200
    log(f"soak 开始: 2 箱 × {rounds} 轮 × {size_mib}MiB (上传+下载+双向sha256校验)")
    results = [None, None]
    threads = [threading.Thread(target=lambda i=i: results.__setitem__(
        i, run_box(i, rounds, size_mib))) for i in (0, 1)]
    for th in threads:
        th.start()
    for th in threads:
        th.join()
    ok = all(r[0]["rounds"] == rounds and all(
        r[0][k] == 0 for k in ("up_fail", "guest_sha_fail", "dl_fail", "host_sha_fail", "sdk_fail", "stalls"))
        for r in results if r)
    ups = [t for r in results if r for t in r[0]["up"]]
    dls = [t for r in results if r for t in r[0]["dl"]]
    rts = [t for r in results if r for t in r[0]["rt"]]
    print(f"\n{'='*70}\n总结: {'✓ 全部通过' if ok else '✗ 存在失败'} | 墙钟 {time.time()-T0:.0f}s")
    if ups:
        print(f"上传 200MiB: p50={pct(ups,50):.2f}s p90={pct(ups,90):.2f}s max={max(ups):.2f}s ({size_mib/pct(ups,50):.0f}MiB/s)")
        print(f"下载 200MiB: p50={pct(dls,50):.2f}s p90={pct(dls,90):.2f}s max={max(dls):.2f}s ({size_mib/pct(dls,50):.0f}MiB/s)")
        print(f"整轮(含双侧sha+exec): p50={pct(rts,50):.1f}s p90={pct(rts,90):.1f}s max={max(rts):.1f}s")
    for i, r in enumerate(results):
        if r:
            s = r[0]
            print(f"box{i}: {s}")


if __name__ == "__main__":
    main()
