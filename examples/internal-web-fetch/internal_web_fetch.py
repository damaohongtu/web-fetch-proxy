#!/usr/bin/env python3
"""Example internal Web-Fetch service and cloud proxy smoke-test client.

Only Python's standard library is required.
"""

from __future__ import annotations

import argparse
import json
import os
import ssl
import sys
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.request import HTTPSHandler, ProxyHandler, Request, build_opener


SCRIPT_DIR = Path(__file__).resolve().parent
PROJECT_DIR = SCRIPT_DIR.parent.parent


def default_artifact(name: str, repository_path: Path) -> str:
    adjacent = SCRIPT_DIR / name
    return str(adjacent if adjacent.exists() else repository_path)


class CloudFetchClient:
    def __init__(
        self,
        endpoint: str,
        ca_file: str,
        cert_file: str,
        key_file: str,
        token: str,
        timeout: float = 75.0,
    ) -> None:
        self.endpoint = endpoint
        self.token = token
        self.timeout = timeout
        self.ssl_context = ssl.create_default_context(cafile=ca_file)
        self.ssl_context.minimum_version = ssl.TLSVersion.TLSv1_2
        self.ssl_context.load_cert_chain(certfile=cert_file, keyfile=key_file)
        # 云端地址必须直连；不要继承操作系统或环境中的其他代理配置。
        self.opener = build_opener(ProxyHandler({}), HTTPSHandler(context=self.ssl_context))

    def fetch(self, payload: dict[str, Any]) -> tuple[int, dict[str, Any]]:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        request = Request(
            self.endpoint,
            data=body,
            method="POST",
            headers={
                "Content-Type": "application/json",
                "Authorization": f"Bearer {self.token}",
                "X-Request-ID": str(uuid.uuid4()),
                "User-Agent": "InternalWebFetchTest/1.0",
            },
        )
        try:
            with self.opener.open(request, timeout=self.timeout) as response:
                return response.status, self._decode(response.read())
        except HTTPError as exc:
            return exc.code, self._decode(exc.read())
        except (URLError, TimeoutError, ssl.SSLError) as exc:
            return 502, {
                "error": {
                    "code": "CLOUD_PROXY_UNAVAILABLE",
                    "message": str(exc),
                }
            }

    @staticmethod
    def _decode(data: bytes) -> dict[str, Any]:
        try:
            value = json.loads(data.decode("utf-8"))
            if isinstance(value, dict):
                return value
            raise ValueError("response is not a JSON object")
        except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
            return {
                "error": {
                    "code": "INVALID_CLOUD_RESPONSE",
                    "message": str(exc),
                }
            }


def make_handler(client: CloudFetchClient) -> type[BaseHTTPRequestHandler]:
    class WebFetchHandler(BaseHTTPRequestHandler):
        server_version = "InternalWebFetchTest/1.0"

        def do_GET(self) -> None:  # noqa: N802
            if self.path == "/healthz":
                self._write_json(200, {"status": "ok"})
            else:
                self._write_json(404, {"error": {"code": "NOT_FOUND", "message": "not found"}})

        def do_POST(self) -> None:  # noqa: N802
            if self.path != "/web-fetch":
                self._write_json(404, {"error": {"code": "NOT_FOUND", "message": "not found"}})
                return
            try:
                length = int(self.headers.get("Content-Length", "0"))
            except ValueError:
                length = 0
            if length <= 0 or length > 65536:
                self._write_json(400, {"error": {"code": "INVALID_REQUEST", "message": "body must be between 1 and 65536 bytes"}})
                return
            try:
                payload = json.loads(self.rfile.read(length).decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                self._write_json(400, {"error": {"code": "INVALID_REQUEST", "message": str(exc)}})
                return
            if not isinstance(payload, dict) or not isinstance(payload.get("url"), str):
                self._write_json(400, {"error": {"code": "INVALID_REQUEST", "message": "url is required"}})
                return
            status, result = client.fetch(payload)
            self._write_json(status, result)

        def _write_json(self, status: int, value: dict[str, Any]) -> None:
            body = json.dumps(value, ensure_ascii=False).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, format_string: str, *args: object) -> None:
            sys.stderr.write("internal-web-fetch: " + format_string % args + "\n")

    return WebFetchHandler


def read_token(token: str | None, token_file: str) -> str:
    value = token or os.environ.get("CLOUD_FETCH_TOKEN", "")
    if value.strip():
        return value.strip()
    try:
        return Path(token_file).read_text(encoding="utf-8").strip()
    except OSError as exc:
        raise SystemExit(f"unable to read token file {token_file}: {exc}") from exc


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Test the cloud Web Fetch proxy through mTLS")
    parser.add_argument("--endpoint", default=os.environ.get("CLOUD_FETCH_ENDPOINT", "https://127.0.0.1:8443/v1/fetch"))
    parser.add_argument("--ca", default=os.environ.get("CLOUD_FETCH_CA", default_artifact("ca.crt", PROJECT_DIR / "var/pki/ca.crt")))
    parser.add_argument("--cert", default=os.environ.get("CLOUD_FETCH_CERT", default_artifact("client.crt", PROJECT_DIR / "var/pki/client.crt")))
    parser.add_argument("--key", default=os.environ.get("CLOUD_FETCH_KEY", default_artifact("client.key", PROJECT_DIR / "var/pki/client.key")))
    parser.add_argument("--token", default=None)
    parser.add_argument("--token-file", default=os.environ.get("CLOUD_FETCH_TOKEN_FILE", default_artifact("fetch_auth_token", PROJECT_DIR / "var/secrets/fetch_auth_token")))
    parser.add_argument("--timeout", type=float, default=75.0)

    commands = parser.add_subparsers(dest="command", required=True)
    fetch = commands.add_parser("fetch", help="perform one fetch and print its JSON result")
    fetch.add_argument("url")
    fetch.add_argument("--method", choices=("GET", "HEAD"), default="GET")
    fetch.add_argument("--max-bytes", type=int, default=None)
    fetch.add_argument("--request-timeout-ms", type=int, default=None)

    serve = commands.add_parser("serve", help="run a local /web-fetch test service")
    serve.add_argument("--listen", default="127.0.0.1")
    serve.add_argument("--port", type=int, default=8090)
    return parser


def main() -> int:
    args = build_parser().parse_args()
    token = read_token(args.token, args.token_file)
    client = CloudFetchClient(args.endpoint, args.ca, args.cert, args.key, token, args.timeout)

    if args.command == "fetch":
        payload: dict[str, Any] = {"url": args.url, "method": args.method}
        if args.max_bytes is not None:
            payload["max_bytes"] = args.max_bytes
        if args.request_timeout_ms is not None:
            payload["timeout_ms"] = args.request_timeout_ms
        status, result = client.fetch(payload)
        print(json.dumps(result, ensure_ascii=False, indent=2))
        return 0 if status == 200 else 1

    address = (args.listen, args.port)
    server = ThreadingHTTPServer(address, make_handler(client))
    print(f"internal Web-Fetch test service listening on http://{args.listen}:{args.port}/web-fetch")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
