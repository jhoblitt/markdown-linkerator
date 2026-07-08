// Package engine is the pure orchestration façade: it wires the cache,
// collector, and pipeline for one run and returns a report.Summary. It performs
// no flag parsing and never calls os.Exit, so the CLI, unit tests, and the e2e
// harness all drive the same Run.
package engine

import (
	"context"
	"fmt"

	"github.com/jhoblitt/markdown-linkerator/internal/cache"
	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/pipeline"
	"github.com/jhoblitt/markdown-linkerator/internal/report"
)

// Run checks all links reachable from inputs and returns the run summary.
// A non-nil error is fatal (bad crawl root, context cancellation); dead links
// are reported in the Summary, never as an error. The URL cache is saved on
// every non-crash return — including interrupted runs — so CI persists what it
// checked.
func Run(ctx context.Context, cfg config.Resolved, inputs []string, rep report.Options) (*report.Summary, error) {
	c, err := cache.New(cfg.Cache.Path, cfg.Cache.TTL, cfg.Cache.Enabled, cfg.CacheFingerprint())
	if err != nil {
		return nil, fmt.Errorf("open cache: %w", err)
	}

	coll := report.NewCollector(cfg, rep)
	coll.StartProgress()
	p := pipeline.New(cfg, coll, c)

	runErr := p.Run(ctx, inputs)

	// Persist definitive results regardless of how the run ended.
	saveErr := c.Save()

	summary := coll.Finish(p.Registry().AllStats())
	// The pipeline collects canceled checks as errored results rather than
	// aborting, so surface a deadline/cancellation here — a --max-time run that
	// did not finish must not report success.
	if runErr == nil {
		runErr = ctx.Err()
	}
	// Unreadable/unparseable input files are a run failure (documentation went
	// unchecked), independent of --fail-on-error, so they never exit green.
	if runErr == nil && p.SourceErrors() > 0 {
		runErr = fmt.Errorf("%d source file(s) could not be read", p.SourceErrors())
	}
	if runErr != nil {
		return &summary, runErr
	}
	if saveErr != nil {
		return &summary, fmt.Errorf("save cache: %w", saveErr)
	}
	return &summary, nil
}
