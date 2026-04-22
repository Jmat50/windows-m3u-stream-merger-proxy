package utils

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

func IsM3UContent(body []byte) bool {
	normalized := normalizeM3UBody(body)
	if normalized == "" {
		return false
	}

	upper := strings.ToUpper(normalized)
	marker := strings.Index(upper, "#EXTM3U")
	if marker == -1 {
		marker = strings.Index(upper, "#EXTINF:")
	}
	if marker == -1 {
		return false
	}

	prefix := upper[:marker]
	if strings.Contains(prefix, "<HTML") || strings.Contains(prefix, "<!DOCTYPE HTML") || strings.Contains(prefix, "<BODY") || strings.Contains(prefix, "<HEAD") {
		return false
	}

	return strings.HasPrefix(upper, "#EXTM3U") || strings.Contains(upper, "#EXTINF:")
}

func normalizeM3UBody(body []byte) string {
	trimmed := bytes.TrimLeft(body, "\xef\xbb\xbf\r\n\t ")
	if len(trimmed) == 0 {
		return ""
	}

	if len(trimmed) >= 2 {
		switch {
		case bytes.HasPrefix(trimmed, []byte{0xFF, 0xFE}):
			return decodeUTF16(trimmed[2:], binary.LittleEndian)
		case bytes.HasPrefix(trimmed, []byte{0xFE, 0xFF}):
			return decodeUTF16(trimmed[2:], binary.BigEndian)
		}
	}

	return string(trimmed)
}

func decodeUTF16(body []byte, order binary.ByteOrder) string {
	if len(body)%2 != 0 {
		body = body[:len(body)-1]
	}

	u16 := make([]uint16, len(body)/2)
	for i := range u16 {
		u16[i] = order.Uint16(body[2*i : 2*i+2])
	}

	return string(utf16.Decode(u16))
}
