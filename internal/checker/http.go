package checker

import (
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"

	"github.com/jhoblitt/markdown-linkerator/internal/config"
	"github.com/jhoblitt/markdown-linkerator/internal/model"
)

// errTooManyRedirects is returned from CheckRedirect once the redirect cap is
// hit. It is a sentinel so CheckRetry can recognize (via errors.Is through the
// url.Error wrapper) that a redirect loop must not be retried.
var errTooManyRedirects = errors.New("stopped after maximum redirects")

// HTTPChecker checks http/https targets with a single retryablehttp.Client
// shared across executor workers. Per-request retry accounting is threaded
// through the request context (see retryState) so the shared client stays
// stateless and safe for concurrent use.
type HTTPChecker struct {
	cfg    config.Resolved
	client *retryablehttp.Client
}

// NewHTTPChecker builds a checker with one shared, concurrency-safe HTTP client.
func NewHTTPChecker(cfg config.Resolved) *HTTPChecker {
	c := &HTTPChecker{cfg: cfg}

	rc := retryablehttp.NewClient()
	rc.Logger = nil // silence the default stderr logger
	rc.RetryMax = cfg.MaxRetries
	rc.RetryWaitMin = orDefault(cfg.FallbackRetryDelay, time.Second)
	rc.RetryWaitMax = orDefault(cfg.BackoffMax, 2*time.Minute)
	rc.CheckRetry = c.checkRetry
	rc.Backoff = c.backoff
	rc.RequestLogHook = requestLogHook
	// Return the last response (e.g. a persistent 429) instead of the default
	// behavior of closing it and returning only an error, so Check can classify
	// the exhausted status rather than treating it as a transport failure.
	rc.ErrorHandler = retryablehttp.PassthroughErrorHandler

	// Reuse the pooled transport retryablehttp created; only override redirect
	// policy and the per-attempt timeout.
	rc.HTTPClient.CheckRedirect = c.checkRedirect
	rc.HTTPClient.Timeout = cfg.Timeout

	c.client = rc
	return c
}

// Check performs a HEAD, falling back to GET when HEAD is not clearly alive,
// and classifies the final status against cfg.AliveStatusCodes. A dead link is
// an ordinary Result (Err == nil); only context cancellation sets Result.Err.
func (c *HTTPChecker) Check(ctx context.Context, t model.Target) model.Result {
	res := model.Result{Target: t, Host: hostOf(t.URL)}

	st := &retryState{}
	rctx := withRetryState(ctx, st)

	// With PassthroughErrorHandler, an exhausted 429/503 comes back as a normal
	// response with err == nil, while genuine failures (redirect loop, transport)
	// keep err != nil — so the err==nil branch classifies the status directly.
	resp, err := c.do(rctx, http.MethodHead, t)
	if err == nil {
		code := resp.StatusCode
		drain(resp)
		if c.shortCircuit(code) {
			return c.classify(res, code, st)
		}
		// A final rate-limit / unavailable status after HEAD's own retry handling
		// is authoritative: a GET fallback would run a second retry cycle and
		// amplify load against a host that is already throttling us.
		if code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable {
			return c.classify(res, code, st)
		}
		// HEAD reached the server but is not clearly alive (405/401/404/...);
		// retry the same URL with GET, whose status becomes authoritative.
	} else if ctx.Err() != nil {
		res.State = model.StateError
		res.Err = ctx.Err()
		return res
	}

	resp, err = c.do(rctx, http.MethodGet, t)
	if err != nil {
		if ctx.Err() != nil {
			res.State = model.StateError
			res.Err = ctx.Err()
			return res
		}
		res.State = model.StateDead
		res.StatusCode = 0
		res.Detail = err.Error()
		applyRetryState(&res, st)
		return res
	}
	code := resp.StatusCode
	drain(resp)
	return c.classify(res, code, st)
}

// shortCircuit reports whether a HEAD status is conclusive enough to skip the
// GET fallback: any 2xx, or a code the config explicitly deems alive.
func (c *HTTPChecker) shortCircuit(code int) bool {
	return (code >= 200 && code < 300) || c.cfg.AliveStatusCodes[code]
}

func (c *HTTPChecker) classify(res model.Result, code int, st *retryState) model.Result {
	res.StatusCode = code
	if c.cfg.AliveStatusCodes[code] {
		res.State = model.StateAlive
	} else {
		res.State = model.StateDead
	}
	applyRetryState(&res, st)
	return res
}

func (c *HTTPChecker) do(ctx context.Context, method string, t model.Target) (*http.Response, error) {
	req, err := retryablehttp.NewRequestWithContext(ctx, method, t.URL, nil)
	if err != nil {
		return nil, err
	}
	if c.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", c.cfg.UserAgent)
	}
	for _, rule := range c.cfg.HTTPHeaders {
		if ruleMatches(rule, t.URL) {
			for k, v := range rule.Headers {
				req.Header.Set(k, v) // may override User-Agent, as intended
			}
			break // first matching rule wins
		}
	}
	return c.client.Do(req)
}

func (c *HTTPChecker) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= c.cfg.MaxRedirects {
		return errTooManyRedirects
	}
	// Go's client strips Authorization/Cookie on a cross-origin redirect, but not
	// the custom headers we attach from httpHeaders — which may carry {{env.*}}
	// secrets. Strip them ourselves so a redirect off-origin cannot exfiltrate
	// them to an attacker-controlled host.
	if len(via) > 0 && !sameOrigin(req.URL, via[0].URL) {
		for _, rule := range c.cfg.HTTPHeaders {
			for k := range rule.Headers {
				req.Header.Del(k)
			}
		}
	}
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// checkRetry decides whether an attempt should be retried. It retries transport
// errors (except non-recoverable ones like a redirect loop), 429 when enabled,
// and 503; everything else is reported as-is. Context cancellation never
// retries. A 429 whose Retry-After exceeds BackoffMax is abandoned rather than
// parked for minutes.
func (c *HTTPChecker) checkRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		if errors.Is(err, errTooManyRedirects) {
			return false, nil
		}
		return true, nil
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		if st := retryStateFrom(ctx); st != nil {
			st.saw429 = true
		}
		if !c.cfg.RetryOn429 {
			return false, nil
		}
		if c.retryAfterTooLong(resp) {
			return false, nil
		}
		return true, nil
	case http.StatusServiceUnavailable:
		if c.retryAfterTooLong(resp) {
			return false, nil
		}
		return true, nil
	default:
		return false, nil
	}
}

func (c *HTTPChecker) retryAfterTooLong(resp *http.Response) bool {
	d, ok := parseRetryAfter(resp.Header.Get("Retry-After"))
	return ok && c.cfg.BackoffMax > 0 && d > c.cfg.BackoffMax
}

// backoff honors a 429/503 Retry-After (as sent, capped at BackoffMax) and
// otherwise applies exponential backoff with jitter between RetryWaitMin and
// BackoffMax. The honored Retry-After is recorded on the request's retryState
// for the pipeline's per-host cooldown.
func (c *HTTPChecker) backoff(lo, hi time.Duration, attemptNum int, resp *http.Response) time.Duration {
	if resp != nil && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable) {
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			if resp.Request != nil {
				if st := retryStateFrom(resp.Request.Context()); st != nil {
					st.retryAfter = d
				}
			}
			// Honor the server's Retry-After as sent, capped only by BackoffMax.
			// It must NOT be floored to RetryWaitMin (the exponential-backoff
			// minimum) — a "Retry-After: 1" forced up to a 30s floor would make
			// every 429 needlessly slow.
			if d < 0 {
				d = 0
			}
			if hi > 0 && d > hi {
				d = hi
			}
			return d
		}
	}
	return expJitterBackoff(lo, hi, attemptNum)
}

// expJitterBackoff returns lo*2^attemptNum capped at hi, with full jitter in
// [0, base] floored at lo to spread retries.
func expJitterBackoff(lo, hi time.Duration, attemptNum int) time.Duration {
	if lo <= 0 {
		lo = time.Millisecond
	}
	if hi > 0 && lo > hi {
		lo = hi // a floor above the ceiling would defeat BackoffMax
	}
	if hi < lo {
		hi = lo
	}
	mult := math.Pow(2, float64(attemptNum)) * float64(lo)
	base := time.Duration(mult)
	if float64(base) != mult || base > hi {
		base = hi
	}
	wait := time.Duration(rand.Int64N(int64(base) + 1))
	if wait < lo {
		wait = lo
	}
	return wait
}

// parseRetryAfter parses a Retry-After value in either integer-seconds or
// HTTP-date form, returning the delay and whether parsing succeeded. A past date
// yields (0, true).
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

func requestLogHook(_ retryablehttp.Logger, r *http.Request, attempt int) {
	if attempt == 0 {
		return
	}
	if st := retryStateFrom(r.Context()); st != nil {
		st.retries++
	}
}

// ruleMatches reports whether a header rule applies to target. It requires an
// exact origin (scheme + host:port) match before honoring any path prefix, so a
// rule for https://api.github.com cannot leak its (possibly secret) headers to a
// look-alike host like https://api.github.com.evil.example. A raw string prefix
// — which markdown-link-check uses — is a credential-exfiltration vector.
func ruleMatches(rule config.HeaderRule, target string) bool {
	tu, err := url.Parse(target)
	if err != nil {
		return false
	}
	for _, u := range rule.URLs {
		if u == "" {
			continue
		}
		ru, err := url.Parse(u)
		if err != nil {
			continue
		}
		if !strings.EqualFold(ru.Scheme, tu.Scheme) || !strings.EqualFold(ru.Host, tu.Host) {
			continue
		}
		if pathWithin(tu.Path, ru.Path) {
			return true
		}
	}
	return false
}

// pathWithin reports whether p is at or under the prefix path, respecting path
// segment boundaries (so "/api" does not match "/apix").
func pathWithin(p, prefix string) bool {
	if prefix == "" || prefix == "/" {
		return true
	}
	prefix = strings.TrimSuffix(prefix, "/")
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func applyRetryState(res *model.Result, st *retryState) {
	res.Retries = st.retries
	res.Saw429 = st.saw429
	res.RetryAfter = st.retryAfter
}
