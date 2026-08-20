# 当前 `sandbox-ctl info` 流程与 fast-fail 边界（2026-08-20）

## 1. 结论

`sandbox-ctl info` 是 **snapshot bundle 的元数据读取器**。它打开一个 snapshot
artifact，从尾部 ZIP 中取出 `snapshot.cfg`；`--json` 再将这份 YAML 解析为
`restore.SnapshotCfg` 并输出 JSON。

它的目的不是执行 restore，也不是证明一个 sandbox 一定能启动成功。当前
orchestrator 主要借它取得两类信息：

1. **root snapshot 的业务属性**：`resources.capacity` 和 metadata 内保存的逻辑网络配置；
   用于 create/resume admission 的资源校验、网络合并和主机侧网络准备。
2. **snapshot graph 的引用属性**：每一层的 `from_refs`，以及 root/data disk 的
   disk artifact refs；用于在启动命令行上生成完整的 `--ref-location` 列表。

因此，当前正确的 fast-fail 边界是：**会影响 admission 的 root 业务属性必须在
accepted 之前读取和校验；只为 runner 构造 `--ref-location` 的整图遍历不必放入
admission，当前也确实是在 accepted 后发生。**

注意：整图扫描即使成功，也只证明 `snapshot.cfg` 都能读到，仍不等于 restore
一定成功；真正的 runtime/base/disk 链、host yaml 匹配、VMM 与网络拓扑校验仍在
`sandbox-ctl run` / restore 路径完成。

## 2. `sandbox-ctl info` 本体做了什么

命令形态：

```text
sandbox-ctl info [--json] [--manifest-config <path>]
                 [--ref-location name=file:///absolute/path] <ref>
```

读取流程：

```text
ref
 ├─ manifest:// 或 file://
 │   └─ sandbox.OpenDiskStream(...)
 │       ├─ manifest://: lazy manifest fetcher → cache/store → fetch.Stream
 │       └─ file://: RefLocations.ResolveFile → local tarstream stream
 └─ plain local path
     └─ fetch.OpenTarStream
             ↓
        fetch.NewReaderAt(stream)
             ↓
        ZIP central directory / snapshot.cfg entry
             ↓
        YAML body
             ↓ (--json only)
        restore.ParseSnapshotCfg
```

`manifest://` 必须有 manifest config；customer key 仍由当前 CLI 的
`MANIFEST_KEY` 环境变量经 manifest config 读取。`file://name@location:foo` 必须
有受信任的 `--ref-location foo=file:///...` 映射；映射缺失会 fail closed。

它实际验证的是“**这个 ref 所指的 bundle 能否按当前 storage/crypto/location
策略打开，并且包含可读的 `snapshot.cfg`**”。使用 `--json` 时还会验证 YAML 能否
unmarshal 成 `SnapshotCfg`。

它不会：

- 自动遍历 `from_refs` 或任何 disk chain；调用者必须逐个调用。
- 校验 `resources.capacity` 是否可用于 admission；这由 orchestrator 做。
- 解析或校验 metadata 中的业务网络 JSON；这由 orchestrator 做。
- 打开 `runtime_ref`、`base_ref`、overlay 或 data disk artifact。
- 执行 `ApplyRules`、容量与 host YAML 的完整匹配、快照内 `config.json` 的网卡拓扑
  检查，或启动 Cloud Hypervisor。

## 3. 当前调用点和时序

| 调用点 | 读取范围 | 用途 | 发生时机 | 失败结果 |
| --- | --- | --- | --- | --- |
| `prepareSandboxLaunch` → `snapshotConfig` | 仅 restore root | capacity + root metadata network | fresh create / paused resume 的 admission | 未 accepted，未持久化 starting row，直接拒绝 |
| `sandboxLaunchSpec` → `snapshotRefLocations` | root 起 BFS 的每个 snapshot.cfg | 收集 `--ref-location` | runner 已分配、通过 config socket 拉 LaunchSpec 时 | 已 accepted；runner 退出，异步 launch rollback |
| `runBuildUnit` → `sourceTemplateNetwork` | build 的 from-template root | build 网络继承 | build worker 内、host attach 前 | build 失败；不是 create admission |
| `BuildSpecFor` → `templateRefLocations` | from-template 的整图 | 给 builder 下发 ref locations | builder unit 已运行、拉 BuildSpec 时 | build 异步失败 |
| builder `resolveBase` | from-template root | 取 base image、overlay 链、start/ready metadata | build pipeline 内 | build 异步失败 |

### 3.1 Create / resume：root 业务读取是同步 preflight

fresh create 在 `acceptFreshLaunch` 中先调用 `prepareSandboxLaunch`，之后才
`Claim`、写入 starting row、发布路由、启动异步 `runLaunch`。paused sandbox 的
resume 也在 `BeginResume` 前走同一个 `prepareSandboxLaunch`。

对于 restore：

```text
prepareSandboxLaunch
  ├─ RestoreRefFor(sb, tmpl)
  ├─ snapshotConfig(root)
  │   └─ inspectSnapshotConfig
  │       └─ sandbox-ctl info --json --manifest-config ... <root>
  ├─ 从 snapshot.cfg 取 resources.capacity
  ├─ MergeNetwork(snapshot metadata, request metadata)
  ├─ ResolveResources(... SnapshotCapacity ...)
  └─ resolveNetwork(...)
```

当前 `snapshotConfig` 对 capacity 的明确要求是 `CPU > 0` 且 `Memory != ""`。
请求资源与 snapshot capacity 的一致性、memory 格式和 node policy 由
`ResolveResources` 继续处理；最终网络字段由 `resolveNetwork` 校验。

root 的 network metadata 当前是 best-effort：`snapshotConfig` 对
`kuasar-sandbox.network` 做 `json.Unmarshal`，但忽略 unmarshal error。也就是说，
malformed root network metadata 在 sandbox create/resume 中**不会**因该字段立即
失败，而是当作“没有可继承网络”并使用 request/node default。这个行为与 build
路径不同（build 会严格 parse，见下文）。

成功的 probe 会按 `(manifestKey, ref)` 放进进程内 `refCache`；失败不缓存。因此
“会调用 info”不等于每次都会 fork：warm hit 不执行 CLI。

### 3.2 accepted 后：runner 拉 LaunchSpec 时扫描 graph

异步 `runLaunch` 完成目录、TAP、YAML、readiness socket 和 runner assignment 后，
`node-ctl run-sandbox` 通过 config socket 请求 `sandbox:<sid>` 的 `LaunchSpec`。

```text
node-ctl run-sandbox
  └─ FetchLaunchSpec("sandbox:<sid>")
       └─ Orchestrator.LaunchSpecFor
            └─ sandboxLaunchSpec
                 └─ sandboxRefLocations
                      └─ snapshotRefLocations(root)
                           └─ BFS: sandbox-ctl info --json <each snapshot ref>
```

这段 BFS 做的是：

1. 对每个 snapshot ref 读取其 `snapshot.cfg`。
2. `from_refs` 作为下一层 snapshot node 入队。
3. 将 root/data disk node 中出现的 artifact ref 提取出来，仅收集其中的
   named `file://...@location:<name>` location。
4. 将收集到的 map 渲染为 `sandbox-ctl run --ref-location name=file:///...`。

它不打开那些 disk artifact，也不把 `Boot.RuntimeRef` 当成 tenant named location。
成功 graph 结果同样在 `refCache` 缓存；循环以 `seen` 去重，并限制最多 1024 个
snapshot node。

若此处失败，config socket 返回 500；`node-ctl` 无法 exec 到 `sandbox-ctl run`，
readiness connection 关闭，`runLaunch` 以异步 launch failure 回滚。此时 create/resume
早已 accepted，故它不是同步 API fast-fail。

### 3.3 Build 有两次不同目的的 root/graph 读取

Build 的 `sourceTemplateNetwork` 在 `runBuildUnit` 内、TAP attach 前读取 root
`snapshot.cfg`，以继承网络。它会用 `sandboxcfg.ParseSpec` 严格解析 metadata；坏的
network JSON 会使 build 失败，而不是降级为默认网络。

随后 `BuildSpecFor` 在 builder 拉取工作单时扫描整图以生成 `RefLocations`。进入
builder pipeline 后，`resolveBase` 还会直接运行一次 `sandbox-ctl info --json`，取
`boot.root.base_ref`、overlay chain 和 E2B start/ready metadata。它们目前没有共享
一次解析结果，所以 source-template build 的同一 root 在冷路径可被读取多次。

## 4. 哪些应当 fast-fail

这里的 fast-fail 指：在 create/resume 返回 accepted、starting row 持久化、路由发布、
TAP attach 或 runner assignment 之前返回失败。它不是泛指“尽早失败”。

| 检查 | 是否应在 create/resume admission fast-fail | 原因 | 当前状态 |
| --- | --- | --- | --- |
| restore root ref 的语法、所需 location 映射、storage/crypto 配置 | 是 | 无法读取 root 就无法得到 restore 的资源合同 | 是，随 root `info` 失败 |
| root bundle 可打开、ZIP 中存在 `snapshot.cfg`、YAML 可解析 | 是 | capacity/network 的唯一来源不可用 | 是 |
| `resources.capacity` 非空且可被资源规则接受 | 是 | restore runtime 固定该 capacity，不能在 accepted 后才发现请求不可能满足 | 是：CPU/memory presence + `ResolveResources` |
| request capacity 与 snapshot capacity 的冲突 | 是 | 是请求不可满足，而非可重试的启动波动 | 是，`ResolveResources` |
| 从 root + request 得到的最终 network 配置是否合法 | 是 | 后续 host TAP attach 依赖它 | 是，`resolveNetwork` |
| root network metadata 本身为 malformed | 取决于兼容性契约；若 metadata 是可信且必须继承，应是 | 否则会静默改成 default network | **create/resume 当前否**（best-effort）；**build 当前是** |
| `from_refs` graph 是否完整、每层 `snapshot.cfg` 是否可读 | 否，不因 admission 目的而必须 | 不参与 capacity/network/TAP 的 admission 决策；当前是 runner 启动输入准备 | 当前在 accepted 后 |
| disk artifact 是否存在、可解密、digest 正确 | 否，不能靠 info 完成 | `info` 不打开它们；真正需要时由 `sandbox-ctl run`/restore 打开 | 当前在 runtime restore |
| runtime/base 与 host config 的匹配、disk count、网卡拓扑 | 否，不能靠 info 完整判断 | 是 sandboxer restore 合同，需 `ApplyRules` / `config.json` / host runtime 环境 | 当前在 `sandbox-ctl run` |

其中前四项是必须保留的同步 root probe 的理由；它们决定“这个请求是否允许进入
starting”。整图 location 收集则是启动命令行构造的一部分，当前属于 accepted 后
的 runner 路径。

## 5. 现状的关键边界与容易误解之处

### `info` 成功不等于 restore 可用

`info` 只需读 ZIP trailer 里的 `snapshot.cfg`。例如 base artifact 缺失、某个
`from_refs` 指向的 memory layer 不可读、host runtime 和 `runtime_ref` 不匹配，仍可能
在 `info` 成功后由 restore 失败。因此，把整图 info 提前到 admission 会增加同步耗时，
但不能把启动失败全部转成 fast-fail。

### root `info` 与 graph `info` 的输出需求不同

root admission 仅消费：

```text
resources.capacity
metadata["kuasar-sandbox.network"]
```

graph BFS 消费：

```text
from_refs
boot.root / boot.disks 中的 disk artifact refs
```

两处目前各自定义 JSON mirror struct：`snapshotDescription` 与
`snapshotRefConfig`。这是同一份 `SnapshotCfg` 的两个投影，而不是两个不同的
snapshot 文件格式。

### 当前缓存只能避免重复 CLI，不能把两种投影合并

`refCacheEntry` 分别存 `snapConfig` 和 `locations`。因此 root capacity probe 命中
不代表 graph walk 已命中；首次 create 的 graph walk 仍会再次对 root 调一次
`sandbox-ctl info`。反过来也一样。成功的 graph walk 才能让后续相同
`(manifestKey, root)` 直接复用 location map。

## 6. 对后续优化的约束（仅由当前行为推出）

若目标是消除 `sandbox-ctl info` 的命令行初始化耗时，替换为 in-process reader 时，
至少要保持以下时序和错误语义：

1. admission 的 root read 仍在 accepted 前完成，并保留 capacity/network 的
   fast-fail 行为。
2. runner 的 graph read 仍可在 LaunchSpec 构造阶段完成；它失败应走现有异步
   launch rollback，而不是意外改变成 create API 的同步失败。
3. reader 必须保留 manifest/file/local 三种 ref 的打开规则、crypto policy 和
   named location fail-closed 规则。
4. `RefLocations` 对外是 `file:///absolute/path` flag；sandboxer 内部解析后保存的是
   absolute filesystem path。两种表示不能混用。
5. 不应把“读取了 snapshot.cfg”表述为“预验证整个 restore”。若要扩大 fast-fail
   范围，需要另行设计一个真正覆盖 artifact chain 与 restore 合同的 preflight。

## 7. 代码索引

- `orchestrator/internal/orch/orch.go`
  - `prepareSandboxLaunch` / `snapshotConfig`：admission root probe。
  - `acceptFreshLaunch`、`ensureResumeAcceptedPrepared`：accepted 前后的边界。
  - `sandboxLaunchSpec`：runner 拉 LaunchSpec 时的 graph probe。
- `orchestrator/internal/orch/ref_locations.go`
  - BFS、artifact location 提取和 `refCache`。
- `orchestrator/internal/orch/build.go`
  - `sourceTemplateNetwork`、`BuildSpecFor`。
- `orchestrator/internal/builder/builder.go`
  - `resolveBase` 的 builder-side `sandbox-ctl info`。
- `sandboxer/cmd/sandbox-ctl/info_cmd.go`
  - CLI 的打开、ZIP 读取与 JSON 输出。
- `sandboxer/pkg/restore/applyrules.go`
  - `SnapshotCfg` schema，以及不属于 info 的 restore 合同校验。
