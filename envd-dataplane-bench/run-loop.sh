#!/bin/sh
# envd 数据面 bench 连跑循环: 每轮独立日志 bench-runN.log, 索引追加 bench-soak-index.tsv
# 用法: TENANT_ENV=<节点tenant.env路径> sh run-loop.sh   (或 sh run-loop.sh <路径>)
#       已 source 过 tenant.env 的环境(E2B_SANDBOX_URL 已在)可免路径直接跑
# 停止: kill $(cat run-loop.pid)  (当前跑着的 dataplane_bench 会自然结束本轮后退出)
cd "$(dirname "$0")" || exit 1
TENANT_ENV="${TENANT_ENV:-$1}"
if [ -n "$TENANT_ENV" ]; then
  . "$TENANT_ENV"
elif [ -z "${E2B_SANDBOX_URL:-}" ]; then
  echo "用法: TENANT_ENV=<tenant.env 路径> sh run-loop.sh (或在已 source 的环境中直跑)" >&2
  exit 1
fi
n=0
while true; do
  n=$((n+1))
  ts=$(date '+%F %T')
  python3 dataplane_bench.py > "bench-run${n}.log" 2>&1
  rc=$?
  verdict=$(grep -m1 '^总结' "bench-run${n}.log" || echo "总结: 异常退出")
  fails=$(grep -c '✗ \[' "bench-run${n}.log")
  pc502=$(grep -c '未恢复' "bench-run${n}.log")
  printf '%s\trun%s\trc=%s\tfails=%s\tpc502=%s\t%s\n' "$ts" "$n" "$rc" "$fails" "$pc502" "$verdict" >> bench-soak-index.tsv
  # 只保留最近 5 份 run 日志, 防磁盘缓涨
  ls bench-run*.log 2>/dev/null | sort -V | head -n -5 | xargs -r rm -f
  sleep 20
done
