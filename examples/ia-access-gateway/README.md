# ia-access-gateway 网关模拟器

模拟企业内部网关（`https://ia-api.cmfcha.com/ia-access-gateway/mcp/...`），在本地完整复现
生产服务器访问外部 MCP 服务器的链路，用于上线前联调与验收：

```
MCP 客户端（扮演生产服务器）                examples/ia-access-gateway/mcp_client_test.py
  ↓ http://127.0.0.1:8088/ia-access-gateway/mcp/<path>
网关模拟器（剥前缀、注入 Bearer + mTLS）     examples/ia-access-gateway/gateway_sim.py
  ↓ https://127.0.0.1:8443/v1/proxy/<path>
云端 web-fetch-proxy（mTLS + Bearer + allowlist + SSRF 防护）
  ↓ https://mcp-server.comein.cn/<path>
真实 MCP 上游（由 x-mcp-host 头指定）
```

## 文件

| 文件 | 作用 |
|---|---|
| `gateway_sim.py` | 网关模拟器，Python 3 标准库，无第三方依赖 |
| `mcp_client_test.py` | MCP streamable-http 端到端测试客户端 |

## 运行

前置条件：本地栈已启动（`./scripts/cloudctl.sh start`），且 `.env` 中的
`ALLOWED_UPSTREAM_HOSTS` 包含目标 MCP 域名，例如：

```
ALLOWED_UPSTREAM_HOSTS=mcp-server.comein.cn,www.baidu.com
```

```bash
# 终端 1：启动网关模拟器（默认监听 127.0.0.1:8088，转发到本机 8443）
python3 examples/ia-access-gateway/gateway_sim.py

# 终端 2：对真实 MCP 服务器跑 initialize → tools/list → SSE → DELETE 全流程
python3 examples/ia-access-gateway/mcp_client_test.py
```

环境变量可覆盖默认值：`CLOUD_HOST`/`CLOUD_PORT`（云端地址）、`GATEWAY_LISTEN_HOST`/
`GATEWAY_LISTEN_PORT`（模拟器监听）、`X_MCP_HOST`/`X_USER_ID`/`MCP_PATH`（测试目标）。

## 模拟器实现的网关职责

与真实 ia-access-gateway 需要满足的行为一一对应（详见 `docs/architecture.md` 网关配置清单）：

1. **前缀改写**：`/ia-access-gateway/mcp/<path>` → 云端 `/v1/proxy/<path>`
2. **凭证改写**：客户端 `Authorization`（上游凭证）改名为 `X-Upstream-Authorization`；
   注入云端 Bearer Token 与 mTLS 客户端证书（网关是证书持有方）
3. **协议头透传**：`x-mcp-host`、`x-user-id`、`X-Upstream-Authorization`、
   `Mcp-Session-Id`、`MCP-Protocol-Version`、`Last-Event-ID`、`Accept`、`Content-Type`
4. **流式转发**：响应逐块转发、每块即刷——SSE 不缓冲
5. **方法白名单**：仅 GET/POST/DELETE（streamable-http 传输所需）
6. **不缓存、不压缩、不重试、不拦截错误页**

## 生产服务器上的 MCP 客户端配置

```json
{
  "institutioncenter": {
    "url": "http://127.0.0.1:8088/ia-access-gateway/mcp/mcp-servers/institutioncenter",
    "type": "streamable-http",
    "headers": {
      "x-user-id": "748112da-0365-4807-b8fe-44ee3498f7b9",
      "x-mcp-host": "https://mcp-server.comein.cn"
    }
  }
}
```

生产环境中把 `127.0.0.1:8088` 换成 `https://ia-api.cmfcha.com`。`x-mcp-host` 指定真正
代理的外部 MCP 服务器；`x-user-id` 透传给上游做业务鉴权。

**上游自带鉴权时**（如 gildata 的 Bearer Token）：供应商配置里的 `Authorization`
**原样保留**，不需要改头名。网关负责凭证改写：

```
客户端 Authorization（上游凭证） ──网关改名──▶ X-Upstream-Authorization ──云端映射──▶ 上游 Authorization
网关注入的云端 Bearer Token ────────────────▶ Authorization（云端代理自己的鉴权）
```

```json
{
  "aidata-assistant-srv-data": {
    "url": "http://127.0.0.1:8088/ia-access-gateway/mcp/mcp-servers/aidata-assistant-srv-data",
    "type": "streamable-http",
    "headers": {
      "Authorization": "Bearer <上游自己的Token>",
      "x-mcp-host": "https://api.gildata.com"
    }
  }
}
```

与直连配置的唯一差别是 `url` 换成网关地址、追加 `x-mcp-host`。客户端显式提供
`X-Upstream-Authorization` 时以它为准（优先级高于改名，一般不需要用）。

## 测试结果记录（2026-10-13，本仓库本地验证）

对 `https://mcp-server.comein.cn/mcp-servers/institutioncenter`：

- `initialize` → 200，`serverInfo: institutioncenter 1.0.0`，协商 `protocolVersion 2025-06-18`
- 服务器为无状态实现（不签发 `Mcp-Session-Id`，符合 MCP 规范）
- `notifications/initialized` → 202
- `tools/list` → 200，66 个工具
- `tools/call`（`internet_search`）→ 返回真实搜索结果
- GET SSE → 405（该服务器不提供服务端主动推送流，规范允许）
