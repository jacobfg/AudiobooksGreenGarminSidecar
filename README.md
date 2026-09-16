# AudiobooksGreen Garmin Sidecar

Personal sidecar for [AudiobooksGreen](https://github.com/fabrikant/AudiobooksGreen). It presents virtual watch playlists that merge exact ABS playlist IDs across libraries, pre-renders Garmin-compatible MP3 media, and serves the generated chunks.

It intentionally does **not** modify the generic AudiobooksGreen proxy or the watch app.

## Configure

Copy `config.example.yaml` to `config.yaml`, replace playlist IDs, and set these environment variables:

- `ABS_URL`: private ABS base URL reachable from the sidecar.
- `ABS_TOKEN`: API token for the ABS account whose playlists are configured.
- `CACHE_DIR` (default `/app/cache`), `POLL_INTERVAL_SECONDS` (default `300`), `AUTH_CACHE_SECONDS` (default `300`).
- `CACHE_CHUNK_MINUTES` (default `30`) and `CACHE_BITRATE` (default `96k`).

`docker compose -f docker-compose.example.yml up -d --build` starts the service.

The worker runs one conversion at a time. A cache entry is published only after all chunks and its manifest are complete. Failed jobs remain under `.staging` for diagnosis and are retried by the next poll.

## Watch and Traefik

Set the watch **Server** and **Proxy** fields to the same public HTTPS hostname and enable **prefer proxy requests**. Adapt `traefik.dynamic.example.yml` to that hostname and network. Its specific sidecar routers must have higher priority than the broad proxy/API routers.

The sidecar shows only virtual playlists. It verifies the watch's Bearer token with ABS before returning virtual data or audio. Progress and covers continue to use ABS through the normal `/api/*` route.

Cached item directories are global (`<cache>/<ABS item ID>`), not playlist-specific. On each fully successful playlist poll, the sidecar deletes cache entries not referenced by any configured source playlist.

## Trigger and inspect a playlist

An authenticated request to the status endpoint immediately refreshes the named
virtual playlist and queues every missing rendition. It reports the ABS source
files and duration, the configured ABS user's playback position, and cache
state (`ready`, `queued`, `generating`, `failed`, or `missing`):

```sh
curl --fail-with-body --silent "https://garmin-abs.example.net/garmin/playlist-status/Adventure" \
  --header "Authorization: Bearer $ABS_TOKEN" | jq
```
