# deploy-ns.sh

## 部署前必须准备存储

执行 `up` 或 `replace` 之前，必须提前提供并确认以下外部存储可用：

- yyl manifest/store/cache 后端已经部署并运行；
- `MANIFEST_CONFIG` 指向该后端的访问配置；
- 配置中使用的 manifest key 已存在且可用；
- NFS 快照目录已经挂载并可写，默认是 `/mnt/sfs_turbo/<NS>-snaps`。

本脚本不会部署、启动、停止或初始化 manifest/store/cache 后端，也不会负责
挂载 NFS。对于 `file://` 快照路径，脚本只会在路径不存在时创建目录并检查
可写性；目录所在的 NFS 挂载需要由部署前的环境自行完成。

`deploy-ns.sh` 用于在同一台非隔离宿主机上部署和管理多套相互独立的
`node-ctl + node-proxy + connector/vswitch` 环境。每套环境由一个命名空间
前缀标识。

本脚本的部署范围不包含 manifest 存储后端：不会部署、启动或停止 yyl
manifest/store/cache 服务，只为 node-ctl 配置外部 manifest 存储的访问配置。

每套环境包含独立的控制面和数据面：

```text
node-ctl conductor   → API 控制面，例如 nova :443
node-proxy           → sandbox 数据面，例如 nova :8443
connector/vswitch    → 独立 Linux netns、管理网卡和 floating-IP /20
proxy worker         → 默认跟随 connector 的 mgmt-extract namespace
```

## 前置条件

- root 权限（`up`、`replace`、`down`）。
- systemd、`/dev/kvm`、`ip`、`iptables`、`curl`、`openssl` 等宿主机工具。
- 一个包含 `node-ctl`、`connector-ctl`、`e2b-key-ctl`、`cloud-hypervisor`、
  `vmlinux` 和 `sandbox-runtime.{erofs,bundle}` 的 bins 目录。也可以通过
  `SANDBOX_CH_PATH` 显式指定 Cloud Hypervisor 路径。
- 独立的 yyl manifest/store/cache 配置文件。默认是当前 NS 配置目录下的
  `manifest-obs.yaml`，例如 nova 的
  `/root/sandbox/nova-work/etc/node-ctl-nova/manifest-obs.yaml`。
- manifest/store/cache 后端必须已经由宿主机或其他部署提供并处于可用状态；
  本脚本只读取该配置文件，不负责后端生命周期。
- 如果要跑认证冒烟，需要该 manifest 配置能解析出 64 位 manifest key。

当前 `/root/sandbox/orchestrator/bin/x86_64/sandbox-ctl` 已软连接到
`/root/sandbox/sandboxer/bin/x86_64/sandbox-ctl`，因此使用 orchestrator 的
bins 目录即可使用后者。

## 基本用法

```bash
cd /root/sandbox/ws-bench/deploy

# 部署 nova；默认 workdir=/root/sandbox/nova-work
./deploy-ns.sh up nova /root/sandbox/orchestrator/bin/x86_64

# 查看关键状态
./deploy-ns.sh status nova

# 停止 nova（保留配置、DB、TLS、tenant 凭据和外部快照）
./deploy-ns.sh down nova

# 替换二进制并重启 nova；保留该 NS 的持久化状态
./deploy-ns.sh replace nova /root/sandbox/orchestrator/bin/x86_64
```

`up` 支持重入：发现同一 NS 已有 unit、进程或残留资源时，会先收敛清理再
重新拉起。`replace` 用于替换二进制并重启该 NS。需要只生成配置、systemd
unit 和冒烟脚本而不启动服务时：

```bash
./deploy-ns.sh prepare nova /root/sandbox/orchestrator/bin/x86_64
```

也可以在第三个参数指定 `/20` floating-IP 段基址。多个 NS 不允许使用重叠
网段：

```bash
./deploy-ns.sh up beta /root/sandbox/orchestrator/bin/x86_64 100.100.144.0
```

`FIP_BASE` 所在的 `/20` 会按网络边界规范化。例如 `100.100.129.0` 实际
对应 `100.100.128.0/20`，状态和 NAT 检查使用规范化后的网段。

多套环境必须使用不同的 NS。非 `nova` NS 的 API、proxy、MMDS 和 metrics
端口会按 NS 的 SHA-256 派生到互不相邻的端口组，并在启动前自动探测可用组；
也可以显式设置：

```bash
API_PORT=30443 \
PROXY_PORT=38443 \
MMDS_PORT=29254 \
METRICS_PORT=39095 \
./deploy-ns.sh up beta /root/sandbox/orchestrator/bin/x86_64 100.100.144.0
```

## 目录和存储

默认情况下，NS 为 `<prefix>` 时：

| 内容 | 默认路径 |
|---|---|
| 工作目录 | `/root/sandbox/<prefix>-work` |
| 运行日志 | `/root/sandbox/<prefix>-work/var/lib/logs/`（含 runner.log、builder.log、conductor.log、proxy.log、vswitch.log） |
| 配置、证书、tenant 凭据 | `/root/sandbox/<prefix>-work/etc/node-ctl-<prefix>/` |
| runner/socket/state | `/root/sandbox/<prefix>-work/run/` |
| 冒烟输出 | `/root/sandbox/<prefix>-work/smoke-tmp/` |
| NFS 快照目录 | `/mnt/sfs_turbo/<prefix>-snaps` |

当前 nova 默认使用 `100.100.160.0/20`。宿主机已有的 `/20` 路由或 NAT
规则会被视为冲突；例如 `e2bdemo` 使用 `100.100.128.0/20`，不能再给 nova
使用该段。

如果非 `nova` NS 没有显式指定 `FIP_BASE`，脚本会从候选 `/20` 中自动选择
与宿主机现有路由、NAT 不重叠的网段；`100.100.160.0/20` 始终保留给
`nova` 的默认环境。显式指定的网段如果冲突则直接失败，不会覆盖其他环境的
资源。

每个 NS 独占以下资源：

| 资源 | 派生方式 |
|---|---|
| vswitch | `e2b<NS>` |
| Linux netns | `e2b<NS>_ns` |
| 管理网卡 | `e2b<NS>m0` |
| floating-IP 网段 | 独立 `/20`，启动前检查重叠 |
| NAT/FORWARD 规则 | 按 FIP 和管理网卡隔离 |
| API/proxy/MMDS/metrics | 宿主机唯一端口 |
| switch/MGMT MAC | 按 NS 的 SHA-256 派生，并检查 switch MAC 和 connector 派生 MAC |
| Unix socket | 位于该 NS 的 work/run 目录 |
| systemd unit | `*-<NS>*` |
| resource cgroup 盘点 | 共享 runner slice 下按 `sandbox-runner-<NS>@` 前缀过滤，不计入其他 NS |
| Cloud Hypervisor CPU model | runner/builder 的 systemd drop-in，默认 `auto` |
| Cloud Hypervisor binary | runner/builder 的 `SANDBOX_CH_PATH` drop-in，默认 `<bins_dir>/cloud-hypervisor` |
| 配置、状态、日志、快照目录 | 位于 `<NS>-work` 或 `<NS>-snaps` |

`PROXY_NETNS` 默认是 `e2b<NS>_mgmt`。脚本先创建这个 connector 管理 namespace，
再以 `--mgmt-extract=$PROXY_NETNS:$MGMT:...` 启动 connector；`MGMT` 管理 veth
由 connector 自己创建到该 namespace。proxy 的 `proxy_netns` 同样指向这个
namespace，因此默认 proxy worker 不在宿主机 namespace 中。脚本不创建额外的
proxy namespace、proxy↔switch veth 或桥接。

如果 connector 已经由外部 unit 提供了独立管理 namespace，可以显式设置
`PROXY_NETNS=<existing-mgmt-ns>`。此时该 namespace 必须预先存在，脚本只把它
同时传给 `--mgmt-extract` 和 `proxy.yaml`，不会创建或删除该 namespace。vswitch unit 会在 connector 完成
管理面后设置实际 mgmt namespace 的 `route_localnet` 和 `ip_forward`，并设置
vswitch namespace 的 `ip_forward`。宿主机的 `data_listen`（例如 `:8443`）仍由
external proxy master 监听；proxy unit 显式 `Requires/After` vswitch unit，确保
先有 vswitch namespace 和 connector 管理面，proxy 才启动。

vswitch unit 与 netns unit 分离：单独执行
`systemctl restart kuasar-vswitch-<NS>.service` 时不会删除 vswitch netns 或 tap
设备。只有 `deploy-ns.sh down/replace` 才会调用 connector 的完整清理并删除
该 NS 的 switch 资源；外部提供的 `PROXY_NETNS` 不由脚本删除。

脚本使用宿主机级全局锁 `/run/kuasar/deploy-ns.lock` 串行化变更，并额外
使用每个 NS 的锁，避免并发操作导致端口、FIP、MAC 或 unit 状态竞争。

DNS `169.254.169.253:53` 的 DNAT 是宿主机级共享规则，不属于某个 NS 的
`NAT_STATE`。脚本会在 `/run/kuasar/deploy-ns-dns.state` 维护引用计数：多个
NS 共用时，关闭一个 NS 不会删除规则；如果规则原本由宿主机或其他系统创建，
脚本只解除自己的引用，绝不会删除外部规则。

manifest 配置和快照目录是两个独立配置：

```bash
MANIFEST_CONFIG=/root/sandbox/nova-work/etc/node-ctl-nova/manifest-obs.yaml \
NFS_DIR=/mnt/sfs_turbo/nova-snaps \
./deploy-ns.sh replace nova /root/sandbox/orchestrator/bin/x86_64
```

等价的远端快照 URI 可以通过 `REMOTE_SNAPSHOT_PARENT` 指定；如果使用
`file://` URI，脚本会创建并检查目标目录可写。脚本只配置并使用外部存储，
不会启动 yyl manifest/store/cache 或其他存储服务。`MANIFEST_CONFIG` 是
manifest 存储客户端配置，不代表脚本会创建对应的存储后端。

每个 NS 的 vswitch 默认创建 128 个端口，且该值会写入 NS 状态并用于生成
systemd unit。可通过 `VSWITCH_PORTS` 覆盖，取值范围为 1-4096：

```bash
VSWITCH_PORTS=128 ./deploy-ns.sh up nova /root/sandbox/orchestrator/bin/x86_64
```

proxy 路由表容量默认是 `2 × VSWITCH_PORTS`（128 个端口对应 256 个 slot），
避免 MMDS 按 floating IP 查找时扫描与实际并发规模无关的 65536 个大记录。
需要额外余量时可设置 `PROXY_ROUTE_CAPACITY`；该值必须不小于
`VSWITCH_PORTS` 且不大于 65536：

```bash
VSWITCH_PORTS=128 PROXY_ROUTE_CAPACITY=512 \
./deploy-ns.sh replace nova /root/sandbox/orchestrator/bin/x86_64
```

常用环境变量还包括 `WORKDIR`、`DOMAIN`、`MAC`、`FIP_BASE`、`UNIT_DIR`、
`RUNNER_POOL_SIZE`、`BUILDER_POOL_SIZE`、`AUTO_CREATE_TEMPLATES`、
`PROXY_ROUTE_CAPACITY`、`CLOUD_HYPERVISOR_CPUS_MODEL`、`SANDBOX_CH_PATH` 和
`DEPLOY_GLOBAL_LOCKFILE`。脚本默认通过以下 NS 独立的 systemd drop-in 把
Cloud Hypervisor CPU model 和二进制路径传给 runner/builder：

```text
/run/systemd/system/sandbox-runner-<NS>@.service.d/10-cloud-hypervisor.conf
/run/systemd/system/sandbox-builder-<NS>@.service.d/10-cloud-hypervisor.conf
```

默认内容为：

```ini
[Service]
Environment="CLOUD_HYPERVISOR_CPUS_MODEL=auto"
Environment="SANDBOX_CH_PATH=/root/sandbox/orchestrator/bin/x86_64/cloud-hypervisor"
```

如需覆盖，可在 `up` 或 `replace` 前设置
`CLOUD_HYPERVISOR_CPUS_MODEL` 或 `SANDBOX_CH_PATH`；脚本会校验值并重新生成
该 NS 的 runner/builder drop-in。`prepare` 时同样会把 drop-in 写入
`<NS>-work/systemd/`。

## 冒烟测试

部署完成后会生成：

```bash
/root/sandbox/nova-work/smoke.sh
```

脚本会检查 systemd unit、control/tapfd socket、API、proxy、MMDS 和 metrics
端口，然后调用 `/root/sandbox/ws-bench/bench.py` 做一次 `ws-bench` probe。
`ws-bench` 的沙箱 `/health` 和 `/tools/invoke` 请求走 node-proxy 数据面：

```text
ws-bench → node-proxy :<PROXY_PORT>
         → SID + E2b-Sandbox-Port:18789
         → sandbox guest 内 gateway :18789
```

冒烟脚本中的 API `/health` 只检查 node-ctl 控制面；后续 `ws-bench` probe
才会验证具体沙箱的数据面。
运行期间的输出写入：

```text
/root/sandbox/nova-work/smoke-tmp/ws-bench.log
```

当前 nova 重建后的 `health` 冒烟基线（`n=1`）如下，后续更换二进制或修改
部署网络后应使用相同条件对比：

```text
scenario         bare
template         snapshot template (ws-template-bare.id)
create          72ms
health1 (cold)  68ms
health2 (warm)   7ms
result          PASS
```

本次基线使用的快照模板 ID：

```text
bare-snp-ZmlsZTovL2ZmM2VjOTFiMmRiYTMzMDhkY2E0NmZmNWJlNjMyMjUxZTkxN2UyMmJkMmIwYTMyM2RkZWFlMTlhZjdmZTA1Zjcuc25hcHNob3RAc2hhMjU2OmZmM2VjOTFiMmRiYTMzMDhkY2E0NmZmNWJlNjMyMjUxZTkxN2UyMmJkMmIwYTMyM2RkZWFlMTlhZjdmZTA1ZjdAbG9jYXRpb246MDE5ZmY0ZjktY2U3ZC03YWNlLWFmYzMtYzQwNjRhNzU0ZmE3
```

对应文件：`/root/sandbox/nova-work/ws-bench/ws-template-bare.id`。
需要固定该模板复跑时：

```bash
SMOKE_TEMPLATE_ID="$(tr -d '[:space:]' < /root/sandbox/nova-work/ws-bench/ws-template-bare.id)" \
SMOKE_PROBE=health SMOKE_ITERATIONS=1 /root/sandbox/nova-work/smoke.sh
```

如果 `create` 或 `health1 (cold)` 后续出现明显波动，优先查看
`<NS>-work/smoke-tmp/ws-bench.log` 以及 `<NS>-work/var/lib/logs/` 下的
conductor、proxy、vswitch 原始日志。

如果自动发现不到模板，可以显式指定模板 ID：

```bash
SMOKE_TEMPLATE_ID="$(tr -d '[:space:]' < /root/sandbox/nova-work/ws-bench/working-current.id)" \
SMOKE_PROBE=health \
SMOKE_ITERATIONS=1 \
/root/sandbox/nova-work/smoke.sh
```

也可以调整 `SMOKE_PROBE=health|exec` 和 `SMOKE_ITERATIONS`。冒烟依赖部署生成
的 tenant 凭据：

```bash
source /root/sandbox/nova-work/etc/node-ctl-nova/tenant.env
```

## 关闭和替换的边界

- `down` 会停止 runner/builder、node-ctl、proxy、vswitch，删除 netns、该 NS
  的 NAT/FORWARD 规则和 systemd unit，并按共享引用计数处理 DNS DNAT。
- `down` 不删除 NFS 快照、manifest/store/cache 数据、配置、DB、TLS 或 tenant
  凭据，便于之后重入部署。
- `replace` 用新 bins 重启单个 NS，不影响其他 NS；它不会替换外部存储中的
  已有快照。
- manifest/store/cache 后端属于外部依赖，`up`、`replace` 和 `down` 都不会
  改变其进程或数据。
- 如果手工创建了不属于该 NS 的同网段路由或 NAT，脚本会拒绝启动；请使用
  独立的 `FIP_BASE`，不要删除其他 NS 的网络资源。
