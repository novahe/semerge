# 冷启动 e2e 基准与首命令冷缺页归因 — 复现步骤

- 日期:2026-11-03 / 栈:kuasar-sandbox **release-v0.1.10 全栈**(sandboxer 含 #378 修复)
- 场景:e2b create 双盘沙箱(openclaw manifest)→ envd 首命令 `true` 返回 = 一次
- 结论先行:img 真冷启 e2e p50 **3442ms**,其中 **exec_after_launch ~2.5s = 冷内存首触换页**
  (零页/CoW 为主,非磁盘)。完整数据见 README「coldstart-envd 基准」。

## 0. 环境前置

1. nova 节点跑 release-v0.1.10 全栈(部署/回滚见 `deploy/deploy-ns.sh replace`)。
2. 环境变量钉住 nova tenant(bench 已内置同样兜底,手工操作时必须显式设):

   ```bash
   export E2B_ENV_FILE=/root/sandbox/nova-work/etc/node-ctl-nova/tenant.env
   ```

   坑:仓库根 `e2b-env.sh` 是 10 月老 demo 环境(指向已删目录),漏设会报
   "No CA certificates were loaded from the system"。
3. 确认 manifest 源里存在 openclaw manifest(KEY `7e78b050c656…`,bench 默认值)。
4. envd 命令必须 `user="root", cwd="/"`(openclaw 镜像无 `user` 账号与 /home/user)。

## ① img 真冷启基准

```bash
cd /root/sandbox/semerge
python3 bench_coldstart_envd.py --breakdown          # 默认 manifest KEY 冷启 × 10
```

- 默认模板 = `e2b-img-<b64url("manifest://7e78b050…")>`,内核真正重新引导
  (= build_bw 快照 pause 前的流程);`--no-check` 跳过开班 lsblk 双盘检查。
- 产物:`repro-results/coldstart-envd-<ts>/`
  - `samples.jsonl` / `result.json`:每样本与汇总分段
  - `journal-<i>-<sid8>.log`:该沙箱 journal 切片(离线分析用)

**预期(2026-11-03, n=10)**:e2e p50 ~3442ms = create 5.3 + launch_wait 840
(node_ctl 8 + runner_pre 55 + **vm_entry 470** + kernel_boot 50 + guest_init 190 + envd 40)
+ **exec_after_launch ~2500**。偏差 ±20% 内都算复现成功(冷缺页量随页缓存状态浮动)。

阶段口径(数据源 = conductor.log 埋点 + journal):

| 键 | 语义 | 切点 |
|---|---|---|
| runner_pre | ctl 起进程→CH spawn(manifest fetch) | journal `CH started pid=` |
| vm_entry | 固件/内核解压/早期引导 | CH spawn→console `Run /sbin/init as init process` |
| kernel_boot | 内核→init | 首 console→`Run /sbin/init` |
| guest_init | overlay 组装等 | `Run /sbin/init`→`launch: app_started` |

## ② exec_after_launch ~2.5s 归因复现

```bash
python3 probe_exec_gap.py     # 自动两轮:A running 即发;B running 后等 3s 再发
```

预期输出:A 轮 cmd1 ~2.4-2.9s、cmd2 ~15ms;B 轮 cmd1 依旧 ~2.4s(排除 envd 慢热)。
探针期间还轮询 envd.sock `/health`(注意 envd 答 **204** 非 200),全程 204 = envd 活着。

补三条手工证据(任取一次 ① 的 journal dump):

```bash
D=repro-results/coldstart-envd-<ts>
# 1) guest 侧首命令会话耗时:cmd1 envd 会话 T+316→T+3246ms(cmd2 会话 ~15ms)
grep -o '\[T+[0-9]*ms\][^|]*' $D/journal-1-*.log | grep -i 'session\|exec'
# 2) 冷缺页速率 vs 盘读:窗口内 uffd ~4000 fault/s、pgin ~32k 页/s,blk0 仅 ~1MB/s
grep '\[lazy\]' $D/journal-1-*.log
# 3) HTTP 层不慢(40ms 即 200,卡响应流收尾):用 SDK debug 或 pyqwest 日志观察
```

结论链:envd 全程 204 → 等 3s 无效 → 二次命令 15ms → HTTP 40ms → guest 首命令
会话 2.9s → uffd/pgin 高而盘读≈0 ⇒ **冷内存首触换页(零页/CoW),非磁盘、非 envd 慢热、非网络**。

## ③ journal 手工核对(v0.1.10 格式)

v0.1.10 起 `SYSLOG_IDENTIFIER` 是裸 `sandbox-ctl`/`console`(pid 在 `_PID`,sid 只在
消息文本里),µs 时间轴用 JSON 输出:

```bash
# 定位该沙箱 sandbox-ctl 的 pid(sid 出现在 CH args socket 路径等消息里)
journalctl -o json --since <起点> | jq -r 'select(.MESSAGE|test("<sid>")) | ._PID' | sort -u
# 取该 pid 的行与 console 行,按 __REALTIME_TIMESTAMP(µs)算差
journalctl -o json _PID=<pid> --since <起点> | jq -r '[..__REALTIME_TIMESTAMP,.MESSAGE] | @tsv'
```

关键标记行:`CH started pid=`(runner_pre 终点)、`Run /sbin/init as init process`
(vm_entry 终点)、`launch: app_started`(guest_init 终点)。

## 优化修改点(按收益排序,验证方式 = 重跑 ① 看对应段)

| # | 修改点 | 位置 | 预期 |
|---|---|---|---|
| ① | 启动序列自跑一次 `true`(warmup exec),把缺页收进 launch 窗口 | guest 侧 sandbox-init/envd | e2e 3442→~900ms(exec 变 15ms 级) |
| ② | uffd zeropage 吞提(fault_queue_wait p50 ~12ms,~4000 fault/s 是 ① 的地板) | 宿主 sandboxer uffd 处理 | 直接缩短 ① 的 warmup/首命令成本,pause/恢复同受益 |
| ③ | vm_entry 470ms(内核解压/早期引导):更小内核镜像/引导参数 | 内核与引导配置 | 封顶 470ms |
| ④ | vdb ext4 mount ~186ms 异步化(挂载挪到 readiness 后) | guest sandbox-init phase1a | guest_init 190→~10ms |

①+④ 纯 guest 侧、不动宿主协议,先做;② 动宿主 uffd 路径,第二步。
