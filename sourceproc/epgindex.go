package sourceproc

import (
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/utils"
)

// EPGChannelIndex holds XMLTV channel id values from embedded EPG URLs.
type EPGChannelIndex struct {
	exact  map[string]struct{}
	folded map[string]string
}

func NewEPGChannelIndex(ctx context.Context) *EPGChannelIndex {
	index := &EPGChannelIndex{
		exact:  make(map[string]struct{}),
		folded: make(map[string]string),
	}

	epgURL, ok := utils.GetEmbeddedEPGURL()
	if !ok {
		return index
	}

	for _, part := range strings.Split(epgURL, ",") {
		url := strings.TrimSpace(part)
		if url == "" {
			continue
		}
		if err := index.ingestURL(ctx, url); err != nil {
			logger.Default.Warnf("EPG channel index skipped %s: %v", url, err)
		}
	}

	return index
}

func (index *EPGChannelIndex) Loaded() bool {
	return index != nil && len(index.exact) > 0
}

func (index *EPGChannelIndex) Has(id string) bool {
	if index == nil {
		return false
	}

	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	if _, ok := index.exact[id]; ok {
		return true
	}

	folded := strings.ToLower(id)
	canonical, ok := index.folded[folded]
	return ok && canonical != ""
}

func (index *EPGChannelIndex) add(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	index.exact[id] = struct{}{}
	folded := strings.ToLower(id)
	if _, exists := index.folded[folded]; !exists {
		index.folded[folded] = id
	}
}

func (index *EPGChannelIndex) ingestURL(ctx context.Context, url string) error {
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", utils.GetEnv("USER_AGENT"))
	req.Header.Set("Accept", "application/xml,text/xml,*/*")

	resp, err := utils.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("unexpected EPG status %d", resp.StatusCode)
	}

	reader, err := epgResponseReader(resp.Body, url, resp.Header.Get("Content-Encoding"))
	if err != nil {
		return err
	}
	if closer, ok := reader.(io.Closer); ok && closer != resp.Body {
		defer closer.Close()
	}

	return index.parseChannels(reader)
}

func epgResponseReader(body io.Reader, url string, contentEncoding string) (io.Reader, error) {
	lowerURL := strings.ToLower(url)
	if strings.HasSuffix(lowerURL, ".gz") || strings.Contains(strings.ToLower(contentEncoding), "gzip") {
		return gzip.NewReader(body)
	}
	return body, nil
}

func (index *EPGChannelIndex) parseChannels(reader io.Reader) error {
	decoder := xml.NewDecoder(reader)

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		start, ok := token.(xml.StartElement)
		if !ok || !strings.EqualFold(start.Name.Local, "channel") {
			continue
		}

		for _, attr := range start.Attr {
			if strings.EqualFold(attr.Name.Local, "id") {
				index.add(attr.Value)
				break
			}
		}
	}
}
