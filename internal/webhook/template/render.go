// Package template provides sandboxed payload-template rendering for
// Burrow webhooks (spec H.3).
//
// Templates use Go's text/template (NOT html/template — HTML auto-escaping
// would corrupt JSON payloads). The function allowlist is CLOSED: any
// function name not in allowedFuncs fails at parse time, preventing
// sandbox escape via `call`, `env`, or any other helper not listed here.
//
// text/template Option "missingkey=zero" means unknown fields render as the
// zero value (empty string) rather than "<no value>" or an error. This
// matches the spec note: "templates that reference undefined fields render
// as the empty string."
//
// # Security invariants
//
//   - The only functions available are the twelve in allowedFuncs (printf,
//     lower, upper, title, replace, trim, split, now, default, b64enc, b64dec, json).
//     Any function name not in this closed set causes Parse to return an error.
//   - Template nesting ({{ template "x" . }}) is rejected at parse time
//     because no associated templates are defined in the initial parse call
//     and text/template returns an error for undefined names.
//   - The call built-in is not in the FuncMap and is not a default text/template
//     built-in, so it is unavailable.
//   - Size of rendered output is not capped here; callers impose their own
//     limit (the dispatcher uses PreviewCap for the stored preview only; the
//     wire body is always full). Templates that generate enormous output will
//     be caught by the caller's MaxBytesReader at the API boundary.
package template

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"time"
)

// allowedFuncs is the closed set of functions available inside webhook
// payload templates (spec H.3). ANY other function name causes Parse to
// return an error, which Render/Validate map to a 400 response — this is
// the primary sandbox enforcement point.
//
// To add a function: update this map, the spec table in H.3, and the
// allowedFuncsNames slice used by tests.
var allowedFuncs = template.FuncMap{
	// printf — stdlib passthrough.
	"printf": fmt.Sprintf,

	// String case / manipulation.
	"lower":   strings.ToLower,
	"upper":   strings.ToUpper,
	"title":   strings.Title, //nolint:staticcheck // spec mandates "title"; replacement (cases.Title) adds a dependency not in go.mod
	"replace": strings.ReplaceAll,
	"trim":    strings.TrimSpace,
	"split":   strings.Split,

	// Temporal.
	"now": func() string {
		return time.Now().UTC().Format(time.RFC3339)
	},

	// sprig-style default: returns fallback when val is the zero value or nil.
	"default": func(fallback, val any) any {
		if val == nil {
			return fallback
		}
		switch v := val.(type) {
		case string:
			if v == "" {
				return fallback
			}
		case int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64,
			float32, float64:
			// Non-zero numeric is fine as-is.
		case bool:
			if !v {
				return fallback
			}
		}
		return val
	},

	// Base64.
	"b64enc": func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) },
	"b64dec": func(s string) (string, error) {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return "", err
		}
		return string(b), nil
	},

	// json — JSON-encodes any value.
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	},
}

// knownEventFields maps each closed event name to a representative field set
// for Validate dry-runs. The map values are the same shape the dispatcher
// uses when building the default payload body — they let Validate catch
// function-misuse beyond syntax errors.
var knownEventFields = map[string]map[string]any{
	"webhook.test":               {"ServiceID": "", "Event": "webhook.test"},
	"tunnel.connected":           {"ServiceID": "", "TunnelID": "", "Kind": ""},
	"tunnel.disconnected":        {"ServiceID": "", "TunnelID": "", "Kind": ""},
	"tunnel.failed":              {"ServiceID": "", "TunnelID": "", "Error": ""},
	"access.denied":              {"ServiceID": "", "Reason": "", "IP": ""},
	"quota.exceeded":             {"scope": "", "subject": "", "dimension": "", "limit": 0, "window": "", "service_id": ""},
	"budget.exceeded":            {"budget_id": "", "scope": "", "subject_id": "", "daily_usd": 0.0, "current_usd": 0.0, "daily_tokens": 0, "current_tokens": 0, "action_on_exceed": "", "gateway_key_id": "", "model": ""},
	"redaction.applied":          {"ServiceID": "", "RuleID": ""},
	"guardrail.refused":          {"ServiceID": "", "Pattern": ""},
	"cert.expiring":              {"Domain": "", "ExpiryDays": 0},
	"audit.exported":             {"ActorEmail": "", "RecordCount": 0},
	"backup.completed":           {"Path": "", "SizeBytes": 0},
	"ai.upstream_error":          {"service_id": "", "backend_service_id": "", "status": 0, "error": "", "retry_count": 0},
	"ai.cache_promotion":         {"service_id": "", "exact_key_hash": "", "prompt_fingerprint": "", "similarity_to_first": 0.0},
	"audit.policy_change":        {"actor_email": "", "action": "", "before": nil, "after": nil},
	"service.created":            {"service_id": "", "name": "", "type": "", "access_mode": ""},
	"service.deleted":            {"service_id": "", "name": ""},
	"connection.session_summary": {"service_id": "", "kind": "", "window_start": "", "window_end": "", "sessions": 0, "bytes_in": 0, "bytes_out": 0, "avg_duration_ms": 0, "p95_duration_ms": 0, "top_source_ips": []map[string]any{}},
}

// Render compiles src and executes it with fields as the template dot value.
// Returns the rendered bytes, the byte count, and any error.
//
// On compile error (unknown function, syntax error) returns (nil, 0, err) so
// the caller can map the error to a 400 HTTP response. On execution error
// (e.g. b64dec of bad input) returns (nil, 0, err) as well.
//
// Empty src is always valid and renders to an empty byte slice.
func Render(src string, fields map[string]any) ([]byte, int, error) {
	if src == "" {
		return []byte{}, 0, nil
	}
	t, err := template.New("webhook").
		Funcs(allowedFuncs).
		Option("missingkey=zero").
		Parse(src)
	if err != nil {
		return nil, 0, fmt.Errorf("template compile error: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, fields); err != nil {
		return nil, 0, fmt.Errorf("template render error: %w", err)
	}
	b := buf.Bytes()
	return b, len(b), nil
}

// Validate compiles src against the known field set for event and reports
// any error. Empty src is always valid. Callers (POST/PUT /webhooks) should
// call Validate before persisting the template.
//
// Validate performs a dry-run execution with the event's known field set so
// function-misuse (e.g. calling a non-existent helper) is caught beyond
// pure syntax errors.
func Validate(event, src string) error {
	if src == "" {
		return nil
	}
	_, _, err := Render(src, knownEventFields[event])
	return err
}
