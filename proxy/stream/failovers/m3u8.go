package failovers

import (
	"bufio"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/proxy/client"
	"windows-m3u-stream-merger-proxy/proxy/loadbalancer"
	"windows-m3u-stream-merger-proxy/utils"
)

type M3U8Processor struct {
	logger logger.Logger
}

const maxMasterFollowDepth = 4

func NewM3U8Processor(logger logger.Logger) *M3U8Processor {
	return &M3U8Processor{logger: logger}
}

func (p *M3U8Processor) ProcessM3U8Stream(
	lbResult *loadbalancer.LoadBalancerResult,
	streamClient *client.StreamClient,
) error {
	return p.processResponse(lbResult, streamClient, lbResult.Response, 0)
}

func (p *M3U8Processor) processResponse(
	lbResult *loadbalancer.LoadBalancerResult,
	streamClient *client.StreamClient,
	response *http.Response,
	depth int,
) error {
	if depth > maxMasterFollowDepth {
		return fmt.Errorf("exceeded maximum master playlist depth")
	}

	if lbResult == nil || lbResult.Response == nil || lbResult.Response.Body == nil {
		return fmt.Errorf("invalid load balancer response for m3u8 processing")
	}
	if response == nil || response.Body == nil || response.Request == nil || response.Request.URL == nil {
		return fmt.Errorf("invalid response for m3u8 processing")
	}
	defer response.Body.Close()

	base, err := url.Parse(response.Request.URL.String())
	if err != nil {
		return err
	}

	reader := bufio.NewScanner(response.Body)
	reader.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lines := make([]string, 0, 512)
	for reader.Scan() {
		lines = append(lines, reader.Text())
	}
	if err := reader.Err(); err != nil {
		return fmt.Errorf("m3u8 scan error: %w", err)
	}
	if len(lines) == 0 {
		return fmt.Errorf("m3u8 playlist is empty")
	}

	if variantURL, isMaster, err := firstMasterVariant(base, lines); err != nil {
		return err
	} else if isMaster {
		nextResp, reqErr := utils.CustomInternalHttpRequest(streamClient.Request, "GET", variantURL)
		if reqErr != nil {
			return fmt.Errorf("failed to fetch master variant %s: %w", variantURL, reqErr)
		}
		if nextResp.StatusCode != http.StatusOK && nextResp.StatusCode != http.StatusPartialContent {
			nextResp.Body.Close()
			return fmt.Errorf("master variant returned status %d", nextResp.StatusCode)
		}
		return p.processResponse(lbResult, streamClient, nextResp, depth+1)
	}

	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/vnd.apple.mpegurl"
	}

	streamClient.SetHeader("Content-Type", contentType)
	lineCount := 0
	for _, line := range lines {
		lineCount++
		if err := p.processLine(lbResult, line, streamClient, base); err != nil {
			return fmt.Errorf("process line error: %w", err)
		}
	}
	if lineCount == 0 {
		return fmt.Errorf("m3u8 playlist is empty")
	}

	return nil
}

func (p *M3U8Processor) processLine(
	lbResult *loadbalancer.LoadBalancerResult,
	line string,
	streamClient *client.StreamClient,
	baseURL *url.URL,
) error {
	if len(line) == 0 {
		return nil
	}

	if line[0] == '#' {
		return p.writeLine(streamClient, line)
	}

	return p.processURL(lbResult, line, streamClient, baseURL)
}

func (p *M3U8Processor) processURL(
	lbResult *loadbalancer.LoadBalancerResult,
	line string,
	streamClient *client.StreamClient,
	baseURL *url.URL,
) error {
	u, err := url.Parse(line)
	if err != nil {
		p.logger.Errorf("Failed to parse M3U8 URL in line: %v", err)
		return p.writeLine(streamClient, line)
	}

	if !u.IsAbs() {
		u = baseURL.ResolveReference(u)
	}

	segment := M3U8Segment{
		URL:       u.String(),
		SourceM3U: lbResult.Index + "|" + lbResult.SubIndex,
	}

	return p.writeLine(streamClient, generateSegmentURL(&segment))
}

func (p *M3U8Processor) writeLine(streamClient *client.StreamClient, line string) error {
	_, err := streamClient.Write([]byte(line + "\n"))
	if err != nil {
		return fmt.Errorf("write line error: %w", err)
	}
	return nil
}

func firstMasterVariant(baseURL *url.URL, lines []string) (string, bool, error) {
	expectVariant := false
	isMaster := false

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			isMaster = true
			expectVariant = true
			continue
		}
		if !isMaster {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if expectVariant {
				// Keep scanning until the next non-tag variant URI.
				continue
			}
			continue
		}

		if !expectVariant {
			continue
		}

		parsed, err := url.Parse(line)
		if err != nil {
			return "", true, fmt.Errorf("failed to parse master variant URL %q: %w", line, err)
		}
		if !parsed.IsAbs() {
			parsed = baseURL.ResolveReference(parsed)
		}
		if parsed.String() == baseURL.String() {
			return "", true, fmt.Errorf("master playlist resolved to itself")
		}
		return parsed.String(), true, nil
	}

	if isMaster {
		return "", true, fmt.Errorf("master playlist has no variants")
	}

	return "", false, nil
}
