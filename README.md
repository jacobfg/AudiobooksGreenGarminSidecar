# AudiobooksGreen Garmin Sidecar

Personal sidecar for [AudiobooksGreen](https://github.com/fabrikant/AudiobooksGreen). It presents virtual watch playlists that merge ABS playlists across libraries, pre-renders Garmin-compatible MP3 media, and serves the generated chunks.

It intentionally does **not** modify the generic AudiobooksGreen proxy or the watch app.

## Configure

Copy `config.example.yaml` to `config.yaml`, set your playlist sources, and set these environment variables:

- `ABS_URL`: private ABS base URL reachable from the sidecar.
- `ABS_TOKEN` or `ABS_TOKEN_FILE`: API token for the ABS account whose playlists are configured, or the path to a UTF-8 file containing it. Set only one. Surrounding whitespace is stripped from file contents; empty or unreadable files fail startup.
- `CACHE_DIR` (default `/app/cache`), `POLL_INTERVAL_SECONDS` (default `300`), `AUTH_CACHE_SECONDS` (default `300`).
- `CACHE_CHUNK_MINUTES` (default `30`) and `CACHE_BITRATE` (default `96k`).

`source_playlist_ids` accepts `library name/playlist name` entries and existing ABS playlist IDs, including a mix of both:

```yaml
virtual_playlists:
  - id: garmin-adventure
    name: Adventure
    source_playlist_ids:
      - "Library A/Adventure Books"
      - "Library B/Adventure Books"
```

Library and playlist names are matched without regard to case. Leading/trailing whitespace is ignored and repeated whitespace is treated as a single space. Use literal names, including spaces, without URL encoding. The first `/` separates the library from the playlist; library names containing `/` require a playlist ID instead. If names are ambiguous after normalization, use an ID to select the intended playlist.

Names are resolved again on every poll and status refresh. Missing libraries or playlists are silently treated as empty sources, including playlists deleted when their last book is removed. A recreated playlist with the same name is picked up on the next refresh without changing the config or restarting. Missing playlist IDs are also skipped, but cannot follow a recreated playlist's new ID. Authentication, network, and other server errors still fail the refresh and preserve existing playlist/cache data.

`docker compose -f docker-compose.example.yml up -d --build` starts the service.

To use a Docker Compose secret, save the token in `abs_token.txt`, remove `ABS_TOKEN` from the Compose environment, and uncomment `ABS_TOKEN_FILE` and both `secrets` sections in `docker-compose.example.yml`. Compose mounts the file at `/run/secrets/abs_token`. Keep the token file out of version control. The token is read at startup; recreate the container after changing it.

The worker runs one conversion at a time. A cache entry is published only after all chunks and its manifest are complete. Failed jobs remain under `.staging` for diagnosis and are retried by the next poll.

## Tests and container releases

GitHub Actions runs the Python tests on pushes and pull requests, builds Linux AMD64 and ARM64 images, and smoke-tests startup with both token sources, the health endpoint, authentication enforcement, and the bundled FFmpeg tools. Run the Python tests locally with `python -m pip install -r requirements-dev.txt` followed by `python -m pytest -q`.

To publish a release, tag a commit on `main` with a stable version such as `0.1.0` (no `v` prefix) and push the tag. The publish workflow checks the tag and its ancestry, runs the tests, and pushes both architectures to `ghcr.io/jacobfg/audiobooksgreengarminsidecar` with the version and `latest` tags, using the repository's `GITHUB_TOKEN`. To use a published image, replace `build: .` in Compose with `image: ghcr.io/jacobfg/audiobooksgreengarminsidecar:0.1.0`, substituting your published version.

## Scheduled dependency updates

The Renovate Bot workflow runs daily at 06:04 UTC and supports manual runs with optional debug logging, following the [container-scheduler setup](https://github.com/jacobfg/container-scheduler/blob/main/.github/workflows/renovatebot.yml). It groups stable minor and patch updates to the Python and FFmpeg Docker images, the CI Python version, and packages in `requirements.txt` and `requirements-dev.txt` into one maintenance PR. Direct Python dependencies are pinned so Renovate can propose version changes; transitive dependencies are still resolved by pip. Existing PRs are refreshed; no updates means no new PR. Major upgrades appear in the Dependency Dashboard issue and require checkbox approval before Renovate creates a PR. Merging remains manual. GitHub Actions and the pinned Renovate action/engine versions are maintained manually.

Before running it, add an Actions secret named `RENOVATE_TOKEN` containing a dedicated GitHub PAT authorized for `jacobfg/AudiobooksGreenGarminSidecar`, with contents, pull-request, and Issues write access, permission to update workflow files (for the CI Python version), and the read permissions required by [Renovate](https://docs.renovatebot.com/modules/platform/github/). A fine-grained token must explicitly include this repository. The dedicated token allows Renovate PRs to trigger CI; do not substitute `GITHUB_TOKEN`. The workflow checks repository access before starting. Scheduled runs begin after merge to `main`.

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
