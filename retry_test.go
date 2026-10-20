package httpclient

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetry(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		status    int
		policy    *RetryPolicy
		wantCalls int
	}{
		{name: "read retries on 502", method: http.MethodGet, status: http.StatusBadGateway, policy: noWait(), wantCalls: 3},
		{name: "read retries on 429", method: http.MethodGet, status: http.StatusTooManyRequests, policy: noWait(), wantCalls: 3},
		{name: "read does not retry on 400", method: http.MethodGet, status: http.StatusBadRequest, policy: noWait(), wantCalls: 1},
		{name: "write never retries", method: http.MethodPost, status: http.StatusBadGateway, policy: noWait(), wantCalls: 1},
		{name: "write retries when opted in", method: http.MethodPost, status: http.StatusBadGateway, policy: &RetryPolicy{Backoff: 0, RetryWrites: true}, wantCalls: 3},
		{name: "NoRetry disables retries", method: http.MethodGet, status: http.StatusBadGateway, policy: NoRetry(), wantCalls: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			resp, err := Do(context.Background(), New(), Request{
				Method: tt.method, URL: srv.URL, Body: payload{Name: "x"}, Retry: tt.policy,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.status, resp.StatusCode)
			assert.Equal(t, tt.wantCalls, calls)
		})
	}
}

func TestRetry_replaysBodyOnEachAttempt(t *testing.T) {
	var bodies []string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		bodies = append(bodies, string(buf))
		if calls < 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, err := Do(context.Background(), New(), Request{
		Method: http.MethodPost, URL: srv.URL, Body: payload{Name: "x"},
		Retry: &RetryPolicy{Backoff: 0, RetryWrites: true},
	})
	require.NoError(t, err)
	require.Len(t, bodies, 2)
	assert.Equal(t, bodies[0], bodies[1], "the retried attempt must send the same body, not an empty one")
}

func TestDo_cancelledContextIsNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var attempts int
	client := New(WithObserver(func(context.Context, Event) { attempts++ }))

	_, err := Do(ctx, client, Request{Method: http.MethodGet, URL: srv.URL, Retry: noWait()})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, attempts, "a dead context never reaches the transport; only the attempt count sees the waste")
	assert.Zero(t, calls)
}

func TestDo_respectsContextDuringBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt, calls := stubTransport(stub{status: http.StatusBadGateway})

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()

		start := time.Now()
		_, err := Do(ctx, New(WithTransport(rt)), Request{
			Method: http.MethodGet, URL: "http://example.invalid",
			Retry: &RetryPolicy{
				MaxAttempts: 5,
				MaxBackoff:  time.Hour,
				RetryAfter:  func(*Response) time.Duration { return time.Hour },
			},
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, 30*time.Millisecond, time.Since(start), "backoff must abort with the context")
		assert.Equal(t, 1, *calls)
	})
}

func retryAfter(v string) func() http.Header {
	return func() http.Header { return http.Header{"Retry-After": {v}} }
}

func retryAfterDate(d time.Duration) func() http.Header {
	return func() http.Header {
		return http.Header{"Retry-After": {time.Now().Add(d).UTC().Format(http.TimeFormat)}}
	}
}

func TestRetry_honoursRetryAfter(t *testing.T) {
	// Telegram states the wait in its own envelope, not in the header.
	telegramRetryAfter := func(resp *Response) time.Duration {
		var envelope struct {
			Parameters struct {
				RetryAfter int `json:"retry_after"`
			} `json:"parameters"`
		}
		if err := json.Unmarshal(resp.Body, &envelope); err != nil {
			return 0
		}
		return time.Duration(envelope.Parameters.RetryAfter) * time.Second
	}

	tests := []struct {
		name string
		// Built inside the bubble: an HTTP-date is relative to the fake clock.
		header func() http.Header
		body   string
		policy *RetryPolicy
		want   time.Duration
	}{
		{
			name:   "seconds",
			header: retryAfter("2"),
			policy: &RetryPolicy{MaxAttempts: 2},
			want:   2 * time.Second,
		},
		{
			name:   "http-date",
			header: retryAfterDate(5 * time.Second),
			policy: &RetryPolicy{MaxAttempts: 2},
			want:   5 * time.Second,
		},
		{
			name:   "a past http-date waits nothing",
			header: retryAfterDate(-time.Hour),
			policy: &RetryPolicy{MaxAttempts: 2, Backoff: 0},
			want:   0,
		},
		{
			name:   "the policy hook wins over the header",
			header: retryAfter("600"),
			body:   `{"ok":false,"parameters":{"retry_after":7}}`,
			policy: &RetryPolicy{MaxAttempts: 2, RetryAfter: telegramRetryAfter},
			want:   7 * time.Second,
		},
		{
			name:   "MaxBackoff caps what the server asks for",
			header: retryAfter("3600"),
			policy: &RetryPolicy{MaxAttempts: 2, MaxBackoff: 5 * time.Second},
			want:   5 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rt, calls := stubTransport(
					stub{status: http.StatusTooManyRequests, header: tt.header(), body: tt.body},
					stub{status: http.StatusOK, body: `{}`},
				)

				start := time.Now()
				resp, err := Do(context.Background(), New(WithTransport(rt)), Request{
					Method: http.MethodGet, URL: "http://example.invalid", Retry: tt.policy,
				})
				require.NoError(t, err)
				assert.Equal(t, http.StatusOK, resp.StatusCode)
				assert.Equal(t, 2, *calls)
				assert.Equal(t, tt.want, time.Since(start))
			})
		})
	}
}

func TestRetry_honoursRetryAfterOnEveryAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt, calls := stubTransport(stub{
			status: http.StatusServiceUnavailable,
			header: retryAfter("3")(),
		})

		start := time.Now()
		_, err := Do(context.Background(), New(WithTransport(rt)), Request{
			Method: http.MethodGet, URL: "http://example.invalid",
			Retry: &RetryPolicy{MaxAttempts: 4},
		})
		require.NoError(t, err)
		assert.Equal(t, 4, *calls)
		assert.Equal(t, 9*time.Second, time.Since(start), "three waits of three seconds")
	})
}

func TestRetry_hookReturningZeroFallsBackToTheHeader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt, calls := stubTransport(stub{
			status: http.StatusServiceUnavailable,
			header: retryAfter("4")(),
		})

		start := time.Now()
		_, err := Do(context.Background(), New(WithTransport(rt)), Request{
			Method: http.MethodGet, URL: "http://example.invalid",
			Retry: &RetryPolicy{MaxAttempts: 2, RetryAfter: func(*Response) time.Duration { return 0 }},
		})
		require.NoError(t, err)
		assert.Equal(t, 2, *calls)
		assert.Equal(t, 4*time.Second, time.Since(start))
	})
}

func TestRetryPolicy_delayForJitter(t *testing.T) {
	p := RetryPolicy{Backoff: 100 * time.Millisecond, MaxBackoff: 10 * time.Second}

	const samples = 500
	distinct := map[time.Duration]struct{}{}
	var maxFirst, maxThird time.Duration

	for range samples {
		first, third := p.delayFor(1, nil), p.delayFor(3, nil)

		require.GreaterOrEqual(t, first, time.Duration(0))
		require.Less(t, first, p.Backoff, "attempt 1 draws from [0, Backoff)")
		require.Less(t, third, 4*p.Backoff, "attempt 3 draws from [0, Backoff*4)")

		maxFirst, maxThird = max(maxFirst, first), max(maxThird, third)
		distinct[third] = struct{}{}
	}

	assert.Greater(t, len(distinct), samples/2, "the wait must be jittered, not fixed")
	assert.Greater(t, maxThird, maxFirst, "the backoff must grow with the attempt")
}

func TestRetryPolicy_delayForCaps(t *testing.T) {
	p := RetryPolicy{Backoff: time.Second, MaxBackoff: 2 * time.Second}

	for _, attempt := range []int{5, 40, 100} {
		assert.LessOrEqual(t, p.delayFor(attempt, nil), p.MaxBackoff, "an overflowing shift must still be capped")
	}

	assert.Zero(t, RetryPolicy{Backoff: 0, MaxBackoff: time.Second}.delayFor(1, nil))
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "absent", value: "", want: 0},
		{name: "seconds", value: "12", want: 12 * time.Second},
		{name: "zero seconds", value: "0", want: 0},
		{name: "negative seconds", value: "-5", want: 0},
		{name: "http-date ahead", value: now.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second},
		{name: "http-date behind", value: now.Add(-time.Minute).Format(http.TimeFormat), want: 0},
		{name: "garbage", value: "soon", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseRetryAfter(tt.value, now))
		})
	}
}

func TestRetryPolicy_shouldRetry(t *testing.T) {
	readOnly := RetryPolicy{MaxAttempts: 3}
	writesToo := RetryPolicy{MaxAttempts: 3, RetryWrites: true}
	refusesEvery := RetryPolicy{MaxAttempts: 3, Retryable: func(*Response) bool { return false }}
	keepsEvery := RetryPolicy{MaxAttempts: 3, Retryable: func(*Response) bool { return true }}

	dead, cancel := context.WithCancel(context.Background())
	cancel()

	// What http.Client.Timeout hands back, and it is not the caller giving up.
	clientTimeout := fmt.Errorf("calling svc: %w", &url.Error{
		Op: "Get", URL: "http://example.invalid", Err: context.DeadlineExceeded,
	})

	tests := []struct {
		name   string
		ctx    context.Context
		policy RetryPolicy
		method string
		resp   *Response
		err    error
		want   bool
	}{
		{name: "read on 502", policy: readOnly, method: http.MethodGet, resp: &Response{StatusCode: 502}, want: true},
		{name: "read on 429", policy: readOnly, method: http.MethodGet, resp: &Response{StatusCode: 429}, want: true},
		{name: "read on 200", policy: readOnly, method: http.MethodGet, resp: &Response{StatusCode: 200}, want: false},
		{name: "read on 404", policy: readOnly, method: http.MethodGet, resp: &Response{StatusCode: 404}, want: false},
		{name: "write on 502 is refused", policy: readOnly, method: http.MethodPost, resp: &Response{StatusCode: 502}, want: false},
		{name: "head on 502", policy: readOnly, method: http.MethodHead, resp: &Response{StatusCode: 502}, want: true},
		{name: "options on 502", policy: readOnly, method: http.MethodOptions, resp: &Response{StatusCode: 502}, want: true},
		{name: "put on 502 is refused", policy: readOnly, method: http.MethodPut, resp: &Response{StatusCode: 502}, want: false},
		{name: "delete on 502 is refused", policy: readOnly, method: http.MethodDelete, resp: &Response{StatusCode: 502}, want: false},
		{name: "patch on 502 is refused", policy: readOnly, method: http.MethodPatch, resp: &Response{StatusCode: 502}, want: false},
		{name: "write on 502 when opted in", policy: writesToo, method: http.MethodPost, resp: &Response{StatusCode: 502}, want: true},
		{name: "transport failure on read", policy: readOnly, method: http.MethodGet, err: assert.AnError, want: true},
		{name: "client timeout", policy: readOnly, method: http.MethodGet, err: clientTimeout, want: true},
		{name: "bare deadline on a live context", policy: readOnly, method: http.MethodGet, err: context.DeadlineExceeded, want: true},
		{name: "caller gave up", ctx: dead, policy: readOnly, method: http.MethodGet, err: clientTimeout, want: false},
		{name: "caller gave up on a plain failure", ctx: dead, policy: readOnly, method: http.MethodGet, err: assert.AnError, want: false},
		{name: "permanent failure", policy: readOnly, method: http.MethodGet, err: permanent(assert.AnError), want: false},
		{name: "transport failure on write is refused", policy: readOnly, method: http.MethodPost, err: assert.AnError, want: false},
		{name: "429 the hook refuses", policy: refusesEvery, method: http.MethodGet, resp: &Response{StatusCode: 429}, want: false},
		{name: "429 the hook keeps", policy: keepsEvery, method: http.MethodGet, resp: &Response{StatusCode: 429}, want: true},
		{name: "404 the hook keeps is still final", policy: keepsEvery, method: http.MethodGet, resp: &Response{StatusCode: 404}, want: false},
		{name: "write the hook keeps is still refused", policy: keepsEvery, method: http.MethodPost, resp: &Response{StatusCode: 502}, want: false},
		{name: "transport failure never asks the hook", policy: refusesEvery, method: http.MethodGet, err: assert.AnError, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			assert.Equal(t, tt.want, tt.policy.shouldRetry(ctx, tt.method, tt.resp, tt.err))
		})
	}
}

func TestRetryPolicy_normalized(t *testing.T) {
	def := (*RetryPolicy)(nil).normalized()
	assert.Equal(t, defaultMaxAttempts, def.MaxAttempts)
	assert.Equal(t, defaultBackoff, def.Backoff)
	assert.Equal(t, defaultMaxBackoff, def.MaxBackoff)
	assert.False(t, def.RetryWrites, "writes must not be retried unless asked for")
	assert.Nil(t, def.RetryAfter)
	assert.Nil(t, def.Retryable, "no hook keeps the status rule")

	custom := (&RetryPolicy{MaxAttempts: 0, Backoff: -1, MaxBackoff: -1}).normalized()
	assert.Equal(t, defaultMaxAttempts, custom.MaxAttempts, "invalid values fall back to the default")
	assert.Equal(t, defaultBackoff, custom.Backoff)
	assert.Equal(t, defaultMaxBackoff, custom.MaxBackoff)

	hook := (&RetryPolicy{RetryAfter: func(*Response) time.Duration { return time.Second }}).normalized()
	require.NotNil(t, hook.RetryAfter)
	assert.Equal(t, time.Second, hook.RetryAfter(nil))

	veto := (&RetryPolicy{Retryable: func(*Response) bool { return false }}).normalized()
	require.NotNil(t, veto.Retryable, "the hook survives normalisation")
}

func TestRetry_aResponseTheHookRefusesIsTheLastAttempt(t *testing.T) {
	rt, calls := stubTransport(
		stub{status: http.StatusTooManyRequests, body: `{"message":"same request again"}`},
		stub{status: http.StatusOK, body: `{}`},
	)

	resp, err := Do(context.Background(), New(WithTransport(rt)), Request{
		Method: http.MethodGet, URL: "http://example.invalid",
		Retry: &RetryPolicy{Backoff: 0, Retryable: func(r *Response) bool {
			return !strings.Contains(string(r.Body), "same request again")
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Equal(t, 1, *calls)
}

func TestRetry_aResponseTheHookKeepsIsTriedAgain(t *testing.T) {
	rt, calls := stubTransport(
		stub{status: http.StatusTooManyRequests, body: `{"message":"slow down"}`},
		stub{status: http.StatusOK, body: `{}`},
	)

	resp, err := Do(context.Background(), New(WithTransport(rt)), Request{
		Method: http.MethodGet, URL: "http://example.invalid",
		Retry: &RetryPolicy{Backoff: 0, Retryable: func(*Response) bool { return true }},
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 2, *calls)
}

func TestRetry_clientTimeoutIsRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			<-r.Context().Done()
			return nil, r.Context().Err()
		})

		client := New(WithTransport(rt), WithTimeout(time.Second))
		_, err := Do(context.Background(), client, Request{
			Method: http.MethodGet, URL: "http://example.invalid", ServiceName: "svc",
			Retry: &RetryPolicy{MaxAttempts: 3, Backoff: 0},
		})
		require.ErrorContains(t, err, "calling svc")
		require.ErrorIs(t, err, context.DeadlineExceeded, "a client timeout looks like a deadline; it is still transient")
		assert.Equal(t, 3, calls, "the caller's context is alive, so the timeout is worth another attempt")
	})
}

func TestRetry_callerContextEndsTheCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			<-r.Context().Done()
			return nil, r.Context().Err()
		})

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		// A dead context never reaches the transport, so only the attempt count
		// can tell one try from three.
		var attempts int
		client := New(WithTransport(rt), WithTimeout(time.Hour),
			WithObserver(func(context.Context, Event) { attempts++ }))

		_, err := Do(ctx, client, Request{
			Method: http.MethodGet, URL: "http://example.invalid", ServiceName: "svc",
			Retry: &RetryPolicy{MaxAttempts: 3, Backoff: 0},
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, 1, attempts, "the caller gave up; there is nothing left to retry for")
		assert.Equal(t, 1, calls)
	})
}

func TestDo_zeroBackoffStillStopsAtACancelledContext(t *testing.T) {
	rt, calls := stubTransport(stub{status: http.StatusBadGateway})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := New(WithTransport(rt), WithObserver(func(context.Context, Event) { cancel() }))

	_, err := Do(ctx, client, Request{
		Method: http.MethodGet, URL: "http://example.invalid", ServiceName: "svc", Retry: noWait(),
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, *calls, "waiting for nothing must still notice the caller gave up")
}

func TestRetriableStatus_boundaries(t *testing.T) {
	for code, want := range map[int]bool{429: true, 499: false, 500: true, 503: true} {
		assert.Equal(t, want, retriableStatus(code), "status %d", code)
	}
}
