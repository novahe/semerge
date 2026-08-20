# EnvD + code-interpreter 并发基线（2026-08-20，normal image）

本文首部是 2026-08-20 重新生成的当前基线：使用官方 CI guest image，**不修改
guest 文件、不使用 `patch_bridge`**，通过官方 `start-up.sh` 的
`E2B_LOCAL=true` 模式启动 bridge。文末保留此前兼容性 patch 版本的数据，便于比较，
不应将两组数据混为同一条时间序列。

## 当前结论

在当前 Nova 部署、`e2b` profile、code-interpreter `run_code` 路径下，B/W 各完成
一轮 `c=1/30/50` 的并发基线，**全部成功**：B `81/81`、W `81/81`，无 discarded
样本，所有首个 `run_code` 均返回预期的 `hello kuasar-sandbox ...` 输出。

首次代码执行的端到端 `code1 e2e` 随并发升高明显变慢；在这次 normal-image
构建中，W 在三个档位均快于 B：

| 并发 | B code1 e2e avg / p50 | W code1 e2e avg / p50 | W 相对 B（avg） |
| ---: | ---: | ---: | ---: |
| 1 | 374.8 / 374.8 ms | 293.5 / 293.5 ms | -81.3 ms (-21.7%) |
| 30 | 710.0 / 714.5 ms | 631.9 / 628.4 ms | -78.1 ms (-11.0%) |
| 50 | 953.5 / 958.8 ms | 803.8 / 811.3 ms | -149.7 ms (-15.7%) |

这是部署、镜像、模板和当次并发波次的基线，不能外推为通用性能结论。`c=1`
只有一个样本，用作单发锚点，不用于尾延迟判断。

## 当前场景与方法

### 工作负载

- 模板：`code-interpreter-e2b-snp-template.id`（B，`drop_caches=true`）和
  `code-interpreter-e2b-ws-template.id`（W，`drop_caches=false`）。
- profile：`e2b`；路径为 CI SDK `run_code` → `49999/execute` → Jupyter。
- guest 启动：`envd commands.run(background=True)` 执行官方镜像自带的
  `/root/.jupyter/start-up.sh`，设置 `E2B_LOCAL=true`。
- 构建没有修改 guest 文件，也没有注入 `patch_bridge`。`E2B_LOCAL=true` 是官方
  bridge 的本地模式，能完成本次代码执行，但不覆盖 EnvD 向 guest 注入环境变量的
  契约；该契约需要单独验收。
- 默认 Python payload：`print('hello kuasar-sandbox {index}')`。每次执行前替换为
  不同的执行标识，避免用完全相同的代码字符串制造缓存命中假象。
- 每个 sandbox：`create` → 第一次 `run_code`（cold）→ 读取 server `launchMs` →
  第二次 `run_code`（warm）→ delete。任一环失败即整条样本 discard。

### 构建门禁与模板

本次 B/W 模板由 `build_bw_code_interpreter.py` 重新生成。构建阶段观测到：

| 项目 | 结果 |
| --- | ---: |
| 首次 `49999/health` ready | 48941 ms |
| 创建验证 `run_code` | 290 ms，返回 `hello kuasar-sandbox create-check` |
| B export | 28550 ms |
| 恢复后 `49999/health` ready | 177 ms |
| W export | 8163 ms |
| W 恢复后预热 | `warm1=187 ms`，`warm2=52 ms`，`warm3=50 ms`，`warm4=50 ms` |

这些是模板构建耗时，不计入下面的 create/`code1 e2e` 并发结果。构建脚本在
`49999/health` 未 ready 或 `run_code` 没有输出时失败，不导出快照。

### 命令

```bash
E2B_ENV_FILE=/root/sandbox/nova-work/etc/node-ctl-nova/tenant.env \
timeout --kill-after=15s 600 \
python3 -u bench.py \
  --b "$(cat code-interpreter-e2b-snp-template.id)" \
  --w "$(cat code-interpreter-e2b-ws-template.id)" \
  --probe code -r 1 -c 1 30 50 --round-interval 0 \
  --run-timeout 60 \
  --failure-log logs/envd-code-interpreter-normal-image-2026-08-20.failures.jsonl
```

`-r 1` 表示每个并发档只执行一波：c=1、30、50 分别为每个 B/W 模板尝试
1、30、50 个 sandbox。失败 JSONL 未生成，且进程退出码为 0，等价于本次零失败。

模板 ID 文件校验值如下：

| 文件 | SHA-256 |
| --- | --- |
| `code-interpreter-e2b-snp-template.id` | `d9f259ead8fb4bcc33dd1ee315b5469f3006a08b215f2a80b867cb914599a08e` |
| `code-interpreter-e2b-ws-template.id` | `49bbd97543ddd8f3dae5bf3b89c3554473b4d66407394740b8b014cd3c880160` |

## 当前结果

### c=1

| 指标（ms） | B avg / p50 / min / max (n) | W avg / p50 / min / max (n) |
| --- | --- | --- |
| code1 e2e | 374.8 / 374.8 / 374.8 / 374.8 (1) | 293.5 / 293.5 / 293.5 / 293.5 (1) |
| create-api | 8.8 / 8.8 / 8.8 / 8.8 (1) | 5.8 / 5.8 / 5.8 / 5.8 (1) |
| create (`launchMs`) | 150.1 / 150.1 / 150.1 / 150.1 (1) | 88.4 / 88.4 / 88.4 / 88.4 (1) |
| code1 cold | 224.7 / 224.7 / 224.7 / 224.7 (1) | 205.0 / 205.0 / 205.0 / 205.0 (1) |
| code2 warm | 54.6 / 54.6 / 54.6 / 54.6 (1) | 60.7 / 60.7 / 60.7 / 60.7 (1) |

`runtime_ready_to_envd_init_done`：B `70.3 ms`，W `21.1 ms`（均 n=1）。

### c=30

| 指标（ms） | B avg / p50 / min / max (n) | W avg / p50 / min / max (n) |
| --- | --- | --- |
| code1 e2e | 710.0 / 714.5 / 651.5 / 755.2 (30) | 631.9 / 628.4 / 595.2 / 767.3 (30) |
| create-api | 43.1 / 33.9 / 14.3 / 109.6 (30) | 53.0 / 47.0 / 13.0 / 228.5 (30) |
| create (`launchMs`) | 257.4 / 260.2 / 212.6 / 297.5 (30) | 182.1 / 176.1 / 153.5 / 244.4 (30) |
| code1 cold | 452.6 / 450.4 / 424.0 / 480.3 (30) | 449.8 / 449.7 / 393.6 / 559.2 (30) |
| code2 warm | 48.0 / 49.0 / 13.6 / 89.0 (30) | 47.3 / 56.4 / 17.8 / 79.1 (30) |

`runtime_ready_to_envd_init_done`：B `124.4 / 122.7 / 86.1 / 153.8 ms`，W
`61.5 / 61.2 / 38.6 / 82.2 ms`（avg / p50 / min / max，均 n=30）。

### c=50

| 指标（ms） | B avg / p50 / min / max (n) | W avg / p50 / min / max (n) |
| --- | --- | --- |
| code1 e2e | 953.5 / 958.8 / 728.3 / 1082.0 (50) | 803.8 / 811.3 / 639.7 / 1009.8 (50) |
| create-api | 58.9 / 54.4 / 13.8 / 185.5 (50) | 64.4 / 57.2 / 15.3 / 305.8 (50) |
| create (`launchMs`) | 305.5 / 296.7 / 222.8 / 433.2 (50) | 220.8 / 218.0 / 164.1 / 380.7 (50) |
| code1 cold | 648.0 / 663.2 / 476.1 / 824.5 (50) | 583.0 / 588.2 / 459.7 / 768.9 (50) |
| code2 warm | 58.4 / 60.7 / 23.4 / 113.2 (50) | 51.9 / 59.5 / 18.5 / 82.5 (50) |

`runtime_ready_to_envd_init_done`：B `154.4 / 149.4 / 96.6 / 225.2 ms`，W
`86.3 / 80.7 / 51.6 / 167.0 ms`（avg / p50 / min / max，均 n=50）。

### 启动阶段趋势

| 并发 | B `runtime_ready_to_envd_init_done` avg | W `runtime_ready_to_envd_init_done` avg |
| ---: | ---: | ---: |
| 1 | 70.3 ms | 21.1 ms |
| 30 | 124.4 ms（相对 c=1 +76.8%） | 61.5 ms（+191.7%） |
| 50 | 154.4 ms（相对 c=1 +119.5%） | 86.3 ms（+309.4%） |

并发增加时，W 的 EnvD 初始化阶段增长比例更明显；这只是阶段相关性，不能单独
证明根因。需要结合 node-ctl、sandbox-ctl 和 EnvD 原始日志按 SID 做归因。

## 当前口径与限制

- `code1 e2e`：从 create 发起到第一次 `run_code` 返回；这是本报告的主指标。
- `create-api`：客户端 POST create 到收到 201 的墙钟耗时；`create (launchMs)` 是
  服务端 accepted 到 launch terminal 的计时；`code1 cold = code1 e2e - launchMs`。
- `code2 warm`：同一 sandbox 的第二次 `run_code`，不与首次 cold 路径混用。
- c=30/50 的慢化同时包含并发下的 create、launch、EnvD init 和首次 Jupyter 执行
  排队；本报告不将它归因到单一组件。
- 本报告只覆盖 CI SDK 的 `run_code`。它不代表 native `sandbox-ctl exec` 或 EnvD
  `commands.run` 的可用性/时延；两条路径需要独立验收。
- `runtime_ready_to_envd_init_done` 表示 node-ctl 收到 runtime ready 到 EnvD
  `/init` 完成的阶段，仅 e2b 场景存在。
- ready ack 和 `launch_timing` 仍应按 SID 对照 node-ctl、sandbox-ctl、EnvD 原始日志；
  bench 聚合结果不能替代日志归因。

## 历史基线（compatibility patch 版本，2026-08-20）

下面原有内容保留，用于和当前 normal-image 基线对照。该组模板来自旧的 guest
bridge patch 构建流程，不能和当前无 patch 模板直接合并统计。

## 结论

在当前 Nova 部署、`e2b` profile、code-interpreter `run_code` 路径下，B/W 各完成
一轮 `c=1/30/50` 的并发基线，**全部成功**：B `81/81`、W `81/81`，无 discarded
样本。

首次代码执行的端到端 `code1 e2e` 随并发升高明显变慢；W 在三个档位均快于 B：

| 并发 | B code1 e2e avg / p50 | W code1 e2e avg / p50 | W 相对 B（avg） |
| ---: | ---: | ---: | ---: |
| 1 | 397.6 / 397.6 ms | 335.3 / 335.3 ms | -62.3 ms (-15.7%) |
| 30 | 824.6 / 824.7 ms | 620.3 / 620.2 ms | -204.3 ms (-24.8%) |
| 50 | 1069.6 / 1068.7 ms | 804.9 / 795.1 ms | -264.7 ms (-24.7%) |

这是部署和本次模板组合的基线，不能外推为通用性能结论。`c=1` 仅一个样本，用作
单发锚点，不用于尾延迟判断。

## 场景与方法

### 工作负载

- 模板：`code-interpreter-e2b-snp-template.id`（B，`drop_caches=true`）和
  `code-interpreter-e2b-ws-template.id`（W，`drop_caches=false`）。
- profile：`e2b`；路径为 CI SDK `run_code` → `49999/execute` → Jupyter。
- 默认 Python payload：`print('hello kuasar-sandbox {index}')`。
  内置 payload 在每次执行前替换成不同的执行标识，避免同一代码字符串被重复提交。
- 每个 sandbox：`create` → 第一次 `run_code`（cold）→ 读取 server `launchMs` →
  第二次 `run_code`（warm）→ delete。任一环失败即整条样本 discard。

### 命令

```bash
E2B_ENV_FILE=/root/sandbox/nova-work/etc/node-ctl-nova/tenant.env \
python3 bench.py \
  --b "$(<code-interpreter-e2b-snp-template.id)" \
  --w "$(<code-interpreter-e2b-ws-template.id)" \
  --probe code -r 1 -c 1 30 50 --round-interval 0 \
  --failure-log logs/envd-code-interpreter-baseline-2026-08-20.failures.jsonl
```

`-r 1` 表示每个并发档只执行一波：c=1、30、50 分别为每个 B/W 模板尝试
1、30、50 个 sandbox。失败 JSONL 没有生成，等价于本次零失败。

模板 ID 文件校验值（避免在文档中写入完整 portable ref）：

| 文件 | SHA-256 |
| --- | --- |
| `code-interpreter-e2b-snp-template.id` | `9826360a755396d2a788459ebaeeac1919292e09ec73e6f92735514e6dc81f90` |
| `code-interpreter-e2b-ws-template.id` | `cca428d99f79483ff824298d26ec491d57fa1f342aad0c27469ed1d9abbdb8ab` |

## 结果

### c=1

| 指标（ms） | B avg / p50 / min / max (n) | W avg / p50 / min / max (n) |
| --- | --- | --- |
| code1 e2e | 397.6 / 397.6 / 397.6 / 397.6 (1) | 335.3 / 335.3 / 335.3 / 335.3 (1) |
| create-api | 8.8 / 8.8 / 8.8 / 8.8 (1) | 5.8 / 5.8 / 5.8 / 5.8 (1) |
| create (`launchMs`) | 111.9 / 111.9 / 111.9 / 111.9 (1) | 97.7 / 97.7 / 97.7 / 97.7 (1) |
| code1 cold | 285.7 / 285.7 / 285.7 / 285.7 (1) | 237.6 / 237.6 / 237.6 / 237.6 (1) |
| code2 warm | 54.4 / 54.4 / 54.4 / 54.4 (1) | 57.0 / 57.0 / 57.0 / 57.0 (1) |

`runtime_ready_to_envd_init_done`：B `33.1 ms`，W `30.4 ms`（均 n=1）。

### c=30

| 指标（ms） | B avg / p50 / min / max (n) | W avg / p50 / min / max (n) |
| --- | --- | --- |
| code1 e2e | 824.6 / 824.7 / 784.1 / 874.5 (30) | 620.3 / 620.2 / 559.1 / 754.2 (30) |
| create-api | 47.3 / 50.0 / 10.6 / 98.5 (30) | 55.7 / 45.7 / 12.8 / 238.2 (30) |
| create (`launchMs`) | 269.7 / 273.8 / 211.8 / 302.0 (30) | 186.3 / 185.8 / 160.2 / 215.9 (30) |
| code1 cold | 554.9 / 554.1 / 505.7 / 615.5 (30) | 434.0 / 431.7 / 390.8 / 575.2 (30) |
| code2 warm | 37.9 / 34.5 / 17.8 / 86.7 (30) | 62.7 / 65.5 / 24.1 / 75.3 (30) |

`runtime_ready_to_envd_init_done`：B `114.7 / 117.6 / 82.0 / 136.8 ms`，W
`65.6 / 63.7 / 49.5 / 84.6 ms`（avg / p50 / min / max，均 n=30）。

### c=50

| 指标（ms） | B avg / p50 / min / max (n) | W avg / p50 / min / max (n) |
| --- | --- | --- |
| code1 e2e | 1069.6 / 1068.7 / 937.3 / 1195.7 (50) | 804.9 / 795.1 / 589.6 / 1022.8 (50) |
| create-api | 64.9 / 58.4 / 15.8 / 209.4 (50) | 63.7 / 61.2 / 11.6 / 238.1 (50) |
| create (`launchMs`) | 309.1 / 312.2 / 212.0 / 474.0 (50) | 217.4 / 211.8 / 155.9 / 309.6 (50) |
| code1 cold | 760.5 / 770.1 / 580.6 / 883.5 (50) | 587.4 / 582.3 / 415.0 / 780.5 (50) |
| code2 warm | 53.0 / 55.2 / 24.5 / 95.2 (50) | 62.2 / 66.8 / 19.3 / 90.1 (50) |

`runtime_ready_to_envd_init_done`：B `140.1 / 133.8 / 82.8 / 238.0 ms`，W
`83.0 / 79.4 / 46.6 / 148.4 ms`（avg / p50 / min / max，均 n=50）。

## 口径与限制

- `code1 e2e`：从 create 发起到第一次 `run_code` 返回；这是本报告的主指标。
- `create-api`：客户端 create 调用返回时间；`launchMs`：orchestrator 从 accepted 到
  launch terminal 的服务端计时；`code1 cold = code1 e2e - launchMs`。
- `code2 warm`：同一 sandbox 的第二次 `run_code`，不应与首次 cold 路径混用。
- c=30/50 的慢化同时包含并发下的 create、launch、EnvD init 和首次 Jupyter 执行排队；
  本报告没有拆出单一根因。
- 本报告只覆盖 `run_code`。它不代表 native `sandbox-ctl exec` 或 EnvD
  `commands.run` 的可用性/时延；这两条路径需要独立验收。
