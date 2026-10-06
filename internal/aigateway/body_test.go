package aigateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestBody(t *testing.T) {
	in := `{"model":"burrow-medium","stream":true,"messages":[{"role":"user","content":"hi <b> & é"}]}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(in))
	b, err := readRequestBody(r, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b.Model() != "burrow-medium" {
		t.Fatalf("Model() = %q", b.Model())
	}
	if string(b.Raw()) != in {
		t.Fatalf("Raw() = %s", b.Raw())
	}
	out := b.WithModel("google/gemini-x")
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["model"]) != `"google/gemini-x"` || string(m["stream"]) != "true" {
		t.Fatalf("out = %s", out)
	}
	// Unknown top-level fields survive too (clients send pre-release fields).
	// Nested content is carried over byte for byte: not re-escaped, not reordered.
	if string(m["messages"]) != `[{"role":"user","content":"hi <b> & é"}]` {
		t.Fatalf("messages changed: %s", m["messages"])
	}
	// WithModel does not modify the receiver: a second call starts from the original.
	if b.Model() != "burrow-medium" || !strings.Contains(string(b.WithModel("x")), `"model":"x"`) {
		t.Fatal("WithModel mutated the body")
	}
}

func TestRequestBody_NotAnObject(t *testing.T) {
	for _, in := range []string{``, `not json`, `["a"]`, `"str"`, `null`, `{"model":7}`, `{"model":null}`, `{"messages":[]}`} {
		b, err := readRequestBody(httptest.NewRequest("POST", "/", strings.NewReader(in)), 1<<20)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if b.Model() != "" {
			t.Errorf("%q: Model() = %q, want empty", in, b.Model())
		}
	}
	// A non-object body is returned unchanged by WithModel.
	for _, in := range []string{`["a"]`, `null`, `not json`} {
		b, _ := readRequestBody(httptest.NewRequest("POST", "/", strings.NewReader(in)), 1<<20)
		if string(b.WithModel("x")) != in {
			t.Fatalf("non-object body %q was altered: %s", in, b.WithModel("x"))
		}
	}
}

func TestReadRequestBody_Limit(t *testing.T) {
	body := `{"model":"m","pad":"` + strings.Repeat("x", 100) + `"}`
	if _, err := readRequestBody(httptest.NewRequest("POST", "/", strings.NewReader(body)), 50); !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("err = %v, want errBodyTooLarge", err)
	}
	if _, err := readRequestBody(httptest.NewRequest("POST", "/", strings.NewReader(body)), int64(len(body))); err != nil {
		t.Fatalf("a body exactly at the limit must be accepted: %v", err)
	}
}

func TestSetBody(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader("old"))
	setBody(r, []byte(`{"a":1}`))
	got, _ := io.ReadAll(r.Body)
	if string(got) != `{"a":1}` || r.ContentLength != 7 || r.Header.Get("Content-Length") != "7" {
		t.Fatalf("body %q length %d header %q", got, r.ContentLength, r.Header.Get("Content-Length"))
	}
}

// Field values are copied verbatim, including their inner whitespace, and the
// result is always a valid JSON object.
func TestRequestBody_WithModelKeepsValuesVerbatim(t *testing.T) {
	in := "{ \"a<b\" : { \"k\" :  [1, 2] } ,\n \"model\" : \"old\" }"
	b, err := readRequestBody(httptest.NewRequest("POST", "/", strings.NewReader(in)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b.WithModel(`we"ird<`)), `{"a<b":{ "k" :  [1, 2] },"model":"we\"ird<"}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	// A body without a model field gains one.
	b, _ = readRequestBody(httptest.NewRequest("POST", "/", strings.NewReader(`{"messages":[]}`)), 1<<20)
	if got := string(b.WithModel("m")); got != `{"messages":[],"model":"m"}` {
		t.Fatalf("got %s", got)
	}
}
