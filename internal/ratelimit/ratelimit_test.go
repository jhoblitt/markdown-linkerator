package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
)

// TestArmCooldownGatesAcquire guards that arming a cooldown (as done the instant a
// 429/503 is observed) makes a subsequent Acquire for the same host wait out the
// window, so queued jobs stop hitting a throttling host immediately — not only
// after the retrying request's post-check Penalize429.
func TestArmCooldownGatesAcquire(t *testing.T) {
	// A fast bucket isolates the cooldown from token-bucket pacing.
	reg := NewRegistry(config.Resolved{PerHostRPS: 1000, PerHostBurst: 1000})
	h := reg.Host("example.com")

	h.ArmCooldown(80 * time.Millisecond)
	start := time.Now()
	require.NoError(t, h.Acquire(context.Background()))
	assert.GreaterOrEqual(t, time.Since(start), 60*time.Millisecond, "Acquire must wait out the armed cooldown")
}

func limit(h *HostState) float64 { return float64(h.limiter.Limit()) }

func TestRegistryHostLazyAndOverrides(t *testing.T) {
	cfg := config.Resolved{
		PerHostRPS:   2,
		PerHostBurst: 3,
		HostOverrides: map[string]config.HostLimit{
			"special.example": {RPS: 10, Burst: 5},
		},
	}
	reg := NewRegistry(cfg)

	tests := []struct {
		name      string
		host      string
		wantRPS   float64
		wantBurst int
	}{
		{"default host", "normal.example", 2, 3},
		{"override host", "special.example", 10, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := reg.Host(tc.host)
			require.NotNil(t, h)
			assert.Equal(t, tc.wantRPS, h.configRPS)
			assert.InDelta(t, tc.wantRPS, limit(h), 1e-9)
			assert.Equal(t, tc.wantBurst, h.limiter.Burst())
		})
	}

	t.Run("same host returns identical pointer", func(t *testing.T) {
		a := reg.Host("normal.example")
		b := reg.Host("normal.example")
		assert.Same(t, a, b)
	})
}

func TestNewHostStateClamps(t *testing.T) {
	tests := []struct {
		name      string
		rps       float64
		burst     int
		wantRPS   float64
		wantBurst int
	}{
		{"zero rps floored", 0, 4, absMinRPS, 4},
		{"negative rps floored", -5, 4, absMinRPS, 4},
		{"zero burst clamped", 2, 0, 2, 1},
		{"negative burst clamped", 2, -1, 2, 1},
		{"both clamped", -1, -1, absMinRPS, 1},
		{"normal untouched", 3, 2, 3, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHostState("h", tc.rps, tc.burst)
			assert.Equal(t, tc.wantRPS, h.configRPS)
			assert.InDelta(t, tc.wantRPS, limit(h), 1e-9)
			assert.Equal(t, tc.wantBurst, h.limiter.Burst())
		})
	}
}

func TestAcquirePacing(t *testing.T) {
	// burst 1 so only the initial token is free; each further Acquire waits 1/rps.
	const rps = 50.0
	h := newHostState("h", rps, 1)
	ctx := context.Background()

	start := time.Now()
	for range 3 {
		require.NoError(t, h.Acquire(ctx))
	}
	elapsed := time.Since(start)

	// Two inter-token intervals must elapse across three Acquire calls.
	perToken := time.Duration(float64(time.Second) / rps)
	minExpected := 2*perToken - 10*time.Millisecond
	assert.GreaterOrEqual(t, elapsed, minExpected, "3 Acquire calls should be paced")
}

func TestAcquireCooldownEnforced(t *testing.T) {
	h := newHostState("h", 1000, 10) // fast limiter; cooldown dominates
	h.Penalize429(60 * time.Millisecond)

	start := time.Now()
	require.NoError(t, h.Acquire(context.Background()))
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, 45*time.Millisecond, "Acquire should wait out the 429 cooldown")
}

func TestAcquireCooldownRespectsContext(t *testing.T) {
	h := newHostState("h", 1000, 10)
	h.Penalize429(10 * time.Second) // long cooldown we must not actually wait

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := h.Acquire(ctx)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, time.Second, "Acquire must abort on ctx cancel, not wait the full cooldown")
}

func TestAcquireContextAlreadyCancelled(t *testing.T) {
	h := newHostState("h", 1000, 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Error(t, h.Acquire(ctx))
}

func TestPenalize429AIMDAndFloor(t *testing.T) {
	tests := []struct {
		name      string
		configRPS float64
		want      []float64 // limiter rate after each successive Penalize429
	}{
		{"rps 1 floors at 0.2", 1, []float64{0.5, 0.25, 0.2, 0.2}},
		{"rps 2 floors at 0.25", 2, []float64{1, 0.5, 0.25, 0.25}},
		{"rps 8 floors at 1", 8, []float64{4, 2, 1, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHostState("h", tc.configRPS, 2)
			for i, want := range tc.want {
				h.Penalize429(0)
				assert.InDeltaf(t, want, limit(h), 1e-9, "after %d penalties", i+1)
			}
			assert.Equal(t, int64(len(tc.want)), h.n429.Load(), "N429 counts every penalty")
		})
	}
}

func TestPenalize429CooldownClampAndExtend(t *testing.T) {
	t.Run("clamped to maxCooldown", func(t *testing.T) {
		h := newHostState("h", 1, 2)
		before := time.Now()
		h.Penalize429(time.Hour)
		h.mu.Lock()
		nb := h.notBefore
		h.mu.Unlock()
		assert.LessOrEqual(t, nb.Sub(before), maxCooldown+time.Second)
		assert.Greater(t, nb.Sub(before), maxCooldown-time.Second)
	})

	t.Run("only extends, never shortens", func(t *testing.T) {
		h := newHostState("h", 1, 2)
		h.Penalize429(500 * time.Millisecond)
		h.mu.Lock()
		far := h.notBefore
		h.mu.Unlock()

		h.Penalize429(1 * time.Millisecond) // a nearer deadline must not win
		h.mu.Lock()
		got := h.notBefore
		h.mu.Unlock()
		assert.Equal(t, far, got)
	})

	t.Run("negative retryAfter is treated as no cooldown", func(t *testing.T) {
		h := newHostState("h", 1, 2)
		start := time.Now()
		h.Penalize429(-1 * time.Second)
		require.NoError(t, h.Acquire(context.Background()))
		assert.Less(t, time.Since(start), 200*time.Millisecond)
	})
}

func TestOnSuccessRecovery(t *testing.T) {
	h := newHostState("h", 1, 2)
	for range 3 { // drive down to the 0.2 floor
		h.Penalize429(0)
	}
	require.InDelta(t, 0.2, limit(h), 1e-9)

	t.Run("below threshold does not raise", func(t *testing.T) {
		for range successesPerRecovery - 1 {
			h.OnSuccess()
		}
		assert.InDelta(t, 0.2, limit(h), 1e-9)
	})

	t.Run("threshold raises additively", func(t *testing.T) {
		h.OnSuccess() // reaches the threshold
		assert.InDelta(t, 0.2+1.0/aimdStepDivisor, limit(h), 1e-9)
	})

	t.Run("never exceeds configRPS", func(t *testing.T) {
		for range 100 { // far more than needed to saturate
			h.OnSuccess()
		}
		assert.InDelta(t, 1.0, limit(h), 1e-9)
	})
}

func TestPenalize429ResetsRecoveryProgress(t *testing.T) {
	h := newHostState("h", 1, 2)
	for range 3 {
		h.Penalize429(0)
	}
	require.InDelta(t, 0.2, limit(h), 1e-9)

	for range successesPerRecovery - 1 { // one shy of a bump
		h.OnSuccess()
	}
	h.Penalize429(0) // resets consecutive counter (still floored at 0.2)
	require.InDelta(t, 0.2, limit(h), 1e-9)

	h.OnSuccess() // would have bumped had the counter not reset
	assert.InDelta(t, 0.2, limit(h), 1e-9)

	for range successesPerRecovery - 1 {
		h.OnSuccess()
	}
	assert.InDelta(t, 0.2+1.0/aimdStepDivisor, limit(h), 1e-9, "bump only after a fresh full streak")
}

func TestRecordCountsAndTimestamps(t *testing.T) {
	h := newHostState("h", 1, 2)
	h.Record(0)
	h.Record(2)
	h.Record(3)

	s := h.Stats()
	assert.Equal(t, int64(3), s.Requests)
	assert.Equal(t, int64(5), s.Retries)

	h.mu.Lock()
	assert.False(t, h.first.IsZero())
	assert.False(t, h.last.IsZero())
	assert.True(t, h.last.After(h.first) || h.last.Equal(h.first))
	h.mu.Unlock()
}

func TestStatsObservedRPS(t *testing.T) {
	base := time.Now()
	tests := []struct {
		name     string
		requests int64
		first    time.Time
		last     time.Time
		want     float64
	}{
		{"no requests", 0, time.Time{}, time.Time{}, 0},
		{"single request", 1, base, base, 0},
		{"three over two seconds", 3, base, base.Add(2 * time.Second), 1.5},
		{"zero elapsed", 2, base, base, 0},
		{"ten over five seconds", 10, base, base.Add(5 * time.Second), 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHostState("h", 1, 2)
			h.requests.Store(tc.requests)
			h.mu.Lock()
			h.first, h.last = tc.first, tc.last
			h.mu.Unlock()
			assert.InDelta(t, tc.want, h.Stats().ObservedRPS, 1e-9)
		})
	}
}

func TestStatsCountsN429AndRetries(t *testing.T) {
	h := newHostState("h", 1, 2)
	h.Record(1)
	h.Record(2)
	h.Penalize429(0)
	h.Penalize429(0)

	s := h.Stats()
	assert.Equal(t, "h", s.Host)
	assert.Equal(t, int64(2), s.Requests)
	assert.Equal(t, int64(3), s.Retries)
	assert.Equal(t, int64(2), s.N429)
}

func TestAllStatsSortedSnapshot(t *testing.T) {
	reg := NewRegistry(config.Resolved{PerHostRPS: 1, PerHostBurst: 1})
	reg.Host("c.example").Record(0)
	reg.Host("a.example").Record(0)
	reg.Host("a.example").Record(0)
	reg.Host("b.example").Record(0)

	stats := reg.AllStats()
	require.Len(t, stats, 3)
	assert.Equal(t, "a.example", stats[0].Host)
	assert.Equal(t, "b.example", stats[1].Host)
	assert.Equal(t, "c.example", stats[2].Host)
	assert.Equal(t, int64(2), stats[0].Requests)
	assert.Equal(t, int64(1), stats[1].Requests)
	assert.Equal(t, int64(1), stats[2].Requests)
}

func TestAllStatsEmpty(t *testing.T) {
	reg := NewRegistry(config.Resolved{PerHostRPS: 1, PerHostBurst: 1})
	assert.Empty(t, reg.AllStats())
}

// A concurrent workout: one pacer draining Acquire while other goroutines drive
// the adaptive state and counters. Exercised under -race.
func TestConcurrentAccessRace(t *testing.T) {
	h := newHostState("h", 5000, 50)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() { // the per-host pacer
		defer close(done)
		for {
			if err := h.Acquire(ctx); err != nil {
				return
			}
			h.Record(0)
		}
	}()

	for range 50 {
		go h.Penalize429(time.Millisecond)
		go h.OnSuccess()
		go func() { _ = h.Stats() }()
	}
	<-done

	assert.Positive(t, h.n429.Load())
	assert.GreaterOrEqual(t, limit(h), float64(rate.Limit(penaltyFloorRPS)))
}
