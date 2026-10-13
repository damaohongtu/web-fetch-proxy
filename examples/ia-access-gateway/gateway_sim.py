#!/usr/bin/env python3
"""ia-access-gateway 网关模拟器。

模拟企业内部网关（https://ia-api.cmfcha.com/ia-access-gateway/mcp/...）的行为，
供本地端到端联调使用：

    生产服务器 MCP 客户端
        → 本模拟器 (http://127.0.0.1:8088/ia-access-gateway/mcp/<path>)
        → 云端 web-fetch-proxy (https://<cloud>/v1/proxy/<path>, mTLS + Bearer)
        → 真实 MCP 上游 (由 x-mcp-host 指定，如 https://mcp-server.comein.cn)

职责（与真实网关一致，见 docs/architecture.md 的网关配置清单）：
  - 剥离公共前缀 /ia-access-gateway/mcp，改写为云端 /v1/proxy
  - 凭证改写：客户端 Authorization（上游凭证）改名为 X-Upstream-Authorization，
    注入云端 Bearer Token 与 mTLS 客户端证书（网关是证书持有方）
  - 原样透传 MCP 协议头：x-mcp-host / x-user-id / Mcp-Session-Id /
    MCP-Protocol-Version / Last-Event-ID / Accept / Content-Type
  - 不缓冲、不压缩、不缓存、不重试、不拦截错误页——SSE 逐块转发

仅用 Python 3 标准库。用法：

    python3 gateway_sim.py            # 前台运行
    CLOUD_HOST=1.2.3.4 CLOUD_PORT=8443 python3 gateway_sim.py
"""

import http.client
import os
import ssl
import sys

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

GATEWAY_PREFIX = "/ia-access-gateway/mcp"
CLOUD_HOST = os.environ.get("CLOUD_HOST", "127.0.0.1")
CLOUD_PORT = int(os.environ.get("CLOUD_PORT", "8443"))
LISTEN_HOST = os.environ.get("GATEWAY_LISTEN_HOST", "127.0.0.1")
LISTEN_PORT = int(os.environ.get("GATEWAY_LISTEN_PORT", "8088"))

CA_CERT = os.path.join(REPO_ROOT, "var", "pki", "ca.crt")
CLIENT_CERT = os.path.join(REPO_ROOT, "var", "pki", "client.crt")
CLIENT_KEY = os.path.join(REPO_ROOT, "var", "pki", "client.key")
TOKEN_FILE = os.path.join(REPO_ROOT, "var", "secrets", "fetch_auth_token")

# 请求方向：MCP 客户端 → 云端需要透传的协议头（小写比较）。
# Authorization 不在透传之列：客户端的 Authorization 是发给上游的凭证，
# 由网关改名为 X-Upstream-Authorization（见 _proxy 中的改写规则）。
PASSTHROUGH_REQUEST_HEADERS = {
    "x-mcp-host",
    "x-user-id",
    "mcp-session-id",
    "mcp-protocol-version",
    "last-event-id",
    "accept",
    "content-type",
}
# 响应方向：云端 → MCP 客户端需要透传的协议头。
PASSTHROUGH_RESPONSE_HEADERS = {
    "mcp-session-id",
    "content-type",
}

ALLOWED_METHODS = ("GET", "POST", "DELETE")


def build_tls_context():
    context = ssl.create_default_context(cafile=CA_CERT)
    context.load_cert_chain(certfile=CLIENT_CERT, keyfile=CLIENT_KEY)
    # 云端证书使用 IP SAN（无域名），确认 SUBJECT_ALTERNATIVE_NAME 匹配即可。
    context.check_hostname = True
    return context


def load_token():
    with open(TOKEN_FILE, "r", encoding="utf-8") as handle:
        return handle.read().strip()


class GatewayHandler(BaseHTTPRequestHandler):
    # HTTP/1.0：每请求一条连接、以连接关闭结束响应体，
    # 无需 Content-Length 即可流式转发 SSE。
    protocol_version = "HTTP/1.0"
    tls_context = None
    token = ""

    def _is_proxied_path(self):
        return self.path == GATEWAY_PREFIX or self.path.startswith(GATEWAY_PREFIX + "/")

    def _proxy(self):
        if not self._is_proxied_path():
            self.send_error(404, "unknown gateway route")
            return
        if self.command not in ALLOWED_METHODS:
            self.send_error(405, "streamable-http only supports GET/POST/DELETE")
            return

        suffix = self.path[len(GATEWAY_PREFIX):] or "/"
        body = None
        length = int(self.headers.get("Content-Length") or 0)
        if length > 0:
            body = self.rfile.read(length)

        headers = {
            name: value
            for name, value in self.headers.items()
            if name.lower() in PASSTHROUGH_REQUEST_HEADERS
        }
        # 凭证改写规则（真实网关必须实现同样的规则）：
        #   MCP 客户端的 Authorization 是发给上游的凭证（供应商配置原样使用），
        #   网关把它改名为 X-Upstream-Authorization，由云端 Worker 映射回
        #   转发上游时的 Authorization；随后网关注入云端自己的 Bearer Token。
        #   显式提供 X-Upstream-Authorization 时以它为准（优先级高于改名）。
        upstream_credentials = self.headers.get("X-Upstream-Authorization") \
            or self.headers.get("Authorization")
        if upstream_credentials:
            headers["X-Upstream-Authorization"] = upstream_credentials
        headers["Authorization"] = "Bearer " + self.token

        conn = http.client.HTTPSConnection(
            CLOUD_HOST, CLOUD_PORT, context=self.tls_context, timeout=3600
        )
        try:
            conn.request(self.command, "/v1/proxy" + suffix, body=body, headers=headers)
            response = conn.getresponse()
            self.send_response(response.status)
            for name, value in response.getheaders():
                if name.lower() in PASSTHROUGH_RESPONSE_HEADERS:
                    self.send_header(name, value)
            self.end_headers()
            # 逐块转发并立即刷新：SSE 不缓冲。
            while True:
                chunk = response.read(4096)
                if not chunk:
                    break
                self.wfile.write(chunk)
                self.wfile.flush()
        except Exception as error:  # noqa: BLE001 - 网关把上游故障原样报告给客户端
            self.log_error("upstream failure: %s", error)
            try:
                self.send_error(502, "cloud web-fetch-proxy unreachable")
            except Exception:  # noqa: BLE001 - 响应头可能已部分发出
                pass
        finally:
            conn.close()

    do_GET = _proxy
    do_POST = _proxy
    do_DELETE = _proxy

    def log_message(self, fmt, *args):
        sys.stderr.write("[gateway] %s - %s\n" % (self.address_string(), fmt % args))


def main():
    for path in (CA_CERT, CLIENT_CERT, CLIENT_KEY, TOKEN_FILE):
        if not os.path.isfile(path):
            sys.exit("missing %s; run scripts/generate-mtls.sh and cloudctl.sh first" % path)
    GatewayHandler.tls_context = build_tls_context()
    GatewayHandler.token = load_token()
    server = ThreadingHTTPServer((LISTEN_HOST, LISTEN_PORT), GatewayHandler)
    print("ia-access-gateway simulator listening on http://%s:%d%s/"
          % (LISTEN_HOST, LISTEN_PORT, GATEWAY_PREFIX))
    print("forwarding to https://%s:%d/v1/proxy/ (mTLS + Bearer)" % (CLOUD_HOST, CLOUD_PORT))
    print("upstream allowlist lives in the cloud .env (ALLOWED_UPSTREAM_HOSTS)")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        server.server_close()


if __name__ == "__main__":
    main()
