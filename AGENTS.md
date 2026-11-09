# Repository Guidelines

## Project Overview

getterr is a tiny Go service that replaces Huntarr's paced backlog searches. On a schedule set per search, it POSTs Sonarr/Radarr "Wanted" commands (missing and cutoff-unmet) to `/api/v3/command`, either for the whole list or for a batch of the least recently searched items. It also paces Bazarr's missing-subtitles searches, whole-list or batched, because Bazarr's own scheduler searches everything at once and trips provider rate limits on big libraries. It ships as a scratch Docker image published to `ghcr.io/pentago/getterr`. `README.md` is for end users and stays in plain, non-technical English; technical detail belongs here.

## Architecture & Data Flow

Everything lives in `main.go` (`package main`, stdlib only).

1. `main` reads `STATE_DIR` (default `/data`) and builds one shared `http.Client`. It then walks the `jobs` table.
2. `jobs` has one row per search: `{app, command, paceVar, batchVar, wanted}`.

   | Pace env var | Command | Wanted list |
   |---|---|---|
   | `SONARR_MISSING_PACE` | `MissingEpisodeSearch` | `missing` |
   | `SONARR_UNMET_PACE` | `CutoffUnmetEpisodeSearch` | `cutoff` |
   | `RADARR_MISSING_PACE` | `MissingMoviesSearch` | `missing` |
   | `RADARR_UNMET_PACE` | `CutoffUnmetMoviesSearch` | `cutoff` |
   | `BAZARR_SERIES_PACE` | task `wanted_search_missing_subtitles_series` | `episodes` |
   | `BAZARR_MOVIES_PACE` | task `wanted_search_missing_subtitles_movies` | `movies` |

   To add a search, add a row. `<APP>_URL` and `<APP>_API_KEY` are derived from `app`; `batchVar` names the optional batch size (positive integer). Sonarr/Radarr share `<APP>_BATCH_SIZE` between both searches of the app; Bazarr has `BAZARR_SERIES_BATCH_SIZE` and `BAZARR_MOVIES_BATCH_SIZE`, since episodes and movies differ a lot in count. `batches` maps Sonarr/Radarr `app` to its batch sort key and by-ID command (`EpisodeSearch {episodeIds}`, `MoviesSearch {movieIds}`). Bazarr is special-cased on `app == "bazarr"` in `connect` and `trigger`; for it `command` is a scheduler task ID and `wanted` the API path segment.
3. Each enabled job gets its own goroutine running `run`, and `main` blocks with `select {}`. Jobs share no mutable state; each owns its own state file. `run` first calls `connect`, which loops on `GET /api/v3/system/status` until it gets `200` with a matching `appName` (Bazarr: `GET /api/system/status`, which has no `appName`, so a non-empty `data.bazarr_version` counts as the match):
   - Fatal (won't fix itself): any other non-5xx status (401 bad key, 404 wrong URL base, 3xx auth proxy), a non-JSON body (proxy login page), or an `appName` for the other app.
   - Retried every `retryDelay`: transport errors (`*url.Error`: refused, DNS, timeout) and 5xx (a reverse proxy in front of an app that's still starting). A mistyped host or port is indistinguishable from an app that's still booting, so it's retried and logged as `cannot reach`, not fatal.
   - Each app is checked once per enabled job, in that job's goroutine, so an unreachable Sonarr doesn't hold up Radarr.
4. `run` loops:
   - Sleep until `last + pace`, then call `trigger`.
   - On success, set `last = now` and write it to the job's state file.
   - On failure, log, sleep `retryDelay` (5m) and retry. `last` is not advanced, so the pace timer only starts after a success.
5. `trigger` sends `POST <url>/api/v3/command` with the `X-Api-Key` header (via `api`):
   - No batch size: `{"name":"<command>"}`, which searches the whole Wanted list.
   - Batch size N: `oldestSearched` reads `GET /api/v3/wanted/<wanted>?monitored=true&pageSize=N&sortKey=<episodes|movies>.lastSearchTime` once ascending and once descending, keeps the N never-searched-then-oldest IDs, and posts the by-ID command. An empty list logs `nothing to search` and counts as a run. Rotation needs no state: Sonarr/Radarr set `lastSearchTime` on every searched item that at least one indexer matched.
6. For Bazarr, `trigger` instead:
   - No batch size: `POST /api/system/tasks?taskid=<command>`, expecting `204`. Bazarr implements this as APScheduler `modify_job(next_run_time=now)`, so it also shifts Bazarr's own schedule for that task; users set the task to "Never" in Bazarr. If the task doesn't exist (Sonarr/Radarr integration disabled in Bazarr) Bazarr returns 500 and the job retries every `retryDelay`.
   - Batch size N: `bazarrBatch` reads the whole `GET /api/<episodes|movies>/wanted` list (no paging params = all rows), sorts by `sonarrEpisodeId`/`radarrId`, takes the N items after the cursor ID (wrapping around), and sends one `PATCH /api/<episodes|movies>/subtitles?…&language=&forced=&hi=` per missing language, expecting `204` (Bazarr queues each as a job, run `concurrent_jobs` at a time). Then it writes the last ID to `<state file>.cursor`.

### Non-obvious invariants

Don't regress these without a reason; only the batch ordering has an automated test.

- **Only `201 Created` counts as success** for commands, and only `200 OK` for the Wanted GETs (`api`'s `want`). 201 is what Sonarr/Radarr return for a queued command (`StartCommand` returns `Created`). A 200 or 3xx on a POST usually means an auth proxy's login page, and nothing was queued; a login page on a GET fails JSON decoding. Bazarr's task POST and subtitles PATCH return `204 No Content`.
- **Redirects are never followed** (`CheckRedirect` returns `http.ErrUseLastResponse`). Go would forward `X-Api-Key` to the redirect target, and a POST that gets a 302 turns into a GET.
- **State persistence:** one file per job in `$STATE_DIR`, holding a single RFC3339 timestamp. This stops container restarts from re-running searches early or skipping long paces.
  - A missing or unparseable file means "run now".
  - A failed write is only logged; that job then runs on the in-memory time until the next restart.
  - File names are part of deployed state: renaming them makes every existing deployment run each search once more, unless the files are renamed on the host before the new image starts.
  - Bazarr batch jobs also keep `<state>.cursor`, holding the last searched ID. A missing or unparseable cursor starts at the lowest ID; a failed write is only logged, and then every run repeats the same batch.
- **Bazarr batches rotate by cursor, not by search time.** Bazarr's wanted API exposes no last-search time (its adaptive-search `failedAttempts` isn't in the response), and the per-language `PATCH …/subtitles` is a manual search that ignores adaptive searching. `TestBazarrBatch` covers the order and the wrap-around. A failure mid-batch re-queues the already-queued part of the batch on retry.
- **Batch ordering is database-independent.** Sonarr/Radarr pass `sortDirection` straight into SQL `ORDER BY` without `NULLS FIRST/LAST`, so never-searched items (null `lastSearchTime`) sort first ascending on SQLite but last on Postgres. `oldestSearched` reads N from both ends and merges, so either way the N never-searched-then-oldest items are in the union. `TestOldestSearched` covers both databases.
  - Batch sort keys must stay in the controllers' allowlists (`MissingController`/`CutoffController` in `*.Api.V3/Wanted`); an unknown key silently falls back to the default sort and every run re-searches the same first N.
  - Sonarr/Radarr only update `lastSearchTime` when at least one indexer was searched. Items that no indexer matches (indexer tags) stay never-searched and are picked first in every batch; if N or more such items exist, batches never reach anything else.
  - API-posted commands run with `CommandTrigger.Manual`, so `MoviesSearch` also searches monitored movies that aren't available yet, as `MissingMoviesSearch` already does.
- **Pace format (`parsePace`):** `<N><unit>`, where `N` is a positive integer.
  - Units are case-sensitive: `h`, `d`, `w`, and `m` (30 **days**, not minutes).
  - Overflow is rejected (`time.Duration(n) > math.MaxInt64/unit`); otherwise an overflowed pace makes `run` hot-loop.
- **Startup errors are fatal:** a bad pace, a pace set without its URL or key, a batch size that isn't a positive integer, no pace set at all, or a fatal `connect` result. An unset pace just disables that job. Once `connect` has passed, errors are never fatal. A fatal `connect` in one job can exit while another job's search is in flight; worst case, that search re-runs once on restart.
- **No signal handling:** Go's default SIGTERM exit works even as PID 1 in scratch (`docker stop` takes about 0.1s). The worst case, a kill between the 201 and the state write, re-runs one search.
- **API version:** `/api/v3` works on Sonarr v4 and v5 (v5 keeps `Sonarr.Api.V3` alongside `V5`) and on Radarr v6/v7 (v3 only). Switch Sonarr to `/api/v5` only if v3 is removed. Bazarr's API is unversioned (`/api`); it reads the key from `X-API-KEY`, and header names are case-insensitive, so `api`'s `X-Api-Key` works.

## Key Directories

- `/`: all source (`main.go`, `main_test.go`), `go.mod`, `staticcheck.conf` (every check enabled), `Dockerfile`, `README.md`, `AGENTS.md`.
- `.github/workflows/`: `ci.yml` (test, docker build, publish `:dev`) and `release.yml` (tags).
- `.worktrees/`: worktrunk (`wt`) worktrees, ignored by git (global gitignore) and skipped by `./...` because of the leading dot.

## Development Commands

There's no Makefile. These mirror the CI `test` job; run all of them before opening a PR:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...   # config: staticcheck.conf
go test -race ./...
go build ./...                      # binary ./getterr is gitignored
docker build -t getterr .
docker buildx build --platform linux/amd64,linux/arm64 .   # multi-arch check
```

Run locally against an instance:

```sh
STATE_DIR=/tmp/getterr SONARR_URL=http://127.0.0.1:8989 SONARR_API_KEY=... SONARR_MISSING_PACE=1d go run .
```

## Code Conventions & Common Patterns

- **No dependencies:** stdlib only, so there's no `go.sum`. Keep it that way unless a dependency clearly pays for itself. If you add one, also add a `gomod` entry to `.github/dependabot.yml`.
- **Single file:** everything stays in the root `package main`. The Dockerfile copies only `go.mod *.go`, so subpackages or a `go.sum` need that `COPY` line changed.
- **Request body:** JSON via `encoding/json` in `api`.
- **Logging:** stdlib `log` to stderr, formatted `"<app> <Command>: <message>"` (e.g. `connected to Sonarr <v> at <url>`, `cannot reach <url>, retrying in 5m0s: <err>`, `queued`, `queued MoviesSearch for 5 items`, `nothing to search`, `next run at <RFC3339>`, `failed, retrying in 5m0s: <err>`, `disabled (X unset)`). No levels, no structured logging.
- **Errors:** return plain `fmt.Errorf` errors. Use `log.Fatalf` only for startup errors; at runtime, log and retry.

## Important Files

- **`main.go`:** `jobs`, `batches`, `units`, `statusError`, `parsePace`, `api`, `trigger`, `oldestSearched`, `bazarrBatch`, `connect`, `main`, `run`, `retryDelay`.
- **`main_test.go`:** `TestParsePace`, `TestOldestSearched` (an `httptest` stub that sorts nulls like SQLite or Postgres), `TestBazarrBatch` (cursor rotation and wrap-around).
- **`Dockerfile`:**
  - The build stage runs on `--platform=$BUILDPLATFORM` and cross-compiles (`GOOS/GOARCH/GOARM=${TARGETVARIANT#v}`, `CGO_ENABLED=0`), so there's no QEMU.
  - The runtime is `scratch`: CA bundle (needed for HTTPS arr URLs), `/getterr`, and `/data` owned by `65532:65532`, run as `USER 65532:65532` with `VOLUME /data`.
  - There's no shell, so `docker exec` debugging and shell healthchecks are impossible. Bind-mounted state dirs must be writable by the container user (`chown 65532:65532`, or whatever `user:` compose sets).
  - There's no tzdata; log times use the host's zone only when `/etc/localtime` is mounted read-only.
- **`.github/workflows/ci.yml`:**
  - Runs on PRs and pushes to `main`.
  - The `publish` job runs only on push to `main`. It pushes `ghcr.io/<owner>/getterr:dev`, plus `:pr-<N>` when `gh api repos/$REPO/commits/$SHA/pulls` finds the PR.
  - Builds `linux/amd64,linux/arm64`, with OCI labels from the GitHub context and `actions/attest`.
- **`.github/workflows/release.yml`:**
  - `v*` tags push `:<version>` and `:latest` and run `gh release create --generate-notes`.
  - It doesn't re-run tests; tag only commits that already passed CI.
- **`.github/dependabot.yml`:** `docker` (golang build base) and `github-actions`, both monthly, grouped, 7-day cooldown.

## Runtime/Tooling Preferences

- **Go:** the version in `go.mod` (1.27) must match the `golang:1.27-alpine` build base. CI uses `go-version-file: go.mod`.
- **Platforms:**
  - Publish both `linux/amd64` and `linux/arm64`. The production host is x86_64 and pulls `:dev`, so never drop amd64.
  - `unknown/unknown` entries in the manifest list (and on the ghcr package page) are buildx provenance attestations, not platforms.
- **GitHub Actions:**
  - Pin to full commit SHAs with a `# vX.Y.Z` comment; resolve with `git ls-remote <url> 'refs/tags/<tag>^{}'`.
  - Use `persist-credentials: false` on checkout.
  - Put custom workflow logic in bash `run:` steps using `gh` (like the `meta` steps), not `actions/github-script`.
  - Lint workflow edits with `nix run nixpkgs#actionlint -- .github/workflows/*.yml`.
- **Git workflow:**
  - Work on a branch in a `wt` worktree (`wt switch --create <branch> --base main`) and open a PR to `main`. Never commit directly to `main`.
  - Commits are signed.
  - Docs and metadata use the `pentago` handle, never a real name or email.

## Testing & QA

- **Framework:** stdlib `testing` only. `main_test.go` has `TestParsePace` with inline map/slice tables (valid cases, then invalid cases including overflow) and `t.Errorf("parsePace(%q) = ...")`, `TestOldestSearched` and `TestBazarrBatch`.
- **Coverage gaps:** `api` (201/204/200 check, no redirects), `trigger`, `connect` (fatal vs retried), `run` (scheduling, state) and env wiring have no tests.
  - When changing them, verify with a throwaway `net/http/httptest` server or a local Python stub.
  - Check that 201 → `queued` plus a state file is written, 302/200/401 → `failed, retrying`, and a restart honours the saved state.
- **Container smoke test:** `docker run --rm getterr` with no env must exit 1 with `no *_PACE variable set, nothing to do`.
