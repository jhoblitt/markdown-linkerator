package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
)

// TestHeartbeatShowsHostBacklog guards that the heartbeat surfaces where the
// in-flight work is queued — the busiest hosts first — so a large paced backlog
// is legible instead of just a number.
func TestHeartbeatShowsHostBacklog(t *testing.T) {
	var buf bytes.Buffer
	c := NewCollector(config.Resolved{}, Options{ProgressOut: &buf})

	for i := 0; i < 5; i++ {
		c.NetEnqueue("github.com")
	}
	for i := 0; i < 3; i++ {
		c.NetEnqueue("docs.ceph.com")
	}
	c.NetEnqueue("x.example.com")

	c.mu.Lock()
	c.updateGauges()
	c.live.heartbeat(true)
	c.mu.Unlock()

	out := buf.String()
	assert.Contains(t, out, "9 in-flight")
	assert.Contains(t, out, "in-flight by host:")
	assert.Contains(t, out, "github.com 5")
	assert.Contains(t, out, "docs.ceph.com 3")
	assert.Less(t, strings.Index(out, "github.com 5"), strings.Index(out, "docs.ceph.com 3"),
		"busiest host must come first")

	// Completing a check drains that host's count.
	c.NetComplete("", "github.com")
	c.mu.Lock()
	c.updateGauges()
	c.mu.Unlock()
	got := 0
	for _, hc := range c.live.hostBacklog {
		if hc.host == "github.com" {
			got = hc.n
		}
	}
	assert.Equal(t, 4, got, "github.com backlog should drop to 4 after one completion")
}
