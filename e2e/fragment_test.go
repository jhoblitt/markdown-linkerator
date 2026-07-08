//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
)

// TestCrossFileFragments proves a cross-file section link is validated against
// the referenced file's anchors: an existing anchor is alive, a missing one is
// dead even though the target file exists. This is the false-negative the
// adversarial review flagged.
func TestCrossFileFragments(t *testing.T) {
	dir := t.TempDir()
	writeMD(t, dir, "b.md", "# B\n\n## Existing Section\n\nbody\n")
	writeMD(t, dir, "a.md", "# A\n\n"+
		"- [ok](./b.md#existing-section)\n"+
		"- [broken](./b.md#missing-section)\n")

	cfg := config.Config{}
	cfg.CheckExternal = ptr(false)
	cfg.Cache.Enabled = ptr(false)

	sum := runEngine(t, cfg, dir)

	assert.Equal(t, 1, sum.Alive, "./b.md#existing-section resolves")
	assert.Equal(t, 1, sum.Dead, "./b.md#missing-section: file exists but anchor is absent")
	assert.Equal(t, 1, sum.ExitCode)
}
