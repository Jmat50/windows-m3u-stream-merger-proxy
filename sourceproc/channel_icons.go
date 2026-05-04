package sourceproc

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/utils"
)

const autoRetrieveChannelIconsEnv = "AUTO_RETRIEVE_CHANNEL_ICONS"

var (
	tvLogosContentsBaseURL = "https://api.github.com/repos/tv-logo/tv-logos/contents/countries/united-states"
	tvLogosHTTPClient      = utils.HTTPClient
	autoChannelIconCache   = &channelIconCache{}
)

type channelIconCache struct {
	mu            sync.RWMutex
	loadedBaseURL string
	exact         map[string]channelIconCandidate
	candidates    []channelIconCandidate
	loadErr       error
}

type channelIconCandidate struct {
	key      string
	tokens   []string
	url      string
	path     string
	priority int
}

type tvLogoDirectoryEntry struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	URL         string `json:"url"`
	Path        string `json:"path"`
	DownloadURL string `json:"download_url"`
}

func maybeApplyAutoChannelIcon(stream *StreamInfo) {
	if stream == nil || strings.TrimSpace(stream.LogoURL) != "" || !autoRetrieveChannelIconsEnabled() {
		return
	}

	logoURL := autoChannelIconCache.lookup(stream.Title, stream.TvgID)
	if logoURL == "" {
		return
	}

	stream.LogoURL = utils.TvgLogoParser(logoURL)
	stream.AutoLogoURL = true
}

func autoRetrieveChannelIconsEnabled() bool {
	value := strings.TrimSpace(os.Getenv(autoRetrieveChannelIconsEnv))
	if value == "" {
		return false
	}

	enabled, err := strconv.ParseBool(value)
	return err == nil && enabled
}

func (c *channelIconCache) lookup(title, tvgID string) string {
	if err := c.ensureLoaded(); err != nil {
		return ""
	}

	keys := buildChannelIconLookupKeys(title, tvgID)
	if len(keys) == 0 {
		return ""
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, key := range keys {
		if candidate, ok := c.exact[key]; ok {
			return candidate.url
		}
	}

	var best channelIconCandidate
	bestScore := 0
	found := false
	for _, key := range keys {
		queryTokens := strings.Split(key, "-")
		if len(queryTokens) < 2 {
			continue
		}

		for _, candidate := range c.candidates {
			if !hasTokenPrefix(candidate.tokens, queryTokens) {
				continue
			}

			score := candidatePrefixScore(candidate, queryTokens)
			if !found || score < bestScore || (score == bestScore && candidateLess(candidate, best)) {
				best = candidate
				bestScore = score
				found = true
			}
		}
	}

	if !found {
		return ""
	}
	return best.url
}

func (c *channelIconCache) ensureLoaded() error {
	baseURL := strings.TrimSpace(tvLogosContentsBaseURL)

	c.mu.RLock()
	if c.loadedBaseURL == baseURL && (c.exact != nil || c.loadErr != nil) {
		err := c.loadErr
		c.mu.RUnlock()
		return err
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.loadedBaseURL == baseURL && (c.exact != nil || c.loadErr != nil) {
		return c.loadErr
	}

	candidates, err := fetchTVLogoCandidates(baseURL)
	if err != nil {
		c.loadedBaseURL = baseURL
		c.exact = nil
		c.candidates = nil
		c.loadErr = err
		logger.Default.Warnf("Auto-retrieve channel icons lookup unavailable: %v", err)
		return err
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidateLess(candidates[i], candidates[j])
	})

	exact := make(map[string]channelIconCandidate, len(candidates))
	for _, candidate := range candidates {
		if existing, ok := exact[candidate.key]; !ok || candidateLess(candidate, existing) {
			exact[candidate.key] = candidate
		}
	}

	c.loadedBaseURL = baseURL
	c.exact = exact
	c.candidates = candidates
	c.loadErr = nil
	return nil
}

func (c *channelIconCache) resetForTests() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.loadedBaseURL = ""
	c.exact = nil
	c.candidates = nil
	c.loadErr = nil
}

func fetchTVLogoCandidates(baseURL string) ([]channelIconCandidate, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("TV logo contents endpoint is empty")
	}

	pending := []string{baseURL}
	seen := make(map[string]struct{}, 8)
	candidates := make([]channelIconCandidate, 0, 256)

	for len(pending) > 0 {
		currentURL := pending[0]
		pending = pending[1:]

		if _, ok := seen[currentURL]; ok {
			continue
		}
		seen[currentURL] = struct{}{}

		entries, err := fetchTVLogoDirectoryEntries(currentURL)
		if err != nil {
			return nil, err
		}

		for _, entry := range entries {
			switch strings.ToLower(strings.TrimSpace(entry.Type)) {
			case "dir":
				nextURL := strings.TrimSpace(entry.URL)
				if nextURL != "" {
					pending = append(pending, nextURL)
				}
			case "file":
				candidate, ok := newChannelIconCandidate(entry)
				if ok {
					candidates = append(candidates, candidate)
				}
			}
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no channel icon PNG files found under %s", baseURL)
	}

	return candidates, nil
}

func fetchTVLogoDirectoryEntries(apiURL string) ([]tvLogoDirectoryEntry, error) {
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "windows-m3u-stream-merger-proxy/1.0")

	resp, err := tvLogosHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf(
			"TV logo contents request %s returned %s: %s",
			apiURL,
			resp.Status,
			strings.TrimSpace(string(body)),
		)
	}

	var entries []tvLogoDirectoryEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("decode TV logo contents response %s: %w", apiURL, err)
	}

	return entries, nil
}

func newChannelIconCandidate(entry tvLogoDirectoryEntry) (channelIconCandidate, bool) {
	name := strings.TrimSpace(entry.Name)
	downloadURL := strings.TrimSpace(entry.DownloadURL)
	if name == "" || downloadURL == "" || !strings.EqualFold(path.Ext(name), ".png") {
		return channelIconCandidate{}, false
	}

	baseName := strings.TrimSuffix(name, path.Ext(name))
	key := normalizeLogoCandidateKey(baseName)
	if key == "" {
		return channelIconCandidate{}, false
	}

	return channelIconCandidate{
		key:      key,
		tokens:   strings.Split(key, "-"),
		url:      downloadURL,
		path:     strings.TrimSpace(entry.Path),
		priority: channelIconPathPriority(entry.Path),
	}, true
}

func normalizeLogoCandidateKey(raw string) string {
	tokens := splitNormalizedChannelIconTokens(raw)
	if len(tokens) == 0 {
		return ""
	}

	if last := tokens[len(tokens)-1]; last == "us" || last == "usa" {
		tokens = tokens[:len(tokens)-1]
	}

	return strings.Join(tokens, "-")
}

func buildChannelIconLookupKeys(title, tvgID string) []string {
	keys := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)

	add := func(raw string) {
		tokens := splitNormalizedChannelIconTokens(raw)
		if len(tokens) == 0 {
			return
		}

		current := append([]string(nil), tokens...)
		for len(current) > 0 {
			key := strings.Join(current, "-")
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				keys = append(keys, key)
			}

			next := trimTrailingNoiseTokens(current)
			if len(next) == len(current) {
				break
			}
			current = next
		}
	}

	add(title)
	add(tvgID)
	return keys
}

func splitNormalizedChannelIconTokens(raw string) []string {
	normalized := normalizeChannelIconText(raw)
	if normalized == "" {
		return nil
	}
	return strings.Split(normalized, "-")
}

func normalizeChannelIconText(raw string) string {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "" {
		return ""
	}

	replacer := strings.NewReplacer(
		"&", " and ",
		"+", " plus ",
		"@", " at ",
		"'", "",
		"’", "",
		".", " ",
		"_", " ",
		"/", " ",
		"\\", " ",
		"-", " ",
		":", " ",
		"(", " ",
		")", " ",
		"[", " ",
		"]", " ",
	)
	value = replacer.Replace(value)

	var builder strings.Builder
	builder.Grow(len(value))
	for _, char := range value {
		switch {
		case unicode.IsLetter(char), unicode.IsDigit(char):
			builder.WriteRune(char)
		default:
			builder.WriteByte(' ')
		}
	}

	fields := strings.Fields(builder.String())
	if len(fields) == 0 {
		return ""
	}

	return strings.Join(fields, "-")
}

func trimTrailingNoiseTokens(tokens []string) []string {
	trimIndex := len(tokens)
	for trimIndex > 0 && isChannelIconNoiseToken(tokens[trimIndex-1]) {
		trimIndex--
	}
	if trimIndex == len(tokens) {
		return tokens
	}

	return append([]string(nil), tokens[:trimIndex]...)
}

func isChannelIconNoiseToken(token string) bool {
	switch token {
	case "us", "usa", "hd", "sd", "uhd", "fhd", "4k", "1080p", "720p", "east", "west", "backup", "feed":
		return true
	default:
		return false
	}
}

func hasTokenPrefix(candidateTokens, queryTokens []string) bool {
	if len(queryTokens) < 2 || len(candidateTokens) <= len(queryTokens) {
		return false
	}

	for index, token := range queryTokens {
		if candidateTokens[index] != token {
			return false
		}
	}

	return true
}

func candidatePrefixScore(candidate channelIconCandidate, queryTokens []string) int {
	extraTokens := candidate.tokens[len(queryTokens):]
	return candidate.priority + len(extraTokens)*10 + suffixVariantPenalty(extraTokens)
}

func suffixVariantPenalty(tokens []string) int {
	penalty := 0
	for _, token := range tokens {
		switch token {
		case "logo", "white", "light", "dark", "square", "round", "live", "event", "coverage", "stream":
			penalty += 4
		case "default", "aluminum", "butterscotch", "garnet", "hz", "screen", "bug", "localish":
			penalty += 3
		default:
			if len(token) == 4 && isAllDigits(token) {
				penalty += 2
			}
		}
	}
	return penalty
}

func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if !unicode.IsDigit(char) {
			return false
		}
	}
	return true
}

func candidateLess(left, right channelIconCandidate) bool {
	if left.priority != right.priority {
		return left.priority < right.priority
	}
	if len(left.tokens) != len(right.tokens) {
		return len(left.tokens) < len(right.tokens)
	}
	if len(left.key) != len(right.key) {
		return len(left.key) < len(right.key)
	}
	return left.path < right.path
}

func channelIconPathPriority(pathValue string) int {
	lowerPath := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(pathValue), "\\", "/"))
	switch {
	case strings.Contains(lowerPath, "/obsolete/"):
		return 60
	case strings.Contains(lowerPath, "/screen-bug/"):
		return 50
	case strings.Contains(lowerPath, "/us-local/"):
		return 30
	case strings.Contains(lowerPath, "/hd/"):
		return 20
	case strings.Contains(lowerPath, "/custom/"):
		return 10
	default:
		return 0
	}
}
