package report

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

// liveProgress writes progress to a side stream (typically stderr) while the run
// is in flight, so a paced multi-minute check is visibly working rather than
// silent. It is only ever touched under the Collector's mutex.
type liveProgress struct {
	out         io.Writer
	start       time.Time
	last        time.Time
	checked     int
	dead        int
	inflight    int          // network checks enqueued but not yet complete (the backlog)
	activeList  []activeSnap // every in-progress check, oldest first
	hostBacklog []hostCount  // per-host in-flight, busiest first
}

// hostCount is a host and its in-flight network-check count, for the heartbeat's
// backlog line.
type hostCount struct {
	host string
	n    int
}

// activeSnap is a point-in-time view of one in-progress check for the heartbeat.
type activeSnap struct {
	url   string
	since time.Time
	note  string
}

// heartbeatTick is how often the time-based heartbeat fires even when no results
// are arriving (e.g. every worker stalled in retry/backoff). Kept below 15s so a
// paced run is never silent longer than that.
const heartbeatTick = 10 * time.Second

const heartbeatInterval = 750 * time.Millisecond

func (l *liveProgress) init(out io.Writer) {
	l.out = out
	l.start = time.Now()
	l.last = l.start
}

// reuseTag labels a result the user did not pay a network round-trip for.
func reuseTag(r model.Result) string {
	switch {
	case r.FromCache:
		return " (cached)"
	case r.Reused:
		return " (reused)"
	default:
		return ""
	}
}

// streamLine prints one completed link as it happens (Verbose mode).
func (l *liveProgress) streamLine(p palette, r model.Result) {
	line := "  " + colorGlyph(p, r.State) + " " + linkDisplay(r.Target) + fmt.Sprintf(" → Status: %d", r.StatusCode) + reuseTag(r)
	if d := detailText(r); d != "" {
		line += " — " + d
	}
	fmt.Fprintln(l.out, line)
}

// activeAgeThreshold is how long a check must run before it is named in the
// heartbeat (so normal fast checks are not listed).
const activeAgeThreshold = 3 * time.Second

// heartbeat prints a throttled status: a one-line counter plus, when checks have
// been running a while, a line per in-flight check naming the URL and why it is
// still running (e.g. a 429 backoff), so a stall is legible rather than a frozen
// counter.
func (l *liveProgress) heartbeat(force bool) {
	now := time.Now()
	if !force && now.Sub(l.last) < heartbeatInterval {
		return
	}
	l.last = now
	elapsed := now.Sub(l.start).Round(time.Second)
	fmt.Fprintf(l.out, "checking… %d checked · %d in-flight · %d dead · %s elapsed\n", l.checked, l.inflight, l.dead, elapsed)
	// Show where the in-flight work is concentrated — the busiest hosts, which
	// are the rate-limited bottlenecks everything else is queued behind.
	if l.inflight > 0 && len(l.hostBacklog) > 0 {
		const topN = 6
		var b strings.Builder
		b.WriteString("  · in-flight by host: ")
		shown := len(l.hostBacklog)
		if shown > topN {
			shown = topN
		}
		for i := 0; i < shown; i++ {
			if i > 0 {
				b.WriteString(" · ")
			}
			fmt.Fprintf(&b, "%s %d", l.hostBacklog[i].host, l.hostBacklog[i].n)
		}
		if rest := len(l.hostBacklog) - shown; rest > 0 {
			fmt.Fprintf(&b, " (+%d more)", rest)
		}
		fmt.Fprintln(l.out, b.String())
	}
	for _, a := range l.activeList {
		age := now.Sub(a.since).Round(time.Second)
		if age < activeAgeThreshold {
			continue
		}
		line := fmt.Sprintf("  · waiting on %s (%s", a.url, age)
		if a.note != "" {
			line += "; " + a.note
		}
		line += ")"
		fmt.Fprintln(l.out, line)
	}
}
