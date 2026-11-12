#!/usr/bin/env python3
"""envd gzip 传输探针(单箱): 先验证再加进 bench PH.

验证链:
  1. proxy 是否透传 Accept-Encoding / Content-Encoding (envd 原生支持双向 gzip)
  2. 下载: Accept-Encoding: gzip → 响应须带 Content-Encoding: gzip, 落盘 gunzip 后 sha 一致
     - 可压缩文本: 线上字节应远小于原文件
     - 随机数据: 无压缩收益, 线上字节 ≈ 原文件(CPU 开销最差情况)
  3. 上传: host gzip 压缩 + Content-Encoding: gzip → envd 服务端流式解压落盘, guest sha=原文件
"""
import gzip, hashlib, os, subprocess, sys, time
from urllib.parse import urlencode

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dataplane_bench as db
from e2b import Sandbox

MIB = 2**20
TEXT = f"{db.WORK}/gzp_text.bin"
RAND = f"{db.WORK}/gzp_rand.bin"
BODY = f"{db.WORK}/gzp_body.bin"   # 响应原始体(压缩态)
HDR = f"{db.WORK}/gzp_hdr.txt"


def sha(p):
    return db.sha256_file(p)


def make_text(mib):
    line = ("the quick brown fox jumps over the lazy dog 0123456789 "
            "envd gzip transfer bench\n").encode()
    block = line * (4 * MIB // len(line))
    with open(TEXT, "wb") as f:
        for _ in range(mib // 4):
            f.write(block)
        rest = mib * MIB - (mib // 4) * 4 * MIB
        if rest:
            f.write(block[:rest])


def curl_gz_dl(sbx, sid, tok, path):
    q = urlencode({"path": path, "username": db.USER,
                   "signature": db.sgn(path, "read", tok)})
    url = f"{db.PROXY}/files?{q}"
    p = subprocess.run(["curl", "-sS", *(["--cacert", db.CACERT] if db.CACERT else []),
                        "-H", f"E2b-Sandbox-Id: {sid}", "-H", f"E2b-Sandbox-Port: {db.ENVD_PORT}",
                        "-H", "Accept-Encoding: gzip", "-D", HDR, "-o", BODY,
                        "--max-time", "180", "-w", "%{http_code} %{size_download} %{time_total}",
                        url], capture_output=True, text=True)
    return p


def parse_hdr():
    h = {}
    for l in open(HDR, errors="ignore").read().splitlines():
        if ":" in l:
            k, v = l.split(":", 1)
            h[k.strip().lower()] = v.strip()
    return h


def upload(sbx, sid, tok, path, infile, gz=False):
    q = urlencode({"path": path, "username": db.USER,
                   "signature": db.sgn(path, "write", tok)})
    url = f"{db.PROXY}/files?{q}"
    cmd = ["curl", "-sS", *(["--cacert", db.CACERT] if db.CACERT else []),
           "-H", f"E2b-Sandbox-Id: {sid}", "-H", f"E2b-Sandbox-Port: {db.ENVD_PORT}",
           "-X", "POST", "-H", "Content-Type: application/octet-stream"]
    if gz:
        cmd += ["-H", "Content-Encoding: gzip"]
    cmd += ["-T", infile, "-o", "/dev/null", "--max-time", "300",
            "-w", "%{http_code} %{size_upload} %{time_total}", url]
    return subprocess.run(cmd, capture_output=True, text=True)


def guest_sha(sbx, path):
    r = sbx.commands.run(f"sha256sum {path}", cwd="/app", user=db.USER, timeout=120)
    return r.stdout.split()[0]


def main():
    os.makedirs(db.WORK, exist_ok=True)
    print(f"probe: PROXY={db.PROXY} TPL=…{db.TPL[-12:]}")
    make_text(50)
    subprocess.run(["dd", "if=/dev/urandom", f"of={RAND}", "bs=4M",
                    "count=12", "status=none"], check=True)  # 48MiB
    sha_t, sha_r = sha(TEXT), sha(RAND)
    print(f"源就绪: text 50MiB sha={sha_t[:12]}… rand 48MiB sha={sha_r[:12]}…")

    sbx = db._create_sandbox()
    sid, tok = sbx.sandbox_id, sbx._envd_access_token
    gp, rp = "/tmp/gzp_text.bin", "/tmp/gzp_rand.bin"
    rc = 0
    try:
        # 基线: identity 上传(与 bench P0 同路径)
        for f, p in ((TEXT, gp), (RAND, rp)):
            r = upload(sbx, sid, tok, p, f)
            print(f"identity 上传 {os.path.basename(f)}: {r.stdout.strip()} {r.stderr.strip()[:60]}")
            assert r.stdout.split()[0] == "200", "identity 上传失败"

        # --- 1) gzip 下载: 可压缩文本 ---
        p = curl_gz_dl(sbx, sid, tok, gp)
        code, nb, ts = p.stdout.split()
        h = parse_hdr()
        enc = h.get("content-encoding", "(无)")
        wire = int(nb)
        # 解压校验
        dsha = "解压失败"
        try:
            with gzip.open(BODY, "rb") as f, open(BODY + ".out", "wb") as o:
                for c in iter(lambda: f.read(4 * MIB), b""):
                    o.write(c)
            dsha = sha(BODY + ".out")
        except OSError as e:
            print(f"gunzip 失败: {e}")
        ok = code == "200" and enc == "gzip" and dsha == sha_t
        print(f"[gzip下载/文本] http={code} enc={enc} 线上={wire/MIB:.2f}MiB(原50) "
              f"耗时={ts}s sha{'✓' if dsha == sha_t else '✗'} → {'PASS' if ok else 'FAIL'}")
        rc |= 0 if ok else 1

        # --- 2) gzip 下载: 随机数据(最差情况) ---
        p = curl_gz_dl(sbx, sid, tok, rp)
        code, nb, ts = p.stdout.split()
        h = parse_hdr()
        enc = h.get("content-encoding", "(无)")
        wire = int(nb)
        try:
            with gzip.open(BODY, "rb") as f, open(BODY + ".out", "wb") as o:
                for c in iter(lambda: f.read(4 * MIB), b""):
                    o.write(c)
            dsha = sha(BODY + ".out")
        except OSError:
            dsha = None
        ok = code == "200" and enc == "gzip" and dsha == sha_r
        print(f"[gzip下载/随机] http={code} enc={enc} 线上={wire/MIB:.2f}MiB(原48) "
              f"耗时={ts}s sha{'✓' if dsha == sha_r else '✗'} → {'PASS' if ok else 'FAIL'}")
        rc |= 0 if ok else 2

        # --- 3) gzip 上传: envd 服务端解压 ---
        t = time.time()
        with open(TEXT, "rb") as fi, gzip.open(TEXT + ".gz", "wb", compresslevel=6) as fo:
            for c in iter(lambda: fi.read(4 * MIB), b""):
                fo.write(c)
        gz_size = os.path.getsize(TEXT + ".gz")
        t_gz = time.time() - t
        r = upload(sbx, sid, tok, "/tmp/gzp_up.bin", TEXT + ".gz", gz=True)
        code, upb, uts = r.stdout.split()
        gsha = guest_sha(sbx, "/tmp/gzp_up.bin")
        ok = code == "200" and gsha == sha_t
        print(f"[gzip上传/文本] host压缩 {t_gz:.1f}s→{gz_size/MIB:.3f}MiB http={code} "
              f"线上={int(upb)/MIB:.3f}MiB 耗时={uts}s guest sha{'✓' if gsha == sha_t else '✗'} "
              f"→ {'PASS' if ok else 'FAIL'}")
        rc |= 0 if ok else 4

        # 对照: 同文件 identity 上传耗时
        r = upload(sbx, sid, tok, "/tmp/gzp_up.bin", TEXT)
        print(f"[对照 identity上传/文本] {r.stdout.strip()}")
    finally:
        try:
            sbx.kill()
        except Exception:
            pass
        for f in os.listdir(db.WORK):
            if f.startswith("gzp_"):
                try:
                    os.remove(f"{db.WORK}/{f}")
                except OSError:
                    pass
    print(f"结论 rc={rc} (bit0=文本下载 bit1=随机下载 bit2=上传)")
    return rc


if __name__ == "__main__":
    sys.exit(main())
