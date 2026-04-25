package utils

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type chainedReadCloser struct {
	reader io.Reader
	closer io.Closer
}

func (c *chainedReadCloser) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *chainedReadCloser) Close() error               { return c.closer.Close() }

// MaybeDecompressResponseBody wraps resp.Body in a gzip reader when the server
// returns Content-Encoding: gzip. This protects paths where upstream bodies are
// forwarded without Go's automatic decompression (e.g. when Accept-Encoding is
// explicitly set by the client and forwarded).
func MaybeDecompressResponseBody(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}

	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if encoding == "" || encoding == "identity" {
		return nil
	}

	// Most providers use a single gzip encoding. If multiple encodings are
	// present, we only handle gzip defensively.
	if !strings.Contains(encoding, "gzip") {
		return nil
	}

	originalBody := resp.Body
	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gzip decode: %w", err)
	}

	// Ensure closing closes both gzip reader and underlying body.
	resp.Body = &chainedReadCloser{
		reader: gr,
		closer: closerFunc(func() error {
			_ = gr.Close()
			return originalBody.Close()
		}),
	}

	// The body is now decoded; keep downstream logic simple.
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return nil
}

type closerFunc func() error

func (c closerFunc) Close() error { return c() }

