package sourceproc

import (
	"encoding/base64"
	"fmt"
	"windows-m3u-stream-merger-proxy/config"
	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/utils"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/puzpuzpuz/xsync/v3"
)

var (
	filterOnce     sync.Once
	includeRegexes [][]*regexp.Regexp
	excludeRegexes [][]*regexp.Regexp
	channelRules   []channelSourceRule
	channelMerges  map[string]string
)

type channelSourceRule struct {
	titleRegex    *regexp.Regexp
	sourceIndexes map[string]struct{}
}

// checkFilter checks if a stream matches the configured filters
func checkFilter(stream *StreamInfo) bool {
	filterOnce.Do(initFilters)

	if !matchChannelSourceRule(stream) {
		return false
	}

	if allFiltersEmpty() {
		return true
	}

	if matchAny(includeRegexes[0], stream.Group) || matchAny(includeRegexes[1], stream.Title) {
		return true
	}

	if matchAny(excludeRegexes[0], stream.Group) || matchAny(excludeRegexes[1], stream.Title) {
		return false
	}

	return len(includeRegexes[0]) == 0 && len(includeRegexes[1]) == 0
}

func initFilters() {
	excludeGroups := utils.GetFilters("EXCLUDE_GROUPS")
	excludeTitle := utils.GetFilters("EXCLUDE_TITLE")
	includeGroups := utils.GetFilters("INCLUDE_GROUPS")
	includeTitle := utils.GetFilters("INCLUDE_TITLE")

	excludeRegexes = [][]*regexp.Regexp{
		compileRegexes(excludeGroups),
		compileRegexes(excludeTitle),
	}
	includeRegexes = [][]*regexp.Regexp{
		compileRegexes(includeGroups),
		compileRegexes(includeTitle),
	}

	channelRules = parseChannelSourceRules(utils.GetFilters("CHANNEL_SOURCES"))
	channelMerges = parseChannelMergeRules(utils.GetFilters("CHANNEL_MERGE"))
}

func ParseStreamInfoBySlug(slug string) (*StreamInfo, error) {
	initInfo, err := DecodeSlug(slug)
	if err != nil {
		return nil, err
	}

	initInfo.URLs = xsync.NewMapOf[string, map[string]string]()
	var wg sync.WaitGroup
	errCh := make(chan error, len(utils.GetM3UIndexes()))

	for _, m3uIndex := range utils.GetM3UIndexes() {
		wg.Add(1)
		go func(idx string) {
			defer wg.Done()
			if err := loadStreamURLs(initInfo, idx); err != nil {
				errCh <- err
			}
		}(m3uIndex)
	}

	// Wait for all goroutines and close error channel
	go func() {
		wg.Wait()
		close(errCh)
	}()

	// Collect any errors
	var errors []error
	for err := range errCh {
		errors = append(errors, err)
	}

	if len(errors) > 0 {
		return nil, fmt.Errorf("errors loading stream URLs: %v", errors)
	}

	return initInfo, nil
}

func loadStreamURLs(stream *StreamInfo, m3uIndex string) error {
	// New format uses URL-safe base64 title and "__" delimiter (Windows-safe).
	safeTitle := base64.RawURLEncoding.EncodeToString([]byte(stream.Title))
	fileName := fmt.Sprintf("%s_%s*", safeTitle, m3uIndex)
	globPatterns := []string{filepath.Join(config.GetStreamsDirPath(), "*", fileName)}

	// Backward-compatible fallback for legacy files that used StdEncoding.
	legacySafeTitle := base64.StdEncoding.EncodeToString([]byte(stream.Title))
	if legacySafeTitle != safeTitle {
		legacyFileName := fmt.Sprintf("%s_%s*", legacySafeTitle, m3uIndex)
		globPatterns = append(globPatterns, filepath.Join(config.GetStreamsDirPath(), "*", legacyFileName))
	}

	fileMatches := make([]string, 0, 8)
	seenMatches := make(map[string]struct{})
	for _, globPattern := range globPatterns {
		matches, err := filepath.Glob(globPattern)
		if err != nil {
			return fmt.Errorf("error finding files for pattern %s: %v", globPattern, err)
		}
		for _, match := range matches {
			if _, exists := seenMatches[match]; exists {
				continue
			}
			seenMatches[match] = struct{}{}
			fileMatches = append(fileMatches, match)
		}
	}

	stream.URLs.Store(m3uIndex, make(map[string]string))

	for _, fileMatch := range fileMatches {
		// Extract filename from path (works with sharded structure).
		fileNameSplit := filepath.Base(fileMatch)
		subIndex := ""
		if parts := strings.SplitN(fileNameSplit, "__", 2); len(parts) == 2 {
			subIndex = parts[1]
		} else if parts := strings.SplitN(fileNameSplit, "|", 2); len(parts) == 2 {
			subIndex = parts[1]
		}
		if subIndex == "" {
			continue
		}

		fileContent, err := os.ReadFile(fileMatch)
		if err != nil {
			logger.Default.Debugf("Error reading file %s: %v", fileMatch, err)
			continue
		}

		encodedUrl := fileContent
		urlIndex := "0"
		splitContent := strings.SplitN(string(fileContent), ":::", 2)
		if len(splitContent) == 2 {
			encodedUrl = []byte(splitContent[1])
			urlIndex = splitContent[0]
		}

		url, err := base64.StdEncoding.DecodeString(string(encodedUrl))
		if err != nil {
			logger.Default.Debugf("Error decoding URL from %s: %v", fileMatch, err)
			continue
		}

		_, _ = stream.URLs.Compute(m3uIndex, func(oldValue map[string]string, loaded bool) (newValue map[string]string, del bool) {
			if oldValue == nil {
				oldValue = make(map[string]string)
			}
			oldValue[subIndex] = strings.TrimSpace(fmt.Sprintf("%s:::%s", urlIndex, string(url)))
			return oldValue, false
		})
	}

	return nil
}

func compileRegexes(filters []string) []*regexp.Regexp {
	var regexes []*regexp.Regexp
	for _, f := range filters {
		re, err := regexp.Compile(f)
		if err != nil {
			logger.Default.Debugf("Error compiling regex %s: %v", f, err)
			continue
		}
		regexes = append(regexes, re)
	}
	return regexes
}

func parseChannelSourceRules(rawRules []string) []channelSourceRule {
	rules := make([]channelSourceRule, 0, len(rawRules))
	for _, raw := range rawRules {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}

		parts := strings.SplitN(trimmed, "|", 2)
		if len(parts) != 2 {
			logger.Default.Debugf("Invalid CHANNEL_SOURCES rule format: %s", trimmed)
			continue
		}

		pattern := strings.TrimSpace(parts[0])
		sourcesRaw := strings.TrimSpace(parts[1])
		if pattern == "" || sourcesRaw == "" {
			logger.Default.Debugf("Invalid CHANNEL_SOURCES rule: %s", trimmed)
			continue
		}

		re, err := regexp.Compile(pattern)
		if err != nil {
			logger.Default.Debugf("Error compiling CHANNEL_SOURCES regex %s: %v", pattern, err)
			continue
		}

		sourceIndexes := make(map[string]struct{})
		for _, index := range strings.Split(sourcesRaw, ",") {
			source := strings.TrimSpace(index)
			if source == "" {
				continue
			}
			sourceIndexes[source] = struct{}{}
		}

		if len(sourceIndexes) == 0 {
			logger.Default.Debugf("No valid source indexes in CHANNEL_SOURCES rule: %s", trimmed)
			continue
		}

		rules = append(rules, channelSourceRule{
			titleRegex:    re,
			sourceIndexes: sourceIndexes,
		})
	}
	return rules
}

func parseChannelMergeRules(rawRules []string) map[string]string {
	merges := make(map[string]string)
	for _, raw := range rawRules {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}

		parts := strings.SplitN(trimmed, "|", 2)
		if len(parts) != 2 {
			logger.Default.Debugf("Invalid CHANNEL_MERGE rule format: %s", trimmed)
			continue
		}

		source := strings.TrimSpace(parts[0])
		target := strings.TrimSpace(parts[1])
		if source == "" || target == "" {
			logger.Default.Debugf("Invalid CHANNEL_MERGE rule: %s", trimmed)
			continue
		}
		if strings.EqualFold(source, target) {
			continue
		}

		merges[normalizeChannelMergeKey(source)] = target
	}
	return merges
}

func applyChannelMergeRule(title string) string {
	filterOnce.Do(initFilters)
	current := strings.Join(strings.Fields(strings.TrimSpace(title)), " ")
	if current == "" {
		return title
	}

	if len(channelMerges) == 0 {
		return current
	}

	seen := make(map[string]struct{})
	for {
		key := normalizeChannelMergeKey(current)
		next, ok := channelMerges[key]
		if !ok {
			return current
		}
		if _, cycle := seen[key]; cycle {
			logger.Default.Debugf("CHANNEL_MERGE cycle detected for title: %s", title)
			return current
		}
		seen[key] = struct{}{}

		next = strings.TrimSpace(next)
		if next == "" {
			return current
		}
		current = strings.Join(strings.Fields(next), " ")
	}
}

func normalizeChannelMergeKey(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}

	normalized := strings.ToLower(strings.Join(strings.Fields(trimmed), " "))
	if normalized == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(normalized))
	for _, r := range normalized {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}

	return b.String()
}

func matchChannelSourceRule(stream *StreamInfo) bool {
	if len(channelRules) == 0 {
		return true
	}

	hasMatch := false
	for _, rule := range channelRules {
		if !rule.titleRegex.MatchString(stream.Title) {
			continue
		}

		hasMatch = true
		if _, ok := rule.sourceIndexes[stream.SourceM3U]; ok {
			return true
		}
	}

	// If no rule matched this title, do not restrict it.
	return !hasMatch
}

func matchAny(regexes []*regexp.Regexp, s string) bool {
	for _, re := range regexes {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func allFiltersEmpty() bool {
	for _, res := range includeRegexes {
		if len(res) > 0 {
			return false
		}
	}
	for _, res := range excludeRegexes {
		if len(res) > 0 {
			return false
		}
	}
	return true
}

