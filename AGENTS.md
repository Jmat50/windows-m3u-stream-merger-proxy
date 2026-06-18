# AGENTS

## Purpose
- This repo is a Go IPTV proxy/playlist merger with a bundled Windows desktop controller in `windows-app/`.
- `main.go` wires the HTTP handlers, updater, and discovery-backed API endpoints.
- Keep this file focused on coding constraints and architecture. Put end-user detail and long runbooks in `README.md` or `docs/troubleshooting/`.

## Runtime Shape
- `updater/` schedules source refreshes and initializes the discovery manager.
- `sourceproc/` downloads source playlists, parses/filters streams, and writes the merged playlist plus slug data.
- `proxy/` owns source selection, failover, buffering, and playback proxying.
- `windows-app/gui_app.py` is the desktop UI entry point. It persists settings to `%LOCALAPPDATA%\\WindowsM3UStreamMergerProxyDesktop\\settings.json` and translates them into env vars before launching or restarting the server.

## Source Configuration
- Static sources come from `M3U_URL_<n>`, `M3U_MAX_CONCURRENCY_<n>`, and `M3U_CONTAINS_VOD_<n>`.
- Dynamic discovery sources are runtime `utils.SourceConfig` entries published through `utils.SetDynamicSources(...)`.
- Do not add new source-aware logic by scanning env vars directly. Use `utils.GetSourceConfigs()`, `utils.GetM3UIndexes()`, and `utils.GetSourceConfig()` from `utils/env.go`.
- Discovery jobs arrive as `DISCOVERY_JOB_<n>` JSON payloads and are converted into dynamic sources by `discovery/`.
- Discovery is HTTP crawler/parser based, not browser automation based.

## Important Behaviors
- With `SHARED_BUFFER=false`, M3U8 playback uses playlist passthrough and `/segment/...` proxying instead of media-byte stitching. Preserve this behavior when touching HLS paths.
- Load balancing and retries should only consider source indexes that actually exist for the decoded stream. Preserve detailed failure diagnostics, including stream ID, source index, candidate subindex, URL, and failure cause.
- Outbound HTTP includes DNS fallback logic in `utils/http.go`; preserve `FALLBACK_DNS_SERVERS` support and lookup/fetch error detail.
- On Windows, slug publish/read logic must tolerate directory rename/remove failures; keep the existing fallback behavior intact.

## Embedded EPG and Channel Number Groups
- Playlist-level XMLTV is configured with `EMBEDDED_EPG_URL` (`utils/playlist_header.go`). The server does not generate or serve XMLTV; it embeds external guide URLs in the `#EXTM3U` header and preserves per-channel `tvg-id` tags for client-side matching.
- Optional shared EPG across multi-source channel-number groups is gated by **both** `EMBEDDED_EPG_URL` and `MERGE_EPG_FOR_SAME_CHANNEL_NUMBER=true` (`utils.IsChannelEPGMergeActive()`). When disabled, legacy first-wins `tvg-id` merge behavior must remain unchanged.
- Desktop UI: **Embedded EPGs** popup exposes embedded guide URLs and the **Merge EPGs for same Channel Number** toggle (`windows-app/gui_app.py`). Channel Settings save publishes `channel_number_groups` in `settings.json` and `CHANNEL_NUMBER_GROUP_<n>` env JSON derived from the channel-number tree.
- Backend flow lives in `sourceproc/epgindex.go` (fetch/parse XMLTV channel ids), `sourceproc/epg_groups.go` (group parse, best `tvg-id` selection, per-source row expansion), and `sourceproc/processor.go` `compileM3U()`.
- During `compileM3U`, streams are read from sort shards without in-memory `URLs` (`json:"-"`). Always restore per-source URLs with `restoreCompileStreamURLs()` in `sourceproc/filter.go` before EPG group expansion or output formatting.
- Multi-entry channel groups emit separate playlist rows (`64.1`, `64.2`) with shared `tvg-id`, distinct `tvg-chno`, distinct slugs via `SubChannelID`, and single-source URL maps. When expanding per-source rows, set each clone's `SourceURL` for `DIRECT_SOURCE_PROXYING=true` output in `formatStreamEntry()`.
- Per-source `tvg-id` values are retained in `StreamInfo.SourceEPGMeta` through title merges (`sourceproc/sorting.go`) so EPG resolution is order-independent.
- If merge EPG is enabled but no `CHANNEL_NUMBER_GROUP_<n>` entries exist yet, `applyInferredMultiSourceEPG()` still promotes the best matching `tvg-id` on multi-source channels without splitting rows.
- Do not add XMLTV serving routes or per-channel embedded guide URLs from upstream source `#EXTM3U` headers unless explicitly requested; current design uses user-configured embedded EPG only.

## Desktop App Notes
- Reuse the existing popup/editor patterns in `windows-app/gui_app.py` for new settings surfaces.
- Web discovery settings are persisted on save and restart the server only if it is already running.
- The discovered-sources management UI talks to `/api/discovery/sources`; keep desktop and backend behavior aligned.
- Embedded EPG settings and the merge-EPG toggle are saved from the Embedded EPGs popup. Channel number groups are saved from Channel Settings; saving Channel Settings is required for sub-channel row output (`64.1`, `64.2`).

## Verification
- Fast suite:
  - `go test ./config ./handlers ./logger ./proxy/... ./sourceproc ./store ./updater ./utils ./discovery`
  - `python -m py_compile windows-app/gui_app.py`
- Root `go test` is slower and closer to integration coverage; treat it separately.

## References
- `README.md` for product behavior and env vars.
- `docs/troubleshooting/windows-server-2012r2.md`
- `docs/troubleshooting/android-client-compatibility.md`
