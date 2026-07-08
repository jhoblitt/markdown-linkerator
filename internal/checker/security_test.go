package checker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
	"github.com/jhoblitt/markdown-linkerator/internal/testserver"
)

// TestRuleMatchesExactOrigin guards the credential-leak fix: header rules must
// require an exact scheme+host origin (with a path boundary), never a raw string
// prefix that a look-alike host could satisfy.
func TestRuleMatchesExactOrigin(t *testing.T) {
	host := config.HeaderRule{URLs: []string{"https://api.github.com"}}
	assert.True(t, ruleMatches(host, "https://api.github.com"))
	assert.True(t, ruleMatches(host, "https://api.github.com/repos/x"))
	assert.False(t, ruleMatches(host, "https://api.github.com.evil.example/x"), "look-alike host must not match")
	assert.False(t, ruleMatches(host, "http://api.github.com/x"), "scheme must match")
	assert.False(t, ruleMatches(host, "https://evil.example/https://api.github.com"))

	path := config.HeaderRule{URLs: []string{"https://h.example/api"}}
	assert.True(t, ruleMatches(path, "https://h.example/api"))
	assert.True(t, ruleMatches(path, "https://h.example/api/v1"))
	assert.False(t, ruleMatches(path, "https://h.example/apix"), "path boundary must hold")
}

// TestCanceledCheckIsError guards that a canceled check is StateError (not the
// StateAlive zero value), so an interrupted run cannot exit green or cache a
// bogus alive entry.
func TestCanceledCheckIsError(t *testing.T) {
	srv := testserver.New()
	defer srv.Close()

	c := NewHTTPChecker(config.Resolved{
		AliveStatusCodes: map[int]bool{200: true},
		Timeout:          time.Second,
		MaxRedirects:     5,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := c.Check(ctx, model.Target{URL: srv.URL("/"), Kind: model.KindHTTP})
	assert.Equal(t, model.StateError, res.State, "canceled check must be error, not alive")
	assert.Error(t, res.Err)
}

// TestNo429GetFallback guards that a persistent 429 on HEAD is authoritative and
// does not trigger a second full retry cycle via GET (which would amplify load
// against a throttling host).
func TestNo429GetFallback(t *testing.T) {
	srv := testserver.New()
	defer srv.Close()

	c := NewHTTPChecker(config.Resolved{
		AliveStatusCodes:   map[int]bool{200: true},
		Timeout:            2 * time.Second,
		MaxRedirects:       5,
		RetryOn429:         true,
		MaxRetries:         1,
		FallbackRetryDelay: time.Millisecond,
		BackoffMax:         20 * time.Millisecond,
	})
	res := c.Check(context.Background(), model.Target{URL: srv.URL("/toomany"), Kind: model.KindHTTP})

	assert.Equal(t, model.StateDead, res.State)
	assert.Equal(t, 429, res.StatusCode)
	// HEAD initial + 1 HEAD retry == 2; a GET fallback would add two more.
	assert.Equal(t, 2, srv.Requests("/toomany"), "429 HEAD must not fall through to GET")
}
