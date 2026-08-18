# envd launch 阶段优化与插桩记录（2026-08-17 ~ 08-18）

本文记录围绕 launch 耗时（以 envd 阶段为切入点）的定位方法、五处已部署代码修改、
验证数据与剩余瓶颈。所有结论基于 nova 节点（`node-ctl-nova`，external proxy 模式）
bench 实测；bench 口径：ws-bench，B=drop-caches 快照 / W=working-set 快照，health 探针。

## 总账（B 模板，c=30 同口径）

| 指标 | 08-17 起点 | 08-18 全部五项后 | 降幅 |
|---|---|---|---|
| launchMs p50 | 294.7ms | **213.9ms** | -27% |
| health1 e2e p50 | 629.1ms | **463.4ms** | -26% |
| envd 阶段 avg | 130.7ms | ~74ms | -43% |
| create-api p50 / max | 33.6 / 62.7ms | **24.9 / 50.6ms** | -26% / -19% |
| MMDS 假失败率 | 30% | **0** | |
| prewarm 赌输率 | 49% | **4%** | |
| W dispatch 图深税 | +9.6ms | +0.7ms | 抹平 |

（c=15 口径 launchMs p50 已到 **167ms**；W 模板除 dispatch/envd 同幅受益外未入总账，
因其快照当时带气球 bug——见 `balloon-ws-pagecache-evicted.md`。）

---

## 修改一：envdInitAttemptTimeout 50ms → 300ms

**文件**：`orchestrator/internal/orch/orch.go`（常量 `envdInitAttemptTimeout`）

**根因**：50ms 客户端超时在 burst 负载下不够。/init p50 ~24ms 但 auth 的 MMDS 查询慢时
超 50ms → conductor 取消在途请求 → guest Go client mid-response RST → guest 日志记
`mmds_put_result=transport_error`（**不是网络故障**，pcap 证实 RST 均为 guest 收完响应后
主动发）→ conductor 1ms 后重发整个 /init。30% 样本中招，每沙箱平均 2.6 次 /init。

**验证**：
- MMDS transport_error：234/793 (30%) → **0**
- envd 阶段 B avg 130.7→84.5（-35%），max 224→119
- 附带效果：W 的 2/150 HTTP0 失败与 warm 9.4s 离群消失

**识别方法**（复用）：guest journal `-t sandbox` 的 `envd_init_timing` 行看
`mmds_put_result` 与 `total_duration`——失败样本 total p50=52.8ms 卡在 50ms 线上即超时
取消；`ip netns exec e2bnova_mgmt tcpdump port 19254` 看 RST 位置。

## 修改二：envd init attempt / forwarder open 插桩

为拆解剩余抖动，在链路三处加了 host 时钟打点（全部已部署在跑）：

**conductor**（`orch.go` `envdInit`）：每次 attempt 一行
```
msg="envd init attempt" sid=… attempt=1 result=success dial_ms=0.046 attempt_ms=56.98 total_ms=56.99
```
- `dial_ms`：UDS connect 单独计时（`udsClient` 返回 `*envdInitDialTiming`）
- transport 失败附 `error_kind`（timeout/refused/reset/other，`envdInitErrorKind`）

**sandbox-ctl forwarder**（`sandboxer/pkg/sandbox/forward.go` `serve`）：
```
port-forward …/envd.sock:127.0.0.1:49983 → …: open 14.27ms   （open ≥5ms 才打）
```
覆盖 vsock dial + CONNECT preface + proto 握手 + connect_ack + guest dial 整段。

**guest envd**：未动，`total_duration` 已有；journal emit 时间戳即 host 时钟的处理完成时刻。

**四段分解结论**（B 模板精确 join）：
```
phase p50 77.4 = open 建链 18.2（p99 44） + guest 处理 35.4（auth 19.5，mmds_put 15.3）
              + 请求/响应在线传输 23.3
```
UDS dial 仅 0.1ms，排除。MMDS 线上（pcap）只要 ~2ms，客户端量出 13-15ms 的差值在
guest 内部 CPU/调度，服务端无辜。

## 修改三：forwarder prewarm v2（首连握手移出关键路径）

**背景**：每次 launch 的第一条 envd 连接要付 ~18ms vsock 握手税。fork 会话已实现
prewarm v1（`restore.go` PostSpawn 在 vm.resume 后 `go Forwarder.Prewarm(第一个 dial spec)`，
即 envd.sock），但命中率只有 51%：attempt1 死于 guest restore-epoch 的 reset 窗口，
15ms 固定延迟重试后 ready 需 51-91ms，与 /init 到达（init_ack ~56ms 后）对赌。

**v2 修改**（`sandboxer/pkg/sandbox/forward.go`）：
1. 重试节凑 `prewarmMaxAttempts=24, prewarmRetryDelay=5ms`（原 8×15ms）
   → reset 窗口一结束的第一个探测即成功，ready p50 42ms（41/45 在 attempt2）
2. `Forwarder` 新增 `prewarmDone map[string]chan struct{}`；`Prewarm` 注册信号、defer close；
   `serve()` 在池空但有在途 prewarm 时 `select { <-ch; <-time.After(prewarmWaitBound=25ms) }`
   等待复用，不再并发开第二条握手（刚 resume 的 2-vCPU guest 免于双份服务）

**验证**（B 模板 c=15 ×3，复跑两轮）：

| 指标 | v1 | v2 |
|---|---|---|
| prewarm 命中率 | 51%（85 次赌输，lazy open p50 19ms） | **96%**（36 直接+7 等待，lazy=0，等待中位 ~11ms） |
| envd 阶段 p50 | ~77ms | **57.8 / 55.3ms** |

## 修改四：orchestrator ref 探测缓存（sandbox-ctl info exec 移出每次 create）

**问题**：每次 create 都要 exec `sandbox-ctl info` 探测快照 ref 图——
`snapshotRefLocations`（LaunchSpec 路径，进 dispatch 窗口）BFS 每 ref 一次，
`inspectSnapshotConfig`（admission 的 capacity/network 探测）又一次。ref 内容寻址不可变
（HMAC/digest 消费端每次打开都校验），同一模板反复 create 纯浪费。实测 info exec ~7ms/次；
W 模板图深一层（FromRefs→B），dispatch 比 B 贵 +9.6ms。

**修改**（`internal/orch/orch.go` + `ref_locations.go`）：
- `Orchestrator` 增加 `refCache map[refCacheKey]refCacheEntry`（key=manifestKey+ref，
  entry=locations 或 snapConfig），mutex 保护，返回防御性拷贝
- `snapshotRefLocations` 命中缓存直接返回；miss 走原 BFS（提取为 `walkSnapshotRefLocations`）
- `inspectSnapshotConfig` 同样缓存（原逻辑移至 `inspectSnapshotConfigUncached`）
- **失败不缓存**（瞬时 NFS/exec 错误必须能重试）；`snapshotInspector`（测试注入）天然绕过

**验证**（c=15 ×3）：

| dispatch p50 | 之前 | 之后 |
|---|---|---|
| B | 14.1ms | **8.4ms**（-5.7ms = 1 次 info exec） |
| W | ~23.7ms | **9.1ms**（B/W 差从 +9.6 → +0.7ms） |

create-api p50 52→47ms（admission 探测同命中缓存）。

## 修改五：sqlite synchronous=OFF 真正生效（cherry-pick 10ff068）

**问题**：`NODE_CTL_DB_SYNCHRONOUS=OFF` 早已写进 systemd env，但实现它的 commit（10ff068，
`feat/db-synchronous-env` 分支）**不在 main**——部署的 node-ctl 一直忽略该 env，store 以
synchronous=FULL 跑：每次 launch 的 4 个 sqlite 写点（InsertSandboxWithMMDSRouteSecretValues /
SetStartingResources / BindStartingRunner / CommitStartingRunning）各自 fsync，单写者串行化。
该 commit 自测数据：c=50 一轮 765 次 fsync / 841ms，负载下 ~1.1ms/次。
识别线索：systemd 有 env 但 conductor.log 无 `[store] sqlite open:` 启动行
（该 commit 会打），`git merge-base --is-ancestor 10ff068 HEAD` = NO。
**教训：branch 上有用的 commit ≠ main 有；env 设置了 ≠ 生效——部署后要验启动日志行。**

**修改**：`git cherry-pick --no-commit 10ff068`（仅 `internal/store/store.go`，
env 解析 + DSN `synchronous()` pragma + 启动实测日志行）。

**验证**（B，c=15×3；W 同幅）：

| 指标 | 前 | 后 |
|---|---|---|
| create-api p50 | 47.4ms | **19.4ms** |
| create-api max | 553ms | **49.8ms**（fsync 撞写盘回写毛刺的长尾消失） |
| launchMs p50 (c=15) | 194ms | **167ms**（launchMs 时间线内 3 个写点同受益） |
| admission_to_node_prepare p50/max | 4.7 / 434ms | 2.2 / 56ms |

启动日志确认：`[store] sqlite open: journal_mode=wal synchronous=0 (requested=OFF)`。
**代价**：断电可能丢最近事务/损坏 db——env 门控，bench 节点用 OFF，生产建议 NORMAL。

## 顺手修复（fork 会话遗留）

`sandboxer/pkg/sandbox/lifecycle.go`：`pauseTiming` 在 `snapshot.Take` 失败路径解引用
nil `out`（`TestHandleSnapshotRequestReattachesGuestAfterTakeFailure` 稳定 panic）。
改为 `err == nil` 才取 `WallclockPauseMs/DumpMs`，失败分支单独记一行。包测试全绿。

---

## 根因闭环：首连成本的最底层归因（probe ping）

回答"open 18ms + 传输 20ms 到底贵在哪一跳"：

**插桩**：`restore.go` PostSpawn 在 pinger 启动后补一发 `probe ping cold`（纯 vsock
往返，TypePing→Pong，**不含** TCP dial / envd），落在 restore_ack 刚结束时刻。

**数据**（B 模板，c=1，15 样本）：`probe ping cold rtt: min 7.3 / p50 11.3 / max 39.0 ms`

bare vsock 往返就要这个价——落在 sandbox-init restore 尾部工作期间（restore notify
本身占 guest ~25ms 串行：MUX/时钟/网络重配/文件注入），排队 + vCPU/中断唤醒成本。
**宿主机、CH（dialraw 0.35ms）、MMDS 服务端（线上 2ms）、响应回程全部排除。**
第二条连接 <5ms（poller 连接从不过 5ms 日志阈值）证明纯首触。

**并发对照（固有成本 vs burst 放大）**：

| 分量 | c=1 | c=30 | 性质 |
|---|---|---|---|
| open 建链（vsock 握手） | 22.9 | ~18 | 固有（restore 瞬间冷往返） |
| 去程（relay 就绪→handler 启动） | 14.8 | 20.6 | 大部分固有（首触字节路径） |
| guest envd 处理 | 11.9 | 35.4 | **3× burst 放大**（MMDS 排队 + 2-vCPU 调度） |

**为何 ~35ms 首触税宿主侧吃不满**（v1 POC 实测仅 -10ms 的原因）：
1. guest 在 restore MUX 完成前会 **reset 陌生 port-5000 连接**（预热 attempt1 必 EOF，
   与已知 restore vsock 竞态同族）；
2. 被接受后的握手仍排在 restore 尾巴后面。v2 的 5ms 重试 + serve 等待把可重叠部分吃到
   96% 命中；剩余要动结构：guest 侧在 restore 流程内预建通道（重建 sandbox-init → 改
   bundle digest → 旧快照全废），或数据面 vhost-vsock 绕开 CH 用户态代理。

## 剩余瓶颈（c=30 口径，下一步候选）

```
resume_to_sandbox_init_ack   74.8ms  ← restore-epoch ~26ms(wallclock2/connect12/network7/files5)
                                        + uffd 缺页(B 定义,W 避开) + files→ack 尾巴
runtime_ready_to_envd_done   71.9ms  ← guest 处理(含 MMDS auth ~19 协议税,不动) + 传输 ~20
sandbox_ctl_pre_ch           30.4ms  ← staged 打点 0-15ms 分散(zip+mem 7 / net_ready 15),无单一肥肉
ch_start_to_resume           18.5ms  ← WaitReady 已是 1ms 起步轮询,CH 真实工作量
create-api 残余              ~20ms   ← TLS 0.2 + admission 3 + barrier + API 通路,可再拆
```

另：7s 周期写盘毛刺（`bench-spike-investigation` 未结）在修改五后对 create-api 的长尾
影响已消（fsync 没了），但 dispatch/pre_ch 毛刺可能仍在。

## 部署状态与回退

| 二进制 | 内容 | 备份 |
|---|---|---|
| `orchestrator/bin/x86_64/node-ctl` | 修改一、二、四、五 | `node-ctl.bak.envd50`（最初）→ `.bak.refcache` → `.bak.refcache2`（最近前版） |
| `orchestrator/bin/x86_64/sandbox-ctl` | 修改二（open 日志）+ 三 + pauseTiming 修 | `sandbox-ctl.bak.inst`（无 prewarm）→ `.bak.prewarm1`（v1） |

注意事项：
- 两棵树改动**均未提交**（orchestrator：orch.go/ref_locations.go/store.go + netserve.go/proxy.go
  的搁置 TLS ticket 代码；sandboxer：forward.go/restore.go/lifecycle.go/serveandwait.go/take.go）。
  **建议尽快固化 commit**，尤其 cherry-pick 的 10ff068 和 ref cache——避免重蹈"branch 有
  main 没有"的覆辙。
- TLS session-ticket 跨 worker 共存代码**未完成未启用**（bench 客户端拿不到 resumption
  收益而搁置），编译通过无行为变化。
- 窗口日志样本：`/tmp/jwin.jsonl`、`/tmp/{runner,conductor}_prewarm2.log`、
  `/tmp/conductor_inst.log`；conductor.log 会截断轮转，分析窗口数据要及时存。
