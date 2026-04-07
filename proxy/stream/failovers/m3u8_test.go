package failovers

import (
	"io"
	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/proxy/client"
	"windows-m3u-stream-merger-proxy/proxy/loadbalancer"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessM3U8Stream_FlattensMasterPlaylistToSingleVariant(t *testing.T) {
	proxyBase := "http://proxy.test"
	require.NoError(t, os.Setenv("BASE_URL", proxyBase))
	defer os.Unsetenv("BASE_URL")

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		switch r.URL.Path {
		case "/master.m3u8":
			_, _ = io.WriteString(w, `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=250000
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1200000
high.m3u8
`)
		case "/low.m3u8":
			_, _ = io.WriteString(w, `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXTINF:6,
seg-low-1.ts
#EXTINF:6,
seg-low-2.ts
`)
		case "/high.m3u8":
			_, _ = io.WriteString(w, `#EXTM3U
#EXT-X-TARGETDURATION:6
#EXTINF:6,
seg-high-1.ts
`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	originReq := httptest.NewRequest(http.MethodGet, "http://test-client.local/p/channel", nil)
	recorder := httptest.NewRecorder()
	streamClient := client.NewStreamClient(recorder, originReq)

	resp, err := http.Get(server.URL + "/master.m3u8")
	require.NoError(t, err)

	lbResult := &loadbalancer.LoadBalancerResult{
		Response: resp,
		Index:    "1",
		SubIndex: "a",
	}

	processor := NewM3U8Processor(&logger.DefaultLogger{})
	require.NoError(t, processor.ProcessM3U8Stream(lbResult, streamClient))

	body := recorder.Body.String()
	assert.NotContains(t, body, "#EXT-X-STREAM-INF", "master variants should not be passed through to clients")
	assert.Contains(t, body, "#EXTINF:6,", "flattened media playlist should be returned")
	assert.Contains(t, body, "/segment/", "segments should still be proxied through segment endpoint")
	assert.NotContains(t, body, "seg-high-1.ts", "only the first variant should be followed")

	segmentLines := extractSegmentLines(body)
	require.GreaterOrEqual(t, len(segmentLines), 2)

	firstSegment := decodeSegmentFromProxyURL(t, segmentLines[0])
	secondSegment := decodeSegmentFromProxyURL(t, segmentLines[1])
	assert.Equal(t, server.URL+"/seg-low-1.ts", firstSegment.URL)
	assert.Equal(t, server.URL+"/seg-low-2.ts", secondSegment.URL)
	assert.Equal(t, "1|a", firstSegment.SourceM3U)
	assert.Equal(t, "1|a", secondSegment.SourceM3U)
}

func TestProcessM3U8Stream_RewritesMediaSegments(t *testing.T) {
	proxyBase := "http://proxy.test"
	require.NoError(t, os.Setenv("BASE_URL", proxyBase))
	defer os.Unsetenv("BASE_URL")

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		if r.URL.Path == "/media.m3u8" {
			_, _ = io.WriteString(w, `#EXTM3U
#EXT-X-TARGETDURATION:4
#EXTINF:4,
segment-a.ts
#EXTINF:4,
segment-b.ts
`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	originReq := httptest.NewRequest(http.MethodGet, "http://test-client.local/p/channel", nil)
	recorder := httptest.NewRecorder()
	streamClient := client.NewStreamClient(recorder, originReq)

	resp, err := http.Get(server.URL + "/media.m3u8")
	require.NoError(t, err)

	lbResult := &loadbalancer.LoadBalancerResult{
		Response: resp,
		Index:    "2",
		SubIndex: "b",
	}

	processor := NewM3U8Processor(&logger.DefaultLogger{})
	require.NoError(t, processor.ProcessM3U8Stream(lbResult, streamClient))

	body := recorder.Body.String()
	assert.NotContains(t, body, "#EXT-X-STREAM-INF")
	segmentLines := extractSegmentLines(body)
	require.Len(t, segmentLines, 2)

	segA := decodeSegmentFromProxyURL(t, segmentLines[0])
	segB := decodeSegmentFromProxyURL(t, segmentLines[1])
	assert.Equal(t, server.URL+"/segment-a.ts", segA.URL)
	assert.Equal(t, server.URL+"/segment-b.ts", segB.URL)
	assert.Equal(t, "2|b", segA.SourceM3U)
	assert.Equal(t, "2|b", segB.SourceM3U)
}

func extractSegmentLines(playlist string) []string {
	lines := strings.Split(playlist, "\n")
	segments := make([]string, 0, len(lines))
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		segments = append(segments, line)
	}
	return segments
}

func decodeSegmentFromProxyURL(t *testing.T, proxyURL string) *M3U8Segment {
	t.Helper()

	parsed, err := url.Parse(proxyURL)
	require.NoError(t, err)

	base := path.Base(parsed.Path)
	slug := strings.SplitN(base, ".", 2)[0]
	require.NotEmpty(t, slug)

	segment, err := ParseSegmentId(slug)
	require.NoError(t, err)
	return segment
}


