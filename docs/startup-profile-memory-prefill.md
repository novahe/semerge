# Startup Profile 并发内存预填充

## 目标

一次 cold start 自动记录应用启动阶段实际触发的内存页，并将 `startup-profile.json` 写入 snapshot。Restore 时，`sandbox-ctl` 根据 Profile 并发拉取热点页，通过 `UFFDIO_COPY` 注入 CH 内存。

该机制替代外部 benchmark 生成热点列表；benchmark 只用于 A/B 验证，不参与生产 Profile 生成。

## 并发模型

```text
spawn cloud-hypervisor
  ├─ CH restore、cgroup/API 等宿主初始化
  └─ Profile worker pool → fetch → local buffer → UFFDIO_COPY
                                             ↓ 完成或超时
                                          vm.resume → Guest demand fault
```

宿主侧路径同时运行，仅在放行 vCPU 前建立安全边界：

- prefill 先完成：`UFFDIO_COPY` 原子安装完整页面；
- CH restore 已经建立 folio：prefill 得到 `EEXIST` 并跳过；`EAGAIN` 做有限重试；
- prefill 永远不直接写 `backendVA`，因此不会用旧 snapshot 数据覆盖 Guest 已修改页面，也不会暴露半写页面。

只有 `vm.resume` 等待 prefill 完成或超时；CH restore、cgroup 放置、API readiness 和其他宿主侧启动动作仍与 prefill 并发。新增等待是 `max(0, prefill耗时 - 其他启动动作耗时)`；若 prefill 在 CH API ready 前完成，则不增加 resume 延迟。该边界确保 Guest demand fault 和 balloon/PUNCH_HOLE 不会进入 prefill 的页面代际窗口。

worker 受 `backendCtx` 管理并加入 `backendWG`。CH 退出时先取消并等待所有 prefill worker，再关闭 handler、uffd fd 和 memfd。

## Cold-start Profile 生成

```text
Cold Start
  → UFFD handler ready
  → LaunchServer.HelloDone()
  → Recorder.Start()
  → 2 秒记录窗口
  → Snapshot 或窗口到期时 Freeze()
  → startup-profile.json 写入 trailing ZIP
```

记录窗口内 UFFD batching 与 Profile 采集互斥：

```text
Recorder 未启动/采集中 → 每次 StateAbsent fault 只解决 1 页
Recorder Freeze 后       → 恢复最多 256 页 batching
```

如果采集时仍批量安装256页，只记录 fault page 会欠采样，记录整个 batch 又会把推测邻页误判为热点。因此记录窗口必须使用单页模式。

Recorder 规则：

- 只记录成功解决的 `StateAbsent` fault；
- 保存 memfd page offset，不保存 CH VA；
- PAGEFAULT 进入 host queue 时分配 order，解决成功后提交；相同页去重并保留最小 order，因此慢 fetch 不会改变 Guest 观测到的触页顺序；
- 默认窗口2秒，最多16384页（64 MiB）；
- 只合并“触页顺序连续且地址递增相邻”的页面，逆序或交错相邻页保持独立 range；
- UFFD 只能观察首次触页，v1 的 `hits` 不代表真实访问频率；
- Freeze 后 Record 为 no-op。

## Profile 格式

```json
{
  "version": 1,
  "page_size": 4096,
  "window_ms": 2000,
  "targets": [{
    "kind": "memory",
    "size": 8589934592,
    "ranges": [{
      "offset": 20971520,
      "length": 131072,
      "order": 0,
      "hits": 1
    }]
  }]
}
```

解析必须校验：版本、页大小、memory size、页对齐、overflow、越界、零长度、Profile 大小和 range 数量。重叠 range 直接拒绝，相邻 range 保持原始 `order`，避免解析阶段破坏 Guest 触页顺序。

## Restore prefill

1. Restore 从 trailing ZIP 解析可选 `startup-profile.json`。
2. memory source 和 Profile 可用后立即启动有界 fetch plan，与磁盘重建、socket/server 初始化、CH spawn/restore 并发。
3. fetch job 按 `order+offset` 预留 buffer，多 worker 可乱序完成，但注入阶段按原顺序消费。
4. CH 上报首个 va_report 后，Handler 建立 memfd offset 到 `(uffd fd, chVA)` 的正向映射，并逐页调用 `UFFDIO_COPY`。
5. API ready 时若 handler+注入已完成，立即执行 `vm.resume`；否则只等待剩余工作，最多消费100ms额外等待预算。预算耗尽后取消并等待唯一的注入协程退出，随后立即 resume；远端 fetch worker 在后台回收，且它们不能调用 `UFFDIO_COPY`。
6. Zero/Hole 不注入，继续保持 lazy zero semantics。
7. 多 region 场景中，高区 range 等待对应 va_report 注册，绝不回退成直接写 memfd。

使用逐页 `UFFDIO_COPY` 是有意选择：批量 ioctl 返回 `EEXIST/EAGAIN` 时不容易判断哪部分成功；逐页调用使每次竞争结果明确，优先保证不会覆盖 Guest 数据。远端 fetch 和解密仍按最多1 MiB run执行，多个 range worker继续并发。

默认预算：

```text
MaxBytes = 128 MiB
Timeout  = 100 ms  # 从 CH API ready 开始计算的 vm.resume 额外等待预算
Workers  = 8
BufferBytes = 64 MiB
```

预算先按 `order` 选择 range，再交给 worker，避免 goroutine 调度让晚期 range 抢占早期启动热点预算。统计区分 requested、filled、existing、zero 和 error，失败或 Zero 不计为 filled。

## Snapshot 与 upload

Snapshot 在 pause/quiesce barrier 后 Freeze Recorder，将非空 Profile 加入：

```text
config.json
state.json
snapshot.cfg
startup-profile.json
```

Profile 是可选性能元数据；生成失败只记录 warning，不能导致 snapshot 失败。`upload-snapshot` 重建 ZIP 时必须原样保留该成员。旧快照没有 Profile 时 Restore 保持原有 UFFD lazy-load。

## 多代增量更新

当带 Profile 的快照 Restore 后再次生成子快照时，Restore 会在 prefill 停止后、`vm.resume` 前开启新一轮2秒记录窗口。下一代 Profile 按以下规则生成：

- 父 Profile 中的页面和优先级保留；
- 本代新触发的 UFFD demand fault 去重后追加；
- 已存在于父 Profile 的页面不会重复加入；
- 合并后的总页数仍受16384页（64 MiB）限制，Profile 不会随代际无限增长；
- 多次 `snapshot → restore → snapshot` 重复相同过程，因此支持连续增量更新；
- 没有父 Profile 的旧快照仍只消费普通 lazy-load，不在首次 Restore 临时学习，保持 cold-start 学习边界。

Profile 仍然只保存逻辑内存 offset。子快照页面内容发生变化时，后续 prefill 会从子快照自己的 memory source 读取新内容，不会复用父快照的页面字节。

## 指标

```text
prefill_requested_bytes
prefill_filled_bytes
prefill_existing_bytes
prefill_skipped_zero
prefill_errors
prefill_duration_ns
prefill_resume_wait_ns
prefill_timed_out
prefill_budget_exhausted
faults_absent
copy_calls
pages_copied
page_in_fetch_p99
```

`prefill_existing_bytes` 表示 CH restore 或其他 worker 已先建立可用页面，属于安全竞争结果，不是数据错误；`StateReleased` 生命周期跳过不会混入该指标。

`prefill_resume_wait_ns` 是 CH API ready 后 `vm.resume` 实际因 prefill 增加的等待；它接近0表示 prefill 完全被 CH restore/宿主初始化遮蔽。100ms是尚未完成时的最大等待预算，不是每次启动都固定等待100ms。

`prefill_timed_out=1` 表示 API ready 后100ms预算耗尽；此时先取消并等待注入协程退出，再执行 `vm.resume`，因此运行中的 Guest 不会与残留 COPY 竞争。远端 fetch worker 的退出不阻塞 resume，由 VM 生命周期在后台回收。

## 验证

- Profile parser：乱序、重叠、相邻、越界、零长度、超限。
- Recorder：并发去重、窗口内单页、Freeze 后恢复256页、Snapshot 提前 Freeze。
- UFFD：验证 prefill 在 `vm.resume` 前完成或超时，CH 已有 folio 时 COPY 返回 EEXIST 且不会覆盖。
- Multi-region：跨 PCI hole ranges 使用正确 uffd fd 和 chVA。
- 生命周期：CH 退出、timeout、context cancel 后 worker 全部退出，再关闭 handler。
- Snapshot/upload：Profile round-trip 字节保持一致。
- Linux `go test -race` 与真实 Restore smoke。

A/B 重点观察首请求 p99、Profile 热页 fault 数、远端 fetch、CH RSS、balloon 回收和总启动时间。并发 prefill 的价值必须以端到端 `start → first request` 改善为准，而不是仅看 prefill 自身吞吐。
