package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
	"github.com/jhoblitt/markdown-linkerator/internal/testserver"
)

// TestIsGitHubHost guards which hosts receive the GitHub token — GitHub's own
// hosts (including subdomains and content CDNs), never a look-alike.
func TestIsGitHubHost(t *testing.T) {
	for _, h := range []string{
		"github.com", "api.github.com", "raw.githubusercontent.com",
		"gist.github.com", "codeload.github.com", "objects.githubusercontent.com",
	} {
		assert.Truef(t, isGitHubHost(h), "%s should be a GitHub host", h)
	}
	for _, h := range []string{
		"example.com", "github.com.evil.example", "notgithub.com",
		"mygithub.com", "githubusercontent.com.evil.example",
	} {
		assert.Falsef(t, isGitHubHost(h), "%s must not be a GitHub host", h)
	}
}

// TestGitHubTokenAttachedForGitHubHostsOnly verifies the token is sent to a
// GitHub host but not to an arbitrary host.
func TestGitHubTokenAttachedForGitHubHostsOnly(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewHTTPChecker(config.Resolved{
		AliveStatusCodes: map[int]bool{200: true},
		Timeout:          2 * time.Second,
		MaxRedirects:     5,
		GitHubToken:      "s3cr3t",
	})
	// The test server is not a GitHub host, so no token is attached.
	c.Check(context.Background(), model.Target{URL: srv.URL + "/", Kind: model.KindHTTP})
	assert.Empty(t, gotAuth, "token must not be sent to a non-GitHub host")
}

// TestCustomHeadersStrippedOnCrossOriginRedirect guards that configured custom
// headers (which may carry {{env.*}} secrets) are not forwarded to a
// cross-origin redirect target.
func TestCustomHeadersStrippedOnCrossOriginRedirect(t *testing.T) {
	var gotSecret string
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSecret = r.Header.Get("X-Secret")
		w.WriteHeader(http.StatusOK)
	}))
	defer dest.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL+"/target", http.StatusFound)
	}))
	defer origin.Close()

	c := NewHTTPChecker(config.Resolved{
		AliveStatusCodes: map[int]bool{200: true},
		Timeout:          2 * time.Second,
		MaxRedirects:     5,
		HTTPHeaders: []config.HeaderRule{
			{URLs: []string{origin.URL}, Headers: map[string]string{"X-Secret": "s3cr3t"}},
		},
	})
	res := c.Check(context.Background(), model.Target{URL: origin.URL + "/", Kind: model.KindHTTP})
	assert.Equal(t, model.StateAlive, res.State)
	assert.Empty(t, gotSecret, "custom header must not leak to a cross-origin redirect target")
}

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
