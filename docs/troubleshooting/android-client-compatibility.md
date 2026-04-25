# Android Client Compatibility Runbook

This runbook focuses on Android and Android TV IPTV players that are stricter than VLC when handling HLS playlists and proxy-delivered streams.

## When to Use This

Use this guide when:

- VLC plays streams but one or more Android apps fail or stall
- startup works intermittently on Android but is stable on desktop players
- segment or playlist fetches fail only on Android-family clients

## Why VLC and Android Can Differ

VLC is generally tolerant of:

- delayed first segments
- non-ideal playlist cadence
- unusual header/content-type patterns
- aggressive reconnect behavior

Many Android players are stricter and fail sooner when:

- startup latency is high
- playlist responses are malformed or delayed
- segment timing or fetch reliability is unstable

## Current Proxy Behaviors Relevant to Android

1. With `SHARED_BUFFER=false`, discovered `.m3u8` streams use playlist passthrough and `/segment/...` proxying.
2. Segment URLs are rewritten by `proxy/stream/failovers/m3u8_helpers.go`.
3. Source selection/retries happen in `proxy/loadbalancer/instance.go`.
4. Client request handling is in `handlers/stream_http.go`.

## High-Impact Tuning Checklist

Apply in order and validate after each step:

1. **Prefer HLS passthrough path for discovered `.m3u8`**
   - Keep `SHARED_BUFFER=false` when Android compatibility is the priority for discovered HLS.

2. **Stabilize upstream DNS/network first**
   - Set `FALLBACK_DNS_SERVERS` on unstable hosts:
     - Example: `FALLBACK_DNS_SERVERS=1.1.1.1,8.8.8.8`

3. **Avoid over-aggressive timeouts/retry churn**
   - Ensure load-balancer selection has enough time on slower hosts.
   - Watch for repeated source exclusions that starve valid candidates.

4. **Check content-type and segment status**
   - Segment responses should typically be `200`/`206`.
   - Content types should remain consistent (`application/vnd.apple.mpegurl` for playlists, `video/MP2T` for TS segments).

5. **Validate `BASE_URL` reachability from Android client network**
   - Android device must resolve and reach the exact host in generated playlist/segment URLs.

## Fast Validation Steps

From a machine on same network as Android device:

1. Fetch merged playlist:

```text
http://<base>/playlist.m3u
```

2. Open one proxied stream URL from playlist; confirm it returns M3U8 content quickly.
3. Fetch one `/segment/...` URL from returned playlist and verify non-error response.

If these fail outside the Android app, fix server/network first.

## Logging Signals to Watch

Healthy pattern:

- `Proxying /p/... to ... without shared buffer`
- `Master playlist detected...`
- successful segment fetches without repeated source re-selection loops

Problematic pattern:

- repeated `tryAllStreams failed ... no available streams`
- repeated source exclusions with no recovery
- DNS lookup timeout/temporary errors
- parse failure logs around M3U8 passthrough

## Common Misconfigurations

- `BASE_URL` points to loopback or unreachable interface for Android device
- host firewall blocks Android client to proxy port
- stale DNS on Android TV while server uses different resolver state
- source index filtering leaves no candidate URL maps for specific slugs

## Escalation Bundle for Debugging

Collect and share:

- stream token path and timestamp
- 40-80 lines of logs around first failed request
- `BASE_URL`, `SHARED_BUFFER`, `FALLBACK_DNS_SERVERS` values
- whether VLC works from same Android-side network
- one failing Android app name/version/device model

## Related Files

- `handlers/stream_http.go`
- `proxy/loadbalancer/instance.go`
- `proxy/stream/stream_instance.go`
- `proxy/stream/failovers/m3u8.go`
- `proxy/stream/failovers/m3u8_helpers.go`
