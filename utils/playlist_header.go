package utils

import (
	"net/url"
	"os"
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

func GetEmbeddedEPGURL() (string, bool) {
	return NormalizeEmbeddedEPGURL(os.Getenv("EMBEDDED_EPG_URL"))
}

func BuildPlaylistHeaderLine() string {
	if epgURL, ok := GetEmbeddedEPGURL(); ok {
		// Emit both common header attributes for broader IPTV-player compatibility.
		return "#EXTM3U x-tvg-url=\"" + epgURL + "\" url-tvg=\"" + epgURL + "\"\n"
	}

	return "#EXTM3U\n"
}
