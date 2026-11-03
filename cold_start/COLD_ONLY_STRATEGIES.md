# 冷启动专属优化：官方资料调研与范围核查

日期：2026-11-03。约束：优化仅用于新建 img 的冷启动；快照恢复保留现有内存加载策略。本次检索 Linux、Cloud Hypervisor、Firecracker 官方文档及 Linux 6.6 源码，并做独立内核机制验证。

**结论：最值得做下一轮 VM PoC 的候选，是让冷启动缺失页走内核正常分配，同时保留 UFFD REMOVE/UNMAP 通知。** 本机已经验证“WP 注册但不启用写保护”可实现这组基础行为。它尚未接入 sandboxer/CH，也没有新的 VM 延迟收益数据；现有 583ms 等结果仍属于之前的两项 PoC。

## 1. 先明确冷启动和恢复的边界

“全零”描述的是冷启动初始内存源，不能推广成整个 VM 生命周期的所有页面都为零。kernel image、guest 和 vhost 都会写入非零数据；已写页面不能被后续零页预填覆盖。

当前 sandboxer 路径核查：

| 场景 | 实际内存源 | 当前策略 |
|---|---|---|
| 新建 img | runtime_sdk.go 使用 uffd.ZeroSource{} | 可选择冷启动专属优化；只处理适用的 Absent 页 |
| 从 snp 恢复 | restore.go 使用 NewStreamSnapshotSource(fetch.NewLayered(...)) | 保留现有 MISSING 和快照按需加载 |
| 快照逻辑内容恰好全零 | 仍是 StreamSnapshotSource | 仍按恢复处理，不能推断为冷启动 |
| 上层快照是 hole、下层是非零 Data | layered snapshot source | 必须保留下层内容，不能当零页 |
| balloon Released 页 | PageState=Released | 不因初始源为 ZeroSource 就扩大其原有预填窗口 |

来源基线为 sandboxer `185f50b`，源码位置：`pkg/sandbox/runtime_sdk.go` 的冷启动 RunParams、`pkg/restore/restore.go` 的 layered source 构造及 RunParams、`pkg/uffd/fault.go` 的 Absent/Released 分支。

### 已有两项补丁的实际范围

- **零页预填补丁**：规划和 tail 上限都使用 ZeroSource 判断，普通恢复 Hole/Zero、Data 和 Released 窗口未扩大。不能仅因为某个 Run.Kind()==Zero 就启用冷启动优化。
- **CH balloon 合并补丁**：当前修改共享 inflate 路径，尚无 cold-only 开关，也会用于恢复后的 VM。之前的 pause/resume 通过是覆盖路径的正确性证据，不能表述为“恢复代码路径完全不变”。

若严格要求恢复沿用原实现，CH 合并需要另做默认关闭的冷启动选择：由宿主机新建 img 路径明确启用，restore 路径明确关闭；临时策略不随快照持久化和恢复。收紧后的版本需要重新验收，不能直接沿用共享版本的全部性能结论。本文没有将这一步标为已实现。

### 本次新增范围回归

补充 [cold-start-scope-tests.patch](poc/cold-start-scope-tests.patch)，仅新增测试，可应用于 baseline 或两种预填 PoC：

1. 真正的 StreamSnapshotSource，即使整段是显式 Zero，仍为 urgent 4KiB + 15 页 tail；第 17 页保持 Absent。
2. 上层全 hole、下层 0x77 的 layered snapshot，安装内容保持 0x77，普通 Data 窗口仍为 64KiB。
3. ZeroSource 的 Released 页仍使用原 64KiB 窗口。

三个测试在原始 baseline、256KiB 和 1MiB 预填版本上均通过。它们验证这些具体边界，不代表整个恢复场景已经完成全面等价性证明。

## 2. 首选候选：内核分配冷启动零页，UFFD 只保留所需通知

### 原理和依据

当前 MISSING 注册把缺失页面交给用户态。冷启动无需从快照读取这些页，但仍要经历通知、调度和填页。Linux 6.6 的 shmem 路径在设置 MISSING 时发出用户态事件；未设置时继续常规页面分配。这里的内存是 MAP_SHARED memfd/shmem，不能直接套用私有匿名映射的共享零页结论。[Linux 6.6 shmem.c](https://github.com/torvalds/linux/blob/v6.6/mm/shmem.c#L1833)

候选配置是：

```text
冷启动且明确启用：
  UFFDIO_REGISTER_MODE_WP
  不调用 UFFDIO_WRITEPROTECT 开启写保护
  不注册 MISSING / MINOR
  保留 EVENT_REMOVE / EVENT_UNMAP

恢复，或冷启动未启用/能力不足：
  沿用当前 MISSING 注册与原 handler
```

WP 注册与实际设置写保护是两个操作。只注册不等于每次写入都陷入用户态；写保护由独立 ioctl 开启。[UFFDIO_WRITEPROTECT 官方手册](https://man7.org/linux/man-pages/man2/UFFDIO_WRITEPROTECT.2const.html)

Linux 6.6 的 REMOVE 发布检查注册上下文和 EVENT_REMOVE feature，不要求必须是 MISSING；同时注册 mode=0 会被拒绝，不能直接设置空 mode 来获得专门的通知模式。[Linux 6.6 userfaultfd.c](https://github.com/torvalds/linux/blob/v6.6/fs/userfaultfd.c#L765)

需要探测 shmem WP 及事件能力，不能因为已有 MISSING_SHMEM 就假定都支持。Linux 6.6 UAPI 提供 UFFD_FEATURE_WP_HUGETLBFS_SHMEM；不满足条件时保留原模式。[Linux 6.6 UAPI](https://github.com/torvalds/linux/blob/v6.6/include/uapi/linux/userfaultfd.h)

这是依据 API/源码组合出来的候选用法，并不是 CH 或 Firecracker 已提供一个同名开关。

### 本机机制验证

独立程序：[probe_cold_uffd_wp.c](poc/probe_cold_uffd_wp.c)。测试对象为本机 Linux `6.6.0-159.4.3.154.oe2403sp4.x86_64`，两个 4KiB MAP_SHARED memfd 页面及一个未注册的 backend alias。

```json
{
  "ok": true,
  "page_size": 4096,
  "pagefault_events": 0,
  "remove_events": 1,
  "unmap_events": 1,
  "fresh_zero": true,
  "existing_data_preserved": true,
  "punch_refault_zero": true,
  "backend_write_preserved": true
}
```

验证了首次访问缺失页为零、已有非零页面保持、CH 风格 MADV_DONTNEED 后收到 REMOVE、punch 后再次访问为零、backend 写入保持、munmap 收到 UNMAP。

**0 指的是用户态收到的 UFFD PAGEFAULT 事件，不是没有 host/KVM/guest 缺页。** 该程序不运行 VM、不捕获或恢复快照，不能用它估算端到端提速。

运行方式：

```bash
cd /root/sandbox/semerge
mkdir -p cold_start/results/cold-only-strategy-probe
gcc -O2 -Wall -Wextra -Werror -pthread cold_start/poc/probe_cold_uffd_wp.c \
  -o cold_start/results/cold-only-strategy-probe/probe
cold_start/results/cold-only-strategy-probe/probe
```

### 接入本栈前必须解决的事项

- **显式协商模式**：CH 与 sandboxer 必须知道该 region 使用哪种注册模式；restore 始终选择原模式，不能把配置仅放在可继承的全局环境变量里。
- **backend EnsureLoaded**：当前 loadPage 仍依赖 UFFDIO_COPY。冷启动新模式必须另设适用的 native-access 分支，不能仅切 CH 注册 mode 后继续沿用原加载逻辑。
- **首次和后续快照捕获**：内核直接分配的页面不会经过当前 Missing handler 的 Loaded 状态提交。需要核对物理 residency、dirty ledger 和 PageState 的组合，证明首次及增量快照都正确。
- **释放通知仍必需**：冷启动 VM 也可能先写入、生成快照、再释放页面。必须保留 Released/dirty/显式 Zero 记录，否则后续分层快照仍可能继承旧值。
- **生命周期**：同一 gate 内的 REMOVE 确认和状态发布、UNMAP 失效、worker 关闭与 backend quiesce 都需要保留正确顺序。

Firecracker 的官方恢复文档同样要求识别 balloon REMOVE，并在随后重新缺页时提供零内容，而不是重新装入快照旧页。它支持上述通知语义约束，不等于证明本候选已经适配本栈。[Firecracker UFFD 与 balloon](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/handling-page-faults-on-snapshot-resume.md#userfaultfd-interaction-with-balloon)

## 3. 其他候选的比较

| 策略 | 优点 | 本栈的限制 / 代价 | 建议 |
|---|---|---|---|
| 冷启动 native allocation + WP 注册不保护 | 有机会省去 Missing 用户态往返；继续按需分配 | 需要适配 EnsureLoaded、状态和捕获；当前只完成机制验证 | 下一轮优先验证 |
| 冷启动自适应预填，64→256KiB→1MiB | 保留现有 handler；按连续访问增长，随机访问收缩 | 新增策略状态，需测预填浪费、峰值和并发尾延迟 | 较小改动的候选或 fallback |
| 冷启动固定 256KiB / 1MiB | 已有 VM 实测，便于比较 | 不同访问模式未必适用同一窗口 | 保留为对照 |
| 只注册 MINOR，缺失页交给内核 | 缺失页不走 Missing；可用 CONTINUE 映射已有页 | 已有 kernel image/backend 页面可能新增 Minor 往返；当前 handler 还拒绝 MINOR | 优先级低于不保护的 WP 注册 |
| 全量 prefault | 提前建立物理页和映射 | 8GiB capacity 下预分配后又回收 7GiB，与 1GiB startup 目标相悖 | 本轮不采用 |
| THP / 大页 | 减少映射及部分缺页开销 | 本栈是 shared memfd；需验证 shmem、UFFD、4KiB dirty/balloon 和 split；可能放大占用 | 暂不更改全局策略 |
| virtio-mem：小内存启动，按需插入 | 可能减少开机扫描和初始 inflate 工作 | 改变 guest 内存设备、资源控制和快照拓扑，超出局部优化 | 架构级备选 |
| Free Page Reporting 代替初始 balloon | 能上报空闲页 | 不等于达到指定 balloon target，也不等于执行相同资源约束 | 不能直接替换当前预算机制 |

### 比较依据

- **自适应预填**：Linux 文件 readahead 会根据先前窗口及连续访问调整大小。可借鉴这个原则，设计冷启动零页策略；它不是 Linux 已有的 ZeroSource 算法，也不能直接套用文件 I/O 的收益。[Linux 6.6 readahead.c](https://github.com/torvalds/linux/blob/v6.6/mm/readahead.c)
- **MINOR**：Linux 6.6 shmem 在页面已存在于缓存、尚未完成映射时检查 Minor；所以“缺失页交内核”不代表没有其他用户态往返。[Linux 6.6 shmem.c](https://github.com/torvalds/linux/blob/v6.6/mm/shmem.c#L1833)
- **prefault 与 virtio-mem**：CH v51.1 已提供全量 prefault 和内存热插拔相关能力。文档说明 prefault 会提前消耗物理内存并增加启动工作；这些选项不等同于本栈已完成相应接入。具体 prefault syscall 以本栈应用补丁后的 memory_manager.rs 为准。[CH v51.1 memory 文档](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/v51.1/docs/memory.md)
- **THP**：Linux 6.6 文档支持 shmem，也说明大页可能增加占用和分配停顿。较新内核文档中的 per-size shmem mTHP 控制不能直接当作本机 6.6 能力。[Linux 6.6 THP 文档](https://docs.kernel.org/6.6/admin-guide/mm/transhuge.html)
- **Free Page Reporting**：协议允许 guest 在 device 确认后重新使用上报页面；它与目标大小控制不是同一个机制。内核文档还描述了报告调度延迟，不适合直接假定能解决亚秒首命令路径。[Virtio 1.2 规范](https://docs.oasis-open.org/virtio/virtio/v1.2/virtio-v1.2.html)、[Linux Free Page Reporting](https://docs.kernel.org/mm/free_page_reporting.html)

## 4. 推荐下一轮实验

1. 先落实“模式只由冷启动入口启用，恢复强制原模式”的边界；把 CH balloon 共享补丁与冷启动零页策略分开验收。
2. 基线使用原 Missing 64KiB；对照固定 256KiB、1MiB，以及冷启动 native allocation 候选。若要归因零页策略，各组必须使用相同 CH balloon 策略。
3. 继续输出七项耗时和端到端 min/p50/p90/p95/max/avg；新增 Missing/Minor/WP 事件数，分别计数，不把内核缺页与用户态事件混为一谈。
4. 记录启动全过程的内存峰值、稳定 PSS、memfd allocation、相同 target 的实际回收收敛与并发首命令尾延迟。
5. 必测首次 snapshot、同一冷启动 VM 的后续增量 snapshot、Released hole 覆盖非零父源、vhost DMA、UNMAP、恢复后同进程活性和内容。恢复侧补充 before/after 的注册模式和延迟对照。

本轮新增的范围测试和内核 probe 证据位于 `cold_start/results/cold-only-strategy-20261103/`，含源码/二进制 SHA256、主机环境、三版本范围测试日志。新的 VM 集成与性能实验尚未执行。
