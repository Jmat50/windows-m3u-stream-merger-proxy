package sourceproc

import (
	"encoding/base64"
	"fmt"
	"errors"
	"os"
	"path/filepath"

	"windows-m3u-stream-merger-proxy/config"
	"windows-m3u-stream-merger-proxy/logger"

	"github.com/goccy/go-json"
	"golang.org/x/crypto/sha3"
)

func EncodeSlug(stream *StreamInfo) string {
	h := sha3.Sum224([]byte(stream.Title))
	slug := base64.RawURLEncoding.EncodeToString(h[:])

	if err := storeSlugMapping(slug, stream); err != nil {
		logger.Default.Warnf("Failed to store slug mapping: %v", err)
	}

	return slug
}

func storeSlugMapping(slug string, stream *StreamInfo) error {
	slugDir := config.GetNewSlugDirPath()
	if err := os.MkdirAll(slugDir, 0755); err != nil {
		return err
	}

	data, err := json.Marshal(stream)
	if err != nil {
		return err
	}

	slugFile := filepath.Join(slugDir, slug)
	return os.WriteFile(slugFile, data, 0644)
}

func DecodeSlug(slug string) (*StreamInfo, error) {
	LockSources()
	defer UnlockSources()

	slugDirs := []string{
		config.GetCurrentSlugDirPath(),
		config.GetNewSlugDirPath(), // fallback for publish race / Windows rename edge-cases
	}

	var data []byte
	var err error
	var readErrs []error
	for _, slugDir := range slugDirs {
		slugFile := filepath.Join(slugDir, slug)
		data, err = os.ReadFile(slugFile)
		if err == nil {
			break
		}
		readErrs = append(readErrs, fmt.Errorf("%s: %w", slugFile, err))
	}
	if err != nil {
		return nil, fmt.Errorf("slug not found: %w", errors.Join(readErrs...))
	}

	var info StreamInfo

	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("error deserializing slug data: %v", err)
	}

	return &info, nil
}

