//go:build e2e

package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jhoblitt/markdown-linkerator/internal/engine"
	"github.com/jhoblitt/markdown-linkerator/internal/report"
	"github.com/jhoblitt/markdown-linkerator/internal/testserver"
)

// TestMaxTimeDeadline proves a run that hits its deadline returns an error
// (→ exit 2), counts the in-flight check as errored rather than alive, and does
// not cache it.
func TestMaxTimeDeadline(t *testing.T) {
	srv := testserver.New()
	defer srv.Close()

	dir := t.TempDir()
	md := writeMD(t, dir, "slow.md", "# Slow\n\n[slow]("+srv.URL("/slow")+")\n")

	cfg := baseConfig(t, 50)
	cfg.Cache.Path = filepath.Join(t.TempDir(), "cache.json")
	resolved, err := cfg.Resolve()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	sum, runErr := engine.Run(ctx, resolved, []string{md}, report.Options{Out: &strings.Builder{}})
	elapsed := time.Since(start)

	require.Error(t, runErr, "a run that hit its deadline must return an error")
	require.NotNil(t, sum)
	assert.Less(t, elapsed, 2*time.Second, "must stop at the deadline, not wait for the slow response")
	assert.Equal(t, 0, sum.Alive, "the slow check must not count as alive")
	assert.Equal(t, 1, sum.Errored, "the canceled check is errored")
}
