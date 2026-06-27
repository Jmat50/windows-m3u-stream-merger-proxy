package sourceproc

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"windows-m3u-stream-merger-proxy/config"
	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/utils"

	"github.com/puzpuzpuz/xsync/v3"
)

const (
	directSourceProxyingEnv         = "DIRECT_SOURCE_PROXYING"
	categoriesGroupedBySourceEnv    = "CATEGORIES_GROUPED_BY_SOURCE"
)

func GetStreamBySlug(slug string) (*StreamInfo, error) {
	var err error
	streamInfo, err := ParseStreamInfoBySlug(slug)
	if err != nil {
		return &StreamInfo{}, fmt.Errorf("error parsing stream info: %v", err)
	}

	return streamInfo, nil
}

func GenerateStreamURL(baseUrl string, stream *StreamInfo) string {
	if stream.URLs == nil {
		stream.URLs = xsync.NewMapOf[string, map[string]string]()
	}

	subPaths := make(chan string, stream.URLs.Size())
	var wg sync.WaitGroup
	var err error

	extension := ""

	// Process URLs concurrently
	stream.URLs.Range(func(_ string, innerMap map[string]string) bool {
		for _, srcUrl := range innerMap {
			if extension == "" {
				extension, err = utils.GetFileExtensionFromUrl(srcUrl)
				if err != nil {
					extension = ""
				}
			}

			wg.Add(1)
			go func(url string) {
				defer wg.Done()
				if subPath, err := utils.GetSubPathFromUrl(url); err == nil {
					subPaths <- subPath
				}
			}(srcUrl)
		}

		return true
	})

	// Close channel after all goroutines complete
	go func() {
		wg.Wait()
		close(subPaths)
	}()

	finalUrl := ""

	// Use the first valid subPath
	for subPath := range subPaths {
		finalUrl = fmt.Sprintf("%s/p/%s/%s", baseUrl, subPath, EncodeSlug(stream))
		break
	}

	// Fallback to default path
	if finalUrl == "" {
		finalUrl = fmt.Sprintf("%s/p/stream/%s", baseUrl, EncodeSlug(stream))
	}

	if strings.Contains(extension, ".m3u") {
		extension = ""
	}

	return finalUrl + extension
}

func SortStreamSubUrls(urls map[string]string) []string {
	type urlInfo struct {
		key string
		idx int
	}

	urlInfos := make([]urlInfo, 0, len(urls))
	for key, url := range urls {
		idxStr := strings.SplitN(url, ":::", 2)[0]
		idx, _ := strconv.Atoi(idxStr)
		urlInfos = append(urlInfos, urlInfo{key, idx})
	}

	sort.Slice(urlInfos, func(i, j int) bool {
		return urlInfos[i].idx < urlInfos[j].idx
	})

	result := make([]string, len(urlInfos))
	for i, info := range urlInfos {
		result[i] = info.key
	}
	return result
}

func ClearProcessedM3Us() {
	err := os.RemoveAll(config.GetProcessedDirPath())
	if err != nil {
		logger.Default.Error(err.Error())
	}
}

func cleanupOrphanedSourceCaches() {
	dir := config.GetSourcesDirPath()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Default.Warnf("Unable to scan source cache directory %s: %v", dir, err)
		}
		return
	}

	active := make(map[string]struct{}, len(utils.GetM3UIndexes()))
	for _, index := range utils.GetM3UIndexes() {
		active[index] = struct{}{}
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		index := strings.TrimSuffix(name, ".new")
		index = strings.TrimSuffix(index, ".m3u")
		if index == name {
			continue
		}
		if _, ok := active[index]; ok {
			continue
		}

		filePath := filepath.Join(dir, name)
		if removeErr := os.Remove(filePath); removeErr != nil && !os.IsNotExist(removeErr) {
			logger.Default.Warnf("Unable to remove orphaned source cache %s: %v", filePath, removeErr)
		}
	}
}

func directSourceProxyingEnabled() bool {
	rawValue := strings.TrimSpace(os.Getenv(directSourceProxyingEnv))
	enabled, err := strconv.ParseBool(rawValue)
	if err != nil {
		return false
	}
	return enabled
}

func categoriesGroupedBySourceEnabled() bool {
	rawValue := strings.TrimSpace(os.Getenv(categoriesGroupedBySourceEnv))
	enabled, err := strconv.ParseBool(rawValue)
	if err != nil {
		return false
	}
	return enabled
}

func sourceDisplayName(sourceIndex string) string {
	sourceIndex = strings.TrimSpace(sourceIndex)
	if sourceIndex == "" {
		return ""
	}

	if sourceConfig, ok := utils.GetSourceConfig(sourceIndex); ok {
		if name := strings.TrimSpace(sourceConfig.Name); name != "" {
			return name
		}
	}

	if num, err := strconv.Atoi(sourceIndex); err == nil {
		return fmt.Sprintf("Source %d", num)
	}

	return sourceIndex
}

func streamSourceIndex(stream *StreamInfo) string {
	if stream == nil {
		return ""
	}

	if sourceIndex := strings.TrimSpace(stream.SourceM3U); sourceIndex != "" {
		return sourceIndex
	}

	if stream.URLs == nil || stream.URLs.Size() == 0 {
		return ""
	}

	var bestIndex string
	stream.URLs.Range(func(sourceIndex string, inner map[string]string) bool {
		if len(inner) == 0 {
			return true
		}
		if bestIndex == "" || lessSourceIndex(sourceIndex, bestIndex) {
			bestIndex = sourceIndex
		}
		return true
	})

	return bestIndex
}

func streamGroupForOutput(stream *StreamInfo) string {
	if stream == nil {
		return ""
	}

	if !categoriesGroupedBySourceEnabled() {
		return stream.Group
	}

	if name := sourceDisplayName(streamSourceIndex(stream)); name != "" {
		return name
	}

	return stream.Group
}

