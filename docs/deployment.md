# 部署和验证

## 1. 前置条件

云服务器需要 Docker Engine、Docker Compose Plugin 和 OpenSSL。安全组入站只开放：

```text
TCP 443，来源为内部服务器固定出口公网 IP/32
```

SSH 应使用独立的运维来源限制。

## 2. 生成 mTLS 证书和 Token

不需要域名。假设云服务器公网 IP 是 `203.0.113.20`，直接用该 IP 生成证书：

```bash
cp .env.example .env
./scripts/generate-certs.sh 203.0.113.20
```

脚本会把该地址写入服务端证书的 `subjectAltName = IP:203.0.113.20`，Python、curl 等客户端因此可以安全校验所连接的服务器 IP。

脚本生成：

```text
certs/ca.crt          CA 公钥，可分发给客户端
certs/server.crt      云端 Nginx 证书
certs/server.key      云端私钥，只保留在云端
certs/client.crt      内部 Web-Fetch 客户端证书
certs/client.key      客户端私钥，只分发到内部服务器
secrets/fetch_auth_token
```

不要把 `*.key`、Token 或整个 `certs`、`secrets` 目录提交到版本库。生产环境更推荐由企业 CA 签发，而不是使用示例脚本生成的本地 CA。

## 3. 启动云端服务

将 `.env` 中端口改为生产值：

```env
PUBLIC_HTTPS_PORT=443
SERVER_NAME=203.0.113.20

# 必须阻止 Fetch Worker 回连云服务器自身公网地址
BLOCKED_CIDRS=203.0.113.20/32
```

启动：

```bash
docker compose up --build -d
docker compose ps
docker compose logs --tail=100 nginx fetch-worker
```

更新服务：

```bash
docker compose build --pull
docker compose up -d
```

## 4. mTLS 验证

不提供客户端证书时，请求必须失败；Nginx 通常返回 `400 No required SSL certificate was sent`：

```bash
curl --cacert certs/ca.crt https://203.0.113.20/healthz
```

提供客户端证书后应返回：

```bash
curl --cacert certs/ca.crt \
  --cert certs/client.crt \
  --key certs/client.key \
  https://203.0.113.20/healthz
```

预期：

```json
{"status":"ok"}
```

## 5. 使用测试内部 Web-Fetch

把以下文件安全复制到内部服务器：

```text
tools/internal_web_fetch.py
certs/ca.crt
certs/client.crt
certs/client.key
secrets/fetch_auth_token
```

直接验证一次：

```bash
python3 tools/internal_web_fetch.py \
  --endpoint https://203.0.113.20/v1/fetch \
  --ca certs/ca.crt \
  --cert certs/client.crt \
  --key certs/client.key \
  --token-file secrets/fetch_auth_token \
  fetch https://example.com
```

或者启动内部模拟服务：

```bash
python3 tools/internal_web_fetch.py \
  --endpoint https://203.0.113.20/v1/fetch \
  --ca certs/ca.crt \
  --cert certs/client.crt \
  --key certs/client.key \
  --token-file secrets/fetch_auth_token \
  serve --listen 127.0.0.1 --port 8090
```

然后调用：

```bash
curl -sS http://127.0.0.1:8090/web-fetch \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com"}'
```

测试脚本仅使用 Python 标准库，不需要安装第三方依赖。

## 6. 生产检查清单

- 内部服务器能够访问云服务器公网 IP 的 TCP 443。
- 服务端证书的 IP SAN 与客户端访问的云服务器公网 IP 完全一致。
- 云安全组只允许内部出口 IP 访问 443。
- 云服务器没有发布 Worker 的 8080 端口。
- `BLOCKED_CIDRS` 已包含云服务器全部公网 IP 和业务要求禁止的额外网段。
- 客户端私钥权限为 `0600`。
- Token 不在环境变量、命令行、日志或镜像层中明文出现。
- 已验证无客户端证书、错误客户端证书、错误 Token 均无法调用。
- 已配置容器日志轮转和磁盘告警。
- 已测试超时、大响应、重定向和私网目标拦截。
