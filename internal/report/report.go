// Package report aggregates link-check results from concurrent workers and
// renders a deterministic, CI-diffable summary at the end of a run. It also
// computes the process exit code: a dead link (or an errored one when the
// config opts in) fails the run; ignored links never do.
package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

// Options controls how a Collector renders its report.
type Options struct {
	Quiet    bool      // print only dead/errored links, no summary chrome
	Verbose  bool      // append status codes and detail to each link line
	Progress bool      // force the live progress heartbeat (on by default unless quiet/json)
	Format   string    // "text" (default) or "json"
	Out      io.Writer // render destination; nil is treated as io.Discard
	// ProgressOut receives live progress (per-link under Verbose, else a throttled
	// heartbeat) during the run so a long paced run is visibly alive rather than
	// silent. Typically os.Stderr; nil disables live output.
	ProgressOut io.Writer
	NoColor     bool // disable ANSI color (the NO_COLOR env var is also honored)
}

// Summary is the machine-readable outcome of a run, returned by Finish.
type Summary struct {
	Total    int
	Alive    int
	Dead     int
	Ignored  int
	Errored  int
	Cached   int              // results served from the on-disk cache (no network trip)
	Reused   int              // results reused from an earlier occurrence in this run
	ExitCode int              // 1 if Dead>0 or (ErrorFailsRun && Errored>0), else 0
	Results  []model.Result   // sorted by SourceFile, then Line, then URL
	Hosts    []model.HostStat // sorted by Host
}

// Collector buffers results during a streaming run and renders the final
// report. The zero value is not usable; construct one with NewCollector.
type Collector struct {
	cfg  config.Resolved
	opts Options

	mu      sync.Mutex
	results []model.Result

	// Network-check gauges for the progress heartbeat, all under mu: enqueued
	// counts dispatched jobs, netDone finished ones, active tracks the URLs
	// currently being checked (so a backoff stall names what it is waiting on),
	// and hostInflight is the per-host in-flight count so the heartbeat can show
	// where the backlog is (e.g. many URLs queued behind one rate-limited host).
	enqueued     int
	netDone      int
	active       map[string]*activeCheck
	hostInflight map[string]int

	live      liveProgress // live stderr feedback during the run
	livePal   palette
	liveOn    bool
	streaming bool // Verbose: stream each link as it completes
	stopTick  chan struct{}
	tickDone  chan struct{}
}

// NetEnqueue records a dispatched network check (queued or in progress).
func (c *Collector) NetEnqueue(host string) {
	c.mu.Lock()
	c.enqueued++
	c.hostInflight[host]++
	c.mu.Unlock()
}

// activeCheck is an in-progress network check: when it started and a note about
// its current state (e.g. a retry/backoff), for the progress heartbeat.
type activeCheck struct {
	since time.Time
	note  string
}

// NetStart records that a URL's network check has begun, so a stalled run can
// report which URLs it is waiting on.
func (c *Collector) NetStart(url string) {
	c.mu.Lock()
	c.active[url] = &activeCheck{since: time.Now()}
	c.mu.Unlock()
}

// NetStatus annotates an in-progress check with why it is still running (e.g.
// waiting out a 429 backoff), surfaced by the heartbeat.
func (c *Collector) NetStatus(url, note string) {
	c.mu.Lock()
	if a := c.active[url]; a != nil {
		a.note = note
	}
	c.mu.Unlock()
}

// NetComplete records a finished network check.
func (c *Collector) NetComplete(url, host string) {
	c.mu.Lock()
	c.netDone++
	delete(c.active, url)
	if c.hostInflight[host]--; c.hostInflight[host] <= 0 {
		delete(c.hostInflight, host)
	}
	c.mu.Unlock()
}

// updateGauges refreshes the heartbeat's in-flight count and the snapshot of
// active checks (oldest first). Caller holds mu.
func (c *Collector) updateGauges() {
	c.live.inflight = c.enqueued - c.netDone
	c.live.activeList = c.live.activeList[:0]
	for u, a := range c.active {
		c.live.activeList = append(c.live.activeList, activeSnap{url: u, since: a.since, note: a.note})
	}
	sort.Slice(c.live.activeList, func(i, j int) bool {
		return c.live.activeList[i].since.Before(c.live.activeList[j].since)
	})
	// Per-host in-flight, busiest first, so the heartbeat shows where the backlog
	// is concentrated (the rate-limited hosts everything is queued behind).
	c.live.hostBacklog = c.live.hostBacklog[:0]
	for h, n := range c.hostInflight {
		c.live.hostBacklog = append(c.live.hostBacklog, hostCount{host: h, n: n})
	}
	sort.Slice(c.live.hostBacklog, func(i, j int) bool {
		if c.live.hostBacklog[i].n != c.live.hostBacklog[j].n {
			return c.live.hostBacklog[i].n > c.live.hostBacklog[j].n
		}
		return c.live.hostBacklog[i].host < c.live.hostBacklog[j].host
	})
}

// NewCollector returns a Collector that renders to opts.Out and uses cfg for
// the exit-code policy (ErrorFailsRun).
func NewCollector(cfg config.Resolved, opts Options) *Collector {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	c := &Collector{cfg: cfg, opts: opts, active: map[string]*activeCheck{}, hostInflight: map[string]int{}}
	// Live output is enabled unless quiet or a machine format; Verbose streams
	// each link, otherwise a throttled heartbeat keeps a long paced run from
	// looking hung.
	c.liveOn = opts.ProgressOut != nil && !opts.Quiet && !machineFormat(opts.Format)
	c.streaming = opts.Verbose
	if c.liveOn {
		c.livePal = newPalette(opts.NoColor)
		c.live.init(opts.ProgressOut)
	}
	return c
}

func machineFormat(f string) bool {
	switch f {
	case "json", "yaml", "yml":
		return true
	default:
		return false
	}
}

// StartProgress launches the time-based heartbeat so status is emitted at least
// every heartbeat interval even while every worker is stalled in retry/backoff
// (when no results arrive to drive Add). The engine calls it before the run;
// Finish stops it.
func (c *Collector) StartProgress() {
	if !c.liveOn {
		return
	}
	c.stopTick = make(chan struct{})
	c.tickDone = make(chan struct{})
	go func() {
		defer close(c.tickDone)
		t := time.NewTicker(heartbeatTick)
		defer t.Stop()
		for {
			select {
			case <-c.stopTick:
				return
			case <-t.C:
				c.mu.Lock()
				c.updateGauges()
				c.live.heartbeat(true)
				c.mu.Unlock()
			}
		}
	}()
}

func (c *Collector) stopProgress() {
	if c.stopTick == nil {
		return
	}
	close(c.stopTick)
	<-c.tickDone
	c.stopTick = nil
}

// Add records one result. It is safe for concurrent use by many workers. It
// buffers the result for the final sorted report and, when live output is
// enabled, emits progress to ProgressOut.
func (c *Collector) Add(r model.Result) {
	c.mu.Lock()
	c.results = append(c.results, r)
	if r.State == model.StateDead {
		c.live.dead++
	}
	c.live.checked++
	if c.liveOn {
		c.updateGauges()
		if c.streaming {
			c.live.streamLine(c.livePal, r)
		} else {
			c.live.heartbeat(false)
		}
	}
	c.mu.Unlock()
}

// Finish sorts the buffered results, renders the report to Options.Out, and
// returns the Summary. hostStats is rendered as a per-host section (text,
// non-quiet only), sorted by request count (busiest first).
func (c *Collector) Finish(hostStats []model.HostStat) Summary {
	c.mu.Lock()
	results := make([]model.Result, len(c.results))
	copy(results, c.results)
	c.mu.Unlock()

	sortResults(results)

	hosts := make([]model.HostStat, len(hostStats))
	copy(hosts, hostStats)
	// Busiest hosts first (the rate-limit risks); host name is a deterministic
	// tie-break so the report stays CI-diffable.
	sort.Slice(hosts, func(i, j int) bool {
		if hosts[i].Requests != hosts[j].Requests {
			return hosts[i].Requests > hosts[j].Requests
		}
		return hosts[i].Host < hosts[j].Host
	})

	s := summarize(results, hosts, c.cfg.ErrorFailsRun)

	if c.liveOn {
		c.stopProgress()
	}

	switch c.opts.Format {
	case "json":
		c.renderJSON(s)
	case "yaml", "yml":
		c.renderYAML(s)
	default:
		c.renderText(s)
	}
	return s
}

func summarize(results []model.Result, hosts []model.HostStat, errorFails bool) Summary {
	s := Summary{Total: len(results), Results: results, Hosts: hosts}
	for _, r := range results {
		switch r.State {
		case model.StateAlive:
			s.Alive++
		case model.StateDead:
			s.Dead++
		case model.StateIgnored:
			s.Ignored++
		case model.StateError:
			s.Errored++
		}
		switch {
		case r.FromCache:
			s.Cached++
		case r.Reused:
			s.Reused++
		}
	}
	if s.Dead > 0 || (errorFails && s.Errored > 0) {
		s.ExitCode = 1
	}
	return s
}

func sortResults(rs []model.Result) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i].Target, rs[j].Target
		if a.SourceFile != b.SourceFile {
			return a.SourceFile < b.SourceFile
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.URL < b.URL
	})
}

func (c *Collector) renderText(s Summary) {
	p := newPalette(c.opts.NoColor)
	w := c.opts.Out

	if s.Total == 0 {
		if !c.opts.Quiet {
			fmt.Fprintln(w, "No hyperlinks found!")
		}
		return
	}

	// When Verbose streamed each link live, re-listing them all here would just
	// duplicate that output; show the summary + failures digest instead.
	if !c.liveOn || !c.streaming {
		c.renderFileGroups(w, p, s.Results)
	}

	if c.opts.Quiet {
		return
	}

	renderHosts(w, p, s.Hosts)
	checked := fmt.Sprintf("  %d link(s) checked", s.Total)
	if s.Cached > 0 {
		checked += fmt.Sprintf(" · %d from cache", s.Cached)
	}
	if s.Reused > 0 {
		checked += fmt.Sprintf(" · %d reused", s.Reused)
	}
	fmt.Fprintln(w, checked+".")
	if s.Dead > 0 {
		renderDeadBlock(w, p, s)
	}
}

// renderFileGroups prints links grouped by source file. In quiet mode a file
// is printed only when it has at least one failure, and only its failing links.
func (c *Collector) renderFileGroups(w io.Writer, p palette, results []model.Result) {
	for _, g := range groupByFile(results) {
		lines := g
		if c.opts.Quiet {
			if lines = failuresOnly(g); len(lines) == 0 {
				continue
			}
		}
		fmt.Fprintln(w, p.cyan("FILE: "+lines[0].Target.SourceFile))
		for _, r := range lines {
			c.renderLink(w, p, r)
		}
	}
}

// linkDisplay renders a target for output, re-attaching a fragment to file
// links (whose URL is the resolved path) so a failed cross-file anchor shows
// which anchor, e.g. "guide.md#missing" rather than a bare "guide.md".
func linkDisplay(t model.Target) string {
	if t.Fragment != "" && !strings.Contains(t.URL, "#") {
		return t.URL + "#" + t.Fragment
	}
	return t.URL
}

func (c *Collector) renderLink(w io.Writer, p palette, r model.Result) {
	line := "  " + colorGlyph(p, r.State) + " " + linkDisplay(r.Target) + reuseTag(r)
	if c.opts.Verbose {
		line += fmt.Sprintf(" → Status: %d", r.StatusCode)
		if d := detailText(r); d != "" {
			line += " — " + d
		}
	}
	fmt.Fprintln(w, line)
}

func renderHosts(w io.Writer, p palette, hosts []model.HostStat) {
	if len(hosts) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, p.cyan("Hosts:"))
	for _, h := range hosts {
		fmt.Fprintf(w, "  %s  %d requests  %.2f req/s  %d retries  %d unresolved-429\n",
			h.Host, h.Requests, h.ObservedRPS, h.Retries, h.N429)
	}
	fmt.Fprintln(w)
}

func renderDeadBlock(w io.Writer, p palette, s Summary) {
	fmt.Fprintln(w, p.red(fmt.Sprintf("ERROR: %d dead link(s) found!", s.Dead)))
	for _, r := range s.Results {
		if r.State != model.StateDead {
			continue
		}
		fmt.Fprintf(w, "  %s %s → Status: %d\n", colorGlyph(p, r.State), linkDisplay(r.Target), r.StatusCode)
	}
}

func groupByFile(results []model.Result) [][]model.Result {
	var groups [][]model.Result
	for _, r := range results {
		if n := len(groups); n > 0 && groups[n-1][0].Target.SourceFile == r.Target.SourceFile {
			groups[n-1] = append(groups[n-1], r)
			continue
		}
		groups = append(groups, []model.Result{r})
	}
	return groups
}

func failuresOnly(rs []model.Result) []model.Result {
	var out []model.Result
	for _, r := range rs {
		if r.State == model.StateDead || r.State == model.StateError {
			out = append(out, r)
		}
	}
	return out
}

func colorGlyph(p palette, s model.State) string {
	g := s.Glyph()
	switch s {
	case model.StateAlive:
		return p.green(g)
	case model.StateDead:
		return p.red(g)
	case model.StateError:
		return p.yellow(g)
	default:
		return p.dim(g)
	}
}

// detailText is the human-readable context for a result, combining the
// explicit Detail with any fatal Err. Used by verbose text and json output.
func detailText(r model.Result) string {
	switch {
	case r.Detail != "" && r.Err != nil:
		return r.Detail + ": " + r.Err.Error()
	case r.Detail != "":
		return r.Detail
	case r.Err != nil:
		return r.Err.Error()
	default:
		return ""
	}
}
