//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/engine"
	"github.com/jhoblitt/markdown-linkerator/internal/report"
)

// TestSourceReadErrorFailsRun proves an unreadable input file fails the run
// (never a false green), independent of --fail-on-error.
func TestSourceReadErrorFailsRun(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-based unreadable-file test requires a non-root user")
	}
	dir := t.TempDir()
	writeMD(t, dir, "good.md", "# ok\n\n[self](#ok)\n")
	unreadable := filepath.Join(dir, "unreadable.md")
	require.NoError(t, os.WriteFile(unreadable, []byte("# secret\n"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	cfg := config.Config{}
	cfg.CheckExternal = ptr(false)
	cfg.Cache.Enabled = ptr(false)
	resolved, err := cfg.Resolve()
	require.NoError(t, err)

	sum, runErr := engine.Run(context.Background(), resolved, []string{dir}, report.Options{Out: &strings.Builder{}})
	require.Error(t, runErr, "an unreadable source file must fail the run")
	require.NotNil(t, sum)
	assert.Contains(t, runErr.Error(), "source file")
}
