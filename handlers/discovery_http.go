package handlers

import (
	"encoding/json"
	"net/http"
	"windows-m3u-stream-merger-proxy/discovery"
	"windows-m3u-stream-merger-proxy/logger"
)

type DiscoveryHTTPHandler struct {
	logger    logger.Logger
	discovery *discovery.Manager
}

func NewDiscoveryHTTPHandler(log logger.Logger, discoveryManager *discovery.Manager) *DiscoveryHTTPHandler {
	return &DiscoveryHTTPHandler{
		logger:    log,
		discovery: discoveryManager,
	}
}

func (h *DiscoveryHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sources := h.discovery.GetDiscoveredSources()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sources)
}
