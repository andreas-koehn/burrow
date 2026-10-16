package aigw

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

type systemTextKey struct{}

// WithSystemText puts the system text the gateway resolved for this request
// (model text, a blank line, key text; either may be empty) in the context.
func WithSystemText(ctx context.Context, text string) context.Context {
	return context.WithValue(ctx, systemTextKey{}, text)
}

// SystemTextFrom returns the text WithSystemText stored, or "".
func SystemTextFrom(ctx context.Context) string {
	t, _ := ctx.Value(systemTextKey{}).(string)
	return t
}

// JoinSystemText combines a model's and a key's text: "" when both are empty,
// one of them alone, or model + "\n\n" + key.
func JoinSystemText(modelText, keyText string) string {
	switch {
	case modelText == "":
		return keyText
	case keyText == "":
		return modelText
	}
	return modelText + "\n\n" + keyText
}

// ApplySystemText returns body with text placed before the app's own system
// content, for the formats that have one; any other path, an empty text or a
// body it cannot parse is returned unchanged with applied == false.
//
// Chat Completions gets a leading system message, Responses a prefix on
// "instructions", Messages a prefix on "system" (a text block when the app
// sent blocks). Only those keys are touched; every other value keeps its bytes.
func ApplySystemText(path string, body []byte, text string) (out []byte, applied bool) {
	if text == "" {
		return body, false
	}
	var key string
	switch strings.TrimRight(path, "/") {
	case "/v1/chat/completions":
		key = "messages"
	case "/v1/responses":
		key = "instructions"
	case "/v1/messages":
		key = "system"
	default:
		return body, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body, false
	}
	cur, present := obj[key]
	if present && string(bytes.TrimSpace(cur)) == "null" {
		present = false
	}

	var next any
	switch key {
	case "messages":
		var msgs []json.RawMessage
		if !present || json.Unmarshal(cur, &msgs) != nil {
			return body, false
		}
		sys, _ := marshalNoEscape(map[string]string{"role": "system", "content": text})
		next = append([]json.RawMessage{sys}, msgs...)
	case "instructions":
		next = text
		if present {
			var s string
			if json.Unmarshal(cur, &s) != nil {
				return body, false
			}
			next = text + "\n\n" + s
		}
	case "system":
		next = text
		if present {
			var s string
			var blocks []json.RawMessage
			switch {
			case json.Unmarshal(cur, &s) == nil:
				next = text + "\n\n" + s
			case json.Unmarshal(cur, &blocks) == nil:
				first, _ := marshalNoEscape(map[string]string{"type": "text", "text": text})
				next = append([]json.RawMessage{first}, blocks...)
			default:
				return body, false
			}
		}
	}
	raw, err := marshalNoEscape(next)
	if err != nil {
		return body, false
	}
	obj[key] = raw
	res, err := marshalNoEscape(obj)
	if err != nil {
		return body, false
	}
	return res, true
}

// marshalNoEscape encodes v without HTML escaping, so the untouched values of
// a body keep their bytes.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
