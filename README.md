# ws-bench — Working-Set B/W 快照延迟基准测试

## 这是什么

量化 working-set 快照（W：`drop_caches=false` + 预热）相对冷快照（B：`drop_caches=true`）在 restore 后首请求延迟上的收益。支持 bare / e2b 两种 profile、exec / health 两种探针。

## 文件说明

| 文件 | 作用 |
|---|---|
| `build_bw_coldboot.py` | 一次冷启动顺序产出 B+W 快照（同 guest，变量最少） |
| `bench.py` | B vs W restore 延迟基准（`--b` / `--w` 条件执行，exec/health 探针） |
| `openclaw_net.py` | host 经 vswitch floating IP 直连 gateway 的 curl/exec 辅助 |
| `e2b_env.py` | 解析 `e2b-env.sh` 注入 `os.environ`，让脚本自包含 |
| `e2b-env.sh` | 环境配置（conductor 地址、API key、TLS 证书、floating IP 段等） |
| `config/` | conductor 参考 config + TLS 证书 |

## 前置条件

1. conductor :4443 在跑（`node-ctl conductor serve --config config/config-4443.yaml`）
2. vswitch + cache-ctl + store-ctl 在跑
3. `e2b-env.sh` 里的路径与本机一致（尤其 `SSL_CERT_FILE`、`E2B_RUN_ROOT`、`E2B_FIP_BASE`）

## 快速开始

```bash
cd /root/sandbox/ws-bench

# ① 打 B+W 快照（bare 或 e2b）
PROFILE=bare python3 build_bw_coldboot.py
# → bare-snp-template.id（B）+ ws-template-bare.id（W）

# ② 跑基准测试（10 预热 + 50 正式）
B="$(cat bare-snp-template.id)"; W="$(cat ws-template-bare.id)"
python3 bench.py --b "$B" --w "$W" --probe exec   -n 50
python3 bench.py --b "$B" --w "$W" --probe health -n 50

# 也可以只跑 B 或只跑 W
python3 bench.py --b "$B" --probe exec -n 50
```

## build_bw_coldboot.py 流程

```
cold boot → gateway ready → settle(2s)
→ pause#1（默认 drop_caches=true）→ export → B（冷快照）
→ connect 恢复 → 预热 health+exec ×4
→ pause#2（drop_caches=false, merge_ref=false）→ export → W（工作集快照）
```

常用环境变量：

| 变量 | 默认 | 说明 |
|---|---|---|
| `PROFILE` | `e2b` | `bare`（launch 注入 gateway）或 `e2b`（envd commands.run） |
| `OUT_B` / `OUT_W` | `bare-snp-template.id` / `ws-template-bare.id` | 输出 .id 文件路径 |
| `READY_SETTLE` | `2` | gateway ready 后、pause#1 前的等待秒数（避免 e2b-B 冷热异常） |
| `WARM_CURLS` | `4` | W 预热轮数（health+exec ×N 打 bash-tools 工作集） |
| `SANDBOX_TIMEOUT` | `600` | 沙箱空闲回收秒数 |

## bench.py 输出

每轮：`create（restore 墙钟）→ 探针 cold → 探针 warm → delete`，汇总 avg/p50/min/max。

```
=== 汇总 ===
  create        B avg=88ms p50=88ms (n=50)  | W avg=90ms p50=90ms (n=50)
  exec1 (cold)  B avg=3599ms p50=3610ms (n=50) | W avg=356ms p50=361ms (n=50)  ★
  exec2 (warm)  B avg=21ms p50=21ms (n=50)  | W avg=135ms p50=130ms (n=50)
  exec cold 提速: 10.1×  (3599→356ms)
```
