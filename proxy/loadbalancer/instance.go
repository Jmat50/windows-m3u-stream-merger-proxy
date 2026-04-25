package loadbalancer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/proxy"
	"windows-m3u-stream-merger-proxy/sourceproc"
	"windows-m3u-stream-merger-proxy/store"
	"windows-m3u-stream-merger-proxy/utils"

	"github.com/puzpuzpuz/xsync/v3"
)

type LoadBalancerInstance struct {
	infoMu        sync.Mutex
	info          *sourceproc.StreamInfo
	Cm            *store.ConcurrencyManager
	config        *LBConfig
	httpClient    HTTPClient
	healthClient  HTTPClient
	logger        logger.Logger
	indexProvider IndexProvider
	slugParser    SlugParser
	testedIndexes *xsync.MapOf[string, []string]
}

type LoadBalancerInstanceOption func(*LoadBalancerInstance)

func WithHTTPClient(client HTTPClient) LoadBalancerInstanceOption {
	return func(s *LoadBalancerInstance) {
		s.httpClient = client
		s.setHealthClient()
	}
}

func WithLogger(logger logger.Logger) LoadBalancerInstanceOption {
	return func(s *LoadBalancerInstance) {
		s.logger = logger
	}
}

func WithIndexProvider(provider IndexProvider) LoadBalancerInstanceOption {
	return func(s *LoadBalancerInstance) {
		s.indexProvider = provider
	}
}

func WithSlugParser(parser SlugParser) LoadBalancerInstanceOption {
	return func(s *LoadBalancerInstance) {
		s.slugParser = parser
	}
}

func NewLoadBalancerInstance(
	cm *store.ConcurrencyManager,
	cfg *LBConfig,
	opts ...LoadBalancerInstanceOption,
) *LoadBalancerInstance {
	instance := &LoadBalancerInstance{
		Cm:            cm,
		config:        cfg,
		httpClient:    utils.HTTPClient,
		logger:        &logger.DefaultLogger{},
		indexProvider: &DefaultIndexProvider{},
		slugParser:    &DefaultSlugParser{},
		testedIndexes: xsync.NewMapOf[string, []string](),
	}
	instance.setHealthClient()

	for _, opt := range opts {
		opt(instance)
	}

	return instance
}

type LoadBalancerResult struct {
	Response *http.Response
	URL      string
	Index    string
	SubIndex string
}

func (instance *LoadBalancerInstance) setHealthClient() {
	if originalClient, ok := instance.httpClient.(*http.Client); ok {
		healthCheckClient := *originalClient

		if originalTransport, ok := originalClient.Transport.(*http.Transport); ok {
			// Create a new transport and copy relevant fields from the original transport
			transportCopy := &http.Transport{
				Proxy:                 originalTransport.Proxy,
				DialContext:           originalTransport.DialContext,
				TLSClientConfig:       originalTransport.TLSClientConfig,
				TLSHandshakeTimeout:   originalTransport.TLSHandshakeTimeout,
				DisableKeepAlives:     originalTransport.DisableKeepAlives,
				DisableCompression:    originalTransport.DisableCompression,
				MaxIdleConns:          originalTransport.MaxIdleConns,
				MaxIdleConnsPerHost:   originalTransport.MaxIdleConnsPerHost,
				IdleConnTimeout:       originalTransport.IdleConnTimeout,
				ResponseHeaderTimeout: 3 * time.Second,
				ExpectContinueTimeout: originalTransport.ExpectContinueTimeout,
				ForceAttemptHTTP2:     originalTransport.ForceAttemptHTTP2,
			}

			// Assign the copied transport to the new client
			healthCheckClient.Transport = transportCopy
		} else {
			// If the transport is not *http.Transport, create a new transport
			healthCheckClient.Transport = &http.Transport{
				ResponseHeaderTimeout: 3 * time.Second,
			}
		}

		instance.healthClient = &healthCheckClient
	} else {
		instance.healthClient = instance.httpClient
	}
}

func (instance *LoadBalancerInstance) GetStreamInfo() *sourceproc.StreamInfo {
	instance.infoMu.Lock()
	defer instance.infoMu.Unlock()
	return instance.info
}

func (instance *LoadBalancerInstance) SetStreamInfo(info *sourceproc.StreamInfo) {
	instance.infoMu.Lock()
	defer instance.infoMu.Unlock()
	instance.info = info
}

func (instance *LoadBalancerInstance) GetStreamId(req *http.Request) string {
	streamId := strings.Split(path.Base(req.URL.Path), ".")[0]
	if streamId == "" {
		return ""
	}
	streamId = strings.TrimPrefix(streamId, "/")

	return streamId
}

func (instance *LoadBalancerInstance) Balance(ctx context.Context, req *http.Request) (*LoadBalancerResult, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context cannot be nil")
	}
	if req == nil {
		return nil, fmt.Errorf("req cannot be nil")
	}
	if req.Method == "" {
		return nil, fmt.Errorf("req.Method cannot be empty")
	}
	if req.URL == nil {
		return nil, fmt.Errorf("req.URL cannot be empty")
	}

	streamId := instance.GetStreamId(req)

	err := instance.fetchBackendUrls(streamId)
	if err != nil {
		return nil, fmt.Errorf("error fetching sources for %s: %w", streamId, err)
	}

	backoff := proxy.NewBackoffStrategy(time.Duration(instance.config.RetryWait)*time.Second, 0)

	for lap := 0; lap < instance.config.MaxRetries || instance.config.MaxRetries == 0; lap++ {
		instance.logger.Debugf("Stream attempt %d out of %d", lap+1, instance.config.MaxRetries)

		result, err := instance.tryAllStreams(ctx, req, streamId)
		if err == nil {
			return result, nil
		}
		instance.logger.Debugf("tryAllStreams error: %v", err)

		if err == context.Canceled {
			return nil, fmt.Errorf("cancelling load balancer")
		}

		instance.clearTested(streamId)

		select {
		case <-time.After(backoff.Next()):
		case <-ctx.Done():
			return nil, fmt.Errorf("cancelling load balancer")
		}
	}

	return nil, fmt.Errorf("error fetching stream: exhausted all streams")
}

func (instance *LoadBalancerInstance) GetNumTestedIndexes(streamId string) int {
	streamTested, ok := instance.testedIndexes.Load(streamId)
	if !ok {
		return 0
	}
	return len(streamTested)
}

func (instance *LoadBalancerInstance) fetchBackendUrls(streamUrl string) error {
	stream, err := instance.slugParser.GetStreamBySlug(streamUrl)
	if err != nil {
		return err
	}

	instance.logger.Debugf("Decoded slug: %v", stream)

	if stream.URLs == nil {
		stream.URLs = xsync.NewMapOf[string, map[string]string]()
	}
	// Validate URLs map
	if stream.URLs.Size() == 0 {
		return fmt.Errorf("stream has no URLs configured")
	}

	// Validate that at least one index has URLs
	hasValidUrls := false
	stream.URLs.Range(func(_ string, innerMap map[string]string) bool {
		if len(innerMap) > 0 {
			hasValidUrls = true
			return false
		}

		return true
	})
	if !hasValidUrls {
		return fmt.Errorf("stream has no valid URLs")
	}

	instance.SetStreamInfo(stream)

	return nil
}

func (instance *LoadBalancerInstance) tryAllStreams(ctx context.Context, req *http.Request, streamId string) (*LoadBalancerResult, error) {
	instance.logger.Logf("Trying all stream urls for: %s", streamId)
	if instance.indexProvider == nil {
		return nil, fmt.Errorf("index provider cannot be nil")
	}
	m3uIndexes := instance.indexProvider.GetM3UIndexes()
	m3uIndexes = appendDiscoveryIndexesFromStreamInfo(m3uIndexes, instance.GetStreamInfo())
	if len(m3uIndexes) == 0 {
		return nil, fmt.Errorf("no M3U indexes available")
	}
	excludedIndexes := excludedIndexesFromContext(ctx)
	if len(excludedIndexes) > 0 {
		filteredIndexes := make([]string, 0, len(m3uIndexes))
		for _, idx := range m3uIndexes {
			if _, excluded := excludedIndexes[idx]; excluded {
				continue
			}
			filteredIndexes = append(filteredIndexes, idx)
		}

		if len(filteredIndexes) > 0 {
			instance.logger.Logf(
				"Skipping source indexes for this retry: %s",
				strings.Join(sortedIndexKeys(excludedIndexes), ", "),
			)
			m3uIndexes = filteredIndexes
		} else {
			instance.logger.Warn("All source indexes excluded for retry; falling back to full source list.")
		}
	}

	select {
	case <-ctx.Done():
		return nil, context.Canceled
	default:
		done := make(map[string]bool)
		initialCount := len(m3uIndexes)

		for len(done) < initialCount {
			sort.Slice(m3uIndexes, func(i, j int) bool {
				left := m3uIndexes[i]
				right := m3uIndexes[j]
				leftPriority := instance.Cm.ConcurrencyPriorityValue(left)
				rightPriority := instance.Cm.ConcurrencyPriorityValue(right)
				if leftPriority != rightPriority {
					return leftPriority > rightPriority
				}
				return lessSourceIndex(left, right)
			})

			var index string
			for _, idx := range m3uIndexes {
				if !done[idx] {
					index = idx
					break
				}
			}

			done[index] = true

			innerMap, ok := instance.GetStreamInfo().URLs.Load(index)
			if !ok {
				instance.logger.Errorf("Channel not found from M3U_%s: %s", index, instance.GetStreamInfo().Title)
				continue
			}

			result, err := instance.tryStreamUrls(req, streamId, index, innerMap)
			if err == nil {
				return result, nil
			}

			select {
			case <-ctx.Done():
				return nil, context.Canceled
			default:
				continue
			}
		}
	}
	return nil, fmt.Errorf("no available streams")
}

func lessSourceIndex(a, b string) bool {
	aNum, aErr := strconv.Atoi(a)
	bNum, bErr := strconv.Atoi(b)

	if aErr == nil && bErr == nil {
		return aNum < bNum
	}
	if aErr == nil {
		return true
	}
	if bErr == nil {
		return false
	}
	return a < b
}

func appendDiscoveryIndexesFromStreamInfo(indexes []string, stream *sourceproc.StreamInfo) []string {
	if stream == nil || stream.URLs == nil {
		return indexes
	}

	seen := make(map[string]struct{}, len(indexes))
	merged := make([]string, 0, len(indexes))
	for _, index := range indexes {
		trimmed := strings.TrimSpace(index)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		merged = append(merged, trimmed)
	}

	stream.URLs.Range(func(index string, innerMap map[string]string) bool {
		trimmed := strings.TrimSpace(index)
		if trimmed == "" || len(innerMap) == 0 {
			return true
		}
		if !strings.HasPrefix(strings.ToUpper(trimmed), "DISC_") {
			return true
		}
		if _, ok := seen[trimmed]; ok {
			return true
		}

		seen[trimmed] = struct{}{}
		merged = append(merged, trimmed)
		return true
	})

	return merged
}

func sortedIndexKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return lessSourceIndex(keys[i], keys[j])
	})
	return keys
}

func isAcceptableStreamStatus(statusCode int) bool {
	return statusCode == http.StatusOK || statusCode == http.StatusPartialContent
}

func (instance *LoadBalancerInstance) tryStreamUrls(
	req *http.Request,
	streamId string,
	index string,
	urls map[string]string,
) (*LoadBalancerResult, error) {
	if instance.healthClient == nil {
		return nil, fmt.Errorf("HTTP client cannot be nil")
	}

	userAgent := utils.GetEnv("USER_AGENT")
	accept := utils.GetEnv("HTTP_ACCEPT")

	sortedSubIndexes := sourceprocSortStreamSubUrls(urls)

	var wg sync.WaitGroup
	resultCh := make(chan *streamTestResult, len(sortedSubIndexes))

	for _, subIndex := range sortedSubIndexes {
		fileContent, ok := urls[subIndex]
		if !ok {
			continue
		}

		url := fileContent
		fileContentSplit := strings.SplitN(fileContent, ":::", 2)
		if len(fileContentSplit) == 2 {
			url = fileContentSplit[1]
		}

		id := index + "|" + subIndex
		var alreadyTested bool
		streamTested, ok := instance.testedIndexes.Load(streamId)
		if ok {
			alreadyTested = slices.Contains(streamTested, id)
		}

		if alreadyTested {
			instance.logger.Debugf(
				"Skipping M3U_%s|%s: already tested", index, subIndex,
			)
			continue
		}

		if instance.Cm.CheckConcurrency(index) {
			instance.logger.Debugf("Concurrency limit reached for M3U_%s: %s", index, url)
			continue
		}

		wg.Add(1)
		go func(subIndex, url, candidateId string) {
			defer wg.Done()

			origHasUA := false
			origHasAccept := false
			originalHeaders := req.Header.Clone()

			newReq, err := http.NewRequest(req.Method, url, nil)
			if err != nil {
				instance.logger.Errorf("Error creating request: %s", err.Error())
				instance.markTested(streamId, candidateId)
				resultCh <- &streamTestResult{err: err}
				return
			}

			for header, values := range originalHeaders {
				canonicalHeader := http.CanonicalHeaderKey(header)
				// This is an upstream "probe" request; avoid forwarding headers that can
				// cause compressed (gzip) playlists to bypass Go's auto-decompression, or
				// trigger partial-content behavior that breaks playlist parsing.
				switch canonicalHeader {
				case "Accept-Encoding", "Range", "If-Range", "Connection", "Proxy-Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Host", "Content-Length":
					continue
				}

				switch canonicalHeader {
				case "User-Agent":
					origHasUA = true
				case "Accept":
					origHasAccept = true
				}

				for _, v := range values {
					newReq.Header.Add(header, v)
				}
			}

			if !origHasUA {
				newReq.Header.Set("User-Agent", userAgent)
			}
			if !origHasAccept {
				newReq.Header.Set("Accept", accept)
			}

			var resp *http.Response

			// Handle file:// URLs specially
			if strings.HasPrefix(url, "file://") {
				filePath, err := utils.FileURLToPath(url)
				if err != nil {
					instance.logger.Errorf("Invalid local file URL %s: %s", url, err.Error())
					instance.markTested(streamId, candidateId)
					resultCh <- &streamTestResult{err: err}
					return
				}
				file, err := os.Open(filePath)
				if err != nil {
					instance.logger.Errorf("Error opening local file %s: %s", filePath, err.Error())
					instance.markTested(streamId, candidateId)
					resultCh <- &streamTestResult{err: err}
					return
				}
				defer file.Close()

				// Get file info for content type detection
				fileInfo, err := file.Stat()
				if err != nil {
					instance.logger.Errorf("Error getting file info for %s: %s", filePath, err.Error())
					instance.markTested(streamId, candidateId)
					resultCh <- &streamTestResult{err: err}
					return
				}

				// Create a response-like object for local files
				resp = &http.Response{
					StatusCode:    http.StatusOK,
					Header:        make(http.Header),
					Body:          file,
					ContentLength: fileInfo.Size(),
					Request:       newReq,
				}

				// Set content type based on file extension
				contentType := "application/octet-stream"
				if ext := strings.ToLower(filepath.Ext(filePath)); ext != "" {
					switch ext {
					case ".ts":
						contentType = "video/MP2T"
					case ".mp4":
						contentType = "video/mp4"
					case ".m3u8":
						contentType = "application/x-mpegURL"
					case ".m3u":
						contentType = "audio/x-mpegurl"
					}
				}
				resp.Header.Set("Content-Type", contentType)
			} else {
				// Do the HTTP request for non-file URLs.
				resp, err = instance.healthClient.Do(newReq)
				if err != nil {
					if isRetryableStreamError(err) {
						instance.logger.Debugf("Temporary stream fetch error: %s", err.Error())
					} else {
						instance.logger.Errorf("Error fetching stream: %s", err.Error())
					}
					instance.markTested(streamId, candidateId)
					resultCh <- &streamTestResult{err: err}
					return
				}
			}

			if resp == nil {
				instance.logger.Errorf("Received nil response from HTTP client")
				instance.markTested(streamId, candidateId)
				resultCh <- &streamTestResult{err: fmt.Errorf("nil response")}
				return
			}
			if !isAcceptableStreamStatus(resp.StatusCode) {
				instance.logger.Errorf("Non-success stream status %d for %s %s",
					resp.StatusCode, req.Method, url)
				instance.markTested(streamId, candidateId)
				resultCh <- &streamTestResult{
					err: fmt.Errorf("non-success stream status: %d", resp.StatusCode),
				}
				return
			}

			health := 0.0
			if utils.IsProbablyM3U8(resp) {
				// Playlist probes should stay lightweight. Reading for a fixed
				// measurement window delays startup and penalizes low-bitrate HLS.
				health = 1.0
			} else {
				evaluatedHealth, evalErr := evaluateBufferHealth(resp, instance.config.BufferChunk)
				if evalErr != nil {
					instance.logger.Errorf("Error evaluating buffer health: %s", evalErr.Error())
					instance.markTested(streamId, candidateId)
					resultCh <- &streamTestResult{err: evalErr}
					return
				}
				health = evaluatedHealth
			}

			instance.logger.Debugf("Successful stream from %s (health: %f)",
				url, health)
			resultCh <- &streamTestResult{
				result: &LoadBalancerResult{
					Response: resp,
					URL:      url,
					Index:    index,
					SubIndex: subIndex,
				},
				health: health,
				err:    nil,
			}
		}(subIndex, url, id)
	}

	wg.Wait()
	close(resultCh)

	var bestResult *streamTestResult
	for res := range resultCh {
		if res.err != nil {
			continue
		}
		if bestResult == nil || res.health > bestResult.health {
			bestResult = res
		}
	}

	if bestResult != nil {
		return bestResult.result, nil
	}
	return nil, fmt.Errorf("all urls failed")
}

func isRetryableStreamError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() || netErr.Temporary() {
			return true
		}
	}
	lowerErr := strings.ToLower(err.Error())
	if strings.Contains(lowerErr, "lookup") || strings.Contains(lowerErr, "temporary") || strings.Contains(lowerErr, "timeout") {
		return true
	}
	return false
}

func (instance *LoadBalancerInstance) markTested(streamId string, id string) {
	instance.testedIndexes.Compute(streamId, func(val []string, _ bool) (newValue []string, delete bool) {
		val = append(val, id)
		return val, false
	})
}

func (instance *LoadBalancerInstance) clearTested(streamId string) {
	instance.testedIndexes.Delete(streamId)
}
