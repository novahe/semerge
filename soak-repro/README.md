# nova 长稳 soak 复现包

单沙箱 agent 负载长稳循环用例,runs 8-12 与发现 #1-#8、issue378 挂死家族判据均由它产出。
本包只含代码+文档(84K);全部历史运行数据留在 `/root/sandbox/semerge/repro-results/`(用户指示永久保留)。

## 文件
- `soak_agent_load.py` — 主用例。负载模型/环境变量/失败语义详见文件头 docstring(权威文档,勿另行复制维护)
- `e2b_env.py` — node-ctl e2b 兼容 API 助手(生命周期直调 HTTP,绕过 SDK 限制)
- `guest-tools/shmshim.c` — guest 垫片源码(sandbox 内核无 SysV IPC,shmget→匿名共享 mmap;`.so` 缺失时脚本会用宿主机 cc 自动编译,无需预置)
- `soak-findings-summary.md` — 发现 #1-#8 总结

## 前置条件
1. 被测 nova 栈在跑(node-ctl/orchestrator;runs 8-12 = release-v0.1.9 ± issue378 热补丁 31edb83)。
2. 模板 manifest `5072d03872754f2c276fc229a6984617e646bdb61e7664c828e8eeee83c6c1b2`(2C/8G code-interpreter;guest vda 3G RO base + vdb 32G RW overlay-32G)。
3. 租户 env `/root/sandbox/nova-work/etc/node-ctl-nova/tenant.env`(E2B_DOMAIN/E2B_API_KEY/SSL_CERT_FILE),可用 `E2B_ENV_FILE` 覆盖。
4. `python3` + e2b SDK;宿主机 `/usr/bin/fio` + `/usr/lib64/fio` 插件(guest 无 fio,首轮自动上传,只落 overlay、pause/resume 全程保留)。

## 快速冒烟(1 轮小剂量,~3 分钟)
```bash
CRUD_NEW_MB=64 DISK_PEAK_MB=2048 ROUNDS=1 FIO_RUNTIME_S=10 MEM_BURSTS=1 MEM_HOLD_S=5 \
  python3 soak_agent_load.py
```

## 正式长稳
```bash
cd /root/sandbox/soak-repro
nohup python3 -u soak_agent_load.py >> repro-results/soak-agent-latest.log 2>&1 &
```
默认 ROUNDS=0 无限轮直到失败。产物:`repro-results/soak-agent-<ts>/rounds.jsonl`(逐轮)+ `result.json`(汇总/失败诊断)。

## 巡检模板(30min 一次)
```bash
PY=$(pgrep -af "python3 -u soak_agent_load.py" | grep -v zsh | head -1 | cut -d' ' -f1)  # 必须 grep -v zsh 防自匹配
```
- 看 rounds.jsonl:pause_ms / pause_factor / resume_factor / smoke_factor(健康 ≈1-1.1;pause_factor 随轮次持续上涨=问题)、`ok=false` 轮、ckmb、host_du_mb 增量。
- **bench 回归先查宿主 `/proc/pressure/io`**——共享 bms 节点,同事 bench/restore 批次污染是最常见假阳性(最快判据,比数 journal restore 直接)。
- conductor 日志:`/root/sandbox/nova-work/var/lib/logs/conductor.log`(unit 不进 journal)。刷 `ownership cleanup incomplete; retrying` + `wake resume failed` = 32GiB presence wedge(pre-#193 reader 拒收全量描述符;**accelerator ≥#193 (fec589f) 必须随栈带上**)。
- 失败语义:进程打印 `=== summary ===` + `沙箱已保留供取证 sid=…` 后**正常退出**(设计终态,不是崩溃);重启即新 run。

## 已知坑(复现时对照,详见 soak-findings-summary.md)
- SDK `connect(timeout=N)` **绝对覆盖** endAt 且缺省只发 300s——审计/巡检脚本一律显式 `timeout=21600`。
- 沙箱续期正解 `POST /sandboxes/{id}/timeout` json=`{"timeout":86400}`;`PATCH /sandboxes/{id}` 是 405。
- `sandbox-ctl` 不在 PATH——guest 探活走 SDK python(`commands.run` TimeoutException = envd 死/内核活的判据)。
- zsh 下 `===` 未加引号会炸;`pgrep -f` 会匹配巡检自身 shell。
- guest 内 fio 必须带 `LD_PRELOAD=/root/soak/bin/shmshim.so`(脚本已处理;手工复测时勿忘,否则报 shmget 未实现)。

## 基线对照(数据在 semerge/repro-results/)
| run | 目录(soak-agent-) | 栈 | 轮次 | 收官原因 |
|---|---|---|---|---|
| 12 | 20261030-160426 | v0.1.9+issue378 补丁 | 80/79 | r80 guest 挂死(#7/#8 家族第 4 次:balloon Out of puff 饿死活锁+envd 死) |
| 11 | 20261030-142223 | 同上(补丁 15:08 落地后) | 9/8 | r9 同族挂死 |
| 10 | 20261029-190934 | v0.1.9(#2 修复栈) | 94/93 | r94 32GiB presence wedge |
| 9 | 20261027-210248 | v0.1.9 | 222/221 | r222 自身 resume balloon 收缩正压 bench |
| 8 | 20261027-143804 | v0.1.9 | 31/30 | r31 IO 塌缩(污染源未捕获) |

一~六跑(v0.1.8/4faafc6 混合栈,20261026-020304 起)同目录,发现 #1-#8 首现。

## 数据保留现状(2026-11-02 清理后)
`semerge/repro-results/` 现存 ~39M:**全部 soak-agent-\* 运行目录(runs 1-12 逐轮 rounds.jsonl/result.json)、cap-probe、r94-presence-wedge、storage-matrix、pause-profile 文本分析与 snapshot-copy-opt 的代码/proposal/微基准 JSON**。
已按用户指示删除的中间产物(~220M):profile 二进制抓取(perf.data / io-events.data / syscall-events.txt)、snapshot-copy-opt 的 bin 与 e2e 工件及源树副本、13 个 20261026 凌晨调试碎片目录。
issue378 两次挂死的取证物证已于 2026-11-02 按用户指示删除,文字分析在 `/root/sandbox/issue378-20261030/INCIDENT-*.md`。
