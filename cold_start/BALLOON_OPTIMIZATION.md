# Balloon 性能优化方向：基于 2026-11-03 最新代码

本文保留源码审查、地址连续性采样与正确性约束。**后续 PoC 已完成并实测有效**：实现、最新 sandboxer 对照结果、第二项零页预填优化与复现方法见 [BALLOON_POC.md](BALLOON_POC.md)。这里的采样和实施建议不是最终性能数据。

## 代码基线已刷新

- sandboxer 远端 main：`185f50b641ea84864bc8e0f7bab5c469100d45e3`，已 fetch，并在 detached worktree 检查。
- guest-runtime 远端 main：`9cda6a9bea6951882b68575016c63d8d74cbcb78`。
- sandboxer 构建的 CH 仍为 **v51.1 + 本仓 native-deps/deps/ch-patches**。这里分析的是本栈实际使用的版本，不是未被采用的 CH 上游 main。
- 与实测部署来源 `3fa03fc7267b5d6c53040592fffa05b9c2220925` 比较，`pkg/uffd`、`pkg/resctl`、`pkg/sandbox/ch.go`、整个 `native-deps` 的 Git 对象完全相同。因此旧实测涉及的热路径在最新 main 仍存在；随后已对 185f50b 构建的 sandbox-ctl 重新做 E2E 对照；node-ctl、kernel、guest bundle 仍固定为 release 构件，详细来源见 PoC 报告。
- 已应用的 `virtio-devices/src/balloon.rs` Git blob 为 `2cfed19d1db75304f121edde6717f956ee62736d`，匹配当前 0004 patch 的 postimage。

本次校验记录：`cold_start/results/balloon-direction-20261103/source-audit.json`。该目录忽略于 Git，历史性能汇总和固定参数见 [EXPERIMENT.md](EXPERIMENT.md)。

## 首选方向：CH inflate 连续 PFN 有界合并

现有调用链：

```text
BalloonEpollHandler::process_queue(queue_index=0)
  对描述符中的每个 u32 PFN：
    release_memory_range_4k(pfn)
      4KiB host page：release_memory_range(addr, 4096)
        MADV_DONTNEED → 同步 REMOVE 确认
        lseek(SEEK_DATA)
        有驻留数据时 PUNCH_HOLE
  完成描述符 → add_used → signal
```

在 4KiB host 上，7GiB 初始 inflate 对应 **1,835,008 个 4KiB PFN**。现有实现对每页独立 release，即使 memfd 已是 hole 也必须产生逻辑通知。这意味着大量系统调用、通知与用户态/内核往返；不只是一把 Go 锁的问题。

### 新采样：连续性足够支持先做简单方案

对一个默认配置测试 VM 的 CH 使用 strace，约 0.8 秒采样窗口解析出 **14,504 次成功 MADV_DONTNEED，全部为 4096 字节**。该诊断会改变执行速度，所以不将其用于性能计时。

| 固定观察窗口 | 参与归并的原调用数 | 连续地址段数 | 原调用/段 |
|---|---:|---:|---:|
| 16 条 | 14,496 | 963 | 15.05 |
| 64 条 | 14,464 | 283 | 51.11 |
| 256 条 | 14,336 | 112 | 128.00 |

保持原顺序、只合并当前 run 前后相邻地址，与排序后合并的结果相同。因此第一版**不需要排序或复杂索引**。

这些观察窗口不是实际 virtqueue 描述符边界；128 倍只是这段地址样本的调用归并空间，不是确定的实现收益，更不是 128 倍 e2e 提速。

原始轨迹、统计与确认删除 404 的记录：`results/balloon-direction-20261103/{ch-release.strace,address-locality.json}`。测试 VM 已删除。

### 第一版具体改法

在 **同一 inflate 描述符内**维护 pending range。读到相邻 PFN 时扩展 run；地址不相邻、达到上限或描述符结束时，调用一次已有 `release_memory_range`。

```text
读取一个 PFN
  若恰好接 pending range 的尾或头，且仍在同一 memory region：扩展
  否则：flush pending，建立新 range
达到可配置/固定的小上限（起步可用 256 PFN，即 1MiB）：flush
描述符结束：flush，成功后才 add_used
```

建议先覆盖 **4KiB host page** 分支；更大 host page 保留现有 `PartiallyBalloonedPage` 路径，避免未收到完整 host page 的所有子 PFN 就错误释放。

必须保留：

- 只合并真实包含在 PFN 列表里的连续页，不能跨中间的未释放页；不能跨 guest-memory region、memfd offset 边界。
- 不跨已完成/已返回 used ring 的描述符滞留释放任务。
- 继续使用 `MADV_DONTNEED → SEEK_DATA → 必要时 PUNCH_HOLE` 的顺序；即使是 hole，也保留逻辑 REMOVE 通知。
- release 完整执行并保持现有错误传播后才完成 descriptor；不能后台延迟 punch，再让 guest 先复用这些页。
- 不一次聚合整个 7GiB；大范围状态更新也会让 consumer 持锁过久，采用有限 run 上限。

### 修改归属与兼容性

修改应进入 `sandboxer/native-deps/deps/ch-patches/` 中 balloon release 的同一功能 patch，重新生成匹配的 native CH binary。原有快照格式、UFFD 协议及外部 API 无需因这一算法优化而改变；不必附带新的 pin/ABI 设计。

源码依据：

- [当前 CH patch 0004](https://github.com/kuasar-sandbox/sandboxer/blob/185f50b641ea84864bc8e0f7bab5c469100d45e3/native-deps/deps/ch-patches/0004-virtio-devices-preserve-balloon-release-notification.patch)：保留逻辑释放通知和 sparse 快路。
- 已应用 balloon.rs：`process_queue` 279–337、`release_memory_range_4k` 242–277、`release_memory_range` 194–240。其 blob 已对当前 patch 验证。

## 第二阶段：UFFD consumer 更短、更公平，而不是直接去锁

最新代码已经有：

- `removeQ` 容量 4096；backendVA madvise 在独立 flusher 中执行。
- flusher 在 64 个事件或 10ms 时触发，排序并合并连续/重叠范围。
- reader 以最多 16 个消息的缓冲读取，先发布本批 REMOVE/UNMAP，再在锁外派发 PAGEFAULT。

因此“加异步 REMOVE 队列”“把 backend madvise 批处理”不是新的优化点，已经存在。

在先做 CH 合并后，如仍观察到明显 host 争用，可做以下小改：

1. 在**已经读到的有界 batch**内合并同注册区域、相同语义的 REMOVE，使 released bitmap、dirty bitmap、PageState 与 removeQ 处理按 range 执行；UNMAP 仍保持原有拓扑失效和唤醒语义。顺序同步的 producer 可能每次只产生一个事件，所以仅增大 reader buffer 不保证有效。
2. `drain` 当前一直处理一个 fd 到 EAGAIN；考虑在完成当前批次后按事件量或时间预算交还 epoll，避免一个 region 的持续 REMOVE 长期压住另一个 region。不能处理一半已确认的 batch 就让 installer 运行。
3. 检查现有 `remove_q_dropped`：它实际表示队列满时转同步 madvise 的 fallback，不是丢弃逻辑释放。若该值显著增长，可将物理 PTE 清理转移到锁外的可靠 pending/coalescer；不能因为队列满而丢通知。之前样本没有证明此 fallback 是主瓶颈，因此不把它列为首改。

### installMu 为什么不能直接删掉或把簿记移出锁

`drain` 的锁覆盖 **unix.Read + Released/dirty 状态发布**。读 UFFD 非缺页事件会确认通知、允许 CH 继续；若 read 在锁外，fault installer 可能在逻辑 Released 尚未发布时装入旧 snapshot 内容。这个约束有现成的 `TestReaderPublishesBatchReleaseBeforeBlockedFaultDispatch` 回归测试。

必须同步完成的最小逻辑状态发布仍应受 gate 保护。物理 backendVA PTE 清理可以异步，旧源码已经这样处理。区域锁、页代次等设计可以后续研究，但需要重新证明事件确认与 installer 的顺序；不是将锁直接替换成若干 mutex 就完成了。

源码：

- [handler.go](https://github.com/kuasar-sandbox/sandboxer/blob/185f50b641ea84864bc8e0f7bab5c469100d45e3/pkg/uffd/handler.go)：`drain` 520、`handleRemoveLocked` 643、`runRemoveFlusher` 734。
- [fault.go](https://github.com/kuasar-sandbox/sandboxer/blob/185f50b641ea84864bc8e0f7bab5c469100d45e3/pkg/uffd/fault.go)：urgentCopy/urgentZero 都在安装 gate 内校验状态、执行 ioctl 和提交状态。
- [load_test.go](https://github.com/kuasar-sandbox/sandboxer/blob/185f50b641ea84864bc8e0f7bab5c469100d45e3/pkg/uffd/load_test.go)：上述 batch-release 顺序测试。

## 同时补对计时口径

已有 `fault_queue_wait` 的窗口是创建带 queued 时间的 faultEvent 到 worker 收到它；不覆盖 reader 尚未读取/派发事件的等待，也不覆盖 worker 后续获取 installMu 的等待。

所以 **12µs 既不能证明锁等待很小，也不能证明这 2.7 秒都在 installMu 上**。不能继续拿它替代完整故障响应时间。

| 计数/时间 | 作用 |
|---|---|
| inflate PFN 数、release run 数、bytes/run 分布 | 验证合并确实减少调用，而非少做回收 |
| CH descriptor/release 时间，MADV 与 PUNCH 调用数 | 识别 producer、通知往返和实际 punching 成本 |
| reader batch 大小、REMOVE 发布耗时 | 判断 consumer 的逻辑处理成本 |
| installMu wait 与 hold，按 reader/fault/vhost 分类 | 真实量化锁竞争，替代错误的队列时间推断 |
| remove_q_dropped、madvise_calls | 确认已有异步物理清理是否回落同步 |
| target 接受到 memory_actual_size 收敛的时间 | 区分“API 接受目标”和“实际完成回收” |

新增字段应明确 `_ns` 或 `_ms`。尽量按 descriptor/run 聚合或采样，避免为每个 PFN 增加高开销计时。strace 只用于诊断模式，不混入无 tracing 的性能统计。

## 配置策略是临时手段，不能替代代码效率

`pkg/sandbox/ch.go` 当前用 initial Budget 计算初始 balloon target。capacity=8GiB、startup=1GiB 会从开机就要求 inflate 7GiB。startup=8GiB 时目标为 0，但只要 allocatable 小于 capacity，仍保留 balloon 设备供后续控制使用。

现有代码的 steady shrink 已按 `MemoryStep=64MiB` 约束目标与实际位置，但初始 CH 参数仍可直接设置 7GiB。若要设计启动期 grace/分阶段回收，应和 node 预留、实际回收收敛与后续业务尾延迟一起验证；只延后同样昂贵的回收会把问题转移到后续命令。

已经测到只改 startup 可大幅改善 e2e，但会增加 admission 预算预留。不能在仍只预留 1GiB 的情况下，擅自把 balloon target 改为 0、让 guest 拥有 8GiB 可用容量而绕过资源约束。

## 推荐实施顺序与验收

1. **只改 CH 4KiB-host inflate 的有界连续 range 合并**，不同时修改 startup、worker 数和锁模型。
2. 增加/使用上述必要计数，对相同资源条件做 before/after。
3. 如仍有明显尾延迟，再做 consumer 批次处理/公平调度；最后才考虑安装 gate 的结构性分片。

先用相同最新 main 构建 baseline，再只替换实验用 CH binary 构建 candidate；kernel、runtime bundle、node-ctl、sandbox-ctl、镜像及资源参数保持一致。不要将此前 release 的数据直接当作最新完整栈的 before。

```bash
# baseline 栈
python3 cold_start/bench.py -n 10 --label balloon-before \
  --capacity-memory 8GiB --allocatable-memory 256MiB --startup-memory 1GiB \
  --out cold_start/results/balloon-before

# 隔离测试栈只替换 CH 为 candidate，参数不变
python3 cold_start/bench.py -n 10 --label balloon-after \
  --capacity-memory 8GiB --allocatable-memory 256MiB --startup-memory 1GiB \
  --compare cold_start/results/balloon-before/result.json \
  --out cold_start/results/balloon-after
```

验收至少同时看：

- create→首命令返回的 e2e 与 exec 的 min/p50/p90/p95/max/avg；不能只是把耗时移到 launch 窗口。
- 同样 7GiB 回收目标的 `memory_actual_size` 进展/收敛、host CH 与 sandbox-ctl RSS/PSS、release bytes；不能靠少回收或不回收换速度。
- 回收期间持续 health 和后续命令尾延迟，不能只测首条 `true`。
- source/capture/restore：非零父 snapshot + 已释放 sparse hole，不能恢复出父旧值；active page 内容保持；并发 vhost load、stale source read、REMOVE/UNMAP 顺序、错误传播都回归。
- 非连续/倒序 PFN、描述符边界、跨 region、部分/无效 PFN、更大 host page 的现有路径保持正确。

已完成第一项 CH range 合并及第二项冷启动 ZeroSource 有界预填的实现和隔离实测，资源参数保持不变；最终数据与剩余验证边界见 [PoC 报告](BALLOON_POC.md)。
