#!/usr/bin/env bash
# deploy-ns.sh — 在同一台非隔离宿主机上管理多套 NS 隔离的 node-ctl + connector 环境。
#
# 用法：
#   ./deploy-ns.sh up      <NS> <bins_dir> [FIP_BASE]   # 拉起全量环境（不启动存储）
#   ./deploy-ns.sh replace <NS> <bins_dir> [FIP_BASE]   # 换 bins 后重启该 NS
#   ./deploy-ns.sh prepare <NS> <bins_dir> [FIP_BASE]   # 只生成中间产物和冒烟用例
#   ./deploy-ns.sh down    <NS> [bins_dir]              # 停止该 NS 的 unit/进程/vswitch
#   ./deploy-ns.sh status  <NS> [bins_dir]              # 看 unit / socket / 端口状态
#
#   <bins_dir>  含 node-ctl 的目录（可为软连接；conductor 自动发现同目录 sandbox-ctl/connector-ctl/...）
#   <NS>        命名空间前缀，如 nova。所有资源按此派生，互不冲突。
#   [FIP_BASE]  floating-ip 段基址，默认 100.100.160.0（/20）。多环境必须各用一段。
#              NS=nova 保留示例默认值；其他 NS 未显式覆盖时自动探测端口并派生 FIP/MAC。
#
# 派生值（NS=nova 示例）：
#   vswitch=e2bnova  netns=e2bnova_ns  proxy_netns=e2bnova_mgmt  mgmt=e2bnovam0
#   tapfd=/run/kuasar/connector/e2bnova/
#   run=/run/sandbox-nova  base=/var/lib/sandbox-nova  saved=/var/lib/sandbox-saved-nova
#   cfg=/etc/node-ctl-nova  runner=sandbox-runner-nova@.service
#   vswitch ports=128（可 env VSWITCH_PORTS 覆盖）
#   api 端口=443（可 env API_PORT 覆盖）  mmds=19254（env MMDS_PORT） metrics=9095/9096
#
# 可用环境变量覆盖（换环境时设这些即可）：
#   WORKDIR API_PORT PROXY_PORT MMDS_PORT METRICS_PORT MAC DOMAIN MANIFEST_CONFIG MANIFEST_KEY FIP_BASE UNIT_DIR PROXY_NETNS
#   CLOUD_HYPERVISOR_CPUS_MODEL
#   NFS_ROOT NFS_DIR NFS_SNAPSHOT_DIR REMOTE_SNAPSHOT_PARENT AUTO_CREATE_TEMPLATES VSWITCH_PORTS
#
# 存储分工：NFS_DIR/REMOTE_SNAPSHOT_PARENT 只用于 snapshot；MANIFEST_CONFIG 独立指向
# yyl 上传的 manifest/store/cache 配置（例如 config/manifest-obs.yaml），本脚本不启动 yyl 存储。
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd -P)"

NS="${2:-nova}"
BINS_INPUT="${3:-${BINS_DIR:-$REPO_ROOT/orchestrator/bin/x86_64}}"
CMD="${1:-status}"

die() { echo "  ✗ $*" >&2; exit 1; }

case "$CMD" in
  up|replace|prepare) [ "$#" -ge 3 ] && [ "$#" -le 4 ] || die "用法: $0 $CMD <NS> <bins_dir> [FIP_BASE]"; FIP_ARG="${4:-}" ;;
  down|status) [ "$#" -le 3 ] || die "用法: $0 $CMD <NS> [bins_dir]"; FIP_ARG="" ;;
  help|-h|--help) sed -n '1,30p' "$0"; exit 0 ;;
  *) die "用法: $0 {up|replace|down|status} ..." ;;
esac

case "$NS" in ''|*[!A-Za-z0-9_-]*) die "NS 只能包含字母、数字、下划线和短横线：$NS" ;; esac
[ "${#NS}" -le 10 ] || die "NS 太长：e2b<NS>m0 不能超过 Linux 网卡名长度限制"
PROXY_NETNS_USER_SET=0; [ -n "${PROXY_NETNS:-}" ] && PROXY_NETNS_USER_SET=1
BINS="$(readlink -f -- "$BINS_INPUT" 2>/dev/null || true)"
[ -d "$BINS" ] || die "bins_dir 不存在或不是目录：$BINS_INPUT"

API_PORT_USER_SET=0; [ -n "${API_PORT+x}" ] && API_PORT_USER_SET=1
PROXY_PORT_USER_SET=0; [ -n "${PROXY_PORT+x}" ] && PROXY_PORT_USER_SET=1
MMDS_PORT_USER_SET=0; [ -n "${MMDS_PORT+x}" ] && MMDS_PORT_USER_SET=1
METRICS_PORT_USER_SET=0; [ -n "${METRICS_PORT+x}" ] && METRICS_PORT_USER_SET=1
CH_CPU_MODEL_USER_SET=0; [ -n "${CLOUD_HYPERVISOR_CPUS_MODEL+x}" ] && CH_CPU_MODEL_USER_SET=1
PROXY_LISTEN_USER_SET=0; [ -n "${PROXY_LISTEN+x}" ] && PROXY_LISTEN_USER_SET=1
VSWITCH_PORTS_USER_SET=0; [ -n "${VSWITCH_PORTS+x}" ] && VSWITCH_PORTS_USER_SET=1
FIP_USER_SET=0
[ -n "$FIP_ARG" ] && FIP_USER_SET=1
[ -n "${FIP_BASE+x}" ] && FIP_USER_SET=1
MAC_USER_SET=0
[ -n "${MAC+x}" ] && MAC_USER_SET=1
PORT_AUTO=0
NFS_DIR_USER_SET=0
if [ -n "${NFS_DIR+x}" ] || [ -n "${NFS_SNAPSHOT_DIR+x}" ]; then NFS_DIR_USER_SET=1; fi
REMOTE_SNAPSHOT_PARENT_USER_SET=0
[ -n "${REMOTE_SNAPSHOT_PARENT+x}" ] && REMOTE_SNAPSHOT_PARENT_USER_SET=1
MANIFEST_CONFIG_USER_SET=0
[ -n "${MANIFEST_CONFIG+x}" ] && MANIFEST_CONFIG_USER_SET=1

WORKDIR="${WORKDIR:-$REPO_ROOT/${NS}-work}"
if [ -n "$WORKDIR" ]; then
  WORKDIR="$(readlink -m -- "$WORKDIR")"
  case "$WORKDIR" in *[[:space:]]*|*\"*) die "WORKDIR 暂不支持空格或引号：$WORKDIR" ;; esac
fi

# ── 派生值 ──────────────────────────────────────────────────────────────────
SWITCH="e2b${NS}"
NETNS="e2b${NS}_ns"
PROXY_NETNS="${PROXY_NETNS:-e2b${NS}_mgmt}"
MGMT="e2b${NS}m0"
case "$PROXY_NETNS" in ''|*[!A-Za-z0-9_.-]*) [ -z "$PROXY_NETNS" ] || die "PROXY_NETNS 含有不安全字符：$PROXY_NETNS" ;; esac
if [ -n "$WORKDIR" ]; then
  RUNROOT="$WORKDIR/run"
  BASEROOT="$WORKDIR/var/lib"
  SAVED="$WORKDIR/var/lib-saved"
  CFGDIR="$WORKDIR/etc/node-ctl-${NS}"
  if [ -n "${HOSTS_FILE+x}" ]; then HOSTS_FILE="$HOSTS_FILE"; elif [ "$CMD" = prepare ]; then HOSTS_FILE="$WORKDIR/hosts"; else HOSTS_FILE=/etc/hosts; fi
else
  RUNROOT="/run/sandbox-${NS}"
  BASEROOT="/var/lib/sandbox-${NS}"
  SAVED="/var/lib/sandbox-saved-${NS}"
  CFGDIR="/etc/node-ctl-${NS}"
  HOSTS_FILE="${HOSTS_FILE:-/etc/hosts}"
fi
LOGDIR="${BASEROOT}/logs"
if [ -n "$WORKDIR" ]; then TAPFD_DIR="$WORKDIR/run/connector/${SWITCH}"; else TAPFD_DIR="/run/kuasar/connector/${SWITCH}"; fi
TAPFD_SOCK="${TAPFD_DIR}/tapfd.sock"
CFG_SOCKET="${RUNROOT}/node-ctl.socket"
RES_SOCKET="${RUNROOT}/resource.sock"
PROXY_SOCK="${RUNROOT}/proxy.sock"
PROXY_SHM="${RUNROOT}/proxy-routes.shm"
PIDFILE="${RUNROOT}/env.pids"            # 兼容旧版后台脚本
STATEFILE="${RUNROOT}/deploy.state"
NAT_STATE="${RUNROOT}/nat.rules"
HOSTS_STATE="${RUNROOT}/hosts.added"
LOCKFILE="${RUNROOT}/deploy.lock"
GLOBAL_LOCKFILE="${DEPLOY_GLOBAL_LOCKFILE:-/run/kuasar/deploy-ns.lock}"
DNS_STATEFILE="${DEPLOY_DNS_STATE_FILE:-/run/kuasar/deploy-ns-dns.state}"

# 同一宿主机上的变更全局串行化，避免两个 NS 同时通过端口/FIP 冲突检查；
# 再按 NS 串行化，避免同一 NS 的 up/down/replace 互相覆盖。
if [ "$CMD" != status ]; then
  mkdir -p "$(dirname -- "$GLOBAL_LOCKFILE")"
  command -v flock >/dev/null 2>&1 || die "缺少命令：flock"
  exec 8>"$GLOBAL_LOCKFILE"
  flock -n 8 || die "宿主机上已有另一个 deploy-ns.sh 操作，请稍后重试"
  mkdir -p "$RUNROOT"
  exec 9>"$LOCKFILE"
  flock -n 9 || die "NS=$NS 正在被另一个 deploy-ns.sh 操作，请稍后重试"
fi

NETNS_UNIT="kuasar-vswitch-${NS}-netns.service"
VS_UNIT="kuasar-vswitch-${NS}.service"
NODE_UNIT="node-ctl-${NS}.service"
PROXY_UNIT="node-proxy-${NS}.service"
RUNNER_TEMPLATE="sandbox-runner-${NS}@.service"
BUILDER_TEMPLATE="sandbox-builder-${NS}@.service"

DOMAIN="${DOMAIN:-sandboxes.${NS}.local}"
command -v sha256sum >/dev/null 2>&1 || die "缺少命令：sha256sum"
NS_HASH_HEX="$(printf '%s' "$NS" | sha256sum | awk '{print substr($1,1,8)}')"
NS_HASH=$((16#$NS_HASH_HEX))
if [ "$NS" != nova ] && [ "$API_PORT_USER_SET" -eq 0 ] && [ "$PROXY_PORT_USER_SET" -eq 0 ] && \
   [ "$MMDS_PORT_USER_SET" -eq 0 ] && [ "$METRICS_PORT_USER_SET" -eq 0 ]; then
  PORT_AUTO=1
fi
if [ -n "${API_PORT+x}" ]; then API_PORT="$API_PORT"; elif [ "$NS" = nova ]; then API_PORT=443; else API_PORT=$((30000 + (NS_HASH % 1000) * 10)); fi
if [ -n "${PROXY_PORT+x}" ]; then PROXY_PORT="$PROXY_PORT"; elif [ "$NS" = nova ]; then PROXY_PORT=8443; else PROXY_PORT=$((API_PORT + 1)); fi
if [ -n "${MMDS_PORT+x}" ]; then MMDS_PORT="$MMDS_PORT"; elif [ "$NS" = nova ]; then MMDS_PORT=19254; else MMDS_PORT=$((40000 + (NS_HASH % 1000) * 10)); fi
if [ -n "${METRICS_PORT+x}" ]; then METRICS="$METRICS_PORT"; elif [ "$NS" = nova ]; then METRICS=9095; else METRICS=$((50000 + (NS_HASH % 1000) * 10)); fi
PROXY_LISTEN="${PROXY_LISTEN:-:${PROXY_PORT}}"
VSWITCH_PORTS="${VSWITCH_PORTS:-128}"
FIP_AUTO=0
if [ -n "$FIP_ARG" ]; then
  FIP_BASE="$FIP_ARG"
elif [ -n "${FIP_BASE+x}" ]; then
  FIP_BASE="$FIP_BASE"
elif [ "$NS" = nova ]; then
  FIP_BASE=100.100.160.0
else
  # 自动候选会在 up 时跳过宿主机已有/重叠的路由和 NAT 网段。
  FIP_AUTO=1
  # 100.100.160.0/20 是 nova 的保留网段，自动分配永远跳过它。
  FIP_THIRDS=(0 16 32 48 64 80 96 112 128 144 176 192 208 224 240)
  FIP_BASE="100.100.${FIP_THIRDS[$((NS_HASH % ${#FIP_THIRDS[@]}))]}.0"
fi
# FIP_BASE 是 connector 使用的地址基址；iptables 的 /20 必须使用规范的
# 网络地址。例如 100.100.129.0/20 会被内核规范化为 100.100.128.0/20。
fip_cidr() {
  echo "$1" | awk -F. '{third=int($3/16)*16; printf "%s.%s.%s.0/20",$1,$2,third}'
}
FIP_CIDR="$(fip_cidr "$FIP_BASE")"
if [ -n "${MAC+x}" ]; then
  MAC="$MAC"
else
  MAC_HEX="$(printf '%s' "$NS" | sha256sum | awk '{print substr($1,1,10)}')"
  MAC="02:${MAC_HEX:0:2}:${MAC_HEX:2:2}:${MAC_HEX:4:2}:${MAC_HEX:6:2}:${MAC_HEX:8:2}"
fi
# 复用 yyl 存储后端（不含存储）
MANIFEST_CONFIG="${MANIFEST_CONFIG:-$REPO_ROOT/e2b-bench-file-bare/config/manifest-obs.yaml}"
if [ -n "${UNIT_DIR+x}" ]; then
  UNIT_DIR="$UNIT_DIR"
elif [ "$CMD" = prepare ] && [ -n "$WORKDIR" ]; then
  UNIT_DIR="$WORKDIR/systemd"
else
  UNIT_DIR=/run/systemd/system
fi
UNIT_MIRROR_DIR="${UNIT_MIRROR_DIR:-${WORKDIR:+$WORKDIR/systemd}}"
CLOUD_HYPERVISOR_CPUS_MODEL="${CLOUD_HYPERVISOR_CPUS_MODEL:-auto}"
case "$CLOUD_HYPERVISOR_CPUS_MODEL" in
  ''|*[!A-Za-z0-9_.:-]*) die "CLOUD_HYPERVISOR_CPUS_MODEL 含有不安全字符：$CLOUD_HYPERVISOR_CPUS_MODEL" ;;
esac
RUNNER_DROPIN_DIR="$UNIT_DIR/${RUNNER_TEMPLATE}.d"
BUILDER_DROPIN_DIR="$UNIT_DIR/${BUILDER_TEMPLATE}.d"
NFS_ROOT="${NFS_ROOT:-/mnt/sfs_turbo}"
NFS_DIR="${NFS_DIR:-${NFS_SNAPSHOT_DIR:-${NFS_ROOT}/${NS}-snaps}}"
NFS_DIR="$(readlink -m -- "$NFS_DIR")"
case "$NFS_DIR" in *[[:space:]]*|*\"*) die "NFS_DIR 暂不支持空格或引号：$NFS_DIR" ;; esac
if [ -n "${REMOTE_SNAPSHOT_PARENT+x}" ]; then
  REMOTE_SNAPSHOT_PARENT="$REMOTE_SNAPSHOT_PARENT"
else
  REMOTE_SNAPSHOT_PARENT="file://$NFS_DIR"
fi
if [ -n "${OVERLAY_DIFF_TEMPLATE+x}" ]; then
  OVERLAY_DIFF_TEMPLATE="$OVERLAY_DIFF_TEMPLATE"
elif [ -n "$WORKDIR" ]; then
  OVERLAY_DIFF_TEMPLATE="$WORKDIR/assets/overlay-1G.ext4"
else
  OVERLAY_DIFF_TEMPLATE=/opt/sandbox/overlay-templates/basic-1G.ext4
fi
if [ -n "${BUILDER_DIFF_TEMPLATE+x}" ]; then
  BUILDER_DIFF_TEMPLATE="$BUILDER_DIFF_TEMPLATE"
elif [ -n "$WORKDIR" ]; then
  BUILDER_DIFF_TEMPLATE="$WORKDIR/assets/builder-24G.ext4"
else
  BUILDER_DIFF_TEMPLATE=/opt/sandbox/overlay-templates/builder-24G.ext4
fi
RUNNER_POOL_SIZE="${RUNNER_POOL_SIZE:-100}"
BUILDER_POOL_SIZE="${BUILDER_POOL_SIZE:-0}"
AUTO_CREATE_TEMPLATES="${AUTO_CREATE_TEMPLATES:-1}"

NODE_CTL="${BINS}/node-ctl"
CONNECTOR="${BINS}/connector-ctl"
E2B_KEY_CTL="${BINS}/e2b-key-ctl"
TENANT_ENV="${CFGDIR}/tenant.env"

c_red=$'\e[1;31m'; c_grn=$'\e[1;32m'; c_ylw=$'\e[1;33m'; c_dim=$'\e[2m'; c_off=$'\e[0m'
[ -t 1 ] || { c_red=; c_grn=; c_ylw=; c_dim=; c_off=; }
say() { echo "${c_dim}  · $*${c_off}"; }
ok()  { echo "${c_grn}  ✓ $*${c_off}"; }
die() { echo "${c_red}  ✗ $*${c_off}" >&2; exit 1; }

# ── 前置条件、状态和 NAT ─────────────────────────────────────────────────────
valid_port() { [[ "$1" =~ ^[0-9]+$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]; }
validate_ports() {
  [[ "$VSWITCH_PORTS" =~ ^[0-9]+$ ]] && [ "$VSWITCH_PORTS" -ge 1 ] && [ "$VSWITCH_PORTS" -le 4096 ] \
    || die "VSWITCH_PORTS 非法（1-4096）：$VSWITCH_PORTS"
  PROXY_METRICS=$((METRICS + 1))
  for p in "$API_PORT" "$PROXY_PORT" "$MMDS_PORT" "$METRICS" "$PROXY_METRICS"; do
    valid_port "$p" || die "端口非法：$p"
  done
  PORTS=("$API_PORT" "$PROXY_PORT" "$MMDS_PORT" "$METRICS" "$PROXY_METRICS")
  for ((i=0; i<${#PORTS[@]}; i++)); do
    for ((j=i+1; j<${#PORTS[@]}; j++)); do
      [ "${PORTS[$i]}" != "${PORTS[$j]}" ] || die "端口冲突：${PORTS[$i]}（API/proxy/MMDS/metrics 必须全部独立）"
    done
  done
}
validate_ports
case "$DOMAIN" in ''|*[!A-Za-z0-9.-]*) die "DOMAIN 含有不安全字符：$DOMAIN" ;; esac

read_state() {
  local key="$1"
  [ -f "$STATEFILE" ] || return 0
  awk -F= -v k="$key" '$1 == k { sub(/^[^=]*=/, ""); v=$0 } END { if (v != "") print v }' "$STATEFILE"
}

if [ "$CMD" != prepare ] && [ -f "$STATEFILE" ]; then
  if [ "$CMD" = status ] || [ "$CMD" = down ] || [ "$API_PORT_USER_SET" -eq 0 ]; then
    [ -n "$(read_state api_port)" ] && API_PORT="$(read_state api_port)"
  fi
  if [ "$CMD" = status ] || [ "$CMD" = down ] || [ "$PROXY_PORT_USER_SET" -eq 0 ]; then
    [ -n "$(read_state proxy_port)" ] && PROXY_PORT="$(read_state proxy_port)"
  fi
  if [ "$CMD" = status ] || [ "$CMD" = down ] || [ "$MMDS_PORT_USER_SET" -eq 0 ]; then
    [ -n "$(read_state mmds_port)" ] && MMDS_PORT="$(read_state mmds_port)"
  fi
  if [ "$CMD" = status ] || [ "$CMD" = down ] || [ "$METRICS_PORT_USER_SET" -eq 0 ]; then
    [ -n "$(read_state metrics_port)" ] && METRICS="$(read_state metrics_port)"
  fi
  if [ "$CMD" = status ] || [ "$CMD" = down ] || [ "$PROXY_LISTEN_USER_SET" -eq 0 ]; then
    [ -n "$(read_state proxy_listen)" ] && PROXY_LISTEN="$(read_state proxy_listen)"
  fi
  if [ "$VSWITCH_PORTS_USER_SET" -eq 0 ]; then
    [ -n "$(read_state vswitch_ports)" ] && VSWITCH_PORTS="$(read_state vswitch_ports)"
  fi
  if [ "$PROXY_NETNS_USER_SET" -eq 0 ]; then
    [ -n "$(read_state proxy_netns)" ] && PROXY_NETNS="$(read_state proxy_netns)"
  fi
  if [ "$MAC_USER_SET" -eq 0 ]; then
    [ -n "$(read_state mac)" ] && MAC="$(read_state mac)"
  fi
  if [ "$CH_CPU_MODEL_USER_SET" -eq 0 ]; then
    [ -n "$(read_state cloud_hypervisor_cpus_model)" ] && \
      CLOUD_HYPERVISOR_CPUS_MODEL="$(read_state cloud_hypervisor_cpus_model)"
  fi
  if [ "$CMD" = status ] || [ "$CMD" = down ] || [ "$FIP_USER_SET" -eq 0 ]; then
    saved_fip="$(read_state fip_base)"
    if [ -n "$saved_fip" ]; then
      FIP_BASE="$saved_fip"
      FIP_AUTO=0
    fi
  fi
  if [ "$NFS_DIR_USER_SET" -eq 0 ]; then
    [ -n "$(read_state nfs_dir)" ] && NFS_DIR="$(read_state nfs_dir)"
  fi
  if [ "$REMOTE_SNAPSHOT_PARENT_USER_SET" -eq 0 ]; then
    [ -n "$(read_state remote_snapshot_parent)" ] && REMOTE_SNAPSHOT_PARENT="$(read_state remote_snapshot_parent)"
  fi
  if [ "$NFS_DIR_USER_SET" -eq 1 ] && [ "$REMOTE_SNAPSHOT_PARENT_USER_SET" -eq 0 ]; then
    REMOTE_SNAPSHOT_PARENT="file://$NFS_DIR"
  fi
  if [ "$MANIFEST_CONFIG_USER_SET" -eq 0 ]; then
    [ -n "$(read_state manifest_config)" ] && MANIFEST_CONFIG="$(read_state manifest_config)"
  fi
  if [ "$PORT_AUTO" -eq 1 ] && [ -n "$(read_state api_port)" ] && [ -n "$(read_state proxy_port)" ] && \
     [ -n "$(read_state mmds_port)" ] && [ -n "$(read_state metrics_port)" ]; then
    PORT_AUTO=0
  fi
  NFS_DIR="$(readlink -m -- "$NFS_DIR")"
  FIP_CIDR="$(fip_cidr "$FIP_BASE")"
fi

validate_ports

case "$CLOUD_HYPERVISOR_CPUS_MODEL" in
  ''|*[!A-Za-z0-9_.:-]*) die "CLOUD_HYPERVISOR_CPUS_MODEL 含有不安全字符：$CLOUD_HYPERVISOR_CPUS_MODEL" ;;
esac

NODE_CTL="$(readlink -f "$BINS/node-ctl" 2>/dev/null || true)"
CONNECTOR="$(readlink -f "$BINS/connector-ctl" 2>/dev/null || true)"
E2B_KEY_CTL="$(readlink -f "$BINS/e2b-key-ctl" 2>/dev/null || true)"
[ -x "$CONNECTOR" ] || CONNECTOR="$(read_state connector_path)"
[ -x "$NODE_CTL" ] || NODE_CTL="$(read_state node_path)"
if [ "$CMD" = down ] && [ "$#" -lt 3 ]; then
  [ -x "$(read_state connector_path)" ] && CONNECTOR="$(read_state connector_path)"
  [ -x "$(read_state node_path)" ] && NODE_CTL="$(read_state node_path)"
fi

require_root_and_tools() {
  [ "${EUID:-$(id -u)}" -eq 0 ] || die "$CMD 需要 root"
  local tool
  for tool in systemctl ip iptables ss openssl awk sed readlink curl; do
    command -v "$tool" >/dev/null 2>&1 || die "缺少命令：$tool"
  done
  [ -d /run/systemd/system ] || die "当前不是 systemd 宿主机，无法管理完整 runner/builder 环境"
  [ "$(ps -p 1 -o comm= 2>/dev/null | tr -d ' ')" = systemd ] || die "PID 1 不是 systemd"
  [ -w "$UNIT_DIR" ] || die "systemd unit 目录不可写：$UNIT_DIR"
  [ -r /dev/kvm ] && [ -w /dev/kvm ] || die "/dev/kvm 不可读写"
}

parse_ipv4() {
  local value="$1" octet
  local -a octets=()
  IFS=. read -r -a octets <<< "$value"
  [ "${#octets[@]}" -eq 4 ] || return 1
  for octet in "${octets[@]}"; do [[ "$octet" =~ ^[0-9]+$ ]] && [ "$octet" -le 255 ] || return 1; done
}
parse_ipv4 "$FIP_BASE" || die "FIP_BASE 不是合法 IPv4：$FIP_BASE"
[[ "$MAC" =~ ^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$ ]] || die "MAC 非法：$MAC"

port_in_use() { ss -H -ltn 2>/dev/null | awk -v p=":$1" '$4 ~ (p "$") { found=1 } END { exit !found }'; }
netns_exists() { ip netns list 2>/dev/null | awk '{print $1}' | grep -Fqx "$NETNS"; }
proxy_netns_exists() { [ -n "$PROXY_NETNS" ] && ip netns list 2>/dev/null | awk '{print $1}' | grep -Fqx "$PROXY_NETNS"; }
link_exists() { ip link show "$1" >/dev/null 2>&1; }
proxy_port_in_use() {
  if [ -n "$PROXY_NETNS" ]; then
    ip netns exec "$PROXY_NETNS" ss -H -ltn 2>/dev/null | awk -v p=":$1" '$4 ~ (p "$") { found=1 } END { exit !found }'
  else
    port_in_use "$1"
  fi
}
unit_active() { systemctl is-active --quiet "$1" 2>/dev/null; }

mac_in_use() {
  local want="${1,,}"
  ip -o link show 2>/dev/null | awk -v want="$want" '
    { for (i = 1; i < NF; i++) if ($i == "link/ether" && tolower($(i + 1)) == want) found = 1 }
    END { exit !found }'
}

mgmt_mac_for_switch() {
  local b1 b2 b3 b4 b5 b6 mgmt_b5
  IFS=: read -r b1 b2 b3 b4 b5 b6 <<< "$1"
  mgmt_b5=$(( ((16#$b5 ^ 128) & 128) | 127 ))
  printf '%02x:%02x:%02x:%02x:%02x:%02x' \
    "$((16#$b1))" "$((16#$b2))" "$((16#$b3))" "$((16#$b4))" "$mgmt_b5" 240
}

MGMT_MAC="$(mgmt_mac_for_switch "$MAC")"

select_auto_ports() {
  [ "$PORT_AUTO" -eq 1 ] || return 0
  local start=$((NS_HASH % 1000)) slot offset api proxy mmds metrics proxy_metrics
  for ((offset=0; offset<1000; offset++)); do
    slot=$(((start + offset) % 1000))
    api=$((30000 + slot * 10))
    proxy=$((api + 1))
    mmds=$((40000 + slot * 10))
    metrics=$((50000 + slot * 10))
    proxy_metrics=$((metrics + 1))
    if ! port_in_use "$api" && ! port_in_use "$proxy" && ! port_in_use "$mmds" && \
       ! port_in_use "$metrics" && ! port_in_use "$proxy_metrics"; then
      API_PORT="$api"
      PROXY_PORT="$proxy"
      MMDS_PORT="$mmds"
      METRICS="$metrics"
      PROXY_METRICS="$proxy_metrics"
      PROXY_LISTEN=":$PROXY_PORT"
      say "NS=$NS 自动选择独立端口 api=:$API_PORT proxy=:$PROXY_PORT mmds=:$MMDS_PORT metrics=:$METRICS/$PROXY_METRICS"
      return 0
    fi
  done
  die "没有可用的 NS 端口组（已探测 1000 组）；请显式指定 API_PORT/PROXY_PORT/MMDS_PORT/METRICS_PORT"
}

cidr_overlaps() {
  awk -v a="$1" -v b="$2" '
    function addr(s, p) {
      if (split(s, p, ".") != 4) return -1
      return p[1] * 16777216 + p[2] * 65536 + p[3] * 256 + p[4]
    }
    BEGIN {
      split(a, aa, "/"); split(b, bb, "/")
      ap = aa[2] + 0; bp = bb[2] + 0
      if (ap < 1 || ap > 32 || bp < 1 || bp > 32) exit 1
      av = addr(aa[1]); bv = addr(bb[1])
      if (av < 0 || bv < 0) exit 1
      as = int(av / (2 ^ (32 - ap))) * (2 ^ (32 - ap))
      bs = int(bv / (2 ^ (32 - bp))) * (2 ^ (32 - bp))
      ae = as + (2 ^ (32 - ap)) - 1
      be = bs + (2 ^ (32 - bp)) - 1
      exit !(as <= be && bs <= ae)
    }'
}

fip_route_conflict() {
  local line token
  while IFS= read -r line; do
    for token in $line; do
      if [[ "$token" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+$ ]] && cidr_overlaps "$FIP_CIDR" "$token"; then
        printf '%s' "$line"
        return 0
      fi
    done
  done < <(ip -4 route show table all 2>/dev/null)
  return 1
}

fip_nat_conflict() {
  local line token
  while IFS= read -r line; do
    for token in $line; do
      if [[ "$token" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+$ ]] && cidr_overlaps "$FIP_CIDR" "$token"; then
        printf '%s' "$line"
        return 0
      fi
    done
  done < <(iptables -t nat -S POSTROUTING 2>/dev/null)
  return 1
}

fip_conflict_reason() {
  local conflict
  if conflict="$(fip_route_conflict)"; then
    printf '宿主路由: %s' "$conflict"
    return 0
  fi
  if conflict="$(fip_nat_conflict)"; then
    printf 'NAT: %s' "$conflict"
    return 0
  fi
  return 1
}

rule_exists() {
  local table="$1" chain="$2"; shift 2
  if [ "$table" = nat ]; then iptables -t nat -C "$chain" "$@" >/dev/null 2>&1; else iptables -C "$chain" "$@" >/dev/null 2>&1; fi
}
rule_add() {
  local table="$1" chain="$2"; shift 2
  if [ "$table" = nat ]; then iptables -t nat -A "$chain" "$@"; else iptables -A "$chain" "$@"; fi
}
rule_delete() {
  local table="$1" chain="$2"; shift 2
  if [ "$table" = nat ]; then iptables -t nat -D "$chain" "$@"; else iptables -D "$chain" "$@"; fi
}

nat_add() {
  local kind="$1" table="$2" chain="$3"; shift 3
  if ! rule_exists "$table" "$chain" "$@"; then
    rule_add "$table" "$chain" "$@"
    printf '%s\n' "$kind" >> "$NAT_STATE"
  fi
}

nat_apply() { # 只删除 NAT_STATE 中由本 NS 脚本实际新增的规则
  local action="$1" item remove_fip="$FIP_CIDR" remove_mgmt="$MGMT"
  if [ "$action" = add ]; then
    : > "$NAT_STATE"
    printf 'meta_fip=%s\nmeta_mgmt=%s\nmeta_proxy_netns=%s\n' "$FIP_CIDR" "$MGMT" "$PROXY_NETNS" >> "$NAT_STATE"
    if [ -z "$PROXY_NETNS" ]; then
      # Connector's default mgmt-extract namespace is the caller/host netns.
      nat_add forward_out filter FORWARD -o "$MGMT" -m state --state RELATED,ESTABLISHED -j ACCEPT
      nat_add forward_in filter FORWARD -i "$MGMT" -j ACCEPT
    fi
    nat_add masquerade nat POSTROUTING -s "$FIP_CIDR" -j MASQUERADE
    sysctl -w net.ipv4.ip_forward=1 >/dev/null
    return 0
  fi
  [ -f "$NAT_STATE" ] || return 0
  while IFS= read -r item; do
    case "$item" in
      meta_fip=*) remove_fip="${item#meta_fip=}" ;;
      meta_mgmt=*) remove_mgmt="${item#meta_mgmt=}" ;;
      forward_out) rule_delete filter FORWARD -o "$remove_mgmt" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true ;;
      forward_in) rule_delete filter FORWARD -i "$remove_mgmt" -j ACCEPT 2>/dev/null || true ;;
      masquerade) rule_delete nat POSTROUTING -s "$remove_fip" -j MASQUERADE 2>/dev/null || true ;;
    esac
  done < "$NAT_STATE"
  rm -f "$NAT_STATE"
}

# DNS 169.254.169.253 是宿主机级共享入口，不能放进单个 NS 的 NAT_STATE。
# state 文件记录引用 NS；只有本脚本创建的规则在引用归零时才允许删除。
dns_rule_target() {
  local proto="$1"
  iptables -t nat -S PREROUTING 2>/dev/null | awk -v proto="$proto" '
    $1 == "-A" && $2 == "PREROUTING" {
      has_dest = has_proto = has_port = 0
      target = ""
      for (i = 3; i <= NF; i++) {
        if ($i == "-d" && $(i + 1) ~ /^169[.]254[.]169[.]253(\/32)?$/) has_dest = 1
        if ($i == "-p" && $(i + 1) == proto) has_proto = 1
        if (($i == "--dport" || $i == "--destination-port") && $(i + 1) == "53") has_port = 1
        if ($i == "--to-destination" && i < NF) {
          target = $(i + 1)
          sub(/:53$/, "", target)
        }
      }
      if (has_dest && has_proto && has_port && target != "") { print target; exit }
    }'
}

dns_state_value() {
  local key="$1"
  [ -f "$DNS_STATEFILE" ] || return 0
  awk -F= -v k="$key" '$1 == k { sub(/^[^=]*=/, ""); v=$0 } END { if (v != "") print v }' "$DNS_STATEFILE"
}

dns_shared_add() {
  local host_dns="${HOST_DNS:-}" udp tcp state_target state_managed target tmp
  parse_ipv4 "$host_dns" || return 0
  mkdir -p "$(dirname -- "$DNS_STATEFILE")"
  udp="$(dns_rule_target udp || true)"
  tcp="$(dns_rule_target tcp || true)"
  if [ -n "$udp" ] || [ -n "$tcp" ]; then
    [ -n "$udp" ] && [ -n "$tcp" ] && [ "$udp" = "$tcp" ] \
      || die "宿主机 DNS DNAT 规则不完整或目标不一致，请先人工核对 PREROUTING"
  fi
  state_target="$(dns_state_value target || true)"
  state_managed="$(dns_state_value managed || true)"
  target="${udp:-${tcp:-${state_target:-$host_dns}}}"
  parse_ipv4 "$target" || die "DNS DNAT 目标不是合法 IPv4：$target"
  if [ -n "$state_target" ] && [ -n "$udp" ] && [ "$state_target" != "$udp" ]; then
    say "检测到宿主机 DNS DNAT 已被外部改为 $udp；后续 down 不会删除该外部规则"
    state_managed=0
  fi
  if [ -z "$udp" ] && [ -z "$tcp" ]; then
    rule_add nat PREROUTING -d 169.254.169.253 -p udp --dport 53 -j DNAT --to-destination "${target}:53" \
      || die "添加 UDP DNS DNAT 规则失败"
    if ! rule_add nat PREROUTING -d 169.254.169.253 -p tcp --dport 53 -j DNAT --to-destination "${target}:53"; then
      rule_delete nat PREROUTING -d 169.254.169.253 -p udp --dport 53 -j DNAT --to-destination "${target}:53" 2>/dev/null || true
      die "添加 TCP DNS DNAT 规则失败"
    fi
    state_managed=1
  elif [ -z "$state_target" ]; then
    # 规则不是本脚本创建的，例如宿主机预置规则；down 只解除引用，不删除它。
    state_managed=0
  fi
  [ "$state_managed" = 1 ] || state_managed=0
  tmp="$(mktemp "${DNS_STATEFILE}.tmp.XXXXXX")"
  {
    printf 'target=%s\nmanaged=%s\n' "$target" "$state_managed"
    [ -f "$DNS_STATEFILE" ] && awk -F= -v ns="$NS" '$1 == "ref" && $2 != ns { print }' "$DNS_STATEFILE"
    printf 'ref=%s\n' "$NS"
  } > "$tmp"
  mv "$tmp" "$DNS_STATEFILE"
}

dns_shared_remove() {
  local state_target state_managed tmp udp tcp failed=0
  [ -f "$DNS_STATEFILE" ] || return 0
  state_target="$(dns_state_value target || true)"
  state_managed="$(dns_state_value managed || true)"
  tmp="$(mktemp "${DNS_STATEFILE}.tmp.XXXXXX")"
  {
    printf 'target=%s\nmanaged=%s\n' "$state_target" "${state_managed:-0}"
    awk -F= -v ns="$NS" '$1 == "ref" && $2 != ns { print }' "$DNS_STATEFILE"
  } > "$tmp"
  if awk -F= '$1 == "ref" { found = 1 } END { exit !found }' "$tmp"; then
    mv "$tmp" "$DNS_STATEFILE"
    return 0
  fi
  if [ "$state_managed" = 1 ] && parse_ipv4 "$state_target"; then
    udp="$(dns_rule_target udp || true)"
    tcp="$(dns_rule_target tcp || true)"
    if [ "$udp" = "$state_target" ]; then
      rule_delete nat PREROUTING -d 169.254.169.253 -p udp --dport 53 -j DNAT --to-destination "${state_target}:53" || failed=1
    fi
    if [ "$tcp" = "$state_target" ]; then
      rule_delete nat PREROUTING -d 169.254.169.253 -p tcp --dport 53 -j DNAT --to-destination "${state_target}:53" || failed=1
    fi
  fi
  if [ "$failed" -eq 0 ]; then
    rm -f "$tmp" "$DNS_STATEFILE"
  else
    mv "$tmp" "$DNS_STATEFILE"
  fi
}

# ── 冲突自检 ─────────────────────────────────────────────────────────────────
select_auto_fip() {
  [ "$FIP_AUTO" -eq 1 ] || return 0
  local count="${#FIP_THIRDS[@]}" start=$((NS_HASH % ${#FIP_THIRDS[@]})) index third reason
  for ((offset=0; offset<count; offset++)); do
    index=$(((start + offset) % count))
    third="${FIP_THIRDS[$index]}"
    FIP_BASE="100.100.${third}.0"
    FIP_CIDR="$(fip_cidr "$FIP_BASE")"
    if ! reason="$(fip_conflict_reason)"; then
      say "NS=$NS 自动选择独立 FIP 网段 $FIP_CIDR"
      return 0
    fi
  done
  die "没有可用的独立 FIP /20 网段；请显式指定 FIP_BASE"
}

check_conflicts() {
  local bad=0 p unit
  netns_exists && { echo "${c_red}  ✗ netns $NETNS 已存在（先 down 或换 NS）${c_off}"; bad=1; }
  if [ -n "$PROXY_NETNS" ]; then
    if [ "$PROXY_NETNS_USER_SET" -eq 1 ]; then
      proxy_netns_exists || { echo "${c_red}  ✗ connector 管理 netns $PROXY_NETNS 不存在${c_off}"; bad=1; }
    else
      proxy_netns_exists && { echo "${c_red}  ✗ connector 管理 netns $PROXY_NETNS 已存在（先 down 或换 NS）${c_off}"; bad=1; }
    fi
  fi
  link_exists "$MGMT" && { echo "${c_red}  ✗ 管理网卡 $MGMT 已存在${c_off}"; bad=1; }
  mac_in_use "$MAC" && { echo "${c_red}  ✗ switch MAC $MAC 已被宿主机使用${c_off}"; bad=1; }
  mac_in_use "$MGMT_MAC" && { echo "${c_red}  ✗ connector 派生的管理 MAC $MGMT_MAC 已被宿主机使用${c_off}"; bad=1; }
  for p in "$API_PORT" "$PROXY_PORT" "$MMDS_PORT" "$METRICS" "$PROXY_METRICS"; do
    port_in_use "$p" && { echo "${c_red}  ✗ 端口 :$p 已被占用${c_off}"; bad=1; }
  done
  [ -e "$CFG_SOCKET" ] && { echo "${c_red}  ✗ config_socket $CFG_SOCKET 已存在${c_off}"; bad=1; }
  [ -e "$TAPFD_SOCK" ] && { echo "${c_red}  ✗ tapfd socket $TAPFD_SOCK 已存在${c_off}"; bad=1; }
  if reason="$(fip_conflict_reason)"; then
    echo "${c_red}  ✗ FIP $FIP_CIDR 与现有网络资源重叠：$reason${c_off}"
    bad=1
  fi
  for unit in "$NETNS_UNIT" "$VS_UNIT" "$NODE_UNIT" "$PROXY_UNIT"; do
    unit_active "$unit" && { echo "${c_red}  ✗ unit $unit 已经 active（用 replace 或 down）${c_off}"; bad=1; }
  done
  return "$bad"
}

state_set() {
  mkdir -p "$RUNROOT"
  printf '%s=%s\n' "$1" "$2" >> "$STATEFILE"
}

ensure_template() {
  local path="$1" size="$2" tmp
  [ -f "$path" ] && return 0
  [ "$AUTO_CREATE_TEMPLATES" = 1 ] || die "缺少模板 $path；设置 AUTO_CREATE_TEMPLATES=1 自动创建"
  command -v truncate >/dev/null 2>&1 || die "缺少 truncate"
  command -v mkfs.ext4 >/dev/null 2>&1 || die "缺少 mkfs.ext4"
  say "创建本地 ext4 模板 $path（稀疏文件 $size）"
  mkdir -p "$(dirname "$path")"
  tmp="${path}.tmp.$$"
  rm -f "$tmp"
  truncate -s "$size" "$tmp"
  mkfs.ext4 -F -q -b 4096 "$tmp"
  if [ -e "$path" ]; then rm -f "$tmp"; else mv "$tmp" "$path"; fi
}

ensure_nfs_dir() {
  local path
  case "$REMOTE_SNAPSHOT_PARENT" in
    file://*) path="${REMOTE_SNAPSHOT_PARENT#file://}" ;;
    *) return 0 ;;
  esac
  [ -n "$path" ] || die "REMOTE_SNAPSHOT_PARENT 为空"
  if [ ! -d "$path" ]; then
    say "创建 NFS 快照目录 $path"
    mkdir -p "$path" || die "无法创建 NFS 快照目录：$path"
  fi
  [ -d "$path" ] || die "NFS 快照目录不是目录：$path"
  [ -w "$path" ] || die "NFS 快照目录不可写：$path"
}

prepare_assets() {
  [ -x "$NODE_CTL" ] || die "找不到可执行 node-ctl：$BINS/node-ctl"
  [ -x "$CONNECTOR" ] || die "找不到可执行 connector-ctl：$BINS/connector-ctl"
  [ -x "$E2B_KEY_CTL" ] || die "找不到可执行 e2b-key-ctl：$BINS/e2b-key-ctl"
  [ -f "$BINS/vmlinux" ] || die "找不到 $BINS/vmlinux"
  RUNTIME="$BINS/sandbox-runtime.erofs"
  [ -f "$RUNTIME" ] || RUNTIME="$BINS/sandbox-runtime.bundle"
  [ -f "$RUNTIME" ] || die "找不到 $BINS/sandbox-runtime.{erofs,bundle}"
  [ -f "$MANIFEST_CONFIG" ] || die "manifest_config 不存在：$MANIFEST_CONFIG（外部存储配置）"
  mkdir -p "$(dirname "$HOSTS_FILE")"
  [ -e "$HOSTS_FILE" ] || : > "$HOSTS_FILE"
  [ -w "$HOSTS_FILE" ] || die "hosts 文件不可写：$HOSTS_FILE"
  if [ -z "$WORKDIR" ] && [ ! -f "$OVERLAY_DIFF_TEMPLATE" ] && [ -f "$REPO_ROOT/local-bench/work/overlay-1G.ext4" ]; then
    OVERLAY_DIFF_TEMPLATE="$REPO_ROOT/local-bench/work/overlay-1G.ext4"
  fi
  ensure_template "$OVERLAY_DIFF_TEMPLATE" 1G
  ensure_template "$BUILDER_DIFF_TEMPLATE" 24G
}

write_hosts() {
  local line
  mkdir -p "$(dirname "$HOSTS_STATE")"
  : > "$HOSTS_STATE"
  for line in "127.0.0.1 api.${DOMAIN} #ns-${NS}" "127.0.0.1 ${DOMAIN} #ns-${NS}"; do
    if ! grep -Fqx "$line" "$HOSTS_FILE"; then
      printf '%s\n' "$line" >> "$HOSTS_FILE"
      printf '%s\n' "$line" >> "$HOSTS_STATE"
    fi
  done
}

remove_hosts() {
  local line tmp
  [ -f "$HOSTS_STATE" ] || return 0
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    tmp="$(mktemp "${HOSTS_FILE}.ns.XXXXXX")"
    awk -v remove="$line" '$0 != remove' "$HOSTS_FILE" > "$tmp"
    mv "$tmp" "$HOSTS_FILE"
  done < "$HOSTS_STATE"
  rm -f "$HOSTS_STATE"
}

write_configs() {
  local cert="$CFGDIR/tls.crt" key="$CFGDIR/tls.key"
  local enc_key
  mkdir -p "$RUNROOT" "$BASEROOT" "$SAVED" "$CFGDIR" "$LOGDIR" "$TAPFD_DIR"
  if [ -e "$cert" ] || [ -e "$key" ]; then
    [ -s "$cert" ] && [ -s "$key" ] || die "TLS 文件不完整：$CFGDIR/tls.{crt,key}"
  else
    say "生成 TLS 自签证书 *.$DOMAIN"
    openssl req -x509 -newkey rsa:2048 -nodes \
      -keyout "$CFGDIR/.tls.key.$$" -out "$CFGDIR/.tls.crt.$$" -days 365 \
      -subj "/CN=*.$DOMAIN" -addext "subjectAltName=DNS:*.$DOMAIN,DNS:$DOMAIN" >/dev/null 2>&1
    mv "$CFGDIR/.tls.key.$$" "$CFGDIR/tls.key"
    mv "$CFGDIR/.tls.crt.$$" "$CFGDIR/tls.crt"
    chmod 600 "$CFGDIR/tls.key"
  fi
  if [ -s "$CFGDIR/encryption.key" ]; then
    enc_key="$(tr -d '[:space:]' < "$CFGDIR/encryption.key")"
  elif [ -s "$CFGDIR/conductor.yaml" ]; then
    enc_key="$(awk -F'"' '/^[[:space:]]*encryption_key:/{print $2; exit}' "$CFGDIR/conductor.yaml")"
  else
    enc_key="$(openssl rand -hex 32)"
  fi
  [[ "$enc_key" =~ ^[0-9a-fA-F]{64}(:[0-9a-fA-F]{64})*$ ]] || die "conductor encryption_key 不是 64 hex"
  printf '%s\n' "$enc_key" > "$CFGDIR/encryption.key"
  chmod 600 "$CFGDIR/encryption.key"

  cat > "$CFGDIR/conductor.yaml" <<YAML
api: { domain: $DOMAIN, listen: ":$API_PORT", tls: { cert: $cert, key: $key } }
proxy: { mode: external, auth: enforce, park_timeout: 30s, metrics_listen: 127.0.0.1:$METRICS }
mmds:
  enabled: true
  listen: 127.0.0.1:$MMDS_PORT
  routes: { enabled: true, max_routes_per_sandbox: 8, reserved_path_prefixes: [/latest/api/, /internal/, /] }
encryption_key: "$enc_key"
manifest_config: $MANIFEST_CONFIG
paths: { run_root: $RUNROOT, base_root: $BASEROOT, config_socket: $CFG_SOCKET }
units: { dir: $UNIT_DIR, runner: $RUNNER_TEMPLATE, builder: $BUILDER_TEMPLATE, runner_pool_size: $RUNNER_POOL_SIZE, builder_pool_size: $BUILDER_POOL_SIZE, install: true }
sandbox:
  timeout_sec: 300
  resources: { vcpu: 2, memory: 8GiB, control_socket: $RES_SOCKET }
  network:
    switch: $SWITCH
    tapfd_socket: $TAPFD_SOCK
    hostname: sandbox
    dns: [169.254.169.253]
    e2b: { inner_ip: 169.254.0.21/30, nexthop: 169.254.0.22 }
    bare: { inner_ip: 169.254.1.1/31, nexthop: 169.254.1.0 }
  boot:
    kernel: $BINS/vmlinux
    runtime: $RUNTIME
    overlay_diff_template: $OVERLAY_DIFF_TEMPLATE
builder: { max_concurrent: 2, diff_template: $BUILDER_DIFF_TEMPLATE, insecure_registry: true, pull_timeout_sec: 600, total_timeout_sec: 1800 }
resource_listen:
  enabled: true
  socket: $RES_SOCKET
  state_path: $RUNROOT/state.json
  audit_path: $RUNROOT/audit.log
  cgroup_unit_prefix: sandbox-runner-${NS}@
  resources: { physical_memory: auto, physical_cpu: auto, host_reserved: { memory: 2GiB, cpu: 1.5 } }
  watermarks: { operational_margin_factor: 0.10, high_factor: 0.85, low_factor: 0.70, emergency_factor: 0.05, startup_factor: 0.50 }
  rate_limits: { memory_grant_per_sec_factor: 0.05 }
  admission: { rate: 128, burst: 128, startup_ttl: 30s, queue_ttl: 30s, queue_max_depth: 256 }
checkpoint: { mode: local, local_dir: $SAVED, remote: { ref_location_parent: $REMOTE_SNAPSHOT_PARENT } }
YAML

  cat > "$CFGDIR/proxy.yaml" <<YAML
config_socket: $CFG_SOCKET
paths: { run_root: $RUNROOT }
proxy_netns: $PROXY_NETNS
data_listen: "$PROXY_LISTEN"
proxy_socket: $PROXY_SOCK
shm_path: $PROXY_SHM
route_capacity: 65536
workers: 4
tls: { cert: $cert, key: $key }
auth: enforce
park_timeout: 30s
metrics_listen: 127.0.0.1:$PROXY_METRICS
YAML

  "$NODE_CTL" config conductor --config "$CFGDIR/conductor.yaml" --resolve >"$LOGDIR/config-conductor.log" 2>&1 || { tail -n 80 "$LOGDIR/config-conductor.log" >&2; die "conductor.yaml 校验失败"; }
  "$NODE_CTL" config proxy --config "$CFGDIR/proxy.yaml" >"$LOGDIR/config-proxy.log" 2>&1 || { tail -n 80 "$LOGDIR/config-proxy.log" >&2; die "proxy.yaml 校验失败"; }
}

write_units() {
  local ip_bin sysctl_bin sh_bin netns_start netns_stop
  ip_bin="$(command -v ip)"; sysctl_bin="$(command -v sysctl)"; sh_bin="$(command -v sh)"
  mkdir -p "$UNIT_DIR"
  if [ "$PROXY_NETNS_USER_SET" -eq 0 ]; then
    netns_start="ip netns list | grep -Eq \"^$NETNS( |$)\" || $ip_bin netns add $NETNS; ip netns list | grep -Eq \"^$PROXY_NETNS( |$)\" || $ip_bin netns add $PROXY_NETNS"
    netns_stop="$ip_bin netns del $PROXY_NETNS 2>/dev/null || true; $ip_bin netns del $NETNS 2>/dev/null || true"
  else
    netns_start="ip netns list | grep -Eq \"^$NETNS( |$)\" || $ip_bin netns add $NETNS"
    netns_stop="$ip_bin netns del $NETNS 2>/dev/null || true"
  fi
  cat > "$UNIT_DIR/$NETNS_UNIT" <<UNIT
[Unit]
Description=kuasar ${NS} vswitch network namespace
Before=$VS_UNIT

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$sh_bin -c '$netns_start'
ExecStop=$sh_bin -c '$netns_stop'
UNIT
  cat > "$UNIT_DIR/$VS_UNIT" <<UNIT
[Unit]
Description=kuasar connector vswitch ($NS)
After=network-online.target $NETNS_UNIT
Wants=network-online.target
Requires=$NETNS_UNIT

[Service]
Type=notify
ExecStart=$CONNECTOR vswitch serve $SWITCH --netns=$NETNS --ports=$VSWITCH_PORTS --mac-addr=$MAC --floating-ip-base=$FIP_BASE --mode=tap --tapfd-listen=$TAPFD_SOCK --mgmt-extract=$PROXY_NETNS:$MGMT:169.254.169.254,0.0.0.0/0 --mgmt-service=169.254.169.254:80:127.0.0.1:$MMDS_PORT
# Configure route_localnet on the connector mgmt device and enable forwarding
# in both connector-managed namespaces.
ExecStartPost=$sh_bin -c 'for i in 1 2 3 4 5 6 7 8 9 10; do if [ -n "$PROXY_NETNS" ]; then $ip_bin netns exec $PROXY_NETNS $ip_bin link show $MGMT >/dev/null 2>&1; else $ip_bin link show $MGMT >/dev/null 2>&1; fi && break; sleep 1; done; if [ -n "$PROXY_NETNS" ]; then $ip_bin netns exec $PROXY_NETNS $ip_bin link set lo up; $ip_bin netns exec $PROXY_NETNS $sysctl_bin -w net.ipv4.conf.all.route_localnet=1; $ip_bin netns exec $PROXY_NETNS $sysctl_bin -w net.ipv4.conf.$MGMT.route_localnet=1; $ip_bin netns exec $PROXY_NETNS $sysctl_bin -w net.ipv4.ip_forward=1; else $sysctl_bin -w net.ipv4.conf.$MGMT.route_localnet=1; $sysctl_bin -w net.ipv4.ip_forward=1; fi; $ip_bin netns exec $NETNS $ip_bin link set lo up; $ip_bin netns exec $NETNS $sysctl_bin -w net.ipv4.ip_forward=1'
# Deliberately no ExecStop connector cleanup: systemctl restart of this
# daemon must preserve tap devices and sandbox network namespaces. Full
# cleanup is performed only by deploy-ns.sh down/replace.
StandardOutput=append:$LOGDIR/vswitch.log
StandardError=append:$LOGDIR/vswitch.log
Restart=on-failure
RestartSec=2
KillMode=control-group
UNIT
  cat > "$UNIT_DIR/$NODE_UNIT" <<UNIT
[Unit]
Description=kuasar node-ctl conductor ($NS)
After=network-online.target $VS_UNIT
Wants=network-online.target
Requires=$VS_UNIT

[Service]
Type=simple
ExecStart=$NODE_CTL conductor serve --config $CFGDIR/conductor.yaml
StandardOutput=append:$LOGDIR/conductor.log
StandardError=append:$LOGDIR/conductor.log
Restart=on-failure
RestartSec=2
KillMode=control-group
UNIT
  cat > "$UNIT_DIR/$PROXY_UNIT" <<UNIT
[Unit]
Description=kuasar node-ctl external proxy ($NS)
After=$VS_UNIT $NODE_UNIT
Requires=$VS_UNIT $NODE_UNIT

[Service]
Type=simple
ExecStart=$NODE_CTL proxy serve --config $CFGDIR/proxy.yaml
StandardOutput=append:$LOGDIR/proxy.log
StandardError=append:$LOGDIR/proxy.log
Restart=always
RestartSec=1
KillMode=control-group
UNIT
  chmod 644 "$UNIT_DIR/$NETNS_UNIT" "$UNIT_DIR/$VS_UNIT" "$UNIT_DIR/$NODE_UNIT" "$UNIT_DIR/$PROXY_UNIT"
  mkdir -p "$RUNNER_DROPIN_DIR" "$BUILDER_DROPIN_DIR"
  cat > "$RUNNER_DROPIN_DIR/10-cloud-hypervisor.conf" <<UNIT
[Service]
# Passed through node-ctl run-sandbox to the Cloud Hypervisor process.
Environment="CLOUD_HYPERVISOR_CPUS_MODEL=$CLOUD_HYPERVISOR_CPUS_MODEL"
# Keep sandbox-ctl/Cloud Hypervisor runtime logs under this NS work directory.
StandardOutput=append:$LOGDIR/runner.log
StandardError=append:$LOGDIR/runner.log
UNIT
  cat > "$BUILDER_DROPIN_DIR/10-cloud-hypervisor.conf" <<UNIT
[Service]
# Keep builder-launched Cloud Hypervisor processes on the same default CPU model.
Environment="CLOUD_HYPERVISOR_CPUS_MODEL=$CLOUD_HYPERVISOR_CPUS_MODEL"
# Keep builder runtime logs under this NS work directory.
StandardOutput=append:$LOGDIR/builder.log
StandardError=append:$LOGDIR/builder.log
UNIT
  chmod 644 "$RUNNER_DROPIN_DIR/10-cloud-hypervisor.conf" "$BUILDER_DROPIN_DIR/10-cloud-hypervisor.conf"
  if [ -n "$UNIT_MIRROR_DIR" ] && [ "$UNIT_MIRROR_DIR" != "$UNIT_DIR" ]; then
    mkdir -p "$UNIT_MIRROR_DIR"
    cp "$UNIT_DIR/$NETNS_UNIT" "$UNIT_DIR/$VS_UNIT" "$UNIT_DIR/$NODE_UNIT" "$UNIT_DIR/$PROXY_UNIT" "$UNIT_MIRROR_DIR/"
    mkdir -p "$UNIT_MIRROR_DIR/${RUNNER_TEMPLATE}.d" "$UNIT_MIRROR_DIR/${BUILDER_TEMPLATE}.d"
    cp "$RUNNER_DROPIN_DIR/10-cloud-hypervisor.conf" "$UNIT_MIRROR_DIR/${RUNNER_TEMPLATE}.d/"
    cp "$BUILDER_DROPIN_DIR/10-cloud-hypervisor.conf" "$UNIT_MIRROR_DIR/${BUILDER_TEMPLATE}.d/"
  fi
  [ "$CMD" = prepare ] || systemctl daemon-reload
}

write_smoke_legacy() {
  local smoke_root="${WORKDIR:-$REPO_ROOT/nova-work}" smoke_file
  smoke_file="$smoke_root/smoke.sh"
  mkdir -p "$smoke_root/smoke-tmp"
  cat > "$smoke_file" <<SMOKE
#!/usr/bin/env bash
# Generated by deploy-ns.sh for NS=$NS. This test does not start storage.
set -Eeuo pipefail

NS='$NS'
DOMAIN='$DOMAIN'
API_PORT='$API_PORT'
PROXY_PORT='$PROXY_PORT'
MMDS_PORT='$MMDS_PORT'
METRICS_PORT='$METRICS'
PROXY_METRICS_PORT='$PROXY_METRICS'
CFG_SOCKET='$CFG_SOCKET'
TAPFD_SOCKET='$TAPFD_SOCK'
TENANT_ENV='$CFGDIR/tenant.env'
RUNROOT='$RUNROOT'
TMPDIR='$smoke_root/smoke-tmp'
mkdir -p "\$TMPDIR"

pass() { echo "  ✓ \$*"; }
skip() { echo "  - \$*"; }
fail() { echo "  ✗ \$*" >&2; exit 1; }
port_up() { ss -H -ltn 2>/dev/null | awk -v p=":\$1" '\$4 ~ (p "\$") { ok=1 } END { exit !ok }'; }
unit_up() { systemctl is-active --quiet "\$1" 2>/dev/null; }

echo "━━━ smoke NS=\$NS ━━━"
for unit in '$NETNS_UNIT' '$VS_UNIT' '$NODE_UNIT' '$PROXY_UNIT'; do
  unit_up "\$unit" || fail "unit inactive: \$unit"
  pass "unit \$unit"
done
[ -S "\$CFG_SOCKET" ] || fail "missing config socket: \$CFG_SOCKET"
[ -S "\$TAPFD_SOCKET" ] || fail "missing tapfd socket: \$TAPFD_SOCKET"
pass "control/tapfd sockets"

for pair in "api \$API_PORT" "proxy \$PROXY_PORT" "mmds \$MMDS_PORT" "metrics \$METRICS_PORT" "proxy-metrics \$PROXY_METRICS_PORT"; do
  name="\${pair%% *}"; port="\${pair##* }"
  port_up "\$port" || fail "\$name is not listening on :\$port"
  pass "\$name :\$port"
done

health_code="\$(curl -ksS --noproxy '*' -o "\$TMPDIR/health.body" -w '%{http_code}' \
  "https://127.0.0.1:\$API_PORT/health" -H "Host: api.\$DOMAIN" || true)"
[[ "\$health_code" =~ ^2 ]] || fail "API /health returned \\$health_code"
pass "API /health returned \\$health_code"

proxy_code="\$(curl -ksS --noproxy '*' -o "\$TMPDIR/proxy.body" -w '%{http_code}' \
  "https://127.0.0.1:\$PROXY_PORT/" -H "Host: smoke.\$DOMAIN" || true)"
[ "\$proxy_code" != 000 ] || fail "external proxy did not accept a connection"
pass "external proxy accepted a request (HTTP \\$proxy_code; no sandbox route is expected yet)"

if [ -f "\$TENANT_ENV" ]; then
  # shellcheck disable=SC1090
  source "\$TENANT_ENV"
  [ -n "\${E2B_API_KEY:-}" ] || fail "tenant.env has no E2B_API_KEY"
  pass "tenant credentials loaded"
  if [ -n "\${SMOKE_TEMPLATE_ID:-}" ]; then
    payload="\$TMPDIR/create.json"
    code="\$(curl -ksS --noproxy '*' -o "\$payload" -w '%{http_code}' -X POST \
      "https://127.0.0.1:\$API_PORT/sandboxes" \
      -H "Host: api.\$DOMAIN" -H "Authorization: Bearer \$E2B_API_KEY" \
      -H 'Content-Type: application/json' -d "{\\"templateID\\":\\"\$SMOKE_TEMPLATE_ID\\",\\"timeout\\":60}" || true)"
    [[ "\$code" =~ ^20[01] ]] || fail "sandbox create returned \\$code: \$(head -c 300 "\$payload")"
    sid="\$(python3 - "\$payload" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    data = json.load(f)
print(data.get("sandboxID") or data.get("sandboxId") or "")
PY
)"
    [ -n "\$sid" ] || fail "sandbox create response has no sandboxID"
    pass "sandbox create \$sid"
    get_code="\$(curl -ksS --noproxy '*' -o /dev/null -w '%{http_code}' \
      "https://127.0.0.1:\$API_PORT/sandboxes/\$sid" -H "Host: api.\$DOMAIN" \
      -H "Authorization: Bearer \$E2B_API_KEY" || true)"
    [[ "\$get_code" =~ ^2 ]] || fail "sandbox get returned \\$get_code"
    curl -ksS --noproxy '*' -o /dev/null -X DELETE \
      "https://127.0.0.1:\$API_PORT/sandboxes/\$sid" -H "Host: api.\$DOMAIN" \
      -H "Authorization: Bearer \$E2B_API_KEY" || true
    pass "sandbox get/delete lifecycle"
  else
    skip "SMOKE_TEMPLATE_ID is unset; lifecycle create/get/delete skipped"
  fi
else
  skip "tenant.env missing; authenticated lifecycle skipped"
fi
echo "━━━ smoke PASS NS=\$NS ━━━"
SMOKE
  chmod 700 "$smoke_file"
  SMOKE_FILE="$smoke_file"
}

write_smoke() {
  local smoke_root="$WORKDIR" smoke_file smoke_tmp
  smoke_file="$smoke_root/smoke.sh"
  mkdir -p "$smoke_root/smoke-tmp"
  cat > "$smoke_file" <<'SMOKE'
#!/usr/bin/env bash
# Generated by deploy-ns.sh. Storage services are intentionally not started.
set -Eeuo pipefail
NS='__NS__'
DOMAIN='__DOMAIN__'
API_PORT='__API_PORT__'
PROXY_PORT='__PROXY_PORT__'
MMDS_PORT='__MMDS_PORT__'
METRICS_PORT='__METRICS_PORT__'
PROXY_METRICS_PORT='__PROXY_METRICS_PORT__'
PROXY_NETNS='__PROXY_NETNS__'
CFG_SOCKET='__CFG_SOCKET__'
TAPFD_SOCKET='__TAPFD_SOCKET__'
TENANT_ENV='__TENANT_ENV__'
WS_BENCH_DIR='__WS_BENCH_DIR__'
WS_BENCH_WORK_DIR='__WS_BENCH_WORK_DIR__'
TMPDIR='__TMPDIR__'
mkdir -p "$TMPDIR"

pass() { echo "  ✓ $*"; }
skip() { echo "  - $*"; }
fail() { echo "  ✗ $*" >&2; exit 1; }
port_up() { ss -H -ltn 2>/dev/null | awk -v p=":$1" '$4 ~ (p "$") { ok=1 } END { exit !ok }'; }
proxy_port_up() {
  if [ -n "$PROXY_NETNS" ]; then
    ip netns exec "$PROXY_NETNS" ss -H -ltn 2>/dev/null | awk -v p=":$1" '$4 ~ (p "$") { ok=1 } END { exit !ok }'
  else
    port_up "$1"
  fi
}
unit_up() { systemctl is-active --quiet "$1" 2>/dev/null; }

echo "━━━ smoke NS=$NS ━━━"
for unit in __NETNS_UNIT__ __VS_UNIT__ __NODE_UNIT__ __PROXY_UNIT__; do
  unit_up "$unit" || fail "unit inactive: $unit"
  pass "unit $unit"
done
[ -S "$CFG_SOCKET" ] || fail "missing config socket: $CFG_SOCKET"
[ -S "$TAPFD_SOCKET" ] || fail "missing tapfd socket: $TAPFD_SOCKET"
pass "control/tapfd sockets"

for pair in "api $API_PORT" "proxy $PROXY_PORT" "metrics $METRICS_PORT" "proxy-metrics $PROXY_METRICS_PORT"; do
  name="${pair%% *}"; port="${pair##* }"
  port_up "$port" || fail "$name is not listening on :$port"
  pass "$name :$port"
done
proxy_port_up "$MMDS_PORT" || fail "mmds is not listening in ${PROXY_NETNS:-host} on :$MMDS_PORT"
pass "mmds ${PROXY_NETNS:-host} :$MMDS_PORT"

health_code="$(curl -ksS --noproxy '*' -o "$TMPDIR/health.body" -w '%{http_code}' \
  "https://127.0.0.1:$API_PORT/health" -H "Host: api.$DOMAIN" || true)"
[[ "$health_code" =~ ^2 ]] || fail "API /health returned $health_code"
pass "API /health returned $health_code"

proxy_code="$(curl -ksS --noproxy '*' -o "$TMPDIR/proxy.body" -w '%{http_code}' \
  "https://127.0.0.1:$PROXY_PORT/" -H "Host: smoke.$DOMAIN" || true)"
[ "$proxy_code" != 000 ] || fail "external proxy did not accept a connection"
pass "external proxy accepted a request (HTTP $proxy_code; no sandbox route is expected yet)"

if [ -f "$TENANT_ENV" ]; then
  # shellcheck disable=SC1090
  source "$TENANT_ENV"
  [ -n "${E2B_API_KEY:-}" ] || fail "tenant.env has no E2B_API_KEY"
  export E2B_API_URL="https://api.$DOMAIN:$API_PORT"
  export E2B_SANDBOX_URL="https://$DOMAIN:$PROXY_PORT"
  pass "tenant credentials loaded"
  smoke_template="${SMOKE_TEMPLATE_ID:-}"
  if [ -z "$smoke_template" ]; then
    for candidate in \
      "$WS_BENCH_WORK_DIR/ws-template-bare.id" \
      "$WS_BENCH_WORK_DIR/bare-snp-template.id" \
      "$WS_BENCH_DIR/ws-template-bare.id" \
      "$WS_BENCH_DIR/snapshots/unencrypted/bare-workingset.id" \
      "$WS_BENCH_DIR/snapshots/unencrypted/bare-baseline.id" \
      "$WS_BENCH_DIR/bare-snp-template.id"; do
      if [ -s "$candidate" ]; then
        smoke_template="$(tr -d '[:space:]' < "$candidate")"
        break
      fi
    done
  fi
  if [ -n "$smoke_template" ]; then
    [ -f "$WS_BENCH_DIR/bench.py" ] || fail "ws-bench/bench.py missing: $WS_BENCH_DIR"
    bench_env="$TMPDIR/ws-bench-env.sh"
    cp "$TENANT_ENV" "$bench_env"
    printf "export E2B_API_URL='%s'\nexport E2B_SANDBOX_URL='%s'\n" \
      "$E2B_API_URL" "$E2B_SANDBOX_URL" >> "$bench_env"
    chmod 600 "$bench_env"
    export E2B_ENV_FILE="$bench_env"
    probe="${SMOKE_PROBE:-health}"
    iterations="${SMOKE_ITERATIONS:-1}"
    if ! (cd "$WS_BENCH_DIR" && python3 bench.py --w "$smoke_template" --probe "$probe" -n "$iterations") \
        > "$TMPDIR/ws-bench.log" 2>&1; then
      tail -n 120 "$TMPDIR/ws-bench.log" >&2 || true
      fail "ws-bench 冒烟失败"
    fi
    grep -Eq '^  \[[BW] [0-9]+/[0-9]+\].*✓ ' "$TMPDIR/ws-bench.log" || {
      tail -n 120 "$TMPDIR/ws-bench.log" >&2 || true
      fail "ws-bench 未产生成功 probe 样本"
    }
    tail -n 40 "$TMPDIR/ws-bench.log"
    pass "ws-bench template=$smoke_template probe=$probe n=$iterations"
  else
    skip "未找到 ws-bench 模板；设置 SMOKE_TEMPLATE_ID，或放置 ws-template-bare.id"
  fi
else
  skip "tenant.env missing; ws-bench authenticated smoke skipped"
fi
echo "━━━ smoke PASS NS=$NS ━━━"
SMOKE
  smoke_tmp="${smoke_file}.tmp.$$"
  sed \
    -e "s|__NS__|$NS|g" \
    -e "s|__DOMAIN__|$DOMAIN|g" \
    -e "s|__API_PORT__|$API_PORT|g" \
    -e "s|__PROXY_PORT__|$PROXY_PORT|g" \
    -e "s|__MMDS_PORT__|$MMDS_PORT|g" \
    -e "s|__METRICS_PORT__|$METRICS|g" \
    -e "s|__PROXY_METRICS_PORT__|$PROXY_METRICS|g" \
    -e "s|__PROXY_NETNS__|$PROXY_NETNS|g" \
    -e "s|__CFG_SOCKET__|$CFG_SOCKET|g" \
    -e "s|__TAPFD_SOCKET__|$TAPFD_SOCK|g" \
    -e "s|__TENANT_ENV__|$CFGDIR/tenant.env|g" \
    -e "s|__WS_BENCH_DIR__|$REPO_ROOT/ws-bench|g" \
    -e "s|__WS_BENCH_WORK_DIR__|$WORKDIR/ws-bench|g" \
    -e "s|__TMPDIR__|$smoke_root/smoke-tmp|g" \
    -e "s|__NETNS_UNIT__|$NETNS_UNIT|g" \
    -e "s|__VS_UNIT__|$VS_UNIT|g" \
    -e "s|__NODE_UNIT__|$NODE_UNIT|g" \
    -e "s|__PROXY_UNIT__|$PROXY_UNIT|g" \
    "$smoke_file" > "$smoke_tmp"
  mv "$smoke_tmp" "$smoke_file"
  chmod 700 "$smoke_file"
  SMOKE_FILE="$smoke_file"
}

stop_instance_units() {
  local prefix="$1"
  local -a units=()
  mapfile -t units < <(systemctl list-units --all --no-legend --no-pager "${prefix}@*.service" 2>/dev/null | awk '{print $1}' || true)
  [ "${#units[@]}" -gt 0 ] && systemctl stop "${units[@]}" >/dev/null 2>&1 || true
}

stop_pid() {
  local pid="$1" label="${2:-process}" pgid i
  [[ "$pid" =~ ^[0-9]+$ ]] || return 0
  [ "$pid" -gt 1 ] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  say "停止 $label pid=$pid"
  pgid="$(ps -o pgid= -p "$pid" 2>/dev/null | tr -d ' ' || true)"
  [ "$pgid" = "$pid" ] && kill -TERM -- "-$pid" 2>/dev/null || true
  kill -TERM "$pid" 2>/dev/null || true
  for i in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || return 0; sleep 0.25; done
  [ "$pgid" = "$pid" ] && kill -KILL -- "-$pid" 2>/dev/null || true
  kill -KILL "$pid" 2>/dev/null || true
}

stop_legacy_processes() {
  local name pid cmdline
  if [ -f "$PIDFILE" ]; then
    while IFS='=' read -r name pid; do
      case "$name" in conductor|proxy|vswitch) stop_pid "$pid" "legacy-$name" ;; esac
    done < "$PIDFILE"
  fi
  # 兼容旧版后台脚本，匹配本 NS 的完整 config/switch，不用通用 pkill。
  for proc in /proc/[0-9]*; do
    pid="${proc##*/}"
    [ "$pid" -gt 1 ] 2>/dev/null || continue
    [ -r "/proc/$pid/cmdline" ] || continue
    cmdline="$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || true)"
    case "$cmdline" in
      *"conductor serve"*"$CFGDIR/conductor.yaml"*) stop_pid "$pid" legacy-conductor ;;
      *"proxy serve"*"$CFGDIR/proxy.yaml"*) stop_pid "$pid" legacy-proxy ;;
      *"vswitch serve"*"$SWITCH"*) stop_pid "$pid" legacy-vswitch ;;
    esac
  done
}

deployment_present() {
  local phase
  phase="$(read_state phase)"
  [ "$phase" = up ] && return 0
  if [ -e "$UNIT_DIR/$NETNS_UNIT" ] || [ -e "$UNIT_DIR/$VS_UNIT" ] || \
    [ -e "$UNIT_DIR/$NODE_UNIT" ] || [ -e "$UNIT_DIR/$PROXY_UNIT" ]; then
    return 0
  fi
  unit_active "$NETNS_UNIT" && return 0
  unit_active "$VS_UNIT" && return 0
  unit_active "$NODE_UNIT" && return 0
  unit_active "$PROXY_UNIT" && return 0
  netns_exists && return 0
  proxy_netns_exists && return 0
  link_exists "$MGMT" && return 0
  [ -e "$CFG_SOCKET" ] && return 0
  [ -e "$TAPFD_SOCK" ] && return 0
  [ -f "$NAT_STATE" ] && return 0
  return 1
}

stop_environment() {
  local remove_units="${1:-1}" connector_path="$(read_state connector_path)"
  [ -x "$connector_path" ] || connector_path="$CONNECTOR"
  command -v systemctl >/dev/null 2>&1 && {
    stop_instance_units "sandbox-runner-${NS}"
    stop_instance_units "sandbox-builder-${NS}"
    # Stop the daemon first. The vswitch unit intentionally has no ExecStop
    # cleanup: a daemon restart must leave persistent tap devices intact.
    systemctl stop "$PROXY_UNIT" "$NODE_UNIT" "$VS_UNIT" >/dev/null 2>&1 || true
  }
  stop_legacy_processes
  # Full down/replace is the explicit cleanup boundary. Connector removes its
  # mgmt/tap links before the switch namespace is removed.
  [ -x "$connector_path" ] && "$connector_path" vswitch stop "$SWITCH" --force >/dev/null 2>&1 || true
  command -v systemctl >/dev/null 2>&1 && systemctl stop "$NETNS_UNIT" >/dev/null 2>&1 || true
  for i in $(seq 1 20); do ! netns_exists && break; sleep 0.25; done
  netns_exists && ip netns del "$NETNS" >/dev/null 2>&1 || true
  if [ "$PROXY_NETNS_USER_SET" -eq 0 ]; then
    proxy_netns_exists && ip netns del "$PROXY_NETNS" >/dev/null 2>&1 || true
  fi
  link_exists "$MGMT" && ip link del "$MGMT" >/dev/null 2>&1 || true
  rm -f "$TAPFD_SOCK"
  rmdir "$TAPFD_DIR" 2>/dev/null || true
  nat_apply remove
  dns_shared_remove
  remove_hosts
  if [ "$remove_units" = 1 ]; then
    rm -f "$UNIT_DIR/$NETNS_UNIT" "$UNIT_DIR/$VS_UNIT" "$UNIT_DIR/$NODE_UNIT" "$UNIT_DIR/$PROXY_UNIT" \
      "$UNIT_DIR/$RUNNER_TEMPLATE" "$UNIT_DIR/$BUILDER_TEMPLATE"
    rm -f "$RUNNER_DROPIN_DIR/10-cloud-hypervisor.conf" "$BUILDER_DROPIN_DIR/10-cloud-hypervisor.conf"
    rmdir "$RUNNER_DROPIN_DIR" "$BUILDER_DROPIN_DIR" 2>/dev/null || true
    if [ -n "$UNIT_MIRROR_DIR" ] && [ "$UNIT_MIRROR_DIR" != "$UNIT_DIR" ]; then
      rm -f "$UNIT_MIRROR_DIR/${RUNNER_TEMPLATE}.d/10-cloud-hypervisor.conf" \
        "$UNIT_MIRROR_DIR/${BUILDER_TEMPLATE}.d/10-cloud-hypervisor.conf"
      rmdir "$UNIT_MIRROR_DIR/${RUNNER_TEMPLATE}.d" "$UNIT_MIRROR_DIR/${BUILDER_TEMPLATE}.d" 2>/dev/null || true
    fi
    systemctl daemon-reload >/dev/null 2>&1 || true
  fi
  # state.json is only the resource controller's transient recovery state;
  # remove it after all units are stopped so a later up starts from live
  # per-NS inventory instead of stale reservations.
  rm -f "$PIDFILE" "$STATEFILE" "$RUNROOT/state.json"
}

UP_IN_PROGRESS=0
cleanup_on_exit() {
  local rc="$?"
  if [ "$UP_IN_PROGRESS" = 1 ]; then
    UP_IN_PROGRESS=0
    stop_environment 1 || true
  fi
  exit "$rc"
}
trap cleanup_on_exit EXIT

wait_socket() {
  local unit="$1" socket="$2" i
  for i in $(seq 1 60); do
    [ -S "$socket" ] && return 0
    unit_active "$unit" || { systemctl status "$unit" --no-pager >&2 || true; return 1; }
    sleep 0.5
  done
  return 1
}

wait_api() {
  local i
  for i in $(seq 1 60); do
    curl -ksS --noproxy '*' -o /dev/null "https://127.0.0.1:$API_PORT/health" -H "Host: api.$DOMAIN" 2>/dev/null && return 0
    unit_active "$NODE_UNIT" || return 1
    sleep 0.5
  done
  return 1
}

manifest_key_for_tenant() {
  local key="${MANIFEST_KEY:-}"
  if [ -z "$key" ]; then
    key="$(sed -n 's/.*manifest:.*key:[[:space:]]*"\{0,1\}\([0-9A-Fa-f]\{64\}\)"\{0,1\}.*/\1/p' "$MANIFEST_CONFIG" | head -n 1)"
  fi
  [[ "$key" =~ ^[0-9A-Fa-f]{64}$ ]] || die "无法从 $MANIFEST_CONFIG 得到 64 hex manifest key；请显式设置 MANIFEST_KEY"
  printf '%s' "$key"
}

# ════════════════════════════════════════════════════════════════════════════
# up
# ════════════════════════════════════════════════════════════════════════════
do_up() {
  [ "$CMD" = up ] || [ "$CMD" = replace ] || return 0
  require_root_and_tools
  UP_IN_PROGRESS=1
  ensure_nfs_dir
  prepare_assets
  if [ "$CMD" = replace ]; then
    say "replace NS=$NS：停止旧 unit/进程，保留 DB/TLS/tenant"
    stop_environment 1
  elif deployment_present; then
    say "up NS=$NS：发现已有实例或残留，按同一 NS 收敛重建（保留 DB/TLS/tenant）"
    stop_environment 1
  fi
  select_auto_ports
  validate_ports
  select_auto_fip
  check_conflicts || die "资源冲突；请换 NS/端口/FIP_BASE，或先执行 down"
  echo "${c_dim}━━━ deploy $CMD  NS=$NS  bins=$BINS  api=:$API_PORT  proxy=$PROXY_LISTEN  FIP=$FIP_CIDR ━━━${c_off}"

  write_configs
  write_units
  : > "$STATEFILE"
  state_set phase up
  state_set namespace "$NS"
  state_set connector_path "$CONNECTOR"
  state_set node_path "$NODE_CTL"
  state_set bins "$BINS"
  state_set fip_base "$FIP_BASE"
  state_set mac "$MAC"
  state_set cloud_hypervisor_cpus_model "$CLOUD_HYPERVISOR_CPUS_MODEL"
  state_set vswitch_ports "$VSWITCH_PORTS"
  state_set proxy_netns "$PROXY_NETNS"
  state_set manifest_config "$MANIFEST_CONFIG"
  state_set nfs_dir "$NFS_DIR"
  state_set remote_snapshot_parent "$REMOTE_SNAPSHOT_PARENT"
  state_set api_port "$API_PORT"
  state_set proxy_port "$PROXY_PORT"
  state_set mmds_port "$MMDS_PORT"
  state_set metrics_port "$METRICS"
  state_set proxy_listen "$PROXY_LISTEN"
  write_smoke
  write_hosts
  HOST_DNS="$(awk '/^nameserver[[:space:]]+[0-9.]+/{print $2; exit}' /etc/resolv.conf 2>/dev/null || true)"
  dns_shared_add
  nat_apply add

  say "启动 $NETNS_UNIT → $VS_UNIT → $NODE_UNIT → $PROXY_UNIT"
  systemctl start "$NETNS_UNIT"
  systemctl start "$VS_UNIT"
  wait_socket "$VS_UNIT" "$TAPFD_SOCK" || die "vswitch 未就绪（查 systemctl status $VS_UNIT）"
  systemctl start "$NODE_UNIT"
  wait_socket "$NODE_UNIT" "$CFG_SOCKET" || die "conductor config socket 未就绪"
  wait_api || die "conductor API 未就绪"

  TENANT_ENV="$CFGDIR/tenant.env"
  if [ ! -f "$TENANT_ENV" ]; then
    say "租户 onboarding（manifest-key add + API key）"
    MK="$(manifest_key_for_tenant)"
    API_SECRET="$("$E2B_KEY_CTL" derive-api-secret "$MK")"
    "$NODE_CTL" manifest-key add --api-secret "$API_SECRET" --label "$NS" --socket "$CFG_SOCKET" "$MK" >"$LOGDIR/manifest-key.log" 2>&1 || {
      tail -n 80 "$LOGDIR/manifest-key.log" >&2
      die "manifest-key add 失败"
    }
    AK="$("$E2B_KEY_CTL" gen-apikey "$API_SECRET")"
    cat > "$TENANT_ENV" <<ENV
# NS=$NS 租户凭据（本脚本生成）
export E2B_DOMAIN='$DOMAIN'
export E2B_API_URL='https://api.$DOMAIN:$API_PORT'
export E2B_SANDBOX_URL='https://$DOMAIN:$PROXY_PORT'
export E2B_API_KEY='$AK'
export E2B_ACCESS_TOKEN='sk_demo'
export SSL_CERT_FILE='$CFGDIR/tls.crt'
export REQUESTS_CA_BUNDLE='$CFGDIR/tls.crt'
export CURL_CA_BUNDLE='$CFGDIR/tls.crt'
export NO_PROXY='*'
ENV
    chmod 600 "$TENANT_ENV"
  else
    say "复用已有租户凭据 $TENANT_ENV"
  fi

  systemctl start "$PROXY_UNIT"
  for i in $(seq 1 60); do
    port_in_use "$PROXY_PORT" && proxy_port_in_use "$MMDS_PORT" && break
    unit_active "$PROXY_UNIT" || die "proxy 未就绪"
    sleep 0.5
  done
  port_in_use "$PROXY_PORT" || die "proxy data_listen 未监听 :$PROXY_PORT"
  proxy_port_in_use "$MMDS_PORT" || die "proxy MMDS 未监听于 ${PROXY_NETNS:-host} :$MMDS_PORT"

  ok "NS=$NS 部署完成（systemd 管理，存储后端未启动）"
  echo "  workdir  $WORKDIR"
  echo "  bins     $BINS"
  echo "  api      https://api.$DOMAIN:$API_PORT"
  echo "  proxy    $PROXY_LISTEN"
  echo "  ports    mmds=:$MMDS_PORT metrics=:$METRICS/$PROXY_METRICS"
  echo "  network  vswitch=$SWITCH netns=$NETNS proxy_netns=$PROXY_NETNS vswitch_ports=$VSWITCH_PORTS fip=$FIP_CIDR mgmt=$MGMT"
  echo "  units    $NETNS_UNIT $VS_UNIT $NODE_UNIT $PROXY_UNIT"
  echo "  ch       CLOUD_HYPERVISOR_CPUS_MODEL=$CLOUD_HYPERVISOR_CPUS_MODEL (runner/builder drop-in)"
  echo "  cfg      $CFGDIR/{conductor,proxy}.yaml"
  echo "  manifest $MANIFEST_CONFIG (yyl store/cache)"
  echo "  snapshots $NFS_DIR (remote=$REMOTE_SNAPSHOT_PARENT)"
  echo "  logs     $LOGDIR"
  echo "  tenant   source $TENANT_ENV"
  echo "  smoke    $SMOKE_FILE"
  echo "  state    $STATEFILE"
  echo "  关闭     $0 down $NS"
  echo "  换包     $0 replace $NS <new-bins-dir>"
  UP_IN_PROGRESS=0
}

do_prepare() {
  [ "$CMD" = prepare ] || return 0
  [ -n "$WORKDIR" ] || die "prepare 必须设置 WORKDIR，例如 WORKDIR=$REPO_ROOT/nova-work"
  prepare_assets
  write_configs
  write_units
  : > "$STATEFILE"
  state_set phase prepared
  state_set namespace "$NS"
  state_set connector_path "$CONNECTOR"
  state_set node_path "$NODE_CTL"
  state_set bins "$BINS"
  state_set fip_base "$FIP_BASE"
  state_set mac "$MAC"
  state_set cloud_hypervisor_cpus_model "$CLOUD_HYPERVISOR_CPUS_MODEL"
  state_set vswitch_ports "$VSWITCH_PORTS"
  state_set proxy_netns "$PROXY_NETNS"
  state_set manifest_config "$MANIFEST_CONFIG"
  state_set nfs_dir "$NFS_DIR"
  state_set remote_snapshot_parent "$REMOTE_SNAPSHOT_PARENT"
  state_set api_port "$API_PORT"
  state_set proxy_port "$PROXY_PORT"
  state_set mmds_port "$MMDS_PORT"
  state_set metrics_port "$METRICS"
  state_set proxy_listen "$PROXY_LISTEN"
  write_hosts
  write_smoke
  echo "━━━ prepare 完成 NS=$NS ━━━"
  echo "  workdir  $WORKDIR"
  echo "  bins     $BINS"
  echo "  config   $CFGDIR/{conductor,proxy}.yaml"
  echo "  units    $UNIT_DIR"
  echo "  ch       CLOUD_HYPERVISOR_CPUS_MODEL=$CLOUD_HYPERVISOR_CPUS_MODEL (runner/builder drop-in)"
  echo "  manifest $MANIFEST_CONFIG (yyl store/cache)"
  echo "  snapshots $NFS_DIR (remote=$REMOTE_SNAPSHOT_PARENT)"
  echo "  network  vswitch=$SWITCH netns=$NETNS proxy_netns=$PROXY_NETNS vswitch_ports=$VSWITCH_PORTS fip=$FIP_CIDR mgmt=$MGMT"
  echo "  logs     $LOGDIR"
  echo "  state    $STATEFILE"
  echo "  smoke    $SMOKE_FILE"
}

# ════════════════════════════════════════════════════════════════════════════
# down
# ════════════════════════════════════════════════════════════════════════════
do_down() {
  [ "$CMD" = down ] || return 0
  require_root_and_tools
  echo "${c_dim}━━━ deploy down  NS=$NS ━━━${c_off}"
  stop_environment 1
  ok "NS=$NS 已关闭（runner/builder + node/proxy + vswitch + netns + NAT 已清理）"
  echo "  保留     配置/DB/TLS/tenant：$CFGDIR $BASEROOT $SAVED"
  echo "  未处理   外部 store/cache 存储服务（不属于本 NS）"
}

# ════════════════════════════════════════════════════════════════════════════
# status
# ════════════════════════════════════════════════════════════════════════════
do_status() {
  [ "$CMD" = status ] || return 0
  echo "NS=$NS switch=$SWITCH netns=$NETNS proxy_netns=$PROXY_NETNS vswitch_ports=$VSWITCH_PORTS api=:$API_PORT proxy=$PROXY_LISTEN fip=$FIP_CIDR mac=$MAC ch_cpu_model=$CLOUD_HYPERVISOR_CPUS_MODEL"
  for unit in "$NETNS_UNIT" "$VS_UNIT" "$NODE_UNIT" "$PROXY_UNIT"; do
    if command -v systemctl >/dev/null 2>&1 && unit_active "$unit"; then echo "  ${c_grn}✓${c_off} unit $unit active"; else echo "  ${c_red}✗${c_off} unit $unit inactive"; fi
  done
  if netns_exists; then echo "  ${c_grn}✓${c_off} netns $NETNS"; else echo "  ${c_red}✗${c_off} netns $NETNS"; fi
  if proxy_netns_exists; then echo "  ${c_grn}✓${c_off} connector mgmt netns $PROXY_NETNS (proxy worker)"; else echo "  ${c_red}✗${c_off} connector mgmt netns $PROXY_NETNS"; fi
  for socket in "$CFG_SOCKET" "$TAPFD_SOCK"; do
    [ -S "$socket" ] && echo "  ${c_grn}✓${c_off} socket $socket" || echo "  ${c_red}✗${c_off} socket $socket"
  done
  for port_name in API_PORT PROXY_PORT MMDS_PORT METRICS PROXY_METRICS; do
    if [ "$port_name" = MMDS_PORT ]; then
      port_check=proxy_port_in_use
    else
      port_check=port_in_use
    fi
    if "$port_check" "${!port_name}"; then echo "  ${c_grn}✓${c_off} $port_name :${!port_name}"; else echo "  ${c_red}✗${c_off} $port_name :${!port_name}"; fi
  done
  [ -f "$TENANT_ENV" ] && echo "  tenant $TENANT_ENV" || echo "  tenant (not onboarded)"
  [ -f "$MANIFEST_CONFIG" ] && echo "  storage manifest_config $MANIFEST_CONFIG (external)" || echo "  storage manifest_config missing: $MANIFEST_CONFIG"
  echo "  snapshots nfs_dir $NFS_DIR (remote $REMOTE_SNAPSHOT_PARENT)"
  echo "  manifest  independent config $MANIFEST_CONFIG (yyl store/cache)"
}

case "$CMD" in
  up|replace) do_up ;;
  prepare) do_prepare ;;
  down)    do_down ;;
  status)  do_status ;;
esac
