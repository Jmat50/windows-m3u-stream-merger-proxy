package sourceproc

import "github.com/puzpuzpuz/xsync/v3"

// SourceEPGMeta stores per-source EPG metadata retained through title merges.
type SourceEPGMeta struct {
	TvgID string `json:"tvg_id,omitempty"`
}

// StreamInfo represents a stream with thread-safe operations
type StreamInfo struct {
	Title         string                                  `json:"title"`
	TvgID         string                                  `json:"tvg_id"`
	TvgChNo       string                                  `json:"tvg_ch"`
	TvgType       string                                  `json:"tvg_type"`
	LogoURL       string                                  `json:"logo"`
	AutoLogoURL   bool                                    `json:"-"`
	Group         string                                  `json:"group"`
	URLs          *xsync.MapOf[string, map[string]string] `json:"-"`
	SourceM3U     string                                  `json:"source_m3u"`
	SourceIndex   int                                     `json:"source_index"`
	SourceURL     string                                  `json:"source_url,omitempty"`
	SourceEPGMeta map[string]SourceEPGMeta                `json:"source_epg_meta,omitempty"`
	SubChannelID  string                                  `json:"sub_channel_id,omitempty"`
	DisplayTitle  string                                  `json:"display_title,omitempty"`
}
