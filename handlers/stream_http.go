package handlers

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/proxy"
	"windows-m3u-stream-merger-proxy/proxy/client"
	"windows-m3u-stream-merger-proxy/proxy/loadbalancer"
	"windows-m3u-stream-merger-proxy/proxy/stream/buffer"
	"windows-m3u-stream-merger-proxy/proxy/stream/config"
	"windows-m3u-stream-merger-proxy/proxy/stream/failovers"
	"windows-m3u-stream-merger-proxy/utils"
)

type StreamHTTPHandler struct {
	manager             ProxyInstance
	logger              logger.Logger
	sharedBufferEnabled bool
}

func parseBoolEnv(name string, defaultValue bool) bool {
	value, ok := os.LookupEnv(name)
	if !ok {
		return defaultValue
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func NewStreamHTTPHandler(manager ProxyInstance, logger logger.Logger) *StreamHTTPHandler {
	return &StreamHTTPHandler{
		manager:             manager,
		logger:              logger,
		sharedBufferEnabled: parseBoolEnv("SHARED_BUFFER", true),
	}
}

func (h *StreamHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	streamClient := client.NewStreamClient(w, r)

	h.handleStream(r.Context(), streamClient)
}

func (h *StreamHTTPHandler) ServeSegmentHTTP(w http.ResponseWriter, r *http.Request) {
	streamClient := client.NewStreamClient(w, r)

	h.handleSegmentStream(streamClient)
}

func (h *StreamHTTPHandler) extractStreamURL(urlPath string) string {
	base := path.Base(urlPath)
	parts := strings.Split(base, ".")
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimPrefix(parts[0], "/")
}

func (h *StreamHTTPHandler) handleStream(ctx context.Context, streamClient *client.StreamClient) {
	r := streamClient.Request

	streamURL := h.extractStreamURL(r.URL.Path)
	if streamURL == "" {
		h.logger.Logf("Invalid m3uID for request from %s: %s", r.RemoteAddr, r.URL.Path)
		h.writeStreamError(streamClient, http.StatusBadRequest, "Invalid stream identifier.")
		return
	}

	if !h.sharedBufferEnabled {
		h.handleStreamWithoutSharedBuffer(ctx, streamClient, streamURL)
		return
	}

	coordinator := h.manager.GetStreamRegistry().GetOrCreateCoordinator(streamURL)
	failedIndexes := make(map[string]struct{})
	const shutdownGracePeriod = time.Second

	for {
		lbResult := coordinator.GetWriterLBResult()
		var err error
		if lbResult == nil {
			if !coordinator.TryStartWriterSetup() {
				h.logger.Debugf("Writer setup already in progress for %s; waiting for shared buffer.", streamURL)
				if waitErr := coordinator.WaitForWriterSetup(ctx); waitErr != nil {
					h.logger.Logf("Client stopped waiting for shared buffer setup on %s: %v", streamURL, waitErr)
					return
				}
				continue
			}

			h.logger.Debugf("No existing shared buffer found for %s", streamURL)
			h.logger.Debugf("Client %s executing load balancer.", r.RemoteAddr)
			lbCtx := ctx
			lbReq := r
			if len(failedIndexes) > 0 {
				excluded := make([]string, 0, len(failedIndexes))
				for index := range failedIndexes {
					excluded = append(excluded, index)
				}
				sort.Strings(excluded)
				h.logger.Logf(
					"Retrying stream %s while excluding failed sources: %s",
					streamURL,
					strings.Join(excluded, ", "),
				)
				lbCtx = loadbalancer.WithExcludedIndexes(ctx, excluded)
				lbReq = r.Clone(lbCtx)
			}

			lbResult, err = h.manager.LoadBalancer(lbCtx, lbReq)
			if err != nil {
				coordinator.FinishWriterSetup()
				if len(failedIndexes) > 0 {
					h.logger.Warnf(
						"Load balancer failed with source exclusions for %s. Clearing exclusions and retrying all sources once.",
						streamURL,
					)
					clear(failedIndexes)
					continue
				}
				h.logger.Logf("Load balancer error (%s): %v", r.URL.Path, err)
				h.writeStreamError(streamClient, http.StatusBadGateway, fmt.Sprintf("Upstream stream selection failed: %v", err))
				return
			}
		} else {
			if _, ok := h.manager.GetConcurrencyManager().Invalid.Load(lbResult.URL); !ok {
				h.logger.Logf("Existing shared buffer found for %s", streamURL)
			}
		}

		exitStatus := make(chan int)
		h.logger.Logf("Proxying %s to %s", r.URL.Path, lbResult.URL)

		proxyCtx, cancel := context.WithCancel(ctx)
		go func() {
			h.manager.ProxyStream(proxyCtx, coordinator, lbResult, streamClient, exitStatus)
		}()

		select {
		case <-ctx.Done():
			cancel()
			select {
			case <-exitStatus:
			case <-time.After(shutdownGracePeriod):
			}
			h.logger.Logf("Client has closed the stream: %s", r.RemoteAddr)
			return
		case code := <-exitStatus:
			cancel()
			done, penalize := h.handleExitCode(code, r)
			if penalize && lbResult != nil {
				failedIndexes[lbResult.Index] = struct{}{}
				h.logger.Warnf(
					"Source M3U_%s failed during active stream for %s. Trying alternate source.",
					lbResult.Index,
					streamURL,
				)
			}
			if done {
				return
			}
			// Otherwise, retry with a new lbResult.
		}

		select {
		case <-ctx.Done():
			h.logger.Logf("Client has closed the stream: %s", r.RemoteAddr)
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (h *StreamHTTPHandler) handleStreamWithoutSharedBuffer(ctx context.Context, streamClient *client.StreamClient, streamURL string) {
	r := streamClient.Request
	failedIndexes := make(map[string]struct{})
	const shutdownGracePeriod = time.Second

	for {
		// Create a fresh coordinator for each attempt to avoid stale state
		coordinator := buffer.NewStreamCoordinator(streamURL, config.NewDefaultStreamConfig(), h.manager.GetConcurrencyManager(), h.logger)

		lbCtx := ctx
		lbReq := r
		if len(failedIndexes) > 0 {
			excluded := make([]string, 0, len(failedIndexes))
			for index := range failedIndexes {
				excluded = append(excluded, index)
			}
			sort.Strings(excluded)
			h.logger.Logf(
				"Retrying stream %s while excluding failed sources: %s",
				streamURL,
				strings.Join(excluded, ", "),
			)
			lbCtx = loadbalancer.WithExcludedIndexes(ctx, excluded)
			lbReq = r.Clone(lbCtx)
		}

		lbResult, err := h.manager.LoadBalancer(lbCtx, lbReq)
		if err != nil {
			if len(failedIndexes) > 0 {
				h.logger.Warnf(
					"Load balancer failed with source exclusions for %s. Clearing exclusions and retrying all sources once.",
					streamURL,
				)
				clear(failedIndexes)
				continue
			}
			h.logger.Logf("Load balancer error (%s): %v", r.URL.Path, err)
			h.writeStreamError(streamClient, http.StatusBadGateway, fmt.Sprintf("Upstream stream selection failed: %v", err))
			return
		}

		exitStatus := make(chan int)
		h.logger.Logf("Proxying %s to %s without shared buffer", r.URL.Path, lbResult.URL)

		proxyCtx, cancel := context.WithCancel(ctx)
		go func() {
			h.manager.ProxyStream(proxyCtx, coordinator, lbResult, streamClient, exitStatus)
		}()

		select {
		case <-ctx.Done():
			cancel()
			select {
			case <-exitStatus:
			case <-time.After(shutdownGracePeriod):
			}
			h.logger.Logf("Client has closed the stream: %s", r.RemoteAddr)
			return
		case code := <-exitStatus:
			cancel()
			done, penalize := h.handleExitCode(code, r)
			if penalize && lbResult != nil {
				failedIndexes[lbResult.Index] = struct{}{}
				h.logger.Warnf(
					"Source M3U_%s failed during active stream for %s. Trying alternate source.",
					lbResult.Index,
					streamURL,
				)
			}
			if done {
				return
			}
		}

		select {
		case <-ctx.Done():
			h.logger.Logf("Client has closed the stream: %s", r.RemoteAddr)
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (h *StreamHTTPHandler) writeStreamError(streamClient *client.StreamClient, status int, message string) {
	if streamClient == nil {
		return
	}

	streamClient.SetHeader("Content-Type", "text/plain; charset=utf-8")
	_ = streamClient.WriteHeader(status)
	if message != "" {
		_, _ = streamClient.Write([]byte(message))
	}
}

func (h *StreamHTTPHandler) handleExitCode(code int, r *http.Request) (done bool, penalizeCurrentSource bool) {
	switch code {
	case proxy.StatusIncompatible:
		h.logger.Errorf("Finished handling M3U8 %s request but failed to parse contents.",
			r.Method, r.RemoteAddr)
		fallthrough
	case proxy.StatusEOF:
		fallthrough
	case proxy.StatusServerError:
		h.logger.Logf("Retrying other servers...")
		return false, true
	case proxy.StatusM3U8Parsed:
		h.logger.Debugf("Finished handling M3U8 %s request: %s", r.Method,
			r.RemoteAddr)
		return true, false
	case proxy.StatusCompleted:
		h.logger.Debugf("Finished handling direct %s request: %s", r.Method, r.RemoteAddr)
		return true, false
	case proxy.StatusM3U8ParseError:
		h.logger.Errorf("Finished handling M3U8 %s request but failed to parse contents.",
			r.Method, r.RemoteAddr)
		return false, true
	default:
		h.logger.Logf("Unable to write to client. Assuming stream has been closed: %s",
			r.RemoteAddr)
		return true, false
	}
}

func (h *StreamHTTPHandler) handleSegmentStream(streamClient *client.StreamClient) {
	r := streamClient.Request

	h.logger.Debugf("Received request from %s for URL: %s",
		r.RemoteAddr, r.URL.Path)

	streamId := h.extractStreamURL(r.URL.Path)
	if streamId == "" {
		h.logger.Errorf("Invalid m3uID for request from %s: %s",
			r.RemoteAddr, r.URL.Path)
		return
	}

	segment, err := failovers.ParseSegmentId(streamId)
	if err != nil {
		h.logger.Errorf("Segment parsing error %s: %s",
			r.RemoteAddr, r.URL.Path)
		_ = streamClient.WriteHeader(http.StatusInternalServerError)
		_, _ = streamClient.Write([]byte(fmt.Sprintf("Segment parsing error: %v", err)))
		return
	}

	resp, err := utils.CustomHttpRequest(r, "GET", segment.URL)
	if err != nil {
		h.logger.Errorf("Failed to fetch URL: %v", err)
		_ = streamClient.WriteHeader(http.StatusInternalServerError)
		_, _ = streamClient.Write([]byte(fmt.Sprintf("Failed to fetch URL: %v", err)))
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			streamClient.Header().Add(key, value)
		}
	}

	_ = streamClient.WriteHeader(resp.StatusCode)

	if _, err = io.Copy(streamClient, resp.Body); err != nil {
		if isBrokenPipe(err) {
			h.logger.Debugf("Client disconnected (broken pipe): %v", err)
		} else {
			h.logger.Errorf("Error copying response body: %v", err)
		}
	}
}

func isBrokenPipe(err error) bool {
	if err == nil {
		return false
	}

	if opErr, ok := err.(*net.OpError); ok {
		if sysErr, ok := opErr.Err.(*os.SyscallError); ok {
			errMsg := sysErr.Err.Error()
			return strings.Contains(errMsg, "broken pipe") ||
				strings.Contains(errMsg, "connection reset by peer")
		}
		errMsg := opErr.Err.Error()
		return strings.Contains(errMsg, "broken pipe") ||
			strings.Contains(errMsg, "connection reset by peer")
	}

	return strings.Contains(err.Error(), "broken pipe") ||
		strings.Contains(err.Error(), "connection reset by peer")
}
