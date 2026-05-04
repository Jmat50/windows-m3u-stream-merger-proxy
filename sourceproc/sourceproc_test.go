package sourceproc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"windows-m3u-stream-merger-proxy/config"
	"windows-m3u-stream-merger-proxy/utils"

	"github.com/puzpuzpuz/xsync/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testDataLock sync.Mutex

func setupTestEnvironment(t *testing.T) func() {
	testDataLock.Lock()
	defer testDataLock.Unlock()

	originalConfig := config.GetConfig()

	tempDir, err := os.MkdirTemp("", fmt.Sprintf("%d-m3u-test-*", time.Now().Unix()))
	require.NoError(t, err)

	testConfig := &config.Config{
		DataPath: filepath.Join(tempDir, "data"),
		TempPath: filepath.Join(tempDir, "temp"),
	}
	config.SetConfig(testConfig)

	// First test M3U with news, sports, and movies
	testM3U1 := `#EXTM3U
#EXTINF:-1 tvg-id="cnn.us" tvg-name="CNN US" tvg-logo="http://example.com/cnn.png" tvg-chno="1" group-title="News",CNN US
http://example.com/cnn
#EXTINF:-1 tvg-id="bbc.news" tvg-name="BBC News" tvg-logo="http://example.com/bbc.png" tvg-chno="2" group-title="News",BBC News
http://example.com/bbc
#EXTINF:-1 tvg-id="espn.us" tvg-name="ESPN US" tvg-logo="http://example.com/espn.png" tvg-chno="100" group-title="Sports",ESPN US
http://example.com/espn
#EXTINF:-1 tvg-id="nba.tv" tvg-name="NBA TV" tvg-logo="http://example.com/nba.png" tvg-chno="101" group-title="Sports",NBA TV
http://example.com/nba
#EXTINF:-1 tvg-id="hbo.us" tvg-name="HBO US" tvg-logo="http://example.com/hbo.png" tvg-chno="200" group-title="Movies",HBO US
http://example.com/hbo
#EXTINF:-1 tvg-id="netflix" tvg-name="Netflix" tvg-logo="http://example.com/netflix.png" tvg-chno="201" group-title="Movies",Netflix Movies
http://example.com/netflix
`

	// Second test M3U with entertainment and documentaries
	testM3U2 := `#EXTM3U
#EXTINF:-1 tvg-id="fox.us" tvg-name="FOX US" tvg-logo="http://example.com/fox.png" tvg-chno="300" group-title="Entertainment",FOX US
http://example.com/fox
#EXTINF:-1 tvg-id="nbc.us" tvg-name="NBC US" tvg-logo="http://example.com/nbc.png" tvg-chno="301" group-title="Entertainment",NBC US
http://example.com/nbc
#EXTINF:-1 tvg-id="discovery" tvg-name="Discovery Channel" tvg-logo="http://example.com/discovery.png" tvg-chno="400" group-title="Documentary",Discovery Channel
http://example.com/discovery
#EXTINF:-1 tvg-id="natgeo" tvg-name="National Geographic" tvg-logo="http://example.com/natgeo.png" tvg-chno="401" group-title="Documentary",National Geographic
http://example.com/natgeo
`

	// Third test M3U with kids and music channels
	testM3U3 := `#EXTM3U
#EXTINF:-1 tvg-id="disney" tvg-name="Disney Channel" tvg-logo="http://example.com/disney.png" tvg-chno="500" group-title="Kids",Disney Channel
http://example.com/disney
#EXTINF:-1 tvg-id="nick" tvg-name="Nickelodeon" tvg-logo="http://example.com/nick.png" tvg-chno="501" group-title="Kids",Nickelodeon
http://example.com/nick
#EXTINF:-1 tvg-id="mtv" tvg-name="MTV" tvg-logo="http://example.com/mtv.png" tvg-chno="600" group-title="Music",MTV
http://example.com/mtv
#EXTINF:-1 tvg-id="vh1" tvg-name="VH1" tvg-logo="http://example.com/vh1.png" tvg-chno="601" group-title="Music",VH1
http://example.com/vh1
#EXTINF:-1 tvg-id="vevo" tvg-name="VEVO Hits" tvg-logo="http://example.com/vevo.png" tvg-chno="602" group-title="Music",VEVO Hits
http://example.com/vevo
`

	// Create temp directory
	require.NoError(t, os.MkdirAll(testConfig.TempPath, 0755))

	// Write all three M3U files
	m3uPath1 := filepath.Join(testConfig.TempPath, "test1.m3u")
	m3uPath2 := filepath.Join(testConfig.TempPath, "test2.m3u")
	m3uPath3 := filepath.Join(testConfig.TempPath, "test3.m3u")

	require.NoError(t, os.WriteFile(m3uPath1, []byte(testM3U1), 0644))
	require.NoError(t, os.WriteFile(m3uPath2, []byte(testM3U2), 0644))
	require.NoError(t, os.WriteFile(m3uPath3, []byte(testM3U3), 0644))

	// Set environment variables for all three M3Us
	os.Setenv("M3U_URL_1", fmt.Sprintf("file://%s", m3uPath1))
	os.Setenv("M3U_URL_2", fmt.Sprintf("file://%s", m3uPath2))
	os.Setenv("M3U_URL_3", fmt.Sprintf("file://%s", m3uPath3))
	os.Setenv("BASE_URL", "http://example.com")

	return func() {
		testDataLock.Lock()
		defer testDataLock.Unlock()

		config.SetConfig(originalConfig)
		utils.ResetCaches()

		os.RemoveAll(tempDir)

		os.Unsetenv("M3U_URL_1")
		os.Unsetenv("M3U_URL_2")
		os.Unsetenv("M3U_URL_3")
		os.Unsetenv("BASE_URL")
	}
}

type testStreamInfo struct {
	group string
	chno  string
	name  string
}

func parseM3UContent(content string) []testStreamInfo {
	var streams []testStreamInfo
	lines := strings.Split(content, "\n")
	var currentStream testStreamInfo

	for _, line := range lines {
		if strings.HasPrefix(line, "#EXTINF") {
			// Extract group-title
			if match := regexp.MustCompile(`group-title="([^"]+)"`).FindStringSubmatch(line); len(match) > 1 {
				currentStream.group = match[1]
			}
			// Extract tvg-chno
			if match := regexp.MustCompile(`tvg-chno="([^"]+)"`).FindStringSubmatch(line); len(match) > 1 {
				currentStream.chno = match[1]
			}
			// Extract name (after the comma)
			if idx := strings.LastIndex(line, ","); idx != -1 {
				currentStream.name = strings.TrimSpace(line[idx+1:])
			}
			streams = append(streams, currentStream)
			currentStream = testStreamInfo{}
		}
	}
	return streams
}

func newAutoChannelIconsTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/contents/countries/united-states", func(w http.ResponseWriter, r *http.Request) {
		t.Helper()
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode([]map[string]string{
			{
				"type":         "file",
				"name":         "discovery-channel-us.png",
				"path":         "countries/united-states/discovery-channel-us.png",
				"download_url": "http://" + r.Host + "/raw/discovery-channel-us.png",
			},
			{
				"type":         "file",
				"name":         "fox-us.png",
				"path":         "countries/united-states/fox-us.png",
				"download_url": "http://" + r.Host + "/raw/fox-us.png",
			},
			{
				"type": "dir",
				"name": "custom",
				"path": "countries/united-states/custom",
				"url":  "http://" + r.Host + "/contents/countries/united-states/custom",
			},
		}))
	})
	mux.HandleFunc("/contents/countries/united-states/custom", func(w http.ResponseWriter, r *http.Request) {
		t.Helper()
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode([]map[string]string{
			{
				"type":         "file",
				"name":         "cnn-us.png",
				"path":         "countries/united-states/custom/cnn-us.png",
				"download_url": "http://" + r.Host + "/raw/cnn-us.png",
			},
		}))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func useAutoChannelIconsTestServer(t *testing.T, server *httptest.Server) {
	t.Helper()

	originalBaseURL := tvLogosContentsBaseURL
	originalHTTPClient := tvLogosHTTPClient

	tvLogosContentsBaseURL = server.URL + "/contents/countries/united-states"
	tvLogosHTTPClient = server.Client()
	autoChannelIconCache.resetForTests()

	t.Cleanup(func() {
		tvLogosContentsBaseURL = originalBaseURL
		tvLogosHTTPClient = originalHTTPClient
		autoChannelIconCache.resetForTests()
	})
}

func TestRevalidatingGetM3U(t *testing.T) {
	// Subtests for RevalidatingGetM3U.
	tests := []struct {
		name          string
		sortingKey    string
		sortingDir    string
		setup         func(t *testing.T)
		validateOrder func(t *testing.T, streams []testStreamInfo)
	}{
		{
			name:       "default sorting",
			sortingKey: "",
			sortingDir: "asc",
			setup: func(t *testing.T) {
			},
			validateOrder: func(t *testing.T, streams []testStreamInfo) {
				// Verify all streams are present.
				assert.Equal(t, 15, len(streams), "Should have 15 channels")
				// Verify all expected groups appear.
				groups := make(map[string]bool)
				for _, s := range streams {
					groups[s.group] = true
				}
				expectedGroups := []string{"News", "Sports", "Movies", "Entertainment", "Documentary", "Kids", "Music"}
				for _, g := range expectedGroups {
					assert.True(t, groups[g], "Should contain group: %s", g)
				}
			},
		},
		{
			name:       "tvg-chno sorting",
			sortingKey: "tvg-chno",
			sortingDir: "asc",
			setup: func(t *testing.T) {
				// Set the sorting environment variables.
				os.Setenv("SORTING_KEY", "tvg-chno")
				os.Setenv("SORTING_DIRECTION", "asc")
			},
			validateOrder: func(t *testing.T, streams []testStreamInfo) {
				// Verify that channel numbers are in ascending order.
				var numbers []int
				for _, s := range streams {
					num, err := strconv.Atoi(s.chno)
					require.NoError(t, err)
					numbers = append(numbers, num)
				}
				for i := 1; i < len(numbers); i++ {
					assert.GreaterOrEqual(t, numbers[i], numbers[i-1],
						"Channel numbers should be in ascending order, got %d after %d",
						numbers[i], numbers[i-1])
				}

				// Also verify we have all the expected numbers.
				expectedNumbers := []int{1, 2, 100, 101, 200, 201, 300, 301, 400, 401, 500, 501, 600, 601, 602}
				sort.Ints(numbers)
				assert.Equal(t, expectedNumbers, numbers, "Should have all expected channel numbers in order")
			},
		},
		{
			name:       "group sorting",
			sortingKey: "tvg-group",
			sortingDir: "asc",
			setup: func(t *testing.T) {
				// Set group sorting to ascending.
				os.Setenv("SORTING_KEY", "tvg-group")
				os.Setenv("SORTING_DIRECTION", "asc")
			},
			validateOrder: func(t *testing.T, streams []testStreamInfo) {
				// Check that the groups are sorted alphabetically.
				lastGroup := ""
				for _, s := range streams {
					cmp := strings.Compare(s.group, lastGroup)
					assert.GreaterOrEqual(t, cmp, 0,
						"Groups should be in alphabetical order, got %s after %s",
						s.group, lastGroup)
					lastGroup = s.group
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanup := setupTestEnvironment(t)
			defer cleanup()

			tt.setup(t)

			processor := NewProcessor()

			req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := processor.Run(ctx, req)
			require.NoError(t, err)

			// Read the generated M3U file.
			content, err := os.ReadFile(processor.GetResultPath())
			require.NoError(t, err)
			streams := parseM3UContent(string(content))

			tt.validateOrder(t, streams)
		})
	}
}

func TestConcurrentAccess(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	const numGoroutines = 10
	done := make(chan bool, numGoroutines)

	processor := NewProcessor()

	// First request to initialize cache
	req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := processor.Run(ctx, req)
	require.NoError(t, err)

	// Make concurrent requests
	for i := 0; i < numGoroutines; i++ {
		go func() {
			content, err := os.ReadFile(processor.GetResultPath())
			require.NoError(t, err)
			result := string(content)

			assert.Contains(t, result, "#EXTM3U")
			assert.Contains(t, result, "CNN US") // Check at least one channel
			done <- true
		}()
	}

	// Wait for all goroutines
	for i := 0; i < numGoroutines; i++ {
		select {
		case <-done:
			// Success
		case <-time.After(5 * time.Second):
			t.Fatal("Timeout waiting for goroutine")
		}
	}
}

func TestHandleRemoteURLAcceptsBOMPrefixedM3U(t *testing.T) {
	expectedM3U := "\ufeff#EXTM3U\n#EXTINF:-1,Test\nhttp://example.com/stream.ts\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte(expectedM3U))
		require.NoError(t, err)
	}))
	defer server.Close()

	result := &SourceDownloaderResult{
		Index: "TEST",
		Lines: make(chan *LineDetails, 10),
		Error: make(chan error, 1),
	}

	handleRemoteURL(server.URL, result.Index, result)
	close(result.Lines)
	close(result.Error)

	var lines []string
	for line := range result.Lines {
		lines = append(lines, line.Content)
	}
	for err := range result.Error {
		require.NoError(t, err)
	}

	assert.Contains(t, strings.Join(lines, "\n"), "#EXTM3U")
	assert.Contains(t, strings.Join(lines, "\n"), "http://example.com/stream.ts")
}

func TestSortingVariations(t *testing.T) {
	sortingTests := []struct {
		name      string
		key       string
		direction string
		validate  func(t *testing.T, streams []testStreamInfo)
	}{
		{
			name:      "sort by name ascending",
			key:       "",
			direction: "asc",
			validate: func(t *testing.T, streams []testStreamInfo) {
				// Verify ALL channels are in ascending alphabetical order
				for i := 1; i < len(streams); i++ {
					assert.LessOrEqual(t,
						strings.ToLower(streams[i-1].name),
						strings.ToLower(streams[i].name),
						"Channel '%s' should come before '%s' in ascending order",
						streams[i-1].name, streams[i].name)
				}
			},
		},
		{
			name:      "sort by name descending",
			key:       "",
			direction: "desc",
			validate: func(t *testing.T, streams []testStreamInfo) {
				// Verify ALL channels are in descending alphabetical order
				for i := 1; i < len(streams); i++ {
					assert.GreaterOrEqual(t,
						strings.ToLower(streams[i-1].name),
						strings.ToLower(streams[i].name),
						"Channel '%s' should come after '%s' in descending order",
						streams[i-1].name, streams[i].name)
				}
			},
		},
		{
			name:      "sort by channel number ascending",
			key:       "tvg-chno",
			direction: "asc",
			validate: func(t *testing.T, streams []testStreamInfo) {
				// Verify ALL channel numbers are in ascending order
				var numbers []int
				for _, s := range streams {
					num, err := strconv.Atoi(s.chno)
					require.NoError(t, err, "Channel number should be numeric: %s", s.chno)
					numbers = append(numbers, num)
				}

				for i := 1; i < len(numbers); i++ {
					assert.LessOrEqual(t, numbers[i-1], numbers[i],
						"Channel number %d should come before %d in ascending order",
						numbers[i-1], numbers[i])
				}

				// Verify we have all expected numbers in order
				expectedNumbers := []int{1, 2, 100, 101, 200, 201, 300, 301, 400, 401, 500, 501, 600, 601, 602}
				assert.Equal(t, expectedNumbers, numbers, "Should have all expected channel numbers in ascending order")
			},
		},
		{
			name:      "sort by channel number descending",
			key:       "tvg-chno",
			direction: "desc",
			validate: func(t *testing.T, streams []testStreamInfo) {
				// Verify ALL channel numbers are in descending order
				var numbers []int
				for _, s := range streams {
					num, err := strconv.Atoi(s.chno)
					require.NoError(t, err, "Channel number should be numeric: %s", s.chno)
					numbers = append(numbers, num)
				}

				for i := 1; i < len(numbers); i++ {
					assert.GreaterOrEqual(t, numbers[i-1], numbers[i],
						"Channel number %d should come after %d in descending order",
						numbers[i-1], numbers[i])
				}

				// Verify we have all expected numbers in reverse order
				expectedNumbers := []int{602, 601, 600, 501, 500, 401, 400, 301, 300, 201, 200, 101, 100, 2, 1}
				assert.Equal(t, expectedNumbers, numbers, "Should have all expected channel numbers in descending order")
			},
		},
		{
			name:      "sort by group ascending",
			key:       "tvg-group",
			direction: "asc",
			validate: func(t *testing.T, streams []testStreamInfo) {
				// Verify ALL groups are in alphabetical order
				for i := 1; i < len(streams); i++ {
					assert.LessOrEqual(t,
						strings.ToLower(streams[i-1].group),
						strings.ToLower(streams[i].group),
						"Group '%s' should come before '%s' in ascending order",
						streams[i-1].group, streams[i].group)
				}

				// Verify expected group order
				expectedGroupOrder := []string{
					"Documentary", "Documentary", // 2 channels
					"Entertainment", "Entertainment", // 2 channels
					"Kids", "Kids", // 2 channels
					"Movies", "Movies", // 2 channels
					"Music", "Music", "Music", // 3 channels
					"News", "News", // 2 channels
					"Sports", "Sports", // 2 channels
				}
				actualGroups := make([]string, len(streams))
				for i, s := range streams {
					actualGroups[i] = s.group
				}
				assert.Equal(t, expectedGroupOrder, actualGroups, "Groups should be in alphabetical order")
			},
		},
		{
			name:      "sort by group descending",
			key:       "tvg-group",
			direction: "desc",
			validate: func(t *testing.T, streams []testStreamInfo) {
				// Verify ALL groups are in reverse alphabetical order
				for i := 1; i < len(streams); i++ {
					assert.GreaterOrEqual(t,
						strings.ToLower(streams[i-1].group),
						strings.ToLower(streams[i].group),
						"Group '%s' should come after '%s' in descending order",
						streams[i-1].group, streams[i].group)
				}

				// Verify expected group order (reverse)
				expectedGroupOrder := []string{
					"Sports", "Sports", // 2 channels
					"News", "News", // 2 channels
					"Music", "Music", "Music", // 3 channels
					"Movies", "Movies", // 2 channels
					"Kids", "Kids", // 2 channels
					"Entertainment", "Entertainment", // 2 channels
					"Documentary", "Documentary", // 2 channels
				}
				actualGroups := make([]string, len(streams))
				for i, s := range streams {
					actualGroups[i] = s.group
				}
				assert.Equal(t, expectedGroupOrder, actualGroups, "Groups should be in reverse alphabetical order")
			},
		},
	}

	for _, tt := range sortingTests {
		t.Run(tt.name, func(t *testing.T) {
			cleanup := setupTestEnvironment(t)
			defer cleanup()

			// Set sorting environment variables
			os.Setenv("SORTING_KEY", tt.key)
			os.Setenv("SORTING_DIRECTION", tt.direction)
			defer func() {
				os.Unsetenv("SORTING_KEY")
				os.Unsetenv("SORTING_DIRECTION")
			}()

			req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
			processor := NewProcessor()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := processor.Run(ctx, req)
			require.NoError(t, err)

			content, err := os.ReadFile(processor.GetResultPath())
			require.NoError(t, err)

			streams := parseM3UContent(string(content))
			require.Equal(t, 15, len(streams), "Should have 15 channels")

			tt.validate(t, streams)
		})
	}
}

func TestMergeAttributesToM3UFile(t *testing.T) {
	os.Setenv("BASE_URL", "http://example.com")
	defer os.Unsetenv("BASE_URL")

	m3u1 := `#EXTINF:-1 tvg-chno="010",First Channel`
	url1 := "http://example.com/source1"
	s1 := parseLine(m3u1, &LineDetails{Content: url1, LineNum: 1}, "M3U_Test")
	require.NotNil(t, s1, "Failed to parse source 1")

	m3u2 := `#EXTINF:-1 tvg-id="id-2" tvg-chno="010" tvg-name="First Channel" tvg-type="type-2",First Channel`
	url2 := "http://example.com/source2"
	s2 := parseLine(m3u2, &LineDetails{Content: url2, LineNum: 2}, "M3U_Test")
	require.NotNil(t, s2, "Failed to parse source 2")

	m3u3 := `#EXTINF:-1 tvg-chno="010" tvg-name="First Channel" group-title="Group-3",First Channel`
	url3 := "http://example.com/source3"
	s3 := parseLine(m3u3, &LineDetails{Content: url3, LineNum: 3}, "M3U_Test")
	require.NotNil(t, s3, "Failed to parse source 3")

	m3u4 := `#EXTINF:-1 tvg-chno="010" tvg-name="First Channel" tvg-logo="http://logo/source4.png",First Channel`
	url4 := "http://example.com/source4"
	s4 := parseLine(m3u4, &LineDetails{Content: url4, LineNum: 4}, "M3U_Test")
	require.NotNil(t, s4, "Failed to parse source 4")

	m3u5 := `#EXTINF:-1 tvg-id="id-5" tvg-chno="010" tvg-name="First Channel",First Channel`
	url5 := "http://example.com/source5"
	s5 := parseLine(m3u5, &LineDetails{Content: url5, LineNum: 5}, "M3U_Test")
	require.NotNil(t, s5, "Failed to parse source 5")

	s1 = mergeStreamInfoAttributes(s1, s2)
	s1 = mergeStreamInfoAttributes(s1, s3)
	s1 = mergeStreamInfoAttributes(s1, s4)
	s1 = mergeStreamInfoAttributes(s1, s5)

	baseURL := "http://dummy" // base URL for stream generation
	entry := formatStreamEntry(baseURL, s1)
	m3uContent := "#EXTM3U\n" + entry

	tempFile, err := os.CreateTemp("", "merged-*.m3u")
	require.NoError(t, err)
	defer os.Remove(tempFile.Name())

	_, err = tempFile.Write([]byte(m3uContent))
	require.NoError(t, err)
	tempFile.Close()

	contentFromFile, err := os.ReadFile(tempFile.Name())
	require.NoError(t, err)
	contentStr := string(contentFromFile)

	parsedStreams := parseM3UContent(contentStr)
	require.Len(t, parsedStreams, 1, "Should have one stream entry in the parsed M3U content")

	parsed := parsedStreams[0]
	assert.Equal(t, "Group-3", parsed.group, "Group should be 'Group-3'")
	assert.Equal(t, "010", parsed.chno, "Channel number should be '010'")
	assert.Equal(t, "First Channel", parsed.name, "Channel name should be 'First Channel'")

	assert.Contains(t, contentStr, `tvg-id="id-2"`, "Should contain tvg-id from merged attributes")
	assert.Contains(t, contentStr, `tvg-type="type-2"`, "Should contain tvg-type from merged attributes")
	assert.Contains(t, contentStr, `tvg-logo="http://example.com/a/aHR0cDovL2xvZ28vc291cmNlNC5wbmc="`, "Should contain tvg-logo from merged attributes")
}

func TestParseLine_AutoAssignsLogoFromTVLogoRepository(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	server := newAutoChannelIconsTestServer(t)
	useAutoChannelIconsTestServer(t, server)
	t.Setenv("AUTO_RETRIEVE_CHANNEL_ICONS", "true")

	stream := parseLine(
		`#EXTINF:-1 tvg-id="discovery.us" tvg-name="Discovery Channel",Discovery Channel`,
		&LineDetails{Content: "http://example.com/discovery", LineNum: 1},
		"M3U_Test",
	)
	require.NotNil(t, stream)

	assert.True(t, stream.AutoLogoURL)
	assert.Equal(t, utils.TvgLogoParser(server.URL+"/raw/discovery-channel-us.png"), stream.LogoURL)
}

func TestParseLine_AutoAssignsLogoAfterTrimmingTitleSuffixes(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	server := newAutoChannelIconsTestServer(t)
	useAutoChannelIconsTestServer(t, server)
	t.Setenv("AUTO_RETRIEVE_CHANNEL_ICONS", "true")

	stream := parseLine(
		`#EXTINF:-1 tvg-id="cnn.us" tvg-name="CNN US HD",CNN US HD`,
		&LineDetails{Content: "http://example.com/cnn", LineNum: 1},
		"M3U_Test",
	)
	require.NotNil(t, stream)

	assert.True(t, stream.AutoLogoURL)
	assert.Equal(t, utils.TvgLogoParser(server.URL+"/raw/cnn-us.png"), stream.LogoURL)
}

func TestFormatStreamEntry_UsesSourceURLWhenDirectSourceProxyingEnabled(t *testing.T) {
	t.Setenv("DIRECT_SOURCE_PROXYING", "true")

	stream := &StreamInfo{
		Title:     "Discovery Channel",
		SourceURL: "http://example-source.test/discovery",
		URLs:      xsync.NewMapOf[string, map[string]string](),
	}
	stream.URLs.Store("1", map[string]string{
		"hash": "1:::http://example-source.test/discovery",
	})

	entry := formatStreamEntry("http://proxy.example.com", stream)
	assert.Contains(t, entry, "\nhttp://example-source.test/discovery\n")
	assert.NotContains(t, entry, "http://proxy.example.com/p/")
}

func TestMergeStreamInfoAttributes_PrefersSourceLogoOverAutoRetrievedLogo(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	server := newAutoChannelIconsTestServer(t)
	useAutoChannelIconsTestServer(t, server)
	t.Setenv("AUTO_RETRIEVE_CHANNEL_ICONS", "true")

	autoStream := parseLine(
		`#EXTINF:-1 tvg-id="fox.us" tvg-name="FOX US",FOX US`,
		&LineDetails{Content: "http://example.com/fox-auto", LineNum: 1},
		"M3U_Test",
	)
	require.NotNil(t, autoStream)
	require.True(t, autoStream.AutoLogoURL)

	sourceLogoStream := parseLine(
		`#EXTINF:-1 tvg-id="fox.us" tvg-name="FOX US" tvg-logo="http://logo/fox-source.png",FOX US`,
		&LineDetails{Content: "http://example.com/fox-source", LineNum: 2},
		"M3U_Test",
	)
	require.NotNil(t, sourceLogoStream)
	require.False(t, sourceLogoStream.AutoLogoURL)

	merged := mergeStreamInfoAttributes(autoStream, sourceLogoStream)
	assert.False(t, merged.AutoLogoURL)
	assert.Equal(t, utils.TvgLogoParser("http://logo/fox-source.png"), merged.LogoURL)
}

func TestProcessorRun_EmbedsEPGURLInPlaylistHeader(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	t.Setenv("EMBEDDED_EPG_URL", "https://epg.example.com/guide.xml.gz")

	processor := NewProcessor()
	require.NotNil(t, processor)

	req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
	require.NoError(t, processor.Run(context.Background(), req))

	content, err := os.ReadFile(processor.GetResultPath())
	require.NoError(t, err)

	lines := strings.SplitN(string(content), "\n", 2)
	require.NotEmpty(t, lines)
	assert.Equal(
		t,
		`#EXTM3U x-tvg-url="https://epg.example.com/guide.xml.gz" url-tvg="https://epg.example.com/guide.xml.gz"`,
		lines[0],
	)
}

func TestProcessorRun_IgnoresInvalidEmbeddedEPGURL(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	t.Setenv("EMBEDDED_EPG_URL", "not-a-valid-url")

	processor := NewProcessor()
	require.NotNil(t, processor)

	req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
	require.NoError(t, processor.Run(context.Background(), req))

	content, err := os.ReadFile(processor.GetResultPath())
	require.NoError(t, err)

	lines := strings.SplitN(string(content), "\n", 2)
	require.NotEmpty(t, lines)
	assert.Equal(t, "#EXTM3U", lines[0])
}

func TestSortingManager_DedupesCanonicalChannelAcrossCaseAndSpacing(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	manager := newSortingManager()
	defer manager.Close()

	makeStream := func(title, sourceM3U, urlKey, url string, index int) *StreamInfo {
		stream := &StreamInfo{
			Title:       title,
			SourceM3U:   sourceM3U,
			SourceIndex: index,
			URLs:        xsync.NewMapOf[string, map[string]string](),
		}
		stream.URLs.Store(sourceM3U, map[string]string{urlKey: fmt.Sprintf("%d:::%s", index, url)})
		return stream
	}

	first := makeStream("A & E", "1", "hash1", "http://example.com/aande-1", 1)
	second := makeStream("a&e", "2", "hash2", "http://example.com/aande-2", 2)

	require.NoError(t, manager.AddToSorter(first))
	require.NoError(t, manager.AddToSorter(second))

	var entries []*StreamInfo
	require.NoError(t, manager.GetSortedEntries(func(stream *StreamInfo) {
		entries = append(entries, stream)
	}))

	require.Len(t, entries, 1, "Canonical title variants should merge into one channel entry")
	assert.Equal(t, "1", entries[0].SourceM3U, "Merged channel should keep the first source as primary")
}

func TestSortingManager_MergesAttributesForDuplicateChannelBeforeFlush(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	manager := newSortingManager()
	defer manager.Close()

	makeStream := func(title, sourceM3U, tvgID, group, urlKey, url string, index int) *StreamInfo {
		stream := &StreamInfo{
			Title:       title,
			TvgID:       tvgID,
			Group:       group,
			SourceM3U:   sourceM3U,
			SourceIndex: index,
			URLs:        xsync.NewMapOf[string, map[string]string](),
		}
		stream.URLs.Store(sourceM3U, map[string]string{urlKey: fmt.Sprintf("%d:::%s", index, url)})
		return stream
	}

	first := makeStream("Discovery Channel", "1", "", "", "hash1", "http://example.com/discovery-a", 10)
	second := makeStream("Discovery Channel", "2", "discovery.2", "Documentary", "hash2", "http://example.com/discovery-b", 20)

	require.NoError(t, manager.AddToSorter(first))
	require.NoError(t, manager.AddToSorter(second))

	var entries []*StreamInfo
	require.NoError(t, manager.GetSortedEntries(func(stream *StreamInfo) {
		entries = append(entries, stream)
	}))

	require.Len(t, entries, 1, "Duplicate channel titles should collapse into one playlist entry")
	assert.Equal(t, "1", entries[0].SourceM3U, "Merged channel should keep the earliest source as primary")
	assert.Equal(t, "discovery.2", entries[0].TvgID, "Merged channel should retain attributes contributed by later duplicates")
	assert.Equal(t, "Documentary", entries[0].Group, "Merged channel should keep missing metadata filled from duplicates")
}

func TestProcessorRun_SynthesizesDiscoveredM3U8SourceIntoPlaylist(t *testing.T) {
	originalConfig := config.GetConfig()
	tempDir, err := os.MkdirTemp("", "discovery-m3u8-*")
	require.NoError(t, err)

	originalIncludeRegexes := includeRegexes
	originalExcludeRegexes := excludeRegexes
	originalRules := channelRules
	originalMerges := channelMerges

	config.SetConfig(&config.Config{
		DataPath: filepath.Join(tempDir, "data"),
		TempPath: filepath.Join(tempDir, "temp"),
	})
	utils.ResetCaches()
	utils.SetDynamicSources(nil)
	includeRegexes = nil
	excludeRegexes = nil
	channelRules = nil
	channelMerges = nil
	filterOnce = sync.Once{}

	t.Setenv("BASE_URL", "http://example.com")

	defer func() {
		config.SetConfig(originalConfig)
		utils.ResetCaches()
		utils.SetDynamicSources(nil)
		includeRegexes = originalIncludeRegexes
		excludeRegexes = originalExcludeRegexes
		channelRules = originalRules
		channelMerges = originalMerges
		filterOnce = sync.Once{}
		_ = os.RemoveAll(tempDir)
	}()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/provider/master.m3u8":
			fmt.Fprintf(
				w,
				"#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=250000\n%s/provider/variant.m3u8\n",
				server.URL,
			)
		case "/provider/variant.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\nsegment1.ts\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	discoveredURL := server.URL + "/provider/master.m3u8"
	utils.SetDynamicSources([]utils.SourceConfig{
		{
			Index:          "DISC_1_TEST",
			URL:            discoveredURL,
			Name:           "Custom Sports",
			Group:          "Provider Crawl",
			MaxConcurrency: 1,
			ContainsVOD:    true,
		},
	})

	processor := NewProcessor()
	require.NotNil(t, processor)
	require.NoError(t, processor.Run(context.Background(), nil))

	content, err := os.ReadFile(processor.GetResultPath())
	require.NoError(t, err)

	parsedStreams := parseM3UContent(string(content))
	require.Len(t, parsedStreams, 1, "discovered m3u8 sources should become a single playlist entry")
	assert.Equal(t, "Provider Crawl", parsedStreams[0].group)
	assert.Equal(t, "Custom Sports", parsedStreams[0].name)

	var playlistURL string
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "http://example.com/p/") {
			playlistURL = strings.TrimSpace(line)
			break
		}
	}
	require.NotEmpty(t, playlistURL, "compiled playlist should contain a proxied stream URL")

	parsedPlaylistURL, err := url.Parse(playlistURL)
	require.NoError(t, err)
	slug := path.Base(parsedPlaylistURL.Path)
	slug = strings.TrimSuffix(slug, path.Ext(slug))
	require.NotEmpty(t, slug)

	streamInfo, err := ParseStreamInfoBySlug(slug)
	require.NoError(t, err)

	urlsBySource, ok := streamInfo.URLs.Load("DISC_1_TEST")
	require.True(t, ok, "slug lookup should reload the discovered source URL")

	var loadedURL string
	for _, entry := range urlsBySource {
		parts := strings.SplitN(entry, ":::", 2)
		if len(parts) == 2 {
			loadedURL = parts[1]
			break
		}
	}
	assert.Equal(t, discoveredURL, loadedURL)

	utils.SetDynamicSources(nil)

	fallbackStreamInfo, err := ParseStreamInfoBySlug(slug)
	require.NoError(t, err)

	fallbackURLsBySource, ok := fallbackStreamInfo.URLs.Load("DISC_1_TEST")
	require.True(t, ok, "slug lookup should preserve the discovered source even without live dynamic source registration")

	loadedURL = ""
	for _, entry := range fallbackURLsBySource {
		parts := strings.SplitN(entry, ":::", 2)
		if len(parts) == 2 {
			loadedURL = parts[1]
			break
		}
	}
	assert.Equal(t, discoveredURL, loadedURL)
}
