// Package ratelimit provides per-host request pacing with graceful 429 backoff.
//
// Each host gets a token-bucket limiter (golang.org/x/time/rate) plus an
// adaptive layer: a 429 response triggers an AIMD multiplicative decrease and a
// Retry-After cooldown, while sustained successes additively restore the rate
// toward its configured ceiling. A Registry lazily owns one HostState per host
// and aggregates their statistics.
package ratelimit

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

const (
	// absMinRPS is the rate used when a host's configured RPS is non-positive,
	// so its limiter still makes slow forward progress instead of stalling.
	absMinRPS = 0.01

	// penaltyFloorRPS bounds how far a run of 429s can drive a host's rate down.
	penaltyFloorRPS = 0.2

	// aimdStepDivisor sizes both the AIMD additive-increase step and the
	// configured-relative component of the decrease floor (configRPS / N).
	aimdStepDivisor = 8

	// maxCooldown caps a server-supplied Retry-After so a hostile or bogus
	// header cannot wedge a host for the rest of the run.
	maxCooldown = 5 * time.Minute

	// successesPerRecovery is how many consecutive non-429 successes earn one
	// additive rate increase.
	successesPerRecovery = 5
)

// Registry lazily creates and owns one HostState per hostname.
type Registry struct {
	cfg   config.Resolved
	mu    sync.Mutex
	hosts map[string]*HostState
}

// NewRegistry returns a Registry that builds host limiters from cfg's per-host
// defaults and HostOverrides.
func NewRegistry(cfg config.Resolved) *Registry {
	return &Registry{
		cfg:   cfg,
		hosts: make(map[string]*HostState),
	}
}

// Host returns the HostState for name, creating it on first use. An entry in
// cfg.HostOverrides for name wins over the PerHostRPS/PerHostBurst defaults.
func (r *Registry) Host(name string) *HostState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.hosts[name]; ok {
		return h
	}
	rps, burst := r.cfg.PerHostRPS, r.cfg.PerHostBurst
	if ov, ok := r.cfg.HostOverrides[name]; ok {
		rps, burst = ov.RPS, ov.Burst
	}
	h := newHostState(name, rps, burst)
	r.hosts[name] = h
	return h
}

// AllStats returns a snapshot of every known host's statistics, sorted by host.
func (r *Registry) AllStats() []model.HostStat {
	r.mu.Lock()
	hosts := make([]*HostState, 0, len(r.hosts))
	for _, h := range r.hosts {
		hosts = append(hosts, h)
	}
	r.mu.Unlock()

	stats := make([]model.HostStat, len(hosts))
	for i, h := range hosts {
		stats[i] = h.Stats()
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].Host < stats[j].Host })
	return stats
}

// HostState is one host's token bucket, adaptive 429 state, and counters. It is
// safe for concurrent use: the executor's workers call Record, Penalize429, and
// OnSuccess while the host's pacer goroutine is blocked in Acquire.
type HostState struct {
	host      string
	configRPS float64 // the rate ceiling recovery restores toward; immutable
	limiter   *rate.Limiter

	requests atomic.Int64
	retries  atomic.Int64
	n429     atomic.Int64

	mu          sync.Mutex
	notBefore   time.Time // 429 cooldown gate, independent of the token bucket
	consecutive int       // consecutive successes since the last increase or 429
	first       time.Time // first/last request time, for ObservedRPS
	last        time.Time
}

func newHostState(host string, rps float64, burst int) *HostState {
	if rps <= 0 {
		rps = absMinRPS
	}
	if burst < 1 {
		burst = 1
	}
	return &HostState{
		host:      host,
		configRPS: rps,
		limiter:   rate.NewLimiter(rate.Limit(rps), burst),
	}
}

// Acquire blocks until a token is available and any active 429 cooldown has
// elapsed, or until ctx is cancelled. It is meant to be called serially by a
// single per-host pacer goroutine.
func (h *HostState) Acquire(ctx context.Context) error {
	if err := h.limiter.Wait(ctx); err != nil {
		return err
	}
	// The cooldown is separate from the bucket: a token was granted above, but a
	// 429 may forbid using it until notBefore. Re-check in a loop because a
	// concurrent Penalize429 can extend notBefore while we sleep.
	for {
		h.mu.Lock()
		wait := time.Until(h.notBefore)
		h.mu.Unlock()
		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Record accounts one physical HTTP request (plus any retries it performed) for
// statistics. Call it once per request the executor issues for this host.
func (h *HostState) Record(retries int) {
	now := time.Now()
	h.requests.Add(1)
	if retries > 0 {
		h.retries.Add(int64(retries))
	}
	h.mu.Lock()
	if h.first.IsZero() {
		h.first = now
	}
	h.last = now
	h.mu.Unlock()
}

// ArmCooldown gates subsequent requests to this host until retryAfter elapses
// (clamped to maxCooldown), without touching the rate or the 429 count. It is
// called the moment a 429/503 is observed mid-retry, so queued same-host jobs
// wait out the server's window instead of piling on before the post-check
// Penalize429 (which still applies the AIMD rate cut once). notBefore only ever
// moves forward, so this composes with a later Penalize429.
func (h *HostState) ArmCooldown(retryAfter time.Duration) {
	if retryAfter <= 0 {
		return
	}
	if retryAfter > maxCooldown {
		retryAfter = maxCooldown
	}
	nb := time.Now().Add(retryAfter)
	h.mu.Lock()
	defer h.mu.Unlock()
	if nb.After(h.notBefore) {
		h.notBefore = nb
	}
}

// Penalize429 applies the AIMD multiplicative decrease and arms a cooldown after
// a 429: the limiter's rate is halved down to a floor, and no token may be used
// until retryAfter has elapsed (clamped to maxCooldown). Safe to call while a
// pacer is blocked in Acquire.
func (h *HostState) Penalize429(retryAfter time.Duration) {
	h.n429.Add(1)

	if retryAfter < 0 {
		retryAfter = 0
	}
	if retryAfter > maxCooldown {
		retryAfter = maxCooldown
	}
	nb := time.Now().Add(retryAfter)

	h.mu.Lock()
	defer h.mu.Unlock()
	next := float64(h.limiter.Limit()) / 2
	if floor := h.penaltyFloor(); next < floor {
		next = floor
	}
	if next > h.configRPS { // a floor above configRPS (tiny configs) must not raise the rate
		next = h.configRPS
	}
	h.limiter.SetLimit(rate.Limit(next))
	if nb.After(h.notBefore) {
		h.notBefore = nb
	}
	h.consecutive = 0
}

// OnSuccess records a non-429 success and, once enough have accrued in a row,
// additively nudges the limiter's rate back toward configRPS (never above it).
// This keeps a single 429 from throttling a host for the rest of the run.
func (h *HostState) OnSuccess() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.consecutive++
	if h.consecutive < successesPerRecovery {
		return
	}
	h.consecutive = 0
	cur := float64(h.limiter.Limit())
	if cur >= h.configRPS {
		return
	}
	next := cur + h.configRPS/aimdStepDivisor
	if next > h.configRPS {
		next = h.configRPS
	}
	h.limiter.SetLimit(rate.Limit(next))
}

// Stats returns a snapshot of this host's counters and observed request rate.
func (h *HostState) Stats() model.HostStat {
	h.mu.Lock()
	first, last := h.first, h.last
	h.mu.Unlock()

	req := h.requests.Load()
	var observed float64
	if req >= 2 {
		if elapsed := last.Sub(first).Seconds(); elapsed > 0 {
			observed = float64(req) / elapsed
		}
	}
	return model.HostStat{
		Host:        h.host,
		Requests:    req,
		Retries:     h.retries.Load(),
		N429:        h.n429.Load(),
		ObservedRPS: observed,
	}
}

// penaltyFloor is the lower bound of the multiplicative decrease: the larger of
// the absolute floor and a fraction of the configured rate.
func (h *HostState) penaltyFloor() float64 {
	if f := h.configRPS / aimdStepDivisor; f > penaltyFloorRPS {
		return f
	}
	return penaltyFloorRPS
}
