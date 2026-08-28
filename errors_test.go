package httpclient

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStatusError_Error(t *testing.T) {
	assert.Equal(t, "svc returned status 404: nope",
		(&StatusError{ServiceName: "svc", StatusCode: 404, Body: "nope"}).Error())
	assert.Equal(t, "svc returned status 404",
		(&StatusError{ServiceName: "svc", StatusCode: 404}).Error())
}

func TestStatusError_ErrorTruncatesTheBody(t *testing.T) {
	html := strings.Repeat("x", errorBodyLimit*3)
	err := &StatusError{ServiceName: "svc", StatusCode: 502, Body: html}

	assert.Equal(t, "svc returned status 502: "+strings.Repeat("x", errorBodyLimit)+"...", err.Error())
	assert.Len(t, err.Body, errorBodyLimit*3, "only the message is trimmed; parsing still needs the whole body")
}

func TestStatusHelpers(t *testing.T) {
	statusErr := func(code int) error {
		return fmt.Errorf("wrapped: %w", &StatusError{ServiceName: "svc", StatusCode: code})
	}

	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{name: "bad request", got: IsBadRequest(statusErr(http.StatusBadRequest)), want: true},
		{name: "unauthorized on 401", got: IsUnauthorized(statusErr(http.StatusUnauthorized)), want: true},
		{name: "unauthorized on 403", got: IsUnauthorized(statusErr(http.StatusForbidden)), want: true},
		{name: "not found", got: IsNotFound(statusErr(http.StatusNotFound)), want: true},
		{name: "conflict", got: IsConflict(statusErr(http.StatusConflict)), want: true},
		{name: "too many requests", got: IsTooManyRequests(statusErr(http.StatusTooManyRequests)), want: true},
		{name: "server error", got: IsServerError(statusErr(http.StatusBadGateway)), want: true},
		{name: "server error on 404", got: IsServerError(statusErr(http.StatusNotFound)), want: false},
		{name: "server error on 500", got: IsServerError(statusErr(500)), want: true},
		{name: "server error on 499", got: IsServerError(statusErr(499)), want: false},
		{name: "not found on 409", got: IsNotFound(statusErr(http.StatusConflict)), want: false},
		{name: "plain error", got: IsNotFound(errors.New("boom")), want: false},
		{name: "nil error", got: IsServerError(nil), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.got)
		})
	}

	assert.Zero(t, StatusCode(errors.New("boom")))
	assert.Zero(t, StatusCode(nil))
}

func TestPermanentError_unwrapsToItsCause(t *testing.T) {
	err := permanent(fmt.Errorf("over the limit: %w", ErrResponseTooLarge))

	assert.ErrorIs(t, err, ErrResponseTooLarge)
	assert.Equal(t, "over the limit: response too large", err.Error())
}
