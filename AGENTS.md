# Repository Guidelines

## Project Overview

getterr is a tiny Go service that replaces Huntarr's paced backlog searches. On a schedule set per search, it asks Sonarr/Radarr to search their "Wanted" lists (Missing and Cutoff Unmet) and Bazarr to search missing subtitles, either over the whole list or a batch at a time. It ships as a scratch Docker image, `ghcr.io/pentago/getterr`. `README.md` is for end users and stays in plain, non-technical English; technical detail belongs here.

## Architecture & Data Flow

One file, `main.go`, stdlib only. Apps are handled uniformly: generic code never checks which app it has.

```
main → search.schedule() → go run() → connect() → loop: sleep until last+pace → search.trigger()
                                                          ├─ batch == 0 → app.searchAll(command)
                                                          └─ batch > 0  → app.searchBatch(list, cursor, n) → next()
```

- **`searches`** has one row per search: `{app, setting, list, command, desc}`. `setting` (e.g. `SONARR_MISSING`) gives `<setting>_PACE`, `<setting>_BATCH_SIZE` and the state file `$STATE_DIR/sonarr-missing`; `appVar()` (text before the first `_`) gives the shared `<APP>_URL` / `<APP>_API_KEY`. To add a search, add a row.
- **`app` interface:** `status`, `searchAll(command)`, `searchBatch(list, cursor, n) (last, msg, err)`.
  - `arr{items, command, idsField}` = Sonarr/Radarr via `/api/v3` (works on Sonarr v4/v5, Radarr v6/v7). Whole list: `POST /api/v3/command {"name": …}`. Batch: `GET /api/v3/wanted/<missing|cutoff>?monitored=true&pageSize=1000000`, then `EpisodeSearch {episodeIds}` / `MoviesSearch {movieIds}`.
  - `bazarr{}` = Bazarr's unversioned `/api`. Whole list: `POST /api/system/tasks?taskid=…`. Batch: `GET /api/<episodes|movies>/wanted` (no paging params = everything), then one `PATCH /api/<list>/subtitles?…` per missing language.
- **`next[T]`** is the shared batch logic: sort by ID, take n after the cursor ID, wrap around.
- **`main`** validates every enabled search, rejects the removed `SONARR_BATCH_SIZE`/`RADARR_BATCH_SIZE`, checks the state dir is writable, then starts one goroutine per search and blocks on `select {}`.

### Non-obvious invariants

- **Success is one exact status** (`conn.do`'s `want`): Sonarr/Radarr commands `201`, Bazarr task POST and subtitle PATCH `204`, GETs `200`. Anything else (proxy redirect, login page) means nothing was queued.
- **Redirects are never followed** (`client.CheckRedirect`): Go would forward `X-Api-Key`, and a POST would become a GET. Client timeout is 2m because whole Wanted lists can be big.
- **`connect`:** `*url.Error` and 5xx are retried every `retryDelay` (an app still starting looks like a mistyped address). Any other status, a non-JSON body, or a different app name returns an error that `run` makes fatal. After connecting, errors are never fatal.
- **Batches rotate by ID cursor for every app.** Sonarr/Radarr Wanted lists can't be sorted by ID (sortKey allowlists in their `MissingController`/`CutoffController`), so the whole list is read, and `totalRecords > len(records)` is an error so a future page cap can't silently shrink the rotation. `lastSearchTime` is deliberately unused: Sonarr/Radarr only set it when an indexer was searched, so tag-excluded items would starve every batch. A failure mid-batch re-queues the queued part on retry.
- **State:** `<state>` holds the last successful run (RFC3339), `<state>.cursor` the last batch ID. Missing/unparseable = run now / start at lowest ID. File names are deployed state; renaming them re-runs every search once. Startup fails if `$STATE_DIR` isn't writable (Docker creates missing bind-mount dirs as root); later write failures are only logged.
- **Paces:** `<N><unit>`, positive N, case-sensitive `m h d w M` (M = 30 days), overflow rejected. **No minimum, by the user's explicit choice** (`1m` with batch 1 is a valid trickle); don't add pace floors. In v0.1.1 and earlier `m` meant months.
- **Bazarr:** the whole-list task POST reschedules Bazarr's own job to now, so users set it to "Never" in Bazarr. The task only exists while Bazarr is connected to Sonarr/Radarr; otherwise Bazarr answers 500 (`bazarr.searchAll` adds that hint). Its API key is the top-level `auth.apikey`.
- **Batch commands run as manual searches**, so Radarr's `MoviesSearch` also searches monitored movies that aren't available yet.
- **No signal handling:** default SIGTERM exit works as PID 1 in scratch; worst case one search re-runs.

## Key Directories

- `/` — all source (`main.go`, `main_test.go`), `go.mod`, `Dockerfile`, docs.
- `.github/workflows/` — `test.yml` (reusable), `ci.yml`, `release.yml`; `.github/dependabot.yml`.
- `.worktrees/` — `wt` worktrees, ignored by git and skipped by `./...` (leading dot).

## Development Commands

These mirror CI (`test.yml`); run them before a PR:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...   # checks = ["all"] in staticcheck.conf
go test -race ./...
docker buildx build --platform linux/amd64,linux/arm64 .
docker run --rm $(docker build -q .)                      # must exit 1: no *_PACE variable set
nix run nixpkgs#actionlint -- .github/workflows/*.yml     # after workflow edits
```

Run locally: `mkdir -p /tmp/getterr && STATE_DIR=/tmp/getterr SONARR_URL=http://127.0.0.1:8989 SONARR_API_KEY=... SONARR_MISSING_PACE=1d go run .`

## Code Conventions & Common Patterns

- **Single file, root `package main`, no dependencies** (no `go.sum`; the Dockerfile copies only `go.mod *.go`). A new dependency also needs a `gomod` entry in `dependabot.yml`.
- **Per-app behaviour lives behind `app`**; generic code (`schedule`, `trigger`, `connect`, `run`, `next`) stays app-agnostic. No `if app == "bazarr"` checks.
- **Logging:** stdlib `log`, `"<app>: <desc> - <message>"` via `search.String()` (e.g. `sonarr: searching missing episodes - queued search for 10 episodes`). No levels.
- **Errors:** plain `fmt.Errorf`/`%w`, naming the env var involved; `statusError` for unexpected HTTP codes (`errors.AsType[statusError]`). `log.Fatalf` only for startup/config problems; runtime failures log and retry after `retryDelay`.
- **Package-level state** is limited to config tables (`searches`, `units`), the shared `client`, and `retryDelay` (a var so tests can shorten it).
- Comments explain *why* (API quirks, invariants), not what.

## Important Files

- `main.go` — `searches`, `app`/`arr`/`bazarr`, `conn.do`, `next`, `parsePace`, `schedule`, `trigger`, `connect`, `main`, `run`.
- `main_test.go` — all tests.
- `Dockerfile` — build on `$BUILDPLATFORM`, cross-compile with `CGO_ENABLED=0` (no QEMU); runtime `scratch` with the CA bundle, `/getterr`, `/data` owned by `65532:65532`, `USER 65532:65532`. No shell (no `docker exec`, no shell healthchecks), no tzdata (mount `/etc/localtime` for local log times).
- `.github/workflows/ci.yml` — PRs and pushes to `main`: `test.yml` + docker build; pushes to `main` publish `:dev` (plus `:pr-<N>`) for `linux/amd64,linux/arm64` with attestation.
- `.github/workflows/release.yml` — `v*` tags run `test.yml`, then publish `:<version>`, plus `:latest` only when the tag has no `-`; hyphenated tags become GitHub pre-releases.

## Runtime/Tooling Preferences

- **Go** version in `go.mod` (1.27) must match the `golang:1.27-alpine` build base; CI reads `go-version-file: go.mod`.
- **Platforms:** always publish `linux/amd64` and `linux/arm64`. The production host is x86_64 and pulls `:dev`, so never drop amd64. `unknown/unknown` manifest entries are buildx attestations.
- **GitHub Actions:** pin full commit SHAs with a `# vX.Y.Z` comment (resolve via `git ls-remote <url> 'refs/tags/<tag>^{}'`); `persist-credentials: false` on checkout; custom logic in bash `run:` steps with `gh`, never `actions/github-script`. Dependabot updates `docker` and `github-actions` monthly, grouped, 7-day cooldown.
- **Git:** work in a `wt` worktree (`wt switch --create <branch> --base main`), PR to `main`, never commit to `main`. Commits are signed. Use the `pentago` handle, never a real name or email.
- Missing CLI tools: use `nix run nixpkgs#<tool>`.

## Testing & QA

- Stdlib `testing` + `net/http/httptest` stubs only; `go test -race ./...`.
- Covered: `TestParsePace` (units, case, overflow), `TestSchedule` (bad batch size is fatal, any pace allowed, unset pace = disabled), `TestConnect` (fatal vs retried, via the real no-redirect `client`), `TestArrBatch`/`TestBazarrBatch` (cursor rotation and wrap-around through `testBatches`; the arr stub pages like Sonarr so a missing `pageSize` fails).
- Patterns: method/path `switch` in the stub with `400` for unexpected requests, `t.Setenv`, `t.TempDir()` for cursor files, `atomic.Int32` when hijacked connections are involved, `retryDelay = 0` with `defer` restore.
- Not covered: `main` wiring, `run` scheduling/state, whole-list `searchAll`. When changing them, check against a throwaway stub server: queued → state file written; 302/200/401 → `failed, retrying`; a restart honours the saved state.
