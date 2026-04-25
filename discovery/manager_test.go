package discovery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/utils"
)

func TestLoadJobsFromEnv(t *testing.T) {
	utils.ResetCaches()
	defer utils.ResetCaches()

	t.Setenv("DISCOVERY_JOB_2", `{"name":"Two","start_url":"https://example.com/two","scan_interval_minutes":30}`)
	t.Setenv("DISCOVERY_JOB_1", `{"name":"One","start_url":"https://example.com/one","recursive":false,"follow_robots":false}`)

	jobs, err := LoadJobsFromEnv()
	if err != nil {
		t.Fatalf("LoadJobsFromEnv() error = %v", err)
	}

	if len(jobs) != 2 {
		t.Fatalf("LoadJobsFromEnv() len = %d, want 2", len(jobs))
	}

	if jobs[0].Name != "One" || jobs[0].ID != "1" {
		t.Fatalf("jobs[0] = %+v, want name=One id=1", jobs[0])
	}
	if jobs[0].Recursive {
		t.Fatalf("jobs[0].Recursive = true, want false")
	}
	if jobs[0].FollowRobots {
		t.Fatalf("jobs[0].FollowRobots = true, want false")
	}
	if jobs[1].Name != "Two" || jobs[1].ScanIntervalMinutes != 30 {
		t.Fatalf("jobs[1] = %+v, want name=Two interval=30", jobs[1])
	}
}

func TestCrawlerDiscoversRecursiveAndSitemapPlaylists(t *testing.T) {
	var serverURL string

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "User-agent: *\nSitemap: %s/sitemap.xml\n", serverURL)
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(
			w,
			`<?xml version="1.0" encoding="UTF-8"?><urlset><url><loc>%s/sitemap-playlist.m3u8</loc></url></urlset>`,
			serverURL,
		)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><a href="/nested">nested</a></body></html>`)
	})
	mux.HandleFunc("/nested", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><a href="/inline-playlist.m3u?token=abc">playlist</a></body></html>`)
	})
	mux.HandleFunc("/inline-playlist.m3u", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXTINF:-1,Inline\nhttp://example.com/stream.ts\n")
	})
	mux.HandleFunc("/sitemap-playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n")
	})

	server := httptest.NewServer(mux)
	defer server.Close()
	serverURL = server.URL

	job := Job{
		ID:                  "1",
		Name:                "Test Discovery",
		StartURL:            server.URL,
		ScanIntervalMinutes: 60,
		Recursive:           true,
		MaxDepth:            2,
		MaxPages:            20,
		FollowRobots:        true,
		SourceConcurrency:   1,
		Enabled:             true,
	}

	crawler, err := newCrawler(job, logger.Default)
	if err != nil {
		t.Fatalf("newCrawler() error = %v", err)
	}

	got, err := crawler.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	want := map[string]struct{}{
		server.URL + "/inline-playlist.m3u?token=abc": {},
		server.URL + "/sitemap-playlist.m3u8":         {},
	}

	if len(got) != len(want) {
		t.Fatalf("Discover() len = %d, want %d (%v)", len(got), len(want), got)
	}

	for _, value := range got {
		if _, ok := want[value]; !ok {
			t.Fatalf("Discover() unexpected url %q (full=%v)", value, got)
		}
	}
}

func TestCrawlerDiscoversPlaylistViaScriptLoadedPage(t *testing.T) {
	var serverURL string

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><script>$.ajax({url:"schedule_table.php",cache:false});</script></body></html>`)
	})
	mux.HandleFunc("/schedule_table.php", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(
			w,
			`<html><body><a href="np_fluidtv.php?channel=%s/player/master.m3u8" target="player_frame">fluidtv</a></body></html>`,
			serverURL,
		)
	})
	mux.HandleFunc("/np_fluidtv.php", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(
			w,
			`<html><body><video><source src="%s/player/master.m3u8" type="application/x-mpegURL"/></video></body></html>`,
			serverURL,
		)
	})
	mux.HandleFunc("/player/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n")
	})

	server := httptest.NewServer(mux)
	defer server.Close()
	serverURL = server.URL

	job := Job{
		ID:                  "1",
		Name:                "Script Discovery",
		StartURL:            server.URL,
		ScanIntervalMinutes: 60,
		Recursive:           true,
		MaxDepth:            2,
		MaxPages:            20,
		FollowRobots:        true,
		SourceConcurrency:   1,
		Enabled:             true,
	}

	crawler, err := newCrawler(job, logger.Default)
	if err != nil {
		t.Fatalf("newCrawler() error = %v", err)
	}

	got, err := crawler.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	want := server.URL + "/player/master.m3u8"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Discover() = %v, want [%q]", got, want)
	}
}

func TestValidatePlaylistAcceptsRedirectedPlaylist(t *testing.T) {
	redirectTarget := ""
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<html><body><a href="/playlist.m3u">link</a></body></html>`)
		case "/playlist.m3u":
			http.Redirect(w, r, redirectTarget, http.StatusFound)
		case "/actual.m3u":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:-1,Redirected\nhttp://example.com/stream.ts\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer serverA.Close()
	redirectTarget = serverA.URL + "/actual.m3u"

	job := Job{
		ID:                  "1",
		Name:                "Redirect Discovery",
		StartURL:            serverA.URL,
		ScanIntervalMinutes: 60,
		Recursive:           true,
		MaxDepth:            2,
		MaxPages:            20,
		FollowRobots:        false,
		SourceConcurrency:   1,
		Enabled:             true,
	}

	crawler, err := newCrawler(job, logger.Default)
	if err != nil {
		t.Fatalf("newCrawler() error = %v", err)
	}

	url, ok := crawler.validatePlaylist(context.Background(), make(map[string]string), serverA.URL+"/playlist.m3u")
	if !ok {
		t.Fatalf("validatePlaylist() returned false, expected true")
	}
	if url != serverA.URL+"/actual.m3u" {
		t.Fatalf("validatePlaylist() returned %q, want %q", url, serverA.URL+"/actual.m3u")
	}
}

func TestCrawlerDiscoversExternalPlaylistHost(t *testing.T) {
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/playlist.m3u8" {
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer external.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<html><body><a href="%s/playlist.m3u8">external playlist</a></body></html>`, external.URL)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	job := Job{
		ID:                  "1",
		Name:                "External Playlist Discovery",
		StartURL:            server.URL,
		ScanIntervalMinutes: 60,
		Recursive:           true,
		MaxDepth:            2,
		MaxPages:            20,
		FollowRobots:        false,
		SourceConcurrency:   1,
		Enabled:             true,
	}

	crawler, err := newCrawler(job, logger.Default)
	if err != nil {
		t.Fatalf("newCrawler() error = %v", err)
	}

	got, err := crawler.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	want := []string{external.URL + "/playlist.m3u8"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("Discover() = %v, want %v", got, want)
	}
}

func TestIsM3UContentHandlesBOM(t *testing.T) {
	if !utils.IsM3UContent([]byte("\ufeff#EXTM3U\n#EXTINF:-1,Test\nhttp://example.com/stream.ts\n")) {
		t.Fatal("IsM3UContent() should return true for BOM-prefixed playlists")
	}
}

func TestPublishDynamicSourcesLockedHonorsDiscoveredSourceOverrides(t *testing.T) {
	utils.ResetCaches()
	defer utils.ResetCaches()

	manager := &Manager{
		jobs: []Job{
			{ID: "1", Name: "Job One"},
			{ID: "2", Name: "Job Two"},
		},
		sourcesByJob: map[string][]utils.SourceConfig{
			"1": {
				{Index: "DISC_1_AAAA", URL: "https://example.com/a.m3u8", Group: "Job One"},
				{Index: "DISC_1_BBBB", URL: "https://example.com/b.m3u8", Group: "Job One"},
			},
			"2": {
				{Index: "DISC_2_CCCC", URL: "https://example.com/c.m3u8", Group: "Job Two"},
			},
		},
		sourceOverrides: map[string]discoveredSourceOverride{
			"DISC_1_BBBB": {
				Index:   "DISC_1_BBBB",
				Enabled: false,
			},
			"DISC_2_CCCC": {
				Index:   "DISC_2_CCCC",
				Name:    "Custom C",
				Enabled: true,
			},
		},
	}

	manager.publishDynamicSourcesLocked()

	sources := utils.GetSourceConfigs()
	if len(sources) != 2 {
		t.Fatalf("GetSourceConfigs() len = %d, want 2 (%v)", len(sources), sources)
	}

	if sources[0].Index != "DISC_1_AAAA" {
		t.Fatalf("sources[0].Index = %q, want DISC_1_AAAA", sources[0].Index)
	}
	if sources[1].Index != "DISC_2_CCCC" {
		t.Fatalf("sources[1].Index = %q, want DISC_2_CCCC", sources[1].Index)
	}
	if sources[1].Name != "Custom C" {
		t.Fatalf("sources[1].Name = %q, want Custom C", sources[1].Name)
	}

	discovered := manager.GetDiscoveredSources()
	if len(discovered) != 3 {
		t.Fatalf("GetDiscoveredSources() len = %d, want 3 (%v)", len(discovered), discovered)
	}

	enabledByIndex := make(map[string]bool, len(discovered))
	for _, source := range discovered {
		enabledByIndex[source.Index] = source.Enabled
	}

	if !enabledByIndex["DISC_1_AAAA"] {
		t.Fatal("DISC_1_AAAA should be enabled")
	}
	if enabledByIndex["DISC_1_BBBB"] {
		t.Fatal("DISC_1_BBBB should be disabled by override")
	}
	if !enabledByIndex["DISC_2_CCCC"] {
		t.Fatal("DISC_2_CCCC should remain enabled")
	}
}
