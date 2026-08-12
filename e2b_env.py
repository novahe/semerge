"""共享环境加载器：把 e2b-env.sh 的 export 解析进 os.environ，让每个 bench 脚本自包含
（直接 `python3 <script>.py` 或 `./<script>.py` 运行，不再需要 .sh 壳source 环境后再 exec）。

e2b-env.sh 是简单的 `export KEY='value'` 文件（无命令替换）；这里按字面解析。
语义对标 `source e2b-env.sh`：e2b-env.sh 的值覆盖调用方已有环境（它是唯一配置源）。
"""
import os
import re
import sys

import requests as _requests

_ENV_FILE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "e2b-env.sh")


def api_url(path):
    base = os.environ.get("E2B_API_URL") or f"https://api.{os.environ['E2B_DOMAIN']}"
    return base.rstrip("/") + path


def api_request(method, path, **kwargs):
    """直调 orchestrator API（绕过 e2b SDK 的脆弱错误解析——SDK 把任何不匹配 Error schema
    的非 200 响应吞成 KeyError('code')，丢失真实原因）。返回 requests.Response。"""
    headers = kwargs.pop("headers", {})
    headers.setdefault("Authorization", f"Bearer {os.environ['E2B_API_KEY']}")
    kwargs.setdefault("verify", os.environ.get("SSL_CERT_FILE"))
    return _requests.request(method, api_url(path), headers=headers, **kwargs)


def diagnose(sid):
    """GET /sandboxes/{sid} 拿真实状态/错误。返回 (status_code, body_text)。
    在 SDK 抛 KeyError('code') 这类被吞的错误时，用它还原 orchestrator 的真实 HTTP 响应。"""
    try:
        r = api_request("GET", f"/sandboxes/{sid}", timeout=15)
        return r.status_code, r.text[:500]
    except Exception as e:
        return -1, f"(诊断请求也失败: {e})"


def load(path=None):
    """解析 <dir>/e2b-env.sh 的 export 行，写入 os.environ。返回 os.environ。"""
    p = path or os.environ.get("E2B_ENV_FILE") or _ENV_FILE
    if not os.path.isfile(p):
        sys.exit(f"✗ 找不到 env 文件：{p}")
    for line in open(p):
        m = re.match(r"\s*export\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$", line)
        if not m:
            continue
        k, raw = m.group(1), m.group(2).strip()
        if raw and raw[0] in ("'", '"'):
            # 引号值：取两个引号之间的内容，忽略行内注释（'value' # comment → value）
            end = raw.find(raw[0], 1)
            v = raw[1:end] if end != -1 else raw[1:]
        else:
            # 无引号：去掉行内注释（ # ...）
            v = re.sub(r"\s+#.*$", "", raw).strip()
        os.environ[k] = v
    return os.environ


def require(*keys):
    """检查必需的环境变量；缺则报错退出。"""
    missing = [k for k in keys if not os.environ.get(k)]
    if missing:
        sys.exit(f"✗ 缺环境变量 {missing} — 检查 e2b-env.sh")
