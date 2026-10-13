#!/usr/bin/env python3
"""经网关模拟器对真实 MCP 服务器做 streamable-http 端到端测试。

链路：本脚本(扮演生产服务器上的 MCP 客户端)
        → gateway_sim.py (http://127.0.0.1:8088/ia-access-gateway/mcp/)
        → 云端 web-fetch-proxy (/v1/proxy/, mTLS + Bearer + allowlist)
        → https://mcp-server.comein.cn/mcp-servers/institutioncenter

执行 initialize → notifications/initialized → tools/list → SSE GET → DELETE，
核心步骤(1-3)失败则退出码非 0；GET SSE 与 DELETE 按规范允许 405(服务器不支持)。
"""

import http.client
import json
import os
import sys
import time

GATEWAY_HOST = os.environ.get("GATEWAY_HOST", "127.0.0.1")
GATEWAY_PORT = int(os.environ.get("GATEWAY_PORT", "8088"))
GATEWAY_PREFIX = os.environ.get("GATEWAY_PREFIX", "/ia-access-gateway/mcp")
MCP_PATH = os.environ.get("MCP_PATH", "/mcp-servers/institutioncenter")
X_MCP_HOST = os.environ.get("X_MCP_HOST", "https://mcp-server.comein.cn")
X_USER_ID = os.environ.get("X_USER_ID", "748112da-0365-4807-b8fe-44ee3498f7b9")

BASE_HEADERS = {
    "x-mcp-host": X_MCP_HOST,
    "x-user-id": X_USER_ID,
}


def request(method, path, payload=None, session_id=None, protocol_version=None,
            accept="application/json, text/event-stream", timeout=30):
    headers = dict(BASE_HEADERS)
    headers["Accept"] = accept
    if session_id:
        headers["Mcp-Session-Id"] = session_id
    if protocol_version:
        headers["MCP-Protocol-Version"] = protocol_version
    body = None
    if payload is not None:
        body = json.dumps(payload)
        headers["Content-Type"] = "application/json"
    conn = http.client.HTTPConnection(GATEWAY_HOST, GATEWAY_PORT, timeout=timeout)
    try:
        conn.request(method, GATEWAY_PREFIX + path, body=body, headers=headers)
        response = conn.getresponse()
        data = response.read()
        return response.status, dict(response.getheaders()), data
    finally:
        conn.close()


def parse_mcp_message(data, content_type):
    """streamable-http 的 POST 响应可能是 JSON 或 SSE，两种都解析。"""
    text = data.decode("utf-8", errors="replace")
    if "text/event-stream" in (content_type or ""):
        for line in text.splitlines():
            if line.startswith("data:"):
                payload = line[len("data:"):].strip()
                if payload:
                    return json.loads(payload)
        return None
    if text.strip():
        return json.loads(text)
    return None


def header(headers, name):
    for key, value in headers.items():
        if key.lower() == name.lower():
            return value
    return ""


def main():
    failures = []

    print("=== [1/5] initialize ===")
    initialize_params = {
        "protocolVersion": "2025-06-18",
        "capabilities": {},
        "clientInfo": {"name": "web-fetch-proxy-e2e", "version": "1.0"},
    }
    status, headers, data = request("POST", MCP_PATH, {
        "jsonrpc": "2.0", "id": 1, "method": "initialize",
        "params": initialize_params,
    })
    print("HTTP %d, Content-Type: %s" % (status, header(headers, "Content-Type")))
    if status != 200:
        failures.append("initialize returned %d" % status)
        print(data.decode("utf-8", errors="replace")[:2000])
        summarize(failures)
        return
    message = parse_mcp_message(data, header(headers, "Content-Type"))
    session_id = header(headers, "Mcp-Session-Id")
    protocol_version = (message or {}).get("result", {}).get("protocolVersion", "")
    server_info = (message or {}).get("result", {}).get("serverInfo", {})
    print("Mcp-Session-Id: %s" % (session_id or "(none)"))
    print("protocolVersion: %s" % protocol_version)
    print("serverInfo: %s" % json.dumps(server_info, ensure_ascii=False))
    if not session_id:
        print("server is stateless (no session id issued) - allowed by the MCP spec")
    if not server_info:
        failures.append("no serverInfo in initialize response")

    print("\n=== [2/5] notifications/initialized ===")
    status, headers, data = request("POST", MCP_PATH, {
        "jsonrpc": "2.0", "method": "notifications/initialized",
    }, session_id=session_id, protocol_version=protocol_version)
    print("HTTP %d, body: %s" % (status, data.decode("utf-8", errors="replace")[:200] or "(empty)"))
    if status not in (200, 202):
        failures.append("notifications/initialized returned %d" % status)

    print("\n=== [3/5] tools/list ===")
    status, headers, data = request("POST", MCP_PATH, {
        "jsonrpc": "2.0", "id": 2, "method": "tools/list",
    }, session_id=session_id, protocol_version=protocol_version)
    print("HTTP %d, Content-Type: %s" % (status, header(headers, "Content-Type")))
    if status != 200:
        failures.append("tools/list returned %d" % status)
        print(data.decode("utf-8", errors="replace")[:2000])
    else:
        message = parse_mcp_message(data, header(headers, "Content-Type"))
        tools = (message or {}).get("result", {}).get("tools", [])
        print("tools (%d): %s" % (len(tools), ", ".join(
            tool.get("name", "?") for tool in tools)))
        if not tools:
            failures.append("tools/list returned no tools")

    print("\n=== [4/5] GET SSE stream (server push, may be 405) ===")
    try:
        status, headers, data = request("GET", MCP_PATH, session_id=session_id,
                                        protocol_version=protocol_version,
                                        accept="text/event-stream", timeout=15)
        content_type = header(headers, "Content-Type")
        print("HTTP %d, Content-Type: %s" % (status, content_type))
        if status == 405:
            print("server does not offer server-initiated streams (allowed by spec)")
        elif status == 200 and "text/event-stream" in content_type:
            text = data.decode("utf-8", errors="replace")
            events = [line for line in text.splitlines() if line.startswith(("event:", "data:"))]
            print("received %d stream lines, first: %s" % (len(events), events[0] if events else "(none)"))
        else:
            print(data.decode("utf-8", errors="replace")[:500])
    except Exception as error:  # noqa: BLE001 - 超时/断流按信息性输出
        print("stream ended: %s" % error)

    print("\n=== [5/5] DELETE session ===")
    if session_id:
        status, headers, data = request("DELETE", MCP_PATH, session_id=session_id,
                                        protocol_version=protocol_version)
        print("HTTP %d, body: %s" % (status, data.decode("utf-8", errors="replace")[:200] or "(empty)"))
        if status not in (200, 202, 405):
            failures.append("DELETE returned %d" % status)
    else:
        print("skipped (stateless server, no session to terminate)")

    summarize(failures)


def summarize(failures):
    print("\n" + "=" * 50)
    if failures:
        print("FAILED:")
        for item in failures:
            print("  - %s" % item)
        sys.exit(1)
    print("MCP end-to-end test PASSED "
          "(client -> gateway sim -> web-fetch-proxy -> %s)" % X_MCP_HOST)


if __name__ == "__main__":
    main()
