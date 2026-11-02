# 2026-11-03 img 冷启动实验复现

目的：复现 `create → running → envd 首命令 true` 的延迟，使用相同阶段口径比较启动预算变化。三个组均使用同一 openclaw manifest、相同内核/runtime 和 2 vCPU。

## 环境

- 在 Nova 节点的完整 semerge checkout 运行。live bench 复用仓库根的 `bench_coldstart_envd.py`、`bench.py`、`e2b_env.py`，不是只复制本目录即可运行的独立 SDK。
- 实测部署路径为 `/root/sandbox/nova-work/release-v0.1.10/`，node-ctl build revision `b1feaf3bdb966dc40826e6a1a95e3ce3866c8d28`，sandbox-ctl/sandbox-init revision `3fa03fc7267b5d6c53040592fffa05b9c2220925`，guest envd `0.6.1`。
- tenant：`E2B_ENV_FILE=/root/sandbox/nova-work/etc/node-ctl-nova/tenant.env`。该文件含凭据，不提交 Git。
- 默认 manifest KEY：`7e78b050c65691f01b3aed6eb593f00f2286b7c08aa4dd84e3b1fd823a66ba20`，需要 manifest/cache/store 服务及相应镜像数据可用。
- 原实验双盘为 vda 只读 base、vdb 32GiB 可写 diff。首命令使用 root、cwd=/。脚本不启动 OpenClaw gateway。
- CPU 容量继承节点配置；本实验为 2 vCPU。重部署后应核对 CH args、内核/runtime 与资源配置，不把历史数字当成当前保证。

## 固定参数重跑三个组

从 semerge 根目录执行。输出目录必须不存在；重跑使用新的目录名称。

```bash
export E2B_ENV_FILE=/root/sandbox/nova-work/etc/node-ctl-nova/tenant.env

# A：默认条件，初始 balloon 目标为 7GiB
python3 cold_start/bench.py -n 10 --label default \
  --capacity-memory 8GiB --allocatable-memory 256MiB --startup-memory 1GiB \
  --out cold_start/results/default

# B：只改变 startup；仍保留 balloon 设备，初始目标为 0
python3 cold_start/bench.py -n 10 --label startup8 \
  --capacity-memory 8GiB --allocatable-memory 256MiB --startup-memory 8GiB \
  --compare cold_start/results/default/result.json \
  --out cold_start/results/startup8

# C：capacity/allocatable/startup 均为 8GiB，CH 不带 balloon 设备
python3 cold_start/bench.py -n 10 --label full8 \
  --capacity-memory 8GiB --allocatable-memory 8GiB --startup-memory 8GiB \
  --compare cold_start/results/default/result.json \
  --out cold_start/results/full8
```

资源 metadata 只作用于本轮新建沙箱，不修改全局节点配置。startup=8GiB 增加 admission 预算预留，不能因实际 RSS 较小就忽略并发密度代价。

## 历史实测，仅作为核对方向

首轮独立资源矩阵，每组三次：

| 组 | 首命令 p50/ms | e2e p50/ms | 生命周期 REMOVE 次数/样本 |
|---|---:|---:|---:|
| default | 2695.003 | 3446.124 | 261378–283394 |
| startup8 | 195.486 | 801.051 | 16386 |
| full8 | 81.586 | 694.814 | 2 |

另一次使用本目录 bench 逻辑的 3+3 对照测得 default exec/e2e 为 2981.248/3714.417ms，startup8 为 200.976/852.860ms。两次独立运行不能混成一个分位数统计。

七个阶段的原始 n=10 默认基线汇总在 `examples/default-10-summary.json`，资源矩阵汇总在 `examples/resource-comparison.json`。原始大日志不进入 Git；每次 live run 自动保存自己的原始证据，供其他 agent 独立复核。

## 离线切分与检查

```bash
python3 cold_start/split_logs.py \
  --input cold_start/results/startup8 \
  --compare cold_start/results/default/result.json \
  --out cold_start/results/startup8-recomputed
```

目录输入直接使用归档的 `phases_ms` 和 journal JSON，不访问宿主日志或创建新沙箱；可以复制结果目录到其他机器离线复核。七项起止点、退出码、旧 exec 口径处理见 [README](README.md)。

检查 `result.json`：fail=0、incomplete_samples 为空、invalid_timing_samples 为空。检查每个 `samples.jsonl` 的 cleanup.ok=true、http_status=404；失败清理时保留目录。细分数据中父子区间有包含关系，不能把所有 p50 相加。

## 已证明与未证明

- 已测到保持容量和 allocatable 不变，只改 startup 会显著降低 exec 与 e2e；启动期大 balloon 回收路径是优先调查方向。
- UFFD `fault_queue_wait_p50` 原始单位为 ns，12045ns 是约 12µs，不是 12ms。REMOVE 数量大，不等价于已证明哪把锁贡献了多少等待。
- 独立 guest 采样观察到 bash 的 `filemap_fault → folio_wait_bit_common` 等待；低块读带宽不能排除文件页读取路径。
- 顺序预读 shell/库虽然使后续 exec 更快，但包含预读的 e2e 没有改善；不能只移动计时窗口后声称优化成功。
- 未测出纯内核解压、envd 进程自身启动，以及 REMOVE/锁竞争/完整块 IO 的独占耗时。原文“零页/CoW 为主”“warmup 直接将 e2e 降到 900ms”等不作为已验证结论。
