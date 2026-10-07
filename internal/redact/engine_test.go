package redact

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestBuiltInRules is the canonical-example table for the bundled rules.
// Each row asserts:
//   - the rule's regex matches the canonical example,
//   - Apply rewrites the body per the rule's action (mask/drop/hash),
//   - the rule fires within its declared scope.
func TestBuiltInRules(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		scope  string // request_body | response_body
		wantIn string // when action=mask|hash: substring expected in rewritten body
		// for drop rules: drop != nil and out body unchanged but Apply returns
		// dropped = &rule (verified separately).
		expectDrop   bool
		expectRule   string // matches Rule.Name when expectDrop is true
		expectMasked bool   // expectIn assertion only checked when true
		expectHashed bool   // hash-action assertion
	}{
		{
			name:  "email mask",
			body:  "contact me at alice@example.com today",
			scope: "request_body", wantIn: "[redacted: email]", expectMasked: true,
		},
		{
			name:  "ipv4 mask",
			body:  "src=192.168.1.42 dst=10.0.0.1",
			scope: "request_body", wantIn: "[redacted: ipv4]", expectMasked: true,
		},
		{
			name:  "ipv6 mask",
			body:  "ip6=2001:0db8:85a3:0000:0000:8a2e:0370:7334",
			scope: "request_body", wantIn: "[redacted: ipv6]", expectMasked: true,
		},
		{
			name:  "aws_access_key drop",
			body:  "key=AKIAIOSFODNN7EXAMPLE rest",
			scope: "request_body", expectDrop: true, expectRule: "aws_access_key",
		},
		{
			name:  "credit_card_luhn mask (valid Luhn)",
			body:  "card 4111 1111 1111 1111 ok",
			scope: "request_body", wantIn: "[redacted: credit_card_luhn]", expectMasked: true,
		},
		{
			name:  "ssn_us mask",
			body:  "ssn=123-45-6789 rest",
			scope: "request_body", wantIn: "[redacted: ssn_us]", expectMasked: true,
		},
		{
			name:  "github_pat drop",
			body:  "pat=ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA rest",
			scope: "request_body", expectDrop: true, expectRule: "github_pat",
		},
		{
			name:  "slack_token drop",
			body:  "tok=xoxb-1234567890-0987654321-abcdef rest",
			scope: "request_body", expectDrop: true, expectRule: "slack_token",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			eng, err := NewEngine(nil) // built-in only
			if err != nil {
				t.Fatalf("NewEngine: %v", err)
			}
			out, dropped, _, err := eng.Apply([]byte(tc.body), tc.scope)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if tc.expectDrop {
				if dropped == nil {
					t.Fatalf("expected drop for %s but got out=%q", tc.expectRule, string(out))
				}
				if dropped.Name != tc.expectRule {
					t.Fatalf("dropped rule=%s want %s", dropped.Name, tc.expectRule)
				}
				return
			}
			if dropped != nil {
				t.Fatalf("unexpected drop: %s", dropped.Name)
			}
			if tc.expectMasked && !strings.Contains(string(out), tc.wantIn) {
				t.Fatalf("rewritten body missing %q: %q", tc.wantIn, string(out))
			}
		})
	}
}

// TestHashAction asserts a custom rule with action=hash rewrites matches to
// sha256(value)[:10] hex chars.
func TestHashAction(t *testing.T) {
	rules := []Rule{
		{ID: "phone", Name: "phone", Pattern: `\b\d{10}\b`, Action: "hash", Scope: "both"},
	}
	eng, err := NewEngine(rules)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	out, dropped, hits, err := eng.Apply([]byte("call 5551234567 now"), "request_body")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if dropped != nil {
		t.Fatalf("unexpected drop")
	}
	if len(hits) != 1 || hits[0].Rule.Name != "phone" || hits[0].Count != 1 {
		t.Fatalf("hits=%+v", hits)
	}
	// 5551234567 → sha256 → first 10 hex chars.
	// We don't hard-code the hex (sha256 is deterministic; we just want 10 hex
	// chars where the digits were, and the original digits gone).
	if strings.Contains(string(out), "5551234567") {
		t.Fatalf("hash should have removed digits: %q", string(out))
	}
	// Find the 10-hex char run that replaced the digits.
	// "call <10hex> now"
	parts := strings.Split(string(out), " ")
	if len(parts) != 3 {
		t.Fatalf("expected three parts, got %v", parts)
	}
	if len(parts[1]) != 10 {
		t.Fatalf("hash replacement should be 10 hex chars, got %q", parts[1])
	}
	for _, c := range parts[1] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("non-hex char in hash replacement: %q", parts[1])
		}
	}
}

// TestInvalidRegexRejected asserts NewEngine returns an error when any custom
// rule has an invalid regex pattern.
func TestInvalidRegexRejected(t *testing.T) {
	rules := []Rule{
		{ID: "bad", Name: "bad", Pattern: `(unclosed`, Action: "mask", Scope: "both"},
	}
	if _, err := NewEngine(rules); err == nil {
		t.Fatal("expected error for invalid regex; got nil")
	}
}

// TestLuhnFiltersFalsePositive asserts a 13-16 digit shape that fails the
// Luhn check does not get masked under the credit_card_luhn built-in.
func TestLuhnFiltersFalsePositive(t *testing.T) {
	eng, err := NewEngine(nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// 1234567890123 — 13 digits, sum 45 → fails Luhn.
	body := []byte("ref=1234567890123 done")
	out, dropped, _, err := eng.Apply(body, "request_body")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if dropped != nil {
		t.Fatal("unexpected drop")
	}
	if strings.Contains(string(out), "[redacted") {
		t.Fatalf("Luhn-invalid digit run should not be masked: %q", string(out))
	}
}

// TestScopeFiltering asserts a rule with scope=request_body does NOT fire
// when Apply is called with scope=response_body.
func TestScopeFiltering(t *testing.T) {
	eng, err := NewEngine(nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// aws_access_key is scope=request_body only.
	body := []byte("key=AKIAIOSFODNN7EXAMPLE in response")
	out, dropped, _, err := eng.Apply(body, "response_body")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if dropped != nil {
		t.Fatal("aws_access_key should not drop on response_body scope")
	}
	if string(out) != string(body) {
		t.Fatalf("body changed under wrong scope: %q", string(out))
	}
}

// TestDeterministicRuleOrder asserts Apply iterates rules in name order so
// the output is reproducible across runs (regression hedge against
// map-iteration order in NewEngine).
func TestDeterministicRuleOrder(t *testing.T) {
	body := []byte("email a@b.co and ip 1.2.3.4")
	want := ""
	for i := 0; i < 5; i++ {
		eng, err := NewEngine(nil)
		if err != nil {
			t.Fatalf("NewEngine: %v", err)
		}
		out, _, _, err := eng.Apply(body, "request_body")
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if i == 0 {
			want = string(out)
			continue
		}
		if string(out) != want {
			t.Fatalf("non-deterministic Apply output on iter %d: %q vs %q", i, string(out), want)
		}
	}
}

// TestPresidioTimeoutShortCircuits stands up an httptest server that takes
// five seconds to respond. The PresidioClient must time out at its 250ms hard
// cap and return a context-deadline error (the caller maps this to 503
// redaction.presidio_unavailable; this test only validates the engine-side
// timeout — the JSON 503 mapping is the caller's responsibility, exercised
// in Task 10).
func TestPresidioTimeoutShortCircuits(t *testing.T) {
	const serverDelay = 5 * time.Second
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Far longer than the client's cap, so that "gave up at the cap" and
		// "waited for the answer" stay apart on a busy machine. The handler
		// ends as soon as the client has hung up (the server notices that
		// once the request body has been read).
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(serverDelay):
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	p := &PresidioClient{BaseURL: srv.URL, HTTP: &http.Client{}}
	start := time.Now()
	_, err := p.Analyze(context.Background(), []byte(`{"text":"hi"}`))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error; got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) &&
		!strings.Contains(err.Error(), "deadline exceeded") &&
		!strings.Contains(err.Error(), "context deadline") {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	// Wide scheduling slack; we just want to confirm we did NOT wait for the
	// server's answer.
	if elapsed >= serverDelay-time.Second {
		t.Fatalf("Analyze did not short-circuit at 250ms: elapsed=%v", elapsed)
	}
}
