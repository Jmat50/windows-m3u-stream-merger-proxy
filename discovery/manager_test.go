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
