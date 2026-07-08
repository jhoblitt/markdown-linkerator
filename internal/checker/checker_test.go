package checker

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

func resolve(t *testing.T, c config.Config) config.Resolved {
	t.Helper()
	r, err := c.Resolve()
	require.NoError(t, err)
	return r
}

func TestIsIgnored(t *testing.T) {
	cfg := resolve(t, config.Config{
		IgnorePatterns: []config.Pattern{
			{Pattern: `^https://ignore\.example/`},
			{Pattern: `\.png$`},
		},
	})
	tests := []struct {
		url  string
		want bool
	}{
		{"https://ignore.example/page", true},
		{"https://cdn.test/a.png", true},
		{"https://ok.example/page", false},
		{"https://ignore.example.evil.test", false}, // prefix-anchored, no matching '/'
	}
	for _, tc := range tests {
		assert.Equalf(t, tc.want, IsIgnored(tc.url, cfg), "url=%s", tc.url)
	}
	// No patterns → never ignored.
	assert.False(t, IsIgnored("https://anything", resolve(t, config.Config{})))
}

func TestCheckFile(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "present.md")
	require.NoError(t, os.WriteFile(existing, []byte("hi"), 0o644))

	alive := CheckFile(model.Target{URL: existing, Kind: model.KindFileRel})
	assert.Equal(t, model.StateAlive, alive.State)
	assert.Equal(t, 200, alive.StatusCode)

	dead := CheckFile(model.Target{URL: filepath.Join(dir, "missing.md"), Kind: model.KindFileRel})
	assert.Equal(t, model.StateDead, dead.State)
	assert.Equal(t, 400, dead.StatusCode)

	// A directory exists, so it resolves alive.
	assert.Equal(t, model.StateAlive, CheckFile(model.Target{URL: dir}).State)
}

func TestCheckHash(t *testing.T) {
	anchors := map[string]bool{
		"introduction": true,
		"bar-baz":      true,
		// A verbatim, case-sensitive HTML id as emitted by CRD reference docs.
		"ceph.rook.io/v1.CephCluster": true,
	}
	tests := []struct {
		frag string
		want model.State
	}{
		{"introduction", model.StateAlive},
		{"#introduction", model.StateAlive},               // leading '#' stripped
		{"Introduction", model.StateAlive},                // lowercased slug match
		{"bar%2Dbaz", model.StateAlive},                   // percent-decoded ('-')
		{"Bar-Baz", model.StateAlive},                     // decode + lowercase
		{"ceph.rook.io/v1.CephCluster", model.StateAlive}, // case-sensitive HTML id
		{"missing", model.StateDead},
		{"", model.StateDead},
	}
	for _, tc := range tests {
		res := CheckHash(model.Target{Fragment: tc.frag, Kind: model.KindHashLocal}, anchors)
		assert.Equalf(t, tc.want, res.State, "frag=%q", tc.frag)
		if tc.want == model.StateAlive {
			assert.Equal(t, 200, res.StatusCode)
		} else {
			assert.Equal(t, 404, res.StatusCode)
		}
	}
}

func TestCheckMailtoSyntax(t *testing.T) {
	cfg := resolve(t, config.Config{}) // MailtoCheckMX defaults false
	tests := []struct {
		url  string
		want model.State
	}{
		{"mailto:someone@example.com", model.StateAlive},
		{"MAILTO:someone@example.com", model.StateAlive},             // case-insensitive scheme
		{"mailto:a@example.com?subject=hi&body=x", model.StateAlive}, // ?headers dropped
		{"mailto:a@example.com,b@example.org", model.StateAlive},     // multi-recipient
		{"mailto:not-an-email", model.StateDead},
		{"mailto:a@example.com,broken", model.StateDead}, // one bad recipient fails all
		{"mailto:", model.StateDead},
	}
	for _, tc := range tests {
		res := CheckMailto(context.Background(), model.Target{URL: tc.url, Kind: model.KindMailto}, cfg)
		assert.Equalf(t, tc.want, res.State, "url=%s", tc.url)
		if tc.want == model.StateAlive {
			assert.Equal(t, 200, res.StatusCode)
		} else {
			assert.Equal(t, 400, res.StatusCode)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantOK  bool
		wantDur time.Duration
	}{
		{"seconds", "2", true, 2 * time.Second},
		{"zero seconds", "0", true, 0},
		{"whitespace seconds", "  5 ", true, 5 * time.Second},
		{"empty", "", false, 0},
		{"garbage", "soon", false, 0},
		{"negative", "-3", false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := parseRetryAfter(tc.in)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantDur, d)
			}
		})
	}

	t.Run("http-date future", func(t *testing.T) {
		future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
		d, ok := parseRetryAfter(future)
		require.True(t, ok)
		assert.Greater(t, d, 20*time.Second)
		assert.LessOrEqual(t, d, 30*time.Second)
	})

	t.Run("http-date past", func(t *testing.T) {
		past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
		d, ok := parseRetryAfter(past)
		require.True(t, ok)
		assert.Equal(t, time.Duration(0), d)
	})
}
