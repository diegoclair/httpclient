package httpclient

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

const (
	defaultMaxAttempts = 3
	defaultBackoff     = 200 * time.Millisecond
	defaultMaxBackoff  = 30 * time.Second
)

// RetryPolicy decides whether and how long to wait before another attempt.
type RetryPolicy struct {
	// MaxAttempts counts the first try. Values below 1 fall back to the default.
	MaxAttempts int

	// Base of the exponential backoff. Zero disables waiting; a negative value
	// falls back to the default.
	Backoff time.Duration

	// Ceiling for a single wait, the server's own Retry-After included. Values
	// below 1 fall back to the default.
	MaxBackoff time.Duration

	// Keep off unless the endpoint takes an idempotency key: a retry after an
	// ambiguous failure can duplicate the resource.
	RetryWrites bool

	// Reads the wait out of the provider's own envelope, which wins over the
	// Retry-After header. Returning 0 falls back to it.
	RetryAfter func(*Response) time.Duration
}

// NoRetry is a policy of a single attempt.
func NoRetry() *RetryPolicy { return &RetryPolicy{MaxAttempts: 1} }

func (p *RetryPolicy) normalized() RetryPolicy {
	out := RetryPolicy{MaxAttempts: defaultMaxAttempts, Backoff: defaultBackoff, MaxBackoff: defaultMaxBackoff}
	if p == nil {
		return out
	}
	if p.MaxAttempts >= 1 {
		out.MaxAttempts = p.MaxAttempts
	}
	if p.Backoff >= 0 {
		out.Backoff = p.Backoff
	}
	if p.MaxBackoff > 0 {
		out.MaxBackoff = p.MaxBackoff
	}
	out.RetryWrites = p.RetryWrites
	out.RetryAfter = p.RetryAfter
	return out
}

// Exactly one of resp and err is meaningful: a transport failure yields err,
// anything that reached the server yields resp.
func (p RetryPolicy) shouldRetry(ctx context.Context, method string, resp *Response, err error) bool {
	if !p.allowsMethod(method) {
		return false
	}
	if err != nil {
		return transportRetriable(ctx, err)
	}
	return retriableStatus(resp.StatusCode)
}

func (p RetryPolicy) delayFor(attempt int, resp *Response) time.Duration {
	if wait := p.serverWait(resp); wait > 0 {
		return min(wait, p.MaxBackoff)
	}
	return p.jittered(attempt)
}

// An explicit instruction from the server is honoured as given, never jittered.
func (p RetryPolicy) serverWait(resp *Response) time.Duration {
	if resp == nil {
		return 0
	}
	if p.RetryAfter != nil {
		if d := p.RetryAfter(resp); d > 0 {
			return d
		}
	}
	return parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
}

// Full jitter spreads a fleet that failed together instead of resynchronising it.
func (p RetryPolicy) jittered(attempt int) time.Duration {
	if p.Backoff <= 0 {
		return 0
	}
	backoff := p.Backoff << (attempt - 1)
	if backoff <= 0 || backoff > p.MaxBackoff {
		backoff = p.MaxBackoff
	}
	return rand.N(backoff)
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return max(time.Duration(secs)*time.Second, 0)
	}
	when, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	return max(when.Sub(now), 0)
}

func (p RetryPolicy) allowsMethod(method string) bool {
	if p.RetryWrites {
		return true
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// A 4xx other than 429 is the caller's fault and will fail again identically.
func retriableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// Only the caller's own context ends the call: a Client.Timeout error is also a
// context.DeadlineExceeded, and it is the most common transient failure there is.
func transportRetriable(ctx context.Context, err error) bool {
	if _, ok := errors.AsType[*permanentError](err); ok {
		return false
	}
	return ctx.Err() == nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
