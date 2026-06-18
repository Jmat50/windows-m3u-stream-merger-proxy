package utils

import "testing"

func TestNormalizeEmbeddedEPGURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantURL string
		wantOK  bool
	}{
		{
			name:    "valid https url",
			input:   "https://epg.example.com/guide.xml.gz",
			wantURL: "https://epg.example.com/guide.xml.gz",
			wantOK:  true,
		},
		{
			name:    "valid http url with whitespace",
			input:   "  http://epg.example.com/guide.xml?token=123  ",
			wantURL: "http://epg.example.com/guide.xml?token=123",
			wantOK:  true,
		},
		{
			name:   "rejects empty value",
			input:  "   ",
			wantOK: false,
		},
		{
			name:   "rejects non http scheme",
			input:  "ftp://epg.example.com/guide.xml",
			wantOK: false,
		},
		{
			name:   "rejects malformed url",
			input:  "not-a-url",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, gotOK := NormalizeEmbeddedEPGURL(tt.input)
			if gotOK != tt.wantOK {
				t.Fatalf("expected ok=%t, got %t", tt.wantOK, gotOK)
			}
			if gotURL != tt.wantURL {
				t.Fatalf("expected normalized url %q, got %q", tt.wantURL, gotURL)
			}
		})
	}
}

func TestBuildPlaylistHeaderLine(t *testing.T) {
	t.Run("builds embedded epg header", func(t *testing.T) {
		t.Setenv("EMBEDDED_EPG_URL", "https://epg.example.com/guide.xml.gz")

		got := BuildPlaylistHeaderLine()
		want := "#EXTM3U x-tvg-url=\"https://epg.example.com/guide.xml.gz\" url-tvg=\"https://epg.example.com/guide.xml.gz\"\n"
		if got != want {
			t.Fatalf("expected %q, got %q", want, got)
		}
	})

	t.Run("falls back to plain header for invalid url", func(t *testing.T) {
		t.Setenv("EMBEDDED_EPG_URL", "bad url")

		got := BuildPlaylistHeaderLine()
		if got != "#EXTM3U\n" {
			t.Fatalf("expected plain header, got %q", got)
		}
	})

	t.Run("builds comma-separated embedded epg header", func(t *testing.T) {
		t.Setenv(
			"EMBEDDED_EPG_URL",
			"https://epg1.example.com/guide.xml.gz,https://epg2.example.com/guide.xml",
		)

		got := BuildPlaylistHeaderLine()
		want := "#EXTM3U x-tvg-url=\"https://epg1.example.com/guide.xml.gz,https://epg2.example.com/guide.xml\" " +
			"url-tvg=\"https://epg1.example.com/guide.xml.gz,https://epg2.example.com/guide.xml\"\n"
		if got != want {
			t.Fatalf("expected %q, got %q", want, got)
		}
	})

	t.Run("ignores invalid entries in comma-separated list", func(t *testing.T) {
		t.Setenv("EMBEDDED_EPG_URL", "not-a-url,https://epg.example.com/guide.xml")

		gotURL, gotOK := GetEmbeddedEPGURL()
		if !gotOK {
			t.Fatalf("expected valid urls to be accepted")
		}
		if gotURL != "https://epg.example.com/guide.xml" {
			t.Fatalf("expected only valid url to remain, got %q", gotURL)
		}
	})
}

func TestIsMergeEPGForSameChannelNumberEnabled(t *testing.T) {
	t.Run("defaults to false", func(t *testing.T) {
		t.Setenv("MERGE_EPG_FOR_SAME_CHANNEL_NUMBER", "")
		if IsMergeEPGForSameChannelNumberEnabled() {
			t.Fatalf("expected disabled by default")
		}
	})

	t.Run("parses true", func(t *testing.T) {
		t.Setenv("MERGE_EPG_FOR_SAME_CHANNEL_NUMBER", "true")
		if !IsMergeEPGForSameChannelNumberEnabled() {
			t.Fatalf("expected enabled")
		}
	})
}

func TestIsChannelEPGMergeActive(t *testing.T) {
	t.Setenv("MERGE_EPG_FOR_SAME_CHANNEL_NUMBER", "true")
	t.Setenv("EMBEDDED_EPG_URL", "https://epg.example.com/guide.xml")

	if !IsChannelEPGMergeActive() {
		t.Fatalf("expected active when both settings are enabled")
	}

	t.Setenv("MERGE_EPG_FOR_SAME_CHANNEL_NUMBER", "false")
	if IsChannelEPGMergeActive() {
		t.Fatalf("expected inactive when merge toggle is off")
	}
}
