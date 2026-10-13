// Package main tests the cloud-side Web Fetch worker.
package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig() config {
	return config{
		ListenAddr: "127.0.0.1:0", AuthToken: strings.Repeat("a", 32),
		DefaultTimeout: time.Second, MaxTimeout: 2 * time.Second,
		ConnectTimeout: time.Second, ResponseHeaderTimeout: time.Second,
		DefaultMaxBytes: 1024, HardMaxBytes: 2048,
		DefaultMaxRedirects: 2, HardMaxRedirects: 5, MaxConcurrency: 2,
		ProxyListenAddr: "127.0.0.1:0", ProxyMaxConcurrency: 2, ProxyMaxStreamBytes: 4096,
	}
}

func TestValidateURLSyntax(t *testing.T) {
	tests := []struct {
		url     string
		blocked bool
	}{
		{"https://example.com/path", false},
		{"http://example.com:80", false},
		{"file:///etc/passwd", true},
		{"https://user:pass@example.com", true},
		{"https://example.com:22", true},
	}
	for _, test := range tests {
		t.Run(test.url, func(t *testing.T) {
			u, _ := url.Parse(test.url)
			err := validateURLSyntax(u)
			if (err != nil) != test.blocked {
				t.Fatalf("validateURLSyntax() error = %v", err)
			}
		})
	}
}

func TestBlockedAddresses(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "192.168.1.1", "::1", "fc00::1", "2001:db8::1"}
	for _, value := range blocked {
		if !isBlockedIP(netip.MustParseAddr(value), nil) {
			t.Errorf("expected %s to be blocked", value)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"}
	for _, value := range allowed {
		if isBlockedIP(netip.MustParseAddr(value), nil) {
			t.Errorf("expected %s to be allowed", value)
		}
	}
}

type fakeResolver struct{ ips []netip.Addr }

func (r fakeResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r.ips, nil
}

type recordingDialer struct{ address string }

func (d *recordingDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.address = address
	return nil, context.Canceled
}

func TestSafeDialerRejectsMixedDNSAnswers(t *testing.T) {
	recording := &recordingDialer{}
	d := safeDialer{
		resolver: fakeResolver{ips: []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("127.0.0.1")}},
		dialer:   recording,
	}
	_, err := d.DialContext(context.Background(), "tcp", "example.com:443")
	if err == nil || !strings.Contains(err.Error(), "prohibited") {
		t.Fatalf("expected prohibited address error, got %v", err)
	}
	if recording.address != "" {
		t.Fatalf("dial must not be attempted, got %s", recording.address)
	}
}

func TestFetchRequiresAuthentication(t *testing.T) {
	s := newServer(testConfig())
	req := httptest.NewRequest(http.MethodPost, "/v1/fetch", strings.NewReader(`{"url":"https://example.com"}`))
	res := httptest.NewRecorder()
	s.fetch(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Code)
	}
}

func TestNormalizeRequestDefaultsAndHeaderPolicy(t *testing.T) {
	s := newServer(testConfig())
	normalized, err := s.normalizeRequest(fetchRequest{URL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Method != http.MethodGet || !normalized.followRedirects || normalized.maxBytes != 1024 {
		t.Fatalf("unexpected defaults: %+v", normalized)
	}
	_, err = s.normalizeRequest(fetchRequest{URL: "https://example.com", Headers: map[string]string{"Authorization": "secret"}})
	if err == nil {
		t.Fatal("expected Authorization header to be rejected")
	}
}

func TestInvalidJSON(t *testing.T) {
	s := newServer(testConfig())
	req := httptest.NewRequest(http.MethodPost, "/v1/fetch", strings.NewReader(`{"url":"https://example.com","unknown":true}`))
	req.Header.Set("Authorization", "Bearer "+testConfig().AuthToken)
	res := httptest.NewRecorder()
	s.fetch(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	var body apiError
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "INVALID_REQUEST" {
		t.Fatalf("code = %s", body.Error.Code)
	}
}

func TestApplicationLoggerWritesMappedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.log")
	logger, closeLog, err := newApplicationLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("mapped log test", "request_id", "test-request")
	if err := closeLog(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"msg":"mapped log test"`) || !strings.Contains(string(content), `"request_id":"test-request"`) {
		t.Fatalf("unexpected log content: %s", content)
	}
}
