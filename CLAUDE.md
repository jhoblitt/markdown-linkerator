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
3. **The cache is definitive-only, and namespaced by request policy.** Only
   `StateAlive` and stable client errors (any `4xx` **except** the transient
   `408/425/429`) are cached (`cache.Definitive`/`stableClientError`). Never
   cache `429`, `5xx`, timeouts, transport errors, `StateError`, `StateIgnored`,
   or any result with `Err != nil` — caching a transient failure as "dead"
   poisons the next run. Never re-`Put` a `FromCache` result (it would refresh
   the TTL without a real check). The on-disk cache file carries a
   `CacheFingerprint()` of the request policy (alive codes, custom headers,
   user-agent, base URL, redirect cap, and a non-reversible **digest of the
   GitHub token** — not merely its presence, so results from one token are never
   reused under a different one); a fingerprint mismatch on load discards the
   cache so results are never reused across incompatible policies.
4. **Dedup keeps the channel graph acyclic.** Dedup is a mutex-guarded seen-map
   with per-URL state, not a goroutine in a channel cycle. A completing check
   fans its result out to every occurrence of the URL. Exactly-once emit is
   guaranteed by the per-URL mutex making "complete + snapshot occurrences"
   atomic against "append occurrence".
5. **Two retry classes, counted and backed-off separately.** `retryablehttp`'s
   single `RetryMax` is `max(MaxRetries, ConnectRetries)`, but `checkRetry`
   enforces each class independently via per-class counters in `retryState`, so
   the budgets never bleed into each other (`retryCount=0` disables 429/503
   retries even with `connectRetries>0`). *Rate-limit* retries (429/503) are
   bounded by `MaxRetries`, honor `Retry-After` (integer-seconds *and* HTTP-date,
   **never floored** to `RetryWaitMin`) capped at `BackoffMax` (an hour-long
   Retry-After makes us give up, not park a slot), arm the host `notBefore`
   cooldown **immediately on observation** (`ArmCooldown`, so queued same-host
   jobs stop feeding a throttling host during its window) and apply the AIMD rate
   cut once post-check. *Connection* failures (refused/reset/DNS) use a separate
   bounded, fast path (`ConnectRetries`, default 3, ~0.5/1/2s) — never the long
   rate-limit backoff, so a dead socket fails in seconds. A per-request *timeout*
   (`--timeout`) is retried up to `MaxRetries` (its own budget, distinct from
   ConnectRetries), each attempt bounded by `--timeout`, and a HEAD timeout stays
   HEAD-only (no GET fallback that would time out again), so a hanging host costs
   at most `(retryCount+1) × timeout` — tune with `--retry-count` (`0` disables). On HEAD, a final 429/503
   is authoritative (no GET fallback that would double the load); the GET fallback
   drains at most `maxDrainBytes` (8 KiB) so a HEAD-rejecting large-body endpoint
   cannot make us download it in full. The client uses
   `retryablehttp.PassthroughErrorHandler` so an exhausted 429 returns its
   response (not the default nil+error, which discards the status).
6. **Interrupted or unreadable runs never exit green.** A canceled check is
   `StateError` (not the `StateAlive` zero value) and is never cached; the engine
   surfaces `ctx.Err()` (SIGINT / `--max-time`) as a non-zero exit. Unreadable or
   unparseable input files increment `Pipeline.SourceErrors()` and fail the run
   regardless of `--fail-on-error` (documentation must not go silently unchecked).
7. **Configured headers never leak off-origin.** `ruleMatches` requires an exact
   scheme+host origin (with a path boundary), not a raw prefix; and
   `checkRedirect` strips custom `httpHeaders` on any cross-origin redirect (Go
   only strips `Authorization`/`Cookie`). The GitHub token is host-scoped to
   GitHub hosts and yields to a user `Authorization` header.
8. **The engine is side-effect-free.** `engine.Run` does no flag parsing, no
   `os.Exit`, no global mutable state (except `internal/version`, set once by
   ldflags). The CLI, unit tests, and e2e harness all drive the same `Run`.
9. **Keys are normalized once.** Dedup and cache share `model.NormalizeKey`
   (lowercase scheme+host, strip default ports, drop fragment). Fragments are
   reported per-occurrence but never part of the network/cache key.
10. **GitHub auth is automatic in CI.** The token defaults to `$GITHUB_TOKEN`
    (injected by every Actions job) and is sent as `Authorization: Bearer` to
    GitHub hosts only, so a repo full of `github.com` links is not throttled by
    the 60/hr unauthenticated limit — the usual cause of a stalled run.
11. **Anchor matching is dual-case.** A `#fragment` matches an anchor by its
    verbatim form (case-sensitive HTML `id`/`name`, e.g. generated CRD
    `ceph.rook.io/v1.CephCluster`) **and** its lowercased form (GitHub heading
    slugs). Cross-file `file.md#anchor` links are validated against the target
    file's anchors when `CheckFragments` is on (default).

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
`urlWorkers=10`, `parseWorkers=10`, `retryCount=4`, `connectRetries=3`, cache
TTL `24h`, `aliveStatusCodes=[200]`, `checkExternal`/`checkFragments` on.

## Progress / observability

A paced run over a large tree takes minutes, and results are buffered for the
final sorted report — so without live output it looks hung. `internal/report`
writes progress to **stderr** (never stdout, which stays the clean report):

- A **time-based heartbeat** (`Collector.StartProgress`, a 10s ticker) fires even
  when every worker is stalled in backoff and no results are arriving — so the
  run is never silent > ~15s. It prints a counter line plus **one line per
  in-flight check** (URL, age, and why — e.g. "HTTP 429, retrying in 30s"),
  fed by `NetEnqueue`/`NetStart`/`NetStatus`/`NetComplete` and the checker's
  `OnRetry` hook.
- `--verbose` streams each link as it completes; a URL is checked once per run,
  so later occurrences are marked `(reused)` and on-disk cache hits `(cached)`.
- `--format` is `text` (default), `json`, or `yaml` (json/yaml share one wire
  schema and suppress live output).

## Lessons learned (field-tested against rook/rook)

Non-obvious behaviors discovered running the tool against a real large docs
tree, each now guarded by a test — do not regress them:

- **GitHub's 60/hr unauthenticated limit is the #1 cause of "hung" CI runs.**
  A repo full of `github.com` links gets 429s and sits in backoff. Auto-using
  `$GITHUB_TOKEN` is the fix (invariant 10), not more retries.
- **Connection failures must fast-fail.** Routing a refused socket through the
  rate-limit backoff makes a single dead localhost link take minutes. Connection
  failures get their own bounded fast path (invariant 5).
- **`go-retryablehttp` discards the response on exhausted retries by default**
  (closes the body, returns `nil`+error) — set `PassthroughErrorHandler` or a
  final 429 looks like a transport error (status 0).
- **Retry-After must not be floored** to `RetryWaitMin`; a `Retry-After: 1`
  inflated to a 30s floor makes every 429 needlessly slow.
- **HTML `id`/`name` anchors are case-sensitive; GitHub heading slugs are
  lowercased.** Generated CRD docs link to `#ceph.rook.io/v1.CephCluster`
  (verbatim id); lowercasing the fragment breaks the match. Match both forms.
- **Custom headers leak across cross-origin redirects** — Go only strips
  `Authorization`/`Cookie`. Strip configured headers ourselves (invariant 7).
- **markdown-link-check's `httpHeaders` prefix match is a secret-leak vector**
  (`api.github.com` prefix-matches `api.github.com.evil.example`). We require an
  exact origin.
- **Local anchor/file links are checked in-memory, never network-queued;** a
  synthetic `Status: 200` means "anchor/file found". Users filter noisy
  generated cross-refs (e.g. `#ceph.rook.io/...`) with `ignorePatterns`.
- **Reuse ≠ repeat.** Dedup collapses a URL to one request; the per-occurrence
  fan-out made it *look* re-checked until occurrences were marked `(reused)`.

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
