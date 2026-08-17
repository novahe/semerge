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

- 复现：`python3 bench.py --w "$(cat openclaw-e2b-ws-balloon-template.id)" --probe health -n 1`
- 诊断脚本：`python3 debug_cold7s.py <template.id>`（envd 采样 PSI/meminfo/gateway 态 + gw.log tail）。
  restore 后 gw.log 的 `[diagnostic] liveness warning` 行直接给出 eventLoopDelayMaxMs 与 recentPhases；
  注意 gw.log 大段日志空洞 = guest 被暂停导出的窗口（guest 时钟冻结），不是真实耗时。
