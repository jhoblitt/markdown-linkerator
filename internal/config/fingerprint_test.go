package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCacheFingerprintTokenScoped guards that the cache fingerprint is keyed by
// which GitHub token was used, not merely whether one was present — so a result
// checked with one token is never reused by a run with a different token.
func TestCacheFingerprintTokenScoped(t *testing.T) {
	base := Resolved{AliveStatusCodes: map[int]bool{200: true}, UserAgent: "ua", MaxRedirects: 8}

	none := base
	tokA := base
	tokA.GitHubToken = "ghp_AAA"
	tokB := base
	tokB.GitHubToken = "ghp_BBB"
	tokA2 := base
	tokA2.GitHubToken = "ghp_AAA"

	assert.NotEqual(t, none.CacheFingerprint(), tokA.CacheFingerprint(), "authenticated vs unauthenticated must differ")
	assert.NotEqual(t, tokA.CacheFingerprint(), tokB.CacheFingerprint(), "distinct tokens must produce distinct fingerprints")
	assert.Equal(t, tokA.CacheFingerprint(), tokA2.CacheFingerprint(), "the same token must produce the same fingerprint")
}
