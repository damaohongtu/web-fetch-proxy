# Web Fetch 云端出口技术方案

## 1. 目标

内部服务器只需开通到一台云服务器的 HTTPS 网络。Agent 向内部 Web-Fetch 提交外部 URL，内部服务通过 mTLS 调用云端，云端 Fetch Worker 发起实际互联网请求并返回结构化结果。

```text
Agent
  -> 内部 Web-Fetch
  -> 云端 Nginx（TLS、客户端证书、限流、日志）
  -> 云端 Fetch Worker（鉴权、URL 安全校验、HTTP 请求）
  -> 外部网站
```

Nginx 不解析请求中的 URL，也不直接动态代理外部地址。它只把经过 mTLS 验证的请求转发到同一 Docker 私有网络内的 Worker。

## 2. 各层参数

### 2.1 Agent 到内部 Web-Fetch

```http
POST /web-fetch
Content-Type: application/json

{"url":"https://example.com/article"}
```

内部服务可接受以下扩展参数；第一版只传 `url` 即可：

| 参数 | 必填 | 默认值 | 服务端上限或规则 |
|---|---:|---:|---|
| `url` | 是 | - | 仅 HTTP/HTTPS，不允许 URL userinfo |
| `method` | 否 | `GET` | 仅 `GET`、`HEAD` |
| `headers` | 否 | `{}` | 仅允许明确的请求头白名单 |
| `timeout_ms` | 否 | 30000 | 不超过 60000 |
| `max_bytes` | 否 | 10 MiB | 不超过 50 MiB |
| `follow_redirects` | 否 | true | 每一跳重新校验 |
| `max_redirects` | 否 | 5 | 不超过 10 |

### 2.2 内部 Web-Fetch 到云端 Nginx

```http
POST https://<云服务器公网IP>/v1/fetch
Content-Type: application/json
Authorization: Bearer <token>
X-Request-ID: <uuid>
```

TLS 参数：

| 参数 | 内容 |
|---|---|
| 服务端证书 | `certs/server.crt` |
| 服务端私钥 | `certs/server.key` |
| 客户端证书 | 内部服务器持有的 `certs/client.crt` |
| 客户端私钥 | 内部服务器持有的 `certs/client.key` |
| 信任 CA | 双方信任 `certs/ca.crt` |
| TLS 版本 | TLS 1.2、TLS 1.3 |

不需要域名。使用公网 IP 访问时，服务端证书的 Subject Alternative Name 必须包含 `IP:<云服务器公网IP>`。只在证书 CN 中填写 IP 不足以通过现代 TLS 客户端的主机身份校验。

请求体示例：

```json
{
  "url": "https://example.com/article",
  "method": "GET",
  "headers": {
    "Accept": "text/html",
    "Accept-Language": "zh-CN,zh;q=0.9"
  },
  "timeout_ms": 30000,
  "max_bytes": 10485760,
  "follow_redirects": true,
  "max_redirects": 5
}
```

### 2.3 Nginx 到 Fetch Worker

Nginx 原样转发 JSON 和 `Authorization`，并覆盖或补充可信网关头：

```http
POST http://fetch-worker:8080/v1/fetch
X-Request-ID: <客户端请求ID或Nginx生成ID>
X-Client-IP: <Nginx观察到的源IP>
X-Client-Cert-Subject: <客户端证书Subject>
```

网关限制：

| 参数 | 值 |
|---|---:|
| 请求体上限 | 64 KiB |
| 单来源速率 | 10 请求/秒 |
| 突发量 | 20 |
| 单来源并发连接 | 20 |
| Worker 连接超时 | 3 秒 |
| Worker 读取超时 | 70 秒 |

### 2.4 Fetch Worker 到外部网站

Worker 根据 JSON 构造标准 HTTP 请求。允许向外传递：`Accept`、`Accept-Language`、`User-Agent`、`Range`、`If-None-Match`、`If-Modified-Since`。

明确禁止转发 `Authorization`、`Proxy-Authorization`、`Cookie`、`Host`、`Forwarded`、`X-Forwarded-For` 等敏感或逐跳请求头。

Worker 会拒绝回环、私网、链路本地、CGNAT、多播、保留地址、云元数据地址和云服务器本机地址。每次 DNS 连接和重定向均重新执行 URL、端口和目标 IP 校验。

## 3. 返回格式

成功访问外部网站时，云端接口自身返回 HTTP 200，外部网站状态码放在 JSON 的 `status` 字段：

```json
{
  "request_id": "...",
  "requested_url": "https://example.com",
  "final_url": "https://example.com/",
  "status": 200,
  "headers": {"content-type": "text/html; charset=UTF-8"},
  "body": "<html>...</html>",
  "body_encoding": "utf8",
  "size": 1256,
  "truncated": false,
  "redirects": [],
  "elapsed_ms": 186
}
```

非 UTF-8 内容使用 Base64。达到响应上限时返回已经读取的前 `max_bytes` 字节，并设置 `truncated: true`。

代理自身错误使用以下状态：

| HTTP | 错误码 | 含义 |
|---:|---|---|
| 400 | `INVALID_REQUEST` / `INVALID_URL` | JSON 或 URL 不合法 |
| 401 | `UNAUTHORIZED` | Token 缺失或错误 |
| 403 | `DESTINATION_BLOCKED` | 目标地址被安全策略阻止 |
| 405 | `METHOD_NOT_ALLOWED` | 方法不允许 |
| 429 | `TOO_MANY_REQUESTS` | Worker 并发达到上限 |
| 502 | `DNS_FAILED` / `CONNECT_FAILED` / `TLS_FAILED` | 外部网络失败 |
| 504 | `FETCH_TIMEOUT` | 外部请求超时 |

## 4. 安全边界

1. 云安全组只允许内部服务器固定出口 IP 访问 TCP 443。
2. Nginx 强制校验客户端证书；没有合法客户端私钥无法完成 TLS 握手。
3. Bearer Token 是独立的第二层鉴权，存放在 Docker secret 中。
4. Worker 不发布宿主机端口，只能被 Nginx 通过私有容器网络访问。
5. Worker 对 DNS 解析结果和真正建立的连接使用同一批经过验证的 IP，降低 DNS 重绑定风险。
6. `BLOCKED_CIDRS` 必须加入云服务器自身公网 IP，避免 Worker 回连云服务器公开服务。
7. 生产环境应定期轮换客户端证书和 Token，并为证书配置较短有效期。
8. URL 查询串可能含敏感信息，默认访问日志只由 Nginx 记录 API 路径；Worker 日志不输出请求正文。
