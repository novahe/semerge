#!/usr/bin/env python3
"""snap_size.py — 查询模板快照的实际磁盘大小。

按 .id 文件（或裸模板 id）解析 location，列出该 location 下所有层文件
（.snapshot = 内存层，.overlay = 磁盘层）及各自大小，并标注 ref 指向哪一层、
restore 实际要叠多少层。

用法：
  python3 snap_size.py                      # 默认 openclaw-e2b-{snp,ws}-template-b256enc.id
  python3 snap_size.py a.id b.id ...        # 指定 id 文件
  python3 snap_size.py e2b-snp-ZmlsZTov...  # 或直接传模板 id
"""
import base64, hashlib, os, re, sys

PARENT = os.path.dirname(os.path.abspath(__file__))
NFS_ROOT = os.environ.get("NFS_SNAP_ROOT", "/mnt/sfs_turbo/nova-snaps")

MIB = 1 << 20


def resolve_ref(template_id):
    """模板 id → (snapshot digest, location uuid)。"""
    s = template_id.strip()
    if s.startswith("e2b-snp-"):
        s = s[len("e2b-snp-"):]
    raw = base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))
    dig = re.search(rb"file://([0-9a-f]{64})", raw)
    loc = re.search(rb"location:([0-9a-f-]{36})", raw)
    if not dig or not loc:
        raise SystemExit(f"✗ 无法从 id 解析 file:// ref: {template_id[:60]}…")
    return dig.group(1).decode(), loc.group(1).decode()


def location_dir(loc):
    h = hashlib.sha256(loc.encode()).hexdigest()[:4]
    return os.path.join(NFS_ROOT, h[:2], h[2:], loc)


def load_id(arg):
    """参数是文件路径/文件名还是裸 id。"""
    for p in (arg, os.path.join(PARENT, arg)):
        if os.path.isfile(p):
            return open(p).read().strip()
    return arg


def main():
    args = sys.argv[1:] or ["openclaw-e2b-snp-template-b256enc.id",
                            "openclaw-e2b-ws-template-b256enc.id"]
    for arg in args:
        tid = load_id(arg)
        dig, loc = resolve_ref(tid)
        d = location_dir(loc)
        label = os.path.basename(arg) if os.path.isfile(arg) or os.path.isfile(os.path.join(PARENT, arg)) else "(inline id)"
        print(f"== {label}")
        if not os.path.isdir(d):
            print(f"   ✗ 快照目录不存在: {d}\n")
            continue
        total = 0
        mem_layers = 0
        enc_state = "未检查"
        for x in sorted(os.listdir(d)):
            p = os.path.join(d, x)
            sz = os.path.getsize(p)
            total += sz
            kind = "内存层" if x.endswith(".snapshot") else "磁盘层"
            mark = " ← ref 指向" if x.startswith(dig) else ""
            enc = ""
            if x.endswith(".snapshot"):
                mem_layers += 1
                # tarstream 加密封套 magic: \x89KTSENC\n（前 8 字节）
                with open(p, "rb") as fh:
                    magic = fh.read(8)
                enc = "  [加密]" if magic == b"\x89KTSENC\n" else "  [明文!]"
                if enc_state == "未检查" or enc.endswith("!"):
                    enc_state = "加密" if not enc.endswith("!") else "存在明文层"
            print(f"   {sz / MIB:9.1f} MiB  {kind}  {x[:20]}…{x[-10:]}{enc}{mark}")
        print(f"   {'-' * 40}")
        print(f"   {total / MIB:9.1f} MiB  合计（{mem_layers} 个内存层，"
              f"restore 叠 {mem_layers} 层，{enc_state}）\n")


if __name__ == "__main__":
    main()
