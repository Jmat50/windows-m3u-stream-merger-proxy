package utils

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var hopByHopRequestHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Proxy-Connection":    {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	"Host":                {},
	"Content-Length":      {},
}

var internalOnlyRequestHeaders = map[string]struct{}{
	"Accept-Encoding": {},
	"If-Range":        {},
	"Range":           {},
}

var defaultDialer = &net.Dialer{
	Timeout:   12 * time.Second,
	KeepAlive: 30 * time.Second,
}

var fallbackDNSResolver = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		for _, server := range getFallbackDNSServers() {
			conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "udp", server)
			if err == nil {
				return conn, nil
			}
		}
		return nil, fmt.Errorf("all fallback DNS servers failed")
	},
}

var HTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialContextWithDNSFallback,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		userAgent := GetEnv("USER_AGENT")
		accept := GetEnv("HTTP_ACCEPT")

		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", accept)
		return nil
	},
}

func dialContextWithDNSFallback(ctx context.Context, network, address string) (net.Conn, error) {
	return dialContextWithResolver(ctx, network, address, defaultDialer.DialContext, resolveHostWithFallback)
}

func dialContextWithResolver(
	ctx context.Context,
	network string,
	address string,
	baseDial func(context.Context, string, string) (net.Conn, error),
	resolver func(context.Context, string) ([]string, error),
) (net.Conn, error) {
	conn, err := baseDial(ctx, network, address)
	if err == nil {
		return conn, nil
	}

	host, port, splitErr := net.SplitHostPort(address)
	if splitErr != nil || !shouldTryDNSFallback(err) {
		return nil, err
	}

	ips, resolveErr := resolver(ctx, host)
	if resolveErr != nil || len(ips) == 0 {
		return nil, err
	}

	var lastErr error = err
	for _, ip := range ips {
		target := net.JoinHostPort(ip, port)
		conn, dialErr := baseDial(ctx, network, target)
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}

	return nil, lastErr
}

func resolveHostWithFallback(ctx context.Context, host string) ([]string, error) {
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err == nil && len(ips) > 0 {
		return ips, nil
	}

	if !shouldTryDNSFallback(err) {
		return nil, err
	}

	return fallbackDNSResolver.LookupHost(ctx, host)
}

func shouldTryDNSFallback(err error) bool {
	if err == nil {
		return false
	}

	var dnsErr *net.DNSError
	if ok := errorsAs(err, &dnsErr); ok {
		return dnsErr.IsTemporary || dnsErr.IsTimeout || strings.TrimSpace(dnsErr.Err) != ""
	}

	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "lookup") ||
		strings.Contains(lower, "getaddrinfo") ||
		strings.Contains(lower, "getaddrinfow") ||
		strings.Contains(lower, "temporary") ||
		strings.Contains(lower, "no such host")
}

func getFallbackDNSServers() []string {
	raw := strings.TrimSpace(os.Getenv("FALLBACK_DNS_SERVERS"))
	if raw == "" {
		return []string{"1.1.1.1:53", "8.8.8.8:53"}
	}

	parts := strings.Split(raw, ",")
	servers := make([]string, 0, len(parts))
	for _, part := range parts {
		server := strings.TrimSpace(part)
		if server == "" {
			continue
		}
		if !strings.Contains(server, ":") {
			server = net.JoinHostPort(server, "53")
		}
		servers = append(servers, server)
	}

	if len(servers) == 0 {
		return []string{"1.1.1.1:53", "8.8.8.8:53"}
	}

	return servers
}

// errorsAs is wrapped for deterministic testing/mocking if needed.
var errorsAs = func(err error, target any) bool {
	return errors.As(err, target)
}

func CustomHttpRequest(origReq *http.Request, method string, url string) (*http.Response, error) {
	return customHttpRequest(origReq, method, url, false)
}

func CustomInternalHttpRequest(origReq *http.Request, method string, url string) (*http.Response, error) {
	return customHttpRequest(origReq, method, url, true)
}

func customHttpRequest(origReq *http.Request, method string, url string, internal bool) (*http.Response, error) {
	userAgent := GetEnv("USER_AGENT")
	accept := GetEnv("HTTP_ACCEPT")

	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}

	origHasUA := false
	origHasAccept := false

	if origReq != nil {
		for header, values := range origReq.Header {
			canonicalHeader := http.CanonicalHeaderKey(header)
			if shouldSkipRequestHeader(canonicalHeader, internal) {
				continue
			}

			switch canonicalHeader {
			case "User-Agent":
				origHasUA = true
			case "Accept":
				origHasAccept = true
			}

			for _, v := range values {
				req.Header.Add(header, v)
			}
		}
	}

	if !origHasUA {
		req.Header.Set("User-Agent", userAgent)
	}
	if !origHasAccept {
		req.Header.Set("Accept", accept)
	}

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func shouldSkipRequestHeader(header string, internal bool) bool {
	if _, ok := hopByHopRequestHeaders[header]; ok {
		return true
	}

	if !internal {
		return false
	}

	_, ok := internalOnlyRequestHeaders[header]
	return ok
}

func DetermineBaseURL(r *http.Request) string {
	if customBase, ok := os.LookupEnv("BASE_URL"); ok {
		return strings.TrimSuffix(customBase, "/")
	}

	if r != nil {
		if r.TLS == nil {
			return fmt.Sprintf("http://%s", r.Host)
		} else {
			return fmt.Sprintf("https://%s", r.Host)
		}
	}

	return ""
}
