//go:build e2e

// Package e2e drives the full engine against the deterministic test server,
// proving the behaviors that unit tests cannot: 429 backoff to alive, per-host
// pacing, and cross-run cache hits. Run with: go test -tags=e2e ./e2e/...
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/engine"
	"github.com/jhoblitt/markdown-linkerator/internal/report"
	"github.com/jhoblitt/markdown-linkerator/internal/testserver"
)

func ptr[T any](v T) *T { return &v }

func writeMD(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

func baseConfig(t *testing.T, rps float64) config.Config {
	t.Helper()
	c := config.Config{
		AliveStatusCodes:   []int{200, 206},
		PerHostRPS:         rps,
		PerHostBurst:       2,
		URLWorkers:         8,
		ParseWorkers:       4,
		RetryOn429:         ptr(true),
		RetryCount:         ptr(6),
		FallbackRetryDelay: config.NewDuration(100 * time.Millisecond),
		BackoffMax:         config.NewDuration(2 * time.Second),
	}
	c.Cache.Enabled = ptr(true)
	c.Cache.Path = filepath.Join(t.TempDir(), "cache.json")
	c.Cache.TTL = config.NewDuration(time.Hour)
	return c
}

func runEngine(t *testing.T, cfg config.Config, inputs ...string) *report.Summary {
	t.Helper()
	resolved, err := cfg.Resolve()
	require.NoError(t, err)
	sum, err := engine.Run(context.Background(), resolved, inputs, report.Options{
		Format: "text",
		Out:    &strings.Builder{},
	})
	require.NoError(t, err)
	require.NotNil(t, sum)
	return sum
}

// Test429Backoff proves the /later route (429 + Retry-After, then 200) ends
// alive after retries — the direct analog of the rook docs.ceph.com failure.
func Test429Backoff(t *testing.T) {
	srv := testserver.New()
	defer srv.Close()
	srv.SetLaterFailures(2)

	dir := t.TempDir()
	md := writeMD(t, dir, "later.md", "# Later\n\n[later]("+srv.URL("/later")+")\n")

	cfg := baseConfig(t, 50)
	sum := runEngine(t, cfg, md)

	assert.Equal(t, 0, sum.Dead, "429-then-200 must resolve alive")
	assert.Equal(t, 1, sum.Alive)
	assert.Equal(t, 0, sum.ExitCode)
	assert.GreaterOrEqual(t, srv.Requests("/later"), 2, "should have retried at least once")
}

// TestCacheHitSkipsRequests proves a second run with a warm cache issues zero
// new requests for already-checked URLs — what makes frequent CI runs 429-safe.
func TestCacheHitSkipsRequests(t *testing.T) {
	srv := testserver.New()
	defer srv.Close()

	dir := t.TempDir()
	md := writeMD(t, dir, "cache.md", "# Cache\n\n[ok]("+srv.URL("/")+")\n")

	cfg := baseConfig(t, 50)
	// Share one cache file across both runs.
	cachePath := filepath.Join(t.TempDir(), "shared-cache.json")
	cfg.Cache.Path = cachePath

	sum1 := runEngine(t, cfg, md)
	require.Equal(t, 1, sum1.Alive)
	after1 := srv.Requests("/")
	require.GreaterOrEqual(t, after1, 1)

	sum2 := runEngine(t, cfg, md)
	assert.Equal(t, 1, sum2.Alive)
	assert.Equal(t, after1, srv.Requests("/"), "second run must be served from cache (no new requests)")
}

// TestPerHostPacing proves distinct URLs to one host are paced at ~rps, not
// fired in a burst (the missing feature that caused the 429s).
func TestPerHostPacing(t *testing.T) {
	srv := testserver.New()
	defer srv.Close()

	const n = 12
	var b strings.Builder
	b.WriteString("# Pacing\n\n")
	for i := 0; i < n; i++ {
		// Distinct query keeps each URL unique (no dedup) but all hit "/" → 200.
		fmt.Fprintf(&b, "- [u%d](%s)\n", i, srv.URL(fmt.Sprintf("/?n=%d", i)))
	}
	dir := t.TempDir()
	md := writeMD(t, dir, "pacing.md", b.String())

	rps := 10.0
	cfg := baseConfig(t, rps)
	cfg.Cache.Enabled = ptr(false) // measure real requests, not cache hits

	start := time.Now()
	sum := runEngine(t, cfg, md)
	elapsed := time.Since(start)

	assert.Equal(t, n, sum.Alive)
	// With burst=2 at rps=10, the remaining n-2 requests take (n-2)/rps seconds;
	// allow 20% slack for scheduling.
	minExpected := time.Duration(float64(n-2) / rps * 0.8 * float64(time.Second))
	assert.GreaterOrEqual(t, elapsed, minExpected, "requests should be paced, not bursted")
}

// TestMixedRoutes checks classification and exit code across the fixture routes.
func TestMixedRoutes(t *testing.T) {
	srv := testserver.New()
	defer srv.Close()

	dir := t.TempDir()
	content := "# Mixed\n\n" +
		"- [ok](" + srv.URL("/") + ")\n" +
		"- [nohead](" + srv.URL("/nohead") + ")\n" +
		"- [partial](" + srv.URL("/partial") + ")\n" +
		"- [loop](" + srv.URL("/loop") + ")\n" +
		"- [notfound](" + srv.URL("/status-404") + ")\n"
	md := writeMD(t, dir, "mixed.md", content)

	cfg := baseConfig(t, 50)
	sum := runEngine(t, cfg, md)

	assert.Equal(t, 3, sum.Alive, "/, /nohead (HEAD->GET), /partial (206)")
	assert.Equal(t, 2, sum.Dead, "/loop (redirect cap) and 404")
	assert.Equal(t, 1, sum.ExitCode)
}
