# AGENTS

## Overview
- This repo is a Go IPTV proxy/playlist merger with a bundled Windows desktop controller in `windows-app/`.
- The Go server is configured from environment variables at process start.
- The Windows desktop app owns the user-facing settings UX, persists settings to JSON, and translates those settings into env vars before launching the server.

## Key Runtime Flow
- `main.go` starts HTTP handlers and the background updater.
- `updater/updater.go` owns scheduled refreshes and now also initializes web discovery jobs.
- `sourceproc/` downloads source playlists, parses streams, applies filters, and writes the merged playlist plus slug mappings.
- `proxy/` handles stream playback, balancing, retries, and buffer coordination.

## Source Configuration Model
- Static playlist sources still come from `M3U_URL_*` and `M3U_MAX_CONCURRENCY_*`.
- Dynamic web-discovered playlists are injected at runtime through `utils.SourceConfig` and `utils.SetDynamicSources(...)`.
- Source indexing logic now lives in `utils/env.go`; avoid bypassing it with direct env scans when adding new source-aware code.

## Desktop App Notes
- Main desktop UI entry point: `windows-app/gui_app.py`.
- Settings persistence is in `%LOCALAPPDATA%\\WindowsM3UStreamMergerProxyDesktop\\settings.json`.
- Popup-style editors already exist in this file; follow those patterns for new configuration surfaces.
- The new Web Discovery popup persists immediately and restarts the server only if it is already running.

## Web Discovery Feature
- Backend package: `discovery/`.
- Jobs are passed from the desktop app via `DISCOVERY_JOB_<n>` JSON payloads.
- Discovery is HTTP-parser based, not browser-automation based.
- The crawler can:
  - seed from the configured page
  - follow same-site links recursively
  - read `robots.txt`
  - read sitemap XML files
  - validate discovered playlists by checking for `#EXTM3U`
- Discovery changes trigger a playlist rebuild through the updater callback.

## Streaming Reliability Notes (Critical)
- **HLS with `SHARED_BUFFER=false`:** discovered `.m3u8` sources must use playlist passthrough (`M3U8Processor`) rather than media-byte stitching. This matches direct-player behavior and avoids Android TV incompatibilities seen in the old path.
- **Load balancer source filtering:** when selecting stream URLs, only evaluate source indexes that actually exist for the decoded slug stream. Avoid retrying indexes that are absent/empty in `stream.URLs`.
- **Windows Server 2012 R2 DNS behavior:** upstream lookups can intermittently fail with `getaddrinfow` temporary errors. Outbound HTTP now includes DNS fallback dialing logic in `utils/http.go`.
- **Fallback DNS config:** `FALLBACK_DNS_SERVERS` can be used to set explicit DNS resolvers (comma-separated, port optional). Defaults to `1.1.1.1:53,8.8.8.8:53`.
- **Slug publish/read robustness:** on Windows Server, directory rename/remove can fail for slug folders. Slug publish uses file-level sync fallback and decode checks both `slugs` and `new-slugs`.
- **Diagnostics expectation:** load balancer errors should include stream ID, source index, candidate subindex, URL, and specific failure cause (fetch timeout, DNS lookup failure, HTTP status, etc.). Preserve this detail when changing error handling.
- **Runbook:** follow `docs/troubleshooting/windows-server-2012r2.md` for incident triage and host validation steps.
- **Android playback tuning:** follow `docs/troubleshooting/android-client-compatibility.md` when VLC works but Android/Android TV clients fail.

## Testing Notes
- Fast package verification that currently passes:
  - `go test ./config ./handlers ./logger ./proxy/... ./sourceproc ./store ./updater ./utils ./discovery`
  - `python -m py_compile windows-app/gui_app.py`
- The root-package `go test` can run much longer because of integration coverage; treat it separately from the fast package suite.
