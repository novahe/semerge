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

场景 3 每轮必须打印成功的 date stdout，例如 ✓ Wed Aug 12 ...；只有耗时、没有
date 输出的轮次不能作为基线。bench.py 也保留 --probe exec，用于已启用 gateway
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
时通过 envd commands.run 启动。toTemplate 导出不会删除源 sandbox，流程最后由
脚本显式 DELETE 清理。

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
create → launchMs → 第一次 probe → 第二次 probe → delete
~~~

- create(launchMs)：conductor 服务端 acceptedAt → launch terminal 的耗时，不是客户端 HTTP create wall time。
- 非 e2b 场景：包含 runtime readiness、用户进程 app_started 确认和状态提交。
- e2b 场景：在上述基础上还包含 envd /init 初始化。
- e2e1：从客户端发起 create 到第一次 probe 返回的端到端耗时。
- run1/cold = e2e1 - create(launchMs)。
- run2/warm：只统计第二次 probe 本身。
- p50：严格中位数；偶数样本取两个中间值的平均，输出保留 1 位小数。

## code-interpreter + e2b 基线

2026-08-12，当前 B/W template，commands.run("date")，3 组 × 10 轮；B/W 共
60 次命令全部成功并返回 date stdout。原始日志保留在：

~~~text
/tmp/ws-bench-code-interpreter-date-baseline-group2.log
/tmp/ws-bench-code-interpreter-date-baseline-group3.log
~~~

每组结果：

| 组 | B run1 cold | W run1 cold | 提速 |
|---:|---:|---:|---:|
| 1 | 726ms | 43ms | 16.95× |
| 2 | 641ms | 37ms | 17.15× |
| 3 | 714ms | 35ms | 20.21× |

合并 30 轮的严格统计：

| 指标 | B | W |
|---|---:|---:|
| create(launchMs) | avg 188.7ms，p50 190.5ms，min 87，max 404 | avg 144.7ms，p50 129.5ms，min 87，max 276 |
| run1 e2e | avg 882.4ms，p50 826.5ms，min 585，max 1873 | avg 183.2ms，p50 167.5ms，min 125，max 348 |
| run1 cold | avg 693.9ms，p50 647.5ms，min 456，max 1470 | avg 38.5ms，p50 38.0ms，min 29，max 73 |
| run2 warm | avg 8.1ms，p50 8.0ms，min 7，max 9 | avg 8.2ms，p50 8.0ms，min 7，max 10 |

结论：run1 cold 平均提速 18.02×（693.9ms → 38.5ms），create 差异为
+44.0ms（B-W）。上述数据已根据两份原始日志和第 1 组终端逐轮数据复算。

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
