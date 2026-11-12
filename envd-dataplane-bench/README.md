# envd 数据面全家桶 bench（A–G 场景矩阵）

pkg/fwd 短写修复（[sandboxer#435](https://github.com/kuasar-sandbox/sandboxer/pull/435) / #434）在 nova-2
（v0.1.9，host sandbox-ctl + guest sandbox-init 双侧热补）上的扩展验证。一次跑全七类场景，
3 沙箱并发，模板 ID 由 manifest 现场拼装，所有大文件传输双向 sha256 校验。

## 场景 → Phase 映射

| 场景 | Phase | 内容 |
| --- | --- | --- |
| 基线构建 | P0 | TPL = `e2b-img-` + base64(`manifest://<sha>`) 现场拼装；3 箱并发 create + 初始 200MiB 上传 |
| A 大文件并发 | PA | 单箱 6 路并发 200MiB 下载（h2×3 + h1.1×3），同时 exec ping 测重载延迟（无卡顿） |
| B 超大文件 | PB | 1GiB × 4 轮下载（h2/h1.1 交替）sha 校验 |
| C pause/resume | PC | `pause()` → exec 触发自动 resume → proxy 数据面恢复探测（2s×120s）→ 200MiB 全量验证 |
| D MUX 对照 | PD | `commands.run` 流式拉 200MiB stdout（guest 侧 base64 装甲规避 UTF-8 解码失真）sha 校验 |
| E 小文件高频 | PE | 1KiB × 200 轮上传+下载 sha 校验 |
| H gzip 压缩传输 | PH | 文本/随机 `Accept-Encoding: gzip` 下载 + `Content-Encoding: gzip` 上传（envd 服务端解压），sha 校验 + 线上字节计量 |
| F 协议覆盖 | PA/PB | h2 与 h1.1 全程交替 |
| G 资源观测 | PG | host 侧 sandbox-ctl/cloud-hypervisor RSS 全程 2s 采样 + guest envd RSS 每 phase 边界采样 |

## 关键设计

- **timeout=1800 + set_timeout(1800)**：规避 300s 自动 pause（上一轮 soak 的污染源）。
- **PC 即 502-after-restore bug 的受控复现**：soak 中发现的"pause→restore 后 proxy 持续 502"
  在这里变为可观测指标（恢复耗时 / 120s 未恢复则记失败并重建沙箱）。
- 所有 curl 走签名直连（`v1_base64nopad(sha256("path:op:user:token"))`，POST +
  `application/octet-stream`），失败零容忍计数。
- 大文件落盘在脚本目录 `work/`（/dev/sda2），不占 tmpfs。
- **PH gzip**：envd files API 原生支持双向 gzip（下载 `Accept-Encoding: gzip`→流式压缩响应；
  上传 `Content-Encoding: gzip`→服务端流式解压落盘），签名与编码无关，proxy 两 header 均透传
  （`probe_gzip.py` 单箱探针已证）。文本（重复行，~1000:1）测线上节省，随机数据测最差情况
  （无压缩收益，~+0.02% 线上开销，瓶颈在 guest gzip CPU）；下载侧 curl 不用 `--compressed`
  （透明解压会掩盖线上字节），手动 gunzip + sha 校验。

## 用法

```sh
source <目标节点>/tenant.env           # 提供 SDK 鉴权 + E2B_DOMAIN/E2B_SANDBOX_URL/CA
export BENCH_TPL=<模板ID>              # 或 BENCH_MANIFEST=<sha>，或放同目录 bench.env
python3 dataplane_bench.py             # 全量 P0+PA..PH
# 连跑: TENANT_ENV=<tenant.env 路径> sh run-loop.sh   (索引 bench-soak-index.tsv)
```

配置三层：**环境变量 > 同目录 `bench.env`（gitignored，本节点覆盖）> 报错退出**。
代码不内置任何节点信息；缺关键配置会列出缺什么再退出（rc=2），不会静默用错值。

产物：stdout 全程日志（含事件时间线、RSS 报告、失败明细）。

## 跨节点移植

脚本不写死任何节点路径。**source 目标节点的 tenant.env 即可直接跑**——proxy 基址取
`E2B_SANDBOX_URL`、CA 取 `CURL_CA_BUNDLE`、NS 标签取 `E2B_DOMAIN` 首段，全部自动推导；
模板 ID 必须显式给（各节点 store 内容不同）。布局/端口特殊时用 `BENCH_*` 环境变量覆盖：

| 变量 | 缺省（推导链） | 说明 |
| --- | --- | --- |
| `BENCH_PROXY` | `$E2B_SANDBOX_URL` → `https://$E2B_DOMAIN:32591` | proxy 基址 |
| `BENCH_PROXY_PORT` | `32591` | 仅当走 E2B_DOMAIN 推导时使用 |
| `BENCH_CACERT` | `$CURL_CA_BUNDLE` → `$SSL_CERT_FILE` | 空则靠 curl 自身 env |
| `BENCH_NS` | `E2B_DOMAIN` 首段去 `sandboxes-` 前缀 | host RSS 采样只盯本 NS 进程 |
| `BENCH_MANIFEST` | 无（必填之一） | 裸 sha 或 `manifest://…` 均可 |
| `BENCH_TPL` | 由 BENCH_MANIFEST 拼装 | 完整模板 ID，直接指定则忽略 MANIFEST |
| `BENCH_WORK` | 脚本目录 `work/` | 大文件落盘目录（需 ≥3GiB 空闲） |
| `BENCH_NBOX` / `BENCH_USER` / `BENCH_ENVD_PORT` | `3` / `root` / `49983` | 并发箱数 / files API 用户 / envd 端口 |
| `BENCH_TIMEOUT` | `1800` | 沙箱空闲超时；必须大于 bench 预计墙钟，防自动 pause |

例（nova 节点）：

```sh
source /root/sandbox/nova-work/etc/node-ctl-nova/tenant.env
export BENCH_MANIFEST=<该节点 store 里模板的 sha>   # 模板内容不同节点不同
python3 /root/sandbox/semerge/envd-dataplane-bench/dataplane_bench.py
```

依赖：python3 + e2b SDK + curl。`envd_soak_updown.py`（semerge 根目录）已同步此配置方案
（同款 bench.env 分层，其 bench.env 在 semerge 根目录）。

## 历史结果

### 2026-11-12 run5（nova-2，v0.1.9 + pkg/fwd 短写修复双侧热补）— ✓ 全部通过

墙钟 386s，3 箱并发，全程 0 失败、0 sha 不匹配、0 stall：

| Phase | 结果 | 关键数据 |
| --- | --- | --- |
| P0 | ✓ | create+初始 200MiB 上传 2.5s/箱 |
| PA | ✓ | 6 路并发下载 6/6；exec ping 空载 p50=7ms → 重载 p50=9ms / p99=21ms（无卡顿） |
| PB | ✓ | 1GiB×4/箱，下载 27~33s/轮（≈33MiB/s，3 箱并发含双侧 sha） |
| PC | ✓ | pause 6.1s（box2 21.0s，写入量大时捕获变慢，与 pause 线性结论一致）；resume 0.5~1.1s；**proxy 数据面恢复 6.3~8.6s，502-after-restore 未复现** |
| PD | ✓ | 200MiB base64 stdout 经 MUX 9.9~11.3s，sha 一致 |
| PE | ✓ | 1KiB×200 上传+下载 31~32ms/轮 |

RSS（host）：原始代 sbxctl 峰值 656~727MiB（+532~579，200MiB+1GiB 传输页面缓存）、CH 峰值 877~950MiB；
PC pause→restore 后出现新代 sbxctl/CH（峰值 ~375/~330MiB）——恢复即换进程代，内存回到干净水位，无跨代泄漏。

注：run1~4 的失败均为脚本侧问题（模板 ID sha 抄错 + b64 padding 被 conductor raw 解码拒绝、
curl `--data-binary` 1GiB 整读 OOM→改 `-T` 流式、上传漏 `-o /dev/null` 污染 `-w` 解析），非栈缺陷；
定位过程见 conductor.log 的 `api error ... decode ref: illegal base64` 与 SDK v2.39.1 把 500 体解析成
KeyError('code') 的坑。

### 2026-11-12 run34（新增 PH gzip 压缩传输）— ✓ 全部通过

墙钟 418s（PH 增量 +32s），3 箱并发，PH 三路全 ✓：

| PH 子场景 | 结果 | 关键数据 |
| --- | --- | --- |
| gzip 下载 200MiB 文本（~295:1） | ✓×3 | 线上 **0.68MiB**（原 200MiB），3.4~4.9s（瓶颈=guest gzip CPU） |
| gzip 下载 200MiB 随机（最差情况） | ✓×3 | 线上 200.06MiB（+0.03% 开销），4.5~4.7s ≈ 43MiB/s（guest gzip CPU 上限） |
| gzip 上传 200MiB 文本 | ✓×3 | 线上 0.68MiB，5.9~8.6s（瓶颈=envd 服务端解压后 200MiB 落盘，~25-34MiB/s），guest sha=原文 ✓ |

对照（probe_gzip.py 单箱 50MiB）：identity 上传文本 2.58s vs gzip 上传 0.24s（**~10×**）；
proxy 对 `Accept-Encoding`/`Content-Encoding` 双向透传，签名不受编码影响。
