# source this — 本 bench 专属 conductor（:4443，config/config-4443.yaml，用 /root/sandbox 重建的 node-ctl），e2b SDK 经 E2B_API_URL 连入。
export E2B_DOMAIN='sandboxes.demo.local'
export E2B_API_URL='https://api.sandboxes.demo.local:4443'   # 独立 conductor（重建的 node-ctl，config-4443.yaml）
export E2B_SANDBOX_URL='https://sandboxes.demo.local:4443'   # 数据面（connect/commands）也走 :4443
export E2B_API_KEY='e2b_02afe769d85baf42db690d146a72c5b9d291d6bd579b2094928fef106dc1e6bbd6327b7c'  # MAC token for manifestKey=0001 (OBS store 加密键), image 65c3599e
export E2B_ACCESS_TOKEN='sk_demo'
export SSL_CERT_FILE='/root/sandbox/e2b-bench-file-bare/config/tls.crt'
export REQUESTS_CA_BUNDLE='/root/sandbox/e2b-bench-file-bare/config/tls.crt'
export NO_PROXY='*'
export E2B_NODE_CTL_SOCKET='/tmp/e2b-bench-4443/node-ctl.socket'
export E2B_RUN_ROOT='/tmp/e2b-bench-4443/run'   # openclaw_net 在此找 <sid>/*.yaml 推导 floating IP（须与 config-4443.yaml paths.run_root 一致）
export E2B_BIN='/root/sandbox/orchestrator/bin'
export E2B_SAVED_DIR='/mnt/sfs_turbo/e2b-bench-file-optimization/snaps'
# e2bdemo vswitch floating_ip_base —— 已从 100.100.96.0 挪到 100.100.128.0（独立 /20，
# 避开共享机上 yyl 的 sw0 100.100.96.0/20）。openclaw_net 按 <base>.(port-1) 推导 host-reachable
# floating IP，故此处必须与 vswitch floating_ip_base 一致，否则 bench curl 错网段。
export E2B_FIP_BASE='100.100.128'
