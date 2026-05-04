package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"windows-m3u-stream-merger-proxy/logger"
)

const testPlaylistBody = `#EXTM3U
#EXTINF:-1 tvg-id="test.id" tvg-name="Test Channel",Test Channel
http://example.com/stream
`

func newTestPlaylistHandler(t *testing.T, playlist string) *M3UHTTPHandler {
	t.Helper()

	playlistPath := filepath.Join(t.TempDir(), "playlist.m3u")
	if err := os.WriteFile(playlistPath, []byte(playlist), 0644); err != nil {
		t.Fatalf("write playlist: %v", err)
	}

	return NewM3UHTTPHandler(&logger.DefaultLogger{}, playlistPath)
}

func newRecorderAndRequest(method string) (*httptest.ResponseRecorder, *http.Request) {
	return httptest.NewRecorder(), httptest.NewRequest(method, "/", nil)
}

func TestM3UHTTPHandler_NoAuth(t *testing.T) {
	t.Setenv("CREDENTIALS", "")

	handler := newTestPlaylistHandler(t, testPlaylistBody)
	recorder, request := newRecorderAndRequest(http.MethodGet)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, recorder.Code)
	}
	if !strings.HasPrefix(recorder.Body.String(), "#EXTM3U\n") {
		t.Fatalf("expected playlist body to start with #EXTM3U header, got %q", recorder.Body.String())
	}
}

func TestM3UHTTPHandler_BasicAuth(t *testing.T) {
	tests := []struct {
		name        string
		credentials string
		username    string
		password    string
		wantStatus  int
	}{
		{
			name:        "valid credentials",
			credentials: "user1:pass1|user2:pass2",
			username:    "user1",
			password:    "pass1",
			wantStatus:  http.StatusOK,
		},
		{
			name:        "invalid password",
			credentials: "user1:pass1",
			username:    "user1",
			password:    "wrongpass",
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "invalid username",
			credentials: "user1:pass1",
			username:    "wronguser",
			password:    "pass1",
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "missing credentials",
			credentials: "user1:pass1",
			username:    "",
			password:    "",
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "case insensitive username",
			credentials: "User1:pass1",
			username:    "user1",
			password:    "pass1",
			wantStatus:  http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CREDENTIALS", tt.credentials)

			handler := newTestPlaylistHandler(t, testPlaylistBody)
			recorder, request := newRecorderAndRequest(http.MethodGet)

			query := request.URL.Query()
			query.Set("username", tt.username)
			query.Set("password", tt.password)
			request.URL.RawQuery = query.Encode()

			handler.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d", tt.wantStatus, recorder.Code)
			}
		})
	}
}

func TestM3UHTTPHandler_ExpirationDate(t *testing.T) {
	tomorrow := time.Now().Add(24 * time.Hour).Format(time.DateOnly)
	yesterday := time.Now().Add(-24 * time.Hour).Format(time.DateOnly)

	tests := []struct {
		name        string
		credentials string
		username    string
		password    string
		wantStatus  int
	}{
		{
			name:        "valid credentials with future expiration",
			credentials: "user1:pass1:" + tomorrow,
			username:    "user1",
			password:    "pass1",
			wantStatus:  http.StatusOK,
		},
		{
			name:        "expired credentials",
			credentials: "user1:pass1:" + yesterday,
			username:    "user1",
			password:    "pass1",
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "multiple users with different expiration dates",
			credentials: "user1:pass1:" + yesterday + "|user2:pass2:" + tomorrow,
			username:    "user2",
			password:    "pass2",
			wantStatus:  http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CREDENTIALS", tt.credentials)

			handler := newTestPlaylistHandler(t, testPlaylistBody)
			recorder, request := newRecorderAndRequest(http.MethodGet)

			query := request.URL.Query()
			query.Set("username", tt.username)
			query.Set("password", tt.password)
			request.URL.RawQuery = query.Encode()

			handler.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d", tt.wantStatus, recorder.Code)
			}
		})
	}
}

func TestM3UHTTPHandler_Headers(t *testing.T) {
	handler := newTestPlaylistHandler(t, testPlaylistBody)
	recorder, request := newRecorderAndRequest(http.MethodGet)

	handler.ServeHTTP(recorder, request)

	expectedHeaders := map[string]string{
		"Access-Control-Allow-Origin": "*",
		"Content-Type":                "text/plain; charset=utf-8",
	}

	for header, expectedValue := range expectedHeaders {
		if value := recorder.Header().Get(header); value != expectedValue {
			t.Fatalf("expected header %s to be %s, got %s", header, expectedValue, value)
		}
	}
}

func TestM3UHTTPHandler_RewritesHeaderWithEmbeddedEPGURL(t *testing.T) {
	t.Setenv("EMBEDDED_EPG_URL", "https://epg.example.com/guide.xml.gz")

	handler := newTestPlaylistHandler(t, testPlaylistBody)
	recorder, request := newRecorderAndRequest(http.MethodGet)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, recorder.Code)
	}

	body := recorder.Body.String()
	lines := strings.SplitN(body, "\n", 3)
	if len(lines) < 2 {
		t.Fatalf("expected at least two playlist lines, got %q", body)
	}

	expectedHeader := `#EXTM3U x-tvg-url="https://epg.example.com/guide.xml.gz" url-tvg="https://epg.example.com/guide.xml.gz"`
	if lines[0] != expectedHeader {
		t.Fatalf("expected rewritten header %q, got %q", expectedHeader, lines[0])
	}
	if !strings.Contains(body, `tvg-id="test.id"`) {
		t.Fatalf("expected channel tvg-id to remain present after rewrite, got %q", body)
	}
}

func TestM3UHTTPHandler_StripsStaleEmbeddedHeaderWhenDisabled(t *testing.T) {
	playlist := `#EXTM3U x-tvg-url="https://old.example.com/guide.xml.gz"
#EXTINF:-1 tvg-id="test.id" tvg-name="Test Channel",Test Channel
http://example.com/stream
`

	handler := newTestPlaylistHandler(t, playlist)
	recorder, request := newRecorderAndRequest(http.MethodGet)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, recorder.Code)
	}

	body := recorder.Body.String()
	lines := strings.SplitN(body, "\n", 3)
	if len(lines) < 2 {
		t.Fatalf("expected at least two playlist lines, got %q", body)
	}

	if lines[0] != "#EXTM3U" {
		t.Fatalf("expected plain header when embedded EPG is disabled, got %q", lines[0])
	}
	if strings.Contains(body, "old.example.com") {
		t.Fatalf("expected stale embedded EPG header to be removed, got %q", body)
	}
}

func TestM3UHTTPHandler_HeadRequestOmitsBody(t *testing.T) {
	t.Setenv("EMBEDDED_EPG_URL", "https://epg.example.com/guide.xml")

	handler := newTestPlaylistHandler(t, testPlaylistBody)
	recorder, request := newRecorderAndRequest(http.MethodHead)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected no response body for HEAD request, got %q", recorder.Body.String())
	}
}

func TestM3UHTTPHandler_MissingPlaylistReturnsNotFound(t *testing.T) {
	handler := NewM3UHTTPHandler(&logger.DefaultLogger{}, filepath.Join(t.TempDir(), "missing.m3u"))
	recorder, request := newRecorderAndRequest(http.MethodGet)

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, recorder.Code)
	}
}
