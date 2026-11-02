# 冷启动优化 PoC：balloon range 合并 + 冷启动零页预填

2026-11-03，隔离 namespace `balpoc` 实测。**两项优化均已实现并验证有效**，资源参数保持 capacity=8GiB、allocatable=256MiB、startup=1GiB、2 vCPU。

组合方案的端到端 p50 从 **3449.220ms 降到 582.601ms（-83.11%）**；首条 exec 从 **2698.794ms 降到 126.809ms（-95.30%）**。每组 10 次，新建 img 沙箱，首命令 `true`，无 strace/perf；四组全部成功、七项齐全、客户端计时闭合、删除最终为 HTTP 404。

补丁和完整运行方式见 [poc/README.md](poc/README.md)，全部细分阶段的 min/p50/p90/p95/max/avg 见 [poc/DISTRIBUTION.md](poc/DISTRIBUTION.md)，可机读汇总见 [examples/balloon-poc-20261103-summary.json](examples/balloon-poc-20261103-summary.json)。

## 七项必需耗时及端到端

单位 ms，下表为各组独立 p50；不能跨阶段相加。

| 阶段 | 原始 | 仅 CH range 合并 | CH + 256KiB 零页 | CH + 1MiB 零页 |
|---|---:|---:|---:|---:|
| e2e_ms | 3449.220 | 773.743 | 608.537 | 582.601 |
| create_ms | 4.777 | 4.821 | 5.174 | 4.752 |
| node_ctl_ms | 7.336 | 7.680 | 7.676 | 7.211 |
| sandbox_ctl_ms | 47.949 | 46.217 | 46.068 | 44.327 |
| kernel_init_ms | 447.824 | 432.825 | 333.519 | 296.510 |
| sandbox_init_ms | 257.206 | 66.419 | 59.995 | 65.480 |
| envd_ms | 41.237 | 24.686 | 23.621 | 22.951 |
| exec_ms | 2698.794 | 159.662 | 113.095 | 126.809 |

- `create_ms` 是异步 create 请求返回时间；不含等待 running。
- `node_ctl_ms` 是 admission + prepare + runner commit 的服务端 elapsed；不是 CPU 时间。
- `sandbox_ctl_ms` 只覆盖首条可见 ctl 日志 → CH started 的准备段。
- `kernel_init_ms` 是 CH started → Run /sbin/init，包含早期 VMM/VM 引导；不是纯内核 CPU 或解压耗时。当前 vmlinux 是 ELF，不应把这段直接归为“内核解压”。
- `sandbox_init_ms` 是 Run /sbin/init → host 收到 app_started，包含 guest 初始化和 host/guest 握手。
- `envd_ms` 是 mandatory /init RPC，包括连接和等待监听；不是 envd 进程自身启动时间。
- `exec_ms` 是观测 running 后 commands.run 调用 → 返回，包含 SDK、连接、RPC、guest fork/exec、流结束。

## 端到端和首命令完整分布

| 组 / 指标 | n | min | p50 | p90 | p95 | max | avg |
|---|---:|---:|---:|---:|---:|---:|---:|
| latest-baseline-10 / e2e_ms | 10 | 3211.771 | 3449.22 | 3627.524 | 3729.23 | 3830.936 | 3478.555 |
| latest-baseline-10 / exec_ms | 10 | 2380.088 | 2698.794 | 2909.067 | 2951.803 | 2994.54 | 2675.239 |
| latest-baseline-10 / kernel_init_ms | 10 | 432.425 | 447.824 | 485.726 | 500.96 | 516.194 | 455.785 |
| latest-candidate-10 / e2e_ms | 10 | 721.881 | 773.743 | 805.649 | 824.393 | 843.138 | 774.225 |
| latest-candidate-10 / exec_ms | 10 | 146.289 | 159.662 | 176.621 | 177.482 | 178.342 | 160.687 |
| latest-candidate-10 / kernel_init_ms | 10 | 416.334 | 432.825 | 438.84 | 439.835 | 440.83 | 431.782 |
| latest-zero256-10 / e2e_ms | 10 | 574.365 | 608.537 | 664.614 | 681.474 | 698.334 | 617.879 |
| latest-zero256-10 / exec_ms | 10 | 99.153 | 113.095 | 134.082 | 142.167 | 150.252 | 119.37 |
| latest-zero256-10 / kernel_init_ms | 10 | 322.822 | 333.519 | 343.803 | 350.137 | 356.471 | 333.515 |
| latest-zero1024-10 / e2e_ms | 10 | 509.628 | 582.601 | 610.333 | 640.177 | 670.021 | 584.527 |
| latest-zero1024-10 / exec_ms | 10 | 100.191 | 126.809 | 134.315 | 134.383 | 134.451 | 124.013 |
| latest-zero1024-10 / kernel_init_ms | 10 | 275.445 | 296.51 | 304.996 | 306.616 | 308.236 | 295.245 |

1MiB 相对“仅 CH”进一步降低端到端 p50 **24.70%**、CH→init p50 **31.49%**。256KiB 组首命令 p50 更低，1MiB 组端到端及 p95 更低；这 10 次不能证明所有工作负载下 1MiB 都最优，所以保留两份互斥的调参补丁。

## 第一项：CH inflate 连续 PFN 有界合并

[balloon-inflate-runs.patch](poc/balloon-inflate-runs.patch) 只修改 CH balloon inflate：同一描述符、同一 RAM region 内，按输入顺序合并相邻 PFN；支持前接/后接，最多 256 PFN（1MiB）。非连续、重复 PFN、跨 region 或达到上限都会拆开；描述符结束时 flush，release 完成后才 add_used。

仍执行 `MADV_DONTNEED → SEEK_DATA → 必要时 PUNCH_HOLE`。即使 memfd 已是 hole，也保留逻辑 REMOVE 通知。大于 4KiB 的 host page 保留原 PartiallyBalloonedPage 路径；不改启动预算、资源控制策略、UFFD gate 或快照格式。

仅改变 CH binary，就让首条 exec p50 **2699→160ms**、端到端 **3449→774ms**。这证明原初始 7GiB balloon 回收是首命令路径的重要干扰源；源码对应逐页系统调用和同步 UFFD REMOVE 往返。当前结果没有把这 2.5 秒进一步精确归到 installMu、reader 调度或 kernel 中某一个独占子段。

`fault_queue_wait_p50≈12µs` 只测事件排队到 worker 收到的窗口，不覆盖 reader 尚未读取的等待和后续 installMu 等待；不能拿它证明锁无影响，也不能读成 12ms。

### 确认没有靠少回收换延迟

使用 CH vm.info 每约 20ms 轮询，单独诊断，同样 target=7GiB、最终 actual=1GiB：

| 项目 | 原始 CH | range 合并 CH |
|---|---:|---:|
| create→actual 收敛 | 19203.790ms | 1236.703ms |
| 首次观测 CH Running→收敛 | 19066.332ms | 1079.888ms |
| 最终 memory_actual_size | 1073741824 | 1073741824 |
| balloon target | 7516192768 | 7516192768 |

这是每组 **1 次诊断**，采样周期和 RPC 会带来观测误差，不是分位数分布。另有每种预填策略 3 次的内存诊断均收敛成功。

bench 中的 REMOVE 中位数原始约 289538、CH 合并约 3492；各组在首命令完成后很快删除，实际完成回收的比例不同，**这两个数不能当作等量 7GiB 回收的准确调用削减倍率**。同样，UFFD 生命周期累计计数和累计时间不能当作启动路径中互斥、可相加的阶段。

## 第二项：只扩大冷启动 ZeroSource 的有界预填

[cold-zero-prefill-1m.patch](poc/cold-zero-prefill-1m.patch) 将权威冷启动 ZeroSource 的窗口由 64KiB 扩为 1MiB；[256KiB 版本](poc/cold-zero-prefill-256k.patch) 为较小窗口对照。两份补丁二选一，不能叠加。

urgent 4KiB 仍优先完成，剩余部分走原单个 best-effort tail worker。窗口同时受 RAM 大小、CH region 和 PageState 连续状态限制，提交 ioctl 前仍复核。普通 Data、恢复快照的 Hole/Zero、StateReleased 的 64KiB 策略保持不变。ZEROPAGE 不支持时，COPY fallback 的只读零缓冲同步扩到对应最大窗口。

仅扩大常量不够：必须同时修改 ZeroSource 的规划上限、tail 执行上限和 fallback scratch，并覆盖 RAM/region/state 边界，否则真实路径仍可能被另一个 64KiB 上限裁掉。

| 每次创建→首命令→删除的 UFFD 指标（中位数） | 原始预填，已合并 CH | 256KiB | 1MiB |
|---|---:|---:|---:|
| faults_absent | 4156.0 | 1110.0 | 348.0 |
| tail_submitted | 4106.5 | 1086.0 | 327.0 |
| pages_zeroed | 65036.0 | 67635.5 | 75209.0 |
| errors | 0.0 | 0.0 | 0.0 |

缺页次数减少与启动段缩短相符；pages_zeroed 增加说明存在额外预填，不能只看缺页数。

### 内存代价

每种策略 3 个独立诊断沙箱，CH 都为合并版。PSS 为 sandbox-ctl + CH 两个进程的合计，memfd allocation 通过 fd 的 st_blocks 计算并按 inode 去重。均为中位数，单位 MiB：

| 策略 | 首命令后 PSS | 收敛后 PSS | 首命令后 RAM memfd allocation | 收敛后 allocation |
|---|---:|---:|---:|---:|
| base | 347.826 | 348.361 | 252.887 | 252.887 |
| zero256 | 350.671 | 348.778 | 254.840 | 254.840 |
| zero1024 | 352.629 | 350.664 | 256.270 | 256.258 |

这些是两个时点的占用，**不是启动瞬时峰值或大并发容量保证**。进一步选择全局默认窗口前，还需覆盖峰值、不同 capacity/vCPU、并发启动和业务命令尾延迟；本次 PoC 不把 1MiB 当作已经充分验证的全局最优值。

## 正确性与清理

- CH release tests：显式包含 ignored 的真实 UFFD 测试，6 个通过。覆盖 run 连续性/重复/gap/region/上限，以及 resident/sparse 非零父源释放后归零、邻页内容不变。
- UFFD package：256KiB 和 1MiB 版本均 `go test ./pkg/uffd` 通过；最终 1MiB 版本 `go test -race ./pkg/uffd` 通过。新增测试覆盖冷启动窗口、RAM/状态/region 裁剪与 COPY fallback，原 restore/release/并发顺序回归继续运行。
- 最新 sandboxer + CH 合并 + 1MiB 预填组合，连续 3 轮 pause/resume 全部通过：同一 Node 进程 PID=74、随机 token 不变；32MiB 非零内存 SHA256 不变；8MiB 已 fsync 文件 SHA256 不变；恢复后定时器继续前进。
- 所有本轮 bench 和诊断沙箱最终确认 GET 404，再清理本地测试目录。实验使用独立 namespace、端口、网络和二进制目录，Nova 的服务与二进制没有替换。收尾已停止 balpoc 的服务和网络；复查无残留 unit/netns，Nova 三个服务仍 active，原 release sandbox-ctl 的 SHA256 与实验前一致（归档 cleanup-check.json）。

这证明所覆盖的组件语义和该 E2E 路径通过，不等于已覆盖所有 guest/host page size、并发压力与生产快照组合。

## 代码与运行版本核对

| 构件 | 来源 |
|---|---|
| sandboxer | `185f50b641ea84864bc8e0f7bab5c469100d45e3`，本轮 fetch 后的 main |
| accelerator | `6ecec94495c0a255479f18e6219e52b4eb2408e7`，支持此 sandboxer 所需 API |
| CH baseline | v51.1 + sandboxer 当前 12 个 native patches；应用后 HEAD `27ecfb2558b0ef684c239eee111d23cc3166ee64` |
| 原始 balloon.rs blob | `2cfed19d1db75304f121edde6717f956ee62736d` |
| node-ctl | 固定 release binary，source `b1feaf3bdb966dc40826e6a1a95e3ce3866c8d28` |
| kernel / guest bundle / image | 四组固定，来自本机 release-v0.1.10 / 同一个 img manifest |
| Connector Go 依赖 | 固定本地 `4c4ef74...` 工作区的 pkg/tapfd；该包包含未提交辅助文件，实际编译源码已归档并记录 SHA256，四组一致 |

这是“最新 sandboxer 上的受控实验”，不声称 node-ctl、guest bundle、镜像中 envd 都重新构建自各自最新 main。CH Cargo.lock 已随 PoC 保存；每组二进制完整 SHA256 见汇总 JSON。

发现并纠正过一个部署陷阱：node-ctl 若软链接到 release，os.Executable() 会定位 release 真路径，helper discovery 也会从 release 目录选 sandbox-ctl。修正为隔离目录里的真实 node-ctl 文件后，核对 /proc/PID/exe 与 SHA256，再重跑本报告四组；每组 10 个样本的 journal `_EXE` 都确认属于对应的私有 sandbox-ctl。早期未修正的 `baseline-10/candidate-10/zero256-pilot` 不纳入本报告。

## 剩余耗时与下一步

1. **CH→init 约 297ms** 仍是最大段。guest dmesg 的独立诊断显示，原预填策略 `Memory:` 在约 306ms、Run /sbin/init 在 379ms；1MiB 策略分别约 202ms、266ms。它们是 guest 内部时钟的单次里程碑，与 host 的 CH started 时钟不同；只能支持“早期内存准备路径有收益”，不能把相邻日志间隔直接当作某个内核函数的 CPU 时间。下一步针对 memmap/早期零页初始化补函数级测量，再考虑 kernel 配置或进一步预填；不凭这些日志修改安全缓解或扩大 guest 资源预算。
2. **exec 约 127ms**：继续拆 SDK 建连、proxy、envd Start、子进程 fork/exec 与流结束；目前不能把它全部算成 `true` 自身执行时间。
3. **sandbox-init 约 65ms，其中可见 app fork 段约 45ms**：适合继续为 launch handshake、Go reexec 和 namespace 建立补埋点，确认可重叠或可缓存的部分。此方向尚未实现。
4. **sandbox-ctl 准备约 44ms，其中 admit 前可见段约 30ms**；node-ctl 本身约 7ms，优先级低于前三项。

当前已完成两项可复核的有效 PoC。大并发与更广资源矩阵、函数级内核及 envd 独占时间仍是明确未完成的后续工作。

## 原始证据

当前机器的完整证据位于 `cold_start/results/balloon-poc-20261103/`（Git 忽略）：四组 samples、attempts、原始 journal、result、report、CSV、binaries，及收敛/内存/快照校验 JSON、测试日志、依赖源码副本和 SHA256SUMS.json。开发工作树、构建产物和隔离环境管理脚本另保留在 `/root/sandbox/balloon-poc-20261103/`。

本仓提交的是补丁、复现方法、固定 lockfile 和统计汇总；租户凭据、TLS 私钥和完整运行日志不入 Git。克隆后的新机器需按复现文档重新采集，不能仅凭汇总 JSON 重建原始 journal。
