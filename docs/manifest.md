# manifest — 内容寻址存储的统一出入口

`manifest-ctl` 是数据进出"内容寻址存储"的统一入口。任何字节流(EROFS
镜像、内存快照、磁盘镜像、用户文件)经 manifest-ctl `store` 写入后,产出
一个**Manifest** —— 一份包含 chunk 元数据 + 加密的密钥表的小型二进制文
件。Manifest 是后续读取(`load`)与跨节点引用(`manifest://<key>`)的
唯一句柄。

## 1. 概述

### 1.1 模块定位

```
   ┌─ input source ──┐      ┌─ manifest-ctl store ──────────────┐      ┌─ output ──────────┐
   │  sparse.Source  │ ───► │  chunker (data segments)          │ ───► │  Manifest         │
   │  (file/stdin/   │      │      ↓                            │      │  (binary file or  │
   │   tar stream)   │      │  convergent encrypt               │      │   stdout)         │
   └─────────────────┘      │      ↓                            │      └───────────────────┘
                            │  store-ctl Put(chunk_hash) ───────┼───►  chunk bytes land in
                            └───────────────────────────────────┘      store-ctl backend
```

读取方向相反:`manifest-ctl load` 从 Manifest 取 chunk 列表 → 经 cache-ctl
(若配置)穿到 store-ctl `Get` → 解密 → 输出明文流。

### 1.2 设计原则

- **写入路径**:`sparse.Source → chunker → encrypt → store.Put → Manifest`
  (Hole 段记为 manifest 空洞、不读取;Zero 段合成零字节过 chunker、免取数)
- **读取路径**:`Manifest → fetch → decrypt → io.Writer`
- **去重单位**:chunk(可变长 FastCDC 或固定长度)
- **跨用户隔离**:salt 区分 dedup 域,加密 + content-addressing 让 store 只
  见密文

### 1.3 不做什么

- 不直接管理后端字节(委派给 [`store.md`](store.md));
- 不维护缓存(委派给 [`cache.md`](cache.md));
- 不感知 docker / OCI / EROFS 等高层格式(它只看字节流);
- 不做 ACL —— customer key 的物理保护是用户责任。

## 2. 命令行接口

### 2.1 公共 flag

```
Global Flags:
  --manifest-config string    清单配置 YAML 路径(覆盖 MANIFEST_CONFIG 环境变量)
```

`--manifest-config` 与 `MANIFEST_CONFIG` 至少需要一项指向有效 YAML;两者
都缺则命令报错(`config generate` 子命令除外)。配置文件 schema 见 §3.1。

### 2.2 子命令一览

| 命令 | 功能 |
|---|---|
| `store`         | 写入数据 → 自动上传 manifest 并返回 hex content key |
| `load`          | 按 manifest key 读取明文数据 |
| `get-manifest`  | 按 hex key 取回 Manifest 原始字节(debug 用) |
| `info`          | 打印本地 Manifest 文件摘要(无需 customer key) |
| `verify`        | 通过 fetch 路径端到端验证 store 中 manifest 可用 |
| `diff`          | 比较两个本地 Manifest 的 chunk 重叠度(去重率分析) |
| `config show`   | 打印解析后的配置 YAML |
| `config generate` | 输出带注释的配置模板 |

输入 / manifest key 均为**位置参数**(匿名):`store` 的数据源、`info`
的 manifest 来源省略或 `-` = stdin;`load` / `get-manifest` / `verify`
以及 `info` 的 key 可带可选 `manifest://` 前缀(`manifest://<hex>` 与
裸 `<hex>` 等价)。位置参数须置于 flags 之后(Go stdlib flag 在首个非
flag 实参处停止解析)。

### 2.3 `manifest-ctl store` — 数据写入

```
manifest-ctl store [flags] <path|->        # <path|-> 省略或 - = stdin

Flags:
  --extra-salt string         额外 salt 字节,叠加到 store 提供的 opaque salt
  --no-progress               禁用进度输出
```

输入是 **tarstream 工件**(平台镜像/快照的统一容器):size 与洞图都在信封里,
stdin 因此可单遍直通(零缓冲),文件则按需定位读;洞永远来自信封元数据,
不做文件系统探测、更不做内容扫描。

stdout 输出一行 64 字符 hex —— 这是上传后的 manifest content key。stderr
是人类可读的摘要(默认开,`--no-progress` 关):

```
image size:   10.0 GiB
stored bytes: 1.0 GiB
chunks:       stored=2048 dedup=18432 zero=0
manifest key: a1b2c3d4...
```

示例:

```bash
# 文件 → manifest key
MKEY=$(manifest-ctl store disk.img)

# stdin → manifest key
cat disk.img | manifest-ctl store > disk.key

# docker save → 展平 → 入库(典型管道)
docker save myapp:v1 | flatten-ctl export --upload > app.key

# 额外 salt(隔离 dedup 域;flags 在位置参数前)
manifest-ctl store --extra-salt "tenant-xyz" snap.bin

# 切换分块 / 加密模式 → 改 YAML
```

### 2.4 `manifest-ctl load` — 数据读取

```
manifest-ctl load [flags] <hex|manifest://hex>

Args:
  <hex|manifest://hex>        要加载的 manifest content key(必填;可带可选
                              manifest:// 前缀)
Flags:
  --output string             输出路径 (default "-", stdout;终端拒写)
  --name string               产物 tar 条目名 (default "image")
  --offset uint               窗口起始偏移
  --length uint               窗口长度 (0 = 余下全部)
  --no-progress               禁用进度输出
```

输出是 **tarstream 工件**:manifest 空洞无损进信封洞图(没有"落洞还是填零"
的策略问题,原 `--hole` 旗标随之取消),IsZero chunk 由写出端本地合成零字节、
不取数。要 raw 字节用 `flatten-ctl tar extract` 解包。

```bash
# 全量还原为工件(flags 在位置参数前)
manifest-ctl load --output disk.img a1b2c3d4...

# 窗口切片(切片本身也是合法工件)
manifest-ctl load --offset 4096 --length 65536 --output slice.img a1b2c3d4...

# 也可带 manifest:// 前缀
manifest-ctl load --output disk.img manifest://a1b2c3d4...
```

### 2.5 `manifest-ctl get-manifest` — 取回 manifest 字节

把 manifest 原始字节从 store 拿出来(主要用于 debug 或 `info`/`diff` 离
线场景)。

```
manifest-ctl get-manifest [--output -|FILE] <hex|manifest://hex>
```

```bash
# 拿出来直接看(也可直接 `manifest-ctl info manifest://<hex>`)
manifest-ctl get-manifest a1b2c3d4... | manifest-ctl info -

# 存档
manifest-ctl get-manifest --output disk.manifest a1b2c3d4...
```

### 2.6 `manifest-ctl info`

读取 manifest 并打印摘要。位置参数三选一:本地 manifest 文件路径、`-`
(或省略)= stdin、或 `manifest://<hex>`(从 store 取回 manifest 字节,
此时需 `--manifest-config`)。前两者无需 customer key、无需 store 连接。

```
manifest-ctl info [--manifest-config <path>] <file|-|manifest://hex>
```

```
version:       1
image size:    10.0 GiB (10737418240 bytes)
chunk mode:    cdc
chunk count:   20480
zero chunks:   0 (0 B)
holes:         0 (0 B)
min chunk:     128.0 KiB    (configured)
max chunk:     1.0 MiB    (configured)
avg chunk:     524.0 KiB

size distribution (data chunks):
       min(N)          P1          P5         P25         P50         P75         P95         P99      max(N)
  63.5K(1)       128.0K      256.0K      384.0K      512.0K      640.0K      896.0K        1.0M     1.0M(412)
  (max == configured max — 412/20480 (2.0%) chunks are forced CDC cuts)
key table:     655380 bytes (sealed)
manifest size: 1887436 bytes
```

`min chunk` / `max chunk` 标 `(configured)` 是因为它们来自 Manifest 头里记录的
**配置值**,不是实测;实测分布在下面的百分位表里(只统计非 zero 的数据 chunk,
`min(N)` / `max(N)` 括号内是取到该极值的 chunk 数;最后一行仅当实测最大值正好
等于配置上限时出现,提示有多少 chunk 是被 CDC 强制切的)。

查看 store 中的 manifest 直接 `manifest-ctl info manifest://<hex>`,等价于
`get-manifest … | info -` 管道。

### 2.7 `manifest-ctl verify`

通过 fetch 路径逐 chunk 端到端解密,验证 store 中的 manifest 全可用。

```
manifest-ctl verify [--no-progress] a1b2c3d4...        # 或 manifest://a1b2c3d4...
```

```
verified: 20480  skipped(zero): 0  holes: 0  failed: 0  total chunks: 20480
```

### 2.8 `manifest-ctl diff` — 去重率分析

```
manifest-ctl diff <manifest-a> <manifest-b>
```

两个参数都是本地 manifest 文件(用 `get-manifest` 取回);diff 不需要打开
store。

```
manifest A:  10.0 GiB, 20480 chunks (20480 unique)
manifest B:  10.1 GiB, 20512 chunks (20512 unique)
shared:      18432 chunks (9.0 GiB)
only in A:   2048 chunks (1.0 GiB)
only in B:   2080 chunks (1.1 GiB)
dedup ratio: 45.0%
```

### 2.9 `manifest-ctl config`

```
manifest-ctl config show     [--manifest-config <path>]
manifest-ctl config generate
```

`generate` 输出带注释的清单配置模板;`show` 打印加载后的有效配置(YAML)。

## 3. 配置

### 3.1 manifest-config.yaml

```yaml
manifest:
  key: "0a1b2c3d..."              # 32 字节 hex 客户密钥;Manifest 内嵌的密钥表用它密封
store:
  endpoint: 127.0.0.1:7100        # store-ctl 端点(必填):host:port 或 Unix socket
                                  # (/run/sandbox/store.sock 或 unix:///...);须与 store-ctl listen 一致
  pool: 4                         # 客户端并行 grpc.ClientConn 数 (round-robin)
  timeout: 5s                     # 单次 store RPC 超时
cache:
  endpoint: 127.0.0.1:7070        # 空 = 跳过 cache 层、直接走 store;同支持 Unix socket 路径
  pool: 4                         # demand Get 独立连接池
  warming_workers: 4              # S2 异步 warm worker/独立连接池, 范围 1..32
  timeout: 2s
chunker:
  mode: cdc                       # cdc | fixed
  cdc:
    min: 128KiB
    avg: 512KiB
    max: 1MiB
  fixed:
    size: 512KiB
crypto:
  chunk: aes                      # aes(唯一支持)
  manifest: aes                   # aes(唯一支持)
```

字段说明:

- `manifest.key` — 客户密钥,**Manifest 中密钥表的密封密钥**。**不参与
  chunk 加密或寻址**。loss → 整个 Manifest 不可读。可留空,改由
  `$MANIFEST_KEY` 环境变量提供——这是密钥的**主要交付方式**,且 `$MANIFEST_KEY`
  存在时**覆盖**此处 YAML 的值(密钥懒解析、不写回 Config,故不会被
  `config show` 回显)。两者皆空时,真正用到密封/解封的命令才报错。
- `store.endpoint` — manifest-ctl 不直接读写持久层;所有 chunk / Manifest
  I/O 通过这个 gRPC 客户端打到 store-ctl 守护进程。取 `host:port`(TCP)或一个
  Unix socket(裸路径 `/run/sandbox/store.sock` 会被规范化为 gRPC 的
  `unix:///run/sandbox/store.sock`,显式 `unix:` 形式原样透传)——与 store-ctl
  的 `listen` 同址(同机经 socket 免 TCP 栈)。
- `cache.endpoint` — 空则 manifest-ctl `load` 路径直走 store gRPC;非空则
  通过 wire 协议穿 cache-ctl。同样接受 `host:port` 或 Unix socket 路径
  (`unix://path` 或裸 `/path`),与 cache-ctl 的 `listen` 同址。
- `cache.warming_workers` — S2 self-layer 后台 warming 的 worker 数，同时作为
  warm-only 独立连接池大小。默认 4，可配置范围 `1..32`；不改变
  `cache.pool` 所配置的 demand Get 连接池。
- `chunker.mode` — `cdc`(FastCDC,变长)或 `fixed`(固定大小)。详见 §4.1。
- `crypto.chunk` / `crypto.manifest` — chunk 与 Manifest 的加密算法,均仅支持
  `aes`(§4.3 / §4.4);其它值在构造时即被拒绝。

### 3.2 加载顺序

**配置文件**的来源只有 CLI flag 与对应环境变量两种,**没有自动查找**:

```
--manifest-config FILE     ┐
                           ├─ precedence: flag > env; error when both absent
MANIFEST_CONFIG            ┘   (except `config generate`)
```

**customer key 是例外**:它可以来自配置文件的 `manifest.key`,**也可以**来自
`$MANIFEST_KEY` 环境变量,且后者存在时覆盖前者(§3.1)——所以即便共享的
MANIFEST_CONFIG 不含 key,命令仍能拿到密钥。

除文件外,`pkg/manifest.ParseConfig` 还支持从**内存 YAML 字节**装配 Config
(例如经 config-socket 投递给 sandbox-ctl),endpoint / crypto 等参数无需落盘;
customer key 通常由调用方单独设到 `Config.Manifest.Key`。

无单字段 CLI override —— 切换分块或加密模式直接改 YAML。

## 4. 设计

### 4.1 chunking

把字节流切成可去重单位。两种模式:

#### FastCDC (cdc)

变长内容定义分块,基于 rolling hash + Gear 表选择切点。

| 参数 | 默认 | 说明 |
|---|---|---|
| `min` | 128 KiB | 切点的最小长度;小于此值不切 |
| `avg` | 512 KiB | 期望平均长度;Gear mask 按此值设置 |
| `max` | 1 MiB   | 切点的最大长度;到此强制切 |

三个尺寸(以及 fixed 的 `size`)都必须是 4 KiB(page)的整数倍,否则配置
解析报错。

性质:对**插入/删除**有局部性 —— 在文件中段插入若干字节,只影响附近若干
个 chunk 的边界,其余 chunk 边界与之前一致 → dedup 命中率高。

#### Fixed-size (fixed)

固定大小切分(默认 512 KiB)。性质:实现简单,但对插入/删除**无**局部
性 —— 在中段插入几字节后,后续所有 chunk 都偏移,几乎全 miss。仅作 perf
基线对比用,生产推荐 cdc。

切换模式只需改 YAML `chunk.mode`,不影响 manifest-ctl 命令。

### 4.2 收敛加密(convergent encryption)

目标:**相同明文 + 相同 salt 生成相同密文**,允许跨用户在 store 上做去
重;同时 store 永远只见密文。

#### Key 派生

```
salt = server_salt (+ extra_salt mixing)   # 见 §4.5
key  = SHA256(salt || plaintext)           # convergent key
```

AES-CTR 的 IV 取全零——key 本身已由 `(salt, plaintext)` 唯一决定,同一
key 永远只加密同一明文,(key, IV) 对不会复用。由 `salt + plaintext` 决定
chunk 的密钥,因此同 salt 域内同 plaintext 必然产出同 ciphertext;同
ciphertext → 同 ContentKey(= `SHA256(ciphertext)`)→ store 上同一份字节。

#### Content key

Chunk 上传时,`SHA256(ciphertext)` 是 store 的寻址键;Manifest 上传时同样
按 `SHA256(manifest_bytes)`。Salt **不参与寻址** —— 寻址完全由密文哈希决定,
保留 salt 的隔离性,但允许 store 端以单一 KV 视图存储(详见 [`store.md`](store.md))。

### 4.3 加密模式 — chunk

YAML `crypto.chunk` 仅支持 `aes`;任何其它值在构造时即被拒绝(无明文回退):

| 模式 | 行为 | 用途 |
|---|---|---|
| `aes` | `[flag=0x01] + AES-256-CTR(key, plaintext)`(IV 全零,§4.2) | 唯一模式 |

chunk key 是收敛密钥 `SHA256(salt‖plaintext)`,由加密层内部派生(调用方只
传 salt、不传 key),故 (key, IV=0) 对不同明文不复用、CTR keystream 不重用。
flag byte 在解密时做格式校验。

### 4.4 加密模式 — Manifest

Manifest 中**密钥表 (key table)** 是一段编码了所有 chunk 加密 key 的二进
制:

| 模式 | 行为 |
|---|---|
| `aes` | AES-GCM(`manifest.key`, key_table) — customer key 解密 |

只要 `manifest.key` 不泄露,**chunk 在 store 上永远不可读**(密文),即
使 store 后端被入侵,store 自己也无法解密。

### 4.5 Salt 与 dedup 域

Salt 隔离 dedup 域 —— 同样的明文用不同 salt 派生不同 key → 不同 ciphertext
→ 不同 ContentKey → store 上是两份。

实际 salt 由两部分组合:

- `server_salt` — manifest-ctl 启动时调一次 `store-ctl GetSalt()` 取得
  store 提供的 opaque salt。Store 内部切换写入域时
  salt 变,不同写入域天然隔离。manifest consumer 不感知 store 的内部代次。
- `extra_salt` — `--extra-salt <bytes>` flag(§2.3),叠加到上面。

最终:

```
final_salt = server_salt                                                       # extra_salt 为空
final_salt = SHA256("accelerator-extra-salt-v1" || server_salt || extra_salt)  # extra_salt 非空
```

`extra_salt` 用于在同一 store salt 域内做更细粒度隔离(例如多租户)。

### 4.6 Manifest 二进制格式

Manifest 是一段紧凑的小型二进制(整数 little-endian):

```
   ┌──────────────── header (64 B) ───────────────┐
   │  magic            "MANI"   4 B               │
   │  version          u8       1 B               │
   │  chunk_mode       u8       1 B               │   1 = cdc  /  2 = fixed
   │  image_size       u64      8 B               │
   │  chunk_count      u32      4 B               │
   │  chunk_min/max    u32 × 2  8 B               │   configured bounds
   │  key_table off/len, hole_count, holes off    │
   │  (reserved padding to 64 B)                  │
   ├──────── chunk index (56 B per entry) ────────┤   sorted by image_offset
   │  image_offset     u64                        │
   │  plain_len        u32                        │
   │  flags            u32                        │   bit0 = zero-chunk
   │  cipher_hash      32 B                       │   store addressing key
   ├──────── hole extents (16 B per hole) ────────┤   entries + holes tile
   │  offset u64 / size u64                       │   [0, image_size) exactly
   ├───────────── key table ──────────────────────┤   sealed with the customer key
   │  AES-GCM( manifest.key,                      │
   │           [ key_0[32], key_1[32], … ] )      │
   └──────────────────────────────────────────────┘
```

零 chunk(明文全零,flags bit0)不加密、不入库、不占密钥表条目,读取时本地
合成零字节;hole 是外部声明的"无数据"区间(文件系统空洞等),与数据 chunk
无重叠、无缝隙地铺满整个镜像。

读路径:解析 header → 二分查找 chunk index 定位 offset → 用 `manifest.key`
解密 key table 得每 chunk 的对称 key → store/cache.Get(cipher_hash) → 解
密 → 写入 io.Writer。

### 4.7 写路径(细节)

```
sparse.Source                ← holes recorded & skipped; zero runs synthesized
   │
   ▼
chunker (cdc | fixed)        ← split per configured mode
   │  → plaintext_chunk[i]
   ▼
crypto.derive(salt, plain)
   │  → key, ciphertext
   ▼
sha256(ciphertext)           ← ContentKey
   │
   ▼
store-ctl Put(partition=chunk, key=ContentKey, ciphertext)
   │  server checks Exists first → dedup hit skips upload (SendAndClose)
   │  else stream-write to tmp → atomic rename to chunk/{gen}/aa/bb/<hash>
   ▼
manifest.append(chunk_meta, key)   ← accumulate index + key table
   │
   ▼
seal(manifest.key, key_table)
   ↓
emit Manifest
```

上图按单个 chunk 画顺序流,但 derive/encrypt/`Put` 那一段是**并发**执行的:
ingest 用一个有界 worker pool,并发度取 store 客户端连接池大小(`store.pool`
——round-robin RPC 调度下真正能同时在途的 `Put` 数就是它)。chunk 切分仍按
文件顺序、manifest 索引按文件顺序组装、进度回调串行化,所以产物字节序不变;
后端不暴露连接池信息时回退为串行。这把入库吞吐从"串行单 `Put` 往返"提升到
"池并发往返",对多 GiB snapshot `--upload` 影响显著(实测见
`orchestrator/release-builder/docs/perf.md` §2.4)。

### 4.8 读路径(细节)

```
Manifest
   │
   ▼
parse(header, index, sealed_key_table)
   │
   ▼
unseal(manifest.key, sealed_key_table)
   │  → key[i]
   ▼
fetch.Range(offset, length) → ChunkIDs
   │
   ▼
for each ChunkID:
   ├─ cache.Get(ContentKey) ──→ store.Get fallback
   │  → ciphertext
   ▼
crypto.decrypt(key[i], nonce, ciphertext) → plaintext
   │
   ▼
io.Writer
```

`fetch.Range` 在 Manifest 的 chunk 索引上做二分查找,定位需要的 chunk 子
集 —— 部分读(`--offset` / `--length`)只触发涉及到的 chunk 拉取,稀疏访
问下不会读全镜像。

## 5. 性能特征

测量入口:`orchestrator/release-builder/docs/perf.md` §2.1–2.2(冷/热 L1 状态下
manifest:// 加载端到端时长)。

主要决定项:

- chunk 模式(cdc 命中率高于 fixed,但分块本身略慢);
- 加密模式(aes 是 CPU 大头,~50–100% CPU 在大 chunk 上);
- store 端点延迟(本地 fs vs 远端 OBS,详见 [`store.md`](store.md))。

## 6. See Also

- [`store.md`](store.md) — manifest-ctl 通过 gRPC 把字节落到 store-ctl
- [`cache.md`](cache.md) — `load` 路径可选穿 cache-ctl 加速;cache-ctl 自身
  以 manifest 同款客户端从 store 取 chunk
- `guest-runtime/docs/flatten.md` — 镜像展平后经 `flatten-ctl export --upload`
  入库
- `sandboxer/docs/sandbox.md` — 沙箱通过 `manifest://<key>` 引用磁盘
  base 与快照
- `orchestrator/release-builder/docs/kuasar-sandbox.md` §4.1–4.4 — Manifest 抽象在系统中的位置
