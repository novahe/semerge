# Balloon / cold-zero PoC 使用方式

结果见 [../BALLOON_POC.md](../BALLOON_POC.md)，四组全部细分分布见 [DISTRIBUTION.md](DISTRIBUTION.md)。这是独立实验用补丁，不会由 bench 自动替换节点二进制。

| 文件 | 用途 |
|---|---|
| balloon-inflate-runs.patch | 应用到 v51.1 + sandboxer 现有 12 个 native patches 后的 CH 源码 |
| cold-zero-prefill-256k.patch | 应用到 sandboxer 185f50b 的 256KiB 对照 |
| cold-zero-prefill-1m.patch | 应用到 sandboxer 185f50b 的 1MiB 对照；与 256KiB 二选一 |
| Cargo.lock | 本次 CH 构建的依赖锁，SHA256 `da048c19408bebd62dbb76f5ef94fe8836bd54b5bfabfba0be0cb2e14e740b2d` |
| validate_runtime.py | 实际二进制身份、完整回收、pause/resume 活性与内容校验 |

## 本机已有构件

本次构件保留在 `/root/sandbox/balloon-poc-20261103/`：

- `bin/ch-baseline`、`bin/ch-candidate`。
- `bin/sandbox-ctl-base`、`bin/sandbox-ctl-zero256`、`bin/sandbox-ctl-zero1024`。
- `manage_env.py up|status|down` 管理本次 `balpoc` 隔离栈，外部 manifest/cache 不在其生命周期内。
- `run_group.py LABEL CH_VARIANT SB_VARIANT N` 切换私有软链接并跑一组，例如 `python3 run_group.py rerun-combined candidate zero1024 10`。要求没有仍存活的本 namespace 沙箱。
- `evidence/` 是开发时原件；规范归档已复制到 `cold_start/results/balloon-poc-20261103/`，包含 SHA256SUMS.json。

这些构件、凭据和原始日志被 Git 忽略或位于仓库外。其他机器按以下步骤重新构建和部署。

## 1. 准备相邻源码并构建 sandbox-ctl

使用新的实验目录。下面用 bash，所有路径均指向自己的实验目录：

```bash
set -euo pipefail
export SEMERGE=/root/sandbox/semerge
export POC=/root/sandbox/cold-start-new-poc
mkdir -p "$POC/bin"

git -C /root/sandbox/sandboxer worktree add --detach \
  "$POC/sandboxer" 185f50b641ea84864bc8e0f7bab5c469100d45e3
git -C /root/sandbox/accelerator worktree add --detach \
  "$POC/accelerator" 6ecec94495c0a255479f18e6219e52b4eb2408e7
git -C /root/sandbox/connector worktree add --detach \
  "$POC/connector" 4c4ef74e2ff74211132d42aee208f3d617d9b6d3
```

若要使用本机实验**完全相同的 Connector 编译输入**，在上述新 worktree 中恢复已归档的 pkg/tapfd 和 go.mod/go.sum：

```bash
tar -xzf "$SEMERGE/cold_start/results/balloon-poc-20261103/connector-tapfd-source.tar.gz" \
  -C "$POC/connector"
```

其他机器无该归档时，可使用该固定 commit 的干净 Connector，需把依赖差异记在自己的报告中。原始对照的四组 Connector 输入始终相同。不要用当前工作区其他未提交改动去覆盖已有生产构件。

```bash
cd "$POC/sandboxer"
GOWORK=off CGO_ENABLED=0 go build -o "$POC/bin/sandbox-ctl-base" ./cmd/sandbox-ctl

git apply --check "$SEMERGE/cold_start/poc/cold-zero-prefill-1m.patch"
git apply "$SEMERGE/cold_start/poc/cold-zero-prefill-1m.patch"
GOWORK=off go test ./pkg/uffd
GOWORK=off go test -race ./pkg/uffd
GOWORK=off CGO_ENABLED=0 go build -o "$POC/bin/sandbox-ctl-zero1024" ./cmd/sandbox-ctl
```

如需 256KiB 对照，在另一个干净 185f50b worktree 中应用 256k patch 并构建为 sandbox-ctl-zero256；不能在 1m patch 上再叠加。

## 2. 构建 CH before / after

使用 sandboxer 的 native-deps 准备本栈实际采用的 v51.1 + patches，再追加 PoC。不能直接将 PoC 应用到 CH 上游 main。

```bash
make -C "$POC/sandboxer/native-deps" ch-patches-apply \
  CLOUD_HYPERVISOR_SRC="$POC/ch-src"
cd "$POC/ch-src"
test "$(git hash-object virtio-devices/src/balloon.rs)" = \
  2cfed19d1db75304f121edde6717f956ee62736d
cp "$SEMERGE/cold_start/poc/Cargo.lock" Cargo.lock
export CARGO_TARGET_DIR="$POC/ch-target"
export CARGO_BUILD_JOBS=4
export RUSTFLAGS="--remap-path-prefix=$POC/ch-src=. --remap-path-prefix=${CARGO_HOME:-$HOME/.cargo}=/cargo"
cargo build --release --locked --bin cloud-hypervisor
cp "$CARGO_TARGET_DIR/release/cloud-hypervisor" "$POC/bin/ch-baseline"

git apply --check "$SEMERGE/cold_start/poc/balloon-inflate-runs.patch"
git apply "$SEMERGE/cold_start/poc/balloon-inflate-runs.patch"
# 有真实 UFFD/KVM 所需权限的 Linux 测试节点；不要省略 include-ignored。
cargo test --release --locked -p virtio-devices balloon::release_tests \
  -- --include-ignored --nocapture
cargo build --release --locked --bin cloud-hypervisor
cp "$CARGO_TARGET_DIR/release/cloud-hypervisor" "$POC/bin/ch-candidate"
sha256sum "$POC/bin/"ch-* "$POC/bin/"sandbox-ctl-*
```

依赖已经缓存时可为 cargo 增加 `--offline`。更换 Rust/Go 版本、源码目录或构建参数后，产物 SHA256 可以不同；每轮都记录实际文件和运行进程的哈希。

## 3. 拉起独立测试栈

节点需已有可用的 manifest/store/cache、img manifest 和 overlay 模板。沿用仓库 [deploy/deploy-ns.sh](../../deploy/deploy-ns.sh)，只管理自己的 namespace。以下路径对应本次宿主机；其他机器换成自己的资源。

```bash
export NS=balpoc
export WORKDIR="$POC/env"
export DOMAIN="sandboxes.$NS.local"
export MANIFEST_CONFIG=/root/sandbox/nova-work/etc/node-ctl-nova/manifest-obs.yaml
export OVERLAY_DIFF_TEMPLATE=/root/sandbox/nova-work/assets/overlay-32G.ext4
export BUILDER_DIFF_TEMPLATE=/root/sandbox/nova-work/assets/builder-250G.ext4
export NFS_DIR="$POC/snapshots"
export RUNNER_POOL_SIZE=4 BUILDER_POOL_SIZE=0 VSWITCH_PORTS=16 AUTO_CREATE_TEMPLATES=0
export SANDBOX_CH_PATH="$POC/bin/cloud-hypervisor"
export RELEASE=/root/sandbox/nova-work/release-v0.1.10/bin

# node-ctl 必须是真实文件：helper discovery 根据 os.Executable() 的目录查找。
cp -L "$RELEASE/node-ctl" "$POC/bin/node-ctl"
for name in connector-ctl e2b-key-ctl flatten-ctl manifest-ctl cache-ctl \
  store-ctl cluster-ctl node-stub-ctl sandbox-init vmlinux vmlinux.sha256 \
  sandbox-runtime.bundle; do
  ln -s "$RELEASE/$name" "$POC/bin/$name"
done
ln -s ch-baseline "$POC/bin/cloud-hypervisor"
ln -s sandbox-ctl-base "$POC/bin/sandbox-ctl"
```

本次 Rustls 客户端拒绝把 `CA:true` 自签证书直接作为服务端叶子（CaUsedAsEndEntity）。可为隔离域名生成 CA + `CA:false` 叶子，并由 tenant.env 的 SSL_CERT_FILE 信任完整链：

```bash
export CFG="$WORKDIR/etc/node-ctl-$NS"
mkdir -p "$CFG" "$POC/pki" "$WORKDIR/var/lib/logs"
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -subj /CN=cold-start-poc-ca \
  -addext basicConstraints=critical,CA:TRUE \
  -addext keyUsage=critical,keyCertSign,cRLSign \
  -keyout "$POC/pki/ca.key" -out "$POC/pki/ca.crt"
openssl req -new -newkey rsa:2048 -nodes -subj "/CN=$DOMAIN" \
  -keyout "$CFG/tls.key" -out "$POC/pki/leaf.csr"
cat > "$POC/pki/leaf.ext" <<EOF
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:$DOMAIN,DNS:*.$DOMAIN
EOF
openssl x509 -req -in "$POC/pki/leaf.csr" -CA "$POC/pki/ca.crt" \
  -CAkey "$POC/pki/ca.key" -CAcreateserial -days 365 \
  -extfile "$POC/pki/leaf.ext" -out "$POC/pki/leaf.crt"
cat "$POC/pki/leaf.crt" "$POC/pki/ca.crt" > "$CFG/tls.crt"
chmod 600 "$CFG/tls.key" "$POC/pki/ca.key"

for name in vswitch conductor proxy builder; do
  touch "$WORKDIR/var/lib/logs/$name.log"
done
if command -v selinuxenabled >/dev/null && selinuxenabled; then
  chcon -t var_log_t "$WORKDIR/var/lib/logs/"*.log
  chcon -t bin_t "$POC/bin/node-ctl" "$POC/bin/"sandbox-ctl-* "$POC/bin/"ch-*
fi
bash "$SEMERGE/deploy/deploy-ns.sh" up "$NS" "$POC/bin"

export E2B_ENV_FILE="$CFG/tenant.env"
export E2B_WORK_DIR="$WORKDIR" E2B_NAMESPACE="$NS"
export E2B_CONDUCTOR_FILE="$CFG/conductor.yaml"
export no_proxy='*' NO_PROXY='*'
export CONDUCTOR_LOG="$WORKDIR/var/lib/logs/conductor.log"
```

首次使用新的 NS，会自动避让端口和 FIP；本次是 API 39830、proxy 39831、MMDS 49830、FIP 100.100.128.0/20。不要在别人的同名 NS 正运行实验时重入 up。已有该 NS 时，保留已有 TLS/tenant，不要重新生成上述证书。

## 4. 先核对实际进程，再跑 bench

`validate_runtime.py` 创建一个独立诊断 VM，用 /proc/PID/exe 的文件内容计算 SHA256，再与指定构件对照。核对后会删除并确认 404；它不会提前运行 bench 的测试 VM。

```bash
cd "$SEMERGE"
python3 cold_start/poc/validate_runtime.py identity \
  --run-root "/run/sandbox-$NS" \
  --expect-sandbox-ctl "$POC/bin/sandbox-ctl-base" \
  --expect-ch "$POC/bin/ch-baseline" --out "$POC/identity-before.json"

python3 cold_start/bench.py -n 10 --label balloon-before \
  --capacity-memory 8GiB --allocatable-memory 256MiB --startup-memory 1GiB \
  --conductor-log "$CONDUCTOR_LOG" --out "$POC/results/before"

# 确认上一组全部沙箱清理完成，再切自己的 CH 链接。sandbox-ctl 保持 base。
ln -sfn ch-candidate "$POC/bin/cloud-hypervisor"
python3 cold_start/poc/validate_runtime.py identity \
  --run-root "/run/sandbox-$NS" \
  --expect-sandbox-ctl "$POC/bin/sandbox-ctl-base" \
  --expect-ch "$POC/bin/ch-candidate" --out "$POC/identity-ch.json"
python3 cold_start/bench.py -n 10 --label balloon-after \
  --capacity-memory 8GiB --allocatable-memory 256MiB --startup-memory 1GiB \
  --conductor-log "$CONDUCTOR_LOG" --compare "$POC/results/before/result.json" \
  --out "$POC/results/ch-after"

# 第二项单独比较：CH 保持 candidate，只换 sandbox-ctl。
ln -sfn sandbox-ctl-zero1024 "$POC/bin/sandbox-ctl"
python3 cold_start/poc/validate_runtime.py identity \
  --run-root "/run/sandbox-$NS" \
  --expect-sandbox-ctl "$POC/bin/sandbox-ctl-zero1024" \
  --expect-ch "$POC/bin/ch-candidate" --out "$POC/identity-combined.json"
python3 cold_start/bench.py -n 10 --label combined-after \
  --capacity-memory 8GiB --allocatable-memory 256MiB --startup-memory 1GiB \
  --conductor-log "$CONDUCTOR_LOG" --compare "$POC/results/ch-after/result.json" \
  --out "$POC/results/combined-after"
```

测量期间停止构建、tracing 和其他并发 bench；每个测试是新建 img，但不清空宿主机的镜像/文件缓存。

bench 输出 report.md、result.json、stages.csv、samples/attempts、逐 VM 原始 journal；退出码 0 才表示成功且七项齐全。记录每轮链接目标与二进制 SHA256，并核对 journal 的 `_EXE`。所有输出路径必须是新的，避免覆盖旧证据。

## 5. 完整回收、恢复校验和离线日志切分

convergence 在 8GiB/256MiB/1GiB 固定参数下每约 20ms 查询 CH vm.info，要求 actual<=1GiB 且 target=7GiB。每个构件组合单独跑，结果是诊断采样，不混入 bench 的分位数。

```bash
python3 cold_start/poc/validate_runtime.py convergence \
  --label combined --run-root "/run/sandbox-$NS" \
  --expect-sandbox-ctl "$POC/bin/sandbox-ctl-zero1024" \
  --expect-ch "$POC/bin/ch-candidate" --out "$POC/convergence-combined.json"

# 模板需要 node、sha256sum；3 轮同进程/32MiB 内存/8MiB fsync 文件校验。
python3 cold_start/poc/validate_runtime.py snapshot --cycles 3 \
  --label combined --run-root "/run/sandbox-$NS" \
  --expect-sandbox-ctl "$POC/bin/sandbox-ctl-zero1024" \
  --expect-ch "$POC/bin/ch-candidate" --out "$POC/snapshot-combined.json"

# 仅离线重算，不创建沙箱，不依赖 SDK/当前节点 journal。
python3 cold_start/split_logs.py --input "$POC/results/combined-after" \
  --compare "$POC/results/before/result.json" --out "$POC/results/offline-comparison"
```

验证器也支持 `--template` 指定已有 img 模板。退出码 0 表示该诊断与清理成功；失败返回 1，并尽量保存错误与 cleanup 状态。未收敛到 GET 404 时保留本地运行目录，继续排查，不要直接删仍活跃的 VM 状态。

## 6. 回滚和清理

先结束本轮创建的全部沙箱并确认 404。只回滚自己的链接，不修改 release 原文件：

```bash
ln -sfn ch-baseline "$POC/bin/cloud-hypervisor"
ln -sfn sandbox-ctl-base "$POC/bin/sandbox-ctl"
bash "$SEMERGE/deploy/deploy-ns.sh" down "$NS" "$POC/bin"
```

down 停止该 NS 的服务和网络，保留 DB/TLS/tenant、外部存储及证据。生产发布仍需将 CH diff 整理进 sandboxer/native-deps 的正式 patch 序列并重新打包 native binary；本目录不执行生产部署或上游推送。
