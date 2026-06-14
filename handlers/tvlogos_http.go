package handlers

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/utils"
)

type TVLogosHTTPHandler struct {
	logger logger.Logger
}

func NewTVLogosHTTPHandler(logger logger.Logger) *TVLogosHTTPHandler {
	return &TVLogosHTTPHandler{logger: logger}
}

func (h *TVLogosHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/tvlogos/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "Invalid URL path: missing "+prefix, http.StatusBadRequest)
		return
	}

	rootDir := utils.ResolveTVLogosRootDir()
	if rootDir == "" {
		http.Error(w, "TV logos directory is not configured", http.StatusNotFound)
		return
	}

	relativePath := strings.TrimPrefix(r.URL.Path, prefix)
	if relativePath == "" {
		http.Error(w, "No logo path provided", http.StatusBadRequest)
		return
	}

	cleanRelative := filepath.ToSlash(filepath.Clean(relativePath))
	if cleanRelative == "." || strings.HasPrefix(cleanRelative, "../") || strings.Contains(cleanRelative, "/../") {
		http.Error(w, "Invalid logo path", http.StatusBadRequest)
		return
	}

	filePath := filepath.Join(rootDir, filepath.FromSlash(cleanRelative))
	rootClean := filepath.Clean(rootDir)
	fileClean := filepath.Clean(filePath)
	if fileClean != rootClean && !strings.HasPrefix(fileClean, rootClean+string(os.PathSeparator)) {
		http.Error(w, "Invalid logo path", http.StatusBadRequest)
		return
	}

	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Logo not found", http.StatusNotFound)
			return
		}
		h.logger.Error("Failed to open TV logo file: " + err.Error())
		http.Error(w, "Error opening logo file", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil || fileInfo.IsDir() {
		http.Error(w, "Logo not found", http.StatusNotFound)
		return
	}

	contentType := "application/octet-stream"
	switch strings.ToLower(filepath.Ext(filePath)) {
	case ".png":
		contentType = "image/png"
	case ".jpg", ".jpeg":
		contentType = "image/jpeg"
	case ".gif":
		contentType = "image/gif"
	case ".webp":
		contentType = "image/webp"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(fileInfo.Size(), 10))

	if _, err := io.Copy(w, file); err != nil {
		h.logger.Error("Failed to write TV logo response: " + err.Error())
	}
}
