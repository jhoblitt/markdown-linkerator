package report

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

// liveProgress writes progress to a side stream (typically stderr) while the run
// is in flight, so a paced multi-minute check is visibly working rather than
// silent. It is only ever touched under the Collector's mutex.
type liveProgress struct {
	out      io.Writer
	start    time.Time
	last     time.Time
	checked  int
	dead     int
	inflight int64 // network checks enqueued but not yet complete (the pending backlog)
	tty      bool
	shown    bool // whether a \r heartbeat line is currently on screen (TTY)
}

// heartbeatTick is how often the time-based heartbeat fires even when no results
// are arriving (e.g. every worker stalled in retry/backoff). Kept below 15s so a
// paced run is never silent longer than that.
const heartbeatTick = 10 * time.Second

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

func (l *liveProgress) init(out io.Writer) {
	l.out = out
	l.start = time.Now()
	l.last = l.start
	l.tty = isTerminal(out)
}

// streamLine prints one completed link as it happens (Verbose mode).
func (l *liveProgress) streamLine(p palette, r model.Result) {
	line := "  " + colorGlyph(p, r.State) + " " + linkDisplay(r.Target) + fmt.Sprintf(" → Status: %d", r.StatusCode) + reuseTag(r)
	if d := detailText(r); d != "" {
		line += " — " + d
	}
	fmt.Fprintln(l.out, line)
}

const heartbeatInterval = 750 * time.Millisecond

// heartbeat prints a throttled one-line status. On a TTY it overwrites in place
// with \r; elsewhere (CI logs) it prints a new line, but only every few seconds
// so it does not flood the log.
func (l *liveProgress) heartbeat(force bool) {
	now := time.Now()
	interval := heartbeatInterval
	if !l.tty {
		interval = 5 * time.Second
	}
	if !force && now.Sub(l.last) < interval {
		return
	}
	l.last = now
	elapsed := now.Sub(l.start).Round(time.Second)
	msg := fmt.Sprintf("checking… %d checked · %d in-flight · %d dead · %s elapsed", l.checked, l.inflight, l.dead, elapsed)
	if l.tty {
		fmt.Fprintf(l.out, "\r\033[K%s", msg)
		l.shown = true
	} else {
		fmt.Fprintln(l.out, msg)
	}
}

// clear removes an in-place heartbeat line before the final report renders.
func (l *liveProgress) clear() {
	if l.tty && l.shown {
		fmt.Fprint(l.out, "\r\033[K")
		l.shown = false
	}
}

// isTerminal reports whether w is a character device (a TTY), without pulling in
// golang.org/x/term.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
