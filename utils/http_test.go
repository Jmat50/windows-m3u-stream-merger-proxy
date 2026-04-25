package utils

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCustomInternalHttpRequest_StripsRangeAndHopByHopHeaders(t *testing.T) {
	receivedHeaders := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	originalClient := HTTPClient
	HTTPClient = server.Client()
	defer func() {
		HTTPClient = originalClient
	}()

	origReq := httptest.NewRequest(http.MethodGet, "http://client.test/watch", nil)
	origReq.Header.Set("User-Agent", "VLC/3.0")
	origReq.Header.Set("Accept", "*/*")
	origReq.Header.Set("Authorization", "Bearer token")
	origReq.Header.Set("Cookie", "session=test")
	origReq.Header.Set("Referer", "http://client.test/")
	origReq.Header.Set("Range", "bytes=0-")
	origReq.Header.Set("If-Range", `"etag"`)
	origReq.Header.Set("Accept-Encoding", "identity")
	origReq.Header.Set("Connection", "keep-alive")
	origReq.Header.Set("Proxy-Connection", "keep-alive")
	origReq.Header.Set("Keep-Alive", "timeout=5")

	resp, err := CustomInternalHttpRequest(origReq, http.MethodGet, server.URL)
	if err != nil {
		t.Fatalf("CustomInternalHttpRequest() error = %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	headers := <-receivedHeaders
	if got := headers.Get("Range"); got != "" {
		t.Fatalf("Range header = %q, want empty", got)
	}
	if got := headers.Get("If-Range"); got != "" {
		t.Fatalf("If-Range header = %q, want empty", got)
	}
	if got := headers.Get("Accept-Encoding"); got == "identity" {
		t.Fatalf("Accept-Encoding header = %q, want original client header to be stripped", got)
	}
	if got := headers.Get("Connection"); got != "" {
		t.Fatalf("Connection header = %q, want empty", got)
	}
	if got := headers.Get("Proxy-Connection"); got != "" {
		t.Fatalf("Proxy-Connection header = %q, want empty", got)
	}
	if got := headers.Get("Keep-Alive"); got != "" {
		t.Fatalf("Keep-Alive header = %q, want empty", got)
	}

	if got := headers.Get("Authorization"); got != "Bearer token" {
		t.Fatalf("Authorization header = %q, want %q", got, "Bearer token")
	}
	if got := headers.Get("Cookie"); got != "session=test" {
		t.Fatalf("Cookie header = %q, want %q", got, "session=test")
	}
	if got := headers.Get("Referer"); got != "http://client.test/" {
		t.Fatalf("Referer header = %q, want %q", got, "http://client.test/")
	}
	if got := headers.Get("User-Agent"); got != "VLC/3.0" {
		t.Fatalf("User-Agent header = %q, want %q", got, "VLC/3.0")
	}
	if got := headers.Get("Accept"); got != "*/*" {
		t.Fatalf("Accept header = %q, want %q", got, "*/*")
	}
}

func TestCustomHttpRequest_PreservesRangeForForwardProxyRequests(t *testing.T) {
	receivedHeaders := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders <- r.Header.Clone()
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	originalClient := HTTPClient
	HTTPClient = server.Client()
	defer func() {
		HTTPClient = originalClient
	}()

	origReq := httptest.NewRequest(http.MethodGet, "http://client.test/segment", nil)
	origReq.Header.Set("Range", "bytes=0-1023")
	origReq.Header.Set("Accept-Encoding", "identity")
	origReq.Header.Set("Connection", "keep-alive")

	resp, err := CustomHttpRequest(origReq, http.MethodGet, server.URL)
	if err != nil {
		t.Fatalf("CustomHttpRequest() error = %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	headers := <-receivedHeaders
	if got := headers.Get("Range"); got != "bytes=0-1023" {
		t.Fatalf("Range header = %q, want %q", got, "bytes=0-1023")
	}
	if got := headers.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("Accept-Encoding header = %q, want %q", got, "identity")
	}
	if got := headers.Get("Connection"); got != "" {
		t.Fatalf("Connection header = %q, want empty", got)
	}
}

func TestDialContextWithResolver_FallsBackToResolvedIP(t *testing.T) {
	t.Helper()

	dialAttempts := make([]string, 0, 4)
	baseDial := func(_ context.Context, _ string, address string) (net.Conn, error) {
		dialAttempts = append(dialAttempts, address)
		if address == "10.10.10.10:443" {
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		}
		return nil, &net.DNSError{Err: "getaddrinfow temporary failure", Name: "cdn.example.com", IsTemporary: true}
	}

	resolver := func(_ context.Context, host string) ([]string, error) {
		if host != "cdn.example.com" {
			return nil, errors.New("unexpected host")
		}
		return []string{"10.10.10.10"}, nil
	}

	conn, err := dialContextWithResolver(context.Background(), "tcp", "cdn.example.com:443", baseDial, resolver)
	if err != nil {
		t.Fatalf("dialContextWithResolver() error = %v", err)
	}
	if conn == nil {
		t.Fatal("dialContextWithResolver() returned nil conn")
	}
	_ = conn.Close()
}

func TestGetFallbackDNSServers_FromEnv(t *testing.T) {
	t.Setenv("FALLBACK_DNS_SERVERS", "9.9.9.9, 1.0.0.1:53")
	servers := getFallbackDNSServers()
	if len(servers) != 2 {
		t.Fatalf("getFallbackDNSServers() len = %d, want 2", len(servers))
	}
	if servers[0] != "9.9.9.9:53" {
		t.Fatalf("getFallbackDNSServers()[0] = %q, want %q", servers[0], "9.9.9.9:53")
	}
	if servers[1] != "1.0.0.1:53" {
		t.Fatalf("getFallbackDNSServers()[1] = %q, want %q", servers[1], "1.0.0.1:53")
	}
}
