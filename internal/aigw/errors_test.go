package aigw_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw"
)

// An entry point may replace the shape of the errors the chain writes.
func TestChain_ErrorWriterFromContext(t *testing.T) {
	chain := aigw.NewChain(nil, nil, nil, nil, nil, nil, nil, nil, testLog())
	chain.MaxRequestBodyBytes = 8
	up := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("upstream reached") })
	body := `{"model":"m","messages":[]}`

	// Default: the shape host-routed services have always had.
	rec := httptest.NewRecorder()
	chain.DispatchMetered(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)), "svc1", "h", "", "", up)
	if rec.Code != http.StatusRequestEntityTooLarge || rec.Body.String() != `{"error":"request body too large"}` {
		t.Fatalf("default: status %d body %s", rec.Code, rec.Body.String())
	}

	var gotStatus int
	var gotCode string
	ew := func(w http.ResponseWriter, status int, code, _ string) {
		gotStatus, gotCode = status, code
		w.WriteHeader(status)
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req = req.WithContext(aigw.WithErrorWriter(req.Context(), ew))
	rec = httptest.NewRecorder()
	chain.DispatchMetered(rec, req, "svc1", "h", "", "", up)
	if gotStatus != http.StatusRequestEntityTooLarge || gotCode != "request_too_large" || rec.Body.Len() != 0 {
		t.Fatalf("custom writer: status %d code %q body %s", gotStatus, gotCode, rec.Body.String())
	}
}
