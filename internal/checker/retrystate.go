package checker

import (
	"context"
	"time"
)

// retryState accumulates the per-request retry accounting that a shared
// retryablehttp.Client cannot hold. One instance is created per Check call and
// carried on the request context, so the client's CheckRetry/Backoff/log hooks
// find it via the context they are handed. Each instance is touched only by the
// single goroutine driving that request, so no locking is needed.
type retryState struct {
	retries           int
	saw429            bool
	retryAfter        time.Duration
	transportFailures int // connection-level failures (bounded by ConnectRetries)
	rateLimitRetries  int // 429/503 retries (bounded by MaxRetries, independent of transport)
}

type retryStateKey struct{}

func withRetryState(ctx context.Context, st *retryState) context.Context {
	return context.WithValue(ctx, retryStateKey{}, st)
}

func retryStateFrom(ctx context.Context) *retryState {
	st, _ := ctx.Value(retryStateKey{}).(*retryState)
	return st
}
