package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/utils"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func bundledTVLogosDir(t *testing.T) string {
	t.Helper()

	candidates := []string{
		filepath.Join("..", "tvlogos"),
		"tvlogos",
	}
	for _, candidate := range candidates {
		absPath, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		unitedStatesDir := filepath.Join(absPath, "countries", "united-states")
		if stat, statErr := os.Stat(unitedStatesDir); statErr == nil && stat.IsDir() {
			return absPath
		}
	}

	t.Skip("bundled tvlogos directory not found")
	return ""
}

func TestTVLogosHTTPHandler_ServesBundledLogo(t *testing.T) {
	rootDir := bundledTVLogosDir(t)
	utils.SetTVLogosRootOverrideForTests(rootDir)
	t.Cleanup(utils.ResetTVLogosRootOverrideForTests)

	handler := NewTVLogosHTTPHandler(logger.Default)
	server := httptest.NewServer(http.HandlerFunc(handler.ServeHTTP))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/tvlogos/countries/united-states/fox-us.png")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "image/png", resp.Header.Get("Content-Type"))
}
