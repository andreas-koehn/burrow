// cmd/server/e2e_translate_test.go
//
// Format translation on the global gateway, through the real wiring (see
// e2e_gateway_test.go for what that is): a model that opted in is served to
// callers of a format none of its targets speaks. The stand-in providers
// speak the other format: "alpha" is an OpenAI-format provider without the
// Responses API, "gamma" an Anthropic-format one.
package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// gwTranslation reads what a usage row says about translation.
func (e *gwEnv) gwTranslation(t *testing.T, requestID string) (translated, dropped string) {
	t.Helper()
	e.usageFor(t, requestID) // waits for the row
	if err := e.sqldb.QueryRow(`SELECT translated, dropped FROM usage_events WHERE request_id = ?`, requestID).Scan(&translated, &dropped); err != nil {
		t.Fatalf("read translation of %s: %v", requestID, err)
	}
	return translated, dropped
}

// gwEventNames returns the "event:" names of an event stream, in order.
func gwEventNames(body []byte) []string {
	var names []string
	for _, line := range strings.Split(string(body), "\n") {
		if name, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "event: "); ok {
			names = append(names, name)
		}
	}
	return names
}

func TestE2E_GatewayTranslation(t *testing.T) {
	e := bootGatewayE2E(t)
	base := e.up.srv.URL

	for _, p := range []map[string]any{
		{"slug": "alpha", "name": "Alpha", "kind": "direct", "api_format": "openai", "base_url": base + "/alpha/v1", "credential_slot": "ALPHA"},
		{"slug": "gamma", "name": "Gamma", "kind": "direct", "api_format": "anthropic", "base_url": base + "/gamma/v1", "credential_slot": "GAMMA", "auth_header": "x-api-key", "auth_format": "{key}"},
	} {
		if code, body := e.admin(t, "POST", "/api/v1/ai/providers", p); code != http.StatusCreated {
			t.Fatalf("create provider %v: %d %s", p["slug"], code, body)
		}
	}
	chat := []map[string]string{{"dialect": "openai", "provider": "alpha", "model": "m-a"}}
	claude := []map[string]string{{"dialect": "anthropic", "provider": "gamma", "model": "m-g"}}
	for _, m := range []map[string]any{
		{"name": "chat-only", "translate": true, "targets": chat},
		{"name": "chat-only-off", "targets": chat},
		{"name": "claude-only", "translate": true, "targets": claude},
		{"name": "both", "translate": true, "targets": append(append([]map[string]string{}, chat...), claude...)},
	} {
		if code, body := e.admin(t, "POST", "/api/v1/ai/models", m); code != http.StatusCreated {
			t.Fatalf("create model %v: %d %s", m["name"], code, body)
		}
	}
	newKey := func(body map[string]any) string {
		t.Helper()
		code, b := e.admin(t, "POST", "/api/v1/ai/keys", body)
		var out struct{ ID, Key string }
		if err := json.Unmarshal(b, &out); code != http.StatusCreated || err != nil || out.Key == "" {
			t.Fatalf("create key %v: %d %s", body["name"], code, b)
		}
		return out.Key
	}
	k1 := newKey(map[string]any{"name": "all"})
	k2 := newKey(map[string]any{"name": "other", "allowed_models": []string{"both"}})
	anthropic := func(key string) []string {
		return []string{"x-api-key", key, "anthropic-version", "2023-06-01", "anthropic-beta", "burrow-beta-1", "Cookie", "burrow_session=abc"}
	}
	messages := func(model string, stream bool) string {
		s := `{"model":"` + model + `","max_tokens":64,"messages":[{"role":"user","content":"hi"}]`
		if stream {
			s += `,"stream":true`
		}
		return s + "}"
	}

	t.Run("the model lists show what is served", func(t *testing.T) {
		ids := anthropicModelIDs(t, e.call(t, "GET", "/anthropic/v1/models", "", "x-api-key", k1))
		if strings.Join(ids, ",") != "both,chat-only,claude-only" {
			t.Fatalf("anthropic model list = %v", ids)
		}
		ids = openAIModelIDs(t, e.call(t, "GET", "/openai/v1/models", "", bearer(k1)...))
		if strings.Join(ids, ",") != "both,chat-only,chat-only-off,claude-only" {
			t.Fatalf("openai model list = %v", ids)
		}
	})

	t.Run("an Anthropic client on a Chat Completions provider", func(t *testing.T) {
		before := e.settled(t)
		r := e.call(t, "POST", "/anthropic/v1/messages?beta=true", messages("chat-only", false), anthropic(k1)...)
		var out struct {
			Type, Role, Model, StopReason string
			Content                       []struct{ Type, Text string }
			Usage                         struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			}
		}
		if r.Status != 200 || json.Unmarshal(r.Body, &out) != nil || out.Type != "message" || out.Role != "assistant" ||
			len(out.Content) != 1 || out.Content[0].Text != "hi from alpha" || out.Usage.InputTokens != 11 || out.Usage.OutputTokens != 7 {
			t.Fatalf("status %d body %s", r.Status, r.Body)
		}
		h := r.Header
		if h.Get("Burrow-Translated") != "messages-chat" || h.Get("Burrow-Dropped") != "anthropic-beta" || h.Get("Burrow-Provider") != "alpha" ||
			h.Get("Burrow-Model") != "m-a" || h.Get("Burrow-Attempts") != "1" || h.Get("X-Seen-Model") != "" {
			t.Fatalf("headers %v", h)
		}
		seen := e.up.last(t, "alpha")
		if seen.Path != "/v1/chat/completions" || seen.Model != "m-a" || seen.Header.Get("Authorization") != "Bearer "+gwAlphaSecret {
			t.Fatalf("the upstream saw %s, model %q, Authorization %q", seen.Path, seen.Model, seen.Header.Get("Authorization"))
		}
		for name, vals := range seen.Header {
			lower := strings.ToLower(name)
			if lower == "x-api-key" || lower == "cookie" || strings.HasPrefix(lower, "anthropic-") || strings.Contains(strings.Join(vals, " "), k1) {
				t.Fatalf("the caller's header %s reached the translated upstream", name)
			}
		}
		id := h.Get("Burrow-Request-Id")
		u := e.usageFor(t, id)
		if u.Dialect != "anthropic" || u.Provider != "alpha" || u.Target != "m-a" || u.Requested != "chat-only" || u.TokensIn != 11 || u.TokensOut != 7 || u.Status != 200 {
			t.Fatalf("usage row %+v", u)
		}
		if tr, dropped := e.gwTranslation(t, id); tr != "messages-chat" || dropped != "anthropic-beta" {
			t.Fatalf("usage row: translated %q dropped %q", tr, dropped)
		}
		if after := e.settled(t); after != before+1 {
			t.Fatalf("%d usage rows for one request", after-before)
		}

		// Streamed: the caller gets the Messages event sequence; the row
		// holds the upstream's usage chunk.
		r = e.call(t, "POST", "/anthropic/v1/messages", messages("chat-only", true), anthropic(k1)...)
		names := gwEventNames(r.Body)
		if r.Status != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream") || len(names) < 6 ||
			names[0] != "message_start" || names[len(names)-1] != "message_stop" || names[len(names)-2] != "message_delta" ||
			!strings.Contains(string(r.Body), `"text":"one"`) {
			t.Fatalf("status %d events %v\n%s", r.Status, names, r.Body)
		}
		if u := e.usageFor(t, r.Header.Get("Burrow-Request-Id")); u.TokensIn != 21 || u.TokensOut != 4 || u.Streamed != 1 {
			t.Fatalf("streamed usage row %+v", u)
		}
	})

	t.Run("a Chat Completions client on a Messages provider", func(t *testing.T) {
		r := e.call(t, "POST", "/openai/v1/chat/completions", `{"model":"claude-only","messages":[{"role":"user","content":"hi"}]}`,
			"Authorization", "Bearer "+k1, "OpenAI-Organization", "org-1", "Accept-Encoding", "br")
		var out struct {
			Object  string
			Choices []struct {
				Message      struct{ Role, Content string }
				FinishReason string `json:"finish_reason"`
			}
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			}
		}
		if r.Status != 200 || json.Unmarshal(r.Body, &out) != nil || out.Object != "chat.completion" || len(out.Choices) != 1 ||
			out.Choices[0].Message.Content != "hi from gamma" || out.Choices[0].FinishReason != "stop" || out.Usage.PromptTokens != 13 || out.Usage.CompletionTokens != 9 {
			t.Fatalf("status %d body %s", r.Status, r.Body)
		}
		if r.Header.Get("Burrow-Translated") != "chat-messages" || r.Header.Get("Burrow-Dropped") != "max_tokens.default" || r.Header.Get("Burrow-Provider") != "gamma" {
			t.Fatalf("headers %v", r.Header)
		}
		seen := e.up.last(t, "gamma")
		if seen.Path != "/v1/messages" || seen.Model != "m-g" || seen.Header.Get("X-Api-Key") != gwGammaSecret || seen.Header.Get("Anthropic-Version") != "2023-06-01" ||
			seen.Header.Get("Authorization") != "" || seen.Header.Get("Openai-Organization") != "" || strings.Contains(seen.Header.Get("Accept-Encoding"), "br") {
			t.Fatalf("the upstream saw %s with %v", seen.Path, seen.Header)
		}
		id := r.Header.Get("Burrow-Request-Id")
		if u := e.usageFor(t, id); u.Dialect != "openai" || u.Provider != "gamma" || u.TokensIn != 13 || u.TokensOut != 9 {
			t.Fatalf("usage row %+v", u)
		}
		if tr, dropped := e.gwTranslation(t, id); tr != "chat-messages" || dropped != "max_tokens.default" {
			t.Fatalf("usage row: translated %q dropped %q", tr, dropped)
		}
	})

	t.Run("a Responses client on providers without the Responses API", func(t *testing.T) {
		for model, want := range map[string][2]string{"chat-only": {"responses-chat", "alpha"}, "claude-only": {"responses-messages", "gamma"}} {
			r := e.call(t, "POST", "/openai/v1/responses", `{"model":"`+model+`","input":"hi"}`, bearer(k1)...)
			var out struct {
				Object, Status string
				Output         []struct {
					Type    string
					Content []struct{ Type, Text string }
				}
			}
			if r.Status != 200 || json.Unmarshal(r.Body, &out) != nil || out.Object != "response" || out.Status != "completed" ||
				len(out.Output) != 1 || len(out.Output[0].Content) != 1 || out.Output[0].Content[0].Text != "hi from "+want[1] {
				t.Fatalf("%s: status %d body %s", model, r.Status, r.Body)
			}
			if r.Header.Get("Burrow-Translated") != want[0] || r.Header.Get("Burrow-Provider") != want[1] {
				t.Fatalf("%s: headers %v", model, r.Header)
			}
		}
		// Without the flag, and on a stored response, nothing changes.
		r := e.call(t, "POST", "/openai/v1/responses", `{"model":"chat-only-off","input":"hi"}`, bearer(k1)...)
		if r.Status != 400 || openAIErrCode(t, r) != "endpoint_unsupported" {
			t.Fatalf("flag off: status %d body %s", r.Status, r.Body)
		}
		r = e.call(t, "POST", "/openai/v1/responses/resp_1/cancel", `{"model":"chat-only"}`, bearer(k1)...)
		if r.Status != 404 || openAIErrCode(t, r) != "endpoint_not_found" {
			t.Fatalf("sub-path: status %d body %s", r.Status, r.Body)
		}
	})

	t.Run("native stays native", func(t *testing.T) {
		alpha := e.up.calls("alpha")
		r := e.call(t, "POST", "/anthropic/v1/messages", messages("both", false), anthropic(k1)...)
		if r.Status != 200 || r.Header.Get("Burrow-Provider") != "gamma" || r.Header.Get("Burrow-Translated") != "" || r.Header.Get("Burrow-Dropped") != "" {
			t.Fatalf("status %d headers %v", r.Status, r.Header)
		}
		if seen := e.up.last(t, "gamma"); seen.Header.Get("Anthropic-Beta") != "burrow-beta-1" || string(seen.Body) != messages("m-g", false) {
			t.Fatalf("the native request was changed: %v %s", seen.Header, seen.Body)
		}
		id := r.Header.Get("Burrow-Request-Id")
		if tr, dropped := e.gwTranslation(t, id); tr != "" || dropped != "" {
			t.Fatalf("usage row of a native answer: translated %q dropped %q", tr, dropped)
		}
		// The native target fails: its answer is the caller's, and the
		// target of the other format is not tried in its place.
		e.up.set("gamma", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
			gwJSON(w, 500, map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "down"}})
			return true
		})
		defer e.up.reset("gamma")
		r = e.call(t, "POST", "/anthropic/v1/messages", messages("both", false), anthropic(k1)...)
		if r.Status != 500 || !strings.Contains(string(r.Body), `"down"`) || r.Header.Get("Burrow-Translated") != "" || e.up.calls("alpha") != alpha {
			t.Fatalf("failing native target: status %d headers %v body %s (alpha calls %d → %d)", r.Status, r.Header, r.Body, alpha, e.up.calls("alpha"))
		}
	})

	t.Run("without the flag, and before the allow-list, nothing shows", func(t *testing.T) {
		alpha := e.up.calls("alpha")
		r := e.call(t, "POST", "/anthropic/v1/messages", messages("chat-only-off", false), anthropic(k1)...)
		code, _, msg := anthropicErr(t, r)
		if r.Status != 400 || code != "format_mismatch" || msg != "model chat-only-off is not served in the anthropic format; use https://gateway.test/openai/v1" {
			t.Fatalf("flag off: status %d code %q message %q", r.Status, code, msg)
		}
		r = e.call(t, "POST", "/anthropic/v1/messages", messages("alpha/m-a", false), anthropic(k1)...)
		if code, _, _ := anthropicErr(t, r); r.Status != 400 || code != "format_mismatch" {
			t.Fatalf("direct address: status %d code %q", r.Status, code)
		}
		// A key that may not use the model gets the one denial, whether the
		// model is translated, plain or unknown.
		var denials []string
		for _, model := range []string{"chat-only", "chat-only-off", "no-such-model"} {
			r := e.call(t, "POST", "/anthropic/v1/messages", messages(model, false), anthropic(k2)...)
			if code, _, _ := anthropicErr(t, r); r.Status != 403 || code != "model_not_allowed" {
				t.Fatalf("%s: status %d code %q", model, r.Status, code)
			}
			denials = append(denials, string(r.Body))
		}
		if denials[0] != denials[1] || denials[0] != denials[2] {
			t.Fatalf("the denials differ: %q", denials)
		}
		if e.up.calls("alpha") != alpha {
			t.Fatal("a refused request reached a provider")
		}
	})

	t.Run("an upstream error keeps its status in the caller's shape", func(t *testing.T) {
		e.up.set("alpha", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
			w.Header().Set("Retry-After", "7")
			gwJSON(w, 429, map[string]any{"error": map[string]string{"message": "slow down", "type": "rate_limit_error"}})
			return true
		})
		defer e.up.reset("alpha")
		r := e.call(t, "POST", "/anthropic/v1/messages", messages("chat-only", false), anthropic(k1)...)
		code, typ, msg := anthropicErr(t, r)
		if r.Status != 429 || code != "upstream_error" || typ != "rate_limit_error" || msg != "slow down" || r.Header.Get("Retry-After") != "7" ||
			r.Header.Get("Burrow-Translated") != "messages-chat" {
			t.Fatalf("status %d code %q type %q message %q headers %v", r.Status, code, typ, msg, r.Header)
		}
		// The attempt log names the upstream's status.
		id := r.Header.Get("Burrow-Request-Id")
		e.usageFor(t, id)
		type attempt struct {
			Provider  string
			Status    int
			ErrorCode string `json:"error_code"`
		}
		var attempts []attempt
		for end := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			code, body := e.admin(t, "GET", "/api/v1/ai/requests/"+url.PathEscape(id)+"/attempts", nil)
			if code != 200 {
				t.Fatalf("attempts: %d %s", code, body)
			}
			attempts = nil
			if err := json.Unmarshal(body, &attempts); err != nil {
				t.Fatalf("attempts body %s: %v", body, err)
			}
			if len(attempts) == 1 || time.Now().After(end) {
				break
			}
		}
		if len(attempts) != 1 || attempts[0].Provider != "alpha" || attempts[0].ErrorCode != "http_429" || attempts[0].Status != 429 {
			t.Fatalf("attempt log: %+v", attempts)
		}

		// An answer that cannot be read is the provider's failure.
		e.up.set("alpha", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"mess`))
			return true
		})
		r = e.call(t, "POST", "/anthropic/v1/messages", messages("chat-only", false), anthropic(k1)...)
		if code, typ, _ := anthropicErr(t, r); r.Status != 502 || code != "upstream_invalid" || typ != "api_error" {
			t.Fatalf("unreadable answer: status %d code %q type %q", r.Status, code, typ)
		}
	})

	t.Run("a request the target cannot be given is the caller's 400", func(t *testing.T) {
		alpha := e.up.calls("alpha")
		body := `{"model":"chat-only","max_tokens":64,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","data":"x"}}]}]}`
		r := e.call(t, "POST", "/anthropic/v1/messages", body, anthropic(k1)...)
		code, typ, msg := anthropicErr(t, r)
		if r.Status != 400 || code != "invalid_request" || typ != "invalid_request_error" || !strings.Contains(msg, "messages[0].content[0]") || e.up.calls("alpha") != alpha {
			t.Fatalf("status %d code %q type %q message %q", r.Status, code, typ, msg)
		}
	})

	t.Run("counting tokens is estimated", func(t *testing.T) {
		before, alpha := e.settled(t), e.up.calls("alpha")
		r := e.call(t, "POST", "/anthropic/v1/messages/count_tokens", `{"model":"chat-only","messages":[{"role":"user","content":"12345678"}]}`, anthropic(k1)...)
		if r.Status != 200 || strings.TrimSpace(string(r.Body)) != `{"input_tokens":2}` || r.Header.Get("Burrow-Estimated") != "1" {
			t.Fatalf("status %d headers %v body %s", r.Status, r.Header, r.Body)
		}
		if e.up.calls("alpha") != alpha || e.settled(t) != before {
			t.Fatal("an estimate reached a provider or wrote a usage row")
		}
		r = e.call(t, "POST", "/anthropic/v1/messages/count_tokens", `{"model":"chat-only-off","messages":[{"role":"user","content":"hi"}]}`, anthropic(k1)...)
		if code, _, _ := anthropicErr(t, r); r.Status != 400 || code != "format_mismatch" {
			t.Fatalf("flag off: status %d code %q", r.Status, code)
		}
	})

	t.Run("the flag is a switch", func(t *testing.T) {
		if code, body := e.admin(t, "PUT", "/api/v1/ai/models/chat-only-off", map[string]any{"name": "chat-only-off", "translate": true, "targets": chat}); code != 200 {
			t.Fatalf("turn translation on: %d %s", code, body)
		}
		r := e.call(t, "POST", "/anthropic/v1/messages", messages("chat-only-off", false), anthropic(k1)...)
		if r.Status != 200 || r.Header.Get("Burrow-Translated") != "messages-chat" {
			t.Fatalf("after turning it on: status %d body %s", r.Status, r.Body)
		}
	})
}
