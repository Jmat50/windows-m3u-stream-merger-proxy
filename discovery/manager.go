package discovery

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/utils"

	"github.com/projectdiscovery/katana/pkg/engine/standard"
	"github.com/projectdiscovery/katana/pkg/output"
	"github.com/projectdiscovery/katana/pkg/types"
	"github.com/temoto/robotstxt"
)

const (
	defaultScanIntervalMinutes = 60
	defaultMaxDepth            = 6
	defaultMaxPages            = 500
	defaultSourceConcurrency   = 1
	defaultRequestTimeout      = 20 * time.Second
	maxValidationBytes         = 1024 * 1024
	discoveryRobotsUserAgent   = "WindowsM3UStreamMergerProxyDiscovery"
	discoveryRequestUserAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"

	katanaRateLimit   = 100
	katanaConcurrency = 10
	katanaParallelism = 1
)

var (
	embeddedPlaylistPattern = regexp.MustCompile(`(?i)(?:(?:https?:)?//|/|\.\./|\./)?[A-Za-z0-9._~!$&()*+,;=:@%/-]+\.m3u8?(?:\?[A-Za-z0-9._~!$&()*+,;=:@%/?-]*)?`)
	quotedValuePattern      = regexp.MustCompile("[\"'`]([^\"'`\\r\\n]+)[\"'`]")
)

type Job struct {
	ID                  string `json:"id,omitempty"`
	Name                string `json:"name"`
	StartURL            string `json:"start_url"`
	ScanIntervalMinutes int    `json:"scan_interval_minutes"`
	Recursive           bool   `json:"recursive"`
	MaxDepth            int    `json:"max_depth"`
	MaxPages            int    `json:"max_pages"`
	IncludeSubdomains   bool   `json:"include_subdomains"`
	FollowRobots        bool   `json:"follow_robots"`
	SourceConcurrency   int    `json:"source_concurrency"`
	Enabled             bool   `json:"enabled"`
}

type Manager struct {
	logger           logger.Logger
	onSourcesChanged func()

	mu           sync.RWMutex
	jobs         []Job
	sourcesByJob map[string][]utils.SourceConfig
	hashByJob    map[string]string
}

type document struct {
	FinalURL   string
	StatusCode int
	Body       []byte
}

type crawler struct {
	job        Job
	logger     logger.Logger
	client     *http.Client
	rootURL    *url.URL
	robotsData *robotstxt.RobotsData
	robotsGrp  *robotstxt.Group
}

func Initialize(ctx context.Context, log logger.Logger, onSourcesChanged func()) (*Manager, error) {
	jobs, err := LoadJobsFromEnv()
	if err != nil {
		return nil, err
	}

	manager := &Manager{
		logger:           log,
		onSourcesChanged: onSourcesChanged,
		jobs:             jobs,
		sourcesByJob:     make(map[string][]utils.SourceConfig, len(jobs)),
		hashByJob:        make(map[string]string, len(jobs)),
	}

	utils.SetDynamicSources(nil)
	if len(jobs) == 0 {
		return manager, nil
	}

	manager.logger.Logf("[DISCOVERY] Loaded %d web discovery job(s).", len(jobs))
	manager.refreshAll(ctx, true)

	for _, job := range jobs {
		job := job
		go manager.runJobLoop(ctx, job)
	}

	return manager, nil
}

func LoadJobsFromEnv() ([]Job, error) {
	rawJobs := utils.GetFilters("DISCOVERY_JOB")
	jobs := make([]Job, 0, len(rawJobs))

	for index, raw := range rawJobs {
		payload := strings.TrimSpace(raw)
		if payload == "" {
			continue
		}

		job := Job{
			Enabled:      true,
			FollowRobots: true,
			Recursive:    true,
		}
		if err := json.Unmarshal([]byte(payload), &job); err != nil {
			return nil, fmt.Errorf("parse DISCOVERY_JOB_%d: %w", index+1, err)
		}

		job = normalizeJob(job, index+1)
		if !job.Enabled || job.StartURL == "" {
			continue
		}
		jobs = append(jobs, job)
	}

	return jobs, nil
}

func normalizeJob(job Job, index int) Job {
	job.ID = strings.TrimSpace(job.ID)
	if job.ID == "" {
		job.ID = fmt.Sprintf("%d", index)
	}

	job.Name = strings.TrimSpace(job.Name)
	if job.Name == "" {
		job.Name = fmt.Sprintf("Web Discovery %d", index)
	}

	job.StartURL = strings.TrimSpace(job.StartURL)
	if job.ScanIntervalMinutes < 1 {
		job.ScanIntervalMinutes = defaultScanIntervalMinutes
	}
	if job.MaxDepth < 0 {
		job.MaxDepth = defaultMaxDepth
	}
	if job.MaxPages < 1 {
		job.MaxPages = defaultMaxPages
	}
	if job.SourceConcurrency < 1 {
		job.SourceConcurrency = defaultSourceConcurrency
	}
	if !job.Recursive {
		job.MaxDepth = 0
	}

	return job
}

func (m *Manager) HasJobs() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.jobs) > 0
}

func (m *Manager) runJobLoop(ctx context.Context, job Job) {
	ticker := time.NewTicker(time.Duration(job.ScanIntervalMinutes) * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, err := m.refreshJob(ctx, job)
			if err != nil {
				m.logger.Warnf("[DISCOVERY] %s scan failed: %v", job.Name, err)
				continue
			}
			if changed && m.onSourcesChanged != nil {
				m.logger.Logf("[DISCOVERY] %s changed. Triggering playlist rebuild.", job.Name)
				go m.onSourcesChanged()
			}
		}
	}
}

func (m *Manager) refreshAll(ctx context.Context, triggerUpdates bool) {
	changed := false
	for _, job := range m.jobs {
		jobChanged, err := m.refreshJob(ctx, job)
		if err != nil {
			m.logger.Warnf("[DISCOVERY] %s initial scan failed: %v", job.Name, err)
			continue
		}
		if jobChanged {
			changed = true
		}
	}

	if changed && triggerUpdates && m.onSourcesChanged != nil {
		go m.onSourcesChanged()
	}
}

func (m *Manager) refreshJob(ctx context.Context, job Job) (bool, error) {
	crawler, err := newCrawler(job, m.logger)
	if err != nil {
		return false, err
	}

	urls, err := crawler.Discover(ctx)
	if err != nil {
		return false, err
	}

	sources := make([]utils.SourceConfig, 0, len(urls))
	for _, discoveredURL := range urls {
		sources = append(sources, utils.SourceConfig{
			Index:          buildDynamicSourceIndex(job, discoveredURL),
			URL:            discoveredURL,
			MaxConcurrency: job.SourceConcurrency,
			ContainsVOD:    true,
		})
	}

	sort.Slice(sources, func(i, j int) bool {
		return sources[i].Index < sources[j].Index
	})

	hash := fingerprintURLs(urls)

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.hashByJob[job.ID] == hash {
		return false, nil
	}

	m.hashByJob[job.ID] = hash
	m.sourcesByJob[job.ID] = sources
	m.publishDynamicSourcesLocked()

	m.logger.Logf("[DISCOVERY] %s discovered %d playlist source(s).", job.Name, len(sources))
	return true, nil
}

func (m *Manager) publishDynamicSourcesLocked() {
	combined := make([]utils.SourceConfig, 0)
	for _, job := range m.jobs {
		combined = append(combined, m.sourcesByJob[job.ID]...)
	}
	utils.SetDynamicSources(combined)
}

func buildDynamicSourceIndex(job Job, playlistURL string) string {
	sum := sha1.Sum([]byte(strings.ToLower(strings.TrimSpace(playlistURL))))
	return fmt.Sprintf("DISC_%s_%s", strings.ToUpper(job.ID), strings.ToUpper(hex.EncodeToString(sum[:4])))
}

func fingerprintURLs(urls []string) string {
	if len(urls) == 0 {
		return ""
	}
	sorted := append([]string(nil), urls...)
	sort.Strings(sorted)
	return strings.Join(sorted, "\n")
}

func newCrawler(job Job, log logger.Logger) (*crawler, error) {
	if strings.TrimSpace(job.StartURL) == "" {
		return nil, fmt.Errorf("job %s is missing a start URL", job.Name)
	}

	rootURL, err := url.Parse(job.StartURL)
	if err != nil {
		return nil, fmt.Errorf("job %s has invalid start URL: %w", job.Name, err)
	}
	if rootURL.Scheme != "http" && rootURL.Scheme != "https" {
		return nil, fmt.Errorf("job %s start URL must use http or https", job.Name)
	}

	return &crawler{
		job:     job,
		logger:  log,
		rootURL: rootURL,
		client: &http.Client{
			Timeout: defaultRequestTimeout,
		},
	}, nil
}

func (c *crawler) Discover(ctx context.Context) ([]string, error) {
	if c.job.FollowRobots {
		c.loadRobots(ctx)
	}

	candidates := make(map[string]struct{})
	var mu sync.Mutex
	collected := 0

	addCandidate := func(raw string) {
		if strings.TrimSpace(raw) == "" {
			return
		}
		normalized, err := c.normalizeURL(c.rootURL.String(), raw)
		if err != nil {
			return
		}
		if !c.isCandidatePlaylistURL(normalized) {
			if !c.shouldVisitURL(normalized) {
				return
			}
		}

		mu.Lock()
		defer mu.Unlock()
		if collected >= c.job.MaxPages {
			return
		}
		if _, exists := candidates[normalized]; exists {
			return
		}
		candidates[normalized] = struct{}{}
		collected++
	}

	for _, sitemapURL := range c.initialSitemapQueue() {
		links, err := c.fetchSitemapURLs(ctx, sitemapURL)
		if err != nil {
			c.logger.Debugf("[DISCOVERY] Sitemap fetch failed for %s: %v", sitemapURL, err)
			continue
		}
		for _, link := range links {
			addCandidate(link)
		}
	}

	depth := c.job.MaxDepth
	if !c.job.Recursive {
		depth = 0
	}

	scope := "fqdn"
	if c.job.IncludeSubdomains {
		scope = "rdn"
	}

	options := &types.Options{
		MaxDepth:               depth,
		Timeout:                int(defaultRequestTimeout.Seconds()),
		BodyReadSize:           maxValidationBytes,
		Concurrency:            katanaConcurrency,
		Parallelism:            katanaParallelism,
		RateLimit:              katanaRateLimit,
		Retries:                2,
		KnownFiles:             "all",
		Strategy:               "breadth-first",
		FieldScope:             scope,
		PathClimb:              true,
		ScrapeJSResponses:      true,
		ScrapeJSLuiceResponses: true,
		TlsImpersonate:         true,
		CustomHeaders: []string{
			fmt.Sprintf("User-Agent: %s", discoveryRequestUserAgent),
			"Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			"Accept-Language: en-US,en;q=0.9",
			"Cache-Control: no-cache",
			"Pragma: no-cache",
		},
		NoColors:           true,
		Silent:             true,
		DisableUpdateCheck: true,
		NoScope:            false,
		OnResult: func(result output.Result) {
			if result.Request == nil {
				return
			}

			baseURL := result.Request.URL
			if result.Response != nil && result.Response.Resp != nil && result.Response.Resp.Request != nil && result.Response.Resp.Request.URL != nil {
				baseURL = result.Response.Resp.Request.URL.String()
			}

			addCandidate(baseURL)
			for _, embedded := range extractEmbeddedPlaylistURLs(baseURL, result.Request.URL) {
				addCandidate(embedded)
			}
			if result.Response != nil {
				for _, embedded := range extractEmbeddedPlaylistURLs(baseURL, result.Response.Body) {
					addCandidate(embedded)
				}
				for _, xhrRequest := range result.Response.XhrRequests {
					addCandidate(xhrRequest.URL)
					for _, embedded := range extractEmbeddedPlaylistURLs(baseURL, xhrRequest.URL) {
						addCandidate(embedded)
					}
				}
			}
		},
	}

	crawlerOptions, err := types.NewCrawlerOptions(options)
	if err != nil {
		return nil, err
	}
	defer crawlerOptions.Close()

	engine, err := standard.New(crawlerOptions)
	if err != nil {
		return nil, err
	}
	defer engine.Close()

	if err := engine.Crawl(c.rootURL.String()); err != nil {
		return nil, err
	}

	validatedCache := make(map[string]string, len(candidates))
	results := make([]string, 0, len(candidates))
	for candidate := range candidates {
		finalURL, ok := c.validatePlaylist(ctx, validatedCache, candidate)
		if ok {
			results = append(results, finalURL)
		}
	}

	sort.Strings(results)
	return dedupe(results), nil
}

func (c *crawler) loadRobots(ctx context.Context) {
	robotsURL := (&url.URL{
		Scheme: c.rootURL.Scheme,
		Host:   c.rootURL.Host,
		Path:   "/robots.txt",
	}).String()

	doc, err := c.fetchDocument(ctx, robotsURL, maxValidationBytes)
	if err != nil || doc.StatusCode == http.StatusNotFound {
		return
	}

	robotsData, err := robotstxt.FromStatusAndBytes(doc.StatusCode, doc.Body)
	if err != nil {
		c.logger.Debugf("[DISCOVERY] robots.txt parse failed for %s: %v", robotsURL, err)
		return
	}
	c.robotsData = robotsData
	c.robotsGrp = robotsData.FindGroup(discoveryRobotsUserAgent)
}

func (c *crawler) initialSitemapQueue() []string {
	queue := make([]string, 0, 4)
	seen := make(map[string]struct{})

	add := func(raw string) {
		value := strings.TrimSpace(raw)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		queue = append(queue, value)
	}

	if c.robotsData != nil {
		for _, sitemapURL := range c.robotsData.Sitemaps {
			add(sitemapURL)
		}
	}

	add((&url.URL{
		Scheme: c.rootURL.Scheme,
		Host:   c.rootURL.Host,
		Path:   "/sitemap.xml",
	}).String())

	return queue
}

func (c *crawler) fetchSitemapURLs(ctx context.Context, sitemapURL string) ([]string, error) {
	doc, err := c.fetchDocument(ctx, sitemapURL, maxValidationBytes)
	if err != nil {
		return nil, err
	}
	if doc.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected sitemap status %d", doc.StatusCode)
	}

	locs, err := extractSitemapLocs(doc.Body)
	if err != nil {
		return nil, err
	}

	urls := make([]string, 0, len(locs))
	for _, loc := range locs {
		value := strings.TrimSpace(loc)
		if value != "" {
			urls = append(urls, value)
		}
	}
	return urls, nil
}

func (c *crawler) validatePlaylist(ctx context.Context, cache map[string]string, candidateURL string) (string, bool) {
	if finalURL, ok := cache[candidateURL]; ok {
		return finalURL, finalURL != ""
	}

	doc, err := c.fetchDocument(ctx, candidateURL, maxValidationBytes)
	if err != nil {
		cache[candidateURL] = ""
		return "", false
	}
	if doc.StatusCode != http.StatusOK || !utils.IsM3UContent(doc.Body) {
		cache[candidateURL] = ""
		return "", false
	}

	cache[candidateURL] = doc.FinalURL
	return doc.FinalURL, true
}

func (c *crawler) fetchDocument(ctx context.Context, rawURL string, limit int64) (document, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return document{}, err
	}

	req.Header.Set("User-Agent", discoveryRequestUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,application/vnd.apple.mpegurl,application/x-mpegURL,text/plain,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")

	resp, err := c.client.Do(req)
	if err != nil {
		return document{}, err
	}
	defer resp.Body.Close()

	body, err := readBody(resp.Body, limit)
	if err != nil {
		return document{}, err
	}

	return document{
		FinalURL:   resp.Request.URL.String(),
		StatusCode: resp.StatusCode,
		Body:       body,
	}, nil
}

func (c *crawler) normalizeURL(baseURL string, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("empty url")
	}
	if strings.HasPrefix(trimmed, "#") {
		return "", fmt.Errorf("fragment-only link")
	}
	low := strings.ToLower(trimmed)
	if strings.HasPrefix(low, "javascript:") || strings.HasPrefix(low, "mailto:") {
		return "", fmt.Errorf("unsupported scheme")
	}

	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(trimmed)
	if err != nil {
		return "", err
	}

	resolved := base.ResolveReference(ref)
	resolved.Fragment = ""
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme")
	}
	return resolved.String(), nil
}

func (c *crawler) shouldVisitURL(candidateURL string) bool {
	parsed, err := url.Parse(candidateURL)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}

	rootHost := strings.ToLower(c.rootURL.Hostname())
	candidateHost := strings.ToLower(parsed.Hostname())
	if candidateHost == "" {
		return false
	}
	if candidateHost != rootHost {
		if !c.job.IncludeSubdomains || !strings.HasSuffix(candidateHost, "."+rootHost) {
			return false
		}
	}

	if c.job.FollowRobots && c.robotsGrp != nil {
		requestPath := parsed.EscapedPath()
		if requestPath == "" {
			requestPath = "/"
		}
		if parsed.RawQuery != "" {
			requestPath += "?" + parsed.RawQuery
		}
		if !c.robotsGrp.Test(requestPath) {
			return false
		}
	}

	return true
}

func (c *crawler) isCandidatePlaylistURL(candidateURL string) bool {
	parsed, err := url.Parse(candidateURL)
	if err != nil {
		return false
	}

	ext := strings.ToLower(path.Ext(parsed.Path))
	if ext == ".m3u" || ext == ".m3u8" {
		return true
	}
	if parsed.RawQuery == "" {
		return false
	}

	queryLower := strings.ToLower(parsed.RawQuery)
	return strings.Contains(queryLower, ".m3u") || strings.Contains(queryLower, ".m3u8")
}

func readBody(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	_, err := io.CopyN(&buf, r, limit+1)
	if err != nil && err != io.EOF {
		return nil, err
	}

	data := buf.Bytes()
	if int64(len(data)) > limit {
		data = data[:limit]
	}
	return append([]byte(nil), data...), nil
}

func extractSitemapLocs(body []byte) ([]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	locs := make([]string, 0, 32)

	for {
		token, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || !strings.EqualFold(start.Name.Local, "loc") {
			continue
		}

		var value string
		if err := decoder.DecodeElement(&value, &start); err != nil {
			return nil, err
		}
		value = strings.TrimSpace(value)
		if value != "" {
			locs = append(locs, value)
		}
	}

	return locs, nil
}

func extractEmbeddedPlaylistURLs(baseURL string, raw string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, 4)

	appendUnique := func(value string) {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return
		}
		if _, ok := seen[trimmed]; ok {
			return
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}

	addCandidate := func(candidate string) {
		if normalized, ok := normalizeEmbeddedPlaylistURL(baseURL, candidate); ok {
			appendUnique(normalized)
		}
	}

	collectCandidates := func(value string) {
		if strings.TrimSpace(value) == "" {
			return
		}

		variants := []string{
			value,
			html.UnescapeString(value),
			strings.ReplaceAll(value, `\/`, `/`),
			strings.ReplaceAll(html.UnescapeString(value), `\/`, `/`),
			strings.ReplaceAll(value, `\u002F`, `/`),
			strings.ReplaceAll(value, `\x2F`, `/`),
			strings.ReplaceAll(html.UnescapeString(value), `\u002F`, `/`),
			strings.ReplaceAll(html.UnescapeString(value), `\x2F`, `/`),
		}
		for _, variant := range variants {
			lower := strings.ToLower(variant)
			if strings.Contains(lower, ".m3u") {
				for _, match := range embeddedPlaylistPattern.FindAllString(variant, -1) {
					addCandidate(match)
				}
				addCandidate(variant)
			}

			for _, match := range quotedValuePattern.FindAllStringSubmatch(variant, -1) {
				if len(match) < 2 {
					continue
				}
				if strings.Contains(strings.ToLower(match[1]), ".m3u") {
					addCandidate(match[1])
				}
			}
		}
	}

	collectCandidates(raw)

	parsed, err := url.Parse(raw)
	if err == nil && parsed.RawQuery != "" {
		values := parsed.Query()
		for _, itemValues := range values {
			for _, item := range itemValues {
				collectCandidates(item)
				if unescaped, decodeErr := url.QueryUnescape(item); decodeErr == nil {
					collectCandidates(unescaped)
				}
			}
		}
	}

	return out
}

func normalizeEmbeddedPlaylistURL(baseURL string, candidate string) (string, bool) {
	value := strings.TrimSpace(candidate)
	if value == "" {
		return "", false
	}

	value = html.UnescapeString(value)
	value = strings.ReplaceAll(value, `\/`, `/`)
	value = strings.Trim(value, "\"'`()[]{}<>,")
	if value == "" {
		return "", false
	}
	if strings.HasPrefix(strings.ToLower(value), "javascript:") {
		return "", false
	}
	if !strings.Contains(strings.ToLower(value), ".m3u") {
		return "", false
	}

	if unescaped, err := url.QueryUnescape(value); err == nil {
		value = unescaped
	}

	if strings.HasPrefix(value, "//") {
		base, err := url.Parse(baseURL)
		if err != nil || base.Scheme == "" {
			return "", false
		}
		value = base.Scheme + ":" + value
	}

	parsed, err := url.Parse(value)
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		parsed.Fragment = ""
		return parsed.String(), true
	}

	base, err := url.Parse(baseURL)
	if err != nil {
		return "", false
	}

	ref, err := url.Parse(value)
	if err != nil {
		return "", false
	}

	resolved := base.ResolveReference(ref)
	resolved.Fragment = ""
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return "", false
	}
	return resolved.String(), true
}

func dedupe(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:0]
	var prev string
	for i, value := range values {
		if i == 0 || value != prev {
			out = append(out, value)
			prev = value
		}
	}
	return out
}

// DiscoveredSource represents a discovered M3U/M3U8 playlist
type DiscoveredSource struct {
	Index     string `json:"index"`
	URL       string `json:"url"`
	JobID     string `json:"job_id"`
	JobName   string `json:"job_name"`
	Enabled   bool   `json:"enabled"`
}

// GetDiscoveredSources returns all discovered playlist sources
func (m *Manager) GetDiscoveredSources() []DiscoveredSource {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var discovered []DiscoveredSource
	jobsByID := make(map[string]Job)
	for _, job := range m.jobs {
		jobsByID[job.ID] = job
	}

	for jobID, sources := range m.sourcesByJob {
		job := jobsByID[jobID]
		for _, source := range sources {
			discovered = append(discovered, DiscoveredSource{
				Index:   source.Index,
				URL:     source.URL,
				JobID:   jobID,
				JobName: job.Name,
				Enabled: job.Enabled,
			})
		}
	}

	return discovered
}
