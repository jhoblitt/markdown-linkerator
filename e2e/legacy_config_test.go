//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/testserver"
)

// TestLegacyRookConfig drives the full engine with rook's ACTUAL
// markdown-link-check config (tests/scripts/mlc_config.json, vendored verbatim
// into testdata/configs/rook-mlc_config.json) to prove the legacy JSON format
// is honored end-to-end: ignorePatterns, aliveStatusCodes (206), retryOn429 +
// retryCount, and the ms-string timeout.
func TestLegacyRookConfig(t *testing.T) {
	srv := testserver.New()
	defer srv.Close()
	srv.SetLaterFailures(1) // retryCount:1 permits exactly one retry

	legacy, err := config.Load("../testdata/configs/rook-mlc_config.json")
	require.NoError(t, err)

	// The legacy keys parsed with their upstream meaning.
	require.Len(t, legacy.IgnorePatterns, 8)
	require.Equal(t, []int{200, 206}, legacy.AliveStatusCodes)
	require.NotNil(t, legacy.RetryOn429)
	require.True(t, *legacy.RetryOn429)
	require.NotNil(t, legacy.RetryCount)
	require.Equal(t, 1, *legacy.RetryCount)
	require.Equal(t, 5*time.Second, legacy.Timeout.D)

	// Overlay only test-runtime knobs (fast pacing, no cache); the legacy
	// semantics above are left untouched.
	legacy.Merge(config.Config{PerHostRPS: 50, PerHostBurst: 5, URLWorkers: 8, ParseWorkers: 4})
	legacy.Cache.Enabled = ptr(false)

	dir := t.TempDir()
	md := "# Legacy config integration\n\n" +
		"- [ok](" + srv.URL("/") + ")\n" + // 200 alive
		"- [partial](" + srv.URL("/partial") + ")\n" + // 206 alive via aliveStatusCodes
		"- [later](" + srv.URL("/later") + ")\n" + // 429->200 via retryOn429/retryCount:1
		"- [objstore](https://my-object-store.my-domain.net/bucket)\n" + // ignored
		"- [hostport](http://host:port/swift/v1)\n" + // ignored
		"- [quickstart](quickstart.md)\n" // ignored via ^quickstart\.md$
	p := writeMD(t, dir, "legacy.md", md)

	sum := runEngine(t, legacy, p)

	assert.Equal(t, 3, sum.Alive, "/, /partial (206), /later (429->200)")
	assert.Equal(t, 3, sum.Ignored, "objstore, hostport, quickstart matched ignorePatterns")
	assert.Equal(t, 0, sum.Dead)
	assert.Equal(t, 0, sum.ExitCode)
}
