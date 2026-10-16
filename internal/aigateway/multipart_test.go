package aigateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testBoundary = "XbOuNdArYx"

// trickyAudio holds "\r\n--" that is not the boundary, a line that starts
// like the boundary, and every byte value.
func trickyAudio() []byte {
	var b bytes.Buffer
	b.WriteString("RIFF\r\n--not-the-boundary\r\n--" + testBoundary[:4] + "\r\n")
	for i := 0; i < 256; i++ {
		b.WriteByte(byte(i))
	}
	b.WriteString("\r\n--")
	return b.Bytes()
}

type formPart struct {
	name, filename string
	content        []byte
}

func buildForm(parts ...formPart) []byte {
	var b bytes.Buffer
	for _, p := range parts {
		b.WriteString("--" + testBoundary + "\r\n")
		b.WriteString(`Content-Disposition: form-data; name="` + p.name + `"`)
		if p.filename != "" {
			b.WriteString(`; filename="` + p.filename + `"` + "\r\nContent-Type: audio/wav")
		}
		b.WriteString("\r\n\r\n")
		b.Write(p.content)
		b.WriteString("\r\n")
	}
	b.WriteString("--" + testBoundary + "--\r\n")
	return b.Bytes()
}

func formRequest(body io.Reader, contentType string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r
}

const formType = "multipart/form-data; boundary=" + testBoundary

func TestReadMultipartModel_ModelBeforeAndAfterFile(t *testing.T) {
	audio := trickyAudio()
	file := formPart{name: "file", filename: "a.wav", content: audio}
	model := formPart{name: "model", content: []byte("whisper-1")}
	lang := formPart{name: "language", content: []byte("de")}
	for name, parts := range map[string][]formPart{
		"before": {model, lang, file},
		"after":  {file, lang, model},
	} {
		t.Run(name, func(t *testing.T) {
			raw := buildForm(parts...)
			got, rewrite, err := readMultipartModel(formRequest(bytes.NewReader(raw), formType), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if got != "whisper-1" {
				t.Fatalf("model = %q", got)
			}
			out, err := rewrite("a-much-longer-target-model")
			if err != nil {
				t.Fatal(err)
			}
			want := bytes.Replace(raw, []byte("whisper-1"), []byte("a-much-longer-target-model"), 1)
			if !bytes.Equal(out, want) {
				t.Fatalf("rewrite changed more than the model value:\n got %q\nwant %q", out, want)
			}
			if !bytes.Contains(out, audio) {
				t.Fatal("the binary part is not byte-identical")
			}
		})
	}
}

func TestReadMultipartModel_Errors(t *testing.T) {
	file := formPart{name: "file", filename: "a.wav", content: []byte("x")}
	cases := []struct {
		name string
		body []byte
		ct   string
		want error
	}{
		{"no model", buildForm(file), formType, errNoModelField},
		{"empty model", buildForm(file, formPart{name: "model"}), formType, errNoModelField},
		{"json content type", []byte(`{"model":"m"}`), "application/json", errNotMultipart},
		{"no content type", buildForm(file), "", errNotMultipart},
		{"no boundary", buildForm(file), "multipart/form-data", errNotMultipart},
		{"two models", buildForm(formPart{name: "model", content: []byte("a")}, formPart{name: "model", content: []byte("b")}), formType, errDuplicateModel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := readMultipartModel(formRequest(bytes.NewReader(c.body), c.ct), 1<<20)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func TestReadMultipartModel_OverTheLimitReadsNoMore(t *testing.T) {
	raw := buildForm(formPart{name: "file", filename: "a.wav", content: bytes.Repeat([]byte("a"), 10_000)},
		formPart{name: "model", content: []byte("m")})
	cr := &countingReader{r: bytes.NewReader(raw)}
	const limit = 1000
	_, _, err := readMultipartModel(formRequest(cr, formType), limit)
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if cr.n > limit+1 {
		t.Fatalf("read %d bytes, want at most %d", cr.n, limit+1)
	}
}

func TestReadMultipartModel_ExactlyTheLimitPasses(t *testing.T) {
	raw := buildForm(formPart{name: "model", content: []byte("m")})
	if _, _, err := readMultipartModel(formRequest(bytes.NewReader(raw), formType), int64(len(raw))); err != nil {
		t.Fatal(err)
	}
}

func TestIsMediaPath(t *testing.T) {
	for _, p := range []string{"/v1/images/generations", "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/audio/translations", "/v1/rerank", "/v1/rerank/"} {
		if !isMediaPath(p) {
			t.Errorf("%s should be a media path", p)
		}
	}
	for _, p := range []string{"/v1/chat/completions", "/v1/embeddings", "/v1/audio"} {
		if isMediaPath(p) {
			t.Errorf("%s should not be a media path", p)
		}
	}
	if !strings.Contains(formType, testBoundary) {
		t.Fatal("setup")
	}
}

// A gateway and an upstream must read the same "model" from a form: the
// reproduced attack has LF line endings that Go's parser accepts and an
// embedded CRLF part header inside the file that a CRLF-only scanner reads.
const attackForm = "--B\nContent-Disposition: form-data; name=\"model\"\n\nexpensive\n--B\nContent-Disposition: form-data; name=\"file\"; filename=\"a\"\n\nAUDIO\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nallowed\r\n--B--\n"

func TestReadMultipartModel_RefusesAmbiguousForms(t *testing.T) {
	ct := "multipart/form-data; boundary=B"
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	for name, body := range map[string]string{
		"lf only line endings (the attack)": attackForm,
		"lf after the first delimiter":      "--B\nContent-Disposition: form-data; name=\"model\"\r\n\r\nm\r\n--B--\r\n",
		"lf after a later delimiter":        crlf("--B\nContent-Disposition: form-data; name=\"file\"; filename=\"a\"\n\nx\n--B") + "\nContent-Disposition: form-data; name=\"model\"\r\n\r\nm\r\n--B--\r\n",
		"text after the delimiter":          "--B junk\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nm\r\n--B--\r\n",
	} {
		_, _, err := readMultipartModel(formRequest(strings.NewReader(body), ct), 1<<20)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Linear whitespace after a delimiter is allowed.
	ok := "--B  \r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nm\r\n--B--\r\n"
	if m, _, err := readMultipartModel(formRequest(strings.NewReader(ok), ct), 1<<20); err != nil || m != "m" {
		t.Errorf("trailing whitespace: %q %v", m, err)
	}
}

func TestReadMultipartModel_RewriteIsReadBackByGo(t *testing.T) {
	raw := buildForm(formPart{name: "file", filename: "a.wav", content: trickyAudio()}, formPart{name: "model", content: []byte("m")})
	_, rewrite, err := readMultipartModel(formRequest(bytes.NewReader(raw), formType), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rewrite("ok"); err != nil {
		t.Fatalf("a plain rewrite: %v", err)
	}
	// A value that would end the part and add another field is refused.
	if _, err := rewrite("x\r\n--" + testBoundary + "\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ny"); err == nil {
		t.Fatal("a model value that injects a part was accepted")
	}
}

// A part whose Content-Disposition Go cannot parse may be read by another
// parser (python-multipart keeps the last duplicate parameter) as the model.
func TestReadMultipartModel_FailsClosedOnUnreadableParts(t *testing.T) {
	ct := "multipart/form-data; boundary=B"
	form := func(disp string) string {
		return "--B\r\nContent-Disposition: " + disp + "\r\n\r\nexpensive\r\n--B\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nallowed\r\n--B--\r\n"
	}
	for name, body := range map[string]string{
		"duplicate name":       form(`form-data; name="x"; name="model"`),
		"trailing empty param": form(`form-data; name="x"; ;`),
		"not form-data":        form(`attachment; name="x"`),
		"no name":              form(`form-data`),
		"unparsable":           form(`form-data; name="x`),
		"no disposition":       "--B\r\nContent-Type: text/plain\r\n\r\nx\r\n--B\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nallowed\r\n--B--\r\n",
		"model with filename":  "--B\r\nContent-Disposition: form-data; name=\"model\"; filename=\"m\"\r\n\r\nx\r\n--B\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nallowed\r\n--B--\r\n",
	} {
		if _, _, err := readMultipartModel(formRequest(strings.NewReader(body), ct), 1<<20); !errors.Is(err, errMalformedForm) {
			t.Errorf("%s: err = %v, want errMalformedForm", name, err)
		}
	}
	// Ordinary forms still pass.
	ok := "--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\nContent-Type: audio/wav\r\n\r\nx\r\n--B\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nallowed\r\n--B--\r\n"
	if m, _, err := readMultipartModel(formRequest(strings.NewReader(ok), ct), 1<<20); err != nil || m != "allowed" {
		t.Errorf("ordinary form: %q %v", m, err)
	}
}
