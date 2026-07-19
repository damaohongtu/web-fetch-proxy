# Web Fetch Proxy

面向受限内网的安全 Web Fetch 出口。云端由 Nginx mTLS 网关和 Go Fetch Worker 组成：

```text
Agent -> 内部 Web-Fetch -> HTTPS/mTLS -> Nginx -> Fetch Worker -> 外部网站
```

详细设计、参数和部署步骤见 [docs/architecture.md](docs/architecture.md) 与 [docs/deployment.md](docs/deployment.md)。

不要求拥有域名。使用云服务器公网 IP 时，执行 `./scripts/generate-certs.sh <公网IP>`，脚本会把 IP 写入服务端证书的 IP SAN，内部服务通过 `https://<公网IP>:443/v1/fetch` 调用。

## 快速验证

```bash
cp .env.example .env
./scripts/generate-certs.sh localhost
docker compose up --build -d

python3 tools/internal_web_fetch.py fetch https://example.com
```

启动模拟的内部 Web-Fetch 服务：

```bash
python3 tools/internal_web_fetch.py serve --listen 127.0.0.1 --port 8090
curl -sS http://127.0.0.1:8090/web-fetch \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com"}'
```

停止服务：

```bash
docker compose down
```

## 目录

```text
nginx/                 Nginx mTLS 网关配置
worker/                Go Fetch Worker
scripts/               证书与密钥生成工具
tools/                 内部 Web-Fetch 测试服务
docs/                  架构与部署文档
docker-compose.yml     云端容器编排
```
