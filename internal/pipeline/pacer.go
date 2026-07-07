package pipeline

import (
	"context"
	"sync"

	"github.com/jhoblitt/markdown-linkerator/internal/model"
	"github.com/jhoblitt/markdown-linkerator/internal/ratelimit"
)

// hostPacer owns one host's rate limiter and releases its queued jobs to the
// shared executor at the host's paced rate. Its intake is unbounded so the
// single dispatcher never blocks (which would reintroduce head-of-line blocking
// across hosts). A slow host's pacer parking on Acquire costs only its own
// goroutine, never an executor slot.
type hostPacer struct {
	hs   *ratelimit.HostState
	out  chan<- *model.CheckJob
	mu   sync.Mutex
	cond *sync.Cond
	q    []*model.CheckJob
	done bool
}

func newHostPacer(hs *ratelimit.HostState, out chan<- *model.CheckJob) *hostPacer {
	p := &hostPacer{hs: hs, out: out}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *hostPacer) enqueue(job *model.CheckJob) {
	p.mu.Lock()
	p.q = append(p.q, job)
	p.mu.Unlock()
	p.cond.Signal()
}

// close signals that no more jobs will be enqueued; the pacer drains its queue
// and exits.
func (p *hostPacer) close() {
	p.mu.Lock()
	p.done = true
	p.mu.Unlock()
	p.cond.Broadcast()
}

func (p *hostPacer) run(ctx context.Context) {
	// Wake a blocked cond.Wait when the context is cancelled.
	stop := context.AfterFunc(ctx, func() { p.cond.Broadcast() })
	defer stop()

	for {
		p.mu.Lock()
		for len(p.q) == 0 && !p.done && ctx.Err() == nil {
			p.cond.Wait()
		}
		if ctx.Err() != nil {
			p.mu.Unlock()
			return
		}
		if len(p.q) == 0 && p.done {
			p.mu.Unlock()
			return
		}
		job := p.q[0]
		p.q = p.q[1:]
		p.mu.Unlock()

		if err := p.hs.Acquire(ctx); err != nil {
			return
		}
		if err := send(ctx, p.out, job); err != nil {
			return
		}
	}
}

// pacerSet lazily creates one hostPacer per host and tracks their goroutines.
type pacerSet struct {
	reg    *ratelimit.Registry
	out    chan<- *model.CheckJob
	ctx    context.Context
	wg     sync.WaitGroup
	pacers map[string]*hostPacer
}

func newPacerSet(ctx context.Context, reg *ratelimit.Registry, out chan<- *model.CheckJob) *pacerSet {
	return &pacerSet{reg: reg, out: out, ctx: ctx, pacers: map[string]*hostPacer{}}
}

// dispatch routes a job to its host's pacer, creating (and starting) the pacer
// on first use. Called only by the single dispatcher goroutine, so the map needs
// no lock.
func (s *pacerSet) dispatch(job *model.CheckJob) {
	p, ok := s.pacers[job.Host]
	if !ok {
		p = newHostPacer(s.reg.Host(job.Host), s.out)
		s.pacers[job.Host] = p
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			p.run(s.ctx)
		}()
	}
	p.enqueue(job)
}

// closeAndWait signals every pacer to drain and blocks until all have exited.
func (s *pacerSet) closeAndWait() {
	for _, p := range s.pacers {
		p.close()
	}
	s.wg.Wait()
}
