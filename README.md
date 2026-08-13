# ws-bench

Working-set B/W 快照延迟基准。B 是 drop_caches=true 的冷快照，W 是
drop_caches=false 并经过预热的工作集快照。

## 三种典型场景

| 场景 | 快照生成 | 基准探针 |
|---|---|---|
| openclaw + e2b（带 envd） | build_bw_coldboot.py PROFILE=e2b | node-proxy → health |
| openclaw（不带 e2b） | build_bw_coldboot.py PROFILE=bare | node-proxy → health |
| code-interpreter + e2b（带 envd） | build_bw_code_interpreter.py | node-proxy → commands.run("date") |

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

# 2. openclaw bare：node-proxy → health
PROFILE=bare IMAGE_NAME=openclaw python3 build_bw_coldboot.py
B="$(cat openclaw-bare-snp-template.id)"
W="$(cat openclaw-bare-ws-template.id)"
python3 bench.py --b "$B" --w "$W" --probe health -n 50

# 3. code-interpreter + e2b：node-proxy → envd commands.run
python3 build_bw_code_interpreter.py
B="$(cat code-interpreter-bare-snp-template.id)"
W="$(cat code-interpreter-e2b-ws-template.id)"
python3 bench.py --b "$B" --w "$W" --probe run --command date -n 10
~~~

场景 3 每轮内部校验 `commands.run("date")` 成功并返回 stdout；默认汇总模式不打印每轮成功明细，
只有失败轮次打印 SID 和错误。bench.py 也保留 --probe exec，用于已启用 gateway
HTTP exec 的 openclaw 镜像。

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
- code-interpreter 当前没有 code-interpreter bridge，因此 49999/execute 不在测试范围。

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
code-interpreter-bare-snp-template.id
code-interpreter-e2b-ws-template.id
~~~

其中 bare-snp 表示 B（冷、无 working set），e2b/bare 表示运行 profile；
W 文件保留对应 profile 名。code-interpreter 场景按约定使用：

~~~bash
B="$(cat code-interpreter-bare-snp-template.id)"
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
  → commands.run("date") 验证 envd
  → pause（drop_caches=true）→ export B
  → connect 恢复
  → commands.run("date") ×4 预热命令执行工作集
  → pause（drop_caches=false, merge_ref=false）→ export W
~~~

默认 date 只预热 envd 的命令执行路径（fork/exec/shell），不代表 Python 或
49999/execute 的工作集。需要测试 Python 路径时，可重新生成 W：

~~~bash
RUN_COMMAND="python3 -c 'print(1)'" WARM_RUNS=4 \
python3 build_bw_code_interpreter.py
~~~

当前 manifest 的实际状态是 envd :49983 可用，但没有 code-interpreter bridge，
因此 :49999/execute 和完整 run_code 不属于本基线。

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
- `bench.py` 默认不打印每轮成功明细，只输出客户端汇总表和独立的启动阶段汇总表；
  失败轮次只打印 SID 和错误。阶段按 `points` 的实际时间顺序统计，B/W 两侧平均都小于 3ms 的阶段不显示，
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

## code-interpreter + e2b 基线

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
| OUT_B / OUT_W | code-interpreter B/W ID 文件 | 可覆盖输出路径 |
| RUN_COMMAND | date | 创建验证和 W 预热命令 |
| WARM_RUNS | 4 | W 预热次数 |
| RUN_TIMEOUT | 60 | 单次命令超时秒数 |
| RUN_USER | root | guest 用户 |
| RUN_CWD | / | guest 工作目录 |

## 文件说明

| 文件 | 作用 |
|---|---|
| bench.py | 执行 B/W 基准，支持 health、exec、run 探针 |
| build_bw_coldboot.py | 生成 openclaw e2b/bare B/W 快照 |
| build_bw_code_interpreter.py | 从 manifest 生成 code-interpreter e2b B/W 快照 |
| openclaw_net.py | node-proxy 与 gateway 请求辅助 |
| e2b_env.py / e2b-env.sh | 环境变量加载与默认配置 |
| config/ | conductor、存储和 TLS 参考配置 |
