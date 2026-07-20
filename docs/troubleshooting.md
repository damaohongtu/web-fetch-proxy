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
