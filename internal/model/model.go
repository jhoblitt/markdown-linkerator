// Package model holds the leaf data types shared across the linkerator
// pipeline. It depends only on the standard library so every other package can
// import it without creating a cycle.
package model

import (
	"net/url"
	"strings"
	"time"
)

// Kind classifies how a link target is checked.
type Kind uint8

const (
	// KindHTTP is an http or https URL checked over the network.
	KindHTTP Kind = iota
	// KindFileRel is a relative or file:// link checked against the filesystem.
	KindFileRel
	// KindHashLocal is a #fragment resolved against the source file's headings.
	KindHashLocal
	// KindMailto is a mailto: address.
	KindMailto
)

func (k Kind) String() string {
	switch k {
	case KindHTTP:
		return "http"
	case KindFileRel:
		return "file"
	case KindHashLocal:
		return "hash"
	case KindMailto:
		return "mailto"
	default:
		return "unknown"
	}
}

// State is the outcome classification of a checked target. Only StateDead (and
// optionally StateError) drives a non-zero exit code.
type State uint8

const (
	// StateAlive means the target resolved successfully.
	StateAlive State = iota
	// StateDead means the target did not resolve (bad status, missing file,
	// unresolved anchor). This is the only state that fails a run by default.
	StateDead
	// StateIgnored means an ignorePattern matched; no request was made.
	StateIgnored
	// StateError means the check could not be completed (unsupported protocol,
	// transport error with no HTTP response).
	StateError
)

func (s State) String() string {
	switch s {
	case StateAlive:
		return "alive"
	case StateDead:
		return "dead"
	case StateIgnored:
		return "ignored"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// Glyph returns the single-character status marker used in text output,
// mirroring markdown-link-check (✓ alive, ✖ dead, / ignored, ⚠ error).
func (s State) Glyph() string {
	switch s {
	case StateAlive:
		return "✓"
	case StateDead:
		return "✖"
	case StateIgnored:
		return "/"
	case StateError:
		return "⚠"
	default:
		return "?"
	}
}

// Target is a single link occurrence discovered in a markdown source.
type Target struct {
	Raw        string // link text exactly as written in the source
	URL        string // after replacement/base-url/env expansion
	Kind       Kind
	SourceFile string // path of the markdown file the link was found in
	Line       int    // 1-based line number of the occurrence
	Fragment   string // #fragment, retained for reporting only
}

// Result is the outcome of checking a Target. A dead link is data, never a Go
// error; Err is reserved for fatal conditions (context cancellation, I/O).
type Result struct {
	Target     Target
	State      State
	StatusCode int           // HTTP code, or synthetic (200 fs-exists, 400 fs-missing, anchor)
	Host       string        // hostname for http targets, for per-host accounting
	Retries    int           // number of retries the checker performed
	Saw429     bool          // a 429 was observed (drives the host AIMD penalty)
	RetryAfter time.Duration // observed Retry-After, if any, for the host cooldown
	FromCache  bool
	Detail     string // human-readable extra context (error text, redirect chain)
	Err        error  // fatal only; nil for an ordinary dead link
}

// CheckJob is the unit dispatched from dedup to the host pacers and executor.
// It carries the shared per-URL state so the completing worker can fan the
// result out to every occurrence of the URL.
type CheckJob struct {
	Key    string // normalized dedup/cache key
	Host   string
	Sample Target // a representative occurrence used to perform the check
}

// HostStat is the per-host accounting reported at the end of a run. It lives in
// model so the ratelimit and report packages share it without a dependency edge.
type HostStat struct {
	Host        string
	Requests    int64
	Retries     int64
	N429        int64
	ObservedRPS float64
}

// NormalizeKey returns the canonical dedup/cache key for a URL: lowercased
// scheme and host, default ports stripped, fragment dropped, path and query
// preserved. Non-URL strings are returned trimmed and unchanged so mailto and
// other schemes still dedup exactly.
func NormalizeKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" {
		return strings.TrimSpace(raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if h := stripDefaultPort(u.Scheme, u.Host); h != "" {
		u.Host = h
	}
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

func stripDefaultPort(scheme, host string) string {
	switch {
	case scheme == "http" && strings.HasSuffix(host, ":80"):
		return strings.TrimSuffix(host, ":80")
	case scheme == "https" && strings.HasSuffix(host, ":443"):
		return strings.TrimSuffix(host, ":443")
	default:
		return host
	}
}
