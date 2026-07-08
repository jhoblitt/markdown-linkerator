// Package cache provides an optional on-disk cache of URL check results with a
// TTL, so repeated CI runs skip re-checking recently-verified URLs. It is
// loaded once at start, TTL-checked in memory, and written atomically at exit.
//
// Only definitive outcomes are cached (see Definitive): a live target, or a
// target that is dead for a stable client-error (4xx) reason. Transient failures
// — 408/425/429, any 5xx, timeouts, transport errors — are never persisted, so
// one flaky run cannot poison the next by pinning a URL as "dead".
package cache

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

// schemaVersion tags the on-disk envelope. A file whose schema differs is
// ignored (the run starts cold) rather than misread as the current format.
const schemaVersion = 1

// Entry is one cached check outcome, stored under its normalized URL key.
type Entry struct {
	StatusCode int         `json:"statusCode"`
	State      model.State `json:"state"`
	CheckedAt  time.Time   `json:"checkedAt"`
}

// file is the on-disk envelope: a schema version, a fingerprint of the
// cache-relevant config, and the key->Entry map.
type file struct {
	Schema      int              `json:"schema"`
	Fingerprint string           `json:"fingerprint,omitempty"`
	Entries     map[string]Entry `json:"entries"`
}

// Cache is a TTL-checked, JSON-file-backed store of definitive check results.
// A disabled cache is a no-op: Get always misses, Put stores nothing, Save
// writes nothing. Construct one with New; the zero value is not usable.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]Entry

	// Set once by New and never mutated, so they are read without the lock.
	path        string
	ttl         time.Duration
	enabled     bool
	fingerprint string
}

// New builds a cache. When enabled it loads path if present; a missing,
// unreadable, corrupt, or foreign-schema file is a warning that leaves the
// cache empty, never an error — the cache is an optimization and must not fail
// a run. When disabled it returns a no-op cache.
func New(path string, ttl time.Duration, enabled bool, fingerprint string) (*Cache, error) {
	c := &Cache{
		entries:     map[string]Entry{},
		path:        path,
		ttl:         ttl,
		enabled:     enabled,
		fingerprint: fingerprint,
	}
	if !enabled {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, nil // no prior cache (or unreadable): start cold
	}
	var f file
	if json.Unmarshal(b, &f) != nil || f.Schema != schemaVersion || f.Fingerprint != fingerprint {
		// corrupt, foreign schema, or a different request policy (headers, alive
		// codes, user-agent): start cold so stale results are not reused.
		return c, nil
	}
	if f.Entries != nil {
		c.entries = f.Entries
	}
	return c, nil
}

// Definitive reports whether r is stable enough to cache. Only two outcomes
// qualify: a live target, or a target dead for a hard-dead reason (400 Bad
// Request, 404 Not Found, 410 Gone). Everything else — 429, any 5xx, timeouts,
// transport errors, StateError, StateIgnored — is transient or request-specific
// and must be re-checked next run. StateAlive is cacheable regardless of code.
func Definitive(r model.Result) bool {
	if r.Err != nil {
		return false // a result carrying an error (e.g. cancellation) is never definitive
	}
	switch r.State {
	case model.StateAlive:
		return true
	case model.StateDead:
		return stableClientError(r.StatusCode)
	}
	return false
}

// stableClientError reports whether a 4xx status is a stable client error worth
// caching. The transient 4xx — 408 Request Timeout, 425 Too Early, 429 Too Many
// Requests — are excluded so a rate-limited run cannot poison the next. 5xx and
// transport errors (code 0) are server-side/transient and never cached.
func stableClientError(code int) bool {
	if code < 400 || code >= 500 {
		return false
	}
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	default:
		return true
	}
}

// Get returns a fresh entry (checked less than ttl ago) for key. ok is false on
// a miss, an expired entry, or a disabled cache.
func (c *Cache) Get(key string) (Entry, bool) {
	if !c.enabled {
		return Entry{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.CheckedAt) >= c.ttl {
		return Entry{}, false
	}
	return e, true
}

// Put records r under key when Definitive(r); otherwise it does nothing. The
// stored CheckedAt is the current time.
func (c *Cache) Put(key string, r model.Result) {
	if !c.enabled || !Definitive(r) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = Entry{
		StatusCode: r.StatusCode,
		State:      r.State,
		CheckedAt:  time.Now(),
	}
}

// Save atomically writes the cache to its path via a temp file in the same
// directory plus os.Rename. It is a no-op for a disabled cache. The map is
// snapshotted under the read lock and marshaled outside it, so concurrent
// Get/Put calls are not blocked on I/O.
func (c *Cache) Save() error {
	if !c.enabled {
		return nil
	}

	c.mu.RLock()
	snapshot := make(map[string]Entry, len(c.entries))
	for k, v := range c.entries {
		snapshot[k] = v
	}
	c.mu.RUnlock()

	b, err := json.MarshalIndent(file{Schema: schemaVersion, Fingerprint: c.fingerprint, Entries: snapshot}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cache: %w", err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(c.path), filepath.Base(c.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp cache file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed away; cleanup on any error path

	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp cache file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp cache file: %w", err)
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		return fmt.Errorf("rename temp cache file: %w", err)
	}
	return nil
}

// Len reports the number of entries held, counting stale ones not yet evicted.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Enabled reports whether the cache reads and writes, or is a no-op.
func (c *Cache) Enabled() bool { return c.enabled }
