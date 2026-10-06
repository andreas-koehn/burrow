package aigateway

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustBody(t *testing.T, in string) *requestBody {
	t.Helper()
	b, err := readRequestBody(httptest.NewRequest("POST", "/", strings.NewReader(in)), 1<<20)
	if err != nil {
		t.Fatalf("%q: %v", in, err)
	}
	return b
}

func TestRequestBody(t *testing.T) {
	in := `{"model":"burrow-medium","stream":true,"messages":[{"role":"user","content":"hi <b> & é"}]}`
	b := mustBody(t, in)
	if b.Model() != "burrow-medium" {
		t.Fatalf("Model() = %q", b.Model())
	}
	if string(b.Raw()) != in {
		t.Fatalf("Raw() = %s", b.Raw())
	}
	want := `{"model":"google/gemini-x","stream":true,"messages":[{"role":"user","content":"hi <b> & é"}]}`
	if got := string(b.WithModel("google/gemini-x")); got != want {
		t.Fatalf("out = %s", got)
	}
	// WithModel does not modify the receiver: a second call starts from the original.
	if b.Model() != "burrow-medium" || string(b.Raw()) != in || !strings.Contains(string(b.WithModel("x")), `"model":"x"`) {
		t.Fatal("WithModel mutated the body")
	}
}

// Apart from the bytes of the model value, the body is forwarded exactly as
// the client sent it.
func TestRequestBody_WithModelChangesOnlyTheValue(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		old   string // the exact bytes of the top-level model value
		model string // what Model() must report
	}{
		{"model first", `{"model":"old","b":1,"a":2}`, `"old"`, "old"},
		{"model middle, keys not sorted", `{"z":1,"model":"old","a":2}`, `"old"`, "old"},
		{"model last", `{"z":1,"a":2,"model":"old"}`, `"old"`, "old"},
		{"model only", `{"model":"old"}`, `"old"`, "old"},
		{"whitespace and newlines", "{\n  \"a\" : [ 1 , 2 ] ,\r\n\t\"model\" \t:\n \"old\" ,\n  \"b\" : { \"k\" :  null }\n}\n", `"old"`, "old"},
		{"leading and trailing whitespace", " \n\t{\"model\":\"old\"} \n", `"old"`, "old"},
		{"number spellings", `{"a":1.0,"b":1e3,"c":-0.0,"big":12345678901234567890123,"model":"old","d":1E+2}`, `"old"`, "old"},
		{"escaped slash", `{"url":"http:\/\/x\/y","model":"old"}`, `"old"`, "old"},
		{"unicode escapes in key and value", `{"k\u00e9y":"caf\u00e9","model":"old","\u006b":"\ud83d\ude00"}`, `"old"`, "old"},
		{"raw non-ASCII in key and value", `{"kéy":"café <b> & ☃","model":"old"}`, `"old"`, "old"},
		{"model value with escapes", `{"model":"a\/b\u00e9\"c\\d","x":1}`, `"a\/b\u00e9\"c\\d"`, `a/bé"c\d`},
		{"nested model key before the top-level one", `{"meta":{"model":"inner"},"list":[{"model":"deep"}],"model":"old"}`, `"old"`, "old"},
		{"nested model key after the top-level one", `{"model":"old","meta":{"model":"inner"}}`, `"old"`, "old"},
		{"other duplicate keys are left alone", `{"a":1,"model":"old","a":2}`, `"old"`, "old"},
		{"model text inside a string", `{"note":"\"model\":\"x\"","model":"old"}`, `"old"`, "old"},
	}
	names := map[string]string{ // new model name -> its JSON spelling
		"google/gemini-x": `"google/gemini-x"`,
		`we"ird\é<&>`:     `"we\"ird\\é<&>"`,
		"":                `""`,
	}
	for _, c := range cases {
		if strings.Count(c.in, c.old) != 1 {
			t.Fatalf("%s: the old value must occur exactly once in the input", c.name)
		}
		b := mustBody(t, c.in)
		if b.Model() != c.model {
			t.Errorf("%s: Model() = %q, want %q", c.name, b.Model(), c.model)
		}
		for name, spelled := range names {
			want := strings.Replace(c.in, c.old, spelled, 1)
			if got := string(b.WithModel(name)); got != want {
				t.Errorf("%s: WithModel(%q)\n got %q\nwant %q", c.name, name, got, want)
			}
		}
		if string(b.Raw()) != c.in {
			t.Errorf("%s: Raw() changed", c.name)
		}
	}
}

// Without a usable top-level model there is nothing to route by: Model() is
// "" and WithModel neither injects nor overwrites anything.
func TestRequestBody_NoModel(t *testing.T) {
	cases := map[string]string{
		"empty":                ``,
		"not json":             `not json`,
		"array":                `["a"]`,
		"array of objects":     `[{"model":"m"}]`,
		"string":               `"model"`,
		"null":                 `null`,
		"model is a number":    `{"model":7}`,
		"model is null":        `{"model":null}`,
		"model is an object":   `{"model":{"model":"m"}}`,
		"model is empty":       `{"model":""}`,
		"model missing":        `{"messages":[]}`,
		"model only nested":    `{"meta":{"model":"m"},"list":[{"model":"m"}]}`,
		"other key case":       `{"Model":"m"}`,
		"upper key case":       `{"MODEL":"m","x":1}`,
		"trailing garbage":     `{"model":"m"} x`,
		"second value":         `{"model":"m"}{"model":"n"}`,
		"truncated":            `{"model":"m","a":`,
		"broken after model":   `{"model":"m","a":tru}`,
		"missing comma":        `{"model":"m" "a":1}`,
		"UTF-8 BOM":            "\ufeff" + `{"model":"m"}`,
		"invalid before model": `{"a":01,"model":"m"}`,
	}
	for name, in := range cases {
		b := mustBody(t, in)
		if b.Model() != "" {
			t.Errorf("%s: Model() = %q, want empty", name, b.Model())
		}
		if got := string(b.WithModel("x")); got != in {
			t.Errorf("%s: WithModel altered the body: %q", name, got)
		}
	}
}

// A body with two top-level model fields is refused: which one an upstream
// reads is not defined, so it could differ from the one Burrow checked.
func TestReadRequestBody_DuplicateModel(t *testing.T) {
	for _, in := range []string{
		`{"model":"a","model":"b"}`,
		`{"model":"a","x":1,"model":"a"}`,
		`{"model":"a","model":7}`,
		`{"model":null,"model":"b"}`,
		`{"model":"a","mod\u0065l":"b"}`,
		" {\n\"model\" : \"a\" ,\n\"model\" : \"b\" }\n",
		// A sibling in another case counts: an upstream that matches keys
		// without regard to case (Go's encoding/json) reads the last one.
		`{"model":"a","Model":"b"}`,
		`{"Model":"b","model":"a"}`,
		`{"model":"a","MODEL":"b"}`,
		`{"Model":"a","MODEL":"b"}`,
		`{"model":"a","x":{"model":"n"},"mOdEl":7}`,
	} {
		b, err := readRequestBody(httptest.NewRequest("POST", "/", strings.NewReader(in)), 1<<20)
		if !errors.Is(err, errDuplicateModel) || b != nil {
			t.Errorf("%q: body %v err %v, want errDuplicateModel and no body", in, b, err)
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
