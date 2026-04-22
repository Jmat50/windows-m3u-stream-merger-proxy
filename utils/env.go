package utils

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type SourceConfig struct {
	Index          string
	URL            string
	MaxConcurrency int
	ContainsVOD    bool
}

func GetEnv(env string) string {
	switch env {
	case "USER_AGENT":
		userAgent, exists := os.LookupEnv("USER_AGENT")
		if !exists {
			userAgent = "IPTV Smarters/1.0.3 (iPad; iOS 16.6.1; Scale/2.00)"
		}
		return userAgent
	case "HTTP_ACCEPT":
		accept, exists := os.LookupEnv("HTTP_ACCEPT")
		if !exists {
			accept = "application/vnd.apple.mpegurl,application/x-mpegURL,video/MP2T,*/*"
		}
		return accept
	default:
		return ""
	}
}

var (
	dynamicSources   []SourceConfig
	dynamicSourcesMu sync.RWMutex
)

func GetSourceConfigs() []SourceConfig {
	staticSources := loadStaticSources()

	merged := make(map[string]SourceConfig, len(staticSources))
	for _, source := range staticSources {
		merged[source.Index] = source
	}

	dynamicSourcesMu.RLock()
	for _, source := range dynamicSources {
		merged[source.Index] = source
	}
	dynamicSourcesMu.RUnlock()

	keys := make([]string, 0, len(merged))
	for index := range merged {
		keys = append(keys, index)
	}
	sort.Slice(keys, func(i, j int) bool {
		return lessIndex(keys[i], keys[j])
	})

	sources := make([]SourceConfig, 0, len(keys))
	for _, index := range keys {
		sources = append(sources, merged[index])
	}

	return sources
}

func GetM3UIndexes() []string {
	sources := GetSourceConfigs()
	indexes := make([]string, 0, len(sources))
	for _, source := range sources {
		indexes = append(indexes, source.Index)
	}
	return indexes
}

func GetSourceConfig(index string) (SourceConfig, bool) {
	dynamicSourcesMu.RLock()
	for _, source := range dynamicSources {
		if source.Index == index {
			dynamicSourcesMu.RUnlock()
			return source, true
		}
	}
	dynamicSourcesMu.RUnlock()

	for _, source := range loadStaticSources() {
		if source.Index == index {
			return source, true
		}
	}

	return SourceConfig{}, false
}

func GetSourceURL(index string) (string, bool) {
	source, ok := GetSourceConfig(index)
	if !ok {
		return "", false
	}
	return source.URL, true
}

func GetSourceMaxConcurrency(index string) int {
	source, ok := GetSourceConfig(index)
	if ok && source.MaxConcurrency > 0 {
		return source.MaxConcurrency
	}

	value := strings.TrimSpace(os.Getenv(fmt.Sprintf("M3U_MAX_CONCURRENCY_%s", index)))
	if value == "" {
		return 1
	}

	maxConcurrency, err := strconv.Atoi(value)
	if err != nil || maxConcurrency < 1 {
		return 1
	}

	return maxConcurrency
}

func SetDynamicSources(sources []SourceConfig) {
	normalized := make([]SourceConfig, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))

	for _, source := range sources {
		index := strings.TrimSpace(source.Index)
		url := strings.TrimSpace(source.URL)
		if index == "" || url == "" {
			continue
		}

		if _, ok := seen[index]; ok {
			continue
		}
		seen[index] = struct{}{}

		maxConcurrency := source.MaxConcurrency
		if maxConcurrency < 1 {
			maxConcurrency = 1
		}

		normalized = append(normalized, SourceConfig{
			Index:          index,
			URL:            url,
			MaxConcurrency: maxConcurrency,
			ContainsVOD:    source.ContainsVOD,
		})
	}

	sort.Slice(normalized, func(i, j int) bool {
		return lessIndex(normalized[i].Index, normalized[j].Index)
	})

	dynamicSourcesMu.Lock()
	dynamicSources = normalized
	dynamicSourcesMu.Unlock()
}

func loadStaticSources() []SourceConfig {
	sources := make([]SourceConfig, 0, 8)
	for _, env := range os.Environ() {
		pair := strings.SplitN(env, "=", 2)
		if !strings.HasPrefix(pair[0], "M3U_URL_") {
			continue
		}

		indexString := strings.TrimPrefix(pair[0], "M3U_URL_")
		url := ""
		if len(pair) > 1 {
			url = strings.TrimSpace(pair[1])
		}
		if indexString == "" || url == "" {
			continue
		}

		maxConcurrency := 1
		if value := strings.TrimSpace(os.Getenv(fmt.Sprintf("M3U_MAX_CONCURRENCY_%s", indexString))); value != "" {
			if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
				maxConcurrency = parsed
			}
		}

		containsVOD := true
		if value := strings.TrimSpace(os.Getenv(fmt.Sprintf("M3U_CONTAINS_VOD_%s", indexString))); value != "" {
			if parsed, err := strconv.ParseBool(value); err == nil {
				containsVOD = parsed
			}
		}

		sources = append(sources, SourceConfig{
			Index:          indexString,
			URL:            url,
			MaxConcurrency: maxConcurrency,
			ContainsVOD:    containsVOD,
		})
	}

	sort.Slice(sources, func(i, j int) bool {
		return lessIndex(sources[i].Index, sources[j].Index)
	})

	return sources
}

var (
	filters     = make(map[string][]string)
	filterMutex sync.RWMutex
)

func GetFilters(baseEnv string) []string {
	filterMutex.RLock()
	if cached, ok := filters[baseEnv]; ok {
		filterMutex.RUnlock()
		return cached
	}
	filterMutex.RUnlock()

	filterMutex.Lock()
	defer filterMutex.Unlock()

	if cached, ok := filters[baseEnv]; ok {
		return cached
	}

	type indexedFilter struct {
		index int
		value string
	}
	filterValues := make([]indexedFilter, 0, 16)
	prefix := fmt.Sprintf("%s_", baseEnv)
	for _, env := range os.Environ() {
		pair := strings.SplitN(env, "=", 2)
		if strings.HasPrefix(pair[0], prefix) {
			// Remove the prefix (e.g. "FILTER_")
			indexStr := strings.TrimPrefix(pair[0], prefix)
			// Ensure the suffix is an integer.
			indexNum, err := strconv.Atoi(indexStr)
			if err != nil {
				continue
			}
			value := ""
			if len(pair) > 1 {
				value = pair[1]
			}
			filterValues = append(filterValues, indexedFilter{index: indexNum, value: value})
		}
	}

	sort.Slice(filterValues, func(i, j int) bool {
		return filterValues[i].index < filterValues[j].index
	})

	envFilters := make([]string, 0, len(filterValues))
	for _, entry := range filterValues {
		envFilters = append(envFilters, entry.value)
	}

	filters[baseEnv] = envFilters
	return envFilters
}

func lessIndex(a, b string) bool {
	aNum, aErr := strconv.Atoi(a)
	bNum, bErr := strconv.Atoi(b)

	if aErr == nil && bErr == nil {
		return aNum < bNum
	}
	if aErr == nil {
		return true
	}
	if bErr == nil {
		return false
	}
	return a < b
}

func ResetCaches() {
	dynamicSourcesMu.Lock()
	dynamicSources = nil
	dynamicSourcesMu.Unlock()

	filterMutex.Lock()
	filters = make(map[string][]string)
	filterMutex.Unlock()
}
