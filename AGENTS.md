# Repository Guidelines

## Project Overview

getterr is a tiny Go service that replaces Huntarr's paced backlog searches. On a schedule set per search, it asks Sonarr/Radarr to search their "Wanted" lists (missing and cutoff unmet) and Bazarr to search missing subtitles, either over the whole list or a batch at a time. It ships as a scratch Docker image, `ghcr.io/pentago/getterr`. `README.md` is for end users and stays in plain, non-technical English; technical detail belongs here.

## Architecture

Everything lives in `main.go` (`package main`, stdlib only). Read it for the details; this section covers the shape.

- `searches` has one row per search: `{app, setting, list, command, desc}`. `setting` (e.g. `SONARR_MISSING`) gives `<setting>_PACE`, `<setting>_BATCH_SIZE` and the state file name (`sonarr-missing`); the part before the first `_` gives `<APP>_URL` and `<APP>_API_KEY`. To add a search, add a row.
- `app` is the interface each app implements: `status`, `searchAll(command)` and `searchBatch(list, cursor, n)`. `arr` covers Sonarr and Radarr (API v3); `bazarr` covers Bazarr. Generic code never checks which app it has.
- `main` validates every enabled search (`schedule`), checks the state dir is writable, then starts one goroutine per search running `run`: `connect`, then sleep until `last + pace`, `trigger`, persist `last`. A failed trigger retries after `retryDelay` without advancing `last`.
- `trigger` calls `searchAll` without a batch size. With one, it reads the cursor file, calls `searchBatch`, and writes back the last ID searched. `next` does the shared part: sort by ID, take n after the cursor, wrap around.

## Non-obvious invariants

- **Success is one exact status.** Sonarr/Radarr commands: `201`. Bazarr task POST and subtitles PATCH: `204`. GETs: `200`. Anything else (a proxy's redirect or login page) means nothing was queued.
- **Redirects are never followed:** Go would forward `X-Api-Key` to the target, and a POST would turn into a GET.
- **`connect` errors:** transport errors and 5xx are retried (an app or proxy still starting looks the same as a mistyped address); every other status, a non-JSON body, or a different app name is fatal. Once connected, errors are never fatal.
- **Batch commands run as manual searches** (`CommandTrigger.Manual`), so Radarr's `MoviesSearch` also searches monitored movies that aren't available yet, like `MissingMoviesSearch` does.
- **State** lives in `$STATE_DIR` (default `/data`): `<state>` holds the last run as RFC3339, `<state>.cursor` the last batch ID. Missing or unparseable means "run now" / "start at the lowest ID". File names are deployed state; renaming them re-runs every search once. Startup fails if the dir isn't writable (Docker creates missing bind-mount dirs as root); later write failures are only logged.
- **Batches rotate by ID cursor for every app.** Sonarr/Radarr Wanted lists can't be sorted by ID (sortKey allowlists in `MissingController`/`CutoffController`), so `arr.searchBatch` reads the whole list with `pageSize=1000000` (no server cap today) and errors if `totalRecords` says the list was cut short. Bazarr's wanted list returns everything without paging params. `lastSearchTime` isn't used: Sonarr/Radarr only set it when an indexer was searched, so tag-excluded items would stay "never searched" and starve every batch. A failure mid-batch re-queues the queued part on retry.
- **Pace floor (`schedule`):** whole-list searches at least `1h`. It protects indexers and rejects v0.1.1-era configs where `m` meant months. Batches have no floor so slow trickles (e.g. one Bazarr item a minute) stay possible; v0.1.1 batch configs fail on `SONARR_BATCH_SIZE`/`RADARR_BATCH_SIZE` (v0.1.2 and earlier), which are rejected so they can't silently become whole-list searches.
- **Pace format (`parsePace`):** `<N><unit>`, positive integer N, case-sensitive units `m h d w M` (M = 30 days), overflow rejected.
- **Bazarr whole-list search** is `POST /api/system/tasks?taskid=…`, which reschedules Bazarr's own job to now, so users set that task to "Never" in Bazarr. The task only exists while Bazarr is connected to Sonarr/Radarr; otherwise Bazarr answers 500.
- **Bazarr batches** send one `PATCH /api/<episodes|movies>/subtitles` per missing language. Its API key is the top-level `auth.apikey` in Bazarr's config, sent as `X-Api-Key` (headers are case-insensitive).
- **API versions:** `/api/v3` works on Sonarr v4/v5 and Radarr v6/v7. Bazarr's API is unversioned.
- **No signal handling:** Go's default SIGTERM exit works as PID 1 in scratch. Worst case, a kill between the queued reply and the state write re-runs one search.

## Development Commands

These mirror CI (`.github/workflows/test.yml`); run them before opening a PR:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...   # config: staticcheck.conf
go test -race ./...
docker buildx build --platform linux/amd64,linux/arm64 .
nix run nixpkgs#actionlint -- .github/workflows/*.yml     # after workflow edits
docker run --rm $(docker build -q .)                        # must exit 1: no *_PACE variable set
```

Run locally: `mkdir -p /tmp/getterr && STATE_DIR=/tmp/getterr SONARR_URL=http://127.0.0.1:8989 SONARR_API_KEY=... SONARR_MISSING_PACE=1d go run .`

## Conventions

- **No dependencies**, so no `go.sum`; the Dockerfile copies only `go.mod *.go`. Adding one needs a `gomod` entry in `.github/dependabot.yml`.
- **Single file** in the root `package main`.
- **Logging:** stdlib `log`, `"<app>: <desc> - <message>"` via `search.String`. No levels.
- **Errors:** plain `fmt.Errorf`; `log.Fatalf` only at startup.
- **Tests:** stdlib `testing` with `httptest` stubs. They cover pace parsing, the pace floor, `connect` fatal vs retry, and batch rotation for `arr` and `bazarr`. `run` (scheduling, state) isn't covered; check it with a stub server when changing it.

## Build & Release

- **Dockerfile:** build stage on `$BUILDPLATFORM`, cross-compiled with `CGO_ENABLED=0` (no QEMU). Runtime is `scratch` with the CA bundle, `/getterr`, and `/data` owned by `65532:65532`. No shell (no `docker exec`, no shell healthchecks), no tzdata (mount `/etc/localtime` for local log times). Go in `go.mod` must match the `golang:*-alpine` base.
- **CI (`ci.yml`):** PRs and pushes to `main` run `test.yml` and a docker build. Pushes to `main` publish `:dev` (and `:pr-<N>`) for `linux/amd64,linux/arm64`. The production host is x86_64 and pulls `:dev`, so never drop amd64.
- **Release (`release.yml`):** `v*` tags run `test.yml`, then publish `:<version>`, plus `:latest` unless the tag has a `-` (those become GitHub pre-releases).
- **Actions:** pin to full commit SHAs with a `# vX.Y.Z` comment; `persist-credentials: false` on checkout; custom logic in bash `run:` steps with `gh`, never `actions/github-script`.
- **Git:** work in a `wt` worktree (`wt switch --create <branch> --base main`), PR to `main`, never commit to `main`. Commits are signed. Use the `pentago` handle, never a real name or email.
- `unknown/unknown` entries in the image manifest are buildx provenance attestations, not platforms.
