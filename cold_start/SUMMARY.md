# 冷启动优化实验总结（2026-11-03）

后续范围约束与官方资料调研见 [COLD_ONLY_STRATEGIES.md](COLD_ONLY_STRATEGIES.md)：零页优化限定冷启动；当前 CH balloon 合并 PoC 仍作用于共享路径，严格隔离恢复需要补充冷启动开关。新候选仅完成内核机制验证。

两项代码修改与实验过程见 [OPTIMIZATION_PROCESS.md](OPTIMIZATION_PROCESS.md)。

已完成两项 PoC：**CH balloon 连续 PFN 合并释放**、**冷启动 ZeroSource 有界零页预填**。实现、实验和复现文档已提交到本地 commit `01941d2`。

## 实测结果

固定资源：2 vCPU、capacity=8GiB、allocatable=256MiB、startup=1GiB；每组 10 个新建 img 沙箱，首命令 `true`。以下为独立 p50，单位 ms，阶段之间存在包含或重叠，不能直接相加。

| 阶段 | 原始方案 | 仅 balloon 合并 | 合并 + 256KiB 预填 | 合并 + 1MiB 预填 |
|---|---:|---:|---:|---:|
| 异步 create | 4.777 | 4.821 | 5.174 | 4.752 |
| node-ctl | 7.336 | 7.680 | 7.676 | 7.211 |
| sandbox-ctl 准备 | 47.949 | 46.217 | 46.068 | 44.327 |
| CH→Run /sbin/init | 447.824 | 432.825 | 333.519 | 296.510 |
| sandbox-init | 257.206 | 66.419 | 59.995 | 65.480 |
| envd /init RPC | 41.237 | 24.686 | 23.621 | 22.951 |
| 首条 exec | 2698.794 | 159.662 | 113.095 | 126.809 |
| **端到端** | **3449.220** | **773.743** | **608.537** | **582.601** |

1MiB 组合方案相对原始方案，端到端下降 **83.11%**，首条 exec 下降 **95.30%**。256KiB 的首命令 p50 更低，1MiB 的端到端及 p95 更低；当前样本不能证明某个窗口在所有工作负载下最优。

## 优化机制

1. **balloon 合并**：在同一描述符、同一内存 region 内合并相邻 PFN，每段最多 1MiB；减少逐 4KiB 释放的系统调用和同步 UFFD 通知往返。保留 MADV_DONTNEED、逻辑 REMOVE、必要的 PUNCH_HOLE 及描述符完成顺序。
2. **零页预填**：只扩大权威冷启动 ZeroSource 的预填窗口，urgent 4KiB 仍优先，尾部沿用 best-effort worker。窗口受 RAM、region、PageState 限制；恢复快照及 Released 的原有 64KiB 策略保持不变。

首命令原有数秒延迟的重要干扰源是初始 7GiB balloon 回收。当前实验没有把全部延迟精确归到某一把锁。`fault_queue_wait_p50≈12µs` 不能读成 12ms，也不覆盖 reader 尚未读取或 worker 后续获取 installMu 的等待。

## 补充验证

- 四组共 **40 次 bench 全通过**，七项耗时齐全，客户端计时闭合，删除最终均确认 HTTP 404。
- 同样 target=7GiB、最终 actual=1GiB，完整回收收敛由 **19203.790ms 降到 1236.703ms**；这是每组一次独立诊断，不是分位数。
- 每种预填策略各 3 次内存诊断：1MiB 方案在首命令后及收敛时的进程合计 PSS，比原预填策略增加约 3–5MiB；尚未覆盖启动瞬时峰值。
- CH release 测试 **6 个通过**，包含真实 UFFD sparse/resident 释放归零和邻页内容校验。
- UFFD package 测试及最终 1MiB 版本的 **race 检查通过**。
- 组合方案连续 **3 轮 pause/resume 通过**：同一进程 PID/token、32MiB 非零内存 SHA256、8MiB fsync 文件 SHA256 保持一致，恢复后定时器继续前进。

## 数据范围与计时边界

sandboxer 固定为最新检查基线 `185f50b641ea84864bc8e0f7bab5c469100d45e3`，accelerator 为 `6ecec94495c0a255479f18e6219e52b4eb2408e7`。node-ctl、kernel、guest bundle 和 img 在四组之间固定，并非全部重建自各自最新 main。

已纠正 node-ctl 软链接导致 helper discovery 选中旧 release sandbox-ctl 的部署问题。上表只采用修正后重新采集的数据，并核对运行进程 SHA256 和各样本 journal 的 `_EXE`。

`CH→Run /sbin/init` 包含早期 VMM/VM 引导，不是纯内核 CPU 或解压时间；`envd` 是 /init RPC 窗口，不是进程自身启动；`exec` 包含 SDK、连接、RPC、guest fork/exec 和流结束。新建 img 不代表清空宿主机缓存。

实验使用独立 `balpoc` namespace，服务和网络已清理。Nova 三个服务仍 active，原 release sandbox-ctl 哈希与实验前一致。

## 后续优化重点

- **CH→init 约 297ms**：继续测量早期内存准备、memmap/零页初始化的函数级耗时。
- **exec 约 127ms**：拆分 SDK 建连、proxy、envd Start、fork/exec、流结束。
- **sandbox-init 约 65ms**：其中可见 app fork 段约 45ms，继续细分握手、Go reexec 和 namespace 建立。
- 扩大资源及并发矩阵，检查预填瞬时内存峰值和业务命令尾延迟，再决定默认窗口。

## 文档与证据入口

- [完整报告及验证边界](BALLOON_POC.md)
- [全部细分阶段 min/p50/p90/p95/max/avg](poc/DISTRIBUTION.md)
- [补丁、构建、bench、日志切分和回滚用法](poc/README.md)
- [可机读统计汇总](examples/balloon-poc-20261103-summary.json)
- 原始证据：`cold_start/results/balloon-poc-20261103/`，含逐样本计时、journal、测试日志及 SHA256SUMS.json，Git 忽略。
- 本机构建与实验管理目录：`/root/sandbox/balloon-poc-20261103/`。
