# Windows M3U Stream Merger Proxy

Windows M3U Stream Merger Proxy is a Windows-first IPTV proxy and playlist merger built with Go and a bundled Python desktop controller. This fork started from the upstream project by Son Roy Almerol, then moved beyond its older Docker-only workflow into a rebuilt Windows-focused app with expanded source management and fork-specific discovery tooling.

This is the canonical README for this repository. The old upstream-oriented reference is kept in `LEGACY README.txt` for archive purposes only.

## What Makes This Project Different

- Windows desktop app for start/stop, settings, logs, and source management
- Static source support for remote URLs and local `file://` M3U files
- Recursive web discovery that can crawl a site, read `robots.txt`, inspect sitemap XML, and validate discovered M3U and M3U8 playlists
- Per-source concurrency and VOD flags
- Title and group filters, source pinning rules, and channel merge rules
- Merged playlist output at `/playlist.m3u`
- Proxy streaming, retries, failover handling, and optional shared buffering
- Still usable headless on Linux or in Docker even though the bundled GUI targets Windows

## Project Layout

- `main.go`: boots the HTTP server, handlers, updater, and discovery API
- `sourceproc/`: downloads sources, parses playlists, applies rules, and writes the merged M3U
- `proxy/`: stream proxying, retries, concurrency, buffering, and failover logic
- `discovery/`: HTTP-based web discovery jobs for finding playlist URLs
- `windows-app/`: Windows desktop controller and build script

## Quick Start

### Windows Desktop App

Build the bundled server and desktop app from the repo root:

```powershell
powershell -ExecutionPolicy Bypass -File .\windows-app\build.ps1
```

Run:

```text
windows-app\dist\WindowsM3UStreamMergerProxyDesktop\WindowsM3UStreamMergerProxyDesktop.exe
```

The desktop app:

- stores settings in `%LOCALAPPDATA%\WindowsM3UStreamMergerProxyDesktop\settings.json`
- manages its own runtime, data, temp, and log folders under the same app-data root
- translates saved settings into environment variables before launching the Go server
- restarts the server automatically when needed after settings changes

See [`windows-app/README.md`](windows-app/README.md) for desktop build details.

### Headless or Docker

You can still run the Go server directly or through Docker. At minimum you need:

- `BASE_URL`
- at least one `M3U_URL_<n>` source or one `DISCOVERY_JOB_<n>`
- a writable `DATA_PATH` and `TEMP_PATH` if you are not using the desktop app defaults

The included [`docker-compose.yaml`](docker-compose.yaml) is a starting point for containerized usage.

## Runtime Flow

1. Sources are loaded from `M3U_URL_<n>` environment variables.
2. Discovery jobs from `DISCOVERY_JOB_<n>` can inject additional sources at runtime.
3. `sourceproc` downloads and parses sources, applies filters and channel rules, and writes a merged playlist.
4. The merged playlist is served at `/playlist.m3u`.
5. Stream requests are proxied through `/p/...`, with retries, concurrency control, and buffering.

## HTTP Endpoints

- `/playlist.m3u`: merged playlist output
- `/p/{originalBasePath}/{streamToken}.{fileExt}`: proxied stream endpoint
- `/a/{encoded}`: passthrough helper endpoint used when generating playlist links
- `/segment/...`: segment proxying for stream handling
- `/api/discovery/sources`: JSON view of currently discovered dynamic sources

## Source Types

### Static Sources

Static sources use numbered environment variables:

- `M3U_URL_1=https://provider.example/playlist.m3u`
- `M3U_MAX_CONCURRENCY_1=2`
- `M3U_CONTAINS_VOD_1=true`

Local files are supported with `file://` URLs, for example:

```text
M3U_URL_2=file:///C:/IPTV/local-playlist.m3u
```

### Web Discovery Sources

Web discovery jobs are passed as JSON in numbered `DISCOVERY_JOB_<n>` variables. The desktop app writes these automatically, but they can also be supplied manually.

Example:

```json
{"name":"Provider Crawl","start_url":"https://example.com/playlists","scan_interval_minutes":60,"recursive":true,"max_depth":3,"max_pages":250,"include_subdomains":false,"follow_robots":true,"source_concurrency":1,"enabled":true}
```

Discovery jobs can:

- start from a configured page
- crawl same-site links recursively
- optionally include subdomains
- inspect `robots.txt`
- read sitemap XML files
- validate discovered URLs by checking for M3U content before publishing them as dynamic sources

## Configuration Reference

### Core Runtime

| Variable | Purpose |
| --- | --- |
| `PORT` | HTTP listen port. Default: `8080`. |
| `BASE_URL` | Required for playlist generation and proxy URLs. |
| `TZ` | Time zone used by the server and credential expiry parsing. |
| `SYNC_CRON` | Background refresh schedule. Default: `0 0 * * *`. |
| `SYNC_ON_BOOT` | Run an initial refresh at startup. Default: `true`. |
| `CLEAR_ON_BOOT` | Clear cached processed data before startup. Default: `false`. |
| `DATA_PATH` | Persistent data directory. |
| `TEMP_PATH` | Temporary working directory. |
| `CREDENTIALS` | Optional playlist auth in `user:pass` or `user:pass:YYYY-MM-DD` form, separated by `|`. |

### Source Configuration

| Variable | Purpose |
| --- | --- |
| `M3U_URL_<n>` | Static M3U source URL or `file://` path. |
| `M3U_MAX_CONCURRENCY_<n>` | Max concurrent requests allowed for that source. |
| `M3U_CONTAINS_VOD_<n>` | Marks whether direct-media/VOD behavior is allowed for that source. |
| `DISCOVERY_JOB_<n>` | JSON-encoded web discovery job. |
| `USER_AGENT` | Custom User-Agent for outgoing source requests. |
| `HTTP_ACCEPT` | Custom Accept header for outgoing source requests. |

### Filtering and Channel Rules

| Variable | Purpose |
| --- | --- |
| `INCLUDE_GROUPS_<n>` / `EXCLUDE_GROUPS_<n>` | Regex filters for channel groups. |
| `INCLUDE_TITLE_<n>` / `EXCLUDE_TITLE_<n>` | Regex filters for channel titles. |
| `CHANNEL_SOURCES_<n>` | Restrict matching titles to specific source indexes using `pattern|1,2,3`. |
| `CHANNEL_MERGE_<n>` | Merge channel titles using `source|target`. |
| `TITLE_SUBSTR_FILTER` | Regex used to strip substrings from output titles. |
| `SORTING_KEY` | Sort by `title`, `tvg-id`, `tvg-chno`, `tvg-group`, `tvg-type`, or `source`. |
| `SORTING_DIRECTION` | `asc` or `desc`. |

### Streaming and Logging

| Variable | Purpose |
| --- | --- |
| `MAX_RETRIES` | Retry count across sources while streaming. |
| `RETRY_WAIT` | Delay in seconds before retrying a failed stream. |
| `STREAM_TIMEOUT` | Timeout in seconds before a stream is considered down. |
| `BUFFER_CHUNK_NUM` | Shared buffer chunk count. |
| `MINIMUM_THROUGHPUT` | Minimum healthy throughput in bytes per second. |
| `SHARED_BUFFER` | Enables or disables shared buffering for direct media paths. |
| `DEBUG` | Enables debug logging. |
| `SAFE_LOGS` | Redacts URLs from logs for safer sharing. |
| `FALLBACK_DNS_SERVERS` | Optional DNS fallback list for outbound HTTP lookups (`ip[:port],ip[:port]`). Defaults: `1.1.1.1:53,8.8.8.8:53`. |

## Notes for Contributors

- Source indexing is centralized in `utils/env.go`; avoid adding new direct env scans for source lists.
- Dynamic discovery sources are injected through `utils.SetDynamicSources(...)`.
- The Windows app is the user-facing settings layer and should stay aligned with server env behavior.

## Troubleshooting (Windows + Player Compatibility)

For a full Windows Server runbook, see [`docs/troubleshooting/windows-server-2012r2.md`](docs/troubleshooting/windows-server-2012r2.md).
For Android/Android TV player behavior differences, see [`docs/troubleshooting/android-client-compatibility.md`](docs/troubleshooting/android-client-compatibility.md).

### Android TV app fails but VLC works

This usually indicates stricter HLS-client behavior (Android TV apps are often less tolerant than VLC), not necessarily a bad stream source. Current proxy behavior for discovered HLS sources is:

- with `SHARED_BUFFER=false`, proxy serves playlist passthrough for `.m3u8` streams
- segment fetches stay proxied under `/segment/...`

If VLC works but Android does not, check server logs first for upstream fetch errors, DNS timeouts, or parse failures before tuning player settings.

### Windows Server 2012 R2: intermittent DNS errors

If logs show errors like:

- `lookup <host>: getaddrinfow: temporary error during hostname resolution`
- `context deadline exceeded` during upstream playlist fetch

then the issue is typically host DNS reliability. This repo now includes DNS-fallback dialing in `utils/http.go`. You can set:

```text
FALLBACK_DNS_SERVERS=1.1.1.1,8.8.8.8
```

for explicit fallback resolvers.

Recommended host checks on 2012 R2:

- `nslookup <upstream-host>`
- `Resolve-DnsName <upstream-host>`
- `curl.exe -I "<upstream-m3u8-url>"`

### Slug mapping on Windows Server

If stream-token lookups fail after refresh, verify `DATA_PATH` and logs around slug publish. The server now uses Windows-safe slug sync behavior (file-level fallback) and slug decode fallback from `slugs` to `new-slugs`.

## Validation Commands

Fast checks that currently fit this repo well:

```powershell
go test ./config ./handlers ./logger ./proxy/... ./sourceproc ./store ./updater ./utils ./discovery
python -m py_compile windows-app/gui_app.py
```

The root-package `go test` includes longer-running integration coverage and is best treated separately.

## Archive Reference

`LEGACY README.txt` is preserved only as an upstream reference snapshot. If the legacy text conflicts with this README, follow this file.
