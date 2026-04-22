package handlers

import (
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/utils"
)

type PassthroughHTTPHandler struct {
	logger logger.Logger
}

func NewPassthroughHTTPHandler(logger logger.Logger) *PassthroughHTTPHandler {
	return &PassthroughHTTPHandler{
		logger: logger,
	}
}

func (h *PassthroughHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/a/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		h.logger.Error("Invalid URL path: missing " + prefix)
		http.Error(w, "Invalid URL provided", http.StatusBadRequest)
		return
	}

	encodedURL := r.URL.Path[len(prefix):]
	if encodedURL == "" {
		h.logger.Error("No encoded URL provided in the path")
		http.Error(w, "No URL provided", http.StatusBadRequest)
		return
	}

	originalURLBytes, err := base64.URLEncoding.DecodeString(encodedURL)
	if err != nil {
		h.logger.Error("Failed to decode original URL: " + err.Error())
		http.Error(w, "Failed to decode original URL", http.StatusBadRequest)
		return
	}

	originalURL := string(originalURLBytes)

	// Handle file:// URLs specially
	if strings.HasPrefix(originalURL, "file://") {
		filePath, err := utils.FileURLToPath(originalURL)
		if err != nil {
			h.logger.Error("Invalid local file URL: " + err.Error())
			http.Error(w, "Error opening local file", http.StatusBadRequest)
			return
		}
		file, err := os.Open(filePath)
		if err != nil {
			h.logger.Error("Failed to open local file: " + err.Error())
			http.Error(w, "Error opening local file", http.StatusNotFound)
			return
		}
		defer file.Close()

		// Get file info
		fileInfo, err := file.Stat()
		if err != nil {
			h.logger.Error("Failed to get file info: " + err.Error())
			http.Error(w, "Error reading file info", http.StatusInternalServerError)
			return
		}

		// Set content type based on file extension
		contentType := "application/octet-stream"
		if ext := strings.ToLower(filepath.Ext(filePath)); ext != "" {
			switch strings.ToLower(ext) {
			case ".png":
				contentType = "image/png"
			case ".jpg", ".jpeg":
				contentType = "image/jpeg"
			case ".gif":
				contentType = "image/gif"
			case ".webp":
				contentType = "image/webp"
			}
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.FormatInt(fileInfo.Size(), 10))

		if _, err := io.Copy(w, file); err != nil {
			h.logger.Error("Failed to write file response: " + err.Error())
		}
		return
	}

	proxyReq, err := http.NewRequest(r.Method, originalURL, r.Body)
	if err != nil {
		h.logger.Error("Failed to create new request: " + err.Error())
		http.Error(w, "Error creating request", http.StatusInternalServerError)
		return
	}

	proxyReq = proxyReq.WithContext(r.Context())
	proxyReq.Header = r.Header.Clone()

	resp, err := utils.HTTPClient.Do(proxyReq)
	if err != nil {
		h.logger.Error("Failed to fetch original URL: " + err.Error())
		http.Error(w, "Error fetching the requested resource", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	w.WriteHeader(resp.StatusCode)

	if _, err := io.Copy(w, resp.Body); err != nil {
		h.logger.Error("Failed to write response body: " + err.Error())
	}
}
