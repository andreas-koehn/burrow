package aigateway

import (
	"encoding/json"
	"net/http"
)

// anthropicErrorType maps an HTTP status to the error type Anthropic clients
// switch on. Burrow's own code travels next to it.
// (The same mapping as ErrorType in internal/aigw/translate/messages/response.go.)
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

// WriteAnthropicError writes an error in the shape Anthropic-format clients
// parse. burrow_code is an extra field those clients ignore; it is repeated
// in the Burrow-Error-Code header.
func WriteAnthropicError(w http.ResponseWriter, status int, code, message string) {
	type detail struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Burrow-Error-Code", code)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Type       string `json:"type"`
		Error      detail `json:"error"`
		BurrowCode string `json:"burrow_code"`
	}{"error", detail{anthropicErrorType(status), message}, code})
}
