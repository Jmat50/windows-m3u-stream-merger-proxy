package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"windows-m3u-stream-merger-proxy/config"
	"windows-m3u-stream-merger-proxy/handlers"
	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/sourceproc"
)

type countingResponseWriter struct {
	mu     sync.Mutex
	header http.Header
	status int
	bytes  int
}

func newCountingResponseWriter() *countingResponseWriter {
	return &countingResponseWriter{
		header: make(http.Header),
	}
}

func (rw *countingResponseWriter) Header() http.Header {
	return rw.header
}

func (rw *countingResponseWriter) Write(data []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	rw.bytes += len(data)
	return len(data), nil
}

func (rw *countingResponseWriter) WriteHeader(statusCode int) {
	rw.mu.Lock()
	defer rw.mu.Unlock()

	if rw.status == 0 {
		rw.status = statusCode
	}
}

func (rw *countingResponseWriter) Flush() {}

func (rw *countingResponseWriter) Snapshot() (status int, bytes int) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.status, rw.bytes
}

func TestStreamHTTPHandler(t *testing.T) {
	// Create temp directory for test data
	tempDir, err := os.MkdirTemp("", "m3u-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	originalConfig := config.GetConfig()
	defer func() {
		t.Log("Cleaning up temporary directory:", tempDir)
		config.SetConfig(originalConfig)
		if err := os.RemoveAll(tempDir); err != nil {
			t.Errorf("Failed to cleanup temp directory: %v", err)
		}
	}()

	// Set up test environment
	testDataPath := filepath.Join(tempDir, "data")
	t.Log("Setting up test data path:", testDataPath)
	if err := os.MkdirAll(testDataPath, 0755); err != nil {
		t.Fatalf("Failed to create test data directory: %v", err)
	}

	tempPath := filepath.Join(testDataPath, "temp")
	t.Log("Creating temp directory:", tempPath)
	if err := os.MkdirAll(tempPath, 0755); err != nil {
		t.Fatalf("Failed to create streams directory: %v", err)
	}

	config.SetConfig(&config.Config{
		TempPath: tempPath,
		DataPath: testDataPath,
	})

	// Initialize handlers with test configuration
	t.Log("Initializing handlers with test configuration")
	streamHandler := handlers.NewStreamHTTPHandler(
		handlers.NewDefaultProxyInstance(),
		logger.Default,
	)

	// Set up test environment variables
	packet := strings.Repeat("A", 64*1024)
	var sourceServer *httptest.Server
	sourceServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/source.m3u":
			w.Header().Set("Content-Type", "application/x-mpegURL")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXTINF:-1 tvg-id=\"test-channel\" group-title=\"Test\",Test Channel\n"+sourceServer.URL+"/stream.ts\n")
		case "/stream.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			w.Header().Set("Connection", "close")
			flusher, _ := w.(http.Flusher)
			ticker := time.NewTicker(25 * time.Millisecond)
			defer ticker.Stop()
			writeChunk := func() bool {
				if _, err := io.WriteString(w, packet); err != nil {
					return false
				}
				if flusher != nil {
					flusher.Flush()
				}
				return true
			}

			if !writeChunk() {
				return
			}

			for i := 0; i < 24; i++ {
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					if !writeChunk() {
						return
					}
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer sourceServer.Close()

	m3uURL := sourceServer.URL + "/source.m3u"
	t.Log("Setting M3U_URL_1:", m3uURL)
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("ALL_PROXY", "")
	t.Setenv("NO_PROXY", "*")
	t.Setenv("M3U_URL_1", m3uURL)
	t.Setenv("DEBUG", "true")

	// Test M3U playlist generation
	t.Log("Testing M3U playlist generation")
	m3uReq := httptest.NewRequest(http.MethodGet, "http://proxy.test/playlist.m3u", nil)
	m3uW := httptest.NewRecorder()

	processor := sourceproc.NewProcessor()
	err = processor.Run(context.Background(), m3uReq)
	if err != nil {
		t.Fatal(err)
	}
	m3uHandler := handlers.NewM3UHTTPHandler(logger.Default, processor.GetResultPath())

	m3uHandler.ServeHTTP(m3uW, m3uReq)

	m3uReq = httptest.NewRequest(http.MethodGet, "http://proxy.test/playlist.m3u", nil)
	m3uW = httptest.NewRecorder()
	m3uHandler.ServeHTTP(m3uW, m3uReq)
	if m3uW.Code != http.StatusOK {
		t.Errorf("Playlist Route - Expected status code %d, got %d", http.StatusOK, m3uW.Code)
		t.Log("Response Body:", m3uW.Body.String())
	}

	// Get streams and test each one
	t.Log("Retrieving streams from store")
	t.Logf("Found %d streams", processor.GetCount())
	if processor.GetCount() == 0 {
		t.Error("No streams found in store")
		// Log cache contents for debugging
		if contents, err := os.ReadFile(processor.GetResultPath()); err == nil {
			t.Log("Processed contents:", string(contents))
		} else {
			t.Log("Failed to read cache file:", err)
		}
	}

	file, err := os.Open(processor.GetResultPath())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var streamURL string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "http") {
			streamURL = line
			break
		}
	}

	if err := scanner.Err(); err != nil {
		t.Error(err)
	}
	if streamURL == "" {
		t.Fatal("No proxied stream URL found in generated playlist")
	}

	t.Run(streamURL, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()

		req := httptest.NewRequest(http.MethodGet, streamURL, nil).WithContext(ctx)
		recorder := newCountingResponseWriter()

		done := make(chan struct{})
		go func() {
			defer close(done)
			streamHandler.ServeHTTP(recorder, req)
		}()

		testDuration := 2 * time.Second
		timer := time.NewTimer(testDuration)
		defer timer.Stop()
		pollTicker := time.NewTicker(50 * time.Millisecond)
		defer pollTicker.Stop()

		for {
			select {
			case <-pollTicker.C:
				status, bytesRead := recorder.Snapshot()
				if bytesRead == 0 {
					continue
				}
				cancel()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("Timed out waiting for stream handler to stop")
				}
				t.Logf("Received data: %d bytes", bytesRead)
				if status != http.StatusOK {
					t.Fatalf("Expected proxied stream status %d, got %d", http.StatusOK, status)
				}
				return
			case <-timer.C:
				cancel()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("Timed out waiting for stream handler to stop")
				}
				status, bytesRead := recorder.Snapshot()
				t.Logf("Test completed after %v", testDuration)
				t.Logf("Total bytes read: %d", bytesRead)
				if bytesRead == 0 {
					t.Fatal("No data received from proxied stream")
				}
				if status != http.StatusOK {
					t.Fatalf("Expected proxied stream status %d, got %d", http.StatusOK, status)
				}
				return
			}
		}
	})
}
