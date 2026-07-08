package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCacheFingerprintTokenPresence guards that the cache fingerprint keys on
// GitHub-token *presence* (auth vs unauth), not the token value. Keying on the
// value would change the fingerprint every CI run (Actions rotates GITHUB_TOKEN)
// and discard the cache cold, defeating cross-run caching.
func TestCacheFingerprintTokenPresence(t *testing.T) {
	base := Resolved{AliveStatusCodes: map[int]bool{200: true}, UserAgent: "ua", MaxRedirects: 8}

	none := base
	tokA := base
	tokA.GitHubToken = "ghp_AAA"
	tokB := base
	tokB.GitHubToken = "ghp_BBB"

	assert.NotEqual(t, none.CacheFingerprint(), tokA.CacheFingerprint(),
		"authenticated vs unauthenticated must differ")
	assert.Equal(t, tokA.CacheFingerprint(), tokB.CacheFingerprint(),
		"different token values must share a fingerprint, so a rotating CI token still hits the cache")
}
