package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/proxy"
	"windows-m3u-stream-merger-proxy/proxy/client"
	"windows-m3u-stream-merger-proxy/proxy/loadbalancer"
	"windows-m3u-stream-merger-proxy/proxy/stream/buffer"
	"windows-m3u-stream-merger-proxy/proxy/stream/config"
	"windows-m3u-stream-merger-proxy/store"
	"windows-m3u-stream-merger-proxy/utils"
)

type mockResponseWriter struct {
	written     []byte
	statusCode  int
	headersSent http.Header
	err         error
	mu          sync.Mutex
}

func (m *mockResponseWriter) Header() http.Header {
	if m.headersSent == nil {
		m.headersSent = make(http.Header)
	}
	return m.headersSent
}

func (m *mockResponseWriter) Write(data []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.written = append(m.written, data...)
	return len(data), nil
}

func (m *mockResponseWriter) WriteHeader(statusCode int) {
	m.statusCode = statusCode
}

func (m *mockResponseWriter) Flush() {
	// Mock implementation
}

type eofAfterDataReader struct {
	data []byte
	read bool
}

func (r *eofAfterDataReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}

	r.read = true
	n := copy(p, r.data)
	return n, io.EOF
}

type blockingReadCloser struct {
	closed  chan struct{}
	first   []byte
	started chan struct{}
	read    bool
	mu      sync.Mutex
}

func newBlockingReadCloser(first []byte) *blockingReadCloser {
	return &blockingReadCloser{
		closed:  make(chan struct{}),
		first:   append([]byte(nil), first...),
		started: make(chan struct{}),
	}
}

func (r *blockingReadCloser) Read(p []byte) (int, error) {
	r.mu.Lock()
	if !r.read {
		r.read = true
		r.mu.Unlock()
		close(r.started)
		n := copy(p, r.first)
		return n, nil
	}
	r.mu.Unlock()
	<-r.closed
	return 0, io.EOF
}

func (r *blockingReadCloser) Close() error {
	select {
	case <-r.closed:
	default:
		close(r.closed)
	}
	return nil
}

type mockHLSServer struct {
	server        *httptest.Server
	mediaPlaylist string
	segments      map[string][]byte
	logger        logger.Logger
}

func newMockHLSServer() *mockHLSServer {
	m := &mockHLSServer{
		segments: make(map[string][]byte),
		logger:   logger.Default,
	}

	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check for request context cancellation
		done := make(chan struct{})
		go func() {
			<-r.Context().Done()
			close(done)
		}()

		select {
		case <-done:
			m.logger.Debug("Request context cancelled")
			return
		default:
			m.handleRequest(w, r)
		}
	}))

	return m
}

func (m *mockHLSServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	m.logger.Debugf("Mock server received request: %s", r.URL.Path)

	switch {
	case strings.HasSuffix(r.URL.Path, ".m3u8"):
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte(m.mediaPlaylist))

	case strings.HasSuffix(r.URL.Path, ".ts"):
		segmentKey := r.URL.Path
		if !strings.HasPrefix(segmentKey, "/") {
			segmentKey = "/" + segmentKey
		}
		if data, ok := m.segments[segmentKey]; ok {
			w.Header().Set("Content-Type", "video/MP2T")
			_, _ = w.Write(data)
		} else {
			m.logger.Errorf("Segment not found: %s", segmentKey)
			w.WriteHeader(http.StatusNotFound)
		}

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (m *mockHLSServer) Close() {
	if m.server != nil {
		m.server.Close()
	}
}

func TestM3U8StreamHandler_HandleStream(t *testing.T) {
	segment1Data := []byte("TESTSEGMNT1!")
	segment2Data := []byte("TESTSEGMNT2!")

	tests := []struct {
		name           string
		config         *config.StreamConfig
		setupMock      func(*mockHLSServer)
		writeError     error
		expectedResult StreamResult
	}{
		{
			name: "successful media playlist",
			config: &config.StreamConfig{
				TimeoutSeconds:   5,
				ChunkSize:        1024,
				SharedBufferSize: 5,
			},
			setupMock: func(m *mockHLSServer) {
				m.mediaPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXTINF:10.0,
/segment1.ts
#EXTINF:10.0,
/segment2.ts
#EXT-X-ENDLIST`
				m.segments["/segment1.ts"] = segment1Data
				m.segments["/segment2.ts"] = segment2Data
			},
			writeError: nil,
			expectedResult: StreamResult{
				BytesWritten: 24, // Two segments of exactly 12 bytes each
				Error:        io.EOF,
				Status:       proxy.StatusEOF,
			},
		},
		{
			name: "write error during streaming",
			config: &config.StreamConfig{
				TimeoutSeconds:   5,
				ChunkSize:        1024,
				SharedBufferSize: 5,
			},
			setupMock: func(m *mockHLSServer) {
				m.mediaPlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXTINF:10.0,
/segment1.ts
#EXT-X-ENDLIST`
				m.segments["/segment1.ts"] = segment1Data
			},
			writeError: errors.New("write error"),
			expectedResult: StreamResult{
				BytesWritten: 0,
				Error:        errors.New("write error"),
				Status:       proxy.StatusClientClosed,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a context with timeout that's slightly shorter than the test timeout
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()

			mockServer := newMockHLSServer()
			defer mockServer.Close()

			tt.setupMock(mockServer)

			cm := store.NewConcurrencyManager()
			coordinator := buffer.NewStreamCoordinator("test_id", tt.config, cm, logger.Default)

			// Make first request with short timeout
			reqCtx, reqCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer reqCancel()

			req, _ := http.NewRequestWithContext(reqCtx, "GET", mockServer.server.URL+"/playlist.m3u8", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Failed to get mock response: %v", err)
			}

			handler := NewStreamHandler(tt.config, coordinator, logger.Default)
			writer := &mockResponseWriter{err: tt.writeError}
			lbRes := loadbalancer.LoadBalancerResult{Response: resp, Index: "1"}

			streamClient := client.NewStreamClient(writer, req)

			result := handler.HandleStream(ctx, &lbRes, streamClient)

			if result.Status != tt.expectedResult.Status {
				t.Errorf("HandleStream() status = %v, want %v", result.Status, tt.expectedResult.Status)
			}
			if result.BytesWritten != tt.expectedResult.BytesWritten {
				t.Errorf("HandleStream() bytesWritten = %v, want %v", result.BytesWritten, tt.expectedResult.BytesWritten)
			}
			if tt.expectedResult.Error != nil {
				if result.Error == nil || !strings.Contains(result.Error.Error(), tt.expectedResult.Error.Error()) {
					t.Errorf("HandleStream() error = %v, want error containing %v", result.Error, tt.expectedResult.Error)
				}
			}
		})
	}
}

func TestM3U8StreamHandler_HandleStream_UsesInitialMasterPlaylistImmediately(t *testing.T) {
	cfg := &config.StreamConfig{
		TimeoutSeconds:   5,
		ChunkSize:        1024,
		SharedBufferSize: 5,
	}

	var masterHits atomic.Int32
	var variantHits atomic.Int32
	segmentData := []byte("TESTSEGMNT1!")

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			masterHits.Add(1)
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=250000\n%s/variant.m3u8\n", server.URL)
		case "/variant.m3u8":
			variantHits.Add(1)
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\n/segment1.ts\n#EXT-X-ENDLIST\n")
		case "/segment1.ts":
			w.Header().Set("Content-Type", "video/MP2T")
			_, _ = w.Write(segmentData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	resp, err := http.Get(server.URL + "/master.m3u8")
	if err != nil {
		t.Fatalf("http.Get() error = %v", err)
	}

	cm := store.NewConcurrencyManager()
	coordinator := buffer.NewStreamCoordinator("test_id", cfg, cm, logger.Default)
	handler := NewStreamHandler(cfg, coordinator, logger.Default)

	writer := &mockResponseWriter{}
	req := httptest.NewRequest(http.MethodGet, "http://proxy.test/p/channel", nil)
	streamClient := client.NewStreamClient(writer, req)

	lbRes := &loadbalancer.LoadBalancerResult{
		Response: resp,
		URL:      server.URL + "/master.m3u8",
		Index:    "DISC_1_TEST",
		SubIndex: "a",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()

	result := handler.HandleStream(ctx, lbRes, streamClient)
	if result.Status != proxy.StatusEOF {
		t.Fatalf("HandleStream() status = %v, want %v", result.Status, proxy.StatusEOF)
	}
	if !bytes.Equal(writer.written, segmentData) {
		t.Fatalf("HandleStream() wrote %q, want %q", writer.written, segmentData)
	}
	if got := masterHits.Load(); got != 1 {
		t.Fatalf("master playlist fetched %d time(s), want 1", got)
	}
	if got := variantHits.Load(); got != 1 {
		t.Fatalf("variant playlist fetched %d time(s), want 1", got)
	}
}

func TestM3U8StreamHandler_HandleStream_StripsClientRangeForInternalFetches(t *testing.T) {
	var playlistRangeSeen atomic.Bool
	var segmentRangeSeen atomic.Bool

	segmentData := []byte("TESTSEGMNT1!")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			switch r.URL.Path {
			case "/playlist.m3u8":
				playlistRangeSeen.Store(true)
			case "/segment1.ts":
				segmentRangeSeen.Store(true)
			}
		}

		switch r.URL.Path {
		case "/playlist.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			if r.Header.Get("Range") != "" {
				w.WriteHeader(http.StatusPartialContent)
			}
			_, _ = fmt.Fprintf(w, `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:2
#EXTINF:2.0,
%s/segment1.ts
#EXT-X-ENDLIST`, server.URL)
		case "/segment1.ts":
			w.Header().Set("Content-Type", "video/MP2T")
			if r.Header.Get("Range") != "" {
				w.WriteHeader(http.StatusPartialContent)
			}
			_, _ = w.Write(segmentData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.NewDefaultStreamConfig()
	cfg.TimeoutSeconds = 5
	cm := store.NewConcurrencyManager()
	coordinator := buffer.NewStreamCoordinator("test_id", cfg, cm, logger.Default)
	handler := NewStreamHandler(cfg, coordinator, logger.Default)

	initialReq, _ := http.NewRequest(http.MethodGet, server.URL+"/playlist.m3u8", nil)
	initialResp, err := http.DefaultClient.Do(initialReq)
	if err != nil {
		t.Fatalf("Failed to get initial playlist response: %v", err)
	}

	clientReq := httptest.NewRequest(http.MethodGet, "http://proxy.test/p/channel.m3u8", nil)
	clientReq.Header.Set("Range", "bytes=0-")
	clientReq.Header.Set("If-Range", `"etag"`)

	writer := &mockResponseWriter{}
	streamClient := client.NewStreamClient(writer, clientReq)

	lbRes := &loadbalancer.LoadBalancerResult{
		Response: initialResp,
		Index:    "1",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := handler.HandleStream(ctx, lbRes, streamClient)

	if result.Status != proxy.StatusEOF {
		t.Fatalf("HandleStream() status = %v, want %v", result.Status, proxy.StatusEOF)
	}
	if playlistRangeSeen.Load() {
		t.Fatal("playlist polling unexpectedly forwarded client Range header")
	}
	if segmentRangeSeen.Load() {
		t.Fatal("segment fetching unexpectedly forwarded client Range header")
	}
}

func TestM3U8StreamHandler_HandleStream_Accepts206ForPlaylistAndSegments(t *testing.T) {
	segmentData := []byte("TESTSEGMNT1!")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/playlist.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = fmt.Fprintf(w, `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:2
#EXTINF:2.0,
%s/segment1.ts
#EXT-X-ENDLIST`, server.URL)
		case "/segment1.ts":
			w.Header().Set("Content-Type", "video/MP2T")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(segmentData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.NewDefaultStreamConfig()
	cfg.TimeoutSeconds = 5
	cm := store.NewConcurrencyManager()
	coordinator := buffer.NewStreamCoordinator("test_id", cfg, cm, logger.Default)
	handler := NewStreamHandler(cfg, coordinator, logger.Default)

	initialReq, _ := http.NewRequest(http.MethodGet, server.URL+"/playlist.m3u8", nil)
	initialResp, err := http.DefaultClient.Do(initialReq)
	if err != nil {
		t.Fatalf("Failed to get initial playlist response: %v", err)
	}

	clientReq := httptest.NewRequest(http.MethodGet, "http://proxy.test/p/channel.m3u8", nil)
	writer := &mockResponseWriter{}
	streamClient := client.NewStreamClient(writer, clientReq)

	lbRes := &loadbalancer.LoadBalancerResult{
		Response: initialResp,
		Index:    "1",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := handler.HandleStream(ctx, lbRes, streamClient)

	if result.Status != proxy.StatusEOF {
		t.Fatalf("HandleStream() status = %v, want %v", result.Status, proxy.StatusEOF)
	}
	if !bytes.Equal(writer.written, segmentData) {
		t.Fatalf("HandleStream() wrote %q, want %q", writer.written, segmentData)
	}
}

// Test StreamHandler
func TestStreamHandler_HandleStream(t *testing.T) {
	tests := []struct {
		name           string
		config         *config.StreamConfig
		responseBody   string
		responseStatus int
		writeError     error
		expectedResult StreamResult
	}{
		{
			name: "successful stream",
			config: &config.StreamConfig{
				TimeoutSeconds:   5,
				ChunkSize:        1024,
				SharedBufferSize: 5,
			},
			responseBody:   "test content",
			responseStatus: http.StatusOK,
			writeError:     nil,
			expectedResult: StreamResult{
				BytesWritten: 12,
				Error:        io.EOF,
				Status:       proxy.StatusEOF,
			},
		},
		{
			name: "write error",
			config: &config.StreamConfig{
				TimeoutSeconds:   5,
				ChunkSize:        1024,
				SharedBufferSize: 5,
			},
			responseBody:   "test content",
			responseStatus: http.StatusOK,
			writeError:     errors.New("write error"),
			expectedResult: StreamResult{
				BytesWritten: 0,
				Error:        errors.New("write error"),
				Status:       0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			config := config.NewDefaultStreamConfig()
			cm := store.NewConcurrencyManager()
			coordinator := buffer.NewStreamCoordinator("test_id", config, cm, logger.Default)
			handler := NewStreamHandler(tt.config, coordinator, logger.Default)

			resp := &http.Response{
				StatusCode: tt.responseStatus,
				Body:       io.NopCloser(strings.NewReader(tt.responseBody)),
				Header:     make(http.Header),
			}
			resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(tt.responseBody)))

			writer := &mockResponseWriter{err: tt.writeError}
			lbRes := loadbalancer.LoadBalancerResult{Response: resp, Index: "1"}

			streamClient := client.NewStreamClient(writer, nil)
			result := handler.HandleStream(ctx, &lbRes, streamClient)

			if result.Status != tt.expectedResult.Status {
				t.Errorf("HandleStream() status = %v, want %v", result.Status, tt.expectedResult.Status)
			}
			if result.BytesWritten != tt.expectedResult.BytesWritten {
				t.Errorf("HandleStream() bytesWritten = %v, want %v", result.BytesWritten, tt.expectedResult.BytesWritten)
			}
			if (result.Error != nil) != (tt.expectedResult.Error != nil) {
				t.Errorf("HandleStream() error = %v, want %v", result.Error, tt.expectedResult.Error)
			}
		})
	}
}

func TestStreamInstance_ProxyStream(t *testing.T) {
	segment1Data := []byte("TESTSEGMNT1!")
	segment2Data := []byte("TESTSEGMNT2!")

	tests := []struct {
		name           string
		method         string
		contentType    string
		setupMock      func(*mockHLSServer)
		forceStatus    int
		expectedStatus int
	}{
		{
			name:        "handle m3u8 stream",
			method:      http.MethodGet,
			contentType: "application/vnd.apple.mpegurl",
			setupMock: func(m *mockHLSServer) {
				m.mediaPlaylist = fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXTINF:10.0,
%s/segment1.ts
#EXTINF:10.0,
%s/segment2.ts
#EXT-X-ENDLIST`, m.server.URL, m.server.URL)

				m.segments["/segment1.ts"] = segment1Data
				m.segments["/segment2.ts"] = segment2Data
			},
			expectedStatus: proxy.StatusEOF,
		},
		{
			name:        "handle media stream",
			method:      http.MethodGet,
			contentType: "video/MP2T",
			setupMock: func(m *mockHLSServer) {
				m.segments["/media"] = []byte("media content!")
			},
			expectedStatus: proxy.StatusEOF,
		},
		{
			name:        "handle m3u8 stream with 206 status",
			method:      http.MethodGet,
			contentType: "application/vnd.apple.mpegurl",
			forceStatus: http.StatusPartialContent,
			setupMock: func(m *mockHLSServer) {
				m.mediaPlaylist = fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXTINF:10.0,
%s/segment1.ts
#EXTINF:10.0,
%s/segment2.ts
#EXT-X-ENDLIST`, m.server.URL, m.server.URL)

				m.segments["/segment1.ts"] = segment1Data
				m.segments["/segment2.ts"] = segment2Data
			},
			expectedStatus: proxy.StatusEOF,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockServer := newMockHLSServer()
			defer mockServer.Close()

			tt.setupMock(mockServer)

			config := config.NewDefaultStreamConfig()
			cm := store.NewConcurrencyManager()
			coordinator := buffer.NewStreamCoordinator("test_id", config, cm, logger.Default)

			instance, err := NewStreamInstance(cm, config,
				WithLogger(logger.Default))
			if err != nil {
				t.Fatalf("Failed to create StreamInstance: %v", err)
			}

			path := "/playlist.m3u8"
			if tt.contentType == "video/MP2T" {
				path = "/media"
			}

			req, _ := http.NewRequest(tt.method, mockServer.server.URL+path, nil)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Request:    req,
			}
			resp.Header.Set("Content-Type", tt.contentType)

			if tt.contentType == "video/MP2T" {
				resp.Body = io.NopCloser(bytes.NewReader(mockServer.segments["/media"]))
			} else if tt.contentType == "application/vnd.apple.mpegurl" {
				resp, err = http.Get(mockServer.server.URL + path)
				if err != nil {
					t.Fatalf("Failed to get mock response: %v", err)
				}
				if tt.forceStatus != 0 {
					resp.StatusCode = tt.forceStatus
				}
			} else {
				resp, err = http.Get(mockServer.server.URL + path)
				if err != nil {
					t.Fatalf("Failed to get mock response: %v", err)
				}
			}

			writer := &mockResponseWriter{}
			statusChan := make(chan int, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			streamClient := client.NewStreamClient(writer, req)

			lbRes := loadbalancer.LoadBalancerResult{Response: resp, Index: "1"}
			instance.ProxyStream(ctx, coordinator, &lbRes, streamClient, statusChan)

			select {
			case status := <-statusChan:
				if status != tt.expectedStatus {
					t.Errorf("ProxyStream() status = %v, want %v", status, tt.expectedStatus)
				}
			case <-time.After(5 * time.Second):
				t.Error("ProxyStream() timed out waiting for status")
			}
		})
	}
}

func TestStreamHandler_HandleDirectStream_WritesFinalChunkAndCompletes(t *testing.T) {
	cfg := config.NewDefaultStreamConfig()
	handler := NewStreamHandler(cfg, nil, logger.Default)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/media.ts", nil)
	writer := &mockResponseWriter{}
	streamClient := client.NewStreamClient(writer, req)

	payload := []byte("final direct chunk")
	resp := &http.Response{
		StatusCode: http.StatusPartialContent,
		Header:     make(http.Header),
		Body: io.NopCloser(&eofAfterDataReader{
			data: payload,
		}),
	}

	lbRes := &loadbalancer.LoadBalancerResult{
		Response: resp,
		URL:      "http://origin.example/media.ts",
		Index:    "1",
	}

	result := handler.HandleDirectStream(context.Background(), lbRes, streamClient)

	if result.Status != proxy.StatusCompleted {
		t.Fatalf("HandleDirectStream() status = %v, want %v", result.Status, proxy.StatusCompleted)
	}
	if result.Error != nil {
		t.Fatalf("HandleDirectStream() error = %v, want nil", result.Error)
	}
	if result.BytesWritten != int64(len(payload)) {
		t.Fatalf("HandleDirectStream() bytesWritten = %v, want %v", result.BytesWritten, len(payload))
	}
	if !bytes.Equal(writer.written, payload) {
		t.Fatalf("HandleDirectStream() wrote %q, want %q", writer.written, payload)
	}
}

func TestStreamInstance_ProxyStream_DoesNotTreat206LiveMediaAsVODWhenSourceDisablesVOD(t *testing.T) {
	utils.ResetCaches()
	defer utils.ResetCaches()

	_ = os.Unsetenv("M3U_URL_1")
	_ = os.Unsetenv("M3U_CONTAINS_VOD_1")
	defer func() {
		_ = os.Unsetenv("M3U_URL_1")
		_ = os.Unsetenv("M3U_CONTAINS_VOD_1")
	}()

	_ = os.Setenv("M3U_URL_1", "http://example.com/live")
	_ = os.Setenv("M3U_CONTAINS_VOD_1", "false")

	cfg := config.NewDefaultStreamConfig()
	cm := store.NewConcurrencyManager()
	coordinator := buffer.NewStreamCoordinator("test_id", cfg, cm, logger.Default)

	instance, err := NewStreamInstance(cm, cfg, WithLogger(logger.Default))
	if err != nil {
		t.Fatalf("Failed to create StreamInstance: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/live", nil)
	resp := &http.Response{
		StatusCode: http.StatusPartialContent,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader([]byte("live transport stream bytes"))),
		Request: &http.Request{
			URL: req.URL,
		},
	}
	resp.Header.Set("Content-Type", "video/MP2T")

	writer := &mockResponseWriter{}
	streamClient := client.NewStreamClient(writer, req)
	statusChan := make(chan int, 1)

	lbRes := &loadbalancer.LoadBalancerResult{
		Response: resp,
		URL:      "http://origin.example/live",
		Index:    "1",
	}

	instance.ProxyStream(context.Background(), coordinator, lbRes, streamClient, statusChan)

	select {
	case status := <-statusChan:
		if status != proxy.StatusEOF {
			t.Fatalf("ProxyStream() status = %v, want %v for non-VOD live media", status, proxy.StatusEOF)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProxyStream() timed out waiting for status")
	}
}

func TestStreamInstance_ProxyStream_UsesDirectPathForLiveMediaWhenSharedBufferDisabled(t *testing.T) {
	utils.ResetCaches()
	defer utils.ResetCaches()

	_ = os.Unsetenv("M3U_URL_1")
	_ = os.Unsetenv("M3U_CONTAINS_VOD_1")
	_ = os.Unsetenv("SHARED_BUFFER")
	defer func() {
		_ = os.Unsetenv("M3U_URL_1")
		_ = os.Unsetenv("M3U_CONTAINS_VOD_1")
		_ = os.Unsetenv("SHARED_BUFFER")
	}()

	_ = os.Setenv("M3U_URL_1", "http://example.com/live")
	_ = os.Setenv("M3U_CONTAINS_VOD_1", "false")
	_ = os.Setenv("SHARED_BUFFER", "false")

	cfg := config.NewDefaultStreamConfig()
	cm := store.NewConcurrencyManager()
	coordinator := buffer.NewStreamCoordinator("test_id", cfg, cm, logger.Default)

	instance, err := NewStreamInstance(cm, cfg, WithLogger(logger.Default))
	if err != nil {
		t.Fatalf("Failed to create StreamInstance: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/live", nil)
	payload := []byte("live direct stream bytes")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: io.NopCloser(&eofAfterDataReader{
			data: payload,
		}),
		Request: &http.Request{
			URL: req.URL,
		},
	}
	resp.Header.Set("Content-Type", "video/MP2T")

	writer := &mockResponseWriter{}
	streamClient := client.NewStreamClient(writer, req)
	statusChan := make(chan int, 1)

	lbRes := &loadbalancer.LoadBalancerResult{
		Response: resp,
		URL:      "http://origin.example/live",
		Index:    "1",
	}

	instance.ProxyStream(context.Background(), coordinator, lbRes, streamClient, statusChan)

	select {
	case status := <-statusChan:
		if status != proxy.StatusCompleted {
			t.Fatalf("ProxyStream() status = %v, want %v for no-buffer live media passthrough", status, proxy.StatusCompleted)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProxyStream() timed out waiting for status")
	}

	if got := cm.GetCount("1"); got != 0 {
		t.Fatalf("expected concurrency count to return to 0 after direct passthrough, got %d", got)
	}
	if !bytes.Equal(writer.written, payload) {
		t.Fatalf("ProxyStream() wrote %q, want %q", writer.written, payload)
	}
}

func TestStreamHandler_HandleStream_CancelReleasesConcurrencyBeforeFirstChunk(t *testing.T) {
	cfg := config.NewDefaultStreamConfig()
	cfg.TimeoutSeconds = 30

	cm := store.NewConcurrencyManager()
	coordinator := buffer.NewStreamCoordinator("test_id", cfg, cm, logger.Default)
	handler := NewStreamHandler(cfg, coordinator, logger.Default)

	body := newBlockingReadCloser([]byte("not an m3u8"))
	req := httptest.NewRequest(http.MethodGet, "http://example.com/live", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       body,
		Request: &http.Request{
			URL: req.URL,
		},
	}
	resp.Header.Set("Content-Type", "video/MP2T")

	lbRes := &loadbalancer.LoadBalancerResult{
		Response: resp,
		URL:      "http://origin.example/live",
		Index:    "1",
	}

	writer := &mockResponseWriter{}
	streamClient := client.NewStreamClient(writer, req)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resultCh := make(chan StreamResult, 1)
	go func() {
		resultCh <- handler.HandleStream(ctx, lbRes, streamClient)
	}()

	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream read to start")
	}

	deadline := time.Now().Add(time.Second)
	for cm.GetCount("1") != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := cm.GetCount("1"); got != 1 {
		t.Fatalf("expected concurrency count to reach 1 before cancel, got %d", got)
	}

	cancel()

	select {
	case result := <-resultCh:
		if result.Status != proxy.StatusClientClosed {
			t.Fatalf("HandleStream() status = %v, want %v", result.Status, proxy.StatusClientClosed)
		}
	case <-time.After(time.Second):
		t.Fatal("HandleStream() did not return promptly after context cancel")
	}

	deadline = time.Now().Add(time.Second)
	for cm.GetCount("1") != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := cm.GetCount("1"); got != 0 {
		t.Fatalf("expected concurrency count to return to 0 after cancel, got %d", got)
	}
}
