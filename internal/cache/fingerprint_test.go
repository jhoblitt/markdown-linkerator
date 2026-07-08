package cache

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

// TestFingerprintInvalidatesCache guards that a warm cache is discarded when the
// request policy (fingerprint) changes, so results checked under one policy are
// not reused under another.
func TestFingerprintInvalidatesCache(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	c1, err := New(p, time.Hour, true, "policy-A")
	require.NoError(t, err)
	c1.Put("https://x/", model.Result{State: model.StateAlive, StatusCode: 200})
	require.NoError(t, c1.Save())

	// Different fingerprint → cold start (entry not reused).
	c2, err := New(p, time.Hour, true, "policy-B")
	require.NoError(t, err)
	_, ok := c2.Get("https://x/")
	assert.False(t, ok, "a changed config fingerprint must invalidate the cache")

	// Same fingerprint → reused.
	c3, err := New(p, time.Hour, true, "policy-A")
	require.NoError(t, err)
	_, ok = c3.Get("https://x/")
	assert.True(t, ok, "the same fingerprint reuses the cache")
}
