// Package pipeline streams markdown discovery, parsing, and link checking
// concurrently. Pacing (per-host rate limiting) is decoupled from concurrency
// (a bounded executor pool): an executor slot is held only during a real
// request, never during a rate-wait. The channel graph is an acyclic DAG and
// each channel has exactly one close-owner.
package pipeline

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/jhoblitt/markdown-linkerator/internal/cache"
	"github.com/jhoblitt/markdown-linkerator/internal/checker"
	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/extract"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
	"github.com/jhoblitt/markdown-linkerator/internal/ratelimit"
	"github.com/jhoblitt/markdown-linkerator/internal/report"
)

// Pipeline owns the checking engine's collaborators for one run.
type Pipeline struct {
	cfg   config.Resolved
	http  *checker.HTTPChecker
	reg   *ratelimit.Registry
	cache *cache.Cache
	coll  *report.Collector

	anchorMu    sync.Mutex
	anchorCache map[string]map[string]bool // referenced markdown file -> anchor set

	srcErrors atomic.Int64 // source files that could not be read/parsed
}

// SourceErrors reports how many input files could not be read or parsed. These
// are run failures (unchecked documentation), not link failures.
func (p *Pipeline) SourceErrors() int64 { return p.srcErrors.Load() }

// New builds a Pipeline. The cache and collector are provided by the engine so
// it can persist the cache and render the report around the run.
func New(cfg config.Resolved, coll *report.Collector, c *cache.Cache) *Pipeline {
	http := checker.NewHTTPChecker(cfg)
	// Surface retry/backoff waits so a stalled heartbeat reads as
	// "waiting on X: HTTP 429, retrying in 30s" rather than a frozen counter.
	http.OnRetry = func(url string, attempt, code int, wait time.Duration) {
		coll.NetStatus(url, fmt.Sprintf("HTTP %d, retrying in %s (attempt %d)", code, wait.Round(time.Second), attempt+1))
	}
	return &Pipeline{
		cfg:         cfg,
		http:        http,
		reg:         ratelimit.NewRegistry(cfg),
		cache:       c,
		coll:        coll,
		anchorCache: map[string]map[string]bool{},
	}
}

// Registry exposes the per-host limiter/stats registry for reporting.
func (p *Pipeline) Registry() *ratelimit.Registry { return p.reg }

// Run executes the streaming pipeline over the given inputs (files, directories,
// globs, or bare URLs; empty means stdin). It returns only a fatal error
// (context cancellation, unreadable crawl root); dead links are collected, not
// returned.
func (p *Pipeline) Run(ctx context.Context, inputs []string) error {
	g, ctx := errgroup.WithContext(ctx)
	filesCh := make(chan srcItem, 64)
	jobsCh := make(chan *model.CheckJob, 256)
	readyCh := make(chan *model.CheckJob, 2*max(1, p.cfg.URLWorkers))
	d := newDedup(p.cache)
	pacers := newPacerSet(ctx, p.reg, readyCh)

	// Stage 1: crawl inputs → filesCh.
	g.Go(func() error {
		defer close(filesCh)
		return crawl(ctx, inputs, filesCh)
	})

	// Stage 2: parse workers → jobsCh (and offline/cached results straight to
	// the collector). The consumer goroutine closes jobsCh once parsing is done.
	g.Go(func() error {
		var pg errgroup.Group
		pg.SetLimit(max(1, p.cfg.ParseWorkers))
		for src := range filesCh {
			pg.Go(func() error {
				p.parseSource(ctx, src, d, jobsCh)
				return nil
			})
		}
		err := pg.Wait()
		close(jobsCh)
		return err
	})

	// Stage 3: dispatcher → per-host pacers → readyCh.
	g.Go(func() error {
		for job := range jobsCh {
			pacers.dispatch(job)
		}
		pacers.closeAndWait()
		close(readyCh)
		return nil
	})

	// Stage 4: executor pool performs the network checks.
	g.Go(func() error {
		var eg errgroup.Group
		eg.SetLimit(max(1, p.cfg.URLWorkers))
		for job := range readyCh {
			eg.Go(func() error {
				p.execute(ctx, job, d)
				return nil
			})
		}
		return eg.Wait()
	})

	return g.Wait()
}

func (p *Pipeline) parseSource(ctx context.Context, src srcItem, d *dedup, jobsCh chan<- *model.CheckJob) {
	if src.url != "" {
		t := model.Target{Raw: src.url, URL: src.url, Kind: model.KindHTTP, SourceFile: src.path, Line: 1}
		p.handleTarget(ctx, t, nil, d, jobsCh)
		return
	}

	var (
		data []byte
		err  error
	)
	if src.stdin {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(src.path)
	}
	if err != nil {
		p.srcErrors.Add(1)
		p.coll.Add(fatalResult(src.path, err))
		return
	}

	fl, err := extract.ParseFile(src.path, data, p.cfg)
	if err != nil {
		p.srcErrors.Add(1)
		p.coll.Add(fatalResult(src.path, err))
		return
	}
	for _, t := range fl.Targets {
		p.handleTarget(ctx, t, fl.Anchors, d, jobsCh)
	}
}

func (p *Pipeline) handleTarget(ctx context.Context, t model.Target, anchors map[string]bool, d *dedup, jobsCh chan<- *model.CheckJob) {
	// Ignore patterns target the link as written (e.g. "^http://host:port",
	// "^quickstart.md$"), so match the raw link as well as the resolved URL —
	// the latter is a filesystem path for file links and would never match a
	// URL-shaped pattern.
	if checker.IsIgnored(t.Raw, p.cfg) || checker.IsIgnored(t.URL, p.cfg) {
		p.coll.Add(model.Result{Target: t, State: model.StateIgnored})
		return
	}
	switch t.Kind {
	case model.KindFileRel:
		p.coll.Add(p.checkFileTarget(t))
	case model.KindHashLocal:
		p.coll.Add(checker.CheckHash(t, anchors))
	case model.KindMailto:
		p.coll.Add(checker.CheckMailto(ctx, t, p.cfg))
	case model.KindHTTP:
		if !p.cfg.CheckExternal {
			p.coll.Add(model.Result{Target: t, State: model.StateIgnored, Detail: "external checks disabled"})
			return
		}
		key := model.NormalizeKey(t.URL)
		job, emit := d.add(key, t)
		for _, r := range emit {
			p.coll.Add(r)
		}
		if job != nil {
			p.coll.NetEnqueue()
			_ = send(ctx, jobsCh, job)
		}
	}
}

// checkFileTarget checks a relative/file link, additionally validating a
// #fragment against the referenced markdown file's anchors so a broken
// cross-file section link (./guide.md#missing) is reported dead rather than
// passing on mere file existence.
func (p *Pipeline) checkFileTarget(t model.Target) model.Result {
	res := checker.CheckFile(t)
	if !p.cfg.CheckFragments || res.State != model.StateAlive || t.Fragment == "" || !isMarkdown(t.URL) {
		return res
	}
	anchors, err := p.fileAnchors(t.URL)
	if err != nil {
		return res // file exists (per CheckFile) but is unreadable now; keep alive
	}
	if hr := checker.CheckHash(t, anchors); hr.State != model.StateAlive {
		return model.Result{
			Target:     t,
			State:      model.StateDead,
			StatusCode: 404,
			Detail:     "file exists but anchor #" + t.Fragment + " not found",
		}
	}
	return res
}

// fileAnchors returns the anchor set for a referenced markdown file, parsing it
// at most once per run.
func (p *Pipeline) fileAnchors(path string) (map[string]bool, error) {
	p.anchorMu.Lock()
	a, ok := p.anchorCache[path]
	p.anchorMu.Unlock()
	if ok {
		return a, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	a = extract.Anchors(data)
	p.anchorMu.Lock()
	p.anchorCache[path] = a
	p.anchorMu.Unlock()
	return a, nil
}

func (p *Pipeline) execute(ctx context.Context, job *model.CheckJob, d *dedup) {
	p.coll.NetStart(job.Sample.URL)
	res := p.http.Check(ctx, job.Sample)
	p.coll.NetComplete(job.Sample.URL)
	hs := p.reg.Host(job.Host)
	hs.Record(res.Retries)
	switch {
	case res.Saw429:
		hs.Penalize429(res.RetryAfter)
	case res.State == model.StateAlive:
		hs.OnSuccess()
	}
	p.cache.Put(job.Key, res)

	// The originating occurrence (index 0) is the real check; the rest reused it.
	for i, occ := range d.complete(job.Key, res) {
		p.coll.Add(resultFor(occ, res, i > 0))
	}
}

func fatalResult(path string, err error) model.Result {
	return model.Result{
		Target: model.Target{URL: path, SourceFile: path},
		State:  model.StateError,
		Detail: err.Error(),
		Err:    nil, // reported as an errored link, not a pipeline-fatal error
	}
}
