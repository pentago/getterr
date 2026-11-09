# Repository Guidelines

## Project Overview

getterr is a tiny Go service that replaces Huntarr's paced backlog searches. On a schedule set per search, it POSTs Sonarr/Radarr "Wanted" commands (missing and cutoff-unmet) to `/api/v3/command`. It ships as a scratch Docker image published to `ghcr.io/pentago/getterr`. `README.md` is for end users and stays in plain, non-technical English; technical detail belongs here.

## Architecture & Data Flow

Everything lives in `main.go` (`package main`, stdlib only).

1. `main` reads `STATE_DIR` (default `/data`) and builds one shared `http.Client`. It then walks the `jobs` table.
2. `jobs` has one row per search: `{app, command, paceVar}`.

   | Pace env var | Command |
   |---|---|
   | `SONARR_MISSING_PACE` | `MissingEpisodeSearch` |
   | `SONARR_UNMET_PACE` | `CutoffUnmetEpisodeSearch` |
   | `RADARR_MISSING_PACE` | `MissingMoviesSearch` |
   | `RADARR_UNMET_PACE` | `CutoffUnmetMoviesSearch` |

   To add a search, add a row. `<APP>_URL` and `<APP>_API_KEY` are derived from `app`.
3. Each enabled job gets its own goroutine running `run`, and `main` blocks with `select {}`. Jobs share no mutable state; each owns its own state file.
4. `run` loops:
   - Sleep until `last + pace`, then call `trigger`.
   - On success, set `last = now` and write it to the job's state file.
   - On failure, log, sleep `retryDelay` (5m) and retry. `last` is not advanced, so the pace timer only starts after a success.
5. `trigger` sends `POST <url>/api/v3/command` with `{"name":"<command>"}` and the `X-Api-Key` header.

### Non-obvious invariants

Don't regress these without a reason; there are no automated tests for them.

- **Only `201 Created` counts as success.** That's what Sonarr/Radarr return for a queued command (`StartCommand` returns `Created`). A 200 or 3xx usually means an auth proxy's login page, and nothing was queued.
- **Redirects are never followed** (`CheckRedirect` returns `http.ErrUseLastResponse`). Go would forward `X-Api-Key` to the redirect target, and a POST that gets a 302 turns into a GET.
- **State persistence:** one file per job in `$STATE_DIR`, holding a single RFC3339 timestamp. This stops container restarts from re-running searches early or skipping long paces.
  - A missing or unparseable file means "run now".
  - A failed write is only logged; that job then runs on the in-memory time until the next restart.
  - File names are part of deployed state: renaming them makes every existing deployment run each search once more, unless the files are renamed on the host before the new image starts.
- **Pace format (`parsePace`):** `<N><unit>`, where `N` is a positive integer.
  - Units are case-sensitive: `h`, `d`, `w`, and `m` (30 **days**, not minutes).
  - Overflow is rejected (`time.Duration(n) > math.MaxInt64/unit`); otherwise an overflowed pace makes `run` hot-loop.
- **Config errors are fatal at startup:** a bad pace, a pace set without its URL or key, or no pace set at all. An unset pace just disables that job. Runtime errors are never fatal.
- **No signal handling:** Go's default SIGTERM exit works even as PID 1 in scratch (`docker stop` takes about 0.1s). The worst case, a kill between the 201 and the state write, re-runs one search.
- **API version:** `/api/v3` works on Sonarr v4 and v5 (v5 keeps `Sonarr.Api.V3` alongside `V5`) and on Radarr v6/v7 (v3 only). Switch Sonarr to `/api/v5` only if v3 is removed.

## Key Directories

- `/`: all source (`main.go`, `main_test.go`), `go.mod`, `Dockerfile`, `README.md`, `AGENTS.md`.
- `.github/workflows/`: `ci.yml` (test, docker build, publish `:dev`) and `release.yml` (tags).
- `.worktrees/`: worktrunk (`wt`) worktrees, ignored by git (global gitignore) and skipped by `./...` because of the leading dot.

## Development Commands

There's no Makefile. These mirror the CI `test` job; run all of them before opening a PR:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
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
- **Request body:** built by string concatenation, which is safe only because command names are constants from `jobs`. Use `encoding/json` if anything user-supplied ever goes in.
- **Logging:** stdlib `log` to stderr, formatted `"<app> <Command>: <message>"` (e.g. `queued`, `next run at <RFC3339>`, `failed, retrying in 5m0s: <err>`, `disabled (X unset)`). No levels, no structured logging.
- **Errors:** return plain `fmt.Errorf` errors. Use `log.Fatalf` only for startup config errors; at runtime, log and retry.

## Important Files

- **`main.go`:** `jobs`, `units`, `parsePace`, `trigger`, `main`, `run`, `retryDelay`.
- **`main_test.go`:** `TestParsePace`.
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

- **Framework:** stdlib `testing` only. `main_test.go` has `TestParsePace` with inline map/slice tables (valid cases, then invalid cases including overflow) and `t.Errorf("parsePace(%q) = ...")`.
- **Coverage gaps:** `trigger` (201 check, no redirects), `run` (scheduling, state) and env wiring have no tests.
  - When changing them, verify with a throwaway `net/http/httptest` server or a local Python stub.
  - Check that 201 → `queued` plus a state file is written, 302/200/401 → `failed, retrying`, and a restart honours the saved state.
- **Container smoke test:** `docker run --rm getterr` with no env must exit 1 with `no *_PACE variable set, nothing to do`.
