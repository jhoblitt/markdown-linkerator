// Package testserver provides a deterministic httptest server that reproduces
// the Express fixtures used by markdown-link-check's test suite. It backs the
// checker unit tests and the e2e suite so link checks run against known routes
// with no external network.
package testserver

import (
	"net/http"
	"net/http/httptest"
	"sync"
)

// Server wraps an httptest.Server with per-path request accounting so tests can
// assert retry and dedup behavior.
type Server struct {
	*httptest.Server

	mu            sync.Mutex
	counts        map[string]int
	laterFailures int // number of leading 429s /later returns before 200
}

// New starts a server serving the fixture routes. The /later route fails
// laterFailures (default 2) times with 429 before succeeding.
func New() *Server {
	s := &Server{counts: map[string]int{}, laterFailures: 2}

	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", s.wrap(handleOK))
	mux.HandleFunc("/nohead", s.wrap(handleNoHead))
	mux.HandleFunc("/partial", s.wrap(handlePartial))
	mux.HandleFunc("/later", s.wrap(s.handleLater))
	mux.HandleFunc("/toomany", s.wrap(handleTooMany)) // always 429, no Retry-After
	mux.HandleFunc("/foo/redirect", s.wrap(handleRedirect("/foo/bar")))
	mux.HandleFunc("/foo/bar", s.wrap(handleOK))
	mux.HandleFunc("/redirect-with-body-in-head", s.wrap(handleRedirectWithBody("/")))
	mux.HandleFunc("/loop", s.wrap(handleRedirect("/loop")))
	mux.HandleFunc("/basic-auth", s.wrap(handleBasicAuth))
	mux.HandleFunc("/hello.jpg", s.wrap(handleImage))
	mux.HandleFunc("/foo(a=b).aspx", s.wrap(handleOK))

	s.Server = httptest.NewServer(mux)
	return s
}

// URL returns the absolute URL for path (which must begin with "/").
func (s *Server) URL(path string) string { return s.Server.URL + path }

// Requests returns how many requests have hit path, counted by the actual
// request path (so redirect targets and loop iterations are included).
func (s *Server) Requests(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[path]
}

// SetLaterFailures configures how many leading 429s /later returns before 200.
// Call it before the first request.
func (s *Server) SetLaterFailures(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.laterFailures = n
}

func (s *Server) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.counts[r.URL.Path]++
		s.mu.Unlock()
		h(w, r)
	}
}

func handleOK(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func handleTooMany(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusTooManyRequests)
}

func handleNoHead(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func handlePartial(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusPartialContent)
}

func (s *Server) handleLater(w http.ResponseWriter, _ *http.Request) {
	// wrap already counted this request, so Requests is the 1-based attempt.
	n := s.Requests("/later")
	s.mu.Lock()
	fail := s.laterFailures
	s.mu.Unlock()

	if n <= fail {
		// Send Retry-After only on the first failure; omit it on the rest so a
		// retry exercises the exponential fallback path.
		if n == 1 {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func handleRedirect(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, to, http.StatusFound)
	}
}

// handleRedirectWithBody redirects but also writes a body, even for HEAD, to
// exercise the checker draining redirect responses.
func handleRedirectWithBody(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", to)
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte("moved along, nothing to see here"))
	}
}

func handleBasicAuth(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func handleImage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte{0xff, 0xd8, 0xff, 0xe0})
}
