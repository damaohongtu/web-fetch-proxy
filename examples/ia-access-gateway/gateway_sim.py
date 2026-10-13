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
  - 头部原封不动透传：除网关自管头（逐跳头、Host、凭证改写）外，
    一切请求头（含供应商自定义头）与响应头（除逐跳头）原样转发
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

# 请求方向：网关不透传的头（小写比较）——仅逐跳头（RFC 9110 §7.6.1）和网关
# 自管头。其余一切请求头（含供应商自定义头，如 X-Api-Key）原封不动透传，
# 绝不能实现白名单式过滤，否则供应商私有头会被丢弃。
GATEWAY_MANAGED_REQUEST_HEADERS = {
    "host",                     # 网关改为云端地址
    "content-length",           # 按实际读取的正文重新发送（见 _proxy）
    "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
    "te", "trailer", "transfer-encoding", "upgrade",  # 逐跳头
    "authorization",            # 上游凭证：改名 X-Upstream-Authorization 后透传
    "x-upstream-authorization",  # 网关凭证改写专用头
}
# 响应方向：仅排除逐跳头，其余（content-encoding、mcp-session-id、
# cache-control 等）原样透传给 MCP 客户端。
GATEWAY_MANAGED_RESPONSE_HEADERS = {
    "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
    "te", "trailer", "transfer-encoding", "upgrade",
    "content-length",           # 流式转发由连接关闭界定正文边界
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

        conn = http.client.HTTPSConnection(
            CLOUD_HOST, CLOUD_PORT, context=self.tls_context, timeout=3600
        )
        try:
            # 逐头转发：除网关自管头外原封不动（保留重复头与原始大小写）；
            # skip_host/skip_accept_encoding 避免注入客户端没发的头。
            conn.putrequest(self.command, "/v1/proxy" + suffix,
                            skip_host=True, skip_accept_encoding=True)
            conn.putheader("Host", CLOUD_HOST if CLOUD_PORT == 443
                           else "%s:%d" % (CLOUD_HOST, CLOUD_PORT))
            for name, value in self.headers.items():
                if name.lower() in GATEWAY_MANAGED_REQUEST_HEADERS:
                    continue
                conn.putheader(name, value)
            content_length = self.headers.get("Content-Length")
            if content_length is not None:
                conn.putheader("Content-Length", content_length)
            # 凭证改写规则（真实网关必须实现同样的规则）：
            #   MCP 客户端的 Authorization 是发给上游的凭证（供应商配置原样使用），
            #   网关把它改名为 X-Upstream-Authorization，由云端 Worker 映射回
            #   转发上游时的 Authorization；随后网关注入云端自己的 Bearer Token。
            #   显式提供 X-Upstream-Authorization 时以它为准（优先级高于改名）。
            upstream_credentials = self.headers.get("X-Upstream-Authorization") \
                or self.headers.get("Authorization")
            if upstream_credentials:
                conn.putheader("X-Upstream-Authorization", upstream_credentials)
            conn.putheader("Authorization", "Bearer " + self.token)
            conn.endheaders(body)
            response = conn.getresponse()
            self.log_request(response.status)
            self.send_response_only(response.status)
            for name, value in response.getheaders():
                if name.lower() not in GATEWAY_MANAGED_RESPONSE_HEADERS:
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
    print("any public http/https upstream is admitted; x-mcp-host selects it per request")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        server.server_close()


if __name__ == "__main__":
    main()
