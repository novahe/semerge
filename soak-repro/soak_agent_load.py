#!/usr/bin/env python3
"""长稳（soak）用例：单沙箱 + agent 负载模型循环 + pause/reuse 一致性，跑到失败为止。

负载模型（常规 agent 形态：突发内存 + 磁盘读写，2C/8G）：
  每轮（round）依次跑一遍全部负载，然后 pause→reuse：
    1. crud    磁盘 CRUD —— 新增 r{i}.bin（CRUD_NEW_MB 随机写）
               + mutable.bin 原地重写（CRUD_REWRITE_MB）
               + stable.log 追加 + 滚动窗口删最老文件（窗口让磁盘峰值 ≤ DISK_PEAK_MB）
    2. fio     磁盘压测 —— 引擎 psync/libaio/io_uring × randwrite/randrw/randread
               轮转，4k direct，time_based FIO_RUNTIME_S，专用数据集 FIO_SIZE_MB
    3. memburst 突发内存 ×MEM_BURSTS —— 固定分配 MEM_TOTAL_MIB×MEM_TARGET_PCT%
               （写死 8G×90%，不探测 guest：内有活动 balloon 读数不可信，
               气球按需回吐；OOM=失败），64MiB 分块 ramp，保持 MEM_HOLD_S 后释放
    4. snapshot 全数据目录 sha256 → 期望状态 E_i
    5. pause→reuse  POST /pause → POST /connect → SDK 重连 → 冒烟
    6. verify   再次 snapshot，必须与 E_i 完全一致（缺失/多出/变更 = 失败）
    7. bench    固定 IO 量 fio 基准 ×3 类（独立文件）×BENCH_REPS 次：
                 A randread 4k / B randwrite 4k / C seqread 1M（psync, direct, 单队列）
               每次 rep 前 sync+drop_caches 取冷态；round 1（=1 次 pause 后）为基线，
               后续轮任一类型中位数 > 基线×REGRESSION_FACTOR → 判失败（pause 次数
               累积导致同量 IO 耗时明显增加的回归检测）

  一直循环（ROUNDS=0 表示无限），任何一步失败立即停止：
    保留沙箱供取证（KEEP_ON_FAIL=1），抓取 guest 内诊断（dmesg/df/meminfo/各阶段日志）
    与 GET /sandboxes/{sid} 状态，写 result.json 后退出码 1。
  结束汇总：pause/resume 耗时 min/p50/avg/p90/max，各 fio 类型全部 rep 值与同统计。

fio 说明：guest 镜像（code-interpreter, Debian trixie）不带 fio 且 apt 不可达、sandbox
内核无 SysV IPC，因此从宿主机上传 fio + 依赖 so + 引擎插件，并用 shmshim.so
（LD_PRELOAD，shmget→匿名共享 mmap，见 guest-tools/shmshim.c）垫过。上传物只落
overlay，pause/resume 全程保留，整个 soak 只需上传一次。

执行链路：生命周期（create/pause/connect/delete）直调 node-ctl 的 e2b 兼容 API
（e2b_env.api_request），负载命令经 e2b SDK（envd exec）下发；长负载一律
"后台启动 + 轮询 ctl/<name>.rc"，规避 envd 流对个别命令不稳的问题，且所有阶段日志
持久化在 guest /root/soak/ctl/（随 checkpoint 保留，失败后可回看）。

配置（环境变量，均有默认）：
  E2B_ENV_FILE     默认 nova 租户 env（/root/sandbox/nova-work/etc/node-ctl-nova/tenant.env）
  MANIFEST_KEY     默认 5072d038…（nova 2C/8G code-interpreter 镜像）
  ROUNDS           轮数，0=无限直到失败（默认 0）
  SANDBOX_TIMEOUT  沙箱空闲/生命周期秒（默认 21600，每轮 connect 会刷新）
  DISK_PEAK_MB     磁盘数据峰值预算（默认 10240 = 10GiB）
  FIO_SIZE_MB      fio 专用数据集大小（默认 2048）
  CRUD_NEW_MB      每轮新增文件大小（默认 256）
  CRUD_REWRITE_MB  每轮原地重写大小（默认 64）
  FIO_RUNTIME_S    每个 fio job 时长（默认 30）
  FIO_BS_KB        fio 块大小 KiB（默认 4）
  MEM_TARGET_PCT   内存突发目标（固定 8G 的百分比，默认 90）
  MEM_TOTAL_MIB    内存总量基数（写死默认 8192，不读 guest）
  MEM_BURSTS       每轮突发次数（默认 3）
  MEM_HOLD_S       每次突发保持秒（默认 15）
  MEM_GAP_S        突发间隔秒（默认 5）
  BENCH_A_MB/B_MB/C_MB  回归基准数据集大小（默认 256/256/512）
  BENCH_REPS       每类基准每轮重复次数（默认 3，记全部原始值，取中位数判回归）
  REGRESSION_FACTOR    回归阈值倍数（默认 2.0：耗时超基线 2 倍判失败）
  POLL_INTERVAL    阶段轮询间隔秒（默认 3）
  PAUSE_HTTP_TIMEOUT / RESUME_HTTP_TIMEOUT   两阶段 HTTP 超时（默认 3600 / 600）
  KEEP_ON_FAIL     失败时保留沙箱（默认 1）
  KEEP_ON_INT      Ctrl+C 中断时保留沙箱（默认 0）
  FIO_BIN / FIO_LIBDIR / FIO_PLUGDIR / SHIM_SO  宿主机 fio 与垫片路径

结果：repro-results/soak-agent-<ts>/rounds.jsonl（逐轮）+ result.json（汇总/失败诊断）。

示例：
  python3 soak_agent_load.py                       # 无限轮，直到失败
  ROUNDS=24 python3 soak_agent_load.py             # 跑 24 轮
  CRUD_NEW_MB=64 DISK_PEAK_MB=2048 ROUNDS=1 FIO_RUNTIME_S=10 MEM_BURSTS=1 MEM_HOLD_S=5 \
    python3 soak_agent_load.py                     # 快速冒烟
"""
import base64
import glob
import json
import os
import re
import signal
import subprocess
import sys
import time

PARENT = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, PARENT)
import e2b_env

e2b_env.load(os.environ.get("E2B_ENV_FILE")
             or "/root/sandbox/nova-work/etc/node-ctl-nova/tenant.env")
e2b_env.require("E2B_DOMAIN", "E2B_API_KEY", "SSL_CERT_FILE")
from e2b import Sandbox as EnvdSandbox

MANIFEST_KEY = os.environ.get(
    "MANIFEST_KEY",
    # 2026-10-25 nova build 01a17116 产物（main 栈 2C/8G 沙箱内展平）
    "5072d03872754f2c276fc229a6984617e646bdb61e7664c828e8eeee83c6c1b2",
)
ROUNDS = int(os.environ.get("ROUNDS", "0"))                      # 0 = 无限
SANDBOX_TIMEOUT = int(os.environ.get("SANDBOX_TIMEOUT", "21600"))
DISK_PEAK_MB = int(os.environ.get("DISK_PEAK_MB", "10240"))
FIO_SIZE_MB = int(os.environ.get("FIO_SIZE_MB", "2048"))
CRUD_NEW_MB = int(os.environ.get("CRUD_NEW_MB", "256"))
CRUD_REWRITE_MB = int(os.environ.get("CRUD_REWRITE_MB", "64"))
FIO_RUNTIME_S = int(os.environ.get("FIO_RUNTIME_S", "30"))
FIO_BS_KB = int(os.environ.get("FIO_BS_KB", "4"))
MEM_TARGET_PCT = float(os.environ.get("MEM_TARGET_PCT", "90"))
MEM_TOTAL_MIB = int(os.environ.get("MEM_TOTAL_MIB", "8192"))   # 写死 8G：guest 内有活动 balloon，读数不可信
MEM_BURSTS = int(os.environ.get("MEM_BURSTS", "3"))
MEM_HOLD_S = float(os.environ.get("MEM_HOLD_S", "15"))
MEM_GAP_S = float(os.environ.get("MEM_GAP_S", "5"))
BENCH_A_MB = int(os.environ.get("BENCH_A_MB", "256"))
BENCH_B_MB = int(os.environ.get("BENCH_B_MB", "256"))
BENCH_C_MB = int(os.environ.get("BENCH_C_MB", "512"))
BENCH_REPS = int(os.environ.get("BENCH_REPS", "3"))
REGRESSION_FACTOR = float(os.environ.get("REGRESSION_FACTOR", "2.0"))
BENCH_TOTAL_MB = BENCH_A_MB + BENCH_B_MB + BENCH_C_MB
BENCH_DEADLINE = int(os.environ.get("BENCH_DEADLINE", "2400"))
POLL_INTERVAL = float(os.environ.get("POLL_INTERVAL", "3"))
PAUSE_HTTP_TIMEOUT = int(os.environ.get("PAUSE_HTTP_TIMEOUT", "3600"))
RESUME_HTTP_TIMEOUT = int(os.environ.get("RESUME_HTTP_TIMEOUT", "600"))
KEEP_ON_FAIL = os.environ.get("KEEP_ON_FAIL", "1") not in ("0", "false", "no")
KEEP_ON_INT = os.environ.get("KEEP_ON_INT", "0") not in ("0", "false", "no")

FIO_BIN = os.environ.get("FIO_BIN", "/usr/bin/fio")
FIO_LIBDIR = os.environ.get("FIO_LIBDIR", "/usr/lib64")
FIO_PLUGDIR = os.environ.get("FIO_PLUGDIR", "/usr/lib64/fio")
SHIM_SO = os.environ.get(
    "SHIM_SO", os.path.join(PARENT, "guest-tools", "shmshim.so"))

ENGINES = ("psync", "libaio", "io_uring")
RWS = ("randwrite", "randrw", "randread")
# 滚动窗口：data 目录最多保留 KEEP 个 r*.bin。预算 = 峰值上限，扣除：
#   fio 数据集 + bench 数据集（setup 预分配、原地复写、体积恒定）
#   + mutable.bin 重写 + 瞬态多出的 1 个新文件（先建后删） + 日志/工具链 slack
KEEP = max(1, (DISK_PEAK_MB - FIO_SIZE_MB - BENCH_TOTAL_MB - CRUD_REWRITE_MB
               - 2 * CRUD_NEW_MB - 64) // max(CRUD_NEW_MB, 1))
RUN_TIMEOUT = 120                      # 短命令（exec/读文件）超时
CRUD_DEADLINE = 900
FIO_DEADLINE = FIO_RUNTIME_S + 600
BURST_DEADLINE = int(MEM_HOLD_S) + 180
SNAP_DEADLINE = 900
DATA_DIR = "/root/soak/data"           # CRUD 数据（参与一致性校验）
FIO_DIR = "/root/soak/fio"             # fio 数据集（fio.bin 参与一致性校验）
BENCH_DIR = "/root/soak/bench"         # 回归基准数据集（b.bin 每轮被复写，不参与校验）
CTL_DIR = "/root/soak/ctl"             # 阶段日志/快照（不参与校验）
RESULTS_DIR = os.path.join(
    PARENT, "repro-results",
    f"soak-agent-{time.strftime('%Y%m%d-%H%M%S')}")
# guest 证据外带目录：diff 盘本身是被测对象、不可信，负载输出必须落到宿主机侧
GUEST_LOG_DIR = os.path.join(RESULTS_DIR, "guest")

_STOP = {"flag": False}                # Ctrl+C / SIGTERM 优雅退出


def template_id(manifest_key):
    payload = ("manifest://" + manifest_key).encode()
    encoded = base64.urlsafe_b64encode(payload).decode().rstrip("=")
    return "e2b-img-" + encoded


# ---------------------------------------------------------------- guest worker
# 长负载一律由 run_phase.sh 后台执行，产物落 /root/soak/ctl/<name>.{out,rc}，
# host 侧只轮询 rc 文件 —— 不依赖 envd exec 流长时间保持。
GUEST_ENV_SH = f"""\
# soak 阶段公共环境（由 soak_agent_load.py 生成，参数以启动时配置为准）
export PATH=/root/soak/bin:$PATH
export LD_LIBRARY_PATH=/root/soak/bin
export LD_PRELOAD=/root/soak/bin/shmshim.so
export KEEP={KEEP} CRUD_NEW_MB={CRUD_NEW_MB} CRUD_REWRITE_MB={CRUD_REWRITE_MB}
export FIO_SIZE_MB={FIO_SIZE_MB} FIO_RUNTIME_S={FIO_RUNTIME_S} FIO_BS_KB={FIO_BS_KB}
export MEM_TARGET_PCT={MEM_TARGET_PCT} MEM_TOTAL_MIB={MEM_TOTAL_MIB} MEM_HOLD_S={MEM_HOLD_S}
export BENCH_REPS={BENCH_REPS} BENCH_A_MB={BENCH_A_MB} BENCH_B_MB={BENCH_B_MB} \\
       BENCH_C_MB={BENCH_C_MB} BENCH_DIR={BENCH_DIR}
"""

GUEST_RUN_PHASE_SH = """\
#!/bin/bash
# run_phase.sh <name> <id> [args…] —— 幂等启动后台阶段。
# 产物落 ctl/<name>.<id>.{out,rc}；<name>.<id>.lock/ 目录原子创建做并发锁：
# 同 id 重复启动（host 侧启动 exec 超时重试）只会真正执行一次，重试方收到
# already-running 后继续轮询同一个 rc 文件；不同 id 互不干扰，旧 rc 永远不会被
# 新一轮误读。
name=$1; id=$2; shift 2
cd /root/soak/ctl || exit 1
if ! mkdir "$name.$id.lock" 2>/dev/null; then
    echo "already-running $name.$id"
    exit 0
fi
( bash /root/soak/bin/$name.sh "$@" > "$name.$id.out" 2>&1
  echo $? > "$name.$id.rc"
  rmdir "$name.$id.lock" ) &
echo "started $name.$id"
"""

GUEST_CRUD_SH = """\
#!/bin/bash
# crud.sh <round> —— 新增 + 原地重写 + 追加 + 滚动删除
set -e
source /root/soak/bin/env.sh
i=$1
dd if=/dev/urandom of=/root/soak/data/r$i.bin bs=8M count=$((CRUD_NEW_MB/8)) \
   conv=fsync status=none
dd if=/dev/urandom of=/root/soak/data/mutable.bin bs=8M \
   count=$((CRUD_REWRITE_MB/8)) conv=fsync status=none
printf 'round-%s %s\\n' "$i" "$(date -Is)" >> /root/soak/data/stable.log
ls -1v /root/soak/data/r*.bin 2>/dev/null | head -n -$KEEP | xargs -r rm -f
sync
echo "crud-ok round=$i keep=$KEEP files=$(ls /root/soak/data | wc -l) du=$(du -sm /root/soak/data | cut -f1)MB"
"""

GUEST_FIO_SH = """\
#!/bin/bash
# fio.sh <engine> <rw> —— 单 job 4k direct，json 落盘后摘要点
set -e
source /root/soak/bin/env.sh
eng=$1; rw=$2
fio --name=soak --filename=/root/soak/fio/fio.bin --size=${FIO_SIZE_MB}m \\
    --rw=$rw --bs=${FIO_BS_KB}k --ioengine=$eng --direct=1 --time_based \\
    --runtime=${FIO_RUNTIME_S} --group_reporting \\
    --output-format=json --output=/root/soak/fio/last.json
python3 - <<'PY'
import json
j = json.load(open('/root/soak/fio/last.json'))
job = j['jobs'][0]
r, w = job['read'], job['write']
rlat = r.get('lat_ns', {}).get('mean', 0) / 1000
wlat = w.get('lat_ns', {}).get('mean', 0) / 1000
print(f"fio-ok riops={r['iops']:.0f} wiops={w['iops']:.0f} "
      f"rbw={r['bw']/1024:.1f}MiBps wbw={w['bw']/1024:.1f}MiBps "
      f"rlat_us={rlat:.1f} wlat_us={wlat:.1f} "
      f"err={job.get('error', 0)}")
PY
"""

GUEST_BURST_PY = f"""\
#!/usr/bin/env python3
# burst.py —— 突发内存：固定分配 MEM_TOTAL_MIB×MEM_TARGET_PCT%（写死，不探测
# guest 内存——本栈 guest 里有活动 balloon，MemTotal/MemAvailable 读数不可信；
# 气球按需回吐，直接分配即可）。64MiB 分块 ramp，保持 MEM_HOLD_S 后进程退出
# 即释放。OOM 被杀（rc=137）由 host 侧判失败——那本身就是有效发现。
import time

TOTAL_MIB = {MEM_TOTAL_MIB}
TARGET_PCT = {MEM_TARGET_PCT}
HOLD_S = {MEM_HOLD_S}
CHUNK_MIB = 64
N_CHUNKS = int(TOTAL_MIB * TARGET_PCT / 100) // CHUNK_MIB

t0 = time.monotonic()
chunks = []
for _ in range(N_CHUNKS):
    chunks.append(bytearray(CHUNK_MIB * 1024 * 1024))   # 零填充 = 立即吃页
    time.sleep(0.02)
hwm = next(int(l.split()[1]) for l in open('/proc/self/status')
           if l.startswith('VmHWM'))
time.sleep(HOLD_S)
print(f"burst-ok alloc_mib={{N_CHUNKS * CHUNK_MIB}} ramp_s="
      f"{{time.monotonic() - t0:.1f}} hwm_kib={{hwm}} "
      f"(fixed {{TOTAL_MIB}}MiB×{{TARGET_PCT}}%)")
"""

GUEST_SNAPSHOT_SH = """\
#!/bin/bash
# snapshot.sh —— 全量一致性快照：data+fio 参与校验的文件 sha256；volatile 的
# fio/last.json 与 ctl/ 不参与。pipefail：find/sort/xargs 任一失败都不许被
# 管道末端的退出码掩盖（校验清单必须完整可信）。
set -eo pipefail
cd /root/soak
find data fio -type f ! -name 'last.json' -print0 | sort -z \\
    | xargs -0 sha256sum > ctl/snapshot.txt
echo "snap-du data=$(du -sm data | cut -f1)MB fio=$(du -sm fio | cut -f1)MB"
grep -E 'MemTotal|MemAvailable' /proc/meminfo
echo "ps-top:"
ps aux --sort=-rss | head -6
"""

GUEST_BENCH_SH = """\
#!/bin/bash
# bench.sh —— 固定 IO 量回归基准（agent 常见 IO 画像，缓冲 IO + 冷态）：
#   A randread 4k      小文件随机读（源码/配置/库）
#   B randrw 4k 70/30  读写混合、读多写少（典型 agent 工作态），计时含末尾 sync
#   C seqread 1m       大块顺序读（依赖/数据集加载）
# 每类独立文件、psync 单队列；每次 rep 前 sync+drop_caches 取冷态。
# 固定 size+bs ⇒ 每轮 IO 次数完全一致，耗时可直接跨轮（= 跨 pause 次数）比较。
# 计时用墙钟（date +%s%3N，含 fio 启动 + 写回 sync），不依赖 fio 内部 runtime。
set -e
source /root/soak/bin/env.sh
drop() { sync; echo 3 > /proc/sys/vm/drop_caches 2>/dev/null \\
         || echo "warn: drop_caches 不可用（rep 间为热态，跨轮仍可比）"; }

run_bench() {   # $1=type $2=file $3=size_m $4=bs $5=rw $6=rwmixread(可空) $7=rep
    drop
    t0=$(date +%s%3N)
    fio --name=bench --filename="$2" --size="$3"m --bs="$4" --rw="$5" \\
        ${6:+--rwmixread=$6} --ioengine=psync --group_reporting \\
        --output-format=json --output=/root/soak/ctl/bench-last.json
    sync
    t1=$(date +%s%3N)
    TYPE="$1" REP="$7" WALL=$((t1 - t0)) python3 - <<'PY'
import json, os
j = json.load(open('/root/soak/ctl/bench-last.json'))
job = j['jobs'][0]
r, w = job['read'], job['write']
iops = r['iops'] + w['iops']
bw = (r['bw'] + w['bw']) / 1024
print(f"bench-ok type={os.environ['TYPE']} rep={os.environ['REP']} "
      f"wall_ms={os.environ['WALL']} iops={iops:.0f} bw={bw:.1f}MiBps")
PY
}

for rep in $(seq 1 $BENCH_REPS); do
    run_bench A $BENCH_DIR/a.bin $BENCH_A_MB 4k randread ""  $rep
    run_bench B $BENCH_DIR/b.bin $BENCH_B_MB 4k randrw    70  $rep
    run_bench C $BENCH_DIR/c.bin $BENCH_C_MB 1m read      ""  $rep
done
echo "bench-done reps=$BENCH_REPS"
"""

GUEST_DIAG_SH = """\
#!/bin/bash
# diag.sh —— 失败取证（host 侧尽力调用一次）
echo "=== $(date -Is) ==="
echo "--- df /"; df -h /
echo "--- meminfo(head)"; head -4 /proc/meminfo
echo "--- dmesg tail"; dmesg 2>/dev/null | tail -30 || echo "(dmesg 不可用)"
echo "--- ps rss top"; ps aux --sort=-rss | head -10
echo "--- ctl 目录"; ls -la /root/soak/ctl/ 2>/dev/null
for f in /root/soak/ctl/*.out; do
    [ -f "$f" ] || continue
    echo "--- tail $f"; tail -8 "$f"
done
"""


# ---------------------------------------------------------------- host helpers
def api(method, path, timeout=120, **kwargs):
    return e2b_env.api_request(method, path, timeout=timeout, **kwargs)


def run_raw(envd, command, timeout=RUN_TIMEOUT):
    """commands.run，非零退出码不抛（返回真实 exit/stdout/stderr）。"""
    try:
        result = envd.commands.run(command, user="root", cwd="/",
                                   timeout=timeout)
        return (result.exit_code, str(result.stdout or ""),
                str(result.stderr or ""))
    except Exception as e:
        code = getattr(e, "exit_code", None)
        if code is None:
            raise
        return (code, str(getattr(e, "stdout", "") or ""),
                str(getattr(e, "stderr", "") or "") or str(e))


def run(envd, command, timeout=RUN_TIMEOUT):
    code, out, err = run_raw(envd, command, timeout)
    if code != 0:
        raise RuntimeError(f"exit={code} stdout={out[:300]!r} stderr={err[:300]!r}")
    return out


def add_hosts(sid, domain):
    with open("/etc/hosts") as hosts:
        lines = hosts.read().splitlines()
    add = [f"127.0.0.1 {port}-{sid}.{domain} #soak"
           for port in ("49983", "49999")
           if not any(f"{port}-{sid}.{domain}" in line for line in lines)]
    if add:
        with open("/etc/hosts", "a") as hosts:
            hosts.write("\n".join(add) + "\n")


def del_hosts(sid, domain):
    with open("/etc/hosts") as hosts:
        kept = [l for l in hosts.read().splitlines()
                if not any(f"{p}-{sid}.{domain}" in l for p in ("49983", "49999"))]
    with open("/etc/hosts", "w") as hosts:
        hosts.write("\n".join(kept) + "\n")


def connect_sdk(sid, with_hosts=False):
    domain = os.environ["E2B_DOMAIN"]
    if with_hosts:
        add_hosts(sid, domain)
        return EnvdSandbox.connect(sid, timeout=SANDBOX_TIMEOUT), True
    try:
        return EnvdSandbox.connect(sid, timeout=SANDBOX_TIMEOUT), False
    except Exception:
        add_hosts(sid, domain)
        return EnvdSandbox.connect(sid, timeout=SANDBOX_TIMEOUT), True


def pause_sandbox(envd, sid):
    """SDK pause（keep_memory=True）。SDK 会把非 200 响应吞成怪异异常，失败时用
    原始 API 重试并还原真实原因；重复 pause 已停沙箱视为成功。"""
    t0 = time.time()
    try:
        envd.pause(request_timeout=PAUSE_HTTP_TIMEOUT)
    except Exception as e:
        r = api("POST", f"/sandboxes/{sid}/pause", timeout=PAUSE_HTTP_TIMEOUT)
        if r.status_code not in (200, 204) and \
                not ("already" in r.text.lower() or r.status_code == 409):
            raise RuntimeError(f"pause 失败（SDK: {str(e)[:120]}；"
                               f"HTTP {r.status_code}: {r.text[:200]}）")
    time.sleep(2)          # 后台收尾余量，计时只算 API 墙钟
    return (time.time() - t0) * 1000


def resume_sandbox(sid, hosts_added):
    """SDK resume：EnvdSandbox.connect 内部即 POST /connect + envd 客户端建立。
    返回 (envd, hosts_added, api_ms)。"""
    t0 = time.time()
    try:
        envd = EnvdSandbox.connect(sid, timeout=SANDBOX_TIMEOUT,
                                   request_timeout=RESUME_HTTP_TIMEOUT)
    except Exception:
        if hosts_added:
            raise
        add_hosts(sid, os.environ["E2B_DOMAIN"])   # 数据面路由兜底再试一次
        envd = EnvdSandbox.connect(sid, timeout=SANDBOX_TIMEOUT,
                                   request_timeout=RESUME_HTTP_TIMEOUT)
        hosts_added = True
    return envd, hosts_added, (time.time() - t0) * 1000


def pstats(values):
    """毫秒列表 → min/p50/avg/p90/max 统计（1 位小数）。"""
    if not values:
        return None
    s = sorted(values)
    n = len(s)
    pick = lambda p: s[min(n - 1, int(round(p / 100 * (n - 1))))]
    return {"min": round(s[0], 1), "p50": round(pick(50), 1),
            "avg": round(sum(s) / n, 1), "p90": round(pick(90), 1),
            "max": round(s[-1], 1), "n": n}


_PHASE_SEQ = [0]


def phase(envd, name, args=(), deadline=600):
    """幂等启动 guest 后台阶段并轮询 ctl/<name>.<id>.rc；返回 (rc, out)。

    每次调用分配唯一 id：启动 exec 超时重试不会重复执行负载（guest 端锁目录
    保证），也绝不会误读上一轮的 rc。轮询 exec 可能因 guest IO 打满暂时超时
    ——只重试不判死，阶段成败以 rc 文件 + deadline 为准。

    输出持续外带：guest 内的 ctl/*.out 落在 diff 盘上，沙箱中途挂掉即失证——
    每个轮询周期把 .out 增量拉回宿主机 guest/ 目录，最多丢一个轮询间隔。"""
    _PHASE_SEQ[0] += 1
    pid_ = f"r{_PHASE_SEQ[0]}"
    arg_s = " ".join(str(a) for a in args)
    rc_path = f"{CTL_DIR}/{name}.{pid_}.rc"
    out_path = f"{CTL_DIR}/{name}.{pid_}.out"
    host_out = os.path.join(GUEST_LOG_DIR, f"{name}.{pid_}.out")
    with open(host_out, "a") as fh:
        fh.write(f"[{time.strftime('%F %T')}] start {name}#{pid_} {arg_s}\n")
    fetched = [0]                   # 已外带的字节数（闭包可变）

    def pull_out():
        """增量拉取 guest .out 追加到宿主机文件；返回当前完整内容。"""
        _, body, _ = run_raw(envd, f"cat {out_path} 2>/dev/null")
        if len(body) < fetched[0]:  # guest 侧被截断/重写 → 整体重落
            with open(host_out, "a") as fh:
                fh.write(f"\n[{time.strftime('%F %T')}] out truncated, refetch\n")
            fetched[0] = 0
        if len(body) > fetched[0]:
            with open(host_out, "ab") as fh:
                fh.write(body[fetched[0]:].encode())
            fetched[0] = len(body)
        return body

    for attempt in range(3):                      # 启动命令本身重试 3 次
        try:
            run(envd, f"setsid bash /root/soak/bin/run_phase.sh {name} {pid_} {arg_s}",
                timeout=60)
            break
        except Exception as e:
            if attempt == 2:
                raise
            print(f"  ⚠ phase {name}#{pid_} 启动重试 {attempt + 1}: {str(e)[:120]}")
            time.sleep(10)
    end = time.monotonic() + deadline
    warned = False
    while time.monotonic() < end:
        if _STOP["flag"]:
            raise KeyboardInterrupt
        time.sleep(POLL_INTERVAL)
        try:
            code, out, _ = run_raw(envd, f"cat {rc_path} 2>/dev/null")
            pull_out()                             # ← 增量外带，失败随下轮重试
        except Exception as e:
            if not warned:
                print(f"  ⚠ phase {name}#{pid_} 轮询 exec 受阻（继续等）: {str(e)[:120]}")
                warned = True
            continue
        if code == 0 and out.strip().lstrip("-").isdigit():
            rc = int(out.strip())
            body = pull_out()                      # 最终一次外带后返回
            if not body.strip():
                # envd exec 流在内存压力下会丢 stdout 载荷：rc=0 但 cat 到空，
                # guest 文件实际有完整内容（20261026 r11 实证）。间隔重拉再判空，
                # 既保住这轮负载结果，也把发生率记录在案。
                for _ in range(3):
                    time.sleep(3)
                    try:
                        body = pull_out()
                    except Exception:
                        pass
                    if body.strip():
                        print(f"  ⚠ phase {name}#{pid_} 输出重拉成功 "
                              f"({len(body)}B)——envd exec 流疑丢载荷（内存压力期）")
                        break
            with open(host_out, "a") as fh:
                fh.write(f"\n[{time.strftime('%F %T')}] done rc={rc}\n")
            return rc, body
    raise RuntimeError(f"phase {name}#{pid_} 未在 {deadline}s 内完成"
                       f"（宿主机侧最新输出见 {host_out}）")


def parse_hashes(txt):
    """ctl/snapshot.txt → {相对路径: sha256}。严格解析：任何非法行都是错误，
    不允许静默丢条目导致“不完整清单也算一致”。"""
    state = {}
    for line in txt.splitlines():
        if not line.strip():
            continue
        parts = line.split()
        if not (len(parts) == 2 and len(parts[0]) == 64
                and parts[1].startswith(("data/", "fio/"))):
            raise RuntimeError(f"snapshot.txt 非法行: {line[:200]!r}")
        state[parts[1]] = parts[0]
    return state


def parse_snapmeta(out):
    """snapshot.sh 输出 → (du_mb, mem_avail_kb)。"""
    m = re.search(r"snap-du data=(\d+)MB fio=(\d+)MB", out)
    du = int(m.group(1)) + int(m.group(2)) if m else None
    m = re.search(r"MemAvailable:\s+(\d+) kB", out)
    return du, (int(m.group(1)) if m else None)


def take_snapshot(envd):
    """跑 snapshot 阶段并读取快照 → (状态 dict, du_mb, mem_avail_kb)。
    除严格解析外，还要求锚点文件在列——空清单/缺核心文件不能当“一致”。"""
    rc, out = phase(envd, "snapshot", (), SNAP_DEADLINE)
    if rc != 0:
        raise RuntimeError(f"snapshot rc={rc}: {out[-400:]!r}")
    txt = run(envd, f"cat {CTL_DIR}/snapshot.txt", timeout=300)
    state = parse_hashes(txt)
    anchors = [f"{FIO_DIR}/fio.bin".replace("/root/soak/", ""),
               "data/mutable.bin", "data/stable.log"]
    missing = [a for a in anchors if a not in state]
    if not any(k.startswith("data/r") for k in state):
        missing.append("<至少一个 data/r*.bin>")
    if missing:
        raise RuntimeError(f"snapshot 锚点缺失 {missing}（清单 {len(state)} 条）")
    du, mem = parse_snapmeta(out)
    return state, du, mem


def diff_states(expected, actual):
    missing = sorted(set(expected) - set(actual))
    extra = sorted(set(actual) - set(expected))
    changed = sorted(k for k in set(expected) & set(actual)
                     if expected[k] != actual[k])
    return missing, extra, changed


def host_du(sid):
    """宿主机侧沙箱 checkpoint 目录体积（MB），路径缺失返回 None。"""
    total = 0
    found = False
    for pat in (f"/root/sandbox/nova-work/var/lib/sandboxes/{sid}",
                f"/run/sandbox-nova/{sid}",
                f"/tmp/e2b-bench-4443/run/sandboxes/{sid}"):
        for p in glob.glob(pat):
            du = subprocess.run(["du", "-sm", p], capture_output=True, text=True)
            if du.returncode == 0:
                total += int(du.stdout.split()[0])
                found = True
    return total if found else None


def ensure_shim():
    """垫片缺失则现场编译（需要宿主机 gcc）。"""
    if os.path.isfile(SHIM_SO):
        return
    src = os.path.join(PARENT, "guest-tools", "shmshim.c")
    if not os.path.isfile(src):
        sys.exit(f"✗ 缺 {SHIM_SO} 且无源码 {src} 可编译")
    os.makedirs(os.path.dirname(SHIM_SO), exist_ok=True)
    subprocess.run(["gcc", "-O2", "-shared", "-fPIC", "-o", SHIM_SO, src],
                   check=True)
    print(f"  • 已编译 {SHIM_SO}")


def setup_guest(envd):
    """上传 fio 工具链 + 写入全部 worker 脚本，并自检。"""
    ensure_shim()
    run(envd, "mkdir -p /root/soak/bin /root/soak/ctl "
              f"{DATA_DIR} {FIO_DIR} {BENCH_DIR} /usr/lib64/fio")
    uploads = [(SHIM_SO, "/root/soak/bin/shmshim.so"),
               (FIO_BIN, "/root/soak/bin/fio")]
    # fio 依赖的宿主机 so（按 NEEDED 常见集合；缺失的跳过）
    for lib in ("libtcmalloc.so.4", "libunwind.so.8", "libaio.so.1"):
        for p in glob.glob(f"{FIO_LIBDIR}/{lib}*"):
            if os.path.isfile(p) and not os.path.islink(p):
                uploads.append((p, f"/root/soak/bin/{os.path.basename(p)}"))
    for p in uploads:
        envd.files.write(p[1], open(p[0], "rb").read())
    n_plug = 0
    for p in glob.glob(f"{FIO_PLUGDIR}/*.so"):
        envd.files.write(f"/usr/lib64/fio/{os.path.basename(p)}",
                         open(p, "rb").read())
        n_plug += 1
    scripts = {
        "env.sh": GUEST_ENV_SH, "run_phase.sh": GUEST_RUN_PHASE_SH,
        "crud.sh": GUEST_CRUD_SH, "fio.sh": GUEST_FIO_SH,
        "burst.py": GUEST_BURST_PY, "memburst.sh": (
            "#!/bin/bash\nsource /root/soak/bin/env.sh\n"
            "exec python3 /root/soak/bin/burst.py\n"),
        "snapshot.sh": GUEST_SNAPSHOT_SH, "bench.sh": GUEST_BENCH_SH,
        "diag.sh": GUEST_DIAG_SH,
    }
    for name, body in scripts.items():
        envd.files.write(f"/root/soak/bin/{name}", body.encode())
    run(envd, "chmod +x /root/soak/bin/*.sh /root/soak/bin/fio && cd /root/soak/bin && "
              "ln -sf libtcmalloc.so.4.* libtcmalloc.so.4 2>/dev/null; "
              "ln -sf libunwind.so.8.* libunwind.so.8 2>/dev/null; "
              "ln -sf libaio.so.1.* libaio.so.1 2>/dev/null; true")
    print(f"  • 已上传 fio 工具链（{len(uploads)} 文件 + {n_plug} 插件 + 垫片）"
          f"与 {len(scripts)} 个 worker 脚本")
    # 预分配数据集（dd 大块随机写 + fsync）：fio/bench 之后全部原地复写。
    # 不预分配的话 randwrite direct 会写进稀疏文件的未写盘区，触发 extent
    # 转换风暴把 guest IO 打满（首跑已实测：envd exec 跟着一起超时）。
    t = time.time()
    run(envd, "set -e; "
              f"dd if=/dev/urandom of={FIO_DIR}/fio.bin bs=8M "
              f"count={FIO_SIZE_MB // 8} conv=fsync status=none; "
              f"dd if=/dev/urandom of={BENCH_DIR}/a.bin bs=8M "
              f"count={BENCH_A_MB // 8} conv=fsync status=none; "
              f"dd if=/dev/urandom of={BENCH_DIR}/b.bin bs=8M "
              f"count={BENCH_B_MB // 8} conv=fsync status=none; "
              f"dd if=/dev/urandom of={BENCH_DIR}/c.bin bs=8M "
              f"count={BENCH_C_MB // 8} conv=fsync status=none; sync; "
              f"du -sm {FIO_DIR}/fio.bin {BENCH_DIR}/*.bin",
        timeout=max(600, (FIO_SIZE_MB + BENCH_TOTAL_MB) // 2))
    print(f"  • 已预分配数据集 fio {FIO_SIZE_MB}MiB + bench "
          f"{BENCH_TOTAL_MB}MiB（{time.time() - t:.0f}s）")
    # 自检：fio 版本、三引擎可用性、基线资源
    out = run(envd, "source /root/soak/bin/env.sh && fio --version && "
              "for e in psync libaio io_uring; do "
              "fio --name=chk --size=16m --rw=randwrite --bs=4k --ioengine=$e "
              "--direct=1 --filename=/root/soak/fio/chk.bin --time_based "
              "--runtime=2 --group_reporting --minimal >/dev/null 2>&1 "
              "&& echo engine-$e-ok || echo engine-$e-FAIL; done; rm -f /root/soak/fio/chk.bin; "
              "nproc; grep MemTotal /proc/meminfo; df -h / | tail -1; "
              "df -h /root/soak | tail -1; "
              "lsblk -o NAME,SIZE,TYPE,RO 2>/dev/null | head -8")
    print("  • 自检:\n    " + "\n    ".join(out.strip().splitlines()))
    if "FAIL" in out:
        raise RuntimeError(f"fio 引擎自检失败: {out}")


def persist_round_forensics(envd, i):
    """每轮把内核/负载明细外带宿主机（diff 盘不可信，guest 内文件只算临时）：
    dmesg 增量、fio 完整 JSON、sha256 清单副本。受阻不致命——envd 可能已不可达，
    此时 phase() 的增量外带才是主通道。"""
    try:
        _, d, _ = run_raw(envd, "dmesg | tail -n 80", timeout=30)
        with open(os.path.join(GUEST_LOG_DIR, "dmesg.log"), "a") as fh:
            fh.write(f"\n===== round {i} {time.strftime('%F %T')} =====\n{d}\n")
        for src, dst in ((f"{FIO_DIR}/last.json", f"fio-last.r{i}.json"),
                         (f"{CTL_DIR}/snapshot.txt", f"snapshot.r{i}.txt")):
            _, body, _ = run_raw(envd, f"cat {src}", timeout=30)
            if body.strip():
                with open(os.path.join(GUEST_LOG_DIR, dst), "w") as fh:
                    fh.write(body)
    except Exception as e:
        print(f"  ⚠ round {i} 轮末取证外带受阻: {str(e)[:120]}")


def grab_diag(envd, sid, reason):
    """失败取证：guest 诊断 + API 状态，尽量写进结果目录（envd 不可用则跳过）。"""
    pack = {"reason": reason[:500], "time": time.strftime("%F %T")}
    if envd is not None:
        try:
            code, out, err = run_raw(envd, "bash /root/soak/bin/diag.sh 2>&1",
                                     timeout=90)
            pack["guest_diag"] = out[:20000]
        except Exception as e:
            pack["guest_diag_error"] = f"{type(e).__name__}: {str(e)[:300]}"
    if sid:
        try:
            r = api("GET", f"/sandboxes/{sid}", timeout=30)
            pack["api_status"] = {"http": r.status_code, "body": r.text[:2000]}
        except Exception as e:
            pack["api_status_error"] = str(e)[:300]
    return pack


def fmt_s(ms):
    return f"{ms / 1000:.1f}s" if ms is not None and ms < 60000 else (
        f"{ms / 60000:.1f}m" if ms is not None else "-")


def main():
    os.makedirs(RESULTS_DIR, exist_ok=True)
    os.makedirs(GUEST_LOG_DIR, exist_ok=True)
    rounds_path = os.path.join(RESULTS_DIR, "rounds.jsonl")
    tid = template_id(MANIFEST_KEY)
    print(f"• soak agent-load 长稳用例  manifest://{MANIFEST_KEY[:12]}…")
    print(f"  templateID={tid}")
    print(f"  env: {e2b_env.api_url('')!r}")
    print(f"  负载: crud {CRUD_NEW_MB}MiB/轮(keep={KEEP}) + {CRUD_REWRITE_MB}MiB 重写 | "
          f"fio {FIO_SIZE_MB}MiB×{FIO_RUNTIME_S}s 4k direct 引擎轮转{ENGINES} | "
          f"mem {MEM_BURSTS}×突发→{MEM_TARGET_PCT:g}% hold={MEM_HOLD_S:g}s | "
          f"磁盘峰值预算 {DISK_PEAK_MB}MB")
    print(f"  rounds={'∞' if not ROUNDS else ROUNDS}  pause→reuse 每轮 1 次")
    print(f"  results: {rounds_path}")

    signal.signal(signal.SIGINT, lambda *_: _STOP.update(flag=True))
    signal.signal(signal.SIGTERM, lambda *_: _STOP.update(flag=True))

    domain = os.environ["E2B_DOMAIN"]
    sid = None
    envd = None
    hosts_added = False
    stats = []
    failure = None
    baselines = {}          # round 1（1 次 pause 后）各 fio 类型中位耗时
    pause_hist = []         # pause/resume 接口耗时对比：首轮为基线，逐轮记录增长倍数
    resume_hist = []
    smoke_hist = []         # resume 后第一击 exec（echo 写轮次到文件）耗时对比

    def emit(rec):
        """逐轮落盘（成功/失败/中断/初始化失败都写）。"""
        stats.append(rec)
        with open(rounds_path, "a") as fh:
            fh.write(json.dumps(rec) + "\n")

    try:
        t0 = time.time()
        # 初始化（create → SDK connect → setup）同样纳入失败记录与保留策略
        try:
            r = api("POST", "/sandboxes",
                    json={"templateID": tid, "timeout": SANDBOX_TIMEOUT},
                    timeout=120)
            if r.status_code != 201:
                raise RuntimeError(f"create HTTP {r.status_code}: {r.text[:400]}")
            sid = r.json()["sandboxID"]
            envd, hosts_added = connect_sdk(sid)
            print(f"  ✓ created sid={sid} ({time.time() - t0:.1f}s)")
            setup_guest(envd)
        except Exception as e:
            rec0 = {"round": 0, "ts": time.strftime("%F %T"), "ok": False,
                    "error": str(e)[:500]}
            print(f"  ✗ 初始化失败: {str(e)[:300]}")
            rec0["diag"] = grab_diag(envd, sid, str(e))
            failure = rec0
            emit(rec0)
            _STOP["flag"] = True          # 跳过轮循环，走 finally 汇总/保留

        i = 0
        while not _STOP["flag"]:
            i += 1
            if ROUNDS and i > ROUNDS:
                break
            rec = {"round": i, "ts": time.strftime("%F %T"), "ok": False}
            t_round = time.time()
            try:
                # 1) CRUD
                t = time.time()
                rc, out = phase(envd, "crud", (i,), CRUD_DEADLINE)
                if rc != 0:
                    raise RuntimeError(f"crud rc={rc}: {out[-400:]!r}")
                m = re.search(r"crud-ok round=\d+ keep=\d+ files=(\d+)", out)
                if m:
                    rec["crud_files"] = int(m.group(1))
                rec["crud_s"] = round(time.time() - t, 1)

                # 2) fio（引擎 × rw 轮转）
                t = time.time()
                eng, rw = ENGINES[(i - 1) % len(ENGINES)], RWS[((i - 1) // len(ENGINES)) % len(RWS)]
                rc, out = phase(envd, "fio", (eng, rw), FIO_DEADLINE)
                if rc != 0 or "fio-ok" not in out:
                    raise RuntimeError(f"fio rc={rc}: {out[-400:]!r}")
                fio_m = re.search(
                    r"fio-ok riops=([\d.]+) wiops=([\d.]+) rbw=([\d.]+)MiBps "
                    r"wbw=([\d.]+)MiBps rlat_us=([\d.]+) wlat_us=([\d.]+)", out)
                rec["fio"] = {"engine": eng, "rw": rw,
                              "riops": float(fio_m.group(1)),
                              "wiops": float(fio_m.group(2)),
                              "rbw_mibps": float(fio_m.group(3)),
                              "wbw_mibps": float(fio_m.group(4)),
                              "rlat_us": float(fio_m.group(5)),
                              "wlat_us": float(fio_m.group(6))}
                rec["fio_s"] = round(time.time() - t, 1)

                # 3) 内存突发 ×N —— 固定 8G×90%（写死，不探测：guest 有活动
                #    balloon 读数不可信，气球按需回吐）；OOM(rc=137) 判失败
                t = time.time()
                bursts = []
                for b in range(1, MEM_BURSTS + 1):
                    if _STOP["flag"]:
                        raise KeyboardInterrupt
                    rc, out = phase(envd, "memburst", (), BURST_DEADLINE)
                    if rc != 0 or "burst-ok" not in out:
                        raise RuntimeError(
                            f"memburst#{b} rc={rc}: {out[-400:]!r}"
                            + ("（rc=137 → guest OOM）" if rc == 137 else ""))
                    bm = re.search(r"burst-ok alloc_mib=(\d+) ramp_s=([\d.]+) "
                                   r"hwm_kib=(\d+)", out)
                    if not bm:
                        raise RuntimeError(f"burst 输出异常: {out[-300:]!r}")
                    bursts.append({"alloc_mib": int(bm.group(1)),
                                   "ramp_s": float(bm.group(2)),
                                   "hwm_kib": int(bm.group(3))})
                    if b < MEM_BURSTS:
                        time.sleep(MEM_GAP_S)
                rec["bursts"] = bursts
                rec["mem_s"] = round(time.time() - t, 1)

                # 4) 期望快照
                expected, du_mb, mem_before = take_snapshot(envd)
                rec.update({"files": len(expected), "du_mb": du_mb,
                            "mem_avail_kb": mem_before})

                # 5) pause → reuse（e2b SDK：envd.pause + EnvdSandbox.connect）
                rec["pause_ms"] = round(pause_sandbox(envd, sid), 1)
                pause_hist.append(rec["pause_ms"])
                if len(pause_hist) > 1:       # pause/resume 接口耗时对比（vs 首轮基线）
                    rec["pause_base_ms"] = pause_hist[0]
                    rec["pause_factor"] = round(rec["pause_ms"] / pause_hist[0], 2)
                envd, hosts_added, resume_ms = resume_sandbox(sid, hosts_added)
                rec["resume_ms"] = round(resume_ms, 1)
                resume_hist.append(rec["resume_ms"])
                if len(resume_hist) > 1:
                    rec["resume_base_ms"] = resume_hist[0]
                    rec["resume_factor"] = round(rec["resume_ms"] / resume_hist[0], 2)
                # resume 后第一击 envd exec：echo 写轮次到文件并返回
                # （覆盖写路径：page-in + diff盘首写，比读命令更贴近冷恢复体验）
                t = time.time()
                run(envd, f"echo round-{i} > /root/soak/ctl/last-resume-round")
                rec["smoke_ms"] = round((time.time() - t) * 1000, 1)
                smoke_hist.append(rec["smoke_ms"])
                if len(smoke_hist) > 1:
                    rec["smoke_base_ms"] = smoke_hist[0]
                    rec["smoke_factor"] = round(rec["smoke_ms"] / smoke_hist[0], 2)

                # 6) 复验一致性
                actual, du2, mem_after = take_snapshot(envd)
                missing, extra, changed = diff_states(expected, actual)
                rec.update({"verify_files": len(actual), "verify_du_mb": du2,
                            "mem_avail_after_kb": mem_after,
                            "missing": missing, "extra": extra,
                            "changed": changed})
                if missing or extra or changed:
                    raise RuntimeError(
                        f"resume 后状态不一致: missing={missing} "
                        f"extra={extra} changed={changed}")

                # 7) 回归基准：agent IO 画像 ×3 类 × BENCH_REPS（固定 IO 量，
                #    与 round 1 基线比较 = pause 次数累积的耗时回归检测）
                t = time.time()
                rc, out = phase(envd, "bench", (), BENCH_DEADLINE)
                if rc != 0 or "bench-done" not in out:
                    raise RuntimeError(f"bench rc={rc}: {out[-400:]!r}")
                reps = {k: [] for k in "ABC"}
                for m in re.finditer(r"bench-ok type=([ABC]) rep=(\d+) "
                                     r"wall_ms=(\d+) iops=([\d.]+) bw=([\d.]+)",
                                     out):
                    reps[m.group(1)].append(
                        {"rep": int(m.group(2)), "wall_ms": int(m.group(3)),
                         "iops": float(m.group(4)), "bw_mibps": float(m.group(5))})
                if not all(len(reps[k]) == BENCH_REPS for k in "ABC"):
                    raise RuntimeError(
                        f"bench 输出不完整 reps={[len(reps[k]) for k in 'ABC']}: "
                        f"{out[-300:]!r}")
                med = {k: sorted(x["wall_ms"] for x in reps[k])[len(reps[k]) // 2]
                       for k in "ABC"}
                rec["bench"] = {"reps": reps, "median_ms": med}
                if not baselines:
                    baselines.update(med)
                    rec["bench"]["is_baseline"] = True
                else:
                    rec["bench"]["baseline_ms"] = dict(baselines)
                    for k in "ABC":
                        if med[k] > baselines[k] * REGRESSION_FACTOR:
                            raise RuntimeError(
                                f"bench 回归: {k} 类中位耗时 {med[k]}ms 超基线 "
                                f"{baselines[k]}ms×{REGRESSION_FACTOR:g} "
                                f"（已 pause {i} 次）")
                rec["bench_s"] = round(time.time() - t, 1)

                rec["host_du_mb"] = host_du(sid)
                ck = glob.glob(f"/root/sandbox/nova-work/var/lib/sandboxes/"
                               f"{sid}/checkpoint/*snapshot")   # 内存快照实际落盘
                if ck:
                    rec["ckpt_snapshot_mb"] = round(
                        os.path.getsize(ck[0]) / 1024**2, 1)
                persist_round_forensics(envd, i)
                rec["ok"] = True
                rec["round_s"] = round(time.time() - t_round, 1)
                f = rec.get("fio", {})
                ckmb = (f"{rec['ckpt_snapshot_mb']:.0f}M"
                        if "ckpt_snapshot_mb" in rec else "-")
                print(f"  round {i:3d} ✓ crud={rec['crud_s']}s "
                      f"fio[{eng}/{rw}] {rec['fio_s']}s "
                      f"(r{f.get('riops', 0):.0f}iops w{f.get('wiops', 0):.0f}iops) | "
                      f"burst×{MEM_BURSTS} {rec['mem_s']}s "
                      f"alloc={bursts[0]['alloc_mib']}M | "
                      f"data {du_mb}MB/{len(expected)}f | "
                      f"pause={fmt_s(rec['pause_ms'])} "
                      f"×{rec.get('pause_factor', 1.0):.2f} "
                      f"resume={fmt_s(rec['resume_ms'])} "
                      f"×{rec.get('resume_factor', 1.0):.2f} "
                      f"exec首击={rec['smoke_ms']:.0f}ms "
                      f"×{rec.get('smoke_factor', 1.0):.2f} | "
                      f"bench A={med['A'] / 1000:.1f}s B={med['B'] / 1000:.1f}s "
                      f"C={med['C'] / 1000:.1f}s | "
                      f"mem {mem_before and mem_before // 1024}M→"
                      f"{mem_after and mem_after // 1024}M | "
                      f"ckpt {ckmb} | "
                      f"total {rec['round_s']}s")
            except KeyboardInterrupt:
                rec["error"] = "interrupted"
                rec["round_s"] = round(time.time() - t_round, 1)
                emit(rec)
                _STOP["flag"] = True
                break
            except Exception as e:
                rec["error"] = str(e)[:500]
                rec["round_s"] = round(time.time() - t_round, 1)
                print(f"  round {i:3d} ✗ {str(e)[:300]}")
                rec["diag"] = grab_diag(envd, sid, str(e))
                failure = rec
                emit(rec)
                break
            emit(rec)

        if _STOP["flag"] and not failure:
            print("\n⏹ 收到停止信号，结束本轮后退出")
    finally:
        passed = sum(1 for x in stats if x.get("ok"))
        result = {
            "sid": sid, "rounds_done": len(stats), "passed": passed,
            "failed": 0 if not failure else 1,
            "first_error": failure and failure.get("error"),
            "uptime_s": round(time.time() - t0, 1) if sid else None,
            "manifest_key": MANIFEST_KEY, "template_id": tid,
            "config": {"DISK_PEAK_MB": DISK_PEAK_MB, "FIO_SIZE_MB": FIO_SIZE_MB,
                       "CRUD_NEW_MB": CRUD_NEW_MB, "KEEP": KEEP,
                       "MEM_TARGET_PCT": MEM_TARGET_PCT, "MEM_BURSTS": MEM_BURSTS,
                       "FIO_RUNTIME_S": FIO_RUNTIME_S,
                       "BENCH_A_MB": BENCH_A_MB, "BENCH_B_MB": BENCH_B_MB,
                       "BENCH_C_MB": BENCH_C_MB, "BENCH_REPS": BENCH_REPS,
                       "REGRESSION_FACTOR": REGRESSION_FACTOR},
        }
        pauses = [x["pause_ms"] for x in stats if x.get("pause_ms")]
        resumes = [x["resume_ms"] for x in stats if x.get("resume_ms")]
        result["pause_ms"] = pstats(pauses)
        result["resume_ms"] = pstats(resumes)
        if baselines:
            result["bench_baseline_ms"] = dict(baselines)
        bench_stats = {}
        for k, label in (("A", "randread4k"), ("B", "randrw4k-70/30"),
                         ("C", "seqread1m")):
            vals = [v["wall_ms"] for x in stats if x.get("bench")
                    for v in x["bench"]["reps"][k]]
            if vals:
                bench_stats[k] = pstats(vals)
                result[f"bench_{k}_{label}_ms"] = bench_stats[k]
        mems = [x["mem_avail_kb"] for x in stats if x.get("mem_avail_kb")]
        if len(mems) >= 2:
            result["mem_drift_mb"] = round((mems[0] - mems[-1]) / 1024, 1)
        with open(os.path.join(RESULTS_DIR, "result.json"), "w") as fh:
            json.dump(result, fh, ensure_ascii=False, indent=2)

        print("\n=== summary ===")
        print(f"rounds {len(stats)}  pass {passed}  fail {result['failed']}"
              + (f"  err: {failure['error']}" if failure else ""))

        def show_stats(name, st):
            if st:
                print(f"{name:6s}: min/p50/avg/p90/max = "
                      f"{fmt_s(st['min'])}/{fmt_s(st['p50'])}/{fmt_s(st['avg'])}/"
                      f"{fmt_s(st['p90'])}/{fmt_s(st['max'])}  (n={st['n']})")

        show_stats("pause", result["pause_ms"])
        show_stats("resume", result["resume_ms"])
        for k in "ABC":
            if k in bench_stats:
                show_stats(f"b-{k}", bench_stats[k])
        if baselines:
            print("bench 基线(round1 中位): "
                  + " ".join(f"{k}={baselines[k]}ms" for k in "ABC")
                  + f"  回归阈值 ×{REGRESSION_FACTOR:g}")
        if "mem_drift_mb" in result:
            print(f"mem   : MemAvailable 首末差 {result['mem_drift_mb']}MB"
                  f"（正=内存被吃掉未还，盯泄漏）")
        print(f"artifacts: {RESULTS_DIR}")
        if sid:
            if hosts_added:
                try:
                    del_hosts(sid, domain)
                except Exception as e:
                    print(f"⚠ cleanup hosts: {e}", file=sys.stderr)
            keep = failure and KEEP_ON_FAIL or (_STOP["flag"] and KEEP_ON_INT)
            if keep:
                print(f"⏸ 沙箱已保留供取证 sid={sid}")
                print(f"   诊断: GET {e2b_env.api_url(f'/sandboxes/{sid}')}")
                print(f"   进入: sandbox-ctl exec --sandbox-id {sid} --run-root "
                      f"/run/sandbox-nova/sandboxes --tty -- /bin/bash")
            else:
                try:
                    api("DELETE", f"/sandboxes/{sid}", timeout=60)
                    print(f"✓ 已删除沙箱 {sid}")
                except Exception as e:
                    print(f"⚠ cleanup sandbox: {e}（手动 DELETE /sandboxes/{sid}）",
                          file=sys.stderr)
    sys.exit(1 if failure else 0)


if __name__ == "__main__":
    main()
