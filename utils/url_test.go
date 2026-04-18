package utils

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsProbablyM3U8_WithPlaylistResponse(t *testing.T) {
	playlist := `#EXTM3U
#EXT-X-TARGETDURATION:10
#EXTINF:10.0,
segment1.ts
#EXTINF:10.0,
segment2.ts
#EXT-X-ENDLIST`

	req := httptest.NewRequest(http.MethodGet, "https://tvpass.org/live/Boomerang/sd", nil)
	resp := &http.Response{
		StatusCode: http.StatusPartialContent,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(playlist)),
		Request:    req,
	}
	resp.Header.Set("Content-Type", "application/vnd.apple.mpegurl")

	if !IsProbablyM3U8(resp) {
		t.Fatalf("IsProbablyM3U8() = false, want true for playlist response")
	}

	if IsProbablyMedia(resp) {
		t.Fatalf("IsProbablyMedia() = true, want false for playlist response")
	}
}

func TestIsProbablyMedia_WithMediaResponse(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusPartialContent,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("media data")),
		Request:    httptest.NewRequest(http.MethodGet, "https://tvpass.org/media/segment.ts", nil),
	}
	resp.Header.Set("Content-Type", "video/mp2t")

	if !IsProbablyMedia(resp) {
		t.Fatalf("IsProbablyMedia() = false, want true for media response")
	}
}
