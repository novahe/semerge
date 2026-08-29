#!/usr/bin/env bash
# 一键下载 kuasar-sandbox 最新（或指定）GitHub release，并平铺成 deploy-ns.sh 直接可用的 bins 目录。
#
# 用法:
#   ./download-release-bins.sh [tag] [-o 输出目录]
#     tag   release tag，缺省 latest（自动查最新，含 prerelease）
#     -o    输出目录，缺省 <repo>/orchestrator/bin/x86_64-gh-<tag里的日期>
#
# 环境变量:
#   GH_MIRROR   强制下载镜像前缀（如 https://ghfast.top/）；留空则对候选镜像测速自动选择
#   REPO        GitHub 仓（缺省 kuasar-sandbox/kuasar-sandbox）
#
# 产物布局（与 deploy-ns.sh prepare_assets 期望一致，全部平铺在输出目录顶层）:
#   node-ctl e2b-key-ctl cluster-ctl node-stub-ctl   ← orchestrator 包
#   connector-ctl                                    ← connector 包
#   sandbox-ctl sandbox-init cloud-hypervisor        ← sandboxer 包
#   vmlinux                                          ← vmlinux 包
#   sandbox-runtime.bundle (+复制一份 .erofs)         ← sandbox-runtime 包
#   flatten-ctl mkfs.erofs                           ← sandbox-runtime 包
#   cache-ctl manifest-ctl store-ctl                 ← accelerator 包
#   （platform-release 包只含 docs/test，不平铺；所有包统一取 tar 内 ./bin/* 顶层平铺）
#
# 之后接:  ./deploy-ns.sh replace nova <输出目录>
set -euo pipefail

REPO="${REPO:-kuasar-sandbox/kuasar-sandbox}"
SCRIPT_DIR="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
API="https://api.github.com/repos/$REPO"

TAG="latest"
OUT=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) OUT="${2:?}"; shift 2 ;;
    *) TAG="$1"; shift ;;
  esac
done

die() { echo "✗ $*" >&2; exit 1; }
log() { echo "• $*"; }
ok()  { echo "  ✓ $*"; }

# ── 1. 解析 release tag ─────────────────────────────────────────────
fetch_release_json() {  # $1=tag → stdout JSON；latest 取列表第一个（本仓 release 全是 prerelease）
  if [ "$1" = "latest" ]; then
    curl -sL --max-time 30 "$API/releases?per_page=1" | python3 -c 'import json,sys; rs=json.load(sys.stdin); sys.exit(1) if not isinstance(rs,list) or not rs else print(json.dumps(rs[0]))'
  else
    curl -sL --max-time 30 "$API/releases/tags/$1"
  fi
}

REL_JSON="$(fetch_release_json "$TAG" || true)"
if [ -n "$REL_JSON" ] && echo "$REL_JSON" | python3 -c 'import json,sys; json.load(sys.stdin)' 2>/dev/null; then
  TAG="$(echo "$REL_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])')"
fi
[ "$TAG" != "latest" ] || die "无法解析最新 release tag（api.github.com 不可达？），请显式传 tag，例如: $0 release-v0.1.0-preview.20260825"

CACHE="$REPO_ROOT/orchestrator/bin/release-cache/$TAG"
mkdir -p "$CACHE"
[ -z "$OUT" ] && OUT="$REPO_ROOT/orchestrator/bin/x86_64-gh-$(echo "$TAG" | grep -oE '[0-9]{8}' | head -1 || echo "$TAG" | tr '/:' '__')"
log "release: $TAG"
log "缓存目录: $CACHE"
log "输出目录: $OUT"

# ── 2. 选择下载镜像（对首个大文件测 2MB 区间速度） ──────────────────
MIRRORS=("" "https://ghfast.top/" "https://gh-proxy.com/")
if [ -n "${GH_MIRROR:-}" ]; then
  MIRROR="$GH_MIRROR"
  log "使用指定镜像: ${MIRROR:-直连}"
else
  PROBE_URL="$(echo "$REL_JSON" | python3 -c '
import json,sys
r=json.load(sys.stdin)
assets=[a for a in r.get("assets",[]) if a["name"].endswith(".tar.gz")]
assets.sort(key=lambda a:-a["size"])
print(assets[0]["browser_download_url"] if assets else "")')"
  MIRROR=""; BEST=0
  if [ -n "$PROBE_URL" ]; then
    for m in "${MIRRORS[@]}"; do
      SP="$(curl -sL -m 20 -r 0-2097151 -o /dev/null -w '%{speed_download}' "${m}${PROBE_URL}" 2>/dev/null || echo 0)"
      SP="${SP:-0}"; SP="${SP%.*}"
      log "测速 ${m:-直连}: ${SP} B/s"
      if [ "$SP" -gt "$BEST" ]; then BEST="$SP"; MIRROR="$m"; fi
    done
  fi
  log "选用: ${MIRROR:-直连} (${BEST} B/s)"
fi
dl() {  # dl <url> <输出文件>（断点续传+重试）
  local i
  for i in 1 2 3 4 5; do
    curl -sSL --retry 3 --retry-delay 2 -C - -o "$2" "${MIRROR}$1" && return 0
    log "重试($i) $2"; sleep 3
  done
  die "下载失败: $1"
}

# ── 3. 下载 assets（已存在且校验通过的跳过） ────────────────────────
asset_urls() { echo "$REL_JSON" | python3 -c '
import json,sys
for a in json.load(sys.stdin).get("assets",[]):
    print(a["name"]+"\t"+a["browser_download_url"])'; }
# 无 API 结果时退回缓存里已有的 SHA256SUMS + tarball
if [ -z "$REL_JSON" ] || [ "$REL_JSON" = "None" ]; then
  [ -s "$CACHE/SHA256SUMS" ] || die "无 API 结果且缓存里没有 SHA256SUMS，无法继续"
  REL_JSON=""
fi

verified_ok() {  # 文件存在且 sha256 与 SHA256SUMS 一致
  local f="$1" want got
  [ -f "$CACHE/$f" ] || return 1
  want="$(awk -v f="$f" '$2==f{print $1}' "$CACHE/SHA256SUMS")"
  [ -n "$want" ] || return 1
  got="$(sha256sum "$CACHE/$f" | awk "{print \$1}")"
  [ "$got" = "$want" ]
}

if [ -n "$REL_JSON" ]; then
  while IFS=$'\t' read -r name url; do
    [ -n "$name" ] || continue
    [ "$name" != "SHA256SUMS" ] || continue   # 不在自身校验表内，统一由下面单独处理
    if verified_ok "$name"; then ok "跳过(已校验) $name"; continue; fi
    log "下载 $name"
    dl "$url" "$CACHE/$name"
  done < <(asset_urls)
fi
[ -s "$CACHE/SHA256SUMS" ] || dl "https://github.com/$REPO/releases/download/$TAG/SHA256SUMS" "$CACHE/SHA256SUMS"

log "SHA256 校验"
( cd "$CACHE" && sha256sum -c SHA256SUMS --quiet 2>&1 | head -5; ) || true
( cd "$CACHE" && sha256sum -c SHA256SUMS >/dev/null 2>&1 ) || die "sha256 校验失败（可删 $CACHE 后重跑）"
ok "全部校验通过"

# ── 4. 解包平铺：所有 tarball 的 ./bin/* 复制到输出目录顶层 ─────────
STAGE="$(mktemp -d /tmp/flatten-release.XXXXXX)"
trap 'rm -rf "$STAGE"' EXIT
for tb in "$CACHE"/*.tar.gz; do
  pkg="$(basename "$tb" .tar.gz)"
  mkdir -p "$STAGE/$pkg"
  tar xzf "$tb" -C "$STAGE/$pkg"
done

mkdir -p "$OUT"
COPIED=0
while IFS= read -r f; do
  name="$(basename "$f")"
  cp -f "$f" "$OUT/$name"
  chmod +x "$OUT/$name"
  COPIED=$((COPIED+1))
done < <(find "$STAGE" -type f -path '*/bin/*' ! -path '*/test/*' ! -path '*/deploy/*')
# conductor.yaml 历史上引用 sandbox-runtime.erofs；release 只带 .bundle（两者内容相同）
if [ -f "$OUT/sandbox-runtime.bundle" ] && [ ! -f "$OUT/sandbox-runtime.erofs" ]; then
  cp -f "$OUT/sandbox-runtime.bundle" "$OUT/sandbox-runtime.erofs"
fi
ok "平铺 $COPIED 个文件 → $OUT"

# ── 5. deploy-ns.sh 必需项自检 + 记录来源 ───────────────────────────
MISSING=""
for need in node-ctl connector-ctl e2b-key-ctl sandbox-ctl cloud-hypervisor vmlinux; do
  [ -x "$OUT/$need" ] || MISSING="$MISSING $need"
done
[ -f "$OUT/sandbox-runtime.erofs" ] || [ -f "$OUT/sandbox-runtime.bundle" ] || MISSING="$MISSING sandbox-runtime.{erofs,bundle}"
[ -z "$MISSING" ] || die "缺 deploy-ns 必需项:$MISSING"

{
  echo "tag: $TAG"
  [ -n "$REL_JSON" ] && echo "$REL_JSON" | python3 -c 'import json,sys; r=json.load(sys.stdin); print("commit:", r.get("target_commitish","")); print("published:", r.get("published_at",""))'
  echo "mirror: ${MIRROR:-direct}"
  echo "date: $(date -Is)"
  ( cd "$CACHE" && sha256sum SHA256SUMS *.tar.gz )
} > "$OUT/RELEASE-INFO"

echo
echo "━━━ done ━━━"
echo "bins:      $OUT"
echo "替换部署:  $SCRIPT_DIR/deploy-ns.sh replace nova $OUT"
echo "全新拉起:  $SCRIPT_DIR/deploy-ns.sh up nova $OUT"
