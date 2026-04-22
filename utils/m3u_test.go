package utils

import "testing"

func TestIsM3UContentHandlesUTF16BOM(t *testing.T) {
	utf16String := "\ufeff#EXTM3U\n#EXTINF:-1,Test\nhttp://example.com/stream.ts\n"
	buf := []byte{0xFF, 0xFE}
	for _, r := range utf16String {
		buf = append(buf, byte(r), byte(r>>8))
	}

	if !IsM3UContent(buf) {
		t.Fatal("IsM3UContent() should return true for UTF-16 LE BOM encoded playlist")
	}
}
func TestIsM3UContentRejectsHtmlWithEmbeddedMarkers(t *testing.T) {
	htmlPage := []byte(`<html><body><p>Example page</p>#EXTM3U
#EXTINF:-1,Test
http://example.com/stream.ts
</body></html>`)
	if IsM3UContent(htmlPage) {
		t.Fatal("IsM3UContent() should reject HTML pages that contain playlist markers")
	}
}
