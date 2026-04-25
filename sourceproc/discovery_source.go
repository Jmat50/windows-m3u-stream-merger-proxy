package sourceproc

import (
	"net/url"
	"path"
	"strings"
	"windows-m3u-stream-merger-proxy/utils"

	"github.com/puzpuzpuz/xsync/v3"
)

var genericDiscoveryPathSegments = map[string]struct{}{
	"channel":  {},
	"index":    {},
	"live":     {},
	"master":   {},
	"media":    {},
	"playlist": {},
	"stream":   {},
}

func isDiscoveredM3U8Source(index string, source utils.SourceConfig) bool {
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(index)), "DISC_") {
		return false
	}

	parsed, err := url.Parse(strings.TrimSpace(source.URL))
	if err != nil {
		return false
	}

	return strings.EqualFold(path.Ext(parsed.Path), ".m3u8")
}

func buildSyntheticDiscoveredStream(source utils.SourceConfig) *StreamInfo {
	index := strings.TrimSpace(source.Index)
	rawURL := strings.TrimSpace(source.URL)
	if index == "" || rawURL == "" {
		return nil
	}

	stream := &StreamInfo{
		Title: deriveDiscoveredStreamTitle(source),
		Group: strings.TrimSpace(source.Group),
		URLs:  xsync.NewMapOf[string, map[string]string](),
	}
	indexStreamURL(stream, index, 0, rawURL)

	return stream
}

func deriveDiscoveredStreamTitle(source utils.SourceConfig) string {
	if name := strings.TrimSpace(source.Name); name != "" {
		return name
	}

	parsed, err := url.Parse(strings.TrimSpace(source.URL))
	if err == nil {
		for _, key := range []string{"name", "channel", "title", "id"} {
			if value := cleanDiscoveredTitleSegment(parsed.Query().Get(key)); value != "" {
				return value
			}
		}

		segments := strings.Split(parsed.Path, "/")
		for i := len(segments) - 1; i >= 0; i-- {
			segment := strings.TrimSpace(segments[i])
			if segment == "" {
				continue
			}

			segment = strings.TrimSuffix(segment, path.Ext(segment))
			cleaned := cleanDiscoveredTitleSegment(segment)
			if cleaned == "" {
				continue
			}

			if _, generic := genericDiscoveryPathSegments[normalizeDiscoveredTitleKey(cleaned)]; generic && i > 0 {
				continue
			}

			return cleaned
		}

		if host := strings.TrimSpace(parsed.Hostname()); host != "" {
			return host
		}
	}

	if group := strings.TrimSpace(source.Group); group != "" {
		return group
	}

	return "Discovered Stream"
}

func cleanDiscoveredTitleSegment(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}

	replacer := strings.NewReplacer(
		"_", " ",
		"-", " ",
		".", " ",
		"%20", " ",
		"+", " ",
	)
	return strings.Join(strings.Fields(replacer.Replace(trimmed)), " ")
}

func normalizeDiscoveredTitleKey(value string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), " ", ""))
}
