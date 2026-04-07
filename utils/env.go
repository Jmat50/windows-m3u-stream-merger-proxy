package utils

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

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
			accept = "video/MP2T, */*"
		}
		return accept
	default:
		return ""
	}
}

var (
	m3uIndexes     []string
	m3uIndexesOnce = new(sync.Once)
)

func GetM3UIndexes() []string {
	m3uIndexesOnce.Do(func() {
		for _, env := range os.Environ() {
			pair := strings.SplitN(env, "=", 2)
			if strings.HasPrefix(pair[0], "M3U_URL_") {
				indexString := strings.TrimPrefix(pair[0], "M3U_URL_")
				m3uIndexes = append(m3uIndexes, indexString)
			}
		}
		sort.Slice(m3uIndexes, func(i, j int) bool {
			return lessIndex(m3uIndexes[i], m3uIndexes[j])
		})
	})
	return m3uIndexes
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
	m3uIndexesOnce = new(sync.Once)
	m3uIndexes = nil

	filterMutex.Lock()
	filters = make(map[string][]string)
	filterMutex.Unlock()
}
