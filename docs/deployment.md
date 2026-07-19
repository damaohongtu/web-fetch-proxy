# 公网 IP + mTLS 部署手册

本文描述如何在一台全新的云服务器上部署 Nginx 和 Fetch Worker。所有命令均从项目根目录执行。

## 1. 部署参数示例

以下地址仅用于文档，请替换：

| 参数 | 示例 | 如何获得 |
|---|---|---|
| 云服务器公网 IPv4 | `203.0.113.20` | 云控制台的公网地址 |
| 内部服务器固定出口 IPv4 | `198.51.100.10` | 内部服务器访问公网时对外呈现的 NAT 地址 |
| 内部来源 CIDR | `198.51.100.10/32` | 单台固定出口使用 `/32` |
| 公网服务端口 | `443/tcp` | 固定使用 HTTPS 端口 |

这里不需要域名。证书脚本会将云服务器公网 IP 写入服务端证书的 `subjectAltName = IP:<公网IP>`。

## 2. 云服务器要求

- Linux x86_64 或 arm64；推荐当前受支持的 Ubuntu/Debian LTS。
- Docker Engine 和较新的 Docker Compose v2 Plugin；`docker compose up --help` 必须包含 `--wait`。
- OpenSSL 1.1.1 或 3.x、curl、bash。
- 系统时间准确，启用 NTP；时间偏差会造成 TLS 证书校验失败。
- 443 端口没有被宿主机上的其他 Nginx、Apache 或容器占用。

检查：

```bash
docker version
docker compose version
openssl version
curl --version
sudo ss -lntp | grep ':443 ' || true
```

如果 Docker 尚未安装，请使用操作系统或 Docker 官方针对该发行版的安装流程；不要使用来源不明的一键脚本。安装后启用守护进程：

```bash
sudo systemctl enable --now docker
```

## 3. 网络和防火墙

### 云安全组入站

| 协议 | 端口 | 来源 | 用途 |
|---|---:|---|---|
| TCP | 443 | `198.51.100.10/32` | 内部 Web-Fetch 调用 |
| TCP | 22 | 运维网络 CIDR | SSH，仅在确有需要时开放 |

不要把 TCP 443 来源配置成 `0.0.0.0/0`。Nginx 还会通过 `ALLOWED_CLIENT_CIDR` 做第二次来源校验。

### 云服务器出站

Fetch Worker 至少需要：

- 到可信 DNS 解析器的 UDP/TCP 53；
- 到互联网目标的 TCP 80、443；
- 如系统时间服务需要，到 NTP 服务的 UDP 123。

### 内部防火墙

只需放行：

```text
内部 Web-Fetch 服务器 -> 203.0.113.20:443/TCP
```

## 4. 获取项目并检查目录

将项目通过组织批准的 Git、制品库或文件传输方式放到云服务器，例如 `/opt/web-fetch-proxy`：

```bash
cd /opt/web-fetch-proxy
find . -maxdepth 2 -type f | sort
```

根目录必须存在：

```text
compose.yaml
scripts/cloudctl.sh
scripts/generate-mtls.sh
services/fetch-worker/
deploy/nginx/
```

## 5. 初始化配置与 mTLS

初始化命令需要两个参数：云服务器公网 IP 和内部服务器出口 CIDR。

```bash
./scripts/cloudctl.sh init 203.0.113.20 198.51.100.10/32
```

`init` 只做初始化，不启动任何容器：

1. 创建 `.env`；
2. 创建私有 CA；
3. 签发包含公网 IP SAN 的服务端证书；
4. 签发内部 Web-Fetch 客户端证书；
5. 生成 256 位随机 Bearer Token；
6. 将私钥权限设置为 `0600`；Linux 上 Token 设置为容器固定 UID `10001` 专用的 `0400`。

它不会覆盖已有 `.env`、证书或 Token。证书和密钥位置：

```text
var/pki/ca.crt
var/pki/ca.key
var/pki/server.crt
var/pki/server.key
var/pki/client.crt
var/pki/client.key
var/secrets/fetch_auth_token
```

检查证书：

```bash
./scripts/generate-mtls.sh inspect
openssl x509 -in var/pki/server.crt -noout -checkip 203.0.113.20
```

预期包含：

```text
IP Address:203.0.113.20
TLS Web Server Authentication
TLS Web Client Authentication
```

## 6. 审核配置

初始化生成的 `.env` 权限为 `0600`。至少检查：

```env
PUBLIC_HTTPS_PORT=443
PUBLIC_BIND_ADDRESS=0.0.0.0
SERVER_NAME=203.0.113.20
ALLOWED_CLIENT_CIDR=198.51.100.10/32
BLOCKED_CIDRS=203.0.113.20/32
```

含义：

- `SERVER_NAME`：客户端实际访问的云公网 IPv4，也是证书 IP SAN。
- `ALLOWED_CLIENT_CIDR`：Nginx 允许的内部出口地址。
- `BLOCKED_CIDRS`：Worker 禁止访问的额外地址，必须包含云服务器自身公网 IP。
- `PUBLIC_BIND_ADDRESS`：生产使用 `0.0.0.0`，本地开发可用 `127.0.0.1`。
- `PUBLIC_HTTPS_PORT`：宿主机发布端口，生产固定为 443。
- `DEFAULT_*`、`MAX_*`：请求默认值和服务端硬上限。
- `*_MEMORY_LIMIT`、`*_CPU_LIMIT`：容器资源上限。

执行预检：

```bash
./scripts/cloudctl.sh preflight
```

预检覆盖 Docker、Compose、配置、证书链、证书与私钥匹配、有效期、IP SAN、Token 长度和 Compose 语法。

## 7. 启动 Nginx 和 Fetch Worker

### 7.1 准备容器镜像

`start` 不会强制更新已经存在的镜像。若本机尚无镜像，Compose 会尝试从镜像仓库拉取 Nginx，并按 `services/fetch-worker/Dockerfile` 构建 Worker。镜像仓库可达时可直接进入下一节。

如果云服务器不能访问 Docker Hub，请在受信任、与云服务器同为 `linux/amd64` 的构建机上离线打包：

```bash
docker pull --platform linux/amd64 nginx:1.28-alpine
docker buildx build --platform linux/amd64 \
  -t web-fetch-proxy-fetch-worker:latest \
  --load services/fetch-worker
docker save nginx:1.28-alpine web-fetch-proxy-fetch-worker:latest \
  | gzip > web-fetch-proxy-images-linux-amd64.tar.gz
```

通过批准的传输通道把压缩包送到云服务器，然后导入：

```bash
gzip -dc web-fetch-proxy-images-linux-amd64.tar.gz | docker load
docker image inspect nginx:1.28-alpine \
  web-fetch-proxy-fetch-worker:latest \
  --format '{{.RepoTags}} architecture={{.Architecture}}'
```

两项都应显示 `architecture=amd64`。确认导入成功后再删除传输压缩包。

### 7.2 启动服务

执行：

```bash
./scripts/cloudctl.sh start
```

启动脚本会明确执行以下等价命令，并等待两个服务都健康：

```bash
docker compose --env-file .env -f compose.yaml \
  up -d --wait --wait-timeout 90 fetch-worker nginx
```

这条命令同时启动：

1. `fetch-worker`：云端出站请求服务；
2. `nginx`：公网 443 mTLS 网关。

检查状态：

```bash
./scripts/cloudctl.sh status
docker compose --env-file .env -f compose.yaml ps
sudo ss -lntp | grep ':443 '
```

预期两个容器都是 `running (healthy)`，宿主机发布 443，但不发布 8080。

## 8. 导出并分发内部客户端文件

在云服务器上导出最小客户端包：

```bash
./scripts/cloudctl.sh export-client /tmp/web-fetch-client
```

包中只有：

```text
ca.crt
client.crt
client.key
fetch_auth_token
client.env
internal_web_fetch.py
smoke-test.sh
```

明确不包含 `ca.key`、`server.key`。客户端包包含敏感凭证，必须使用组织批准的加密传输渠道；传输完成后删除云端临时包。

内部服务器文件建议权限：

```bash
chmod 700 web-fetch-client
chmod 600 web-fetch-client/client.key web-fetch-client/fetch_auth_token web-fetch-client/client.env
```

## 9. 从内部服务器验收

进入客户端包目录并读取 endpoint：

```bash
cd web-fetch-client
set -a
. ./client.env
set +a
```

### 验证 mTLS 健康检查

```bash
curl --cacert ca.crt \
  --cert client.crt \
  --key client.key \
  "${CLOUD_FETCH_ENDPOINT%/v1/fetch}/healthz"
```

预期：

```json
{"status":"ok"}
```

### 单次外网抓取

```bash
python3 internal_web_fetch.py \
  --endpoint "${CLOUD_FETCH_ENDPOINT}" \
  fetch https://example.com
```

### 启动模拟内部 Web-Fetch

```bash
python3 internal_web_fetch.py \
  --endpoint "${CLOUD_FETCH_ENDPOINT}" \
  serve --listen 127.0.0.1 --port 8090
```

另一个终端调用：

```bash
curl -sS http://127.0.0.1:8090/web-fetch \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com"}'
```

Python 示例只使用标准库，不需要安装代理工具或第三方 Python 包。

## 10. 完整验收矩阵

在内部服务器的客户端包目录执行：

```bash
./smoke-test.sh
```

该脚本验证：

1. 没有客户端证书时被拒绝；
2. 正确 mTLS 可以经 Nginx 到达 Worker；
3. 错误 Token 返回 401；
4. 访问 `127.0.0.1` 的 SSRF 请求返回 403；
5. 合法公网 HTTPS 抓取返回 200。

`cloudctl.sh verify` 也会调用同一验收脚本，但它只适用于本地测试，或云服务器自身公网回环流量也落在 `ALLOWED_CLIENT_CIDR` 的环境。生产验收以内部服务器执行客户端包中的 `smoke-test.sh` 为准；云端容器状态通过 `cloudctl.sh status` 检查。

- curl脚本
```shell
cd /Users/mao/workspace/web-fetch-proxy/var/client-bundle

curl --cacert ca.crt \
  --cert client.crt \
  --key client.key \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $(tr -d '\r\n' < fetch_auth_token)" \
  -d '{"url":"https://www.baidu.com/","max_bytes":1048576}' \
  https://8.134.83.141/v1/fetch
```



## 11. 开机自启

Compose 服务使用 `restart: unless-stopped`。只要 Docker 守护进程已设置为开机启动，机器重启后容器会自动恢复：

```bash
sudo systemctl is-enabled docker
docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' web-fetch-proxy-nginx-1
docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' web-fetch-proxy-fetch-worker-1
```

日常升级、备份、证书与 Token 轮换见 [运维手册](operations.md)，异常见 [故障排查](troubleshooting.md)。
