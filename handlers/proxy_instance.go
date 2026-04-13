package handlers

import (
	"context"
	"net/http"
	"time"

	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/proxy"
	"windows-m3u-stream-merger-proxy/proxy/client"
	"windows-m3u-stream-merger-proxy/proxy/loadbalancer"
	"windows-m3u-stream-merger-proxy/proxy/stream"
	"windows-m3u-stream-merger-proxy/proxy/stream/buffer"
	"windows-m3u-stream-merger-proxy/proxy/stream/config"
	"windows-m3u-stream-merger-proxy/store"
)

type ProxyInstance interface {
	GetConcurrencyManager() *store.ConcurrencyManager
	GetStreamRegistry() *buffer.StreamRegistry
	LoadBalancer(ctx context.Context, req *http.Request) (*loadbalancer.LoadBalancerResult, error)
	ProxyStream(ctx context.Context, coordinator *buffer.StreamCoordinator,
		lbResult *loadbalancer.LoadBalancerResult, sClient *client.StreamClient,
		exitStatus chan<- int)
}

type DefaultProxyInstance struct {
	lbConfig     *loadbalancer.LBConfig
	streamConfig *config.StreamConfig
	registry     *buffer.StreamRegistry
	cm           *store.ConcurrencyManager
	logger       logger.Logger
}

func NewDefaultProxyInstance() *DefaultProxyInstance {
	cm := store.NewConcurrencyManager()
	streamConfig := config.NewDefaultStreamConfig()
	return &DefaultProxyInstance{
		lbConfig:     loadbalancer.NewDefaultLBConfig(),
		streamConfig: streamConfig,
		cm:           cm,
		logger:       logger.Default,
		registry:     buffer.NewStreamRegistry(streamConfig, cm, logger.Default, 30*time.Second),
	}
}

func (sm *DefaultProxyInstance) LoadBalancer(ctx context.Context, req *http.Request) (*loadbalancer.LoadBalancerResult, error) {
	instance := loadbalancer.NewLoadBalancerInstance(sm.cm, sm.lbConfig, loadbalancer.WithLogger(sm.logger))
	return instance.Balance(ctx, req)
}

func (sm *DefaultProxyInstance) ProxyStream(ctx context.Context, coordinator *buffer.StreamCoordinator,
	lbResult *loadbalancer.LoadBalancerResult, streamClient *client.StreamClient,
	exitStatus chan<- int) {
	instance, err := stream.NewStreamInstance(sm.cm, sm.streamConfig,
		stream.WithLogger(sm.logger))
	if err != nil {
		coordinator.FinishWriterSetup()
		sm.logger.Errorf("Failed to create stream instance: %v", err)
		exitStatus <- proxy.StatusServerError
		return
	}
	instance.ProxyStream(ctx, coordinator, lbResult, streamClient, exitStatus)
}

func (sm *DefaultProxyInstance) GetConcurrencyManager() *store.ConcurrencyManager {
	return sm.cm
}

func (sm *DefaultProxyInstance) GetStreamRegistry() *buffer.StreamRegistry {
	return sm.registry
}

