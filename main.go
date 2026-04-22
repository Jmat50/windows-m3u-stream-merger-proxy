package main

import (
	"context"
	"fmt"
	"windows-m3u-stream-merger-proxy/config"
	"windows-m3u-stream-merger-proxy/handlers"
	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/updater"
	"net/http"
	"os"
	"time"
	_ "time/tzdata"
)

func failStartup(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	logger.Default.Errorf("STARTUP ERROR: %s", msg)
	_, _ = fmt.Fprintf(os.Stderr, "STARTUP ERROR: %s\n", msg)
	os.Exit(1)
}

func main() {
	config.InitFromEnv()

	// Context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m3uHandler := handlers.NewM3UHTTPHandler(logger.Default, "")
	streamHandler := handlers.NewStreamHTTPHandler(handlers.NewDefaultProxyInstance(), logger.Default)
	passthroughHandler := handlers.NewPassthroughHTTPHandler(logger.Default)

	logger.Default.Log("Starting updater...")
	updaterInstance, err := updater.Initialize(ctx, logger.Default, m3uHandler)
	if err != nil {
		failStartup("Error initializing updater: %v", err)
	}

	discoveryHandler := handlers.NewDiscoveryHTTPHandler(logger.Default, updaterInstance.GetDiscoveryManager())

	// manually set time zone
	if tz := os.Getenv("TZ"); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			logger.Default.Warnf("error loading location '%s': %v; continuing with system local timezone", tz, err)
		} else {
			time.Local = loc
		}
	}

	logger.Default.Log("Setting up HTTP handlers...")
	// HTTP handlers
	http.HandleFunc("/playlist.m3u", func(w http.ResponseWriter, r *http.Request) {
		m3uHandler.ServeHTTP(w, r)
	})
	http.HandleFunc("/api/discovery/sources", func(w http.ResponseWriter, r *http.Request) {
		discoveryHandler.ServeHTTP(w, r)
	})
	http.HandleFunc("/p/", func(w http.ResponseWriter, r *http.Request) {
		streamHandler.ServeHTTP(w, r)
	})
	http.HandleFunc("/a/", func(w http.ResponseWriter, r *http.Request) {
		passthroughHandler.ServeHTTP(w, r)
	})
	http.HandleFunc("/segment/", func(w http.ResponseWriter, r *http.Request) {
		streamHandler.ServeSegmentHTTP(w, r)
	})

	// Start the server
	logger.Default.Logf("Server is running on port %s...", os.Getenv("PORT"))
	logger.Default.Log("Playlist Endpoint is running (`/playlist.m3u`)")
	logger.Default.Log("Stream Endpoint is running (`/p/{originalBasePath}/{streamID}.{fileExt}`)")
	err = http.ListenAndServe(fmt.Sprintf(":%s", os.Getenv("PORT")), nil)
	if err != nil {
		failStartup("HTTP server error: %v", err)
	}
}

