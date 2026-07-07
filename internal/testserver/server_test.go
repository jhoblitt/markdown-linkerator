package testserver

import (
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noRedirect is a client that returns each 3xx as-is so raw route behavior can
// be inspected.
func noRedirect() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func do(t *testing.T, c *http.Client, method, url string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	require.NoError(t, err)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func TestRoutes(t *testing.T) {
	srv := New()
	defer srv.Close()
	c := noRedirect()

	assert.Equal(t, 200, do(t, c, http.MethodGet, srv.URL("/"), nil).StatusCode)

	assert.Equal(t, 405, do(t, c, http.MethodHead, srv.URL("/nohead"), nil).StatusCode)
	assert.Equal(t, 200, do(t, c, http.MethodGet, srv.URL("/nohead"), nil).StatusCode)

	assert.Equal(t, 206, do(t, c, http.MethodGet, srv.URL("/partial"), nil).StatusCode)

	redirect := do(t, c, http.MethodGet, srv.URL("/foo/redirect"), nil)
	assert.Equal(t, 302, redirect.StatusCode)
	assert.Equal(t, "/foo/bar", redirect.Header.Get("Location"))
	assert.Equal(t, 200, do(t, c, http.MethodGet, srv.URL("/foo/bar"), nil).StatusCode)

	loop := do(t, c, http.MethodGet, srv.URL("/loop"), nil)
	assert.Equal(t, 302, loop.StatusCode)
	assert.Equal(t, "/loop", loop.Header.Get("Location"))

	assert.Equal(t, 401, do(t, c, http.MethodGet, srv.URL("/basic-auth"), nil).StatusCode)
	authed := do(t, c, http.MethodGet, srv.URL("/basic-auth"), http.Header{"Authorization": {"Basic x"}})
	assert.Equal(t, 200, authed.StatusCode)

	img := do(t, c, http.MethodGet, srv.URL("/hello.jpg"), nil)
	assert.Equal(t, 200, img.StatusCode)
	assert.Equal(t, "image/jpeg", img.Header.Get("Content-Type"))

	assert.Equal(t, 200, do(t, c, http.MethodGet, srv.URL("/foo(a=b).aspx"), nil).StatusCode)
}

func TestLaterAndCounts(t *testing.T) {
	srv := New()
	defer srv.Close()
	c := noRedirect()

	first := do(t, c, http.MethodGet, srv.URL("/later"), nil)
	assert.Equal(t, 429, first.StatusCode)
	assert.Equal(t, "1", first.Header.Get("Retry-After"))

	second := do(t, c, http.MethodGet, srv.URL("/later"), nil)
	assert.Equal(t, 429, second.StatusCode)
	assert.Empty(t, second.Header.Get("Retry-After"), "fallback: Retry-After omitted on a later failure")

	third := do(t, c, http.MethodGet, srv.URL("/later"), nil)
	assert.Equal(t, 200, third.StatusCode)

	assert.Equal(t, 3, srv.Requests("/later"))
	assert.Equal(t, 0, srv.Requests("/never-hit"))
}

func TestSetLaterFailures(t *testing.T) {
	srv := New()
	defer srv.Close()
	srv.SetLaterFailures(1)
	c := noRedirect()

	assert.Equal(t, 429, do(t, c, http.MethodGet, srv.URL("/later"), nil).StatusCode)
	assert.Equal(t, 200, do(t, c, http.MethodGet, srv.URL("/later"), nil).StatusCode)
}
