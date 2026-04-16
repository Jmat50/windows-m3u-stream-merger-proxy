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

## Testing Notes
- Fast package verification that currently passes:
  - `go test ./config ./handlers ./logger ./proxy/... ./sourceproc ./store ./updater ./utils ./discovery`
  - `python -m py_compile windows-app/gui_app.py`
- The root-package `go test` can run much longer because of integration coverage; treat it separately from the fast package suite.
