// Package httpclient is the shared plumbing for outbound JSON API calls, so an
// adapter keeps only what is specific to its provider.
//
// Client-level settings ride on the transport New installs: WithTransport is the
// only composition point, and assigning Transport after New reverts them to defaults.
package httpclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTimeout          = 20 * time.Second
	defaultMaxResponseBytes = 8 << 20

	// Several APIs ask for a UA naming the integration; Go's default says nothing.
	defaultUserAgent = "httpclient (Go)"
)

// Event is the record of a single attempt handed to an Observer.
type Event struct {
	Service string
	Method  string

	// Host and path only: a query string routinely carries tokens.
	URL string

	// Zero when the attempt never reached a response.
	Status   int
	Attempt  int
	Duration time.Duration
	Err      error
}

// Observer receives one Event per attempt, retries included.
type Observer func(ctx context.Context, e Event)

// Client-level knobs Do needs; they travel on the Transport so a plain
// *http.Client stays the only handle the API exposes.
type settings struct {
	userAgent        string
	prepare          func(*http.Request) error
	observer         Observer
	maxResponseBytes int64
}

type jsonTransport struct {
	base http.RoundTripper
	settings
}

func (t *jsonTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	applyDefaultHeaders(req, t.userAgent)
	return t.base.RoundTrip(req)
}

func applyDefaultHeaders(req *http.Request, userAgent string) {
	setDefaultHeader(req, "Accept", "application/json")
	// Only tag a body as JSON; a GET with Content-Type confuses some APIs.
	if req.Body != nil {
		setDefaultHeader(req, "Content-Type", "application/json")
	}
	setDefaultHeader(req, "User-Agent", userAgent)
}

// RawHeaders may already carry the name in a spelling Header.Get would miss,
// and two spellings of one standard header go out as two headers.
func setDefaultHeader(req *http.Request, name, value string) {
	for k := range req.Header {
		if strings.EqualFold(k, name) {
			return
		}
	}
	req.Header.Set(name, value)
}

// Falls back to the defaults for a client this package did not build.
func settingsOf(c *http.Client) settings {
	if t, ok := c.Transport.(*jsonTransport); ok {
		return t.settings
	}
	return settings{userAgent: defaultUserAgent, maxResponseBytes: defaultMaxResponseBytes}
}

// Option configures the client returned by New.
type Option func(*http.Client)

func configure(f func(*settings)) Option {
	return func(c *http.Client) {
		if t, ok := c.Transport.(*jsonTransport); ok {
			f(&t.settings)
		}
	}
}

// WithTimeout overrides the default per-request timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *http.Client) {
		if d > 0 {
			c.Timeout = d
		}
	}
}

// WithTransport sets the RoundTripper the JSON defaults wrap; it is the only
// composition point, since assigning Transport after New drops every client-level setting.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *http.Client) {
		if t, ok := c.Transport.(*jsonTransport); ok && rt != nil {
			t.base = rt
		}
	}
}

// WithUserAgent names the integration; a per-request header still wins over it.
func WithUserAgent(ua string) Option {
	return configure(func(s *settings) {
		if ua != "" {
			s.userAgent = ua
		}
	})
}

// WithPrepare runs the hook on every request of this client, before Request.Prepare.
func WithPrepare(fn func(*http.Request) error) Option {
	return configure(func(s *settings) { s.prepare = fn })
}

// WithObserver reports every attempt; no-op by default.
func WithObserver(o Observer) Option {
	return configure(func(s *settings) { s.observer = o })
}

// WithMaxResponseBytes bounds the body read. Values below 1 fall back to 8 MiB.
func WithMaxResponseBytes(n int64) Option {
	return configure(func(s *settings) {
		if n > 0 {
			s.maxResponseBytes = n
		}
	})
}

// New builds a JSON client. It always sets a timeout: an http.Client without
// one can hang forever.
func New(opts ...Option) *http.Client {
	c := &http.Client{
		Timeout: defaultTimeout,
		Transport: &jsonTransport{
			base:             http.DefaultTransport,
			userAgent:        defaultUserAgent,
			maxResponseBytes: defaultMaxResponseBytes,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func observedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host + u.Path
}
