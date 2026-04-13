package stream

import (
	"context"
	"fmt"
	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/proxy"
	"windows-m3u-stream-merger-proxy/proxy/client"
	"windows-m3u-stream-merger-proxy/proxy/loadbalancer"
	"windows-m3u-stream-merger-proxy/proxy/stream/buffer"
	"windows-m3u-stream-merger-proxy/proxy/stream/config"
	"windows-m3u-stream-merger-proxy/proxy/stream/failovers"
	"windows-m3u-stream-merger-proxy/store"
	"windows-m3u-stream-merger-proxy/utils"
	"strings"
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
	if lbResult.Response.StatusCode == 206 || strings.HasSuffix(lbResult.URL, ".mp4") {
		coordinator.FinishWriterSetup()
		handler.logger.Logf("VOD request detected from: %s", streamClient.Request.RemoteAddr)
		handler.logger.Warn("VODs do not support shared buffer.")
		result = handler.HandleDirectStream(ctx, lbResult, streamClient)
	} else {
		if _, ok := instance.Cm.Invalid.Load(lbResult.URL); !ok {
			result = handler.HandleStream(ctx, lbResult, streamClient)
		} else {
			result = StreamResult{
				Status: proxy.StatusIncompatible,
			}
		}
	}
	if result.Error != nil {
		if result.Status != proxy.StatusIncompatible {
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

