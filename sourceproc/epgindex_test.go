package sourceproc

import (
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEPGChannelIndex_ParseChannels(t *testing.T) {
	xmltv := `<?xml version="1.0" encoding="UTF-8"?>
<tv>
  <channel id="gameshow.us"><display-name>Game Show Network</display-name></channel>
  <channel id="cnn.us"><display-name>CNN</display-name></channel>
</tv>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(xmltv))
	}))
	defer server.Close()

	t.Setenv("EMBEDDED_EPG_URL", server.URL)

	index := NewEPGChannelIndex(context.Background())
	if !index.Loaded() {
		t.Fatalf("expected EPG index to load channel ids")
	}
	if !index.Has("gameshow.us") {
		t.Fatalf("expected gameshow.us in index")
	}
	if index.Has("missing.id") {
		t.Fatalf("did not expect missing.id in index")
	}
}

func TestEPGChannelIndex_ParseGzipChannels(t *testing.T) {
	xmltv := `<?xml version="1.0" encoding="UTF-8"?><tv><channel id="gzip.id"></channel></tv>`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(xmltv))
		_ = gz.Close()
	}))
	defer server.Close()

	t.Setenv("EMBEDDED_EPG_URL", server.URL)

	index := NewEPGChannelIndex(context.Background())
	if !index.Has("gzip.id") {
		t.Fatalf("expected gzip.id in index")
	}
}

func TestEPGChannelIndex_CaseInsensitiveLookup(t *testing.T) {
	index := &EPGChannelIndex{
		exact:  map[string]struct{}{"Game.Show": {}},
		folded: map[string]string{"game.show": "Game.Show"},
	}

	if !index.Has("game.show") {
		t.Fatalf("expected case-insensitive lookup to match")
	}
}

func TestEPGChannelIndex_GracefulFailure(t *testing.T) {
	t.Setenv("EMBEDDED_EPG_URL", "http://127.0.0.1:1/not-found")

	index := NewEPGChannelIndex(context.Background())
	if index.Loaded() {
		t.Fatalf("expected empty index on fetch failure")
	}
}
