# nova 长稳测试问题总结

> 2026-10-25 ~ 10-26 · soak_agent_load.py 三跑(87bb211 34 轮 / 4faafc6 40 轮 r40 失败 / v0.1.8-preview 进行中)· 双盘模式
> 数据:`repro-results/soak-agent-20261026-{020304,082150,161621}/`;#2 专项根因见 [pause-cap-rootcause.md](pause-cap-rootcause.md)

## 测试背景

- **负载模型**:每轮 = crud 滚动窗口(256MiB 新文件/轮,keep=25)+ fio 4k direct 30s(psync/libaio/io_uring 轮转)+ **3×7360MiB 内存突发** + 全目录 sha256 一致性快照 + pause→reuse(resume)+ bench A(randread 4k)/B(randrw)/C(seqread 1m)×3
- **栈**:conductor = orchestrator main `6dda306`;sandbox-ctl 首跑 `87bb211` → 二跑 `4faafc6`(含 #351 diffstore Discard 修复);**三跑(161621)起 = release-v0.1.8-preview.20261026 全栈**(同事 15:59 部署,sandbox-ctl 随 node-ctl 同目录自动发现);runtime bundle `a419345`;节点 2C/8G/沙箱,balloon 初始 7.75GiB
- **双盘**:vda 3G RO=1(只读 base erofs)+ vdb 32G RO=0(diff 盘 overlay-32G.ext4)✓
- 取证全部宿主侧外化(`guest/`:dmesg/meminfo/fio json,不依赖被测对象)

## 发现总览

| # | 问题 | 定位 | 状态 | 严重度 |
|---|---|---|---|---|
| 1 | envd exec 压力期丢 stdout 载荷 / 启动超时 | envd/exec 通道 | 实锤,已防御 | P2 |
| 2 | pause 一次突发后持续变慢,5分钟内不回落 | guest balloon cap + host actual-bound | **已定位**,修复尚未验证 | **P1** |
| 3 | 宿主空间只涨不收(+327MB/轮) | diffstore 块地址高水位 | **终局已实测:无 presence 墙,绑 guest 盘容量** | P3(降级) |
| 4 | bench randrw 轮瞬时毛刺(+11~27%) | 外部争抢(3 次实锤) | 已定位 | P3 |
| 5 | teardown 双 SIGTERM → CH 100% 被 SIGKILL | sandboxer 信号处理 | 实锤(82/82) | P2 |
| 6 | diff 盘偶发单读 ~711-714ms 停顿 | diffstore 读路径 | 实锤,机制未知 | P3 |
| 7 | 挂死沙箱无回收(15.5h 僵尸) | conductor 健康检查缺失 | 实锤,已手工清理 | P2 |
| 8 | resume 后 ~60s guest envd 静默死亡,9 分钟无人回收 | envd/健康检查 | **2 次实现**(二跑 r40 + 三跑 r12=v0.1.8) | P1 |

---

## #2(P1)pause 台阶 — 已定位 guest 压力 cap 不恢复

**同样 8GiB,触发取决于内存使用历史。** 独立新沙箱3/6/7GiB突发后释放并等待5分钟,后续pause分别仍为15~16s/30~32s/约35s,基线约1.6s。**下沿已补测**(512MiB/1GiB/2GiB → 2.7/6.4/11.1s):压力/deflate 阈值在 512MiB~1GiB 之间(≈启动授予内可容纳),但**快照膨胀无阈值** — 512MiB 档无任何压力事件 snapshot 仍=459MB 永久携带;pause≈1.6s+~4.4s/GiB 线性。有限观察不能表述为永不回落。

机理与部署内核反汇编见 [pause-cap-rootcause.md](pause-cap-rootcause.md):
1. guest自定义virtio-balloon补丁的压力cap只降不升;部署内核反汇编确认了同一行为。
2. 3/6/7GiB组actual balloon恰好停在cap=4735/1597/573MiB,低于host target=4736/1600/576MiB。
3. host actual-relative步长与64MiB向下对齐使下一步仍等于已接受target,回收停滞。直接调用实际NextShrinkTarget复核通过;补齐欠充后即可产生下一档目标。
4. guest free不等于宿主可跳过的memfd洞,快照仍捕获大量内存;BeginSnapshot本身不解除cap。

**修正原结论**:本组平台期ObservedBudget=Reservation,并有shrink settled,因此不能归因为预算闸门死锁或完全不充气;dmesg没有inflate字样也不能证明未回收。原长稳7634→7957→8002→8022M曲线保留为历史观察,不外推所有生产负载。

修复首选验证guest cap受限恢复,保留host安全结算;需新内核冷启动并重新生成快照。仅提高快照前target仍可能被cap阻挡,不能承诺回到5s。首次空闲后另有约60s进入快照前等待,尚未定位,与持续大快照成本分开记录。

## #1(P2)envd exec 流在内存压力期不可靠

- **丢载荷**:rc=0 但 stdout 空,guest 文件内容完整(首跑 3/34 轮);二跑 4faafc6 上 0 复发
- **启动超时**:burst 窗口内 commands.run 连续 2×60s 超时,几乎每轮 1-2 次(两版本都有),重试 1 次即成功
- 无 OOM、脚本完好、数据持久 → 指向 envd/exec 流控在 balloon 收缩期丢流
- 防御:rc=0 空输出自动重拉 3 次,全部恢复

## #3(P2)宿主空间只涨不收 — **终局已实测(2026-10-27,修正预测)**

- guest 滚动删除(每轮删最老 256MiB)下,宿主 du **+327MB/轮恒定**(首跑 11.3→22.2GB/33 轮;run 6 同斜率至 58 轮)
- **#351(Discard=完整 Zero)无效**:二跑斜率 ~327MB/轮与首跑持平;guest overlay 根不透传 FITRIM(`fstrim /` 报 not supported)
- **终局实测推翻"32GiB presence 硬上限/preflight 拒绝"预测**(run 6 取证沙箱推至 guest 盘 100% 满/沙箱总 38.2GiB/单 diff 30.24GiB):写/pause(102s)/restore(0.4s)/exec/数据校验全通零丢失。**真正绑定约束 = guest overlay ext4 自身容量**(overlay-32G 模板)。
- **增长语义修正**:写入命中 guest 已存在块地址(ext4 复用释放块)则 presence 不增(实测 9GiB urandom → diff 仅 +1.1MB);轮内 +327MB = 新块地址高水位扩展。稳态下增速会显著放缓,非纯只增不减。
- 详见 [cap-probe-20261027.md](repro-results/cap-probe-20261027.md)。剩余关切降级为:宿主盘水位(而非 presence 墙)。

## #5(P2)teardown 双 SIGTERM → CH 永远死于 SIGKILL

每次 pause/delete 的 teardown 100% 走此链(journal 82/82):
```
lifted VMM memory.high for ordered shutdown
→ vmm.shutdown API 500 "receiving on a closed channel"   ← CH API 已死,优雅关闭从未成功过
→ forwarding SIGTERM to CH (will SIGKILL after 5s)
→ 同一毫秒第二个 "received terminated while shutdown in progress" → sending SIGKILL now
→ CH exited code=-1
```
第二个 SIGTERM 与第一个同毫秒到达,直接升级 SIGKILL,"5s 宽限"从未生效 → **优雅关闭路径实际是死代码**。pause 数据正确性不受影响(checkpoint 先落盘),但信号去抖缺失 + CH API closed-channel 时序问题值得上游修复。

## #6(P3)diff 盘偶发单读 ~700ms 停顿

- blk1(diff 盘)读 p99 尖刺 **711.8ms(4faafc6)/ 714.4ms(87bb211)** — 两版本 max 几乎同值 → 确定性机制(锁等待/定长操作),非磁盘抖动
- 常出现在 0-14 rd/s 近零流量(单次卡死型);同窗口 blk0(只读 base)400 rd/s 仅 3ms → 特定于 diffstore 后端
- 频率约每跑一次 1 次,落在 resume 关键路径(恢复首击 exec 1.2-1.3s 的组成部分)

## #7(P2)挂死沙箱无回收路径

- `01a17256`:restore 后 guest 即挂死(vsock exec_ack 全部 i/o timeout),sandbox-ctl 存活 **15.5h** 无健康检查/超时触发,占 runner+内存,需手工 API DELETE
- 对照:**conductor 数据侧 GC 正常** — API DELETE 会自动清 var/lib(01a1741c 11.7G 自动回收);缺的是"runner 活着但 guest 死"的检测回收
- 附带清理:本次共回收 ~45G(挂死沙箱 + 过期残骸 + 旧格式 parked),盘 343G→298G

## #8(P2)r40:resume 后 ~60s guest envd 静默死亡,9 分钟无人回收

二跑唯一失败轮(r40,39/40 过)。完整时间线(journal 交叉):
```
15:44:28  r39 resume 正常(runner sr-…-828d,CH restore,vhost 双盘 connect ✓)
15:45:30  vsock ConnectionReset(pp=5000=envd 端口)← envd 进程死亡时刻
15:45:40+ guest 内核仍活(vsock 能 establish),但 envd 端口秒断
          → port-forward 盲目重拨 9 分钟(established→session done ~9ms,每 3s)
15:48-51  r40 snapshot#r318:"Command ended without an end event"→重试" sandbox not found"
15:55:44  diag API 200 state=dead(VMM/sandbox-ctl 仍活着!)
15:59:48  同事 v0.1.8 部署重启 node-ctl,才顺带清掉滞留 VMM
```
- **排除项**:窗口内(15:44:30-15:47:30)零 balloon/shrink/pressure 事件 — 非宿主侧内存回收压死;无 OOM/panic 日志;同事 build 批量在死亡之后(15:55+)才起
- **与 #7 同族但更糟**:内核活、仅 agent 死,sandbox-ctl/conductor 9 分钟无健康检查无回收,若非外部重启将复刻 #7 僵尸
- **与 #1 不同**:当时无内存压力,envd 是进程级死亡(exec 流丢载荷是另一回事)
- 附带:diag `endAt=13:44:28Z` 与真实死亡时刻(07:45Z)差 6h — API 时间戳格式化疑点,未深究
- 取证沙箱 var/lib 已回收;v0.1.8 冒烟(T+90s 延迟 exec ×1)未复现,三跑继续盯

### #8 第二次实现(三跑 r12,v0.1.8,2026-10-26 18:08)

与 r40 签名完全一致:round 开局 resume restore(18:07:48)→ **12s 后**(18:08:00)port-forward 即进入 established→9ms 秒断盲拨循环(guest 内核活、envd 死)→ 该轮 crud/fio 竟然成功、memburst 起 exec 全废 → SDK 报"upstream error/sandbox timeout"。sandbox-ctl/VMM 滞留存活。**升级 P1**:两个版本(4faafc6 / v0.1.8)、两次跑、间隔 12s/60s,确定性家族缺陷。

r12 附带三项语义发现(对保留沙箱受控实验验证):
1. **connect 的 timeout 是绝对覆盖 endAt,可改短**;SDK 缺省 timeout=300s(`sandbox/main.py:25`,`timeout or default` 填充)——任何不带长 timeout 的 connect 都会把长命沙箱截到 5 分钟
2. **v0.1.8 到期默认 auto-pause 而非 kill**(endAt 过后 state=paused、内存保留,再 connect 可复活);exec 实测复活成功(envd 90min 后可用,疑 guest 内 supervisor 慢重启,未证)
3. **SDK 的"sandbox timeout"报文不可信**:envd/rpc.py 把 gRPC UNAVAILABLE、HTTP 502 统一翻译成该文案——r12 真因是 envd 死,不是超时
4. 未解:失败时刻 endAt=18:12:47=开局 resume 完成时刻+300s,但 pause 不改 endAt(实测)、SDK 21600 路径正常 +6h(实测)、raw connect 600 也被尊重(实测)——**+300 的设置者未定位**,疑 v0.1.8 内部(park/proxy?),待 conductor 源码或日志级调试

四跑(run 4,2026-10-26 18:5x 起)防御:fd_audit/smoke 的 connect 已改 timeout=21600;巡检时对 soak 沙箱 GET endAt,若余量 <1h 主动 POST connect timeout=21600 续期。

## #4(P3)bench 瞬时毛刺(部分降级为环境噪音)

首跑 r6/r13/r24/r33 毛刺 +11~27%,总在 randrw 轮、次轮恢复 → 疑 fio 引擎切换/页缓存交互。二跑 r2/r3 的抬升已确认是**同事批量负载污染**(08:40-08:5x,87 个 restore 进程打同一 conductor,共享节点),退去后 r4+ bench 持平(A 31.3-31.5s)。首跑毛刺是否同样有外部成分不可考,降级观察。

**四跑 r24(2026-10-26 22:35)第二次实锤同款污染,证据链完整**:bench A rep1 正常 33.3s → 同事 23 个 restore 于 22:35:47 起批(21 个非本沙箱)→ rep2/3 崩至 103/118s(B rep2 207s,6.8×)→ 判回归轮失败。批次结束后 23:36 在同一保留沙箱实测同文件同引擎(psync/direct/4k)= **2036 IOPS 满血**(崩时 603-638)。结论:共享节点上 bench 回归判定必须先排除外部批次,建议 soak 在 bench 前采样宿主负载/restore 计数做护栏(未实现,巡检人工核对 journal)。

**六跑 r57/r58(2026-10-27 12:05-12:50)第三次实锤,新源类型——持续 fsync 写风暴**:chenxiaohui 的 cache-kv 实验(`/var/tmp/cache-kv-20261027-*`,service-probe,rocks→bbolt 引擎迭代,`bolt-no-sync=false`)~160MB/s 持续写同一 sda2,叠加外来沙箱 restore。宿主 **IO PSI full=76%**(some=77%,avg300=58%)。r57:exec首击 6046ms(×4.91,run 6 首次破 1.3s 稳态)、bench A=39.7s/B=43.0s(1.34×,未过 2× 回归线)、pause 42.3s(×1.20 最高)、total 1081s;r58 bench A rep1=69.2s。两窗口分别对上实验第一轮(12:03 起 rocks)+restore(12:07)与第二轮(12:38 起 bbolt)。轮次仍 ✓(阈值未破),数据按污染标记。与 r24 的差异:污染源从突发 restore 批变为**长时间恒定写压**,宿主级 PSI 是最快判据(比 journal 数 restore 更直接)。

## 健康面(无问题的部分)

- **数据正确性**:一致性校验 34+8 轮全过,删除不复活、更新不回滚
- **resume**:0.01~0.8s,稳定无漂移
- **内存**:guest 无泄漏(7343→7340MB 8 轮)
- **bench 长期**:无回归(净环境下持平)

## 修复建议优先级

1. **#2 guest cap恢复**(guest-runtime):恢复压力解除后的实际充气进展,再验证快照大小与pause;快照前充气收益尚未验证
2. **#7 conductor 健康检查**(orchestrator):guest 心跳/超时回收,防资源泄漏
3. **#5 信号去抖**(sandboxer):二 SIGTERM 合并 + 查 CH API closed-channel 时序
4. **#3 trim 通道**(sandboxer+guest):diff 盘 discard 支持 → fstrim → presence 回收,解 32GiB 上限死期
5. #1 envd 流控、#6 diffstore 读停顿:先做最小复现再定位
