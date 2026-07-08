# markdown-linkerator

A fast, **429-safe** Markdown link checker for CI. It crawls a markdown tree,
extracts links, and checks them concurrently with **per-host rate limiting**,
**worker pools**, an **optional on-disk cache**, and **graceful 429 backoff** —
so you can lint links on every pull request across a large docs tree without
tripping rate limits.

It is a drop-in-compatible replacement for
[tcort/markdown-link-check](https://github.com/tcort/markdown-link-check): your
existing `.markdown-link-check.json` config keeps working.

## Why

`markdown-link-check` checks links with a fixed global concurrency and **no
per-host throttle**, so a docs tree that references one host many times (e.g.
`docs.ceph.com`) bursts requests at it and gets HTTP 429s. `markdown-linkerator`
paces requests **per host** at a conservative default rate, dedups repeated URLs
across files, retries 429s honoring `Retry-After`, and can cache results across
CI runs — turning those flaky failures into fast, green runs.

## Install

```sh
go install github.com/jhoblitt/markdown-linkerator/cmd/markdown-linkerator@latest
```

Or use the released binaries / the OCI image `ghcr.io/jhoblitt/markdown-linkerator`,
or the GitHub Action (below).

## Usage

```sh
# Check a directory tree (recurses for *.md), the conservative default: ~1 req/s per host.
markdown-linkerator docs/

# Multiple inputs: files, directories, or bare URLs; reads stdin when given none.
markdown-linkerator README.md docs/ https://example.com

# Reuse an existing markdown-link-check config.
markdown-linkerator -c .markdown-link-check.json docs/

# Tune throughput (still 429-safe): 4 req/s per host, 20 URL workers.
markdown-linkerator --rate 4 --workers 20 docs/

# Cache results for a day so reruns skip re-checking.
markdown-linkerator --cache --cache-ttl 24h docs/
```

Exit code is `1` when any link is **dead**, `0` otherwise (ignored/errored links
don't fail unless `--fail-on-error`); `2` when a run is interrupted or exceeds
`--max-time`.

GitHub throttles unauthenticated requests to 60/hr, which is the usual cause of
slow, retry-stalled runs on repos full of `github.com` links. The tool picks up
`$GITHUB_TOKEN` automatically (set in every GitHub Actions job) and authenticates
requests to GitHub hosts, lifting that limit — no config needed in CI.

Because requests are paced per host, a large tree can take minutes. Progress is
written to **stderr** so a run is never silently "hung": a throttled heartbeat
(printed at least every 10s, even while every worker is stalled in retry/backoff,
and showing the in-flight backlog), or a live per-link stream under `-v`. Pipe
stdout to capture just the final report. A URL is checked once per run; later
occurrences are marked `(reused)`, and on-disk cache hits `(cached)`.

### Flags

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-c, --config` | `LINKERATOR_CONFIG` | auto | config file (`.json` or `.yaml`) |
| `--workers` | `LINKERATOR_WORKERS` | `10` | concurrent URL-check workers |
| `--parse-workers` | `LINKERATOR_PARSE_WORKERS` | `10` | concurrent markdown-parse workers |
| `--rate` | `LINKERATOR_RATE` | `1` | per-host requests/second |
| `--burst` | `LINKERATOR_BURST` | `2` | per-host burst |
| `--timeout` | `LINKERATOR_TIMEOUT` | `10s` | per-request timeout |
| `--max-time` | `LINKERATOR_MAX_TIME` | `0` | maximum total run time (0 = no limit); on expiry the run stops and exits 2 |
| `--retry-on-429` | `LINKERATOR_RETRY_ON_429` | `true` | retry 429 honoring `Retry-After` |
| `--retry-count` | `LINKERATOR_RETRY_COUNT` | `4` | max retries per URL on 429/503 |
| `--connect-retries` | `LINKERATOR_CONNECT_RETRIES` | `3` | fast retries on a connection failure before giving up |
| `--retry-max-wait` | `LINKERATOR_RETRY_MAX_WAIT` | `2m` | cap on `Retry-After` wait |
| `-a, --alive` | `LINKERATOR_ALIVE_CODES` | `200` | extra alive HTTP codes (CSV) |
| `--check-external` | `LINKERATOR_CHECK_EXTERNAL` | `true` | check http(s) links (`false` = offline) |
| `--check-fragments` | `LINKERATOR_CHECK_FRAGMENTS` | `true` | validate cross-file `#anchors` (`false` = markdown-link-check parity) |
| `--mailto-check-mx` | `LINKERATOR_MAILTO_CHECK_MX` | `false` | live MX lookup for mailto (default: syntax only) |
| `--project-base-url` | `LINKERATOR_BASE_URL` | — | base for root-relative links (`{{BASEURL}}`) |
| `--github-token` | `LINKERATOR_GITHUB_TOKEN` | `$GITHUB_TOKEN` | auth token for GitHub hosts (avoids the 60/hr unauthenticated rate limit) |
| `--cache` | `LINKERATOR_CACHE` | `false` | enable the on-disk result cache |
| `--cache-path` | `LINKERATOR_CACHE_PATH` | `.linkerator-cache.json` | cache file |
| `--cache-ttl` | `LINKERATOR_CACHE_TTL` | `24h` | cache entry TTL |
| `--format` | `LINKERATOR_FORMAT` | `text` | `text`, `json`, or `yaml` |
| `-q, --quiet` / `-v, --verbose` | | | quiet = failures only; verbose = show status codes |
| `--fail-on-error` | `LINKERATOR_FAIL_ON_ERROR` | `false` | treat errored links as failures |
| `--version` | | | print version and exit |

Per-host overrides, ignore/replacement patterns, and custom headers live in the
config file.

## Configuration

Two dialects, one schema. A drop-in tcort JSON:

```json
{
  "ignorePatterns": [{ "pattern": "^https://internal\\.example\\.com" }],
  "aliveStatusCodes": [200, 206],
  "timeout": "5s",
  "retryOn429": true
}
```

Or a native `linkerator.yaml` with the extended keys:

```yaml
aliveStatusCodes: [200, 206]
timeout: 5s
perHostRPS: 1
perHostBurst: 2
hostOverrides:
  docs.ceph.com: { rps: 1, burst: 1 }
urlWorkers: 10
cache:
  enabled: true
  ttl: 24h
```

See `testdata/configs/` for complete examples.

## GitHub Action

Use [`jhoblitt/markdown-linkerator-action`](https://github.com/jhoblitt/markdown-linkerator-action),
which runs this tool and persists the URL cache across runs via `actions/cache`:

```yaml
- uses: jhoblitt/markdown-linkerator-action@v1
  with:
    paths: "docs/**/*.md"
    rate: "1"
    cache: "true"
```

## License

Apache-2.0. Test fixtures derived from markdown-link-check (ISC) are attributed
in `NOTICE` and `testdata/THIRD_PARTY.md`.
