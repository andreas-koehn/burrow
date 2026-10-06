// Package aigateway serves the /ai/ namespace: model providers addressed as
// https://<domain>/ai/<provider>/v1/…
package aigateway

import (
	"encoding/json"
	"net/http"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// WriteError writes an error in the shape OpenAI-compatible clients parse.
// Every error under /ai/ and /openai/ uses it, so a client never receives an
// HTML page. The code is repeated in the Burrow-Error-Code header.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Burrow-Error-Code", code)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Message: message, Type: "burrow_error", Code: code}})
}
