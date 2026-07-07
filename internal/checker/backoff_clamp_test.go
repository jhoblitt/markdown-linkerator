package checker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

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
