package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_hasTimeoutAndDefaultHeaders(t *testing.T) {
	var accept, ua, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept, ua, contentType = r.Header.Get("Accept"), r.Header.Get("User-Agent"), r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"name":"ok"}`))
	}))
	defer srv.Close()

	c := New()
	require.Positive(t, c.Timeout, "a client with no timeout can hang forever")

	out, err := DoJSON[payload](context.Background(), c, Request{
		Method: http.MethodPost, URL: srv.URL, Body: payload{Name: "x"}, ServiceName: "svc",
	})
	require.NoError(t, err)

	assert.Equal(t, "ok", out.Name)
	assert.Equal(t, "application/json", accept)
	assert.Equal(t, "application/json", contentType)
	assert.Equal(t, "httpclient (Go)", ua, "the default user agent must name no product")
}

func TestWithUserAgent(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, err := Do(context.Background(), New(WithUserAgent("rednev/1.0")), Request{Method: http.MethodGet, URL: srv.URL})
	require.NoError(t, err)
	assert.Equal(t, "rednev/1.0", ua)
}

func TestWithTransport_isWrappedByTheJSONDefaults(t *testing.T) {
	var accept string
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		accept = r.Header.Get("Accept")
		return nil, errors.New("boom")
	})

	_, err := Do(context.Background(), New(WithTransport(rt)), Request{
		Method: http.MethodGet, URL: "http://example.invalid", Retry: NoRetry(),
	})
	require.Error(t, err)
	assert.Equal(t, "application/json", accept)
}

func TestDo_usesADefaultClientWhenGivenNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"ok"}`))
	}))
	defer srv.Close()

	resp, err := Do(context.Background(), nil, Request{Method: http.MethodGet, URL: srv.URL})
	require.NoError(t, err)
	assert.True(t, resp.OK())
}

func TestDo_getHasNoContentType(t *testing.T) {
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, err := Do(context.Background(), New(), Request{Method: http.MethodGet, URL: srv.URL})
	require.NoError(t, err)
	assert.Empty(t, contentType, "a bodyless GET must not claim a JSON body")
}

func TestDo_rawHeadersKeepExactSpelling(t *testing.T) {
	var canonical, raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		canonical = r.Header.Get("access_token")
		for k := range r.Header {
			if k == "Access_token" {
				raw = k
			}
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, err := Do(context.Background(), New(), Request{
		Method:     http.MethodGet,
		URL:        srv.URL,
		RawHeaders: map[string]string{"access_token": "secret"},
	})
	require.NoError(t, err)
	assert.Equal(t, "secret", canonical)
	assert.Equal(t, "Access_token", raw, "the server canonicalizes on receipt; the wire kept our spelling")
}

func TestPrepare_seesEveryHeaderAndRunsClientFirst(t *testing.T) {
	var order []string
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := New(WithPrepare(func(r *http.Request) error {
		order = append(order, "client")
		return nil
	}))

	_, err := Do(context.Background(), client, Request{
		Method:     http.MethodPost,
		URL:        srv.URL,
		Body:       payload{Name: "x"},
		Headers:    map[string]string{"X-Trace": "abc"},
		RawHeaders: map[string]string{"access_token": "secret"},
		Prepare: func(r *http.Request) error {
			order = append(order, "request")
			seen = r.Header.Clone()
			r.Header.Set("X-Signature", "signed")
			return nil
		},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"client", "request"}, order)
	assert.Equal(t, "abc", seen.Get("X-Trace"))
	assert.Equal(t, []string{"secret"}, rawHeader(seen, "access_token"))
	assert.Equal(t, "application/json", seen.Get("Content-Type"))
	assert.Equal(t, defaultUserAgent, seen.Get("User-Agent"))
}

func TestPrepare_runsOnEveryAttemptAndItsFailureIsNotRetried(t *testing.T) {
	rt, calls := stubTransport(stub{status: http.StatusBadGateway})

	var prepared int
	_, err := Do(context.Background(), New(WithTransport(rt)), Request{
		Method: http.MethodGet, URL: "http://example.invalid", Retry: noWait(),
		Prepare: func(*http.Request) error { prepared++; return nil },
	})
	require.NoError(t, err)
	assert.Equal(t, 3, prepared, "a signature must be recomputed per attempt, not replayed")
	assert.Equal(t, 3, *calls)

	rt, calls = stubTransport(stub{status: http.StatusOK})
	prepared = 0
	_, err = Do(context.Background(), New(WithTransport(rt)), Request{
		Method: http.MethodGet, URL: "http://example.invalid", Retry: noWait(),
		Prepare: func(*http.Request) error { prepared++; return errors.New("no token") },
	})
	require.ErrorContains(t, err, "no token")
	assert.Equal(t, 1, prepared, "a signer that cannot sign will not sign on the next attempt either")
	assert.Zero(t, *calls)
}

func TestObserver_recordsOneEventPerAttempt(t *testing.T) {
	rt, _ := stubTransport(
		stub{status: http.StatusBadGateway},
		stub{status: http.StatusOK, body: `{"name":"ok"}`},
	)

	var events []Event
	client := New(WithTransport(rt), WithObserver(func(_ context.Context, e Event) {
		events = append(events, e)
	}))

	_, err := Do(context.Background(), client, Request{
		Method: http.MethodGet, URL: "http://api.example.com/v1/orders", ServiceName: "svc",
		Query: url.Values{"access_token": {"secret"}}, Retry: noWait(),
	})
	require.NoError(t, err)

	require.Len(t, events, 2)
	assert.Equal(t, 1, events[0].Attempt)
	assert.Equal(t, http.StatusBadGateway, events[0].Status)
	assert.Equal(t, 2, events[1].Attempt)
	assert.Equal(t, http.StatusOK, events[1].Status)

	for _, e := range events {
		assert.Equal(t, "svc", e.Service)
		assert.Equal(t, http.MethodGet, e.Method)
		assert.Equal(t, "api.example.com/v1/orders", e.URL)
		assert.NotContains(t, e.URL, "secret", "a query string routinely carries tokens")
		assert.NoError(t, e.Err)
	}
}

func TestObserver_recordsTransportFailures(t *testing.T) {
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial refused") })

	var events []Event
	client := New(WithTransport(rt), WithObserver(func(_ context.Context, e Event) { events = append(events, e) }))

	_, err := Do(context.Background(), client, Request{
		Method: http.MethodGet, URL: "http://api.example.com/v1/orders", Retry: noWait(),
	})
	require.Error(t, err)

	require.Len(t, events, 3)
	for _, e := range events {
		assert.Zero(t, e.Status)
		assert.ErrorContains(t, e.Err, "dial refused")
	}
}

func TestWithMaxResponseBytes(t *testing.T) {
	tests := []struct {
		name    string
		limit   int64
		body    string
		wantErr bool
	}{
		{name: "under the limit", limit: 32, body: `{"name":"ok"}`},
		{name: "exactly at the limit", limit: 13, body: `{"name":"ok"}`},
		{name: "one byte over the limit", limit: 12, body: `{"name":"ok"}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			resp, err := Do(context.Background(), New(WithMaxResponseBytes(tt.limit)), Request{
				Method: http.MethodGet, URL: srv.URL, ServiceName: "svc", Retry: noWait(),
			})
			if tt.wantErr {
				require.ErrorIs(t, err, ErrResponseTooLarge)
				assert.Nil(t, resp)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.body, string(resp.Body))
		})
	}
}

func TestWithMaxResponseBytes_oversizedBodyIsNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()

	_, err := Do(context.Background(), New(WithMaxResponseBytes(10)), Request{
		Method: http.MethodGet, URL: srv.URL, Retry: noWait(),
	})
	require.ErrorIs(t, err, ErrResponseTooLarge)
	assert.Equal(t, 1, calls, "the same endpoint answers the same size again")
}

func TestOptions_ignoreMeaninglessValues(t *testing.T) {
	c := New(WithTimeout(0), WithUserAgent(""), WithMaxResponseBytes(0), WithTransport(nil))

	assert.Equal(t, defaultTimeout, c.Timeout)
	cfg := settingsOf(c)
	assert.Equal(t, defaultUserAgent, cfg.userAgent)
	assert.Equal(t, int64(defaultMaxResponseBytes), cfg.maxResponseBytes)
	assert.NotNil(t, c.Transport.(*jsonTransport).base)
}

func TestSettingsOf_foreignClientGetsTheDefaults(t *testing.T) {
	cfg := settingsOf(&http.Client{})
	assert.Equal(t, defaultUserAgent, cfg.userAgent)
	assert.Equal(t, int64(defaultMaxResponseBytes), cfg.maxResponseBytes)
}

func TestWithTimeout(t *testing.T) {
	assert.Equal(t, 5*time.Second, New(WithTimeout(5*time.Second)).Timeout)
}

func TestObservedURL_dropsWhatItCannotParse(t *testing.T) {
	assert.Equal(t, "api.example.com/v1", observedURL("https://api.example.com/v1?token=secret"))
	assert.Empty(t, observedURL("://nope"))
}

func TestDo_reportsABodyItCannotRead(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(iotest.ErrReader(errors.New("connection reset"))),
			Request:    r,
		}, nil
	})

	_, err := Do(context.Background(), New(WithTransport(rt)), Request{
		Method: http.MethodGet, URL: "http://example.invalid", ServiceName: "svc", Retry: NoRetry(),
	})
	require.ErrorContains(t, err, "read svc response")
}

func TestSettingsOf_replacingTheTransportAfterNewRevertsToTheDefaults(t *testing.T) {
	var seen http.Header
	c := New(WithUserAgent("rednev/1.0"), WithMaxResponseBytes(4))
	c.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"name":"ok"}`)),
			Request:    r,
		}, nil
	})

	cfg := settingsOf(c)
	assert.Equal(t, defaultUserAgent, cfg.userAgent)
	assert.Equal(t, int64(defaultMaxResponseBytes), cfg.maxResponseBytes)

	resp, err := Do(context.Background(), c, Request{Method: http.MethodGet, URL: "http://example.invalid"})
	require.NoError(t, err)
	assert.Equal(t, `{"name":"ok"}`, string(resp.Body), "the 4-byte bound went with the transport")
	assert.Equal(t, "application/json", seen.Get("Accept"))
	assert.Equal(t, defaultUserAgent, seen.Get("User-Agent"), "Do still applies the per-request defaults")
}

func TestDo_aLowercaseRawHeaderDoesNotDuplicateTheDefault(t *testing.T) {
	var contentType []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header["Content-Type"]
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, err := Do(context.Background(), New(), Request{
		Method: http.MethodPost, URL: srv.URL, Body: payload{Name: "x"},
		RawHeaders: map[string]string{"content-type": "text/csv"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"text/csv"}, contentType, "one Content-Type went out, not two")
}

func TestDo_aPerRequestUserAgentWinsOverTheClientOne(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, err := Do(context.Background(), New(WithUserAgent("rednev/1.0")), Request{
		Method: http.MethodGet, URL: srv.URL,
		Headers: map[string]string{"User-Agent": "caller/2.0"},
	})
	require.NoError(t, err)
	assert.Equal(t, "caller/2.0", ua)
}

func TestJSONTransport_appliesTheDefaultsToADirectClientCall(t *testing.T) {
	var accept, ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept, ua = r.Header.Get("Accept"), r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	resp, err := New(WithUserAgent("rednev/1.0")).Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "application/json", accept, "a caller that never touches Do still gets the defaults")
	assert.Equal(t, "rednev/1.0", ua)
}
