package httpclient

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Empty is the type parameter for a response with no body.
type Empty struct{}

// Request configures one outbound call.
type Request struct {
	Method string
	URL    string
	Query  url.Values

	// Body is JSON-marshaled when non-nil.
	Body any

	Headers map[string]string

	// Written straight into the header map to preserve exact spelling, for APIs
	// that reject the name Header.Set would canonicalize it to.
	RawHeaders map[string]string

	// Runs on every attempt once the headers are set and before the send, so
	// what it computes is recomputed rather than replayed.
	Prepare func(*http.Request) error

	// Prefixes every error this call produces.
	ServiceName string

	// nil means the default policy: reads retried, writes never.
	Retry *RetryPolicy
}

// Response is the outcome of a call that reached the server, whatever its status.
type Response struct {
	StatusCode  int
	Header      http.Header
	Body        []byte
	ServiceName string
}

// OK reports a 2xx status.
func (r *Response) OK() bool { return r.StatusCode >= 200 && r.StatusCode <= 299 }

// Err returns a *StatusError for a non-2xx response, nil otherwise.
func (r *Response) Err() error {
	if r.OK() {
		return nil
	}
	return &StatusError{ServiceName: r.ServiceName, StatusCode: r.StatusCode, Body: string(r.Body)}
}

// Do executes the request under the retry policy. A non-2xx status is not an
// error here — it comes back in Response — so a returned error is always infrastructure.
func Do(ctx context.Context, client *http.Client, r Request) (*Response, error) {
	if client == nil {
		client = New()
	}
	cfg := settingsOf(client)

	reqURL, err := buildURL(r.URL, r.Query)
	if err != nil {
		return nil, err
	}

	var payload []byte
	if r.Body != nil {
		if payload, err = json.Marshal(r.Body); err != nil {
			return nil, fmt.Errorf("httpclient: marshal request body: %w", err)
		}
	}

	service := r.ServiceName
	if service == "" {
		service = "service"
	}

	policy := r.Retry.normalized()
	event := Event{Service: service, Method: r.Method, URL: observedURL(reqURL)}

	for attempt := 1; ; attempt++ {
		start := time.Now()
		resp, err := doOnce(ctx, client, cfg, r, reqURL, payload, service)
		observe(ctx, cfg.observer, event, attempt, time.Since(start), resp, err)

		if attempt >= policy.MaxAttempts || !policy.shouldRetry(ctx, r.Method, resp, err) {
			return resp, err
		}
		if waitErr := sleepCtx(ctx, policy.delayFor(attempt, resp)); waitErr != nil {
			return nil, fmt.Errorf("httpclient: waiting to retry %s: %w", service, waitErr)
		}
	}
}

func observe(ctx context.Context, o Observer, e Event, attempt int, took time.Duration, resp *Response, err error) {
	if o == nil {
		return
	}
	e.Attempt, e.Duration, e.Err = attempt, took, err
	if resp != nil {
		e.Status = resp.StatusCode
	}
	o(ctx, e)
}

func doOnce(ctx context.Context, client *http.Client, cfg settings, r Request, reqURL string, payload []byte, service string) (*Response, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, reqURL, body)
	if err != nil {
		return nil, permanent(fmt.Errorf("httpclient: build request: %w", err))
	}

	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range r.RawHeaders {
		req.Header[k] = []string{v}
	}
	applyDefaultHeaders(req, cfg.userAgent)

	for _, prepare := range []func(*http.Request) error{cfg.prepare, r.Prepare} {
		if prepare == nil {
			continue
		}
		if err := prepare(req); err != nil {
			return nil, permanent(fmt.Errorf("httpclient: prepare %s request: %w", service, err))
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpclient: calling %s: %w", service, err)
	}
	defer resp.Body.Close()

	limit := cfg.maxResponseBytes

	// One byte past the limit is what tells a body at the limit from one over it.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("httpclient: read %s response: %w", service, err)
	}
	if int64(len(raw)) > limit {
		return nil, permanent(fmt.Errorf("httpclient: %s response over %d bytes: %w", service, limit, ErrResponseTooLarge))
	}

	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: raw, ServiceName: service}, nil
}

func buildURL(base string, query url.Values) (string, error) {
	if len(query) == 0 {
		return base, nil
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("httpclient: parse url: %w", err)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("httpclient: parse url query: %w", err)
	}
	for k, values := range query {
		q.Del(k)
		for _, v := range values {
			q.Add(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Decode unmarshals the body into T. Field names are matched case-sensitively
// and duplicate JSON names are rejected, per encoding/json/v2.
func Decode[T any](resp *Response) (*T, error) {
	var out T
	if len(resp.Body) == 0 {
		return &out, nil
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("httpclient: decode %s response: %w", resp.ServiceName, err)
	}
	return &out, nil
}

// DoJSON is Do + status check + Decode. Use Do plus Decode when the provider's
// error envelope needs custom parsing.
func DoJSON[T any](ctx context.Context, client *http.Client, r Request) (*T, error) {
	resp, err := Do(ctx, client, r)
	if err != nil {
		return nil, err
	}
	if err := resp.Err(); err != nil {
		return nil, err
	}
	return Decode[T](resp)
}
