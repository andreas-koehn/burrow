package aigw

import (
	"context"
	"net/http"
)

// ErrorWriter writes an error response that Burrow itself originates.
type ErrorWriter func(w http.ResponseWriter, status int, code, message string)

type errorWriterKey struct{}

// WithErrorWriter makes the chain, and the middleware it calls, write their
// own errors through ew for this request. An entry point with its own error
// shape (the /ai/ gateway) sets it; without one the errors keep the shape
// host-routed services have always had.
func WithErrorWriter(ctx context.Context, ew ErrorWriter) context.Context {
	return context.WithValue(ctx, errorWriterKey{}, ew)
}

// ErrorWriterFrom returns the request's error writer, or nil when none is set.
func ErrorWriterFrom(ctx context.Context) ErrorWriter {
	ew, _ := ctx.Value(errorWriterKey{}).(ErrorWriter)
	return ew
}

// writeError writes a chain-originated error: through the request's error
// writer when one is set, else as the legacy {"error":"<legacy>"} envelope.
func writeError(w http.ResponseWriter, r *http.Request, status int, legacy, code, message string) {
	if ew := ErrorWriterFrom(r.Context()); ew != nil {
		ew(w, status, code, message)
		return
	}
	writeJSONError(w, status, legacy)
}
