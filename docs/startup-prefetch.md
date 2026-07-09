# 启动热点 Profile 与 Restore L1 预热实施方案

## 1. 背景与目标

标准生产生命周期是：

```text
Cold Start
  → 启动窗口记录 memory page / disk block offsets
  → Snapshot 冻结 Profile
  → startup-profile.json 写入 snapshot trailing ZIP
  → Snapshot 分块、去重、上传
  → Restore 读取 Profile
  → offset 通过当前 manifest/layer 解析成 Chunk key
  → 后台拉取 ciphertext Chunk
  → cache-ctl 回填本机 L1
```

当前 Restore 有两条 demand 路径：

```text
memory:
Guest fault → UFFD → manifest/cache fetch → UFFDIO_COPY

disk:
Guest read → virtio-blk → vhost-user-blk → manifest/cache fetch
```

本方案的目标是提前完成其中延迟最不稳定的远端 Chunk fetch，让 Guest 恢复后访问启动关键数据时尽量命中节点 L1 Cache。

本方案不做以下事情：

- 不主动写 Guest memfd。
- 不替代 `UFFDIO_COPY`。
- 不改变 FastCDC 或 dedup 算法。
- 不处理本地 `file://` 的 `readahead/fadvise`。
- 不要求 Cloud Hypervisor 启动前等待全部预热完成。

## 2. 收益边界

优化前：

```text
Guest 恢复运行
  → 首次访问启动数据
  → L1 miss
  → 等待远端 store
  → 校验、解密、复制
  → Guest 继续运行
```

优化后：

```text
sandbox-ctl 解析热点 Profile
  → 后台拉取热点 ciphertext Chunk
  → cache-ctl 回填节点 L1
  → Guest 随后访问
  → L1 hit
```

预期收益主要体现在：

- 降低 `vm.resume` 后首个有效请求的 p99/p99.9。
- 降低 UFFD page-in 和 vhost block read 的远端 fetch 尾延迟。
- 同节点密集恢复相同快照时复用同一批 L1 Chunk。
- 降低远端 store QPS、网络流量和 cache stampede。

该方案不会减少快照大小、提高去重率或消除 UFFD/vhost 的 CPU 复制。对完全本地且 Host Page Cache 已热的 `file://` 路径，收益也会很小。

## 3. 为什么 Profile 保存逻辑 offset

Profile 不保存 cold-start 阶段看到的 Chunk key，而保存稳定的逻辑地址：

```text
logical target + offset/range
  → current manifest ChunkEntry.Offset/Size
  → current CiphertextHash
  → L1 cache object
```

原因是快照创建会重新对 memfd 和 writable diff 分块。FastCDC 边界可能改变，snapshot merge/flatten 也可能改变真正提供数据的 layer。冷启动源的 Chunk key 不一定等于最终快照的 Chunk key。

分块和 dedup 不会改变逻辑地址空间：

- FastCDC 可以改变 Chunk 边界，但 manifest 仍保存每个 Chunk 的 `Offset` 和 `Size`。
- Dedup 只让多个 manifest entry 引用相同 `CiphertextHash`，不会重排逻辑 offset。
- 内容改变会产生新 hash；旧 L1 对象不会再被新 manifest 引用，之后由容量策略淘汰。
- Restore 使用当前 layered resolver重新解析 offset，因此能找到当前 snapshot 真正需要的 Chunk。

对于 layered source：

```text
profile offset
    │
    ▼
captured top
    ├─ Data → 预热 top layer Chunk
    ├─ Zero → 不预热
    └─ Hole → 查 base_from_refs 下一层
                    ├─ Data → 预热该层 Chunk
                    └─ Hole → 继续回落
```

## 4. Profile 数据模型

快照 trailing ZIP 新增可选成员：

```text
startup-profile.json
```

建议的 v1 格式：

```json
{
  "version": 1,
  "window_ms": 2000,
  "block_size": 4096,
  "targets": [
    {
      "kind": "memory",
      "size": 8589934592,
      "ranges": [
        {
          "offset": 20971520,
          "length": 8192,
          "order": 0,
          "hits": 1
        }
      ]
    },
    {
      "kind": "disk",
      "name": "root.base",
      "size": 4294967296,
      "ranges": [
        {
          "offset": 1048576,
          "length": 65536,
          "order": 1,
          "hits": 3
        }
      ]
    }
  ]
}
```

稳定 target 命名：

```text
memory
root.base
root.cow
disk0.base
disk0.cow
disk1.base
disk1.cow
...
```

Profile 不包含时间戳、sandbox ID、socket path、临时目录或绝对文件路径。相同冷启动访问和相同 VM 内容应生成确定性一致的 Profile 和 ZIP。

## 5. Accelerator 修改方案

### 5.1 新增 `pkg/manifest/fetch/prefetch.go`

增加逻辑范围和预热统计：

```go
type Range struct {
    Offset uint64
    Length uint64
    Order  uint32
}

type PrefetchStats struct {
    RequestedRanges uint64
    ResolvedChunks  uint64
    UniqueChunks    uint64
    Hits            uint64
    Misses          uint64
    Errors          uint64
    SkippedHole     uint64
    SkippedZero     uint64
    SkippedFile     uint64
}

func PrefetchRanges(
    ctx context.Context,
    stream Stream,
    ranges []Range,
    concurrency int,
) PrefetchStats
```

内部使用可选能力接口，不扩大基础 `Stream` 的强制契约：

```go
type chunkResolver interface {
    resolveChunkKeys(r Range) ([]store.ContentKey, ResolveStats)
}

type chunkPrefetcher interface {
    prefetchChunk(ctx context.Context, key store.ContentKey) error
}
```

`PrefetchRanges` 的行为：

- 按 `Range.Order` 调度。
- 校验整数 overflow、stream size 和 range 边界。
- 解析覆盖 range 的所有 Data Chunk。
- 对 Chunk key 全局去重。
- 使用固定 worker pool，默认并发 4。
- 每个 Chunk 只调用 `cache.Get(ctx, store.PartitionChunk, key)`。
- hit Blob 必须立即 `Release()`。
- 单 Chunk 失败只更新统计，不中断其他 Chunk。
- context 取消后停止派发新任务。

### 5.2 修改 `pkg/manifest/fetch/fetch.go`

为 `manifestStream` 增加内部 range 解析和 ciphertext prefetch 能力：

- 复用 `RunChunkAt()` 找到 range 覆盖的 manifest entry。
- Data 返回 `CiphertextHash`。
- Zero 计入 `SkippedZero`。
- Hole 计入 `SkippedHole`。
- 预热直接调用 `manifestStream.cache.Get()`。
- 不调用 `readChunkInto()`。
- 不执行 hash verification、`DecryptInPlace` 或明文 `copy`。

### 5.3 修改 `pkg/manifest/fetch/layered.go`

为 `layeredStream` 增加 range 解析：

- 复用现有 `resolve()`。
- upper layer Hole 时向下层回落。
- Data 使用真正 serving layer 的 Chunk key。
- Zero 不向下层回落。
- 跨 layer/run 边界时拆分 range。
- 多个 layer 返回的相同 key 交给全局 dedup。

### 5.4 新增 `pkg/manifest/fetch/prefetch_test.go`

覆盖：

- 单 manifest offset 映射到正确 Chunk key。
- range 跨多个不等长 Chunk。
- layered Hole fallthrough。
- Zero 不 fetch。
- file/tar stream 跳过。
- 重复 key 只 fetch 一次。
- worker 最大并发不超过配置值。
- context cancel 后停止派发。
- 每个 hit Blob 恰好 `Release()` 一次。
- decryptor 和 `Stream.ReadAt` 设为 panic，证明预热不进入明文路径。

## 6. Sandboxer 冷启动 Recorder 修改方案

### 6.1 新增 `pkg/profile/startup.go`

增加并发安全的 Recorder：

```go
type TargetID struct {
    Kind string
    Name string
    Size uint64
}

type Recorder struct {
    // concurrent target/block state
}

func NewRecorder(window time.Duration, maxBlocks int) *Recorder
func (r *Recorder) Start()
func (r *Recorder) RecordMemory(offset uint64)
func (r *Recorder) RecordDisk(target TargetID, offset, length uint64)
func (r *Recorder) Freeze() Profile
func (r *Recorder) Marshal() ([]byte, error)
```

实现规则：

- 默认按 4 KiB 粒度归一化。
- 相同 target+block 去重。
- 首次访问分配全局 order。
- 重复访问累计 hits。
- Freeze 后 Record 为 no-op。
- Marshal 时合并连续 blocks。
- 默认最多 8192 blocks，超限后保留最早 block并累计 dropped。
- JSON 输出确定性排序，序列化大小限制为 1 MiB。
- vhost/UFFD 热路径不执行 JSON 或文件 I/O。

### 6.2 修改 `pkg/uffd/handler.go`

在 Handler 配置中增加可选 callback：

```go
OnFaultResolved func(memfdOffset uint64)
```

只在以下条件全部满足时调用：

- 当前是 cold-start handler。
- faulting page 已成功解决。
- Recorder 窗口仍 active。

只记录 `pageOffset` 对应的单个 4 KiB faulting page，不能记录 `runBytes`。后者包含 UFFD batching 顺带安装、但 Guest 未必真正访问的邻居页。

UFFD batching 预先安装的邻居页之后不会再次 fault，因此内存热点是 best-effort 观测。MVP 不为提高记录完整度而关闭 batching。

### 6.3 修改 `pkg/vhost/server.go`

给每个 Server 绑定稳定 target：

```go
func (s *Server) SetStartupRecorder(
    target profile.TargetID,
    recorder *profile.Recorder,
)
```

### 6.4 修改 `pkg/vhost/worker.go`

在 `BlkTypeIn` 成功完成后调用：

```go
recorder.RecordDisk(target, requestOffset, uint64(bytesIO))
```

只记录成功读取的实际字节。Write、Flush、Discard 和错误请求不记录。

### 6.5 修改 `pkg/sandbox/serveandwait.go`

创建 vhost server 时分配稳定 target：

- 单盘：`root.cow` / `diskN.cow`。
- Overlay：`root.base/root.cow`、`diskN.base/diskN.cow`。

同一个 Recorder 同时传给 UFFD 和全部 vhost server。

### 6.6 修改 `pkg/sandbox/lifecycle.go`

只在 cold-start `PostSpawn` 中启动记录：

```text
等待 LaunchServer.HelloDone()
  → Recorder.Start()
  → 默认 2 秒后自动 Freeze
```

`HelloDone` 表示 Host 已向 sandbox-init 写出 launch spec，能覆盖应用 exec、动态链接和首次依赖加载。Restore 路径不启动 Recorder，只消费快照中的已有 Profile。

## 7. Snapshot 封装修改方案

### 7.1 修改 `pkg/snapshot/take.go`

扩展 `Sources`：

```go
StartupProfile func() ([]byte, error)
```

CH pause 和 vhost quiesce 后：

- Freeze Recorder。
- 序列化 Profile。
- Profile 非空时加入：

```go
entries["startup-profile.json"] = profileBytes
```

Profile 是性能提示数据。生成失败时记录 warning 并继续生成不带 Profile 的快照，不能破坏快照主流程。

### 7.2 修改 `pkg/sandbox/lifecycle.go`

`SnapshotHandler` 持有 Recorder，并将获取冻结 Profile 的回调传给 `snapshot.Sources`。

如果应用未进入记录窗口或 Profile 为空，则省略 ZIP 成员。

### 7.3 修改 `pkg/restore/upload.go`

当前本地 snapshot upload 重建 ZIP 时只复制三个固定成员，需要同时保留可选的：

```text
startup-profile.json
```

重写 `snapshot.cfg` 时不修改 Profile 的逻辑 offsets。老快照没有该成员时保持现有行为。

## 8. Restore 主动预热修改方案

### 8.1 修改 `pkg/restore/restore.go`

解析 trailing ZIP 时：

- `config.json`、`state.json`、`snapshot.cfg` 仍为必需成员。
- `startup-profile.json` 为可选成员。
- 缺失、损坏、未知版本、过大或非法 range 时仅记录 warning。
- Profile 错误绝不导致 Restore 失败。

完成 memory/disk layer 构建后建立 target 映射：

```go
map[string]fetch.Stream
```

示例：

```text
memory    → fetch.NewLayered(memLayers...)
root.cow  → captured top + base_from_refs
root.base → root EROFS base stream
diskN.*   → 对应数据盘 stream
```

然后启动一个后台 prefetch coordinator：

- 所有 target 共用一个并发预算，默认 4。
- 按全局 order 调度。
- 调用 `fetch.PrefetchRanges()`。
- 不等待预热完成再启动 CH。
- Restore context 取消时停止。
- 单个 target 不存在、size 不一致或 range 越界时只跳过该 target。
- 保证 prefetch goroutine 结束或取消前相关 stream 不被关闭。

### 8.2 COW 与 Overlay 映射

- `*.base` range 在对应只读 base stream 上解析。
- `*.cow` range 在 captured-top + `base_from_refs` layered stream 上解析。
- 如果 cold-start 读取来自 writable diff，快照后的 captured top 提供该 offset。
- captured top 为 Hole 时，按实际 Restore 语义向 lower layer 回落。
- Restore 新建的空 writable diff 不参与预热。

### 8.3 新增 `pkg/restore/prefetch_test.go`

覆盖：

- 无 Profile 的旧快照。
- Profile JSON 损坏或未知版本。
- memory/disk target 正确绑定。
- target size 不匹配。
- range 越界。
- prefetch 被阻塞时 Restore 主流程仍立即继续。
- context cancel 后 worker 回收。
- 预热错误不影响 Restore。
- fake UFFD/memfd sentinel 证明预热不写 Guest memory。

## 9. CLI 与可观测性

修改 `cmd/sandbox-ctl/run.go`，增加：

```text
--startup-profile-window=2s
--startup-profile-max-blocks=8192
--prefetch-concurrency=4
```

语义：

- `window=0`：cold-start 不记录，Restore 仍可消费已有 Profile。
- `max-blocks=0`：使用默认值。
- `prefetch-concurrency=0`：解析但不执行预热。
- 非法负值在 CLI 参数解析阶段报错。

日志：

```text
startup-profile: recording started window=2s
startup-profile: frozen memory_blocks=N disk_blocks=M dropped=D
snapshot: embedded startup-profile bytes=N
prefetch: loaded targets=N ranges=N
prefetch: completed unique_chunks=N hits=H misses=M errors=E duration=...
```

cache-ctl 的 TieredCache Fill 当前是异步的：Getter hit 表示已经取得 Chunk 并触发 L1 回填，不保证日志出现时 RocksDB Fill 已经持久完成。MVP 不增加 Restore 等待屏障。

## 10. 验证方案

### 10.1 单元和 race 测试

```bash
# accelerator
go test -race ./pkg/manifest/fetch
go test ./pkg/cache/...
go test ./...
go vet ./...

# sandboxer
go test -race ./pkg/profile ./pkg/uffd ./pkg/vhost
go test ./pkg/snapshot ./pkg/restore ./pkg/sandbox
go test ./...
go vet ./...
```

### 10.2 端到端 A/B

```text
1. Cold-start 应用并记录 2 秒热点
2. 创建 manifest snapshot
3. 清空节点 L1
4. A：关闭 prefetch Restore
5. 再次清空 L1
6. B：开启 prefetch Restore
```

采集：

- `vm.resume → first successful request` p50/p99/p99.9。
- demand remote Chunk fetch 数量和 p99。
- UFFD page-in p99。
- vhost block read p99。
- cache L1 hit ratio。
- 预取字节、实际使用字节和浪费率。
- 同快照并发 100 sandbox 时的 store QPS、网络字节和启动成功率。

第一版建议验收门槛：

- Profile Chunk 使用率不低于 70%。
- demand remote miss 至少下降 50%。
- 首请求 p99 至少下降 20%。
- 同节点多实例的远端 Chunk fetch 至少下降 70%。
- 预取字节不超过实际启动使用字节的 1.5 倍。
- Profile 缺失、损坏或 fetch 失败时，Restore 行为与当前版本一致。

## 11. 实施顺序

1. 在 accelerator 实现并测试 offset → Chunk key 的只读解析和 ciphertext prefetch。
2. 在 sandboxer 实现独立 Recorder，并接入 UFFD/vhost 成功路径。
3. 使用 `HelloDone()` 控制 cold-start 的 2 秒记录窗口。
4. 将确定性 Profile 写入 snapshot ZIP，并修复 upload 时的附加成员保留。
5. 在 Restore 中绑定 Profile target、启动后台预热并补齐 fail-open 行为。
6. 增加 CLI、日志和统计。
7. 完成单元、race、全量测试及冷 L1 A/B benchmark。

## 12. 风险与约束

- UFFD batching 会让部分邻居页不再产生后续 fault，内存热点存在欠采样；MVP 不关闭 batching。
- Profile 太宽会污染 L1，因此必须限制窗口、最大 block 数和预取并发。
- 多 sandbox 同时预热可能放大 store burst；所有 target 必须共享同一并发预算。
- Profile 只是性能提示，任何解析或 fetch 错误都必须 fail-open。
- Snapshot 的设备拓扑或 size 不匹配时跳过相关 target，不能猜测映射。
- file-backed target 不具备远端 L1 Chunk，第一版直接跳过。
