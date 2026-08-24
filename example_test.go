package httpclient_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"

	"github.com/diegoclair/httpclient"
)

// telegramError keeps the vendor's own description and exposes the status
// through Unwrap, so the Is* helpers keep working on the adapter's typed error.
type telegramError struct {
	*httpclient.StatusError
	Description string
}

func (e *telegramError) Error() string {
	return fmt.Sprintf("telegram: %s (%d)", e.Description, e.StatusCode)
}

func (e *telegramError) Unwrap() error { return e.StatusError }

func ExampleStatusError() {
	resp := &httpclient.Response{
		StatusCode:  http.StatusNotFound,
		Body:        []byte(`{"ok":false,"description":"chat not found","result":null}`),
		ServiceName: "telegram",
	}

	var envelope struct {
		OK          bool           `json:"ok"`
		Description string         `json:"description"`
		Result      jsontext.Value `json:"result"`
	}
	if err := json.Unmarshal(resp.Body, &envelope); err != nil {
		panic(err)
	}

	statusErr, _ := resp.Err().(*httpclient.StatusError)
	var err error = &telegramError{StatusError: statusErr, Description: envelope.Description}

	fmt.Println(err)
	fmt.Println(httpclient.IsNotFound(err), httpclient.StatusCode(err))
	// Output:
	// telegram: chat not found (404)
	// true 404
}
