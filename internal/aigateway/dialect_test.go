package aigateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/aigw"
)

func TestWriteError_OpenAIShapeAndHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, 404, "model_not_found", "unknown model x")
	if rec.Code != 404 || rec.Header().Get("Burrow-Error-Code") != "model_not_found" {
		t.Fatalf("code %d header %q", rec.Code, rec.Header().Get("Burrow-Error-Code"))
	}
	want := `{"error":{"message":"unknown model x","type":"burrow_error","code":"model_not_found"}}`
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// fail writes through the request's error writer when one is set.
func TestGatewayFail_UsesRequestErrorWriter(t *testing.T) {
	g := newGateway(http.NotFoundHandler(), nil)
	called := ""
	ew := func(w http.ResponseWriter, status int, code, message string) { called = code; w.WriteHeader(status) }
	r := httptest.NewRequest("GET", "/", nil)
	r = r.WithContext(aigw.WithErrorWriter(r.Context(), ew))
	g.fail(httptest.NewRecorder(), r, 403, "forbidden", "no")
	if called != "forbidden" {
		t.Fatal("the request's error writer was not used")
	}
	rec := httptest.NewRecorder()
	g.fail(rec, httptest.NewRequest("GET", "/", nil), 403, "forbidden", "no")
	if !strings.Contains(rec.Body.String(), `"type":"burrow_error"`) {
		t.Fatalf("fallback shape: %s", rec.Body.String())
	}
}

func TestDialectOpenAI(t *testing.T) {
	d := DialectOpenAI
	if d.Name != "openai" {
		t.Fatal(d.Name)
	}
	for _, p := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/responses"} {
		if !d.inference(p) {
			t.Errorf("%s must be an inference path", p)
		}
	}
	for _, p := range []string{"/v1/models", "/v1/messages", "/v1/chat/completions/x", "/v2/chat/completions", "/", ""} {
		if d.inference(p) {
			t.Errorf("%s must not be an inference path", p)
		}
	}
	if !d.metered("/v1/chat/completions") {
		t.Error("chat completions must be metered")
	}
	if got, ok := DialectByName("openai"); !ok || got != d {
		t.Error("DialectByName(openai)")
	}
	if _, ok := DialectByName("grpc"); ok {
		t.Error("unknown dialect accepted")
	}
}

func TestDialectOpenAI_ModelList(t *testing.T) {
	rec := httptest.NewRecorder()
	DialectOpenAI.writeModels(rec, []modelItem{{ID: "burrow-simple", OwnedBy: "burrow"}, {ID: "zai/glm-5.1", OwnedBy: "zai"}})
	want := `{"object":"list","data":[{"id":"burrow-simple","object":"model","owned_by":"burrow"},{"id":"zai/glm-5.1","object":"model","owned_by":"zai"}]}`
	if strings.TrimSpace(rec.Body.String()) != want || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("body = %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	DialectOpenAI.writeModels(rec, nil)
	if strings.TrimSpace(rec.Body.String()) != `{"object":"list","data":[]}` {
		t.Fatalf("empty list = %s", rec.Body.String())
	}
}
