# 运维手册

所有命令默认在项目根目录执行。

## 服务生命周期

```bash
./scripts/cloudctl.sh status
./scripts/cloudctl.sh logs nginx
./scripts/cloudctl.sh logs fetch-worker
./scripts/cloudctl.sh restart
./scripts/cloudctl.sh rebuild
./scripts/cloudctl.sh stop
./scripts/cloudctl.sh start
```

`start` 和 `restart` 都明确管理 `fetch-worker` 与 `nginx`。Worker 收到 SIGTERM 时最多等待 15 秒完成在途请求，Compose 为其保留 20 秒停止宽限期。

`rebuild` 会访问镜像仓库以更新 Nginx 和 Worker 的基础镜像。受限网络中不要把它当作日常重启命令；先按[部署手册的离线镜像流程](deployment.md#71-准备容器镜像)导入经过批准的新镜像，再执行 `restart`。现有镜像未被删除时，`start` 和 `restart` 不需要访问 Docker Hub。

日志有两条保留路径：

- Docker stdout/stderr 仍限制为每文件 10 MiB、保留 3 个文件，供 `cloudctl.sh logs` 即时排障；
- 宿主机持久日志位于 `var/log/nginx/` 和 `var/log/fetch-worker/`，供审计、采集和按天保留。

访问日志不记录 Token 或响应正文。Worker 同时写 stdout 与宿主机文件，内容为 JSON；Nginx 访问日志为 JSON，错误日志为文本。

## 日志轮转与 7 天保留

宿主机定时器每天执行：

```bash
./scripts/cleanup-logs.sh 7
```

脚本行为：

1. 将 Nginx 和 Worker 活动日志移动为带 UTC 时间戳的归档；
2. 重新创建 Nginx 和 Worker，使所有进程关闭旧文件并打开新的活动日志；该步骤会造成短暂服务中断；
3. 使用 gzip 压缩本次归档；
4. 删除修改时间超过 7×24 小时且名称匹配 `*.log.*` 的归档；
5. 使用目录锁阻止两个清理进程并发执行。

检查任务和最近执行结果：

```bash
systemctl status web-fetch-proxy-log-cleanup.timer
systemctl list-timers web-fetch-proxy-log-cleanup.timer
journalctl -u web-fetch-proxy-log-cleanup.service --since '7 days ago'
find var/log -maxdepth 2 -type f -ls
```

修改保留期时，编辑 systemd service 的 `ExecStart` 参数并执行 `systemctl daemon-reload`。不要对整个 `var/` 目录使用递归删除命令；其中还包含 CA 私钥、服务端私钥和 Token。

## 修改配置

1. 备份 `.env`；
2. 编辑配置；
3. 执行预检；
4. 重建两个容器使环境变量生效。

```bash
cp -p .env .env.backup
./scripts/cloudctl.sh preflight
./scripts/cloudctl.sh restart
```

扩大超时、响应大小或并发前，需要同时评估 Worker 内存、Nginx 超时和外网带宽。

### 启用/调整 MCP 代理

MCP 代理默认关闭（fail-closed）。启用或调整上游范围：

```bash
# .env 中设置（逗号分隔；非标准端口用 host:port）
ALLOWED_UPSTREAM_HOSTS=mcp-server.comein.cn
# 网关使用公共前缀时同步设置，用于改写上游 3xx Location
PUBLIC_PATH_PREFIX=/ia-access-gateway/mcp
./scripts/cloudctl.sh restart
./scripts/cloudctl.sh verify      # [6/8]-[8/8] 为 MCP 代理验收
```

新增上游域名后无需改代码，重启即生效；移除域名即立即拒绝该上游（403
`UPSTREAM_NOT_ALLOWED`）。验收矩阵见 [部署手册第 10 节](deployment.md#10-完整验收矩阵)，
本地联调可用 `examples/ia-access-gateway/` 的网关模拟器。

## 应用升级与回滚

升级前记录当前版本和镜像：

```bash
git rev-parse HEAD
docker compose --env-file .env -f compose.yaml images
```

升级：

```bash
git fetch --all --tags
git checkout <经过批准的版本>
./scripts/cloudctl.sh preflight
./scripts/cloudctl.sh rebuild
./scripts/cloudctl.sh verify
```

回滚：切回此前记录的 Git 版本，再执行 `preflight`、`restart` 和 `verify`。不要在没有备份 `.env` 和 `var/` 的情况下升级。

## 备份

必须备份：

```text
.env
var/pki/ca.crt
var/pki/ca.key
var/pki/server.crt
var/pki/server.key
var/pki/client.crt
var/pki/client.key
var/secrets/fetch_auth_token
```

备份应加密并存入受控密钥库或备份系统。`ca.key` 泄露意味着攻击者可以签发任意受信客户端证书；条件允许时应离线保管 CA 私钥。

恢复后执行：

```bash
chmod 600 .env var/pki/*.key
chmod 644 var/pki/*.crt
chown 10001:10001 var/secrets/fetch_auth_token
chmod 400 var/secrets/fetch_auth_token
./scripts/cloudctl.sh preflight
./scripts/cloudctl.sh start
```

## 检查证书到期时间

```bash
./scripts/generate-mtls.sh inspect
openssl x509 -checkend 2592000 -noout -in var/pki/server.crt
openssl x509 -checkend 2592000 -noout -in var/pki/client.crt
```

`2592000` 表示 30 天。应在监控系统中定期执行并提前告警。

## 轮换服务端证书

云公网 IP不变时：

```bash
./scripts/generate-mtls.sh server 203.0.113.20
./scripts/cloudctl.sh restart
./scripts/cloudctl.sh verify
```

公网 IP变化时，同时修改 `.env` 中的 `SERVER_NAME`、`BLOCKED_CIDRS`，修改安全组，再签发包含新 IP SAN 的服务端证书。

## 轮换客户端证书

```bash
./scripts/generate-mtls.sh client internal-web-fetch
./scripts/cloudctl.sh export-client /tmp/web-fetch-client-new
```

当前方案由同一个私有 CA 直接信任客户端证书。签发新证书并不会自动吊销旧证书；若旧客户端凭证疑似泄露，应同时轮换 Bearer Token，或者替换 CA/配置 CRL，才能立即使旧身份失效。

## 轮换 Bearer Token

Token 在 Worker 启动时读取，轮换需要短暂协调：

1. 安排维护窗口；
2. 生成新 Token；
3. 安全同步到内部服务器；
4. 重建 Worker；
5. 切换内部 Web-Fetch；
6. 验证 401/200 行为。

```bash
./scripts/generate-mtls.sh token
./scripts/cloudctl.sh restart
```

不要把 Token 放进命令历史、工单正文或日志。

## 全量 CA 轮换

CA 轮换会同时影响服务端和所有客户端，属于计划性中断操作。先备份旧 PKI，生成一套新 PKI，通过安全渠道分发新 `ca.crt`、客户端证书和私钥，再在双方约定窗口切换。不要把 `generate-mtls.sh init` 当作日常轮换命令。

## 停用和删除

停止容器：

```bash
./scripts/cloudctl.sh stop
```

删除 PKI 或备份属于不可恢复操作。只有在确认服务退役、客户端已撤销且备份策略允许后，才应按组织的数据销毁流程处理 `var/pki` 和 `var/secrets`。
