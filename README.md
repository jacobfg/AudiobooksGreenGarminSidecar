# AudiobooksGreen Garmin Sidecar

Go sidecar for [AudiobooksGreen](https://github.com/fabrikant/AudiobooksGreen). It presents virtual watch playlists that merge ABS playlists across libraries, pre-renders Garmin-compatible MP3 media, and serves the generated chunks.

It intentionally does **not** modify the generic AudiobooksGreen proxy or the watch app.

## Configure

Configure the service entirely through environment variables:

- `PLAYLIST_NAMES`: required CSV list of ABS playlist names, for example `Adventure,Running,Science Fiction`.
- `ABS_URL`: private ABS base URL reachable from the sidecar.
- `ABS_TOKEN` or `ABS_TOKEN_FILE`: API token for the ABS account whose playlists are configured, or the path to a UTF-8 file containing it. Set only one. Surrounding whitespace is stripped from file contents; empty or unreadable files fail startup.
- `CACHE_DIR` (default `/app/cache`), `POLL_INTERVAL_SECONDS` (default `300`), `AUTH_CACHE_SECONDS` (default `300`).
- `STAGING_RETENTION_SECONDS` (default `86400`, 24 hours): retention for failed or cancelled conversion scratch files; must be positive.
- `CACHE_CHUNK_MINUTES` (default `30`) and `CACHE_BITRATE` (default `96k`).

For example:

```yaml
environment:
  PLAYLIST_NAMES: 'Adventure,Running,"History, Politics"'
```

Each name creates one watch playlist by merging **all** matching ABS playlists across every library accessible to the configured ABS user. Matching ignores case and repeated whitespace. CSV quoting supports commas in names; empty entries and malformed CSV fail startup. Repeated equivalent names are deduplicated. Discovery uses paginated per-library playlist listings and deduplicates books by ABS item ID. Source playlist IDs are sorted for consistent merged order; each source's book order is preserved.

Configured playlists remain visible when no source matches. Deleted and recreated playlists are rediscovered automatically. A failed or malformed library/playlist response preserves the previous complete snapshot and cache. Multiple matching playlists within one library are merged as well.

`GET /audiobookshelf/playlists` lists the stable IDs and display names. `GET /audiobookshelf/playlist?playlist_id=Garmin` accepts either a configured name (ignoring case and extra whitespace) or the generated ID (`garmin-garmin` in this example). Exact IDs take precedence if a name happens to match another playlist's ID. The `playlist_id` query parameter is required.

Watch playlist IDs are derived from normalized names: `Adventure` becomes `garmin-adventure`. Punctuated or non-ASCII names receive a stable hash suffix. Conflicting IDs (for example `A B` and `A-B`) fail startup with a clear error. Renaming an entry changes its ID; changing only case or whitespace does not.

To migrate from the YAML-based Go version, set `PLAYLIST_NAMES`, remove `SIDECAR_CONFIG` and the `config.yaml` mount, and keep the cache volume. YAML config files and explicit source-ID mappings are no longer used. Existing complete audio remains readable; manifests without source fingerprints are rebuilt once in the background to establish source tracking. Custom virtual IDs may change, requiring the watch playlist to be selected again.

`docker compose -f docker-compose.example.yml up -d --build` starts the service.

To use a Docker Compose secret, save the token in `abs_token.txt`, remove `ABS_TOKEN` from the Compose environment, and uncomment `ABS_TOKEN_FILE` and both `secrets` sections in `docker-compose.example.yml`. Compose mounts the file at `/run/secrets/abs_token`. Keep the token file out of version control. The token is read at startup; recreate the container after changing it.

The worker runs one conversion at a time. A cache entry is published only after all chunks and its manifest are complete. Failed jobs remain under `.staging` for diagnosis and are retried by the next poll. Abandoned staging directories are automatically removed at startup and on each poll once their retention period expires, even when ABS is unavailable. Active conversions are protected regardless of age. Retention starts when an attempt fails or is cancelled; after an abrupt process exit, cleanup uses the staging directory’s last modification time. Successful attempts delete their own staging directory immediately. Use one sidecar process per cache volume so active work is tracked by its owner.

## Live updates and polling

The sidecar connects to ABS using its Engine.IO v4 / Socket.IO JSON event interface over WebSocket, authenticating with the same service token. Playlist, library, and item changes trigger a refresh after a 500 ms batching window. Multiple events during a refresh cause another refresh rather than being lost. Notifications are hints: the sidecar retrieves a complete HTTP snapshot before queueing or pruning media. Conversion still runs one book at a time.

The connection uses `/socket.io/` under the `ABS_URL` base path and supports HTTPS. Reconnection uses exponential backoff with jitter, capped at one minute. Each authenticated connection triggers a full refresh to recover missed events. `POLL_INTERVAL_SECONDS` continues to trigger independent refreshes during socket outages; no proxy changes are needed if the sidecar reaches ABS directly. If using a reverse proxy between them, it must allow WebSocket upgrades. This client targets ABS's default-namespace JSON events; unsupported protocol messages cause reconnection while REST polling continues.

Each successful refresh checks source audio metadata (file identity, order, name, size, modification timestamps, duration, and conversion settings). A changed fingerprint queues a rebuild; book titles, covers, and listening progress do not. Detection depends on ABS scanning changed files and reporting their updated metadata. The previous complete rendition remains available until the new manifest is atomically published. Open downloads can finish using the old rendition. Removing a book from every configured playlist cancels queued/running work and removes its cache; a superseded conversion cannot publish stale output.

`GET /garmin/status` requires the usual Bearer token and reports socket state, last successful refresh, and the latest refresh failure. Playlist-status responses include the same `sync` information. During a rebuild, `cache.state` stays `ready` while the old rendition is usable and `cache.refresh_state` reports `queued`, `generating`, or `failed`.

## Tests and container releases

Requires Go 1.27.3 or newer. Install FFmpeg and FFprobe on your PATH for local development and tests.

```sh
go test -race -cover ./...
go vet ./...
go build -trimpath -ldflags="-s -w" -o sidecar-server ./cmd/sidecar
PLAYLIST_NAMES=Adventure ./sidecar-server
```

Tests use a local mock ABS HTTP server, including authentication, playlist discovery, progress, and source downloads. The checked-in synthetic MP3 and chaptered M4B fixtures in `testdata/` exercise real FFmpeg conversion, chapter splitting, cached downloads, and HTTP range requests. No real ABS instance or token is needed. Socket protocol tests cover authentication, heartbeat, reconnects, event batching, and polling recovery. For the official Socket.IO interoperability test, install Node.js 24 and run `npm ci --prefix testdata/socketio --ignore-scripts`, then `SIDECAR_SOCKETIO_TEST=1 go test -race ./...`. CI requires this test; the standard Go command skips it when the opt-in variable is absent. Node and Socket.IO are test dependencies only and are excluded from the runtime image. FFmpeg is required; integration tests fail if it is missing.

GitHub Actions runs Go tests with race detection and vet on pushes and pull requests, builds Linux AMD64 and ARM64 images, and smoke-tests startup with both token sources, the health endpoint, authentication enforcement, and bundled FFmpeg tools.

The `scratch` runtime image contains a stripped Go binary, CA certificates, and the same static FFmpeg tools. Port 8082, the watch-facing HTTP routes, and existing complete cache manifests remain supported. Playlist configuration now uses `PLAYLIST_NAMES` instead of YAML. Keep the existing cache volume when upgrading. Conversion still runs one job at a time; FFmpeg remains the main CPU consumer during encoding. Measure idle and conversion memory with `docker stats` on your server before choosing resource limits; a smaller service runtime does not eliminate FFmpeg's memory requirements.

To publish a release, tag a commit on `main` with a stable version such as `0.1.0` (no `v` prefix) and push the tag. The publish workflow checks the tag and its ancestry, runs the tests, and pushes both architectures to `ghcr.io/jacobfg/audiobooksgreengarminsidecar` with the version and `latest` tags, using the repository's `GITHUB_TOKEN`. To use a published image, replace `build: .` in Compose with `image: ghcr.io/jacobfg/audiobooksgreengarminsidecar:0.1.0`, substituting your published version.

### Validation

The original Python suite passed all 31 tests before migration and again before its removal. Its behavior is covered by the Go regression suite, with additional real audio conversion and download tests. Additional regression tests cover uncached watch requests, pagination, interrupted downloads, encoder failures after a completed chunk, invalid-cache rebuilding, unsupported sources, ABS status failures, cancellation of an active FFmpeg process, and staging retention with active-job protection and cleanup during ABS outages. The initial Go migration reached 92.8% server-package statement coverage with race detection; `go vet` and Linux AMD64/ARM64 cross-builds passed.

The follow-up event/discovery suite also runs against the official Socket.IO 4.8.4 server. Local validation uses mocked ABS HTTP responses; deployment against your particular ABS version remains a server-side check.

An indicative local macOS ARM64 measurement against the same empty mock ABS playlist, after ten authenticated requests, showed idle RSS of **72.9 MiB for Python** and **13.7 MiB for Go** (about 81% lower). Stripped Linux binaries were about **7.7 MiB AMD64** and **7.1 MiB ARM64**. These are local process measurements, not container memory or conversion benchmarks. Docker image builds and runtime smoke tests need CI or a working Docker installation; the development machine's Docker context was unavailable.

## Scheduled dependency updates

The Renovate Bot workflow runs daily at 06:04 UTC and supports manual runs with optional debug logging, following the [container-scheduler setup](https://github.com/jacobfg/container-scheduler/blob/main/.github/workflows/renovatebot.yml). It groups stable minor and patch updates to Go modules (including indirect dependencies), the Go build image, FFmpeg, the documented Go version, and the test-only Socket.IO package into one maintenance PR. Go dependencies are recorded in `go.mod` and verified by `go.sum`; Renovate runs `go mod tidy` when updating them. CI reads its Go version from `go.mod`. Existing PRs are refreshed; no updates means no new PR. Major upgrades appear in the Dependency Dashboard issue and require checkbox approval before Renovate creates a PR. Merging remains manual. GitHub Actions and the pinned Renovate action/engine versions are maintained manually.

Renovate adds `docker.io/` to Dockerfile and Compose images without an explicit
registry, including namespaced Docker Hub images. Existing registry hosts,
locally built application and smoke-test images, and `scratch` are preserved.
Compose image versions remain under manual maintenance. Dockerfile image
versions continue to follow the application maintenance policy above.

Before running it, add an Actions secret named `RENOVATE_TOKEN` containing a dedicated GitHub PAT authorized for `jacobfg/AudiobooksGreenGarminSidecar`, with contents, pull-request, and Issues write access, permission to update workflow files (for workflow maintenance), and the read permissions required by [Renovate](https://docs.renovatebot.com/modules/platform/github/). A fine-grained token must explicitly include this repository. The dedicated token allows Renovate PRs to trigger CI; do not substitute `GITHUB_TOKEN`. The workflow checks repository access before starting. Scheduled runs begin after merge to `main`.

## Watch and Traefik

Set the watch **Server** and **Proxy** fields to the same public HTTPS hostname and enable **prefer proxy requests**. Adapt `traefik.dynamic.example.yml` to that hostname and network. Its specific sidecar routers must have higher priority than the broad proxy/API routers.

The sidecar shows only virtual playlists. It verifies the watch's Bearer token with ABS before returning virtual data or audio. Progress and covers continue to use ABS through the normal `/api/*` route.

Cached item directories are global (`<cache>/<ABS item ID>`), not playlist-specific. On each fully successful playlist poll, the sidecar deletes cache entries not referenced by any configured source playlist. Missing sources count as empty for this cleanup.

## Trigger and inspect a playlist

An authenticated request to the status endpoint immediately refreshes the named
virtual playlist and queues every missing rendition. It reports the ABS source
files and duration, the configured ABS user's playback position, and cache
state (`ready`, `queued`, `generating`, `failed`, or `missing`). While a book
is converting, `cache.generation.files` reports exact source/output file
progress such as `3/12` rather than an estimated percentage:

```sh
curl --fail-with-body --silent "https://garmin-abs.example.net/garmin/playlist-status/Adventure" \
  --header "Authorization: Bearer $ABS_TOKEN" | jq
```

Renovate automatically discovers `.github/renovate.json5` on the default branch.
The workflow does not also load it as global configuration, avoiding duplicate
custom managers. The workflow and shared update policy are identical across fittools,
container-scheduler, and AudiobooksGreenGarminSidecar: daily at 06:04 UTC, manual debug
runs, one grouped minor/patch PR, a Dependency Dashboard with approval required
for major upgrades, and no automerge. Language-specific dependency managers stay
local to each repository. Branch-update failures are promoted to errors so a
rejected push fails the Action. The access preflight only checks visibility;
it does not prove the token can push. A push rejected with HTTP 403 requires
checking the token's repository selection, write permissions, and repository
rules. Issues write access is required for the dashboard.
