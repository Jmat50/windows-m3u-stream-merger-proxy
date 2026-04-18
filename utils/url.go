package utils

import (
	"bytes"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
)

func IsAnM3U8Media(resp *http.Response) bool {
	knownMimeTypes := []string{
		"application/x-mpegurl",
		"text/plain",
		"audio/x-mpegurl",
		"audio/mpegurl",
		"application/vnd.apple.mpegurl",
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	for _, mimeType := range knownMimeTypes {
		if strings.HasPrefix(contentType, mimeType) {
			return true
		}
	}

	if resp.Request != nil && resp.Request.URL != nil {
		urlPath := resp.Request.URL.Path
		knownExtensions := []string{
			".m3u",
			".m3u8",
		}

		extension := strings.ToLower(filepath.Ext(urlPath))
		if slices.Contains(knownExtensions, extension) {
			return true
		}
	}

	return false
}

func IsProbablyM3U8(resp *http.Response) bool {
	if resp == nil {
		return false
	}

	if IsAnM3U8Media(resp) {
		return true
	}

	return hasM3U8Signature(resp)
}

func IsProbablyMedia(resp *http.Response) bool {
	if resp == nil {
		return false
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "video/") || strings.HasPrefix(contentType, "audio/") {
		return true
	}

	if strings.Contains(contentType, "application/octet-stream") {
		return true
	}

	if resp.Request != nil && resp.Request.URL != nil {
		urlPath := resp.Request.URL.Path
		ext := strings.ToLower(filepath.Ext(urlPath))
		knownExtensions := []string{
			".ts",
			".mp4",
			".m4s",
			".aac",
		}

		if slices.Contains(knownExtensions, ext) {
			return true
		}
	}

	return false
}

func hasM3U8Signature(resp *http.Response) bool {
	if resp == nil || resp.Body == nil {
		return false
	}

	const peekSize = 4096
	buf := make([]byte, peekSize)
	n, err := io.ReadAtLeast(resp.Body, buf, 1)
	if err != nil && err != io.ErrUnexpectedEOF {
		return false
	}

	data := buf[:n]
	data = bytes.TrimLeft(data, " \t\r\n")
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	upperData := bytes.ToUpper(data)
	hasSignature := bytes.Contains(upperData, []byte("#EXTM3U")) || bytes.Contains(upperData, []byte("#EXTINF:")) || bytes.Contains(upperData, []byte("#EXT-X-STREAM-INF"))

	if n > 0 {
		resp.Body = io.NopCloser(io.MultiReader(
			bytes.NewReader(data),
			resp.Body,
		))
	}

	return hasSignature
}
