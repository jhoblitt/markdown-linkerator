package checker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
	"github.com/jhoblitt/markdown-linkerator/internal/testserver"
)

// TestConnectionFailureBoundedFast guards that a connection failure retries a
// bounded few times quickly and gives up, rather than sitting in the long
// rate-limit backoff on a socket that will never connect.
func TestConnectionFailureBoundedFast(t *testing.T) {
	// A port that is bound then closed → connection refused, reliably.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	c := NewHTTPChecker(config.Resolved{
		AliveStatusCodes:   map[int]bool{200: true},
		Timeout:            2 * time.Second,
		MaxRedirects:       5,
		MaxRetries:         4,
		ConnectRetries:     2,
		FallbackRetryDelay: 30 * time.Second, // the long rate-limit backoff must NOT be used here
		BackoffMax:         2 * time.Minute,
	})
	start := time.Now()
	res := c.Check(context.Background(), model.Target{URL: "http://" + addr + "/", Kind: model.KindHTTP})
	elapsed := time.Since(start)

	assert.Equal(t, model.StateDead, res.State)
	assert.Equal(t, 0, res.StatusCode)
	assert.Less(t, elapsed, 15*time.Second, "connection failures must fail fast, not sit in the rate-limit backoff")
}

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

// TestRetryCountZeroDisables429Retries guards that retryCount=0 truly disables
// rate-limit retries even when connectRetries>0 — the two budgets must not share.
func TestRetryCountZeroDisables429Retries(t *testing.T) {
	srv := testserver.New()
	defer srv.Close()

	c := NewHTTPChecker(config.Resolved{
		AliveStatusCodes:   map[int]bool{200: true},
		Timeout:            2 * time.Second,
		MaxRedirects:       5,
		RetryOn429:         true,
		MaxRetries:         0, // retryCount=0 → no 429 retries
		ConnectRetries:     3, // must NOT leak into the 429 retry budget
		FallbackRetryDelay: time.Millisecond,
		BackoffMax:         20 * time.Millisecond,
	})
	res := c.Check(context.Background(), model.Target{URL: srv.URL("/toomany"), Kind: model.KindHTTP})

	assert.Equal(t, model.StateDead, res.State)
	assert.Equal(t, 429, res.StatusCode)
	assert.Equal(t, 1, srv.Requests("/toomany"), "retryCount=0 must not retry a 429, even with connectRetries>0")
}

// TestGetFallbackBodyBounded guards that a HEAD-rejecting endpoint serving a huge
// GET body cannot make the checker download it in full just to read the status.
func TestGetFallbackBodyBounded(t *testing.T) {
	const total = 20 << 20 // 20 MiB the server would serve if we drained to EOF
	var written int64
	getDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed) // force the GET fallback
			return
		}
		defer close(getDone)
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, 64<<10)
		for sent := 0; sent < total; sent += len(buf) {
			n, err := w.Write(buf)
			atomic.AddInt64(&written, int64(n))
			if err != nil {
				return // client closed the body early
			}
		}
	}))
	defer srv.Close()

	c := NewHTTPChecker(config.Resolved{
		AliveStatusCodes: map[int]bool{200: true},
		Timeout:          5 * time.Second,
		MaxRedirects:     5,
	})
	res := c.Check(context.Background(), model.Target{URL: srv.URL + "/", Kind: model.KindHTTP})
	assert.Equal(t, model.StateAlive, res.State)

	select {
	case <-getDone:
	case <-time.After(5 * time.Second):
		t.Fatal("GET handler did not stop after the client closed the body")
	}
	assert.Lessf(t, atomic.LoadInt64(&written), int64(2<<20),
		"GET fallback must not drain the whole body; server wrote %d bytes", atomic.LoadInt64(&written))
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
