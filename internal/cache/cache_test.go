package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

func res(state model.State, code int) model.Result {
	return model.Result{State: state, StatusCode: code}
}

// writeCacheFile drops a fixture cache file at path, using the real on-disk
// envelope so loads exercise the actual format.
func writeCacheFile(t *testing.T, path string, f file) {
	t.Helper()
	b, err := json.Marshal(f)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0o644))
}

// TestDefinitive pins the caching policy: alive always, dead only for the hard-
// dead codes, and nothing else. This is the rule that keeps a transient 429 or
// 5xx from being persisted as "dead".
func TestDefinitive(t *testing.T) {
	cases := []struct {
		name string
		r    model.Result
		want bool
	}{
		{"alive-200", res(model.StateAlive, 200), true},
		{"alive-206", res(model.StateAlive, 206), true},
		{"alive-no-code", res(model.StateAlive, 0), true},
		{"dead-400", res(model.StateDead, 400), true},
		{"dead-404", res(model.StateDead, 404), true},
		{"dead-410", res(model.StateDead, 410), true},
		{"dead-401-transient-auth", res(model.StateDead, 401), false},
		{"dead-403-transient-auth", res(model.StateDead, 403), false},
		{"dead-429-rate-limited", res(model.StateDead, 429), false},
		{"dead-500", res(model.StateDead, 500), false},
		{"dead-503", res(model.StateDead, 503), false},
		{"dead-no-code-timeout", res(model.StateDead, 0), false},
		{"error-with-code", res(model.StateError, 500), false},
		{"error-no-code", res(model.StateError, 0), false},
		{"ignored", res(model.StateIgnored, 0), false},
		{"ignored-with-code", res(model.StateIgnored, 200), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Definitive(tc.r))
		})
	}
}

// TestPutDefinitiveOnly proves Put enforces the policy end-to-end: only
// definitive results become retrievable entries.
func TestPutDefinitiveOnly(t *testing.T) {
	cases := []struct {
		name   string
		r      model.Result
		cached bool
	}{
		{"alive", res(model.StateAlive, 200), true},
		{"dead-404", res(model.StateDead, 404), true},
		{"dead-410", res(model.StateDead, 410), true},
		{"dead-400", res(model.StateDead, 400), true},
		{"dead-429", res(model.StateDead, 429), false},
		{"dead-500", res(model.StateDead, 500), false},
		{"error", res(model.StateError, 0), false},
		{"ignored", res(model.StateIgnored, 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(filepath.Join(t.TempDir(), "c.json"), time.Hour, true)
			require.NoError(t, err)

			c.Put("https://x/", tc.r)

			e, ok := c.Get("https://x/")
			assert.Equal(t, tc.cached, ok)
			if tc.cached {
				assert.Equal(t, 1, c.Len())
				assert.Equal(t, tc.r.State, e.State)
				assert.Equal(t, tc.r.StatusCode, e.StatusCode)
			} else {
				assert.Equal(t, 0, c.Len())
			}
		})
	}
}

// TestGetFreshVsExpired covers TTL freshness. The fresh entry is written by Put
// (real clock); the expired one is injected via a past CheckedAt in a loaded
// file, since Put always stamps time.Now().
func TestGetFreshVsExpired(t *testing.T) {
	t.Run("put-then-get-is-fresh", func(t *testing.T) {
		c, err := New(filepath.Join(t.TempDir(), "c.json"), time.Hour, true)
		require.NoError(t, err)
		c.Put("k", res(model.StateAlive, 200))

		e, ok := c.Get("k")
		require.True(t, ok)
		assert.Equal(t, 200, e.StatusCode)
		assert.Equal(t, model.StateAlive, e.State)
		assert.WithinDuration(t, time.Now(), e.CheckedAt, time.Minute)
	})

	t.Run("loaded-entries-filtered-by-ttl", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "c.json")
		writeCacheFile(t, p, file{Schema: schemaVersion, Entries: map[string]Entry{
			"https://stale/": {StatusCode: 200, State: model.StateAlive, CheckedAt: time.Now().Add(-2 * time.Hour)},
			"https://fresh/": {StatusCode: 404, State: model.StateDead, CheckedAt: time.Now().Add(-1 * time.Minute)},
		}})

		c, err := New(p, time.Hour, true)
		require.NoError(t, err)
		assert.Equal(t, 2, c.Len()) // both loaded; Get filters, Len does not

		_, ok := c.Get("https://stale/")
		assert.False(t, ok, "entry older than ttl must miss")

		e, ok := c.Get("https://fresh/")
		require.True(t, ok, "entry younger than ttl must hit")
		assert.Equal(t, 404, e.StatusCode)
		assert.Equal(t, model.StateDead, e.State)
	})

	t.Run("zero-ttl-always-misses", func(t *testing.T) {
		c, err := New(filepath.Join(t.TempDir(), "c.json"), 0, true)
		require.NoError(t, err)
		c.Put("k", res(model.StateAlive, 200))

		_, ok := c.Get("k")
		assert.False(t, ok)
	})
}

// TestSaveLoadRoundTrip writes populated cache to disk and reloads it, checking
// the on-disk envelope, entry fidelity, and that no temp files are left behind.
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cache.json")

	c1, err := New(p, time.Hour, true)
	require.NoError(t, err)
	c1.Put("https://a/", res(model.StateAlive, 200))
	c1.Put("https://b/", res(model.StateDead, 404))
	c1.Put("https://transient/", res(model.StateDead, 500)) // dropped by policy
	require.NoError(t, c1.Save())
	assert.Equal(t, 2, c1.Len())

	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	var f file
	require.NoError(t, json.Unmarshal(raw, &f))
	assert.Equal(t, schemaVersion, f.Schema)
	assert.Len(t, f.Entries, 2)

	tmps, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, tmps, "temp file must be renamed/cleaned up")

	c2, err := New(p, time.Hour, true)
	require.NoError(t, err)
	assert.Equal(t, 2, c2.Len())

	ea, ok := c2.Get("https://a/")
	require.True(t, ok)
	assert.Equal(t, model.StateAlive, ea.State)
	assert.Equal(t, 200, ea.StatusCode)

	eb, ok := c2.Get("https://b/")
	require.True(t, ok)
	assert.Equal(t, model.StateDead, eb.State)
	assert.Equal(t, 404, eb.StatusCode)

	_, ok = c2.Get("https://transient/")
	assert.False(t, ok)
}

// TestSaveOverwritesAtomically proves a second Save replaces the prior file
// rather than appending or corrupting it.
func TestSaveOverwritesAtomically(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache.json")
	c, err := New(p, time.Hour, true)
	require.NoError(t, err)

	c.Put("https://a/", res(model.StateAlive, 200))
	require.NoError(t, c.Save())

	c.Put("https://b/", res(model.StateAlive, 200))
	require.NoError(t, c.Save())

	reloaded, err := New(p, time.Hour, true)
	require.NoError(t, err)
	assert.Equal(t, 2, reloaded.Len())
}

// TestDisabledIsNoop proves a disabled cache never reads, stores, or writes.
func TestDisabledIsNoop(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cache.json")

	// A pre-existing file must be ignored by a disabled cache.
	writeCacheFile(t, p, file{Schema: schemaVersion, Entries: map[string]Entry{
		"https://pre/": {StatusCode: 200, State: model.StateAlive, CheckedAt: time.Now()},
	}})

	c, err := New(p, time.Hour, false)
	require.NoError(t, err)
	assert.False(t, c.Enabled())
	assert.Equal(t, 0, c.Len())

	_, ok := c.Get("https://pre/")
	assert.False(t, ok)

	c.Put("k", res(model.StateAlive, 200))
	_, ok = c.Get("k")
	assert.False(t, ok)
	assert.Equal(t, 0, c.Len())

	// Save must not touch the (still original) file content.
	require.NoError(t, c.Save())
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	var f file
	require.NoError(t, json.Unmarshal(raw, &f))
	assert.Len(t, f.Entries, 1) // untouched pre-existing entry
}

// TestDisabledSaveWritesNothing checks Save creates no file when disabled and
// no file existed.
func TestDisabledSaveWritesNothing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache.json")
	c, err := New(p, time.Hour, false)
	require.NoError(t, err)
	require.NoError(t, c.Save())

	_, statErr := os.Stat(p)
	assert.True(t, os.IsNotExist(statErr))
}

// TestLoadIsLenient proves every bad-file condition loads an empty cache with a
// nil error, so a corrupt cache degrades to a cold run rather than failing it.
func TestLoadIsLenient(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		c, err := New(filepath.Join(t.TempDir(), "nope.json"), time.Hour, true)
		require.NoError(t, err)
		assert.Equal(t, 0, c.Len())
	})

	badFiles := map[string]string{
		"garbage":        "{not valid json",
		"empty":          "",
		"json-array":     "[1,2,3]",
		"json-scalar":    "42",
		"wrong-schema":   `{"schema":99,"entries":{"https://x/":{"statusCode":200,"state":0,"checkedAt":"2020-01-01T00:00:00Z"}}}`,
		"missing-schema": `{"entries":{"https://x/":{"statusCode":200,"state":0,"checkedAt":"2020-01-01T00:00:00Z"}}}`,
	}
	for name, body := range badFiles {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "cache.json")
			require.NoError(t, os.WriteFile(p, []byte(body), 0o644))

			c, err := New(p, time.Hour, true)
			require.NoError(t, err)
			assert.Equal(t, 0, c.Len())

			_, ok := c.Get("https://x/")
			assert.False(t, ok)
		})
	}
}

func TestEnabled(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache.json")
	en, err := New(p, time.Hour, true)
	require.NoError(t, err)
	assert.True(t, en.Enabled())

	dis, err := New(p, time.Hour, false)
	require.NoError(t, err)
	assert.False(t, dis.Enabled())
}

// TestConcurrentAccess exercises the RWMutex under the race detector: many
// goroutines Put/Get/Len while a Save snapshots concurrently.
func TestConcurrentAccess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache.json")
	c, err := New(p, time.Hour, true)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("https://h%d/", i%8)
			c.Put(key, res(model.StateAlive, 200))
			c.Get(key)
			c.Len()
			if i%16 == 0 {
				_ = c.Save()
			}
		}(i)
	}
	wg.Wait()

	require.NoError(t, c.Save())
	assert.LessOrEqual(t, c.Len(), 8)

	reloaded, err := New(p, time.Hour, true)
	require.NoError(t, err)
	assert.Equal(t, c.Len(), reloaded.Len())
}
