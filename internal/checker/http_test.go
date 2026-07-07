package checker

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
	"github.com/jhoblitt/markdown-linkerator/internal/testserver"
)

// httpTestCfg builds a Resolved tuned for fast tests: a tiny RetryWaitMin and a
// BackoffMax of 2s (>= the fixture's 1s Retry-After, so that value is honored
// rather than abandoned). alive overrides the alive status codes (default 200).
func httpTestCfg(alive ...int) config.Resolved {
	codes := map[int]bool{}
	for _, c := range alive {
		codes[c] = true
	}
	if len(codes) == 0 {
		codes[200] = true
	}
	return config.Resolved{
		AliveStatusCodes:   codes,
		Timeout:            5 * time.Second,
		RetryOn429:         true,
		RetryCount:         4,
		FallbackRetryDelay: 5 * time.Millisecond,
		MaxRetries:         4,
		BackoffMax:         2 * time.Second,
		UserAgent:          "linkerator-test",
		MaxRedirects:       8,
	}
}

func target(url string) model.Target {
	return model.Target{Raw: url, URL: url, Kind: model.KindHTTP}
}

func TestHTTPChecker(t *testing.T) {
	srv := testserver.New()
	defer srv.Close()

	t.Run("ok", func(t *testing.T) {
		c := NewHTTPChecker(httpTestCfg())
		res := c.Check(context.Background(), target(srv.URL("/")))
		require.NoError(t, res.Err)
		assert.Equal(t, model.StateAlive, res.State)
		assert.Equal(t, 200, res.StatusCode)
	})

	t.Run("head-to-get fallback", func(t *testing.T) {
		c := NewHTTPChecker(httpTestCfg())
		res := c.Check(context.Background(), target(srv.URL("/nohead")))
		assert.Equal(t, model.StateAlive, res.State)
		assert.Equal(t, 200, res.StatusCode)
		// HEAD (405) then GET (200) → two requests.
		assert.Equal(t, 2, srv.Requests("/nohead"))
	})

	t.Run("partial dead unless configured alive", func(t *testing.T) {
		dead := NewHTTPChecker(httpTestCfg()).Check(context.Background(), target(srv.URL("/partial")))
		assert.Equal(t, model.StateDead, dead.State)
		assert.Equal(t, 206, dead.StatusCode)

		alive := NewHTTPChecker(httpTestCfg(200, 206)).Check(context.Background(), target(srv.URL("/partial")))
		assert.Equal(t, model.StateAlive, alive.State)
		assert.Equal(t, 206, alive.StatusCode)
	})

	t.Run("later alive after honored retry-after", func(t *testing.T) {
		c := NewHTTPChecker(httpTestCfg())
		start := time.Now()
		res := c.Check(context.Background(), target(srv.URL("/later")))
		require.NoError(t, res.Err)
		assert.Equal(t, model.StateAlive, res.State)
		assert.Equal(t, 200, res.StatusCode)
		assert.Greater(t, res.Retries, 0)
		assert.True(t, res.Saw429)
		assert.Equal(t, time.Second, res.RetryAfter) // last honored Retry-After
		assert.Less(t, time.Since(start), 2*time.Second)
	})

	t.Run("redirect followed", func(t *testing.T) {
		c := NewHTTPChecker(httpTestCfg())
		res := c.Check(context.Background(), target(srv.URL("/foo/redirect")))
		assert.Equal(t, model.StateAlive, res.State)
		assert.Equal(t, 200, res.StatusCode)
	})

	t.Run("redirect with body in head", func(t *testing.T) {
		c := NewHTTPChecker(httpTestCfg())
		res := c.Check(context.Background(), target(srv.URL("/redirect-with-body-in-head")))
		assert.Equal(t, model.StateAlive, res.State)
	})

	t.Run("image alive", func(t *testing.T) {
		c := NewHTTPChecker(httpTestCfg())
		res := c.Check(context.Background(), target(srv.URL("/hello.jpg")))
		assert.Equal(t, model.StateAlive, res.State)
		assert.Equal(t, 200, res.StatusCode)
	})

	t.Run("parentheses in path", func(t *testing.T) {
		c := NewHTTPChecker(httpTestCfg())
		res := c.Check(context.Background(), target(srv.URL("/foo(a=b).aspx")))
		assert.Equal(t, model.StateAlive, res.State)
	})

	t.Run("redirect loop is dead, no hang", func(t *testing.T) {
		c := NewHTTPChecker(httpTestCfg())
		res := checkWithin(t, 10*time.Second, func() model.Result {
			return c.Check(context.Background(), target(srv.URL("/loop")))
		})
		require.NoError(t, res.Err)
		assert.Equal(t, model.StateDead, res.State)
		assert.Equal(t, 0, res.StatusCode) // transport-level failure → synthetic 0
		assert.NotEmpty(t, res.Detail)
	})

	t.Run("basic auth", func(t *testing.T) {
		bare := NewHTTPChecker(httpTestCfg()).Check(context.Background(), target(srv.URL("/basic-auth")))
		assert.Equal(t, model.StateDead, bare.State)
		assert.Equal(t, 401, bare.StatusCode)

		cfg := httpTestCfg()
		cfg.HTTPHeaders = []config.HeaderRule{{
			URLs:    []string{srv.URL("/basic-auth")},
			Headers: map[string]string{"Authorization": "Basic dXNlcjpwYXNz"},
		}}
		withAuth := NewHTTPChecker(cfg).Check(context.Background(), target(srv.URL("/basic-auth")))
		assert.Equal(t, model.StateAlive, withAuth.State)
		assert.Equal(t, 200, withAuth.StatusCode)
	})

	t.Run("host populated", func(t *testing.T) {
		c := NewHTTPChecker(httpTestCfg())
		res := c.Check(context.Background(), target(srv.URL("/")))
		assert.NotEmpty(t, res.Host)
	})
}

func retryResp(t *testing.T, st *retryState, status int, retryAfter string) *http.Response {
	t.Helper()
	ctx := withRetryState(context.Background(), st)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://example.test/", nil)
	require.NoError(t, err)
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return &http.Response{StatusCode: status, Header: h, Request: req}
}

func TestCheckRetry(t *testing.T) {
	c := NewHTTPChecker(func() config.Resolved {
		cfg := httpTestCfg()
		cfg.BackoffMax = time.Second
		return cfg
	}())

	t.Run("429 retried and records saw429", func(t *testing.T) {
		st := &retryState{}
		resp := retryResp(t, st, 429, "1")
		ok, err := c.checkRetry(resp.Request.Context(), resp, nil)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.True(t, st.saw429)
	})

	t.Run("429 with over-long retry-after gives up", func(t *testing.T) {
		st := &retryState{}
		resp := retryResp(t, st, 429, "3600") // 1h > BackoffMax(1s)
		ok, err := c.checkRetry(resp.Request.Context(), resp, nil)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.True(t, st.saw429) // still observed, just not retried
	})

	t.Run("503 retried", func(t *testing.T) {
		ok, err := c.checkRetry(context.Background(), retryResp(t, &retryState{}, 503, ""), nil)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("other 5xx not retried", func(t *testing.T) {
		ok, err := c.checkRetry(context.Background(), retryResp(t, &retryState{}, 500, ""), nil)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("404 not retried", func(t *testing.T) {
		ok, err := c.checkRetry(context.Background(), retryResp(t, &retryState{}, 404, ""), nil)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("transport error retried", func(t *testing.T) {
		ok, err := c.checkRetry(context.Background(), nil, errors.New("connection refused"))
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("redirect loop not retried", func(t *testing.T) {
		ok, err := c.checkRetry(context.Background(), nil, errTooManyRedirects)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("context cancellation never retries", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ok, err := c.checkRetry(ctx, retryResp(t, &retryState{}, 429, "1"), nil)
		assert.Error(t, err)
		assert.False(t, ok)
	})

	t.Run("retryOn429 disabled", func(t *testing.T) {
		cfg := httpTestCfg()
		cfg.RetryOn429 = false
		st := &retryState{}
		resp := retryResp(t, st, 429, "")
		ok, err := NewHTTPChecker(cfg).checkRetry(resp.Request.Context(), resp, nil)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.True(t, st.saw429)
	})
}

func TestBackoff(t *testing.T) {
	c := NewHTTPChecker(httpTestCfg())

	t.Run("honors retry-after and records it", func(t *testing.T) {
		st := &retryState{}
		resp := retryResp(t, st, 429, "1")
		wait := c.backoff(5*time.Millisecond, 2*time.Second, 0, resp)
		assert.Equal(t, time.Second, wait)
		assert.Equal(t, time.Second, st.retryAfter)
	})

	t.Run("clamps retry-after to max", func(t *testing.T) {
		resp := retryResp(t, &retryState{}, 503, "10")
		wait := c.backoff(5*time.Millisecond, 500*time.Millisecond, 0, resp)
		assert.Equal(t, 500*time.Millisecond, wait)
	})

	t.Run("exponential fallback without retry-after", func(t *testing.T) {
		resp := retryResp(t, &retryState{}, 429, "")
		wait := c.backoff(10*time.Millisecond, time.Second, 3, resp)
		assert.GreaterOrEqual(t, wait, 10*time.Millisecond)
		assert.LessOrEqual(t, wait, time.Second)
	})

	t.Run("exponential is capped at max", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			wait := expJitterBackoff(10*time.Millisecond, 100*time.Millisecond, 20)
			assert.LessOrEqual(t, wait, 100*time.Millisecond)
			assert.GreaterOrEqual(t, wait, 10*time.Millisecond)
		}
	})
}

func TestHTTPCheckerContextCancel(t *testing.T) {
	srv := testserver.New()
	defer srv.Close()

	c := NewHTTPChecker(httpTestCfg())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	res := c.Check(ctx, target(srv.URL("/")))
	require.Error(t, res.Err)
}

// TestHTTPCheckerConcurrent shares one checker across many goroutines to prove
// the client (and the context-threaded retry state) is race-free under -race.
// Only stateless routes are used so results stay deterministic.
func TestHTTPCheckerConcurrent(t *testing.T) {
	srv := testserver.New()
	defer srv.Close()

	c := NewHTTPChecker(httpTestCfg())
	cases := []struct {
		path string
		want model.State
	}{
		{"/", model.StateAlive},
		{"/nohead", model.StateAlive},
		{"/hello.jpg", model.StateAlive},
		{"/foo/redirect", model.StateAlive},
		{"/foo(a=b).aspx", model.StateAlive},
		{"/loop", model.StateDead},
	}

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		tc := cases[i%len(cases)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := c.Check(context.Background(), target(srv.URL(tc.path)))
			assert.Equalf(t, tc.want, res.State, "path=%s", tc.path)
		}()
	}
	wg.Wait()
}

// checkWithin runs fn and fails the test if it does not return within d, so a
// checker hang surfaces as a failure rather than wedging the suite.
func checkWithin(t *testing.T, d time.Duration, fn func() model.Result) model.Result {
	t.Helper()
	done := make(chan model.Result, 1)
	go func() { done <- fn() }()
	select {
	case res := <-done:
		return res
	case <-time.After(d):
		t.Fatalf("Check did not return within %s (possible hang)", d)
		return model.Result{}
	}
}
