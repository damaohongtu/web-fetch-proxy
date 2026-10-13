// Package main tests the transparent MCP proxy route.
package main

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newLoopbackProxyServer builds a server whose transport skips the safe
// dialer, so tests can proxy to httptest servers listening on loopback.
func newLoopbackProxyServer(cfg config) *server {
	fetchTransport := &http.Transport{}
	return &server{
		cfg:            cfg,
		transport:      fetchTransport,
		proxyTransport: newProxyTransport(fetchTransport),
		sem:            make(chan struct{}, cfg.MaxConcurrency),
		proxySem:       make(chan struct{}, cfg.ProxyMaxConcurrency),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func authorizedProxyRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	return req
}

func TestParseUpstreamOrigin(t *testing.T) {
	tests := []struct {
		value   string
		wantErr string // empty means success
	}{
		{"https://mcp.example.com", ""},
		{"http://mcp.example.com", ""},
		{"mcp.example.com", ""},
		{"ftp://mcp.example.com", "only supports http and https"},
		{"https://user:pass@mcp.example.com", "credentials"},
		{"https://mcp.example.com/servers", "only contain a scheme and host"},
		{"https://mcp.example.com/?a=1", "only contain a scheme and host"},
		{"", "must include a host"},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			origin, err := parseUpstreamOrigin(test.value)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("parseUpstreamOrigin() error = %v", err)
				}
				if origin.Hostname() != "mcp.example.com" {
					t.Fatalf("hostname = %s", origin.Hostname())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("parseUpstreamOrigin() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestResolveUpstream(t *testing.T) {
	tests := []struct {
		name         string
		header       string
		target       string
		wantUpstream string
		wantErr      string // empty means success
	}{
		{
			name:         "scheme host and path",
			header:       "https://MCP.Example.com",
			target:       "http://fetch-worker/v1/proxy/servers/x",
			wantUpstream: "https://mcp.example.com/servers/x",
		},
		{
			name:         "bare host defaults to https",
			header:       "mcp.example.com",
			target:       "http://fetch-worker/v1/proxy/",
			wantUpstream: "https://mcp.example.com/",
		},
		{
			name:         "encoded path and query preserved",
			header:       "https://mcp.example.com",
			target:       "http://fetch-worker/v1/proxy/a%20b/x?a=1&b=2+c",
			wantUpstream: "https://mcp.example.com/a%20b/x?a=1&b=2+c",
		},
		{
			name:         "non-standard port admitted without configuration",
			header:       "https://mcp.example.com:8443",
			target:       "http://fetch-worker/v1/proxy/x",
			wantUpstream: "https://mcp.example.com:8443/x",
		},
		{
			name:         "arbitrary vendor host admitted",
			header:       "https://anywhere-vendor.example.net",
			target:       "http://fetch-worker/v1/proxy/x",
			wantErr:      "",
			wantUpstream: "https://anywhere-vendor.example.net/x",
		},
		{
			name:    "missing header",
			header:  "",
			target:  "http://fetch-worker/v1/proxy/x",
			wantErr: "X-Mcp-Host header is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := &server{}
			req := httptest.NewRequest(http.MethodGet, test.target, nil)
			if test.header != "" {
				req.Header.Set(upstreamHostHeader, test.header)
			}
			target, err := s.resolveUpstream(req)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("resolveUpstream() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveUpstream() error = %v", err)
			}
			if target.String() != test.wantUpstream {
				t.Fatalf("target = %s, want %s", target.String(), test.wantUpstream)
			}
		})
	}
}

func TestProxyRequiresAuthentication(t *testing.T) {
	s := newLoopbackProxyServer(testConfig())
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/x", strings.NewReader("{}"))
	res := httptest.NewRecorder()
	s.proxy(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Code)
	}
}

func TestProxyRejectsMissingUpstreamHeader(t *testing.T) {
	s := newLoopbackProxyServer(testConfig())
	res := httptest.NewRecorder()
	s.proxy(res, authorizedProxyRequest(http.MethodPost, "/v1/proxy/x", strings.NewReader("{}")))
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	if !strings.Contains(res.Body.String(), "INVALID_REQUEST") {
		t.Fatalf("body = %s", res.Body.String())
	}
}

// TestProxyBlocksLoopback proves the safe dialer (SSRF protection) also guards
// the proxy route, not only /v1/fetch.
func TestProxyBlocksLoopback(t *testing.T) {
	cfg := testConfig()
	s := newServer(cfg) // real safeDialer blocks loopback
	req := authorizedProxyRequest(http.MethodGet, "/v1/proxy/x", nil)
	req.Header.Set(upstreamHostHeader, "http://127.0.0.1")
	res := httptest.NewRecorder()
	s.proxy(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.Code)
	}
	if !strings.Contains(res.Body.String(), "DESTINATION_BLOCKED") {
		t.Fatalf("body = %s", res.Body.String())
	}
}

// startProxyPair runs an httptest upstream plus a downstream server exposing
// the proxy mux, returning the downstream URL and the upstream origin. The
// upstream needs no allowlist entry — any origin is admitted.
func startProxyPair(t *testing.T, cfg config, upstream http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)
	upstreamURL, err := url.Parse(upstreamServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := newLoopbackProxyServer(cfg)
	downstream := httptest.NewServer(newProxyMux(s))
	t.Cleanup(downstream.Close)
	return downstream, upstreamURL.String()
}

// noRedirectClient surfaces redirect responses instead of following them, so
// tests can assert exactly what the proxy returned.
var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// rawHeaderClient additionally suppresses the transport's automatic
// "Accept-Encoding: gzip", so tests can assert the proxy forwards that
// header exactly as the caller wrote it — or not at all.
var rawHeaderClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport:     &http.Transport{DisableCompression: true},
}

func proxyClientRequest(t *testing.T, method, downstreamURL, upstreamOrigin, path string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, downstreamURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	req.Header.Set(upstreamHostHeader, upstreamOrigin)
	res, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestProxyHeaderPolicy(t *testing.T) {
	var received http.Header
	var receivedHost string
	downstream, origin := startProxyPair(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Clone()
		receivedHost = r.Host
		w.WriteHeader(http.StatusOK)
	})

	req, err := http.NewRequest(http.MethodPost, downstream.URL+"/v1/proxy/servers/x", strings.NewReader(`{"jsonrpc":"2.0"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	req.Header.Set(upstreamHostHeader, origin)
	req.Header.Set("X-User-Id", "748112da")
	req.Header.Set("Mcp-Session-Id", "session-123")
	req.Header.Set(upstreamAuthHeader, "Bearer upstream-secret")
	req.Header.Set("X-Request-ID", "req-1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if got := received.Get("X-Mcp-Host"); got != "" {
		t.Errorf("X-Mcp-Host leaked upstream: %q", got)
	}
	if got := received.Get("X-Upstream-Authorization"); got != "" {
		t.Errorf("X-Upstream-Authorization leaked upstream: %q", got)
	}
	if got := received.Get("Authorization"); got != "Bearer upstream-secret" {
		t.Errorf("Authorization = %q, want mapped upstream credentials", got)
	}
	if got := received.Get("X-User-Id"); got != "748112da" {
		t.Errorf("X-User-Id = %q, want passthrough", got)
	}
	if got := received.Get("Mcp-Session-Id"); got != "session-123" {
		t.Errorf("Mcp-Session-Id = %q, want passthrough", got)
	}
	if got := received.Get("X-Request-ID"); got != "" {
		t.Errorf("X-Request-ID leaked upstream: %q", got)
	}
	upstreamHost := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	if receivedHost != upstreamHost {
		t.Errorf("Host = %q, want upstream host %q", receivedHost, upstreamHost)
	}
}

func TestProxyAcceptEncodingPassthrough(t *testing.T) {
	tests := []struct {
		name         string
		clientHeader string
		wantSeen     string
	}{
		{"client omitted Accept-Encoding, upstream must not see one", "", ""},
		{"client value forwarded verbatim", "gzip, br", "gzip, br"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen string
			downstream, origin := startProxyPair(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Get("Accept-Encoding")
				w.WriteHeader(http.StatusOK)
			})
			req, err := http.NewRequest(http.MethodPost, downstream.URL+"/v1/proxy/servers/x", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
			req.Header.Set(upstreamHostHeader, origin)
			if tt.clientHeader != "" {
				req.Header.Set("Accept-Encoding", tt.clientHeader)
			}
			res, err := rawHeaderClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if seen != tt.wantSeen {
				t.Errorf("upstream Accept-Encoding = %q, want %q", seen, tt.wantSeen)
			}
		})
	}
}

func TestProxyVerbatimPassthrough(t *testing.T) {
	const rpcResult = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`
	downstream, origin := startProxyPair(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "session-abc")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, rpcResult)
	})

	res := proxyClientRequest(t, http.MethodPost, downstream.URL, origin, "/v1/proxy/servers/institutioncenter",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Mcp-Session-Id"); got != "session-abc" {
		t.Fatalf("Mcp-Session-Id = %q, want passthrough", got)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != rpcResult {
		t.Fatalf("body = %s, want verbatim %s", body, rpcResult)
	}
}

func TestProxyPathAndQueryPassthrough(t *testing.T) {
	var seenURI string
	downstream, origin := startProxyPair(t, testConfig(), func(w http.ResponseWriter, r *http.Request) {
		seenURI = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
	})

	proxyClientRequest(t, http.MethodGet, downstream.URL, origin, "/v1/proxy/a%20b/x?a=1&b=2+c", nil)
	if seenURI != "/a%20b/x?a=1&b=2+c" {
		t.Fatalf("upstream saw %q, want %q", seenURI, "/a%20b/x?a=1&b=2+c")
	}
}

func TestProxyDeletePassthrough(t *testing.T) {
	downstream, origin := startProxyPair(t, testConfig(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	res := proxyClientRequest(t, http.MethodDelete, downstream.URL, origin, "/v1/proxy/servers/x", nil)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", res.StatusCode)
	}
}

func TestProxyMethodNotAllowed(t *testing.T) {
	downstream, _ := startProxyPair(t, testConfig(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	res := proxyClientRequest(t, http.MethodPut, downstream.URL, "http://127.0.0.1", "/v1/proxy/x", nil)
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.StatusCode)
	}
}

// TestProxySSEStreamFlushes asserts events reach the client while the
// upstream is still producing them; a buffering hop would delay every event
// until the stream completes.
func TestProxySSEStreamFlushes(t *testing.T) {
	const events = 6
	const interval = 100 * time.Millisecond
	downstream, origin := startProxyPair(t, testConfig(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < events; i++ {
			_, _ = io.WriteString(w, "event: message\ndata: {\"n\":")
			_, _ = io.WriteString(w, string(rune('0'+i)))
			_, _ = io.WriteString(w, "}\n\n")
			flusher.Flush()
			time.Sleep(interval)
		}
	})

	started := time.Now()
	res := proxyClientRequest(t, http.MethodGet, downstream.URL, origin, "/v1/proxy/servers/x", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	scanner := bufio.NewScanner(res.Body)
	if !scanner.Scan() {
		t.Fatalf("no event received: %v", scanner.Err())
	}
	firstEventElapsed := time.Since(started)
	if firstEventElapsed > time.Duration(events-1)*interval {
		t.Fatalf("first event after %s; the proxy is buffering the stream", firstEventElapsed)
	}
	if !strings.HasPrefix(scanner.Text(), "event: message") {
		t.Fatalf("unexpected first line %q", scanner.Text())
	}
}

func TestProxyStreamByteCap(t *testing.T) {
	cfg := testConfig()
	cfg.ProxyMaxStreamBytes = 16
	downstream, origin := startProxyPair(t, cfg, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, strings.Repeat("x", 1024))
		flusher.Flush()
	})

	res := proxyClientRequest(t, http.MethodGet, downstream.URL, origin, "/v1/proxy/x", nil)
	// The cap aborts the stream mid-flight, so the client sees a truncated
	// body; assert on the number of bytes actually delivered.
	var delivered bytes.Buffer
	n, _ := io.CopyN(&delivered, res.Body, cfg.ProxyMaxStreamBytes+1024)
	if n > cfg.ProxyMaxStreamBytes {
		t.Fatalf("body size = %d, want capped at %d", n, cfg.ProxyMaxStreamBytes)
	}
}

func TestRewriteUpstreamRedirect(t *testing.T) {
	tests := []struct {
		name         string
		prefix       string
		location     string
		upstreamHost string
		want         string // expected Location after rewriting
	}{
		{"absolute url same host", "/ia-access-gateway/mcp", "https://mcp.example.com/servers/x/", "mcp.example.com", "/ia-access-gateway/mcp/servers/x/"},
		{"absolute path", "/ia-access-gateway/mcp", "/servers/x/", "mcp.example.com", "/ia-access-gateway/mcp/servers/x/"},
		{"absolute url with query", "/pfx", "https://mcp.example.com/a?b=1", "mcp.example.com", "/pfx/a?b=1"},
		{"cross host untouched", "/pfx", "https://cdn.example.com/a", "mcp.example.com", "https://cdn.example.com/a"},
		{"no prefix configured", "", "/servers/x/", "mcp.example.com", "/servers/x/"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.PublicPathPrefix = test.prefix
			s := newLoopbackProxyServer(cfg)
			target, err := url.Parse("https://" + test.upstreamHost + "/servers/x")
			if err != nil {
				t.Fatal(err)
			}
			resp := &http.Response{Header: http.Header{}, StatusCode: http.StatusTemporaryRedirect}
			resp.Header.Set("Location", test.location)
			s.rewriteUpstreamRedirect(resp, target, "test")
			if got := resp.Header.Get("Location"); got != test.want {
				t.Fatalf("Location = %q, want %q", got, test.want)
			}
		})
	}
}

func TestProxyRedirectLocationRewritten(t *testing.T) {
	cfg := testConfig()
	cfg.PublicPathPrefix = "/ia-access-gateway/mcp"
	downstream, origin := startProxyPair(t, cfg, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/servers/x/")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	res := proxyClientRequest(t, http.MethodGet, downstream.URL, origin, "/v1/proxy/servers/x", nil)
	if got := res.Header.Get("Location"); got != "/ia-access-gateway/mcp/servers/x/" {
		t.Fatalf("Location = %q", got)
	}
}

func TestProxyRequestSizeLimit(t *testing.T) {
	downstream, origin := startProxyPair(t, testConfig(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	res := proxyClientRequest(t, http.MethodPost, downstream.URL, origin, "/v1/proxy/x",
		bytes.NewReader(make([]byte, proxyRequestBodyLimit+1)))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}
