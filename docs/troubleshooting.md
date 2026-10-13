# 故障排查

## 首先收集状态

```bash
./scripts/cloudctl.sh preflight
./scripts/cloudctl.sh status
./scripts/cloudctl.sh logs nginx
./scripts/cloudctl.sh logs fetch-worker
docker compose --env-file .env -f compose.yaml config
```

日志中的 `X-Request-ID` 可关联 Nginx 与 Worker 请求。

宿主机持久日志：

```bash
tail -f var/log/nginx/access.log
tail -f var/log/nginx/error.log
tail -f var/log/fetch-worker/worker.log
```

`cloudctl.sh logs` 查看的是容器 stdout/stderr；宿主机文件用于 7 天留存和日志采集。

## 常见现象

| 现象 | 常见原因 | 处理 |
|---|---|---|
| TCP 443 连接超时 | 云安全组、内部防火墙或宿主防火墙未放行 | 核对云公网 IP、内部出口 IP 和 TCP 443 规则 |
| 启动时报端口占用 | 宿主已有 Nginx/Apache/容器监听 443 | `sudo ss -lntp \| grep ':443 '`，停止冲突服务 |
| `400 No required SSL certificate` | 未发送客户端证书 | 配置 `client.crt` 和 `client.key` |
| TLS `unknown ca` | 客户端没有信任 `ca.crt`，或证书来自其他 CA | 检查客户端 CA 和证书链 |
| TLS IP mismatch | 服务端证书 IP SAN 与访问 IP不同 | 用当前公网 IP重新签发服务端证书 |
| HTTP 403 来自 Nginx | 调用方公网出口不在 `ALLOWED_CLIENT_CIDR` | 确认真实 NAT 出口并更新 `.env`、重启 |
| HTTP 401 `UNAUTHORIZED` | Token 不一致或 Worker 尚未重启加载新 Token | 同步 Token 并执行 `cloudctl.sh restart` |
| HTTP 403 `DESTINATION_BLOCKED` | 目标解析到私网、回环、保留地址或 `BLOCKED_CIDRS` | 检查目标 DNS；不要关闭 SSRF 防护 |
| HTTP 502 `DNS_FAILED` | 云服务器 DNS 不可达或域名不存在 | 检查容器 DNS 和云出站 UDP/TCP 53 |
| HTTP 502 `CONNECT_FAILED` | 目标拒绝、云出站 80/443 被拦或 TLS失败 | 在 Worker 日志中按 request ID 排查 |
| HTTP 504 `FETCH_TIMEOUT` | 目标过慢或超时上限太低 | 检查目标和带宽，谨慎调整超时 |
| 返回 `truncated: true` | 响应超过 `max_bytes` | 确认业务确有需要后提高限制和内存预算 |
| Nginx healthy、请求仍失败 | 容器健康仅证明配置和 Worker健康路径 | 从内部服务器执行完整 mTLS 验收 |
| MCP 代理 400 `INVALID_REQUEST` | 缺 `x-mcp-host` 头，或值含路径/查询/userinfo | 客户端配置改为 origin 形式，如 `https://mcp-server.comein.cn` |
| MCP 代理 401 `UNAUTHORIZED` | 云端 Bearer Token 缺失或错误（网关未注入） | 网关注入云端 Token；供应商自己的 Token 走 `Authorization`，由网关改名透传，不受此项影响 |
| 上游返回 401 | 网关未把客户端 `Authorization` 改名为 `X-Upstream-Authorization`（改写规则见架构文档 7.4），或上游 Token 失效 | 网关实现凭证改写规则；确认供应商 Token 有效 |
| 供应商自定义头未到达上游 | 网关实现了白名单式头部过滤，只透传枚举出的协议头 | 改为默认透传：仅排除逐跳头、`Host`、`Authorization`（见架构文档 7.3/7.4） |
| MCP 429 `TOO_MANY_REQUESTS` | 代理并发达到 `PROXY_MAX_CONCURRENCY` | 网关侧按用户维度收敛并发，或云端提高上限 |
| SSE 流中途断开 | 某一跳缓冲或短超时掐流 | 见下节"SSE 断流排查" |
| MCP 客户端 307 重定向后 404 | 上游尾斜杠重定向丢了公共前缀 | 云端设置 `PUBLIC_PATH_PREFIX` 并重启；确认网关透传 `Location` |

## SSE 断流排查

MCP streamable-http 的 SSE 长连接被掐断时，按链路逐跳定位（从前到后）：

1. **内部网关（ia-access-gateway）**：是否关闭了响应缓冲与压缩？读超时是否 ≥ 3600s？
   是否有"读超时后自动重试"把 POST 重发给了无状态上游？
2. **云端 Nginx `/v1/proxy/`**：`proxy_buffering off`、`gzip off`、`proxy_read_timeout 3600s`
   三项缺一不可；用 `docker compose exec nginx nginx -T` 核对生成配置。
3. **云端 Worker :8081**：`PROXY_MAX_STREAM_BYTES` 是否过小（超限会主动断流，
   Worker 日志有 `proxy stream exceeded the configured byte cap`）；
   `PROXY_MAX_CONCURRENCY` 是否打满（429 会直接拒绝）。

Worker 完成日志按 `request_id` 关联：`proxy completed` 一条含 upstream_host、
status、bytes、elapsed_ms。Nginx 访问日志的 `mcp_upstream` 字段可直接按上游域名区分流量。

## 容器无法启动

检查运行时文件：

```bash
find var -maxdepth 2 -type f -ls
./scripts/generate-mtls.sh inspect
docker compose --env-file .env -f compose.yaml config --quiet
```

常见问题是没有先执行 `cloudctl.sh init`、文件权限错误，或者把旧版 `certs/` 路径与新版 `var/pki/` 混用。

## Nginx 模板问题

```bash
docker compose --env-file .env -f compose.yaml exec nginx nginx -T
```

确认生成配置中：

- `server_name` 是云公网 IP；
- `allow` 是内部固定出口 CIDR；
- `ssl_verify_client on`；
- upstream 为 `fetch-worker:8080`。

## Worker 无法访问公网

```bash
docker compose --env-file .env -f compose.yaml exec fetch-worker wget -qO- https://www.baidu.com/
```

注意最终 Worker 镜像中的工具只用于诊断，正式抓取仍通过 API。若 IP可访问但域名失败，重点检查 DNS；二者都失败则检查云出站策略和 Docker 网络。

## 查看证书细节

```bash
openssl verify -CAfile var/pki/ca.crt var/pki/server.crt var/pki/client.crt
openssl x509 -in var/pki/server.crt -noout -subject -issuer -dates -ext subjectAltName
openssl x509 -in var/pki/client.crt -noout -subject -issuer -dates -ext extendedKeyUsage
```

## 仍无法定位

保留以下信息再升级处理：

- 脱敏后的 `.env`，删除或掩盖敏感值；
- `cloudctl.sh preflight` 输出；
- `cloudctl.sh status` 输出；
- 对应时间段 Nginx/Worker 日志；
- 请求 ID、HTTP 状态和错误码；
- 云安全组及内部防火墙规则截图；
- 服务端证书的 SAN 和有效期。

不要提交私钥、Bearer Token 或完整响应正文。
