package sourceproc

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"windows-m3u-stream-merger-proxy/config"
	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/utils"

	"github.com/puzpuzpuz/xsync/v3"
	"golang.org/x/crypto/sha3"
)

var (
	// attributeRegex matches M3U attributes in the format key="value"
	attributeRegex = regexp.MustCompile(`([a-zA-Z0-9_-]+)="([^"]*)"`)
)

// parseLine parses a single M3U line into a StreamInfo
func parseLine(line string, nextLine *LineDetails, m3uIndex string) *StreamInfo {
	logger.Default.Debugf("Parsing line: %s", line)
	logger.Default.Debugf("Next line: %s", nextLine.Content)

	cleanUrl := strings.TrimSpace(nextLine.Content)

	// Resolve relative URLs for local M3U sources
	if sourceConfig, ok := utils.GetSourceConfig(m3uIndex); ok && strings.HasPrefix(sourceConfig.URL, "file://") {
		if !strings.HasPrefix(cleanUrl, "http://") && !strings.HasPrefix(cleanUrl, "https://") && !strings.HasPrefix(cleanUrl, "file://") {
			// This is a relative URL in a local M3U file, resolve it relative to the M3U file's directory
			m3uPath := sourceConfig.URL
			resolvedPath, err := utils.FileURLToPath(m3uPath)
			if err == nil {
				m3uDir := filepath.Dir(resolvedPath)
				relativeResolvedPath := filepath.Join(m3uDir, cleanUrl)
				absPath, err := filepath.Abs(relativeResolvedPath)
				if err == nil {
					cleanUrl = "file://" + filepath.ToSlash(absPath)
				}
			}
		}
	}

	stream := &StreamInfo{
		URLs: xsync.NewMapOf[string, map[string]string](),
	}

	matches := attributeRegex.FindAllStringSubmatch(line, -1)
	lineWithoutPairs := line

	for _, match := range matches {
		key := strings.TrimSpace(match[1])
		value := strings.TrimSpace(match[2])

		switch strings.ToLower(key) {
		case "tvg-id":
			stream.TvgID = utils.TvgIdParser(value)
		case "tvg-chno", "channel-id", "channel-number":
			stream.TvgChNo = utils.TvgChNoParser(value)
		case "tvg-name":
			stream.Title = utils.TvgNameParser(value)
		case "tvg-type":
			stream.TvgType = utils.TvgTypeParser(value)
		case "tvg-group", "group-title":
			stream.Group = utils.GroupTitleParser(value)
		case "tvg-logo":
			stream.LogoURL = utils.TvgLogoParser(value)
		}
		lineWithoutPairs = strings.Replace(lineWithoutPairs, match[0], "", 1)
	}

	if commaSplit := strings.SplitN(lineWithoutPairs, ",", 2); len(commaSplit) > 1 {
		stream.Title = utils.TvgNameParser(strings.TrimSpace(commaSplit[1]))
	}

	stream.Title = applyChannelMergeRule(stream.Title)

	if stream.Title == "" {
		stream.Title = "Unknown Channel"
	}

	if stream.Title == "" {
		logger.Default.Debugf("Stream missing title, skipping: %s", line)
		return nil
	}

	indexStreamURL(stream, m3uIndex, nextLine.LineNum, cleanUrl)

	return stream
}

func indexStreamURL(stream *StreamInfo, m3uIndex string, sourceIndex int, cleanURL string) {
	if stream == nil {
		return
	}

	if stream.URLs == nil {
		stream.URLs = xsync.NewMapOf[string, map[string]string]()
	}
	_, _ = stream.URLs.LoadOrStore(m3uIndex, make(map[string]string))

	encodedURL := base64.StdEncoding.EncodeToString([]byte(cleanURL))

	// Use URL-safe base64 for filenames to avoid path separators and other invalid chars on Windows.
	base64Title := base64.RawURLEncoding.EncodeToString([]byte(stream.Title))
	h := sha3.Sum224([]byte(cleanURL))
	urlHash := hex.EncodeToString(h[:])

	// Determine shard from the first 3 hex characters of the URL hash
	shard := urlHash[:3]
	shardDir := filepath.Join(config.GetStreamsDirPath(), shard)
	// "|" is invalid in Windows filenames, so use "__" as a cross-platform delimiter.
	fileName := fmt.Sprintf("%s_%s__%s", base64Title, m3uIndex, urlHash)
	filePath := filepath.Join(shardDir, fileName)

	stream.SourceM3U = m3uIndex
	stream.SourceIndex = sourceIndex
	stream.SourceURL = strings.TrimSpace(cleanURL)

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		// Create shard directory if it doesn't exist
		if err := os.MkdirAll(shardDir, os.ModePerm); err != nil {
			logger.Default.Debugf("Error creating shard directory %s: %v", shardDir, err)
		}
		content := fmt.Sprintf("%d:::%s", sourceIndex, encodedURL)
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			logger.Default.Debugf("Error indexing stream: %s (#%s) -> %v", stream.Title, m3uIndex, err)
		}

		_, _ = stream.URLs.Compute(m3uIndex, func(oldValue map[string]string, loaded bool) (newValue map[string]string, del bool) {
			if oldValue == nil {
				oldValue = make(map[string]string)
			}
			oldValue[urlHash] = fmt.Sprintf("%d:::%s", sourceIndex, cleanURL)
			return oldValue, false
		})
	}
}

// formatStreamEntry formats a stream entry for M3U output
func formatStreamEntry(baseURL string, stream *StreamInfo) string {
	var entry strings.Builder

	extInfTags := []string{"#EXTINF:-1"}

	if stream.TvgID != "" {
		extInfTags = append(extInfTags, fmt.Sprintf("tvg-id=\"%s\"", stream.TvgID))
	}
	if stream.TvgChNo != "" {
		extInfTags = append(extInfTags, fmt.Sprintf("tvg-chno=\"%s\"", stream.TvgChNo))
	}
	if stream.LogoURL != "" {
		extInfTags = append(extInfTags, fmt.Sprintf("tvg-logo=\"%s\"", stream.LogoURL))
	}
	if stream.Group != "" {
		extInfTags = append(extInfTags, fmt.Sprintf("tvg-group=\"%s\"", stream.Group))
		extInfTags = append(extInfTags, fmt.Sprintf("group-title=\"%s\"", stream.Group))
	}
	if stream.TvgType != "" {
		extInfTags = append(extInfTags, fmt.Sprintf("tvg-type=\"%s\"", stream.TvgType))
	}
	if stream.Title != "" {
		extInfTags = append(extInfTags, fmt.Sprintf("tvg-name=\"%s\"", stream.Title))
	}

	entry.WriteString(fmt.Sprintf("%s,%s\n", strings.Join(extInfTags, " "), stream.Title))
	entry.WriteString(GenerateStreamURL(baseURL, stream))
	entry.WriteString("\n")

	return entry.String()
}
