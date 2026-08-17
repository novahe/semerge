# 录制工作集期间文件页缓存被气球回收，导致工作集恢复后仍去读盘、冷启动很慢

## 现象

带气球的 W（working-set, drop_caches=false）模板 restore 后，首个 /health 单次请求耗时 7-11s
（基线：非气球 W 同路径 252ms / p50 99.7ms）；warm 探针也可能中招（实测 2522ms）。
launch 本身仅 ~110ms，慢的全部发生在"launch 完成后 → gateway 首次应答"之间。

| 模板 | cold /health | warm | restore blk0(erofs) 读 |
|---|---|---|---|
| W + 气球（坏） | 7032–8371ms | 5.9ms–2523ms | 2100–3715 次 / 39–69MB |
| B + 气球（同期） | 98–810ms（抽签） | ~6ms | 0 次 |
| W 无气球 | 252ms | ~7ms | 0 次 |
| W + 气球（修复后） | **84.3ms** | 6.0ms | **0 次** |

## 根因

**判据（一句话）：pause#2 冻结瞬间，恢复后要用的文件页必须还驻留在 guest 内存里
（没被气球收走），从而被写进快照。违反它，restore 就要为每个文件页付一次远程盘读。**

机制链：

1. 节点策略 `allocatable 256MiB < capacity 8GiB` → 每个沙箱自带 ~7.75GiB 气球目标
   （conductor.yaml 注释原话："build_bw_coldboot*.py 不显式覆盖 allocatable 时出的快照都是气球快照"）。
2. 气球只能收 free 页 + **干净文件页缓存**，收不动 anon。guest 被过度订阅
   （anon ~1.2GB ≈ 预算），文件缓存被持续压到 ≈0（实测冻结时 MemAvailable 4MB / Cached 13MB）。
3. W 快照（drop_caches=false）只能保存**驻留页** → 冻结晚于气球收敛的 W 只带走了 anon 堆，
   **文件页不在快照里**（B 冻得早、气球没收完，所以文件页在 B 里——B 快只是抽签抽得好）。
4. restore 时冻结期间错过的 timer 全部到期，gateway 的生命周期阶段必然重跑
   （update-check / sidecars 哨兵 / maintenance / agent-runtime 插件预加载，
   gw.log `[diagnostic] recentPhases=...`，实测 eventLoopDelayMaxMs=7252）。
5. 补跑要读 ~40–70MB 文件 → 全部 miss → 远程 erofs（manifest:// OBS，p50 3.1ms/次），
   node 单线程事件循环串行 → **读数 × 3.1ms ≈ 总停滞**：
   2100→7.0s / 2212→7.35s / 3533→8.37+2.52s，严格线性。
   首 health 只是挤进重放间隙，剩余尾部由 warm 请求买单。
6. guest 全局受累（envd 一条 `ps` 也要 7.8s；PSI_io some avg10 ~40-70%；gateway D 态
   `folio_wait_bit_common`）——因为 ps/cat 等二进制同样不在缓存。

## 已证伪的归因（避免再走弯路）

| 假设 | 证伪实验 | 结果 |
|---|---|---|
| restore 后 settled 1.26GiB→256MiB 挤压导致 | `--allocatable-memory 1GiB` 覆盖（无挤压、无 PSI 事件） | cold 仍 7352ms；B/非气球 W 同样被 settle 却不卡 |
| 冻结时 gateway 忙（pre-warm 进行中） | READY_SETTLE=10 静置后再冻（gw.log 确认完全空闲） | cold 仍 7.2s——生命周期阶段每次 restore 都重跑，与冻结时状态无关 |
| 重新制作快照即可 | 三次重建 | W 恒慢（3533 读）；B 98/171/810ms 抽签 |

补充：`startup 1GiB` 预算在冷启动与 restore 都生效，但只是
`max(startup, floor, allocatable_at_snapshot)` 三元组之一（nodectl/admission.go:499）；
1GiB < anon(1.2GB)，救不了文件缓存。**起步预算与"快照里有没有文件页"是两回事。**

## 修复（已验证 2026-08-17）

构建时显式放宽 allocatable，让气球全程瘪着、文件页全程驻留：

```bash
E2B_ENV_FILE=/root/sandbox/nova-work/etc/node-ctl-nova/tenant.env \
ALLOCATABLE_MEMORY=7.5GiB \
python3 build_bw_coldboot.py
```

- 必须 < capacity（8GiB）：`=capacity` 时 CH 不加气球设备，恢复后无法挤压、高密失效。
  7.5GiB → 气球仅 512MiB；值不必精确，原则是"大到气球吃不动文件缓存"。
- 只影响**冻进快照的起步状态**；稳态不变：restore 后 ~5s（resctl 心跳周期，
  controller.go:714）controller 照样 settle 到 256MiB 地板，PSI +64MiB 步进协商
  （长驻实测：t+6s 气球 7808MiB，平衡在气球 ~7.6-7.7GiB ≈ 可用 500-600MiB）。
- 效果：W cold 8371→84.3ms，B 稳定 97.1ms（抽签消失），restore 零 erofs 读，W 反超 B。
- 注意：构建 settle/quiesce 别设 ~10s——openclaw 的 npmjs update-check 失败后会在
  ready+~10.0s 自动全量插件重载，10s 恰好把 pause#1 冻进重载（B 因此从 98ms 变 810ms）。

## 防回归建议

1. build 脚本 pause#2 前自检：经 envd 读 `grep -E '^(Cached|MemAvailable)' /proc/meminfo`，
   Cached 仅十几 MB → 报警/中止（好快照为几百 MB 级）。
2. warm 探针后 `find /app/dist -type f -exec cat {} + > /dev/null` 强制文件页驻留再冻结（双保险）。
3. 把 `ALLOCATABLE_MEMORY` 默认值设为 7.5GiB（产无气球对照时显式传 8GiB）。

## 上游结构化修复（可选，消除对构建纪律的依赖）

- sandboxer 侧：restore 时不信任快照气球状态，统一 clamp 到 startup budget 起步
  （`restore.go` / `balloonstate.go`），地板由 controller 延迟收紧——任何模板都不可能再冻出坏快照。
- 平台侧：rootfs 本地缓存。热集仅 ~50-70MB（1.2GiB 镜像的 ~5%），单读 3.1ms→本地后，
  稳态挤压下的冷文件读也快（当前稳态挤压仍会清空缓存，此问题独立存在）。

## 复现 / 诊断

### 复现步骤（不依赖 ws-bench 脚本，两条 curl 即可）

前置：从 tenant.env 取四个值（API 地址、key、CA、数据面地址）：

```bash
export E2B_API_URL=https://api.sandboxes.nova.local:443
export E2B_SANDBOX_URL=https://sandboxes.nova.local:8443
export E2B_API_KEY=<tenant.env 里的 key>
export SSL_CERT_FILE=<tenant.env 里的 CA 路径>
```

① 用坏模板创建沙箱（坏模板 = 气球挤压态下制作的 W 快照；修复后模板作对照）：

```bash
TPL=$(cat openclaw-e2b-ws-balloon-template.id)     # 坏；对照组用 *-q2-template.id
RESP=$(curl -s --cacert $SSL_CERT_FILE -X POST "$E2B_API_URL/sandboxes" \
  -H "Authorization: Bearer $E2B_API_KEY" -H "Content-Type: application/json" \
  -d "{\"templateID\":\"$TPL\",\"timeout\":120}")
SID=$(echo "$RESP" | python3 -c 'import json,sys;print(json.load(sys.stdin)["sandboxID"])')
TOKEN=$(echo "$RESP" | python3 -c 'import json,sys;print(json.load(sys.stdin)["forwardAccessToken"])')
```

② 计时发 health——create 一返回就发（这是唯一关键测量），紧接着再发一次作 warm：

```bash
curl --noproxy '*' -s -o /dev/null --cacert $SSL_CERT_FILE \
  -w 'cold: HTTP%{http_code} %{time_total}s\n' \
  -H "E2b-Sandbox-Id: $SID" -H "E2b-Sandbox-Port: 18789" -H "X-Access-Token: $TOKEN" \
  "$E2B_SANDBOX_URL/health"
# 同一条命令再执行一次 → warm
```

判定：坏模板 cold ≈ 7-11s（warm 6ms~2.5s）；修复后模板 cold ≈ 0.1s。
对照组：换无气球模板或修复后模板重复 ①②。

③ 清理：

```bash
curl -s --cacert $SSL_CERT_FILE -X DELETE "$E2B_API_URL/sandboxes/$SID" \
  -H "Authorization: Bearer $E2B_API_KEY"
```

④（可选佐证，需节点 host 权限）沙箱退出后看 runner.log 尾部：

```
[vhost-stats] backend=blk0 ...  read reqs=3533 bytes=68.6MiB   ← 坏（数千次远程读）
[vhost-stats] backend=blk0 ...  read reqs=0                    ← 好
```

说明：复现只需要「一个坏模板 + 两条 curl」，不需要 bench/debug 脚本。坏模板的本质是
"create 时带 256MiB 气球地板（或节点默认）运行数分钟后，以
`X-Kuasar-Sandbox-Checkpoint: {"drop_caches":false,"merge_ref":false}` pause 导出的 W 快照"，
任何途径产出的这种快照都会复现；用 `ws-bench/build_bw_coldboot.py` 制作最方便
（脚本自包含，只依赖 python3 + curl + e2b SDK，可整体拷走）。

### bench / 诊断脚本（在 ws-bench 仓库时）

- 一键复现：`python3 bench.py --w "$(cat openclaw-e2b-ws-balloon-template.id)" --probe health -n 1`
- 深入诊断：`python3 debug_cold7s.py <template.id>`（envd 采样 PSI/meminfo/gateway 态 + gw.log tail）。
  restore 后 gw.log 的 `[diagnostic] liveness warning` 行直接给出 eventLoopDelayMaxMs 与 recentPhases；
  注意 gw.log 大段日志空洞 = guest 被暂停导出的窗口（guest 时钟冻结），不是真实耗时。
