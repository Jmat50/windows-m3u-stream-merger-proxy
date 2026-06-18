package utils

import (
	"net/url"
	"os"
	"strconv"
	"strings"
)

// NormalizeEmbeddedEPGURL validates an optional XMLTV URL used in the
// generated M3U header. We intentionally accept any well-formed http/https URL
// so XMLTV endpoints with query strings still work.
func NormalizeEmbeddedEPGURL(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", false
	}

	parsed, err := url.ParseRequestURI(value)
	if err != nil {
		return "", false
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return "", false
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", false
	}

	return value, true
}

// GetEmbeddedEPGURL returns one or more validated XMLTV URLs from
// EMBEDDED_EPG_URL. Multiple sources use the common comma-separated
// x-tvg-url / url-tvg playlist header format.
func GetEmbeddedEPGURL() (string, bool) {
	raw := strings.TrimSpace(os.Getenv("EMBEDDED_EPG_URL"))
	if raw == "" {
		return "", false
	}

	var urls []string
	for _, part := range strings.Split(raw, ",") {
		if normalized, ok := NormalizeEmbeddedEPGURL(part); ok {
			urls = append(urls, normalized)
		}
	}
	if len(urls) == 0 {
		return "", false
	}

	return strings.Join(urls, ","), true
}

func BuildPlaylistHeaderLine() string {
	if epgURL, ok := GetEmbeddedEPGURL(); ok {
		// Emit both common header attributes for broader IPTV-player compatibility.
		return "#EXTM3U x-tvg-url=\"" + epgURL + "\" url-tvg=\"" + epgURL + "\"\n"
	}

	return "#EXTM3U\n"
}

// IsMergeEPGForSameChannelNumberEnabled reports whether playlist generation should
// share tvg-id values across channel-number groups from Channel Settings.
func IsMergeEPGForSameChannelNumberEnabled() bool {
	raw := strings.TrimSpace(os.Getenv("MERGE_EPG_FOR_SAME_CHANNEL_NUMBER"))
	if raw == "" {
		return false
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false
	}
	return enabled
}

// IsChannelEPGMergeActive is true when embedded EPG URLs are configured and the
// merge-EPG-for-same-channel-number toggle is enabled.
func IsChannelEPGMergeActive() bool {
	if !IsMergeEPGForSameChannelNumberEnabled() {
		return false
	}
	_, ok := GetEmbeddedEPGURL()
	return ok
}
