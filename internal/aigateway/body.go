package aigateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
)

var errBodyTooLarge = errors.New("aigateway: request body too large")

// requestBody is a buffered request body. For a JSON object its top-level
// fields are kept as raw messages, so replacing one field leaves the value of
// every other field exactly as the client sent it.
type requestBody struct {
	raw    []byte
	fields map[string]json.RawMessage // nil when raw is not a JSON object
}

// readRequestBody buffers r's body, up to limit bytes.
func readRequestBody(r *http.Request, limit int64) (*requestBody, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errBodyTooLarge
	}
	b := &requestBody{raw: raw}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil {
		b.fields = fields // stays nil for the JSON value null
	}
	return b, nil
}

// Raw returns the body exactly as it was read.
func (b *requestBody) Raw() []byte { return b.raw }

// Model returns the request's "model" field, or "" when there is none.
func (b *requestBody) Model() string {
	var model string
	if b.fields == nil || json.Unmarshal(b.fields["model"], &model) != nil {
		return ""
	}
	return model
}

// WithModel returns the body with "model" set to model. The receiver is not
// modified, so each fallback attempt can derive its own body. The values of
// the other fields are copied verbatim; only the order of the top-level
// fields and the whitespace between them may differ from what was read.
func (b *requestBody) WithModel(model string) []byte {
	if b.fields == nil {
		return b.raw
	}
	keys := make([]string, 0, len(b.fields)+1)
	for k := range b.fields {
		if k != "model" {
			keys = append(keys, k)
		}
	}
	keys = append(keys, "model")
	sort.Strings(keys)

	var out bytes.Buffer
	out.Grow(len(b.raw) + len(model) + 16)
	out.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(jsonString(k))
		out.WriteByte(':')
		if k == "model" {
			out.Write(jsonString(model))
		} else {
			out.Write(b.fields[k])
		}
	}
	out.WriteByte('}')
	return out.Bytes()
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
