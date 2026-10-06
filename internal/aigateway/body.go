package aigateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

var errBodyTooLarge = errors.New("aigateway: request body too large")

// errDuplicateModel reports a JSON object with more than one top-level
// "model" field, in any letter case ("model" next to "Model" counts). Which
// of them an upstream reads is not defined, so such a
// body must not be forwarded: the endpoint answers it with a coded 400.
var errDuplicateModel = errors.New("aigateway: request body has more than one model field")

// requestBody is a buffered request body and the place of its "model" field.
//
// What a caller must do with it:
//   - readRequestBody refuses a body with two top-level "model" fields,
//     compared without regard to case (errDuplicateModel), and returns no
//     requestBody; answer 400, never forward.
//   - Model() == "" means "no model": the body is not a JSON object, has no
//     top-level key spelled exactly "model" ("Model" alone is not one), or
//     its value is not a non-empty string. There is nothing
//     to route by; answer a coded 400. WithModel leaves such a body as it is.
//   - Otherwise the model Burrow checks is the one an upstream reads: it is
//     the only top-level "model" field, and WithModel replaces exactly the
//     bytes of its value. Every other byte is forwarded as the client sent it.
type requestBody struct {
	raw   []byte
	model string // "" = no model
	// keys counts the top-level keys equal to "model" without regard to
	// case: 0 or 1 (two are refused).
	keys int
	// raw[start:end] is the top-level model value, quotes included. Only
	// meaningful when model != "".
	start, end int
}

// readRequestBody buffers r's body, up to limit bytes, and locates its
// top-level "model" field. It fails with errBodyTooLarge, errDuplicateModel
// or the error of the read. A body that is not JSON is not an error: it has
// no model.
func readRequestBody(r *http.Request, limit int64) (*requestBody, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errBodyTooLarge
	}
	b := &requestBody{raw: raw}
	if err := b.locateModel(); err != nil {
		return nil, err
	}
	return b, nil
}

// locateModel walks the top level of the object in b.raw. Nested objects and
// arrays are skipped as one value each, so a "model" key inside them is never
// seen. The only error is errDuplicateModel.
func (b *requestBody) locateModel() error {
	// One valid JSON value and nothing else: no byte-order mark, no
	// trailing data.
	if !json.Valid(b.raw) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b.raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var (
		seen       int
		model      string
		start, end int
	)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, _ := tok.(string) // decoded, so an escaped spelling of the key is seen as written out
		// Every letter case counts towards the duplicate check: an upstream
		// that matches keys without regard to case (Go's encoding/json, as
		// in Ollama) reads the last of "model" and "Model", which need not
		// be the one checked here.
		if strings.EqualFold(key, "model") {
			seen++
		}
		if key != "model" { // only the exact key is a model and is spliced
			var skip skipValue
			if dec.Decode(&skip) != nil {
				return nil
			}
			continue
		}
		// The value starts after the key, a colon and optional whitespace.
		at := skipSpace(b.raw, int(dec.InputOffset()))
		if at >= len(b.raw) || b.raw[at] != ':' {
			return nil
		}
		at = skipSpace(b.raw, at+1)
		var val json.RawMessage // the value's exact bytes
		if dec.Decode(&val) != nil {
			return nil
		}
		if at+len(val) > len(b.raw) || !bytes.Equal(b.raw[at:at+len(val)], val) {
			return nil // offsets do not line up: do not splice
		}
		var s string
		if len(val) > 0 && val[0] == '"' && json.Unmarshal(val, &s) == nil {
			model, start, end = s, at, at+len(val)
		}
	}
	if seen > 1 {
		return errDuplicateModel
	}
	b.model, b.start, b.end, b.keys = model, start, end, seen
	return nil
}

// skipValue consumes one JSON value without keeping it.
type skipValue struct{}

func (*skipValue) UnmarshalJSON([]byte) error { return nil }

// skipSpace returns the index of the first byte at or after i that is not
// JSON whitespace.
func skipSpace(raw []byte, i int) int {
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r') {
		i++
	}
	return i
}

// Raw returns the body exactly as it was read.
func (b *requestBody) Raw() []byte { return b.raw }

// Model returns the request's top-level "model" field. "" means there is no
// model to route by (see requestBody); the endpoint answers that with a
// coded 400.
func (b *requestBody) Model() string { return b.model }

// HasModelKey reports whether the body has a top-level key equal to "model"
// without regard to case. With Model() == "" that is a field an upstream may
// read as the model ("Model", or a "model" that is not a string) although
// Burrow has none to check or meter: answer a coded 400.
func (b *requestBody) HasModelKey() bool { return b.keys > 0 }

// WithModel returns the body with the value of "model" replaced by model.
// Only the bytes of that value change. A body without a model (Model() == "")
// is returned as it is: nothing is injected and nothing is overwritten. The
// receiver is not modified, so each fallback attempt can derive its own body.
func (b *requestBody) WithModel(model string) []byte {
	if b.model == "" {
		return b.raw
	}
	name := jsonString(model)
	out := make([]byte, 0, len(b.raw)-(b.end-b.start)+len(name))
	out = append(out, b.raw[:b.start]...)
	out = append(out, name...)
	return append(out, b.raw[b.end:]...)
}

// jsonString encodes s as a JSON string without HTML escaping.
func jsonString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// setBody replaces r's body and keeps its length fields consistent.
func setBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
}
