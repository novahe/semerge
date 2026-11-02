# 冷启动各级耗时：采集、离线重算和优化前后对照

`cold_start/bench.py` 在 Nova 节点上直接创建真实 img 冷启动沙箱，等待 running，执行首条 `true`，删除并确认 GET 404 后，自动汇总七个必需阶段。每个样本是新沙箱，不先运行命令预热。测量对象为 envd 首命令，不包括 OpenClaw gateway 就绪。

目录结构：

```text
cold_start/
  bench.py          # 实机 bench 入口
  split_logs.py     # 已有日志离线切分入口，不创建沙箱
  stages.py         # 共用的切分、统计、比较逻辑
  test_stages.py    # 单位/计时边界/缺测/日志归属测试
  README.md
  results/          # 默认结果目录，不提交 Git
```

复现三组资源配置、历史实测和未完成边界见 [EXPERIMENT.md](EXPERIMENT.md)；可核对的历史汇总在 `examples/`。

已完成的 balloon/零页预填 PoC、四组各 10 次分布和验证结果见 [BALLOON_POC.md](BALLOON_POC.md)，补丁与复现命令见 [poc/README.md](poc/README.md)。源码分析与正确性约束见 [BALLOON_OPTIMIZATION.md](BALLOON_OPTIMIZATION.md)。

脚本沿用 `bench_coldstart_envd.py` 的环境加载器和 manifest 模板解析。默认使用 Nova tenant.env；如需其他租户，显式设置 `E2B_ENV_FILE`。仅本进程将租户域名加入大小写 no_proxy，避免 requests 状态轮询走外部代理。需要能读取节点的 conductor.log 和 journal。

## 一次运行拿到七项耗时

```bash
cd /root/sandbox/semerge
python3 cold_start/bench.py -n 10 --label baseline \
  --out cold_start/results/baseline
```

默认 openclaw manifest、2 vCPU、资源配置继承节点默认值；不修改节点配置。`--template` 可传 manifest KEY、完整 img 模板 ID 或 `.id` 文件；不接受 snp 冒充冷启动。

| 字段 | 单位 | 计时口径 |
|---|---|---|
| create_ms | ms | 客户端调用异步 create → SDK 收到响应，不等待启动完成 |
| node_ctl_ms | ms | 服务端 admission + prepare + runner commit 的 elapsed time，不是 CPU 时间 |
| sandbox_ctl_ms | ms | sandbox-ctl 首条可见日志 → CH started，仅可见启动准备段，不是整个进程生命周期 |
| kernel_init_ms | ms | CH started → Run /sbin/init，包含 VM 早期引导，不是纯内核初始化/解压时间 |
| sandbox_init_ms | ms | Run /sbin/init → host 收到 app_started，包括 guest 初始化和握手 |
| envd_ms | ms | mandatory /init RPC 阶段，包括连接、等待监听、重试，不是 envd 进程自身启动时间 |
| exec_ms | ms | 观测 running 后调用 commands.run → 返回，含 SDK、RPC、guest 子进程和流结束 |

额外输出 e2e、launch_wait、服务端总时长，以及 node-ctl 子阶段、sandbox-ctl 准备步骤、CH→UFFD 注册里程碑、console 可见尾段、guest 的挂载/overlay/网络/app fork 等细分区间。

`console_tail_ms` 是 `kernel_init_ms` 的子区间。首条可见 console 可能已是 virtio-blk 初始化消息，不能把尾段当作完整内核初始化。`guest_vda_to_vdb_ms` 是两个 mounted 日志之间的间隔，不能当作精确的 ext4 mount 函数耗时。

日志阶段使用 journald 的微秒时间戳，换算为 ms；亚毫秒间隔受日志交付扰动，不能据此声称某个函数只有几十微秒。独立分段的 p50 不可相加。create 与 admission 重叠；runtime_ready 是 ctl/VM/guest 的上层等待窗口。

## 直接验证优化

只对本轮新建沙箱覆盖 startup，保持 capacity、allocatable 继承默认配置：

```bash
python3 cold_start/bench.py -n 10 --label startup8 \
  --startup-memory 8GiB \
  --compare cold_start/results/baseline/result.json \
  --out cold_start/results/startup8
```

也支持 `--capacity-memory`、`--allocatable-memory`，均通过请求 metadata 覆盖本轮测试沙箱。startup=capacity 会增加 admission 预算预留，影响并发容量；缩小 capacity 会改变 guest 可见内存，比较时须注明这些差异。

对照报告逐阶段列 before/after 的 p50、变化量和变化率；负数代表更快，同时保存每组样本数。模板或命令不同会拒绝生成比较。应同时看 `e2e_ms` 和 `exec_ms`，防止只是将首命令耗时移到启动/预热阶段。

## 输出与退出码

每个输出目录必须是新目录，防止混入旧样本：

- `report.md`：全部阶段 n/min/p50/p90/p95/max/avg、口径、缺测边界和 before/after。
- `stages.csv`：同样的阶段统计，方便画图和持续跟踪。
- `result.json`：schema、单位、参数、资源覆盖、脚本 SHA256、阶段统计和比较。
- `samples.jsonl`：逐样本计时、日志锚点、原始服务端阶段、缺测项、清理结果。
- `attempts.jsonl`：每次 create/exec/cleanup 的原始结果，在解析证据前落盘。
- `journal-<i>.jsonl`：仅该样本 sandbox-ctl/console 的完整原始 JSON，按时间排序，按 PID 和 boot ID 过滤；保留完整秒内尾部。

退出码：`0` 表示样本成功且七项齐全；`1` 表示创建、命令或清理失败；`2` 表示样本成功但有缺测或客户端计时闭合差超过 1ms（参数错误也返回 2）。计时不闭合的样本不参与统计。缺失阶段显示为缺测，不以 0 代替。未确认删除收敛到 404 时保留本地目录，避免破坏仍运行的沙箱。

UFFD 时间在 `uffd_lifetime_time_ms` 单独展示。原始 `fault_queue_wait_p50` 的单位是 ns，例如 12045ns=0.012045ms，不能误读为 12ms；这些是生命周期累计时间或单沙箱分位数，不是可相加的独占阶段。REMOVE、madvise 等原始计数保存在逐样本数据里。

分位数使用排序后的线性插值。`-n 10` 时，p95 位于第 9、10 个有序样本之间，是这十次观测的描述统计，不代表稳定的大样本尾延迟。JSON、CSV 和 Markdown 采用相同算法，`avg` 保留。

## 离线重算，不重新创建沙箱

新 bench 结果目录包含原始 journal 和服务端阶段归档，可以直接离线切分，不依赖节点 journal、conductor.log、SDK 或凭据：

```bash
python3 cold_start/split_logs.py \
  --input cold_start/results/baseline \
  --out cold_start/results/recomputed
```

也可以在优化后重新切分并与旧结果比较：

```bash
python3 cold_start/split_logs.py \
  --input cold_start/results/startup8 \
  --compare cold_start/results/baseline/result.json \
  --out cold_start/results/recomputed-startup8
```

旧日志需要 samples.jsonl、原始 journal JSONL 和 conductor.log，不能从截断的文本日志猜测：

```bash
python3 cold_start/split_logs.py \
  --samples /path/to/samples.jsonl \
  --journal-json /path/to/raw-journal.jsonl \
  --conductor-log /path/to/conductor.log \
  --out cold_start/results/recomputed-legacy
```

2026-11-03 早期 bench 将 launch_wait 混入 exec_ms 的旧数据，必须显式加 `--legacy-exec-includes-launch-wait`；新脚本不混入。也可用 `--phase-json` 读取归档的 `[{"sid":...,"phase":...,"ms":...}]` 服务端阶段。

该脚本不能独立切出纯内核执行、envd 进程自身启动、各级 REMOVE/锁竞争的完整独占时间；这些区间需要新增 runtime 埋点或 profiler。报告明确列出这些未测部分。

验证：

```bash
python3 -m unittest discover -s cold_start -p 'test_*.py' -v
```
