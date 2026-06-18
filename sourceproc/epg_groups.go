package sourceproc

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/puzpuzpuz/xsync/v3"

	"windows-m3u-stream-merger-proxy/utils"
)

type ChannelNumberGroupEntry struct {
	SourceIndex  int    `json:"source_index"`
	SourceName   string `json:"source_name,omitempty"`
	ChannelTitle string `json:"channel_title"`
	SubNumber    string `json:"sub_number"`
}

type ChannelNumberGroup struct {
	Number    int                       `json:"number"`
	Canonical string                    `json:"canonical"`
	Entries   []ChannelNumberGroupEntry `json:"entries"`
}

type epgCandidate struct {
	tvgID       string
	sourceIndex int
	order       int
}

func LoadChannelNumberGroups() []ChannelNumberGroup {
	prefix := "CHANNEL_NUMBER_GROUP_"
	type indexed struct {
		index int
		group ChannelNumberGroup
	}
	var groups []indexed

	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := parts[0]
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(key, prefix)
		index, err := strconv.Atoi(suffix)
		if err != nil || index < 1 {
			continue
		}

		var group ChannelNumberGroup
		if err := json.Unmarshal([]byte(parts[1]), &group); err != nil {
			continue
		}
		if len(group.Entries) == 0 {
			continue
		}
		groups = append(groups, indexed{index: index, group: group})
	}

	sort.Slice(groups, func(i, j int) bool {
		return groups[i].index < groups[j].index
	})

	out := make([]ChannelNumberGroup, 0, len(groups))
	for _, item := range groups {
		out = append(out, item.group)
	}
	return out
}

func pickBestTvgID(candidates []epgCandidate, index *EPGChannelIndex) string {
	if len(candidates) == 0 {
		return ""
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].sourceIndex != candidates[j].sourceIndex {
			return candidates[i].sourceIndex < candidates[j].sourceIndex
		}
		return candidates[i].order < candidates[j].order
	})

	if index == nil || !index.Loaded() {
		return candidates[0].tvgID
	}

	var matched []epgCandidate
	for _, candidate := range candidates {
		if index.Has(candidate.tvgID) {
			matched = append(matched, candidate)
		}
	}
	if len(matched) > 0 {
		return matched[0].tvgID
	}

	return candidates[0].tvgID
}

func collectTvgIDCandidates(stream *StreamInfo) []epgCandidate {
	if stream == nil {
		return nil
	}

	seen := make(map[string]struct{})
	var candidates []epgCandidate
	order := 0

	add := func(tvgID, sourceM3U string) {
		tvgID = strings.TrimSpace(tvgID)
		if tvgID == "" {
			return
		}
		key := strings.ToLower(tvgID)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}

		sourceIndex := 0
		if parsed, err := strconv.Atoi(strings.TrimSpace(sourceM3U)); err == nil {
			sourceIndex = parsed
		}

		candidates = append(candidates, epgCandidate{
			tvgID:       tvgID,
			sourceIndex: sourceIndex,
			order:       order,
		})
		order++
	}

	add(stream.TvgID, stream.SourceM3U)
	if stream.SourceEPGMeta != nil {
		keys := make([]string, 0, len(stream.SourceEPGMeta))
		for key := range stream.SourceEPGMeta {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			return lessSourceIndex(keys[i], keys[j])
		})
		for _, key := range keys {
			add(stream.SourceEPGMeta[key].TvgID, key)
		}
	}

	return candidates
}

func resolveGroupTvgID(group ChannelNumberGroup, streams []*StreamInfo, index *EPGChannelIndex) string {
	var candidates []epgCandidate
	order := 0

	for _, entry := range group.Entries {
		stream := findStreamForGroupEntry(group, entry, streams)
		if stream == nil {
			continue
		}

		entryCandidates := collectTvgIDCandidates(stream)
		for _, candidate := range entryCandidates {
			if entry.SourceIndex > 0 {
				candidate.sourceIndex = entry.SourceIndex
			}
			candidate.order = order
			order++
			candidates = append(candidates, candidate)
		}
	}

	return pickBestTvgID(candidates, index)
}

func findStreamForGroupEntry(group ChannelNumberGroup, entry ChannelNumberGroupEntry, streams []*StreamInfo) *StreamInfo {
	sourceKey := strconv.Itoa(entry.SourceIndex)
	targetTitle := applyChannelMergeRule(strings.TrimSpace(entry.ChannelTitle))
	if targetTitle == "" {
		targetTitle = strings.TrimSpace(group.Canonical)
	}
	targetKey := sanitizeField(targetTitle)

	for _, stream := range streams {
		if stream == nil || stream.URLs == nil {
			continue
		}
		if !streamHasSourceIndex(stream, sourceKey) {
			continue
		}
		if sanitizeField(stream.Title) != targetKey {
			continue
		}
		return stream
	}

	canonicalKey := strings.ToLower(strings.TrimSpace(group.Canonical))
	for _, stream := range streams {
		if stream == nil || stream.URLs == nil {
			continue
		}
		if !streamHasSourceIndex(stream, sourceKey) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(stream.Title), canonicalKey) {
			return stream
		}
	}

	return nil
}

func streamHasSourceIndex(stream *StreamInfo, sourceIndex string) bool {
	if stream == nil || stream.URLs == nil {
		return false
	}
	inner, ok := stream.URLs.Load(sourceIndex)
	return ok && len(inner) > 0
}

func cloneStreamForGroupEntry(stream *StreamInfo, entry ChannelNumberGroupEntry, group ChannelNumberGroup, tvgID string) *StreamInfo {
	if stream == nil {
		return nil
	}

	clone := *stream
	clone.TvgID = tvgID
	clone.TvgChNo = strings.TrimSpace(entry.SubNumber)
	clone.SubChannelID = strings.TrimSpace(group.Canonical) + "\x00" + strconv.Itoa(entry.SourceIndex)
	clone.DisplayTitle = strings.TrimSpace(entry.ChannelTitle)
	if clone.DisplayTitle == "" {
		clone.DisplayTitle = strings.TrimSpace(group.Canonical)
	}

	sourceKey := strconv.Itoa(entry.SourceIndex)
	clone.URLs = cloneSingleSourceURLs(stream, sourceKey)
	clone.SourceM3U = sourceKey
	clone.SourceURL, clone.SourceIndex = primarySourceURL(&clone, sourceKey)
	if clone.SourceURL == "" {
		clone.SourceURL, clone.SourceIndex = primarySourceURL(stream, sourceKey)
	}

	return &clone
}

func cloneSingleSourceURLs(stream *StreamInfo, sourceIndex string) *xsync.MapOf[string, map[string]string] {
	result := xsync.NewMapOf[string, map[string]string]()
	if stream == nil || stream.URLs == nil {
		return result
	}
	if inner, ok := stream.URLs.Load(sourceIndex); ok && len(inner) > 0 {
		copied := make(map[string]string, len(inner))
		for key, value := range inner {
			copied[key] = value
		}
		result.Store(sourceIndex, copied)
	}
	return result
}

func expandStreamsForCompile(entries []*StreamInfo, groups []ChannelNumberGroup, index *EPGChannelIndex) []*StreamInfo {
	if !utils.IsChannelEPGMergeActive() || len(groups) == 0 {
		return entries
	}

	multiEntryGroups := make(map[string]ChannelNumberGroup)
	for _, group := range groups {
		if len(group.Entries) < 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(group.Canonical))
		if key == "" {
			continue
		}
		multiEntryGroups[key] = group
	}

	if len(multiEntryGroups) == 0 {
		return applyGroupTvgIDToEntries(entries, groups, index)
	}

	handled := make(map[string]struct{})
	var output []*StreamInfo

	for _, stream := range entries {
		if stream == nil {
			continue
		}

		group, ok := multiEntryGroups[strings.ToLower(strings.TrimSpace(stream.Title))]
		if !ok {
			output = append(output, applyGroupTvgID(stream, groups, entries, index))
			continue
		}

		groupKey := strings.ToLower(strings.TrimSpace(group.Canonical))
		if _, seen := handled[groupKey]; seen {
			continue
		}
		handled[groupKey] = struct{}{}

		resolvedTvgID := resolveGroupTvgID(group, entries, index)
		var clones []*StreamInfo
		for _, entry := range group.Entries {
			sourceKey := strconv.Itoa(entry.SourceIndex)
			if !streamHasSourceIndex(stream, sourceKey) {
				continue
			}
			clone := cloneStreamForGroupEntry(stream, entry, group, resolvedTvgID)
			if clone != nil && clone.URLs != nil && clone.URLs.Size() > 0 {
				clones = append(clones, clone)
			}
		}
		if len(clones) > 0 {
			output = append(output, clones...)
			continue
		}

		output = append(output, applyGroupTvgID(stream, groups, entries, index))
	}

	return output
}

func applyGroupTvgIDToEntries(entries []*StreamInfo, groups []ChannelNumberGroup, index *EPGChannelIndex) []*StreamInfo {
	output := make([]*StreamInfo, 0, len(entries))
	for _, stream := range entries {
		output = append(output, applyGroupTvgID(stream, groups, entries, index))
	}
	return output
}

func applyGroupTvgID(stream *StreamInfo, groups []ChannelNumberGroup, allStreams []*StreamInfo, index *EPGChannelIndex) *StreamInfo {
	if stream == nil {
		return stream
	}

	for _, group := range groups {
		if len(group.Entries) < 2 {
			continue
		}
		if !streamMatchesGroup(stream, group) {
			continue
		}
		resolved := resolveGroupTvgID(group, allStreams, index)
		if resolved != "" {
			stream.TvgID = resolved
		}
		break
	}

	return stream
}

func streamMatchesGroup(stream *StreamInfo, group ChannelNumberGroup) bool {
	if stream == nil {
		return false
	}
	for _, entry := range group.Entries {
		if findStreamForGroupEntry(group, entry, []*StreamInfo{stream}) != nil {
			return true
		}
	}
	return strings.EqualFold(strings.TrimSpace(stream.Title), strings.TrimSpace(group.Canonical))
}

func applyInferredMultiSourceEPG(entries []*StreamInfo, index *EPGChannelIndex) []*StreamInfo {
	for _, entry := range entries {
		if entry == nil || entry.URLs == nil || entry.URLs.Size() < 2 {
			continue
		}
		if resolved := pickBestTvgID(collectTvgIDCandidates(entry), index); resolved != "" {
			entry.TvgID = resolved
		}
	}
	return entries
}

func lessSourceIndex(left, right string) bool {
	leftIndex, leftErr := strconv.Atoi(strings.TrimSpace(left))
	rightIndex, rightErr := strconv.Atoi(strings.TrimSpace(right))
	if leftErr == nil && rightErr == nil {
		return leftIndex < rightIndex
	}
	return strings.Compare(left, right) < 0
}
