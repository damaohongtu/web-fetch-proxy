// Package main implements the cloud-side Web Fetch worker.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

type config struct {
	ListenAddr            string
	AuthToken             string
	LogFile               string
	DefaultTimeout        time.Duration
	MaxTimeout            time.Duration
	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
	DefaultMaxBytes       int64
	HardMaxBytes          int64
	DefaultMaxRedirects   int
	HardMaxRedirects      int
	MaxConcurrency        int
	BlockedPrefixes       []netip.Prefix
	ProxyListenAddr       string
	AllowedUpstreamHosts  []string
	ProxyMaxConcurrency   int
	ProxyMaxStreamBytes   int64
	PublicPathPrefix      string
}

type fetchRequest struct {
	URL             string            `json:"url"`
	Method          string            `json:"method,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	TimeoutMS       int               `json:"timeout_ms,omitempty"`
	MaxBytes        int64             `json:"max_bytes,omitempty"`
	FollowRedirects *bool             `json:"follow_redirects,omitempty"`
	MaxRedirects    int               `json:"max_redirects,omitempty"`
}

type redirectRecord struct {
	Status int    `json:"status"`
	From   string `json:"from"`
	To     string `json:"to"`
}

type fetchResponse struct {
	RequestID    string            `json:"request_id"`
	RequestedURL string            `json:"requested_url"`
	FinalURL     string            `json:"final_url"`
	Status       int               `json:"status"`
	Headers      map[string]string `json:"headers"`
	Body         string            `json:"body"`
	BodyEncoding string            `json:"body_encoding"`
	Size         int               `json:"size"`
	Truncated    bool              `json:"truncated"`
	Redirects    []redirectRecord  `json:"redirects"`
	ElapsedMS    int64             `json:"elapsed_ms"`
}

type apiError struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type securityError struct{ message string }

func (e *securityError) Error() string { return e.message }

type server struct {
	cfg            config
	transport      *http.Transport
	proxyTransport *http.Transport
	sem            chan struct{}
	proxySem       chan struct{}
	logger         *slog.Logger
}

var allowedRequestHeaders = map[string]struct{}{
	"Accept":            {},
	"Accept-Language":   {},
	"User-Agent":        {},
	"Range":             {},
	"If-None-Match":     {},
	"If-Modified-Since": {},
}

var allowedResponseHeaders = map[string]struct{}{
	"Content-Type":     {},
	"Content-Length":   {},
	"Content-Encoding": {},
	"Content-Language": {},
	"Last-Modified":    {},
	"Etag":             {},
	"Cache-Control":    {},
	"Expires":          {},
	"Location":         {},
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	logger, closeLog, err := newApplicationLogger(cfg.LogFile)
	if err != nil {
		slog.Error("initialize application logger", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := closeLog(); err != nil {
			slog.Error("close application log", "error", err)
		}
	}()

	s := newServerWithLogger(cfg, logger)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/fetch", s.fetch)

	proxyMux := newProxyMux(s)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.MaxTimeout + 10*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	// The proxy listener serves long-lived SSE streams, so it must not carry
	// the fetch server's WriteTimeout. Idle streams are cut by Nginx's
	// proxy_read_timeout instead.
	proxyServer := &http.Server{
		Addr:              cfg.ProxyListenAddr,
		Handler:           proxyMux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       75 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	serverError := make(chan error, 2)
	go func() {
		s.logger.Info("fetch worker listening", "address", cfg.ListenAddr)
		serverError <- httpServer.ListenAndServe()
	}()
	go func() {
		s.logger.Info("proxy worker listening",
			"address", cfg.ProxyListenAddr, "allowed_upstreams", len(cfg.AllowedUpstreamHosts))
		serverError <- proxyServer.ListenAndServe()
	}()

	shutdownContext, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	select {
	case err := <-serverError:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("server stopped", "error", err)
			os.Exit(1)
		}
	case <-shutdownContext.Done():
		s.logger.Info("shutdown requested")
		graceContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(graceContext); err != nil {
			s.logger.Error("graceful shutdown failed", "error", err)
			os.Exit(1)
		}
		if err := proxyServer.Shutdown(graceContext); err != nil {
			s.logger.Error("graceful shutdown failed", "error", err)
			os.Exit(1)
		}
	}
}

func newServer(cfg config) *server {
	return newServerWithLogger(cfg, slog.New(slog.NewJSONHandler(os.Stdout, nil)))
}

func newServerWithLogger(cfg config, logger *slog.Logger) *server {
	dialer := &safeDialer{
		resolver:        net.DefaultResolver,
		dialer:          &net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second},
		blockedPrefixes: cfg.BlockedPrefixes,
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &server{
		cfg:            cfg,
		transport:      transport,
		proxyTransport: newProxyTransport(transport),
		sem:            make(chan struct{}, cfg.MaxConcurrency),
		proxySem:       make(chan struct{}, cfg.ProxyMaxConcurrency),
		logger:         logger,
	}
}

func newApplicationLogger(path string) (*slog.Logger, func() error, error) {
	if strings.TrimSpace(path) == "" {
		return slog.New(slog.NewJSONHandler(os.Stdout, nil)), func() error { return nil }, nil
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, fmt.Errorf("open application log %q: %w", path, err)
	}
	output := io.MultiWriter(os.Stdout, file)
	return slog.New(slog.NewJSONHandler(output, nil)), file.Close, nil
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) fetch(w http.ResponseWriter, r *http.Request) {
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
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		writeAPIError(w, http.StatusTooManyRequests, "TOO_MANY_REQUESTS", "worker concurrency limit reached")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req fetchRequest
	if err := decoder.Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body must be valid JSON: "+err.Error())
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	normalized, err := s.normalizeRequest(req)
	if err != nil {
		var secErr *securityError
		if errors.As(err, &secErr) {
			writeAPIError(w, http.StatusForbidden, "DESTINATION_BLOCKED", secErr.Error())
		} else {
			writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		}
		return
	}

	started := time.Now()
	result, err := s.perform(r.Context(), requestID, normalized)
	if err != nil {
		status, code, message := classifyFetchError(err)
		writeAPIError(w, status, code, message)
		s.logger.Warn("fetch failed", "request_id", requestID, "host", normalized.parsedURL.Hostname(), "error_code", code, "elapsed_ms", time.Since(started).Milliseconds())
		return
	}
	result.ElapsedMS = time.Since(started).Milliseconds()
	s.logger.Info("fetch completed", "request_id", requestID, "host", normalized.parsedURL.Hostname(), "status", result.Status, "size", result.Size, "truncated", result.Truncated, "elapsed_ms", result.ElapsedMS)
	writeJSON(w, http.StatusOK, result)
}

type normalizedRequest struct {
	fetchRequest
	parsedURL       *url.URL
	timeout         time.Duration
	maxBytes        int64
	maxRedirects    int
	followRedirects bool
}

func (s *server) normalizeRequest(req fetchRequest) (normalizedRequest, error) {
	if strings.TrimSpace(req.URL) == "" {
		return normalizedRequest{}, errors.New("url is required")
	}
	parsed, err := url.Parse(req.URL)
	if err != nil {
		return normalizedRequest{}, fmt.Errorf("invalid url: %w", err)
	}
	if err := validateURLSyntax(parsed); err != nil {
		return normalizedRequest{}, err
	}

	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodHead {
		return normalizedRequest{}, errors.New("method must be GET or HEAD")
	}
	req.Method = method

	timeout := s.cfg.DefaultTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	if timeout <= 0 || timeout > s.cfg.MaxTimeout {
		return normalizedRequest{}, fmt.Errorf("timeout_ms must be between 1 and %d", s.cfg.MaxTimeout.Milliseconds())
	}

	maxBytes := s.cfg.DefaultMaxBytes
	if req.MaxBytes > 0 {
		maxBytes = req.MaxBytes
	}
	if maxBytes <= 0 || maxBytes > s.cfg.HardMaxBytes {
		return normalizedRequest{}, fmt.Errorf("max_bytes must be between 1 and %d", s.cfg.HardMaxBytes)
	}

	maxRedirects := s.cfg.DefaultMaxRedirects
	if req.MaxRedirects > 0 {
		maxRedirects = req.MaxRedirects
	}
	if maxRedirects < 0 || maxRedirects > s.cfg.HardMaxRedirects {
		return normalizedRequest{}, fmt.Errorf("max_redirects must be between 0 and %d", s.cfg.HardMaxRedirects)
	}

	follow := true
	if req.FollowRedirects != nil {
		follow = *req.FollowRedirects
	}
	for name := range req.Headers {
		canonical := http.CanonicalHeaderKey(name)
		if _, ok := allowedRequestHeaders[canonical]; !ok {
			return normalizedRequest{}, fmt.Errorf("request header %q is not allowed", name)
		}
	}

	return normalizedRequest{fetchRequest: req, parsedURL: parsed, timeout: timeout, maxBytes: maxBytes, maxRedirects: maxRedirects, followRedirects: follow}, nil
}

func (s *server) perform(parent context.Context, requestID string, req normalizedRequest) (fetchResponse, error) {
	ctx, cancel := context.WithTimeout(parent, req.timeout)
	defer cancel()

	outbound, err := http.NewRequestWithContext(ctx, req.Method, req.parsedURL.String(), nil)
	if err != nil {
		return fetchResponse{}, err
	}
	for name, value := range req.Headers {
		outbound.Header.Set(http.CanonicalHeaderKey(name), value)
	}
	if outbound.Header.Get("User-Agent") == "" {
		outbound.Header.Set("User-Agent", "WebFetchProxy/1.0")
	}

	redirects := make([]redirectRecord, 0)
	client := &http.Client{Transport: s.transport}
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if !req.followRedirects {
			return http.ErrUseLastResponse
		}
		if len(via) > req.maxRedirects {
			return fmt.Errorf("maximum redirects exceeded")
		}
		if err := validateURLSyntax(next.URL); err != nil {
			return err
		}
		status := 0
		if next.Response != nil {
			status = next.Response.StatusCode
		}
		redirects = append(redirects, redirectRecord{Status: status, From: via[len(via)-1].URL.String(), To: next.URL.String()})
		return nil
	}

	resp, err := client.Do(outbound)
	if err != nil {
		return fetchResponse{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, req.maxBytes+1))
	if err != nil {
		return fetchResponse{}, err
	}
	truncated := int64(len(body)) > req.maxBytes
	if truncated {
		body = body[:req.maxBytes]
	}

	bodyText := string(body)
	encoding := "utf8"
	if !utf8.Valid(body) {
		bodyText = base64.StdEncoding.EncodeToString(body)
		encoding = "base64"
	}

	headers := filterResponseHeaders(resp.Header)
	if resp.Uncompressed {
		delete(headers, "content-encoding")
		delete(headers, "content-length")
	}
	return fetchResponse{
		RequestID: requestID, RequestedURL: req.parsedURL.String(), FinalURL: resp.Request.URL.String(),
		Status: resp.StatusCode, Headers: headers, Body: bodyText, BodyEncoding: encoding,
		Size: len(body), Truncated: truncated, Redirects: redirects,
	}, nil
}

func validateURLSyntax(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return &securityError{message: "only http and https URLs are allowed"}
	}
	if u.Hostname() == "" {
		return errors.New("url must include a host")
	}
	if u.User != nil {
		return &securityError{message: "URL userinfo is not allowed"}
	}
	port := u.Port()
	if port != "" && port != "80" && port != "443" {
		return &securityError{message: "only destination ports 80 and 443 are allowed"}
	}
	return nil
}

type resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type contextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

type safeDialer struct {
	resolver        resolver
	dialer          contextDialer
	blockedPrefixes []netip.Prefix
}

func (d *safeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if literal, parseErr := netip.ParseAddr(host); parseErr == nil {
		ips = []netip.Addr{literal}
	} else {
		ips, err = d.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 {
		return nil, &net.DNSError{Name: host, Err: "no addresses returned"}
	}
	for _, ip := range ips {
		if isBlockedIP(ip, d.blockedPrefixes) {
			return nil, &securityError{message: "destination resolves to a prohibited address"}
		}
	}
	var lastErr error
	for _, ip := range ips {
		conn, dialErr := d.dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, lastErr
}

func isBlockedIP(ip netip.Addr, extra []netip.Prefix) bool {
	if !ip.IsValid() {
		return true
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return true
	}
	for _, prefix := range append(defaultBlockedPrefixes(), extra...) {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func defaultBlockedPrefixes() []netip.Prefix {
	values := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	}
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}

func filterResponseHeaders(input http.Header) map[string]string {
	result := make(map[string]string)
	for name := range allowedResponseHeaders {
		if value := input.Get(name); value != "" {
			result[strings.ToLower(name)] = value
		}
	}
	return result
}

func validBearer(header, expected string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	actual := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if len(actual) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("request body must contain exactly one JSON value")
	}
	return fmt.Errorf("invalid trailing JSON data: %w", err)
}

func classifyFetchError(err error) (int, string, string) {
	var secErr *securityError
	if errors.As(err, &secErr) {
		return http.StatusForbidden, "DESTINATION_BLOCKED", secErr.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "FETCH_TIMEOUT", "the upstream request exceeded its timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return http.StatusBadGateway, "DNS_FAILED", "failed to resolve destination hostname"
	}
	return http.StatusBadGateway, "CONNECT_FAILED", "failed to fetch destination"
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSONStatus(w, status, apiError{Error: errorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	writeJSONStatus(w, status, value)
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		slog.Error("failed to encode response", "error", err)
	}
}

func randomID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(buffer)
}

func loadConfig() (config, error) {
	token, err := loadSecret("FETCH_AUTH_TOKEN", "FETCH_AUTH_TOKEN_FILE")
	if err != nil {
		return config{}, err
	}
	if len(token) < 32 {
		return config{}, errors.New("fetch authentication token must contain at least 32 characters")
	}

	cfg := config{
		ListenAddr:            envString("LISTEN_ADDR", "0.0.0.0:8080"),
		AuthToken:             token,
		LogFile:               strings.TrimSpace(os.Getenv("LOG_FILE")),
		DefaultTimeout:        envDurationMS("DEFAULT_TIMEOUT_MS", 30000),
		MaxTimeout:            envDurationMS("MAX_TIMEOUT_MS", 60000),
		ConnectTimeout:        envDurationMS("CONNECT_TIMEOUT_MS", 5000),
		ResponseHeaderTimeout: envDurationMS("RESPONSE_HEADER_TIMEOUT_MS", 15000),
		DefaultMaxBytes:       envInt64("DEFAULT_MAX_BYTES", 10<<20),
		HardMaxBytes:          envInt64("HARD_MAX_BYTES", 50<<20),
		DefaultMaxRedirects:   envInt("DEFAULT_MAX_REDIRECTS", 5),
		HardMaxRedirects:      envInt("HARD_MAX_REDIRECTS", 10),
		MaxConcurrency:        envInt("MAX_CONCURRENCY", 100),
		ProxyListenAddr:       envString("PROXY_LISTEN_ADDR", "0.0.0.0:8081"),
		ProxyMaxConcurrency:   envInt("PROXY_MAX_CONCURRENCY", 50),
		ProxyMaxStreamBytes:   envInt64("PROXY_MAX_STREAM_BYTES", 100<<20),
		PublicPathPrefix:      normalizePublicPathPrefix(strings.TrimSpace(os.Getenv("PUBLIC_PATH_PREFIX"))),
	}
	if cfg.DefaultTimeout <= 0 || cfg.DefaultTimeout > cfg.MaxTimeout {
		return config{}, errors.New("DEFAULT_TIMEOUT_MS must be positive and no greater than MAX_TIMEOUT_MS")
	}
	if cfg.DefaultMaxBytes <= 0 || cfg.DefaultMaxBytes > cfg.HardMaxBytes {
		return config{}, errors.New("DEFAULT_MAX_BYTES must be positive and no greater than HARD_MAX_BYTES")
	}
	if cfg.MaxConcurrency <= 0 {
		return config{}, errors.New("MAX_CONCURRENCY must be positive")
	}
	if cfg.ProxyMaxConcurrency <= 0 {
		return config{}, errors.New("PROXY_MAX_CONCURRENCY must be positive")
	}
	if cfg.ProxyMaxStreamBytes < 0 {
		return config{}, errors.New("PROXY_MAX_STREAM_BYTES must not be negative")
	}
	if raw := strings.TrimSpace(os.Getenv("ALLOWED_UPSTREAM_HOSTS")); raw != "" {
		for _, value := range strings.Split(raw, ",") {
			entry := strings.ToLower(strings.TrimSpace(value))
			host, port, hasPort := strings.Cut(entry, ":")
			if host == "" || strings.ContainsAny(host, ":/?#@ ") {
				return config{}, fmt.Errorf("invalid ALLOWED_UPSTREAM_HOSTS entry %q: expected a hostname such as mcp-server.comein.cn, optionally with :port", value)
			}
			if hasPort {
				portNum, convErr := strconv.Atoi(port)
				if convErr != nil || portNum < 1 || portNum > 65535 {
					return config{}, fmt.Errorf("invalid ALLOWED_UPSTREAM_HOSTS entry %q: port must be between 1 and 65535", value)
				}
			}
			cfg.AllowedUpstreamHosts = append(cfg.AllowedUpstreamHosts, entry)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("BLOCKED_CIDRS")); raw != "" {
		for _, value := range strings.Split(raw, ",") {
			prefix, parseErr := netip.ParsePrefix(strings.TrimSpace(value))
			if parseErr != nil {
				return config{}, fmt.Errorf("invalid BLOCKED_CIDRS entry %q: %w", value, parseErr)
			}
			cfg.BlockedPrefixes = append(cfg.BlockedPrefixes, prefix)
		}
	}
	return cfg, nil
}

func normalizePublicPathPrefix(value string) string {
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	return strings.TrimSuffix(value, "/")
}

func loadSecret(valueEnv, fileEnv string) (string, error) {
	if value := strings.TrimSpace(os.Getenv(valueEnv)); value != "" {
		return value, nil
	}
	path := strings.TrimSpace(os.Getenv(fileEnv))
	if path == "" {
		return "", fmt.Errorf("%s or %s is required", valueEnv, fileEnv)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	return strings.TrimSpace(string(content)), nil
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt64(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDurationMS(name string, fallback int) time.Duration {
	return time.Duration(envInt(name, fallback)) * time.Millisecond
}
