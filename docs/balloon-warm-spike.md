# W 沙箱 warm 探针秒级毛刺——问题报告

> 2026-08-15 · nova 环境 · b256enc 模板（256MiB allocatable + 1GiB startup + 气球 + 加密快照）
> 结论先行：**restore 后气球再收敛挤进 guest 活跃页触发 direct reclaim，且放气速度（256MiB/5s）远慢于挤压速度，guest 卡秒级，envd 的 warm /health 是受害者**。envd 注册本身无辜。

## 1. 现象

- **症状**：W（工作集）快照恢复的沙箱，第二次 `/health`（warm 探针）偶发秒级停顿。`-r 5 -c 30`（150 样本）实测：warm p50 17ms、**max 7006-7717ms**，~0.7% 样本命中
- **伴随特征**：
  - cold 探针完全正常（p50 228ms、max 279ms，极差仅 ~100ms）
  - B（drop-caches 冷快照）warm 干净（max 24ms）
  - launch 各阶段全部正常，含 `runtime_ready_to_envd_init_done`（envd 注册 73-170ms）
  - 毛刺样本与 guest 内核日志 `virtio_balloon: pressure at ... (deflate ~256 MiB), converging` **1:1 对应**（150 样本中各出现 1 次）

毛刺样本时间线（复现轮实测）：

```
20:05:24.22  accepted（restore 开始）
20:05:24.55  launch 完成（envd init 73ms，一切正常）
20:05:25.0   cold /health → 224ms ✓
20:05:25.47  ⚡ virtio_balloon: pressure at 6.79GiB -> cap 6.54GiB
             ── guest 卡进 direct reclaim ~7.7s ──
20:05:33.x   warm /health 才返回（7717ms）
```

## 2. 根因链

**restore 后的气球再收敛把 guest 挤进内核 direct reclaim，且解压速度远慢于挤压速度。**

1. **起步是安全的**：restore 继承快照记录的 allocatable（源沙箱 pause 时刻值，如 1.2GiB），气球设备连 size 带预算一起从 state.json 恢复；settle 后控制器更把预算放开到 capacity=8GiB（气球=0，`settled-restore: applied correction … → 8589934592`）

2. **再收敛开始**：控制器按 guest 周期上报的 `MemAvailable`（每 5s）调整气球目标：
   `balloon = capacity − (工作集 + TargetFreeBuffer)`
   其中 **`TargetFreeBuffer = max(64MiB, capacity/32) = 256MiB`**（硬编码公式，无配置穿透）

3. **误判**：`MemAvailable` 把 page cache 大部分算作"可用"。W 沙箱 restore/uffd 页入灌入大量 cache 撑高"可用"→ 控制器据此多挤；而 cache 一被再次引用立即变"必需"，256MiB 目标缓冲**一触即溃**

4. **一步穿透**：充气步长 `MaxStep=256MiB` 恰好等于缓冲本身——一步就能越过缓冲扎进 envd/gateway 的活跃页

5. **卡死**：guest 内核进 direct reclaim。匿名页无 swap，只能等气球驱动 `deflate_on_oom` 放气

6. **解压挤牙膏**：原版放气同样受 256MiB/5s 步长限制——欠 512MiB 要等 2 轮 ≥10s（实测卡 7.7s）。期间任何 guest 进程碰到被 reclaim 的页即停摆 → warm 毛刺

**三因素叠加**：目标缓冲太薄（capacity/32）+ 充气步长等于缓冲（一步可穿透）+ 放气挤牙膏（256MiB/5s）。

**为什么只有 W 中招**：B 的快照 pause 时 `drop_caches=true`，guest 起步干净、挤压余量充足；W 带工作集 + page cache 恢复，实际占用贴近挤压边界，时机凑巧即过界（故仅 ~0.7% 样本）。

## 3. 证据清单

| 证据 | 数据 |
|---|---|
| 毛刺-压力 1:1 | 150 样本各 1 次，时间戳精确对齐（压力事件 ⊂ warm 7.7s 窗口内）|
| guest 内核日志 | `pressure at 1781462 pages (6.79GiB) -> cap 1715926 pages (6.54GiB), deflate ~256 MiB, converging` |
| 阶段排除 | 毛刺样本 dispatch 17ms / pre_ch 22ms / envd_init 73ms 全正常，唯独 warm 卡 |
| 排除项 | envd 注册、proxy 并发（workers 4→16 无变化）、CPU（burst 期 load ~9/88）、NFS、DB fsync（已 OFF）、systemd job 队列（慢窗口实测空）|
| 参数出处 | `sandboxer/pkg/resctl/balloon.go defaults()`：Interval 5s / TargetFreeBuffer max(64MiB, cap/32) / MaxStep 256MiB / Slack 32MiB，均硬编码、无 yaml 穿透 |

排查路径备忘：毛刺最初被怀疑为 envd/物化写盘/dispatch 队列等多个方向，逐一被分相数据（launchTiming 的 Points/PhasesMs）+ journalctl guest 日志 + vmstat 写盘脉冲排除/定位；分相埋点（orchestrator #0d0465c + sandboxer #63afa92）是关键工具。

## 4. 修复

**已打**（`sandboxer/pkg/resctl/balloon.go` Hint()，未 commit）——**压力侧全额放气**：
`memAvailable` 低于目标且差额超过 MaxStep 时，不再按步长放，一次放足全额并立即 kick reconcile（SetTarget 自带 kick，不等 5s 周期）。放气方向 notifier burst 以差额为界；充气方向保留步长限制。新增 `pressure relief` 日志便于验证命中。

**候选（未打，一行级）**：
- 缓冲加厚：`capacity/16`（512MiB），或按 `anon + N×cache` 动态取
- 步长约束：充气步长 ≤ `TargetFreeBuffer/2`，一步永远扎不穿缓冲
- 代价：内存超卖率略降（挤压收敛更保守）

**配置侧规避**（不动代码）：节点 allocatable 512MiB 时该现象此前从未出现——256MiB 是敏感区。

## 5. 验证状态

| 轮次 | 结果 |
|---|---|
| 修复前 `-r5 -c30` B+W | W warm max 7006ms，压力事件 1 次 |
| 修复前复跑 | W warm max 7717ms，压力事件 1 次（1:1 再证）|
| 修复后 `-r5 -c30` B+W | W warm max **38ms**，压力事件 0，300/300 全过 |
| 修复后 `-r10 -c30` W | W warm max **53ms**，压力事件 0 |

**遗留**：修复后压力事件未再出现 → `pressure relief` 快路径尚未被实战命中，属"没踩雷"而非"踩雷不卡"。严格闭环需压力注入实验（恢复一个工作集远超快照 allocatable 的沙箱强制触发）。

## 6. 关联

- bench 侧快照物化清理（ws-bench `6fba970`）消掉的是另一类毛刺（物化写盘脉冲污染 dispatch/pre_ch），与本问题独立
- DB fsync 串行税：orchestrator `feat/db-synchronous-env` + 上游 issue kuasar-sandbox/orchestrator#202
- 上游可提 issue：TargetFreeBuffer 公式对 page-cache 重负载偏激进 + MaxStep=TargetFreeBuffer 的一步穿透；修复（压力全额放气）可随 PR

---

## 7. 更正与补全（2026-08-15 深夜，压力注入实验后）

强制复现（`--probe run --command 'dd … /dev/shm 1.6G'`，3 沙箱全卡 7.7-11.6s）暴露了更完整的三层结构，**§2 的"Hint 挤牙膏"只是次要路径**：

```
20:52:44.09  heartbeat: controller adjusted alloc 1.2GiB → 256MiB   ← 元凶①
             气球一步充到 7.9GiB，无视快照记录的工作集
20:52:44.19  sensor 救火：+64MiB (urgency=normal reason=high_event) ← 元凶②
20:52:45-48  +64MiB × 8 次，欠 ~1GiB 要 16 次 ≈ 卡 8s
```

1. **元凶①（orchestrator 资源控制器）**：restore 继承快照 allocatable（1.2GiB）后，heartbeat 的策略校正**一步**砍回节点策略 256MiB（`controller.go heartbeatOnce` 应用 `NewAllocatable`）——下行无速率限制（上行有 `memory_grant_per_sec_factor`，下行没有）
2. **元凶②（sandboxer sensor 步长）**：压力传感器 `RequestBudget` 每次只 +64MiB（`StartSensor(ctx, 64<<20)` 硬编码于 lifecycle.go:514 / restore.go:659），高紧迫度也不放量
3. **放大器（host 内核）**：PSI 未启用（`/proc/pressure` 不存在），sensor 降级 events_poll（100ms 轮询 OOM 事件），丢掉亚毫秒快路径
4. **误报源（guest 内核）**：tmpfs 零页/page cache 在 `MemAvailable` 里计为"可用"，guest 上报虚高 → Hint 路径的挤压误判（§2 的机制，真实但次要）

**Hint 压力全额放气补丁**（§4"已打"）针对第 4 层的低报场景，无害保留，但不在主路径上。

**修正后的修复菜单（按性价比排序）**：
- ② sandboxer：sensor 步长 64→256MiB，或 urgency=high 时按差额放量（一行级）
- ① orchestrator：下行校正加速率限制（对称复用 memory_grant_per_sec_factor），或 restore 后 N 秒内以快照 allocatable 为下限
- ③ host：内核 cmdline 加 `psi=1` 重启，恢复 sensor 快路径（运维项）

## 8. 修复实测（fix/balloon-pressure-relief @ 94077f9）

| 项 | 修复前 | 修复后 |
|---|---|---|
| 强制卡顿（dd 1.6G tmpfs ×3 沙箱）| 7.7-11.6s | **5.6s** |
| 下行校正 | 一步 1.2GiB→256MiB | 阶梯 ≤256MiB/跳（日志 `stepping down`）|
| sensor 步长 | 64MiB/次恒定 | high 紧迫度 ×4=256MiB |

**剩余 5.6s 的定性**：guest 内部 reclaim 对 host 完全不可见——events_poll 三个信号（cgroup OOM、high 计数、RSS/memory.high>0.95）随阶梯同步下移永不触发（实测修复轮 granted=0 次）。**点亮 PSI（host `psi=1` 重启）是下一步关键**，sensor 将获得亚毫秒级 stall 感知，三修复合力应把卡顿压到亚秒。
