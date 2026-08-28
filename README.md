# httpclient

<p align="center">
 <b>Shared plumbing for outbound JSON API calls in Go — retries with Retry-After, per-request signing hooks and per-attempt observability, on the standard library alone</b><br>
    <a href="https://github.com/diegoclair/httpclient/tags" alt="GitHub tag">
     <img src="https://img.shields.io/github/tag/diegoclair/httpclient.svg" />
    </a>
    <a href='https://coveralls.io/github/diegoclair/httpclient?branch=main'>
     <img src='https://coveralls.io/repos/github/diegoclair/httpclient/badge.svg?branch=main' alt='Coverage Status' />
    </a>
    <a href="https://github.com/diegoclair/httpclient/actions">
     <img src="https://github.com/diegoclair/httpclient/actions/workflows/ci.yaml/badge.svg" alt="build status">
    </a>
    <a href="https://pkg.go.dev/github.com/diegoclair/httpclient">
     <img src="https://pkg.go.dev/badge/github.com/diegoclair/httpclient.svg" alt="Go Reference">
    </a>
    <a href="https://opensource.org/licenses/MIT">
     <img src="https://img.shields.io/badge/License-MIT-yellow.svg" />
    </a>
</p>

An adapter for a third-party API keeps only what is actually specific to that
provider — base URL, auth injection and error-envelope parsing — and delegates
the mechanics here: marshal, query building, execute, size-bounded read, decode
and retry.

The client it hands back is a plain `*http.Client`, so nothing about it is
exotic: pass it to any library that takes one. **Zero third-party
dependencies** — `testify` is used by the tests only.

## Install

```sh
go get github.com/diegoclair/httpclient
```

Requires **Go 1.27+** (`encoding/json/v2` and `testing/synctest`).

## Quickstart

A non-2xx status is **not** an error: it comes back in the `Response`, because
only the adapter knows how to read its provider's error envelope. `Do` returns
an error for infrastructure failures alone — bad URL, marshal failure,
transport, context.

```go
type Client struct {
    http  *http.Client
    token string
}

func New(token string) *Client {
    return &Client{
        http: httpclient.New(
            httpclient.WithUserAgent("rednev/1.0"),
            httpclient.WithTimeout(15*time.Second),
        ),
        token: token,
    }
}

// The Bot API answers 200 with ok:false, and states its own rate-limit wait in
// the envelope — so the envelope is what decides, not the status line.
type envelope struct {
    OK          bool           `json:"ok"`
    Result      jsontext.Value `json:"result"`
    Description string         `json:"description"`
    Parameters  struct {
        RetryAfter int `json:"retry_after"`
    } `json:"parameters"`
}

type apiError struct {
    *httpclient.StatusError
    Description string
}

func (e *apiError) Error() string { return "telegram: " + e.Description }
func (e *apiError) Unwrap() error { return e.StatusError }

func (c *Client) call(ctx context.Context, method string, in, out any) error {
    resp, err := httpclient.Do(ctx, c.http, httpclient.Request{
        Method:      http.MethodPost,
        URL:         fmt.Sprintf("https://api.telegram.org/bot%s/%s", c.token, method),
        Body:        in,
        ServiceName: "telegram",
        Retry: &httpclient.RetryPolicy{
            RetryWrites: true,
            RetryAfter:  retryAfterFromEnvelope, // see "Retry" below
        },
    })
    if err != nil {
        return err
    }

    body, err := httpclient.Decode[envelope](resp)
    if err != nil {
        return err
    }
    if !body.OK {
        return &apiError{
            StatusError: &httpclient.StatusError{
                ServiceName: resp.ServiceName,
                StatusCode:  resp.StatusCode,
                Body:        string(resp.Body),
                Header:      resp.Header,
            },
            Description: body.Description,
        }
    }
    if out == nil {
        return nil
    }
    return json.Unmarshal(body.Result, out)
}
```

Because `apiError` exposes the `*StatusError` through `Unwrap`, the helpers keep
working on the adapter's own type:

```go
if httpclient.IsTooManyRequests(err) { ... }
if httpclient.IsUnauthorized(err) { ... } // 401 or 403
switch httpclient.StatusCode(err) { ... } // 0 when err is not an HTTP status
```

When the provider needs no envelope parsing, `DoJSON` is `Do` + status check +
`Decode` in one call:

```go
order, resp, err := httpclient.DoJSON[Order](ctx, c.http, httpclient.Request{
    Method: http.MethodGet,
    URL:    "https://api.mercadolibre.com/orders/" + id,
    Query:  url.Values{"attributes": {"id,status"}},
})
```

The response comes back on the status-error path too, so a caller that only
wants the decoded value discards it and the one that needs `Retry-After`, a
`Location` or the exact 2xx reads it off `resp`:

```go
order, _, err := httpclient.DoJSON[Order](ctx, c.http, req)
```

Use `httpclient.Empty` as the type parameter for a call that answers no body;
a POST whose only useful answer is a header is the case `resp` is there for.

## Options

| Option | Effect |
| --- | --- |
| `WithTimeout(d)` | Per-request timeout (default 20s); a client without one can hang forever |
| `WithUserAgent(ua)` | Names the integration (default `httpclient (Go)`); a per-request header still wins |
| `WithTransport(rt)` | Replaces the `http.RoundTripper` the JSON defaults are layered on (see [Composing with other transports](#design-notes)) |
| `WithPrepare(fn)` | Hook run on every request of this client, before `Request.Prepare` |
| `WithObserver(o)` | Reports every attempt (noop by default) |
| `WithMaxResponseBytes(n)` | Bounds the body read (default 8 MiB) |

`New` returns an `*http.Client` whose transport fills in `Accept`,
`Content-Type` (only when there is a body — a bodyless `GET` claiming JSON
confuses some APIs) and `User-Agent`, unless the call already set them.

## Request

| Field | Effect |
| --- | --- |
| `Method`, `URL`, `Query` | `Query` is merged into whatever the URL already carries |
| `Body` | JSON-marshaled when non-nil |
| `Headers` | Set through `Header.Set`, so the name is canonicalized |
| `RawHeaders` | Written straight into the map, preserving exact spelling |
| `Prepare` | Runs per attempt after the headers, before the send |
| `ServiceName` | Prefixes every error this call produces |
| `Retry` | `nil` means the default policy; `httpclient.NoRetry()` means one attempt |

### Prepare: signing and rotating credentials

`Prepare` receives the fully built `*http.Request` — headers, body and final
URL — and runs immediately before it goes out, **on every attempt**. An HMAC
over path plus timestamp plus body, or a bearer token that may have rotated
since the first try, is therefore recomputed rather than replayed. Read the
body through `r.GetBody()`, never `r.Body` — draining `r.Body` sends an empty
request:

```go
Prepare: func(r *http.Request) error {
    ts := strconv.FormatInt(time.Now().Unix(), 10)
    r.Header.Set("Authorization", sign(r.URL.Path, ts, body))
    r.Header.Set("X-Timestamp", ts)
    return nil
},
```

Its failure is treated as permanent: a signer that cannot sign will not sign on
the next attempt either, so no retry budget is spent on it.

## Retry

The default policy is three attempts on **reads only** — a retry after an
ambiguous write can duplicate the resource, so `RetryWrites` is opt-in and
belongs to endpoints that take an idempotency key. Retried are `429`, every
`5xx` and transport failures; a `4xx` other than `429` is the caller's own fault
and fails identically on the next try. What ends the call for good is the
**caller's own context**: once `ctx` is cancelled or past its deadline nothing
is retried, backoff included. A `Client.Timeout` is not that — it is the most
common transient failure there is, and it gets another attempt. Every attempt
sends the same body bytes.

| Field | Effect |
| --- | --- |
| `MaxAttempts` | Counts the first try (default 3) |
| `Backoff` | Base of the exponential backoff (default 200ms); `0` waits not at all |
| `MaxBackoff` | Ceiling for a single wait (default 30s) |
| `RetryWrites` | Allow retrying non-idempotent methods |
| `RetryAfter` | Reads the wait out of the provider's own envelope |

The wait is **full jitter**: uniform over `[0, min(Backoff<<(attempt-1),
MaxBackoff))`. Fleets that fail together retry together; drawing the whole
interval is what pulls them apart again.

When the server says how long to wait, that instruction wins and is used as
given — never jittered, only capped by `MaxBackoff` so one hostile header cannot
park a goroutine for an hour. The order is `RetryPolicy.RetryAfter` first, then
the `Retry-After` header (both a delay in seconds and an HTTP-date are
understood):

```go
Retry: &httpclient.RetryPolicy{
    MaxAttempts: 4,
    RetryAfter: func(resp *httpclient.Response) time.Duration {
        var e envelope
        if err := json.Unmarshal(resp.Body, &e); err != nil {
            return 0 // fall back to the Retry-After header
        }
        return time.Duration(e.Parameters.RetryAfter) * time.Second
    },
}
```

## Observability

The library never logs. It reports one `Event` per attempt — retries included —
and what to do with it is yours: a metric, a span, a log line:

```go
client := httpclient.New(httpclient.WithObserver(func(ctx context.Context, e httpclient.Event) {
    metrics.Observe(e.Service, e.Method, e.Status, e.Duration)
    if e.Err != nil {
        log.Warn(ctx, "call failed", "service", e.Service, "attempt", e.Attempt, "err", e.Err)
    }
}))
```

```go
type Event struct {
    Service  string
    Method   string
    URL      string // host and path only
    Status   int    // zero when the attempt never reached a response
    Attempt  int
    Duration time.Duration
    Err      error
}
```

`URL` carries no query string: it routinely holds tokens, and an observability
pipeline is the last place they should land.

## Design notes

**A `*http.Client`, not a wrapper type.** `New` returns the standard type, and
`Do` takes one — so the client composes with anything in the ecosystem and a
caller can bring its own. The client-level settings ride on the transport, which
is why `Do` can read them back off a plain `*http.Client` and falls back to the
defaults for one this package did not build.

**Composing with other transports.** The client-level settings ride on the
transport `New` installs, so an instrumented or authenticated `RoundTripper`
goes *underneath* it, through the option:

```go
client := httpclient.New(
    httpclient.WithTransport(otelhttp.NewTransport(http.DefaultTransport)),
)
```

Assigning `client.Transport` after `New` wraps that transport away and silently
takes every client-level setting with it — user agent, `Prepare`, observer and
response bound all fall back to the package defaults. `WithTransport` is the
composition point.

**Bounded reads.** A body past `WithMaxResponseBytes` is discarded, not
truncated: half a JSON document decodes to noise or, worse, to a plausible wrong
value. The error wraps `ErrResponseTooLarge` and is not retried.

**`encoding/json/v2`.** Field names match case-sensitively and duplicate JSON
names are rejected — both silently tolerated by v1, and both a real source of
wrong values from providers that emit `id` and `ID` in the same object.

**Errors are plain.** `fmt.Errorf("httpclient: …: %w")`, nothing else, so the
library imposes no error taxonomy on whoever adopts it.

## License

MIT — see [LICENSE](LICENSE).
