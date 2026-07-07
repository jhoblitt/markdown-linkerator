// Package pipeline streams markdown discovery, parsing, and link checking
// concurrently. Pacing (per-host rate limiting) is decoupled from concurrency
// (a bounded executor pool): an executor slot is held only during a real
// request, never during a rate-wait. The channel graph is an acyclic DAG and
// each channel has exactly one close-owner.
package pipeline

import (
	"context"
	"io"
	"os"

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
}

// New builds a Pipeline. The cache and collector are provided by the engine so
// it can persist the cache and render the report around the run.
func New(cfg config.Resolved, coll *report.Collector, c *cache.Cache) *Pipeline {
	return &Pipeline{
		cfg:   cfg,
		http:  checker.NewHTTPChecker(cfg),
		reg:   ratelimit.NewRegistry(cfg),
		cache: c,
		coll:  coll,
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
		p.coll.Add(fatalResult(src.path, err))
		return
	}

	fl, err := extract.ParseFile(src.path, data, p.cfg)
	if err != nil {
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
		p.coll.Add(checker.CheckFile(t))
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
			_ = send(ctx, jobsCh, job)
		}
	}
}

func (p *Pipeline) execute(ctx context.Context, job *model.CheckJob, d *dedup) {
	res := p.http.Check(ctx, job.Sample)
	hs := p.reg.Host(job.Host)
	hs.Record(res.Retries)
	switch {
	case res.Saw429:
		hs.Penalize429(res.RetryAfter)
	case res.State == model.StateAlive:
		hs.OnSuccess()
	}
	p.cache.Put(job.Key, res)

	for _, occ := range d.complete(job.Key, res) {
		p.coll.Add(resultFor(occ, res))
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
