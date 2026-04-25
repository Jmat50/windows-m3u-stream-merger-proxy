# Windows Server 2012 R2 Troubleshooting Runbook

This runbook covers the recurring failure modes seen on Windows Server 2012 R2 for this project, especially DNS instability, slug publish edge-cases, and player compatibility differences.

## Scope

Use this document when one or more of these occur:

- playback works on Windows 10 but fails on Windows Server 2012 R2
- logs show `getaddrinfow`, `context deadline exceeded`, or repeated load balancer retries
- discovered source streams fail while static sources work
- slug lookup/publish errors appear around `data/slugs` and `data/new-slugs`

## Quick Triage Checklist

1. Confirm app/server versions match the branch with:
   - DNS fallback dialing (`FALLBACK_DNS_SERVERS`)
   - slug publish file-sync fallback
   - detailed LB diagnostics
2. Capture a fresh log sample covering:
   - first `/p/...` request
   - load balancer selection
   - any `/segment/...` fetches
3. Confirm `DATA_PATH` and `TEMP_PATH` are writable for the runtime user.
4. Run DNS and upstream checks directly on the 2012 R2 host.

## Known Failure Signatures

### A) DNS resolution instability

Typical log lines:

- `dial tcp: lookup <host>: getaddrinfow: This is usually a temporary error during hostname resolution`
- `context deadline exceeded` during upstream fetch

Meaning:

- host resolver path is unstable or overloaded
- upstream connect is timing out before successful DNS+TLS setup

### B) Empty/irrelevant source index attempts

Typical log lines:

- `Channel not found from M3U_DISC_...`
- `Source M3U_<n> has no candidate URLs for stream ...`

Meaning:

- decoded stream token has no URL candidates for that index
- source filtering/index merge path is not aligned for that stream

### C) Slug publish/read issues

Typical log lines:

- `slug not found: open ...\data\slugs\...`
- Windows errors around slug directory operations (`Access is denied`, `parameter is incorrect`)

Meaning:

- Windows directory-level rename/remove semantics can fail under transient file handles
- slug directory swap must use fallback behavior

## Host-Level Validation Commands (2012 R2)

Run on the server where the app is running:

```powershell
nslookup cdn.klowdtv.net
Resolve-DnsName cdn.klowdtv.net
curl.exe -I "https://cdn.klowdtv.net/803B48A/n1.klowdtv.net/live2/gsn_720p/playlist.m3u8?checkedby:iptvcat.com"
```

If these are intermittent/failing, fix host DNS/network first.

## Recommended Runtime Environment

Set these for 2012 R2 deployments:

```text
FALLBACK_DNS_SERVERS=1.1.1.1,8.8.8.8
DEBUG=true
SAFE_LOGS=true
```

Notes:

- `FALLBACK_DNS_SERVERS` is comma-separated; `:53` is optional.
- Keep `SAFE_LOGS=true` when sharing logs publicly.

## Player Compatibility Notes

- VLC is more tolerant of edge conditions and malformed timing.
- Many Android TV players are stricter and expose startup/playlist issues earlier.
- If VLC works and Android fails, check server logs first for upstream fetch/segment errors before concluding app-only incompatibility.

## Architecture Behaviors That Matter

1. **No-shared-buffer HLS path**
   - With `SHARED_BUFFER=false`, discovered `.m3u8` streams are served using playlist passthrough and `/segment/...` proxying.

2. **Load balancer retries**
   - LB now logs source/subindex/url-level failure causes.
   - Temporary DNS failures are retried per candidate URL.

3. **Slug handling**
   - decode reads from `slugs` and falls back to `new-slugs`
   - publish uses Windows-safe file-level sync fallback

## Incident Response Steps

1. Reproduce once with `DEBUG=true`.
2. Extract one complete request chain:
   - `Trying all stream urls for: <slug>`
   - source failure details
   - final LB error
3. Classify by signature:
   - DNS unstable -> fix resolver/network and keep fallback DNS configured
   - empty candidate maps -> inspect source indexing and stream token mappings
   - slug errors -> inspect `DATA_PATH` permissions and slug directory contents
4. Re-test on both:
   - Windows 10 + VLC (baseline)
   - Windows Server 2012 R2 + target Android app

## Escalation Data to Collect

When opening an internal issue/PR, include:

- exact app + server commit SHA
- `DATA_PATH`, `TEMP_PATH`, `BASE_URL`, `SHARED_BUFFER`, `FALLBACK_DNS_SERVERS` values
- 30-60 lines of logs around one failing stream request
- output of the three host validation commands above

## Related Files

- `utils/http.go` (DNS fallback dialing)
- `proxy/loadbalancer/instance.go` (selection, retries, diagnostics)
- `sourceproc/processor.go` (slug publish fallback)
- `sourceproc/slug.go` (slug decode fallback)
- `proxy/stream/stream_instance.go` (HLS passthrough behavior)
