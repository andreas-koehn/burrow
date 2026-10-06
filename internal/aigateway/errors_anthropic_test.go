package aigateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteAnthropicError(t *testing.T) {
	cases := []struct {
		status int
		typ    string
	}{
		{400, "invalid_request_error"}, {401, "authentication_error"}, {403, "permission_error"},
		{404, "not_found_error"}, {405, "invalid_request_error"}, {413, "request_too_large"},
		{429, "rate_limit_error"}, {500, "api_error"}, {502, "api_error"}, {503, "overloaded_error"}, {504, "api_error"},
		{529, "overloaded_error"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		WriteAnthropicError(rec, c.status, "some_code", "something happened")
		if rec.Code != c.status || rec.Header().Get("Burrow-Error-Code") != "some_code" || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%d: code %d headers %v", c.status, rec.Code, rec.Header())
		}
		want := `{"type":"error","error":{"type":"` + c.typ + `","message":"something happened"},"burrow_code":"some_code"}`
		if strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("%d: body = %s", c.status, rec.Body.String())
		}
	}
}
