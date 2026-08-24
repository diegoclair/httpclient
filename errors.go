package httpclient

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrResponseTooLarge reports a body past the client's size bound; the body is
// discarded rather than truncated, since half a JSON document decodes to noise.
var ErrResponseTooLarge = errors.New("response too large")

// StatusError carries a non-2xx response. An adapter that wraps it in its own
// error must expose it through Unwrap, or the Is* helpers stop working.
type StatusError struct {
	ServiceName string
	StatusCode  int
	Body        string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("%s returned status %d: %s", e.ServiceName, e.StatusCode, e.Body)
	}
	return fmt.Sprintf("%s returned status %d", e.ServiceName, e.StatusCode)
}

// Marks a deterministic failure, which the retry policy must not spend an
// attempt on.
type permanentError struct{ err error }

func permanent(err error) error { return &permanentError{err: err} }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// StatusCode returns 0 when err is not an HTTP-status error.
func StatusCode(err error) int {
	if statusErr, ok := errors.AsType[*StatusError](err); ok {
		return statusErr.StatusCode
	}
	return 0
}

// IsStatus reports whether err carries exactly this HTTP status.
func IsStatus(err error, code int) bool { return StatusCode(err) == code }

// IsBadRequest reports a 400.
func IsBadRequest(err error) bool { return IsStatus(err, http.StatusBadRequest) }

// IsUnauthorized reports a 401 or a 403.
func IsUnauthorized(err error) bool {
	return IsStatus(err, http.StatusUnauthorized) || IsStatus(err, http.StatusForbidden)
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool { return IsStatus(err, http.StatusNotFound) }

// IsConflict reports a 409.
func IsConflict(err error) bool { return IsStatus(err, http.StatusConflict) }

// IsTooManyRequests reports a 429.
func IsTooManyRequests(err error) bool { return IsStatus(err, http.StatusTooManyRequests) }

// IsServerError reports any 5xx.
func IsServerError(err error) bool { return StatusCode(err) >= 500 }
