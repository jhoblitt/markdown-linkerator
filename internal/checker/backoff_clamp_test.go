package checker

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
)

// TestRetryAfterNotFlooredToMin guards the fix for a 429 storm being slow: a
// small Retry-After must be honored as sent, never floored up to RetryWaitMin
// (FallbackRetryDelay). A server saying "retry after 1s" must wait ~1s even
// when the fallback delay is 30s.
func TestRetryAfterNotFlooredToMin(t *testing.T) {
	c := NewHTTPChecker(config.Resolved{
		FallbackRetryDelay: 30 * time.Second,
		BackoffMax:         2 * time.Minute,
		RetryOn429:         true,
	})
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"1"}},
	}
	d := c.backoff(30*time.Second, 2*time.Minute, 0, resp)
	assert.Equal(t, time.Second, d, "Retry-After must be honored, not floored to RetryWaitMin")
}

// TestExpJitterBackoffRespectsCeiling guards against a floor (RetryWaitMin /
// FallbackRetryDelay) that exceeds the ceiling (RetryWaitMax / BackoffMax): the
// wait must never exceed the ceiling, else a single retry could park for the
// full fallback delay despite a smaller retry-max-wait.
func TestExpJitterBackoffRespectsCeiling(t *testing.T) {
	const ceiling = 2 * time.Second
	for attempt := 0; attempt < 8; attempt++ {
		w := expJitterBackoff(30*time.Second, ceiling, attempt)
		assert.LessOrEqualf(t, w, ceiling, "attempt %d exceeded ceiling", attempt)
	}
}
