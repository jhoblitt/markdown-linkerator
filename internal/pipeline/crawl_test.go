package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExpandGlobRootedAtPrefix guards that a ** glob is rooted at its literal
// prefix directory, not the whole repository: docs/**/*.md must not pull in
// files under a sibling like other/.
func TestExpandGlobRootedAtPrefix(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel string) {
		full := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte("x"), 0o644))
	}
	mk("docs/a.md")
	mk("docs/sub/b.md")
	mk("other/c.md")

	matches, err := expandGlob(filepath.Join(dir, "docs", "**", "*.md"))
	require.NoError(t, err)

	assert.Len(t, matches, 2, "only the two files under docs/ should match")
	sep := string(filepath.Separator)
	for _, m := range matches {
		assert.NotContains(t, m, sep+"other"+sep, "must not scan outside the docs/ prefix")
	}
}
