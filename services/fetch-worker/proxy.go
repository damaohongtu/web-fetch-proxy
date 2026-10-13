// Package main implements the transparent MCP proxy route.
//
// Unlike /v1/fetch, which wraps the exchange in a JSON envelope, this route
// forwards MCP streamable-http requests (GET/POST/DELETE) to the upstream
// selected by the X-Mcp-Host header and passes method, headers, body and
// streaming responses through verbatim.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const (
	proxyPathPrefix       = "/v1/proxy"
	upstreamHostHeader    = "X-Mcp-Host"
	upstreamAuthHeader    = "X-Upstream-Authorization"
	proxyRequestBodyLimit = 64 << 10
)

func newProxyMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/proxy/", s.proxy)
	mux.HandleFunc("POST /v1/proxy/", s.proxy)
	mux.HandleFunc("DELETE /v1/proxy/", s.proxy)
	return mux
}

type upstreamNotAllowedError struct{ host string }

func (e *upstreamNotAllowedError) Error() string {
	return fmt.Sprintf("upstream host %q is not permitted by ALLOWED_UPSTREAM_HOSTS", e.host)
}

func (s *server) proxy(w http.ResponseWriter, r *http.Request) {
	requestID := r.Header.Get("X-Request-ID")
	if requestID == "" {
		requestID = randomID()
	}
	w.Header().Set("X-Request-ID", requestID)

	if !validBearer(r.Header.Get("Authorization"), s.cfg.AuthToken) {
		writeAPIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid authentication credentials")
		return
	}

	select {
	case s.proxySem <- struct{}{}:
		defer func() { <-s.proxySem }()
	default:
		writeAPIError(w, http.StatusTooManyRequests, "TOO_MANY_REQUESTS", "proxy concurrency limit reached")
		return
	}

	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, proxyRequestBodyLimit)
	}

	target, err := s.resolveUpstream(r)
	if err != nil {
		var notAllowed *upstreamNotAllowedError
		var secErr *securityError
		switch {
		case errors.As(err, &notAllowed):
			writeAPIError(w, http.StatusForbidden, "UPSTREAM_NOT_ALLOWED", notAllowed.Error())
		case errors.As(err, &secErr):
			writeAPIError(w, http.StatusForbidden, "DESTINATION_BLOCKED", secErr.Error())
		default:
			writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		}
		return
	}

	recorder := &proxyResponseWriter{ResponseWriter: w}
	started := time.Now()
	s.newReverseProxy(target, requestID).ServeHTTP(recorder, r)
	s.logger.Info("proxy completed",
		"request_id", requestID,
		"upstream_host", target.Hostname(),
		"method", r.Method,
		"path", target.EscapedPath(),
		"status", recorder.status,
		"bytes", recorder.bytes,
		"elapsed_ms", time.Since(started).Milliseconds())
}

// resolveUpstream derives the outbound URL from the X-Mcp-Host header and the
// inbound path, keeping the query string and any percent-encoding intact.
func (s *server) resolveUpstream(r *http.Request) (*url.URL, error) {
	value := strings.TrimSpace(r.Header.Get(upstreamHostHeader))
	if value == "" {
		return nil, errors.New("the X-Mcp-Host header is required to select the upstream")
	}
	origin, err := parseUpstreamOrigin(value)
	if err != nil {
		return nil, err
	}
	if err := s.checkUpstreamAllowed(origin); err != nil {
		return nil, err
	}

	target := *r.URL
	target.Scheme = origin.Scheme
	target.Host = strings.ToLower(origin.Host)
	target.User = nil
	target.Fragment = ""
	target.Path = strings.TrimPrefix(r.URL.Path, proxyPathPrefix)
	if r.URL.RawPath != "" {
		target.RawPath = strings.TrimPrefix(r.URL.RawPath, proxyPathPrefix)
	} else {
		target.RawPath = ""
	}
	return &target, nil
}

// checkUpstreamAllowed matches the origin against ALLOWED_UPSTREAM_HOSTS. A
// bare-host entry only admits ports 80 and 443; a "host:port" entry admits
// exactly that port, for upstreams served on non-standard ports.
func (s *server) checkUpstreamAllowed(origin *url.URL) error {
	hostname := strings.ToLower(origin.Hostname())
	port := origin.Port()
	for _, entry := range s.cfg.AllowedUpstreamHosts {
		entryHost, entryPort, hasPort := strings.Cut(entry, ":")
		if entryHost != hostname {
			continue
		}
		if hasPort {
			if entryPort == port || (port == "" && entryPort == defaultPort(origin.Scheme)) {
				return nil
			}
			continue
		}
		if port == "" || port == "80" || port == "443" {
			return nil
		}
	}
	return &upstreamNotAllowedError{host: origin.Host}
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

func parseUpstreamOrigin(value string) (*url.URL, error) {
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	origin, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("invalid X-Mcp-Host header: %w", err)
	}
	if origin.Scheme != "http" && origin.Scheme != "https" {
		return nil, &securityError{message: "X-Mcp-Host header only supports http and https origins"}
	}
	if origin.User != nil {
		return nil, &securityError{message: "X-Mcp-Host header must not contain credentials"}
	}
	if origin.Hostname() == "" {
		return nil, errors.New("X-Mcp-Host header must include a host")
	}
	if origin.Path != "" && origin.Path != "/" || origin.RawQuery != "" || origin.Fragment != "" {
		return nil, errors.New("X-Mcp-Host header must only contain a scheme and host")
	}
	return origin, nil
}

// newProxyTransport clones the fetch transport with transparent gzip
// disabled: net/http otherwise injects "Accept-Encoding: gzip" when the
// client sent none and silently decompresses the response, both of which
// violate the verbatim passthrough this route promises.
func newProxyTransport(base *http.Transport) *http.Transport {
	clone := base.Clone()
	clone.DisableCompression = true
	return clone
}

func (s *server) newReverseProxy(target *url.URL, requestID string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = target
			pr.Out.Host = target.Host
			pr.Out.RequestURI = ""
			pr.Out.Header.Del(upstreamHostHeader)
			// The proxy's own Bearer token must never leak to the upstream.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del(upstreamAuthHeader)
			pr.Out.Header.Del("X-Request-ID")
			pr.Out.Header.Del("X-Client-IP")
			pr.Out.Header.Del("X-Client-Cert-Subject")
			pr.Out.Header.Del("X-Forwarded-For")
			if credentials := pr.In.Header.Get(upstreamAuthHeader); credentials != "" {
				pr.Out.Header.Set("Authorization", credentials)
			}
		},
		Transport:     s.proxyTransport,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			s.rewriteUpstreamRedirect(resp, target, requestID)
			if s.cfg.ProxyMaxStreamBytes > 0 {
				resp.Body = &cappedStreamBody{
					ReadCloser: resp.Body,
					cap:        s.cfg.ProxyMaxStreamBytes,
					remaining:  s.cfg.ProxyMaxStreamBytes,
					requestID:  requestID,
					logger:     s.logger,
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			var maxBytesErr *http.MaxBytesError
			switch {
			case errors.Is(err, context.Canceled):
				// The client went away mid-stream; there is nobody left to report to.
			case errors.As(err, &maxBytesErr):
				writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body exceeds the proxy size limit")
			default:
				status, code, message := classifyFetchError(err)
				writeAPIError(w, status, code, message)
			}
			s.logger.Warn("proxy upstream failed",
				"request_id", requestID,
				"upstream_host", target.Hostname(),
				"error", err.Error())
		},
	}
}

// rewriteUpstreamRedirect maps upstream 3xx Location headers back into the
// public path prefix so that MCP clients following the redirect stay on the
// gateway route. Cross-host redirects cannot be expressed through the
// X-Mcp-Host convention and pass through untouched.
func (s *server) rewriteUpstreamRedirect(resp *http.Response, target *url.URL, requestID string) {
	location := resp.Header.Get("Location")
	if location == "" {
		return
	}
	parsed, err := url.Parse(location)
	if err != nil {
		s.logger.Warn("proxy upstream returned an unparseable Location header",
			"request_id", requestID, "upstream_host", target.Hostname(), "location", location)
		return
	}
	if parsed.IsAbs() && !strings.EqualFold(parsed.Hostname(), target.Hostname()) {
		s.logger.Warn("proxy upstream redirected to another host",
			"request_id", requestID, "upstream_host", target.Hostname(), "location", location)
		return
	}
	s.logger.Warn("proxy upstream returned a redirect",
		"request_id", requestID, "upstream_host", target.Hostname(),
		"status", resp.StatusCode, "location", location)
	if s.cfg.PublicPathPrefix == "" {
		return
	}
	suffix := parsed.RequestURI()
	if suffix == "" {
		suffix = "/"
	}
	resp.Header.Set("Location", s.cfg.PublicPathPrefix+suffix)
}

// cappedStreamBody aborts a runaway stream once the configured byte cap is
// reached; the client observes a truncated response.
type cappedStreamBody struct {
	io.ReadCloser
	cap       int64
	remaining int64
	requestID string
	logger    *slog.Logger
	tripped   bool
}

func (b *cappedStreamBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		if !b.tripped {
			b.tripped = true
			b.logger.Warn("proxy stream exceeded the configured byte cap",
				"request_id", b.requestID, "cap_bytes", b.cap)
		}
		return 0, errors.New("proxy stream exceeded the configured maximum response size")
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

// proxyResponseWriter records the status and byte count for logging while
// preserving Flush support, which streaming responses require.
type proxyResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *proxyResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *proxyResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *proxyResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
