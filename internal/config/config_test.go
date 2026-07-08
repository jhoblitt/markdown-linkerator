package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDurationUnmarshal(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		set  bool
	}{
		{`"5s"`, 5 * time.Second, true},
		{`"2000ms"`, 2000 * time.Millisecond, true},
		{`"1m"`, time.Minute, true},
		{`60000`, 60000 * time.Millisecond, true}, // bare number = ms (tcort)
		{`"500"`, 500 * time.Millisecond, true},   // bare-number string = ms
		{`null`, 0, false},
		{`""`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			var d Duration
			require.NoError(t, d.UnmarshalJSON([]byte(tc.in)))
			assert.Equal(t, tc.set, d.Set)
			assert.Equal(t, tc.want, d.D)
		})
	}
}

// TestLoadTcortDropIn proves a verbatim markdown-link-check JSON config parses.
func TestLoadTcortDropIn(t *testing.T) {
	dir := t.TempDir()
	// This is rook's actual mlc_config.json shape.
	js := `{
	  "ignorePatterns": [
	    { "pattern": "^https://my-object-store\\.my-domain\\.net" },
	    { "pattern": "^#ceph\\.rook\\.io" }
	  ],
	  "replacementPatterns": [
	    { "pattern": "^/", "replacement": "{{BASEURL}}/" }
	  ],
	  "httpHeaders": [
	    { "urls": ["https://example.com"], "headers": { "Authorization": "Basic Zm9v" } }
	  ],
	  "aliveStatusCodes": [200, 206],
	  "timeout": "5s",
	  "retryOn429": true,
	  "retryCount": 1
	}`
	p := filepath.Join(dir, "mlc_config.json")
	require.NoError(t, os.WriteFile(p, []byte(js), 0o644))

	c, err := Load(p)
	require.NoError(t, err)
	assert.Len(t, c.IgnorePatterns, 2)
	assert.Equal(t, "^https://my-object-store\\.my-domain\\.net", c.IgnorePatterns[0].Pattern)
	assert.Len(t, c.ReplacementPatterns, 1)
	assert.Equal(t, "{{BASEURL}}/", c.ReplacementPatterns[0].Replacement)
	assert.Len(t, c.HTTPHeaders, 1)
	assert.Equal(t, []int{200, 206}, c.AliveStatusCodes)
	assert.Equal(t, 5*time.Second, c.Timeout.D)
	require.NotNil(t, c.RetryOn429)
	assert.True(t, *c.RetryOn429)
	require.NotNil(t, c.RetryCount)
	assert.Equal(t, 1, *c.RetryCount)
}

// TestLoadRookLegacyFixture parses rook's actual markdown-link-check config
// (vendored verbatim) to guard the drop-in compatibility contract.
func TestLoadRookLegacyFixture(t *testing.T) {
	c, err := Load("../../testdata/configs/rook-mlc_config.json")
	require.NoError(t, err)
	assert.Len(t, c.IgnorePatterns, 8)
	assert.Equal(t, "^http://host:port", c.IgnorePatterns[1].Pattern)
	assert.Equal(t, "^quickstart\\.md$", c.IgnorePatterns[6].Pattern)
	assert.Equal(t, []int{200, 206}, c.AliveStatusCodes)
	assert.Equal(t, 5*time.Second, c.Timeout.D)
	require.NotNil(t, c.RetryOn429)
	assert.True(t, *c.RetryOn429)
	require.NotNil(t, c.RetryCount)
	assert.Equal(t, 1, *c.RetryCount)

	// It resolves without error and folds retryCount into the retry limit.
	r, err := c.Resolve()
	require.NoError(t, err)
	assert.Equal(t, 1, r.MaxRetries)
	assert.True(t, r.AliveStatusCodes[206])
	assert.Len(t, r.IgnorePatterns, 8) // all patterns compiled
}

// TestLoadNativeYAML proves the same struct parses native YAML with extended keys.
func TestLoadNativeYAML(t *testing.T) {
	dir := t.TempDir()
	y := `
aliveStatusCodes: [200, 206]
timeout: 20s
perHostRPS: 2.5
hostOverrides:
  docs.ceph.com:
    rps: 1
    burst: 1
urlWorkers: 20
cache:
  enabled: true
  ttl: 12h
`
	p := filepath.Join(dir, "linkerator.yaml")
	require.NoError(t, os.WriteFile(p, []byte(y), 0o644))

	c, err := Load(p)
	require.NoError(t, err)
	assert.Equal(t, 2.5, c.PerHostRPS)
	assert.Equal(t, 20*time.Second, c.Timeout.D)
	assert.Equal(t, HostLimit{RPS: 1, Burst: 1}, c.HostOverrides["docs.ceph.com"])
	assert.Equal(t, 20, c.URLWorkers)
	require.NotNil(t, c.Cache.Enabled)
	assert.True(t, *c.Cache.Enabled)
	assert.Equal(t, 12*time.Hour, c.Cache.TTL.D)
}

func TestMergePrecedence(t *testing.T) {
	base := Defaults()
	assert.Equal(t, 1.0, base.PerHostRPS)
	assert.Equal(t, 10, base.URLWorkers)

	// A file that only raises workers must not clobber other defaults.
	file := Config{URLWorkers: 25}
	base.Merge(file)
	assert.Equal(t, 25, base.URLWorkers)
	assert.Equal(t, 1.0, base.PerHostRPS) // untouched default survives

	// A later layer overrides.
	flags := Config{PerHostRPS: 3}
	base.Merge(flags)
	assert.Equal(t, 3.0, base.PerHostRPS)
	assert.Equal(t, 25, base.URLWorkers)
}

func TestRetryCountZeroDisablesRetries(t *testing.T) {
	zero := 0
	r, err := Config{RetryCount: &zero}.Resolve()
	require.NoError(t, err)
	assert.Equal(t, 0, r.MaxRetries, "an explicit retry-count 0 must disable retries")

	unset, err := Config{}.Resolve()
	require.NoError(t, err)
	assert.Equal(t, 4, unset.MaxRetries, "an unset retry-count falls back to the default")
}

func TestResolveConservativeDefaults(t *testing.T) {
	r, err := Config{}.Resolve()
	require.NoError(t, err)
	assert.Equal(t, 1.0, r.PerHostRPS)
	assert.Equal(t, 2, r.PerHostBurst)
	assert.Equal(t, 10, r.URLWorkers)
	assert.Equal(t, 10, r.ParseWorkers)
	assert.Equal(t, 24*time.Hour, r.Cache.TTL)
	assert.True(t, r.AliveStatusCodes[200])
	assert.True(t, r.CheckExternal)
}
