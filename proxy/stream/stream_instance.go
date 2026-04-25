package stream

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/proxy"
	"windows-m3u-stream-merger-proxy/proxy/client"
	"windows-m3u-stream-merger-proxy/proxy/loadbalancer"
	"windows-m3u-stream-merger-proxy/proxy/stream/buffer"
	"windows-m3u-stream-merger-proxy/proxy/stream/config"
	"windows-m3u-stream-merger-proxy/proxy/stream/failovers"
	"windows-m3u-stream-merger-proxy/store"
	"windows-m3u-stream-merger-proxy/utils"
)

type StreamInstance struct {
	Cm           *store.ConcurrencyManager
	config       *config.StreamConfig
	logger       logger.Logger
	failoverProc *failovers.M3U8Processor
}

type StreamInstanceOption func(*StreamInstance)

func WithLogger(logger logger.Logger) StreamInstanceOption {
	return func(s *StreamInstance) {
		s.logger = logger
	}
}

func NewStreamInstance(
	cm *store.ConcurrencyManager,
	config *config.StreamConfig,
	opts ...StreamInstanceOption,
) (*StreamInstance, error) {
	if cm == nil {
		return nil, fmt.Errorf("concurrency manager is required")
	}

	instance := &StreamInstance{
		Cm:           cm,
		config:       config,
		failoverProc: failovers.NewM3U8Processor(&logger.DefaultLogger{}),
	}

	// Apply all options
	for _, opt := range opts {
		opt(instance)
	}

	if instance.logger == nil {
		instance.logger = &logger.DefaultLogger{}
	}

	return instance, nil
}

func (instance *StreamInstance) ProxyStream(
	ctx context.Context,
	coordinator *buffer.StreamCoordinator,
	lbResult *loadbalancer.LoadBalancerResult,
	streamClient *client.StreamClient,
	statusChan chan<- int,
) {
	handler := NewStreamHandler(instance.config, coordinator, instance.logger)

	var result StreamResult

	// Try to determine if this is an M3U8 playlist
	isM3U8 := utils.IsProbablyM3U8(lbResult.Response)

	// Check if this source contains VOD files
	sourceConfig, _ := utils.GetSourceConfig(lbResult.Index)
	allowsVOD := sourceConfig.ContainsVOD
	sharedBufferEnabled := true
	if value, ok := os.LookupEnv("SHARED_BUFFER"); ok {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
			sharedBufferEnabled = parsed
		}
	}

	isDirectMedia := strings.HasSuffix(strings.ToLower(lbResult.URL), ".mp4") ||
		strings.HasSuffix(strings.ToLower(lbResult.URL), ".ts") ||
		utils.IsProbablyMedia(lbResult.Response)

	if isM3U8 && !sharedBufferEnabled {
		// When shared buffer is disabled, serve native HLS playlists/segments
		// instead of stitching media bytes. This is significantly closer to
		// direct playback behavior and avoids client-side compatibility issues.
		coordinator.FinishWriterSetup()
		if err := instance.failoverProc.ProcessM3U8Stream(lbResult, streamClient); err != nil {
			instance.logger.Errorf(
				"M3U8 passthrough failed for source M3U_%s|%s (%s): %v",
				lbResult.Index,
				lbResult.SubIndex,
				lbResult.URL,
				err,
			)
			statusChan <- proxy.StatusIncompatible
			return
		}
		statusChan <- proxy.StatusM3U8Parsed
		return
	}

	if isM3U8 {
		// This is an M3U8 playlist, handle as HLS stream
		if _, ok := instance.Cm.Invalid.Load(lbResult.URL); !ok {
			result = handler.HandleStream(ctx, lbResult, streamClient)
		} else {
			result = StreamResult{
				Status: proxy.StatusIncompatible,
			}
		}
	} else if isDirectMedia && (!sharedBufferEnabled || allowsVOD) {
		// This is NOT an M3U8, but is a direct stream (VOD or live media stream)
		coordinator.FinishWriterSetup()
		if !instance.Cm.UpdateConcurrency(lbResult.Index, true) {
			result = StreamResult{
				Error:  fmt.Errorf("concurrency limit reached"),
				Status: proxy.StatusServerError,
			}
		} else {
			defer instance.Cm.UpdateConcurrency(lbResult.Index, false)
			handler.logger.Logf("Direct media request detected from: %s", streamClient.Request.RemoteAddr)
			result = handler.HandleDirectStream(ctx, lbResult, streamClient)
		}
	} else {
		// Default: try to handle as HLS stream in case content-type is missing
		if _, ok := instance.Cm.Invalid.Load(lbResult.URL); !ok {
			result = handler.HandleStream(ctx, lbResult, streamClient)
		} else {
			result = StreamResult{
				Status: proxy.StatusIncompatible,
			}
		}
	}
	if result.Error != nil {
		if result.Status != proxy.StatusIncompatible &&
			result.Status != proxy.StatusClientClosed &&
			result.Status != proxy.StatusEOF &&
			result.Status != proxy.StatusCompleted {
			instance.logger.Errorf("Stream handler status: %v", result.Error)
		}
	}

	if result.Status == proxy.StatusIncompatible && utils.IsAnM3U8Media(lbResult.Response) {
		coordinator.FinishWriterSetup()
		if _, ok := instance.Cm.Invalid.Load(lbResult.URL); !ok {
			instance.logger.Logf("Source is known to have an incompatible media type for an M3U8. Trying a fallback passthrough method.")
			instance.logger.Logf("Passthrough method will not have any shared buffer. Concurrency support might be unreliable.")
			instance.Cm.Invalid.Store(lbResult.URL, struct{}{})
		}

		if err := instance.failoverProc.ProcessM3U8Stream(lbResult, streamClient); err != nil {
			instance.logger.Errorf(
				"M3U8 fallback passthrough failed for source M3U_%s|%s (%s): %v",
				lbResult.Index,
				lbResult.SubIndex,
				lbResult.URL,
				err,
			)
			statusChan <- proxy.StatusIncompatible
			return
		}

		statusChan <- proxy.StatusM3U8Parsed
		return
	}

	if utils.IsAnM3U8Media(lbResult.Response) {
		lbResult.Response.Body.Close()
	}

	statusChan <- result.Status
}
