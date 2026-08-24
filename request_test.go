package httpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDo_buildsQuery(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, err := Do(context.Background(), New(), Request{
		Method: http.MethodGet,
		URL:    srv.URL + "/path?keep=1",
		Query:  url.Values{"limit": {"10"}},
	})
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "limit=10")
	assert.Contains(t, gotQuery, "keep=1", "query params already on the URL must survive")
}

func TestDo_rejectsAnUnparseableURL(t *testing.T) {
	_, err := Do(context.Background(), New(), Request{
		Method: http.MethodGet, URL: "://nope", Query: url.Values{"a": {"1"}},
	})
	require.ErrorContains(t, err, "parse url")
}

func TestDo_rejectsAnUnmarshalableBody(t *testing.T) {
	_, err := Do(context.Background(), New(), Request{
		Method: http.MethodPost, URL: "http://example.invalid", Body: make(chan int),
	})
	require.ErrorContains(t, err, "marshal request body")
}

func TestDo_rejectsABadMethodWithoutSpendingRetries(t *testing.T) {
	rt, calls := stubTransport(stub{status: http.StatusOK})

	_, err := Do(context.Background(), New(WithTransport(rt)), Request{
		Method: "in valid", URL: "http://example.invalid", Retry: noWait(),
	})
	require.ErrorContains(t, err, "build request")
	assert.Zero(t, *calls)
}

func TestDo_statusIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"nope"}`))
	}))
	defer srv.Close()

	resp, err := Do(context.Background(), New(), Request{Method: http.MethodGet, URL: srv.URL, ServiceName: "svc", Retry: noWait()})
	require.NoError(t, err, "Do reports transport failures only; the caller owns the status")
	assert.False(t, resp.OK())
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	err = resp.Err()
	require.Error(t, err)
	assert.True(t, IsNotFound(err))
	assert.Equal(t, http.StatusNotFound, StatusCode(err))
	assert.Contains(t, err.Error(), "svc")
}

func TestDo_exposesResponseHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "abc")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	resp, err := Do(context.Background(), New(), Request{Method: http.MethodGet, URL: srv.URL})
	require.NoError(t, err)
	assert.Equal(t, "abc", resp.Header.Get("X-Request-Id"))
}

func TestDo_namesTheServiceWhenTheCallerDidNot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	resp, err := Do(context.Background(), New(), Request{Method: http.MethodGet, URL: srv.URL, Retry: NoRetry()})
	require.NoError(t, err)
	assert.Equal(t, "service", resp.ServiceName)
}

func TestDoJSON_failsOnNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	_, err := DoJSON[payload](context.Background(), New(), Request{Method: http.MethodGet, URL: srv.URL, Retry: noWait()})
	require.Error(t, err)
	assert.True(t, IsConflict(err))
}

func TestDoJSON_propagatesTransportFailures(t *testing.T) {
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial refused") })

	_, err := DoJSON[payload](context.Background(), New(WithTransport(rt)), Request{
		Method: http.MethodGet, URL: "http://example.invalid", ServiceName: "svc", Retry: NoRetry(),
	})
	require.ErrorContains(t, err, "calling svc")
	assert.Zero(t, StatusCode(err))
}

func TestDecode(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr string
	}{
		{name: "matching field", body: `{"name":"ok"}`, want: "ok"},
		{name: "unknown members are ignored", body: `{"name":"ok","extra":1}`, want: "ok"},
		{name: "field names are case-sensitive", body: `{"Name":"ok"}`, want: ""},
		{name: "duplicate names are rejected", body: `{"name":"a","name":"b"}`, wantErr: "duplicate"},
		{name: "invalid json", body: `{`, wantErr: "decode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Decode[payload](&Response{StatusCode: http.StatusOK, Body: []byte(tt.body), ServiceName: "svc"})
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, out.Name)
		})
	}
}

func TestDecode_emptyBody(t *testing.T) {
	out, err := Decode[Empty](&Response{StatusCode: http.StatusNoContent})
	require.NoError(t, err)
	assert.NotNil(t, out)
}

func TestBuildURL_rejectsAnUnparseableBaseQuery(t *testing.T) {
	for _, base := range []string{"http://example.com/p?a=%zz", "http://example.com/p?a=1;b=2"} {
		_, err := buildURL(base, url.Values{"c": {"1"}})
		assert.Error(t, err, base)
	}
}

func TestBuildURL(t *testing.T) {
	tests := []struct {
		name  string
		base  string
		query url.Values
		want  string
	}{
		{
			name:  "every value of a multi-valued key survives",
			base:  "http://example.com/p",
			query: url.Values{"id": {"1", "2", "3"}},
			want:  "http://example.com/p?id=1&id=2&id=3",
		},
		{
			name:  "the request query replaces that key and keeps the others",
			base:  "http://example.com/p?id=old&keep=1",
			query: url.Values{"id": {"1", "2"}},
			want:  "http://example.com/p?id=1&id=2&keep=1",
		},
		{
			name: "an empty query leaves the URL byte-identical",
			base: "http://example.com/p?b=2&a=1&t=%7E",
			want: "http://example.com/p?b=2&a=1&t=%7E",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildURL(tt.base, tt.query)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDo_sendsEveryValueOfAMultiValuedQuery(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, err := Do(context.Background(), New(), Request{
		Method: http.MethodGet, URL: srv.URL + "/path?keep=1",
		Query: url.Values{"id": {"1", "2", "3"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"1", "2", "3"}, got["id"])
	assert.Equal(t, []string{"1"}, got["keep"])
}

func TestDo_anInvalidURLIsPermanent(t *testing.T) {
	rt, calls := stubTransport(stub{status: http.StatusOK})

	var attempts int
	client := New(WithTransport(rt), WithObserver(func(context.Context, Event) { attempts++ }))

	_, err := Do(context.Background(), client, Request{
		Method: http.MethodGet, URL: "://nope", Retry: noWait(),
	})
	require.ErrorContains(t, err, "build request")
	assert.Equal(t, 1, attempts, "a URL http.NewRequest rejects is rejected again next attempt")
	assert.Zero(t, *calls)
}

func TestResponse_OKBoundaries(t *testing.T) {
	for code, want := range map[int]bool{199: false, 200: true, 299: true, 300: false} {
		assert.Equal(t, want, (&Response{StatusCode: code}).OK(), "status %d", code)
	}
}
