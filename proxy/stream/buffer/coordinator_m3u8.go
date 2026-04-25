package buffer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"windows-m3u-stream-merger-proxy/proxy"
	"windows-m3u-stream-merger-proxy/proxy/client"
	"windows-m3u-stream-merger-proxy/proxy/loadbalancer"
	"windows-m3u-stream-merger-proxy/utils"
)

const (
	initialSegmentCap = 32
)

type PlaylistMetadata struct {
	TargetDuration float64
	MediaSequence  int64
	Version        int
	IsEndlist      bool
	Segments       []string
	Variants       []string
	IsMaster       bool
}

func (c *StreamCoordinator) StartHLSWriter(ctx context.Context, lbResult *loadbalancer.LoadBalancerResult, streamC *client.StreamClient) {
	defer func() {
		c.LBResultOnWrite.Store(nil)
		if r := recover(); r != nil {
			c.logger.Errorf("Panic in StartHLSWriter: %v", r)
			c.writeError(fmt.Errorf("internal server error"), proxy.StatusServerError)
		}
	}()

	c.LBResultOnWrite.Store(lbResult)
	c.FinishWriterSetup()
	c.WriterRespHeader.Store(nil)

	newHeaderChan := make(chan struct{})
	c.respHeaderSet.Store(&newHeaderChan)
	c.m3uHeaderSet.Store(false)
	c.logger.Debug("StartHLSWriter: Beginning read loop")

	if !c.cm.UpdateConcurrency(lbResult.Index, true) {
		c.logger.Warnf("Failed to acquire concurrency slot for M3U_%s", lbResult.Index)
		c.writeError(fmt.Errorf("concurrency limit reached"), proxy.StatusServerError)
		return
	}
	defer c.cm.UpdateConcurrency(lbResult.Index, false)

	playlistURL := lbResult.Response.Request.URL.String()
	currentResp := lbResult.Response

	var lastErr error
	emptyPlaylistCount := 0
	lastChangeTime := time.Now()
	pollInterval := time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	writeErrorAndReturn := func(err error, status int) {
		ticker.Stop()
		c.writeError(err, status)
	}

	lastMediaSeq := int64(-1)
	c.lastProcessedSeq.Store(lastMediaSeq)

	for atomic.LoadInt32(&c.state) == stateActive {
		var resp *http.Response
		if currentResp != nil {
			resp = currentResp
			currentResp = nil
		} else {
			select {
			case <-ctx.Done():
				if lastErr == nil {
					c.logger.Debug("StartHLSWriter: Context cancelled")
					writeErrorAndReturn(ctx.Err(), proxy.StatusClientClosed)
				}
				return
			case <-ticker.C:
			}

			timeout := time.Duration(c.config.TimeoutSeconds)*time.Second + pollInterval
			if time.Since(lastChangeTime) > timeout {
				c.logger.Debug("No sequence changes detected within timeout period")
				writeErrorAndReturn(ErrStreamTimeout, proxy.StatusEOF)
				return
			}

			var err error
			resp, err = utils.CustomInternalHttpRequest(streamC.Request, "GET", playlistURL)
			if err != nil {
				c.logger.Warnf("Failed to fetch playlist: %v", err)
				lastErr = err
				continue // Retry on next tick
			}
		}

		if resp == nil {
			c.logger.Warn("Received nil response from HTTP client")
			continue
		}

		if !isAcceptablePlaylistStatus(resp.StatusCode) {
			resp.Body.Close()
			c.logger.Warnf("Unexpected status for playlist: %d", resp.StatusCode)
			lastErr = fmt.Errorf("playlist returned status %d", resp.StatusCode)
			continue
		}

		_ = utils.MaybeDecompressResponseBody(resp)

		m3uPlaylist, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			c.logger.Warnf("Failed to read playlist body: %v", err)
			lastErr = err
			continue
		}

		metadata, err := c.parsePlaylist(playlistURL, string(m3uPlaylist))
		if err != nil {
			c.logger.Warnf("Failed to parse playlist: %v", err)
			lastErr = err
			continue
		}

		if metadata.TargetDuration > 0 {
			newInterval := time.Duration(metadata.TargetDuration * float64(time.Second) / 2)
			jitter := time.Duration(float64(newInterval) * (0.9 + 0.2*rand.Float64()))

			if math.Abs(float64(jitter-pollInterval)) > float64(pollInterval)*0.1 {
				pollInterval = jitter
				ticker.Reset(pollInterval)
				c.logger.Debugf("Updated polling interval to %v", pollInterval)
			}
		}

		if metadata.IsMaster {
			if len(metadata.Variants) == 0 {
				c.logger.Warn("Master playlist has no variants")
				writeErrorAndReturn(fmt.Errorf("master playlist has no variants"), proxy.StatusServerError)
				return
			}

			nextPlaylist := metadata.Variants[0]
			if nextPlaylist == playlistURL {
				c.logger.Warnf("Master playlist resolved to itself: %s", playlistURL)
				writeErrorAndReturn(fmt.Errorf("master playlist resolved to itself"), proxy.StatusServerError)
				return
			}

			c.logger.Logf("Master playlist detected. Following first variant: %s", nextPlaylist)
			playlistURL = nextPlaylist
			lastMediaSeq = -1
			c.lastProcessedSeq.Store(lastMediaSeq)
			lastChangeTime = time.Now()
			emptyPlaylistCount = 0

			currentResp, err = utils.CustomInternalHttpRequest(streamC.Request, "GET", playlistURL)
			if err != nil {
				c.logger.Warnf("Failed to fetch master variant: %v", err)
				lastErr = err
			}
			continue
		}

		if len(metadata.Segments) == 0 {
			emptyPlaylistCount++
			lastErr = fmt.Errorf("playlist has no media segments")
			c.logger.Warnf("Playlist has no media segments (%s), retry %d", playlistURL, emptyPlaylistCount)
			if emptyPlaylistCount >= 3 {
				writeErrorAndReturn(lastErr, proxy.StatusServerError)
				return
			}
			continue
		}
		emptyPlaylistCount = 0

		if metadata.IsEndlist {
			err := c.processSegments(ctx, metadata.Segments, streamC)
			if err != nil {
				c.logger.Errorf("Error processing segments: %v", err)
			}
			writeErrorAndReturn(io.EOF, proxy.StatusEOF)
			return
		}

		if metadata.MediaSequence > lastMediaSeq {
			lastChangeTime = time.Now()
			newSegments := c.getNewSegments(metadata.Segments, lastMediaSeq, metadata.MediaSequence)

			if err := c.processSegments(ctx, newSegments, streamC); err != nil {
				if ctx.Err() != nil {
					writeErrorAndReturn(err, proxy.StatusServerError)
					return
				}
				lastErr = err
			} else {
				lastMediaSeq = metadata.MediaSequence
				c.lastProcessedSeq.Store(lastMediaSeq)
				lastErr = nil // Clear error on success
			}
		}
	}
}

func (c *StreamCoordinator) getNewSegments(allSegments []string, lastSeq, currentSeq int64) []string {
	const startupLiveEdgeSegments = 2

	if lastSeq < 0 {
		if len(allSegments) <= startupLiveEdgeSegments {
			return allSegments
		}
		// For initial live startup, avoid replaying the entire playlist window.
		return allSegments[len(allSegments)-startupLiveEdgeSegments:]
	}

	segmentCount := int64(len(allSegments))
	seqDiff := currentSeq - lastSeq

	if seqDiff <= 0 || seqDiff >= segmentCount {
		return allSegments
	}

	skipCount := segmentCount - seqDiff
	if skipCount < 0 {
		skipCount = 0
	}

	return allSegments[skipCount:]
}

func (c *StreamCoordinator) processSegments(ctx context.Context, segments []string, streamC *client.StreamClient) error {
	for _, segment := range segments {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := c.streamSegment(ctx, segment, streamC); err != nil {
				if err != io.EOF {
					return err
				}
			}
		}
	}
	return nil
}

func (c *StreamCoordinator) streamSegment(ctx context.Context, segmentURL string, streamC *client.StreamClient) error {
	resp, err := utils.CustomInternalHttpRequest(streamC.Request, "GET", segmentURL)
	if err != nil {
		return fmt.Errorf("Error fetching segment stream: %v", err)
	}

	if resp == nil {
		return errors.New("Returned nil response from HTTP client")
	}
	defer resp.Body.Close()

	if !isAcceptablePlaylistStatus(resp.StatusCode) {
		return fmt.Errorf("unexpected status code received: %d for %s", resp.StatusCode, segmentURL)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		resp.Header.Set("Content-Type", "video/MP2T")
	}

	if c.m3uHeaderSet.CompareAndSwap(false, true) {
		resp.Header.Del("Content-Length")
		c.WriterRespHeader.Store(&resp.Header)

		if ch := c.respHeaderSet.Load(); ch != nil {
			close(*ch)
		}
	}

	return c.readAndWriteStream(ctx, resp.Body, false, func(b []byte) error {
		chunk := newChunkData()
		_, _ = chunk.Buffer.Write(b)
		chunk.Timestamp = time.Now()
		if !c.Write(chunk) {
			chunk.Reset()
		}
		return nil
	})

}

func isAcceptablePlaylistStatus(statusCode int) bool {
	return statusCode == http.StatusOK || statusCode == http.StatusPartialContent
}

func (c *StreamCoordinator) parsePlaylist(mediaURL string, content string) (*PlaylistMetadata, error) {
	metadata := &PlaylistMetadata{
		Segments:       make([]string, 0, initialSegmentCap),
		Variants:       make([]string, 0, 8),
		TargetDuration: 2,
	}

	base, err := url.Parse(mediaURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse base URL: %w", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	expectVariant := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		switch {
		case strings.HasPrefix(line, "#EXTM3U"):
			continue
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
			metadata.IsMaster = true
			expectVariant = true
			continue
		case strings.HasPrefix(line, "#EXT-X-VERSION:"):
			_, _ = fmt.Sscanf(line, "#EXT-X-VERSION:%d", &metadata.Version)
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			_, _ = fmt.Sscanf(line, "#EXT-X-TARGETDURATION:%f", &metadata.TargetDuration)
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			_, _ = fmt.Sscanf(line, "#EXT-X-MEDIA-SEQUENCE:%d", &metadata.MediaSequence)
		case line == "#EXT-X-ENDLIST":
			metadata.IsEndlist = true
		case !strings.HasPrefix(line, "#") && line != "":
			segURL, err := url.Parse(line)
			if err != nil {
				c.logger.Warnf("Invalid segment URL %q: %v", line, err)
				continue
			}

			if !segURL.IsAbs() {
				segURL = base.ResolveReference(segURL)
			}

			if metadata.IsMaster {
				metadata.Variants = append(metadata.Variants, segURL.String())
				expectVariant = false
				continue
			}

			metadata.Segments = append(metadata.Segments, segURL.String())
		case strings.HasPrefix(line, "#") && metadata.IsMaster && expectVariant:
			// Ignore non-URI master playlist tags until the next variant URI appears.
			continue
		}
	}

	return metadata, scanner.Err()
}
