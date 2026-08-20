# ws-bench

Working-set B/W 快照延迟基准。B 是 drop_caches=true 的冷快照，W 是
drop_caches=false 并经过预热的工作集快照。

## 三种典型场景

| 场景 | 快照生成 | 基准探针 |
|---|---|---|
| openclaw + e2b（带 envd） | build_bw_coldboot.py PROFILE=e2b | node-proxy → health |
| openclaw（不带 e2b） | build_bw_coldboot.py PROFILE=bare | node-proxy → health |
| code-interpreter + e2b（带 envd） | build_bw_code_interpreter.py | CI SDK → 49999/execute → Jupyter |

前置条件：conductor :4443、vswitch、cache-ctl、store-ctl 已启动，并确认
e2b-env.sh 中的路径、TLS 证书和 API 配置正确。若使用 nova 部署环境：

~~~bash
export E2B_ENV_FILE=/root/sandbox/nova-work/etc/node-ctl-nova/tenant.env
~~~

## node-ctl / sandboxer 修改补丁

当前计时链路需要同时更新 sandboxer 和 orchestrator/node-ctl。对应 patch 放在
`patches/`，只包含本次 readiness timing、launchTiming 和 NS cgroup unit prefix
相关修改，不包含构建产物，也不包含 orchestrator 中无关的 `docs/`、`release-builder/`
文件。

应用到干净的对应仓库基线：

~~~bash
WS_BENCH=/root/sandbox/ws-bench

cd /root/sandbox/sandboxer
git apply --check "$WS_BENCH/patches/sandboxer-readiness-timing.patch"
git apply "$WS_BENCH/patches/sandboxer-readiness-timing.patch"
env GOCACHE=/tmp/e2b-go-build-cache go test ./pkg/sandbox ./pkg/restore ./cmd/sandbox-ctl

cd /root/sandbox/orchestrator
git apply --check "$WS_BENCH/patches/orchestrator-node-ctl-timing-isolation.patch"
git apply "$WS_BENCH/patches/orchestrator-node-ctl-timing-isolation.patch"
env GOCACHE=/tmp/e2b-go-build-cache go test ./internal/config ./internal/nodectl ./internal/orch \
  ./internal/configsock ./internal/api ./cmd/node-ctl
~~~

补丁应用后需要重新构建 `sandbox-ctl` 和 `node-ctl`，再执行 nova 的
`deploy-ns.sh replace`；只替换 node-ctl 不会让旧 sandboxer 进程获得新的 readiness
时间点。部署验证仍使用本 README 的 `bench.py --probe health` 命令。

补丁文件：

- `patches/sandboxer-readiness-timing.patch`：sandbox-ctl 记录 CH 启动、resume 返回、sandbox-init ACK，并通过 readiness wire 传给 node-ctl。
- `patches/orchestrator-node-ctl-timing-isolation.patch`：node-ctl 暴露 launchTiming、读取 readiness 时间点，并按 NS 的 systemd runner unit prefix 隔离资源扫描。

## 快速运行

~~~bash
cd /root/sandbox/ws-bench

# 1. openclaw + e2b：node-proxy → health
PROFILE=e2b IMAGE_NAME=openclaw python3 build_bw_coldboot.py
B="$(cat openclaw-e2b-bare-snp-template.id)"
W="$(cat openclaw-e2b-ws-template.id)"
python3 bench.py --b "$B" --w "$W" --probe health -n 50

# 多档并发：每个并发档位各执行 50 个 B/W 样本，并分别输出汇总
python3 bench.py --b "$B" --w "$W" --probe health -n 50 -c 1 10 30 50

# 分轮并发：每档 5 轮；每轮同时启动该档位数量，轮间等待全部完成
python3 bench.py --b "$B" --w "$W" --probe health -r 5 -c 1 10 30 50

# 2. openclaw bare：node-proxy → health
PROFILE=bare IMAGE_NAME=openclaw python3 build_bw_coldboot.py
B="$(cat openclaw-bare-snp-template.id)"
W="$(cat openclaw-bare-ws-template.id)"
python3 bench.py --b "$B" --w "$W" --probe health -n 50

# 3. code-interpreter + e2b：49999/execute → Jupyter
python3 build_bw_code_interpreter.py
B="$(cat code-interpreter-e2b-snp-template.id)"
W="$(cat code-interpreter-e2b-ws-template.id)"
python3 bench.py --b "$B" --w "$W" --probe code -n 10
~~~

场景 3 每轮内部校验 `run_code` 成功并返回 stdout。bench.py 也保留 --probe exec，
用于已启用 gateway HTTP exec 的 openclaw 镜像。

`-c/--concurrency` 可接收一个或多个正整数，默认 `1`。旧模式下，`-n/--iterations` 表示每个
并发档位、每个已选择模板的总样本数，而不是每个 worker 的样本数；例如 `-n 50 -c 1 10 30 50`
总计分别执行 4 × 50 个 B 样本和 4 × 50 个 W 样本。

`-r/--rounds` 是严格分轮模式，与 `-n` 互斥。每轮启动恰好 `concurrency` 个样本，等待该批
全部结束后再开始下一轮。例如 `-r 5 -c 1 10 30 50` 对每个模板分别尝试
`5 × (1 + 10 + 30 + 50) = 455` 个样本；`c=1` 的 5 轮就是 5 次串行测试。失败样本仍会丢弃，
并计入 `discarded` 和失败 JSONL 日志，不会补偿重试。

每档默认只打印 attempted/accepted/discarded 计数；加 `-v/--verbose` 后才打印每个成功 sandbox 的
SID 与 e2e/create-api/launch/cold/warm 耗时。失败样本无论是否加 `-v` 都会立即打印 SID 和错误；最后的并发趋势
表对比各档的总耗时和启动阶段耗时相对基线的变化。任一 create、cold probe、launch timing 或
warm probe 失败时，整条样本从所有统计中丢弃；关键信息默认追加保存到
`logs/bench-failures.jsonl`，可用 `--failure-log PATH` 修改路径。

只有选择两个或以上并发档位时才打印并发趋势表；默认的 `-c 1` 或显式单档 `-c 1` 只输出该档
汇总，不做无意义的自身基线比较。B/W 的 cold 和 create 原始指标保留在汇总表中。

## 镜像基线

镜像元信息由 manifest-ctl/flatten-ctl info 校对；openclaw 镜像在 e2b 和 bare 场景中复用，
code-interpreter 只跑 e2b 场景。场景差异是启动方式和 probe，不是对应镜像的 rootfs。

| 场景 | 镜像 | manifest key | 版本/说明 | EROFS 大小 | CDC chunk |
|---|---|---|---|---:|---:|
| openclaw + e2b | openclaw | 65c3599e5f52264c63549b5069cf313065a6255fad1c9190d5b55a3c809bfa3f | OpenClaw 2026.7.1，Node 24.16.0，exec3 镜像 | 1.2 GiB | 2186 |
| openclaw bare | openclaw | 65c3599e5f52264c63549b5069cf313065a6255fad1c9190d5b55a3c809bfa3f | OpenClaw 2026.7.1，Node 24.16.0，exec3 镜像 | 1.2 GiB | 2186 |
| code-interpreter + e2b | code-interpreter rootfs | 8aeffe688f1061d0826a9ec3d79b911a230bf0968c3648c8a403198fd7b3270a | Ubuntu 24.04.1，envd 0.6.1，Python 3.12.8 + Jupyter 依赖 | 4.7 GiB | 8104 |

chunk 补充信息：

- 两个 manifest 都是 CDC 分块，零 chunk 和 holes 均为 0。
- openclaw 平均 chunk 约 596 KiB；code-interpreter 平均 chunk 约 611.5 KiB。
- code-interpreter 上传记录为 8060 个存储数据块、44 个去重块、0 个零块；8104
  是 manifest 的逻辑 chunk 总数。
- code-interpreter 通过 e2b profile 暴露 envd :49983 和 bridge :49999；构建脚本只启动
  镜像自带 bridge，并在导出 B/W 前验证 `/health` 和一次有输出的 `run_code`。

## Template ID 命名

快照 ID 文件使用镜像名加场景/快照类型命名。openclaw 的规则是：

~~~text
<镜像名>-e2b-bare-snp-template.id
<镜像名>-e2b-ws-template.id
<镜像名>-bare-snp-template.id
<镜像名>-bare-ws-template.id
~~~

当前文件示例：

~~~text
openclaw-e2b-bare-snp-template.id
openclaw-e2b-ws-template.id
openclaw-bare-snp-template.id
openclaw-bare-ws-template.id
code-interpreter-e2b-snp-template.id
code-interpreter-e2b-ws-template.id
~~~

其中 snp 表示 B（冷、无 working set），e2b/bare 表示运行 profile；
W 文件保留对应 profile 名。code-interpreter 场景按约定使用：

~~~bash
B="$(cat code-interpreter-e2b-snp-template.id)"
W="$(cat code-interpreter-e2b-ws-template.id)"
~~~

## 快照生成流程

### openclaw

build_bw_coldboot.py 的流程：

~~~text
启动 gateway
  → gateway /health ready
  → pause 默认策略（drop_caches=true）→ export B
  → connect 恢复
  → node-proxy health + exec 预热 ×4
  → pause（drop_caches=false, merge_ref=false）→ export W
~~~

PROFILE=bare 时 gateway 通过 create metadata 的 launch spec 启动；PROFILE=e2b
时通过 envd `commands.run(background=True)` 启动长驻 gateway，并立即用 node-proxy
`/health` 校验，不等待长驻进程的前台输出流。toTemplate 导出不会删除源 sandbox，
流程最后由脚本显式 DELETE 清理。

### code-interpreter + e2b

使用 yyl 上传的 manifest rootfs，默认 manifest key：

~~~text
8aeffe688f1061d0826a9ec3d79b911a230bf0968c3648c8a403198fd7b3270a
~~~

流程为：

~~~text
create manifest template
  → envd commands.run(background) 以镜像原生 `E2B_LOCAL=true` 模式启动自带 start-up.sh
  → 49999/health + run_code 有输出门禁
  → pause（drop_caches=true）→ export B
  → connect 恢复
  → 49999/health + run_code ×4 预热 Python/Jupyter 工作集
  → pause（drop_caches=false, merge_ref=false）→ export W
~~~

构建流程不修改 guest 文件。`BRIDGE_READY_TIMEOUT` 默认等待 120 秒；快照前如果
49999 不 ready 或 `run_code` 没有 stdout/result，构建会失败，不会导出坏快照。
`E2B_LOCAL=true` 是官方 bridge 的本地模式，代码执行正常，但不会从 EnvD 注入环境变量。
需要改预热代码时：

~~~bash
RUN_CODE="print(1)" WARM_RUNS=4 \
python3 build_bw_code_interpreter.py
~~~

## 计时口径

每轮流程：

~~~text
create → 立即第一次 probe → 读取 launchMs → 第二次 probe → delete
~~~

- create(launchMs)：conductor 服务端 acceptedAt → launch terminal 的耗时，不是客户端 HTTP create wall time。
- 非 e2b 场景：包含 runtime readiness、用户进程 app_started 确认和状态提交。
- e2b 场景：在上述基础上还包含 envd /init 初始化。
- e2e1：从客户端发起 create，到 create 返回后立即执行的第一次 probe 返回的端到端耗时；
  中间不插入 `wait_launch_ms` 轮询。
- run1/cold = e2e1 - create(launchMs)。
- run2/warm：只统计第二次 probe 本身。
- p50：严格中位数；偶数样本取两个中间值的平均，输出保留 1 位小数。

### launchTiming 与原始日志校对

`GET /api/v1/sandboxes/<sid>` 在 `launchMs` 旁返回 `launchTiming`。其中：

- `points` 是 node-ctl 与 sandbox-ctl 的 RFC3339Nano 时间点；
- `phasesMs` 是阶段差值，e2b 快照包含 `ch_start_to_resume`、
  `resume_to_sandbox_init_ack`、`sandbox_init_ack_to_runtime_ready` 和
  `runtime_ready_to_envd_init_done`；bare 没有 `envd` 阶段；
- `accountedMs + unaccountedMs` 应等于 `launchMs`（只允许微小的取整误差）。
- health 场景使用 host 侧 `curl` 经 `node-proxy` 转发到 OpenClaw 的 `/health`，
  不是直连 floating IP，也不是 SDK `commands.run`。
- `bench.py` 默认不打印每轮成功明细，只输出客户端汇总表、独立的启动阶段汇总表和样本计数；
  `-v/--verbose` 才打印每个成功沙箱的逐样本明细，失败轮次无论是否加 `-v` 都打印 SID 和错误。
  阶段按 `points` 的实际时间顺序统计，B/W 两侧平均都小于 3ms 的阶段不显示，
  但不会从 `accountedMs` 中删除；`unaccountedMs` 仅用于内部完整性校对，不放入阶段表。
- B/W 运行期间只显示单行原地更新的进度条，不输出成功轮次的明细。
- 汇总表的列头统一为 `avg(ms)`、`p50(ms)`、`min(ms)`、`max(ms)`、`n`，B/W 分组下只显示数值。
- 客户端汇总顺序为 `health1 e2e`、`create-api`、`create(launchMs)`、首 probe cold 和第二次 warm；
  `create-api` 是客户端 POST create 到收到 201 的墙钟耗时。

阶段含义：

| 阶段 | 时间边界 |
|---|---|
| `admission_to_node_prepare` | conductor 接收 create → node-ctl 完成节点准备 |
| `sandbox_ctl_dispatch` | runner 接手 → sandbox-ctl 开始处理 |
| `sandbox_ctl_pre_ch` | sandbox-ctl 开始处理 → Cloud Hypervisor 启动 |
| `ch_start_to_resume` | Cloud Hypervisor 启动 → resume 接口返回 |
| `resume_to_sandbox_init_ack` | resume 返回 → sandbox-init 初始化完成并 ACK |
| `sandbox_init_ack_to_runtime_ready` | sandbox-init ACK → node-ctl 收到 runtime ready |
| `runtime_ready_to_envd_init_done` | node-ctl 收到 runtime ready → envd `/init` 完成，仅 e2b |
| `envd_init_to_launch_done` | envd `/init` 完成 → launch terminal/state running，仅 e2b |
| `accountedMs` | 已纳入阶段统计的耗时总和，应接近 `create(launchMs)` |

bare 场景没有 envd 阶段，使用 `ch_start_to_runtime_ready` 和
`runtime_ready_to_launch_done` 两段替代 e2b 的 runtime/envd 拆分。

ready ack 的原始 wire 同时写入 sandbox-ctl 日志，node-ctl 的
`sandbox launch terminal` 日志写入完整 `launch_timing`。因此一轮测试完成后，先保存
`<NS>-work/var/lib/logs/`，再按 sandbox ID 搜索两边日志，检查 CH 启动、resume、
restore ack、runtime ready、envd init 和 launch terminal 的时间顺序；不能只看
bench 的聚合结果。

## code-interpreter + e2b 基线（2026-08-20）

当前基线使用官方 CI guest image，构建过程不修改 guest 文件、不使用
`patch_bridge`，而是通过官方 `/root/.jupyter/start-up.sh` 的 `E2B_LOCAL=true`
模式启动 bridge。B/W 各执行 c=1、30、50 一波，共 81 个样本；B/W 均
`81/81` 成功，无 discarded，首个 `run_code` 均返回正确 stdout。

| 并发 | B code1 e2e avg / p50 | W code1 e2e avg / p50 |
|---:|---:|---:|
| 1 | 374.8 / 374.8 ms | 293.5 / 293.5 ms |
| 30 | 710.0 / 714.5 ms | 631.9 / 628.4 ms |
| 50 | 953.5 / 958.8 ms | 803.8 / 811.3 ms |

完整的分阶段结果、模板校验值和复现命令见
[`docs/envd-code-interpreter-baseline-2026-08-20.md`](docs/envd-code-interpreter-baseline-2026-08-20.md)。

`c=1` 只有一个样本，仅作单发锚点；c=30/50 的结果包含并发下的 create、launch、
EnvD init 和首次 Jupyter 执行排队，不能外推为通用性能结论。`E2B_LOCAL=true` 下
本次代码执行路径正常，但 EnvD guest 环境变量注入不在该基线覆盖范围内。

### 历史 commands.run 基线（2026-08-13）

2026-08-13（Asia/Shanghai），nova 环境，当前 B/W template，
commands.run("date")，B/W 各 30 轮；所有成功样本均返回 date stdout。
统计口径是 create 后立即执行首个 probe，未在 e2e1 前插入 `wait_launch_ms` 轮询。

30 轮严格统计：

| 指标 | B | W |
|---|---:|---:|
| create(launchMs) | avg 168ms，p50 122.4ms，min 91，max 277 | avg 145ms，p50 131.2ms，min 94，max 211 |
| run1 e2e | avg 860ms，p50 949.2ms，min 564，max 1122 | avg 172ms，p50 158.9ms，min 123，max 237 |
| run1 cold | avg 692ms，p50 783.9ms，min 433，max 924 | avg 27ms，p50 26.6ms，min 25，max 31 |
| run2 warm | avg 9ms，p50 8.4ms，min 8，max 10 | avg 8ms，p50 8.1ms，min 7，max 9 |

结论：run1 cold 平均提速 25.56×（692ms → 27ms），create 差异为
+23ms（B-W）。

## 配置参数

build_bw_coldboot.py：

| 变量 | 默认值 | 说明 |
|---|---|---|
| PROFILE | e2b | e2b 或 bare |
| IMAGE_NAME | openclaw | 影响 B/W ID 文件名 |
| OUT_B / OUT_W | 按 profile 生成 | 可覆盖输出文件路径 |
| READY_SETTLE | 2 | B 导出前的 gateway 稳定等待秒数 |
| WARM_CURLS | 4 | W 的 health + exec 预热次数 |
| SANDBOX_TIMEOUT | 600 | 沙箱超时秒数 |

build_bw_code_interpreter.py：

| 变量 | 默认值 | 说明 |
|---|---|---|
| MANIFEST_KEY | 上述 key | manifest rootfs 清单密钥 |
| OUT_B / OUT_W | code-interpreter-e2b B/W ID 文件 | 可覆盖输出路径 |
| RUN_CODE | `print('hello kuasar-sandbox {index}')` | 创建验证和 W 预热 Python 代码；内置默认值每次以不同执行标识替换 `{index}` |
| WARM_RUNS | 4 | W 预热次数 |
| RUN_TIMEOUT | 60 | 单次代码执行超时秒数 |
| BRIDGE_READY_TIMEOUT | 120 | 49999/health readiness 等待秒数 |

## 文件说明

| 文件 | 作用 |
|---|---|
| bench.py | 执行 B/W 基准，支持 health、exec、run、code 探针 |
| build_bw_coldboot.py | 生成 openclaw e2b/bare B/W 快照 |
| build_bw_code_interpreter.py | 从 manifest 生成 code-interpreter e2b B/W 快照 |
| openclaw_net.py | node-proxy 与 gateway 请求辅助 |
| e2b_env.py / e2b-env.sh | 环境变量加载与默认配置 |
| config/ | conductor、存储和 TLS 参考配置 |
