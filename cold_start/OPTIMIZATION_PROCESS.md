# 两项冷启动优化：代码修改与验证过程

后续范围约束与官方资料调研见 [COLD_ONLY_STRATEGIES.md](COLD_ONLY_STRATEGIES.md)：零页优化限定冷启动；当前 CH balloon 合并 PoC 仍作用于共享路径，严格隔离恢复需要补充冷启动开关。新候选仅完成内核机制验证。

日期：2026-11-03。本文记录 CH balloon 合并释放、sandboxer 冷启动零页预填两项 PoC 的修改与实验过程。结果总览见 [SUMMARY.md](SUMMARY.md)，完整数据边界见 [BALLOON_POC.md](BALLOON_POC.md)。

## 实验基线

- sandboxer：`185f50b641ea84864bc8e0f7bab5c469100d45e3`。
- accelerator：`6ecec94495c0a255479f18e6219e52b4eb2408e7`。
- CH：v51.1 + sandboxer 现有 12 个 native patches；应用后源码 HEAD 为 `27ecfb2558b0ef684c239eee111d23cc3166ee64`。
- node-ctl、kernel、guest bundle、img 在各组间固定；不是所有组件都重建自各自最新 main。
- 资源固定为 2 vCPU、capacity=8GiB、allocatable=256MiB、startup=1GiB，初始 balloon target=7GiB。
- 使用独立 `balpoc` namespace；每个性能样本创建新的 img 沙箱，等待 running 后执行首条 `true`，最后删除并确认 HTTP 404。

两项优化分开比较：先只替换 CH；确认收益后，保持 CH 不变，再替换 sandbox-ctl。性能采样期间不运行 strace、构建或其他 bench。

## 修改一：CH balloon 连续 PFN 合并释放

### 问题定位

原 inflate 路径对描述符中的每个 4KiB PFN 分别执行释放。7GiB 回收目标对应 1,835,008 个 PFN，逐页调用放大了系统调用、UFFD REMOVE 通知和调度往返。

诊断采样中，14,504 次成功 MADV_DONTNEED 全为 4096 字节。对其中固定 256 条观察窗口按地址连续性归并，14,336 条调用可归为 112 个连续段。观察窗口不是实际描述符边界，归并倍率也不是性能提速倍率，但足以支持先验证简单的连续 range 合并。

### 代码修改

补丁：[balloon-inflate-runs.patch](poc/balloon-inflate-runs.patch)。目标文件：CH `virtio-devices/src/balloon.rs`。

| 修改位置 | 修改内容 |
|---|---|
| `InflateRun` | 保存 pending range 的 base、len、region_start；支持前接、后接连续 PFN |
| `MAX_INFLATE_RUN_BYTES` | 每段最多 256 个 PFN，即 1MiB |
| `release_inflate_pfn` | 4KiB host 上累积连续 PFN；不连续、重复、跨 region 或达到上限时拆段 |
| `flush_inflate_run` | 将 pending range 交给已有 release_memory_range |
| `process_queue(0)` | 每个描述符建立独立 pending range，结束时 flush，完成释放后才 add_used |

处理流程：

```text
读取 PFN
  → 同 region 且与 pending range 相邻：扩展
  → 否则 flush 旧 range，再建立新 range
描述符结束
  → flush
  → add_used
```

保留原有 `MADV_DONTNEED → SEEK_DATA → 必要时 PUNCH_HOLE` 顺序。即使物理 memfd 已是 hole，也必须发送逻辑 REMOVE，防止 lazy snapshot 父层旧内容重新出现。不能跳过通知、跨描述符延迟释放或合并中间未返回的页面。大于 4KiB 的 host page 继续走原 PartiallyBalloonedPage 路径。

### 验证过程与结果

1. 从同一应用过 native patches 的 CH 源码构建 baseline。
2. 应用 PoC，构建 candidate；两者使用同一依赖锁和构建参数。
3. 执行 release tests，显式包含 ignored 的真实 UFFD 测试：6 个通过。覆盖连续性、重复、gap、region、上限，以及 sparse/resident 非零源释放后归零、邻页保持。
4. 固定其他构件及资源，分别跑 10 次无 tracing 的冷启动。
5. 独立查询 CH vm.info，验证同样 7GiB target 的实际回收收敛。

| 指标 | 原始 CH | 合并 CH |
|---|---:|---:|
| 端到端 p50 | 3449.220ms | 773.743ms |
| 首条 exec p50 | 2698.794ms | 159.662ms |
| create→actual 收敛，单次诊断 | 19203.790ms | 1236.703ms |
| 最终 actual | 1GiB | 1GiB |
| balloon target | 7GiB | 7GiB |

首命令 p50 下降约 94.08%。同样目标的完整回收也更快，收益不是靠少回收或调整 startup 预算取得。

这组对照证明初始 balloon 回收是首命令的重要干扰源；没有进一步证明全部节省时间都来自某一把锁。`fault_queue_wait_p50≈12µs` 不覆盖 reader 尚未读取和 worker 后续获取 installMu 的等待，不能误读成 12ms 或用来排除锁竞争。

## 修改二：冷启动 ZeroSource 有界零页预填

### 后续瓶颈

balloon 优化后，CH→Run /sbin/init 的 p50 仍约 433ms，是剩余最大的可见阶段。原 ZeroSource 路径使用 64KiB 总窗口：urgent 4KiB + best-effort 15 页尾部。内核早期连续触碰零内存时，仍产生较多缺页往返。

因此保留 urgent 页优先的结构，只扩大冷启动权威零源的尾部预填窗口，分别验证 256KiB 和 1MiB。

### 代码修改

补丁二选一：

- [cold-zero-prefill-256k.patch](poc/cold-zero-prefill-256k.patch)
- [cold-zero-prefill-1m.patch](poc/cold-zero-prefill-1m.patch)

目标文件位于 sandboxer `pkg/uffd/`：

| 文件 / 位置 | 修改内容 |
|---|---|
| `handler.go` | 新增独立 coldZeroFaultFillBytes，普通 zeroFaultFillBytes 保持 64KiB |
| `fault.go` / handleAbsentFault | 仅 isZeroSource 为真时采用更大的规划上限 |
| `fault.go` / tail 执行 | 仅 ZeroSource + StateAbsent 的 zero tail 使用更大执行上限 |
| `fault.go` / zeroScratch | 扩到相应最大窗口，保证 ZEROPAGE 不支持时 COPY fallback 不越界 |
| `fault_test.go` | 增加窗口、RAM 末端、Loaded 状态边界、CH region 和 COPY fallback 测试；普通恢复零源测试继续验证 64KiB |

安全边界沿用现有机制：窗口受 RAM、CH region 和连续 PageState 限制；ioctl 前再次复核状态；urgent 4KiB 先完成，尾部使用原单个 best-effort worker。普通 Data、恢复快照 Hole/Zero、StateReleased 的 64KiB 策略不随此实验扩大。

### 实现过程中发现的问题

最初仅扩大 zeroFaultFillBytes，不足以改变真实路径：handleAbsentFault 的共同规划窗口仍为 64KiB，tail 执行也另有上限。单元测试暴露了规划、执行与预期不一致。

最终改成独立冷启动常量，并同步处理规划上限、tail 执行上限、fallback scratch。恢复快照的通用策略继续保持原值；两份调参补丁不能叠加应用。

### 验证过程与结果

1. 先完成 UFFD package 单元测试，再构建相应 sandbox-ctl。
2. 保持 CH 为合并版本，分别试跑 256KiB、1MiB；确认有收益后，各扩到 10 次。
3. 对照生命周期内的缺页次数和填零页数，检查收益是否伴随额外预填。
4. 每种预填策略另跑 3 个诊断 VM，比较首命令后及回收收敛后的 PSS 和 memfd allocation。
5. 最终 1MiB 版本执行 UFFD race 检查，并做真实 pause/resume 回归。

| 指标 | 合并 CH + 原 64KiB | +256KiB | +1MiB |
|---|---:|---:|---:|
| CH→init p50 | 432.825ms | 333.519ms | 296.510ms |
| 首条 exec p50 | 159.662ms | 113.095ms | 126.809ms |
| 端到端 p50 | 773.743ms | 608.537ms | 582.601ms |
| faults_absent 中位数 | 4156 | 1110 | 348 |
| pages_zeroed 中位数 | 65036 | 67635.5 | 75209 |

1MiB 相对仅 CH 方案，进一步降低端到端 p50 24.70%、CH→init p50 31.49%。填零页数有所增加，因此不能仅凭缺页次数减少判断收益。

每种策略 3 次诊断中，1MiB 在首命令后和收敛时的进程合计 PSS 比原预填策略高约 3–5MiB。这是两个时点的占用，不是启动瞬时峰值。256KiB 首命令 p50 更低，1MiB 端到端及 p95 更低；仍需更大样本和并发资源矩阵来选择默认值。

## 实验部署问题与数据纠正

过程中发现：隔离目录的 node-ctl 若是指向 release 的软链接，os.Executable() 会定位 release 真路径，helper discovery 也从该目录选 sandbox-ctl，导致私有目录新构建的 sandbox-ctl 未实际运行。

处理方法：将 node-ctl 改为隔离目录内的真实文件；创建独立诊断 VM，核对 /proc/PID/exe 和 SHA256；再重跑四组各 10 次测试。每组 journal 的 `_EXE` 也核对到对应私有 sandbox-ctl。

本文及总结只采用修正后的 `latest-*` 数据。早期未修正的 baseline-10、candidate-10、zero256-pilot 不纳入最终统计。可复用的核对入口是 [validate_runtime.py](poc/validate_runtime.py) 的 identity 模式。

## 组合回归与最终结论

- 四组共 40 次 bench 全通过：七项耗时齐全、客户端计时闭合、删除最终确认 404。
- CH release tests 6 个通过；两种窗口的 UFFD package 测试通过；最终 1MiB 版本 race 检查通过。
- 最新 sandboxer + CH 合并 + 1MiB 预填连续 3 轮 pause/resume 通过：同一进程 PID/token 保持，32MiB 非零内存与 8MiB fsync 文件 SHA256 保持，定时器恢复前进。
- 组合方案端到端 p50 从 3449.220ms 降到 582.601ms（-83.11%），首条 exec 从 2698.794ms 降到 126.809ms（-95.30%）。
- 实验结束后清理 balpoc 服务和网络，Nova 服务保持 active，原 release sandbox-ctl 哈希与实验前一致。

两项修改目前为已验证的 PoC。尚未完成的范围包括启动内存峰值、大并发、更多 capacity/vCPU 与 host page size、业务命令尾延迟；没有将其视为已经充分验证的全局默认策略。

下一步最大可见段为 CH→init 约 297ms，其次是 exec 约 127ms。应继续补内核早期内存准备和 SDK/proxy/envd/fork/流结束的分段测量；这些区间尚不能按现有日志精确归到某个函数。

## 复现与证据

- [构建、部署、版本核对、bench、日志切分与回滚](poc/README.md)
- [四组全部细分阶段 min/p50/p90/p95/max/avg](poc/DISTRIBUTION.md)
- [完整报告](BALLOON_POC.md)
- [统计 JSON](examples/balloon-poc-20261103-summary.json)
- 原始证据：`cold_start/results/balloon-poc-20261103/`，Git 忽略；包含 samples、journal、构件哈希、测试日志、收敛与快照校验、SHA256SUMS.json。
- 本机源码、构建产物和实验管理脚本：`/root/sandbox/balloon-poc-20261103/`。
