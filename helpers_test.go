package httpclient

import (
	"io"
	"net/http"
	"strings"
)

type payload struct {
	Name string `json:"name"`
}

func noWait() *RetryPolicy { return &RetryPolicy{Backoff: 0} }

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type stub struct {
	status int
	header http.Header
	body   string
}

// Canned responses keep the time-based tests out of the network, which a
// synctest bubble cannot see.
func stubTransport(stubs ...stub) (http.RoundTripper, *int) {
	calls := 0
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		s := stubs[min(calls, len(stubs)-1)]
		calls++
		h := s.header
		if h == nil {
			h = http.Header{}
		}
		return &http.Response{
			StatusCode: s.status,
			Header:     h,
			Body:       io.NopCloser(strings.NewReader(s.body)),
			Request:    req,
		}, nil
	})
	return rt, &calls
}

// RawHeaders skip canonicalization on purpose, so a literal index would trip SA1008.
func rawHeader(h http.Header, key string) []string { return h[key] }
