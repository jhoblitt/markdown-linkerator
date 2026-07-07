# CLAUDE.md — markdown-linkerator

Architecture, invariants, and house rules for this repository. Read this before
changing the engine. It is pedantic on purpose: the tool's entire value is
subtle concurrency correctness, so the load-bearing rules are spelled out.

## What this is

`markdown-linkerator` is a Go replacement for
[tcort/markdown-link-check](https://github.com/tcort/markdown-link-check). It
crawls a markdown tree, extracts links, and checks them over HTTP with
**per-host rate limiting, worker pools, an optional on-disk cache, and graceful
429 backoff**, so a large docs tree can be link-checked frequently in CI without
tripping rate limits (the failure that motivated it: a rook/rook run getting
HTTP 429s from `docs.ceph.com`).

Primary objectives, in order:
1. Never fail a run because of 429 / rate limiting.
2. Minimize wall-clock time without causing rate limiting.

## Module & package map

```
cmd/markdown-linkerator     thin main: cobra wiring, ldflags version, os.Exit(engine result)
internal/cli                flags, --version, config load+merge (defaults<file<env<flags), arg/stdin classification
internal/engine             Run(ctx, cfg, inputs) (*report.Summary, error) — the pure orchestrator façade
internal/model              LEAF types (Target, Result, Kind, State, CheckJob, HostStat) + NormalizeKey. No internal deps.
internal/config             Config (json tags = tcort keys) + Duration + Resolve(); parses JSON and YAML alike
internal/extract            goldmark walk + x/net/html; disable directives; GitHub-slug anchors; replacement/classify
internal/checker            HTTPChecker (retryablehttp, HEAD→GET, Retry-After backoff), file/hash/mailto, IsIgnored
internal/ratelimit          per-host token bucket, AIMD 429 penalty, cooldown, per-host stats
internal/cache              JSON on-disk result cache, TTL, definitive-only policy
internal/report             Collector: buffer, stable sort, text/json render, exit-code accounting
internal/testserver         httptest server mirroring markdown-link-check's fixture routes (test-only helper)
internal/version            build metadata for --version
```

**Allowed dependency direction:** `model` is imported by everything and imports
no internal package. Leaf packages (`config`, `extract`, `checker`, `ratelimit`,
`cache`, `report`) import only `model` (+ `config` where noted) — never
`engine` or `pipeline`. `engine` sits at the top and wires them together. No
import cycles. Everything is under `internal/`; there is no exported Go API
promise (the GitHub Action consumes the binary/image, not the source).

## Load-bearing invariants — do not break these

1. **Pacing is decoupled from concurrency.** Per-host rate limiting is done by
   cheap per-host *pacer goroutines* that block on the host's
   `x/time/rate` limiter. The global *executor pool* holds a slot only during a
   real HTTP request, never during a rate-wait. A worker must never call
   `limiter.Wait` while holding an executor slot — that reintroduces
   head-of-line collapse to the slowest host (the whole bug we exist to avoid).
2. **A dead link is data, not an error.** Checkers return a `model.Result` with
   `Err == nil` for an ordinary dead link. `Err` (and errgroup cancellation) is
   reserved for context cancellation and fatal I/O. One dead link must never
   tear down the pipeline; the collector gathers every result.
3. **The cache is definitive-only.** Only `StateAlive` and hard-dead
   (`400/404/410`) results are cached. Never cache `429`, `5xx`, timeouts,
   transport errors, `StateError`, or `StateIgnored` — caching a transient 429
   as "dead" poisons the next run. Also: never re-`Put` a `FromCache` result
   (it would refresh the TTL without a real check).
4. **Dedup keeps the channel graph acyclic.** Dedup is a mutex-guarded seen-map
   with per-URL state, not a goroutine in a channel cycle. A completing check
   fans its result out to every occurrence of the URL. Exactly-once emit is
   guaranteed by the per-URL mutex making "complete + snapshot occurrences"
   atomic against "append occurrence".
5. **429 backoff honors Retry-After (both forms), clamped.** The HTTP checker
   parses `Retry-After` as integer-seconds *and* HTTP-date, and clamps the wait
   to `BackoffMax`; a server asking for an hour makes us give up, not park a
   slot. A 429 also triggers the host AIMD penalty (halve the rate to a floor)
   and a `notBefore` cooldown.
6. **The engine is side-effect-free.** `engine.Run` does no flag parsing, no
   `os.Exit`, no global mutable state (except `internal/version`, set once by
   ldflags). The CLI, unit tests, and e2e harness all drive the same `Run`.
7. **Keys are normalized once.** Dedup and cache share `model.NormalizeKey`
   (lowercase scheme+host, strip default ports, drop fragment). Fragments are
   reported per-occurrence but never part of the network/cache key.

## Configuration & the mlc-compat contract

One `config.Config` struct whose `json` tags are the tcort
markdown-link-check keys, so a verbatim `.markdown-link-check.json` parses, and
so does a native `linkerator.yaml` (sigs.k8s.io/yaml routes YAML through JSON).
`Duration` accepts the npm `ms` formats (`"5s"`, `2000` = ms). Precedence is
`defaults < config file < LINKERATOR_* env < explicit CLI flags`, resolved in
`internal/cli`; `config.Merge` overlays only set fields.

**Stability promise:** the tcort JSON keys (`ignorePatterns`,
`replacementPatterns`, `httpHeaders`, `aliveStatusCodes`, `timeout`,
`retryOn429`, `retryCount`, `fallbackRetryDelay`, `ignoreDisable`,
`projectBaseUrl`) must keep parsing with their upstream meaning. Changing that
mapping is a breaking change requiring a major version bump.

Conservative shipped defaults: `perHostRPS=1`, `perHostBurst=2`,
`urlWorkers=10`, `parseWorkers=10`, cache TTL `24h`, `aliveStatusCodes=[200]`.

## Tests

- `go test ./...` — unit tests. No network: HTTP behavior is exercised against
  `internal/testserver` (httptest). Never add a unit test that hits the public
  internet.
- `go test -tags=e2e ./e2e/...` — end-to-end through the built engine/binary
  against `testserver`, asserting 429-backoff, per-host spacing, and cache hits.
- **Slug golden oracle:** `testdata/golden/hash-links.slugs.json` locks the
  GitHub-slug algorithm against `testdata/tree/hash-links.md`. Regenerate
  goldens with the package's `-update` flag; review the diff — a change here is
  a change in anchor-resolution behavior.
- testify: `require` for fatal preconditions (setup, "must not error"), `assert`
  for field checks.

## Release mechanics

Tag `vX.Y.Z` → `.github/workflows/release.yml` → GoReleaser: cross-compiled
static binaries (linux/darwin/windows × amd64/arm64, `CGO_ENABLED=0`),
`checksums.txt`, SPDX SBOMs, cosign keyless signatures, a GitHub Release, and a
multi-arch distroless OCI image (built by ko, no Dockerfile) pushed to
`ghcr.io/jhoblitt/markdown-linkerator` and cosign-signed. `--version` is
populated from `-ldflags -X internal/version.{Version,Commit,Date}`.

## GitHub Actions house rules

- **SHA-pin every `uses:`** — run `pinact run` (needs a token:
  `GITHUB_TOKEN=$(gh auth token) pinact run`); the `# vX.Y.Z` comments are
  Dependabot-readable and bumped via the `github-actions` Dependabot entry.
- Run `actionlint` (with `shellcheck` installed for inline `run:` scripts)
  before committing workflow changes.
- `setup-go` uses `go-version-file: go.mod` — the single source of the Go
  version. Least-privilege `permissions:` per workflow (top-level
  `contents: read`; jobs opt into more).

## How to add a new check type

1. Add a `Kind` to `internal/model` and classify it in `internal/extract`.
2. Implement the check in `internal/checker` returning a `model.Result`
   (dead = data, `Err` only for fatal). If it is offline (no network), the
   pipeline resolves it inline in the parse stage; if it is networked, it goes
   through dedup → pacer → executor like http.
3. Add fixtures/tests. If networked, add a route to `internal/testserver`.

## Do-Not list

- No CGO. The binary must stay statically linked (`CGO_ENABLED=0`).
- No network in unit tests. Use `internal/testserver`.
- No global mutable state (except `internal/version`).
- Don't call `limiter.Wait` from an executor worker (invariant 1).
- Don't cache non-definitive results (invariant 3).
- Don't break the tcort JSON key mapping without a major version bump.
- Don't add a dependency without a clear reason; prefer stdlib + `golang.org/x`.
