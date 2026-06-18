package sourceproc

import (
	"os"
	"strings"
	"testing"

	"github.com/puzpuzpuz/xsync/v3"
)

func TestPickBestTvgID_PrefersEPGMatch(t *testing.T) {
	index := &EPGChannelIndex{
		exact:  map[string]struct{}{"gameshow.us": {}},
		folded: map[string]string{"gameshow.us": "gameshow.us"},
	}

	got := pickBestTvgID([]epgCandidate{
		{tvgID: "GSN", sourceIndex: 1, order: 0},
		{tvgID: "gameshow.us", sourceIndex: 2, order: 1},
	}, index)

	if got != "gameshow.us" {
		t.Fatalf("expected gameshow.us, got %q", got)
	}
}

func TestPickBestTvgID_FallbackWithoutIndex(t *testing.T) {
	got := pickBestTvgID([]epgCandidate{
		{tvgID: "first.id", sourceIndex: 1, order: 0},
		{tvgID: "second.id", sourceIndex: 2, order: 1},
	}, &EPGChannelIndex{})

	if got != "first.id" {
		t.Fatalf("expected first.id, got %q", got)
	}
}

func TestMergeRecordsPerSourceEPGMeta(t *testing.T) {
	first := &StreamInfo{
		Title:     "Game Show Network",
		TvgID:     "GSN",
		SourceM3U: "1",
		URLs:      xsync.NewMapOf[string, map[string]string](),
	}
	first.URLs.Store("1", map[string]string{"a": "1:::http://example.com/gsn"})

	second := &StreamInfo{
		Title:     "Game Show Network",
		TvgID:     "gameshow.us",
		SourceM3U: "2",
		URLs:      xsync.NewMapOf[string, map[string]string](),
	}
	second.URLs.Store("2", map[string]string{"b": "2:::http://example.com/sid"})

	merged := mergeStreamInfoAttributes(first, second)

	if merged.SourceEPGMeta["1"].TvgID != "GSN" {
		t.Fatalf("expected source 1 tvg-id GSN, got %q", merged.SourceEPGMeta["1"].TvgID)
	}
	if merged.SourceEPGMeta["2"].TvgID != "gameshow.us" {
		t.Fatalf("expected source 2 tvg-id gameshow.us, got %q", merged.SourceEPGMeta["2"].TvgID)
	}
}

func TestExpandStreamsForCompile_EmitsPerSourceRows(t *testing.T) {
	t.Setenv("MERGE_EPG_FOR_SAME_CHANNEL_NUMBER", "true")
	t.Setenv("EMBEDDED_EPG_URL", "https://epg.example.com/guide.xml")

	stream := &StreamInfo{
		Title:     "Game Show Network",
		TvgID:     "GSN",
		SourceM3U: "1",
		URLs:      xsync.NewMapOf[string, map[string]string](),
		SourceEPGMeta: map[string]SourceEPGMeta{
			"1": {TvgID: "GSN"},
			"2": {TvgID: "gameshow.us"},
		},
	}
	stream.URLs.Store("1", map[string]string{"a": "1:::http://example.com/gsn"})
	stream.URLs.Store("2", map[string]string{"b": "2:::http://example.com/sid"})

	groups := []ChannelNumberGroup{
		{
			Number:    64,
			Canonical: "Game Show Network",
			Entries: []ChannelNumberGroupEntry{
				{SourceIndex: 1, ChannelTitle: "Game Show Network", SubNumber: "64.1"},
				{SourceIndex: 2, ChannelTitle: "Game Show Network", SubNumber: "64.2"},
			},
		},
	}

	index := &EPGChannelIndex{
		exact:  map[string]struct{}{"gameshow.us": {}},
		folded: map[string]string{"gameshow.us": "gameshow.us"},
	}

	expanded := expandStreamsForCompile([]*StreamInfo{stream}, groups, index)
	if len(expanded) != 2 {
		t.Fatalf("expected 2 playlist rows, got %d", len(expanded))
	}

	for _, row := range expanded {
		if row.TvgID != "gameshow.us" {
			t.Fatalf("expected shared tvg-id gameshow.us, got %q", row.TvgID)
		}
		if row.URLs.Size() != 1 {
			t.Fatalf("expected single-source row, got %d sources", row.URLs.Size())
		}
	}

	if expanded[0].TvgChNo != "64.1" || expanded[1].TvgChNo != "64.2" {
		t.Fatalf("expected sub channel numbers 64.1 and 64.2, got %q and %q", expanded[0].TvgChNo, expanded[1].TvgChNo)
	}
	if expanded[0].SubChannelID == expanded[1].SubChannelID {
		t.Fatalf("expected distinct sub-channel slugs")
	}
}

func TestApplyInferredMultiSourceEPG_WithoutChannelGroups(t *testing.T) {
	t.Setenv("MERGE_EPG_FOR_SAME_CHANNEL_NUMBER", "true")
	t.Setenv("EMBEDDED_EPG_URL", "https://epg.example.com/guide.xml")

	stream := &StreamInfo{
		Title:     "Game Show Network",
		TvgID:     "GSN",
		SourceM3U: "1",
		URLs:      xsync.NewMapOf[string, map[string]string](),
		SourceEPGMeta: map[string]SourceEPGMeta{
			"1": {TvgID: "GSN"},
			"2": {TvgID: "gameshow.us"},
		},
	}
	stream.URLs.Store("1", map[string]string{"a": "1:::http://example.com/gsn"})
	stream.URLs.Store("2", map[string]string{"b": "2:::http://example.com/sid"})

	index := &EPGChannelIndex{
		exact:  map[string]struct{}{"gameshow.us": {}},
		folded: map[string]string{"gameshow.us": "gameshow.us"},
	}

	entries := applyInferredMultiSourceEPG([]*StreamInfo{stream}, index)
	if len(entries) != 1 {
		t.Fatalf("expected single merged row fallback, got %d", len(entries))
	}
	if entries[0].TvgID != "gameshow.us" {
		t.Fatalf("expected shared tvg-id gameshow.us, got %q", entries[0].TvgID)
	}
}

func TestCloneStreamForGroupEntry_SetsDirectSourceURL(t *testing.T) {
	t.Setenv("DIRECT_SOURCE_PROXYING", "true")

	stream := &StreamInfo{
		Title:     "Game Show Network",
		TvgID:     "GSN",
		SourceM3U: "1",
		SourceURL: "http://example.com/gsn-stream",
		URLs:      xsync.NewMapOf[string, map[string]string](),
	}
	stream.URLs.Store("1", map[string]string{"a": "1:::http://example.com/gsn-stream"})
	stream.URLs.Store("2", map[string]string{"b": "2:::http://example.com/sid-stream"})

	group := ChannelNumberGroup{Canonical: "Game Show Network"}
	entry := ChannelNumberGroupEntry{
		SourceIndex:  2,
		ChannelTitle: "Game Show Network",
		SubNumber:    "64.2",
	}

	clone := cloneStreamForGroupEntry(stream, entry, group, "gameshow.us")
	if clone == nil {
		t.Fatalf("expected clone")
	}
	if clone.SourceURL != "http://example.com/sid-stream" {
		t.Fatalf("expected sid source URL, got %q", clone.SourceURL)
	}

	entryText := formatStreamEntry("http://proxy.example.com", clone)
	if !strings.Contains(entryText, "http://example.com/sid-stream") {
		t.Fatalf("expected direct source URL in playlist entry, got %q", entryText)
	}
	if strings.Contains(entryText, "http://proxy.example.com/p/") {
		t.Fatalf("did not expect proxied URL, got %q", entryText)
	}
}

func TestExpandStreamsForCompile_MergeEPGDisabled_SkipsExpansion(t *testing.T) {
	t.Setenv("MERGE_EPG_FOR_SAME_CHANNEL_NUMBER", "false")
	t.Setenv("EMBEDDED_EPG_URL", "https://epg.example.com/guide.xml")

	stream := &StreamInfo{
		Title:     "Game Show Network",
		TvgID:     "GSN",
		SourceM3U: "1",
		URLs:      xsync.NewMapOf[string, map[string]string](),
	}
	stream.URLs.Store("1", map[string]string{"a": "1:::http://example.com/gsn"})
	stream.URLs.Store("2", map[string]string{"b": "2:::http://example.com/sid"})

	groups := []ChannelNumberGroup{
		{
			Number:    64,
			Canonical: "Game Show Network",
			Entries: []ChannelNumberGroupEntry{
				{SourceIndex: 1, ChannelTitle: "Game Show Network", SubNumber: "64.1"},
				{SourceIndex: 2, ChannelTitle: "Game Show Network", SubNumber: "64.2"},
			},
		},
	}

	expanded := expandStreamsForCompile([]*StreamInfo{stream}, groups, &EPGChannelIndex{})
	if len(expanded) != 1 {
		t.Fatalf("expected legacy single row, got %d", len(expanded))
	}
	if expanded[0].TvgID != "GSN" {
		t.Fatalf("expected unchanged tvg-id, got %q", expanded[0].TvgID)
	}
}

func TestLoadChannelNumberGroups_FromEnv(t *testing.T) {
	t.Setenv("CHANNEL_NUMBER_GROUP_1", `{"number":64,"canonical":"Game Show Network","entries":[{"source_index":1,"channel_title":"Game Show Network","sub_number":"64.1"},{"source_index":2,"channel_title":"Game Show Network","sub_number":"64.2"}]}`)

	groups := LoadChannelNumberGroups()
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Canonical != "Game Show Network" {
		t.Fatalf("unexpected canonical %q", groups[0].Canonical)
	}
	if len(groups[0].Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(groups[0].Entries))
	}

	os.Unsetenv("CHANNEL_NUMBER_GROUP_1")
}

func TestSlugInputKey_SubChannelDisambiguation(t *testing.T) {
	base := &StreamInfo{Title: "Game Show Network"}
	subA := &StreamInfo{Title: "Game Show Network", SubChannelID: "Game Show Network\x001"}
	subB := &StreamInfo{Title: "Game Show Network", SubChannelID: "Game Show Network\x002"}

	if slugInputKey(base) == slugInputKey(subA) {
		t.Fatalf("expected base and sub-channel slug keys to differ")
	}
	if slugInputKey(subA) == slugInputKey(subB) {
		t.Fatalf("expected sub-channel slug keys to differ")
	}
	if EncodeSlug(subA) == EncodeSlug(subB) {
		t.Fatalf("expected distinct slugs for sub-channels")
	}
}
