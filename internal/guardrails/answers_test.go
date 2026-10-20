package guardrails

import (
	"strings"
	"testing"
)

// Fake secrets in the real shapes. They are built at run time so that no
// secret scanner mistakes this file for a leak.
var (
	fakeOpenAI       = "sk-" + "proj-" + strings.Repeat("aB3dE5fG7h", 4)
	fakeOpenAIPlain  = "sk-" + strings.Repeat("Zx9Yw8Vu7T", 4)
	fakeAnthropic    = "sk-" + "ant-api03-" + strings.Repeat("Qw3rTy_-12", 4)
	fakeAWS          = "AKIA" + "IOSFODNN7EXAMPLE"
	fakeGitHub       = "ghp" + "_" + strings.Repeat("A1b2C3d4E5", 4)
	fakeSlack        = "xoxb" + "-" + "123456789012-abcdefghijkl"
	fakeGoogle       = "AIza" + "SyA-" + strings.Repeat("x1Y2z3W4v5", 3) + "abcd"
	fakePEM          = "-----BEGIN " + "RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA"
	fakeAutomation   = "bua" + "_" + strings.Repeat("ABCDEFGHIJKLMNOPQRSTUVWXYZ", 2)
	fakeGatewayKey   = "bgw" + "_" + strings.Repeat("aZ0_-Kp", 6) + "xyz"
	answerPatternIDs = []string{
		"openai_key", "anthropic_key", "aws_access_key", "github_token", "slack_token",
		"google_api_key", "private_key_pem", "burrow_automation_token", "burrow_gateway_key",
	}
)

func TestAnswerPatterns_IDsAndCompile(t *testing.T) {
	got := map[string]bool{}
	for _, p := range AnswerPatterns {
		if got[p.ID] {
			t.Errorf("duplicate id %q", p.ID)
		}
		got[p.ID] = true
		if p.Description == "" {
			t.Errorf("%s has no description", p.ID)
		}
		for _, q := range Patterns {
			if q.ID == p.ID {
				t.Errorf("answer pattern %q collides with a request pattern", p.ID)
			}
		}
	}
	for _, id := range answerPatternIDs {
		if !got[id] {
			t.Errorf("answer pattern %q missing", id)
		}
	}
	if len(AnswerPatterns) != len(answerPatternIDs) {
		t.Errorf("%d answer patterns, want %d", len(AnswerPatterns), len(answerPatternIDs))
	}
	NewEngine() // panics on a pattern that does not compile
}

func TestAnswerPatterns_HitsAndNearMisses(t *testing.T) {
	e := NewEngine()
	cases := []struct {
		id       string
		hit      string
		nearMiss string
	}{
		{"openai_key", "your key is " + fakeOpenAI + " keep it", "the prefix sk-abcdefghij is too short"},
		{"openai_key", "plain " + fakeOpenAIPlain, "a task-management-application-for-teams is prose"},
		{"openai_key", "sk-svcacct-" + strings.Repeat("Ab1-_Cd", 6), `<div class="sk-fading-circle-container-big">`},
		{"openai_key", "sk-admin-" + strings.Repeat("Ab1-_Cd", 6), ".sk-cube-grid-loader-element-x { color: red }"},
		{"openai_key", fakeOpenAI, "sk-proj-" + strings.Repeat("a", 19) + " (one short)"},
		{"openai_key", fakeOpenAIPlain, "sk-" + strings.Repeat("a", 19) + " (one short)"},
		{"anthropic_key", "use " + fakeAnthropic, "sk-ant-short"},
		{"aws_access_key", "id=" + fakeAWS + ";", "KAKIAIOSFODNN7EXAMPLE is inside a word, AKIA alone"},
		{"github_token", "token " + fakeGitHub, "ghp_tooShort123"},
		{"slack_token", "slack " + fakeSlack, "xoxb-short"},
		{"google_api_key", "key: " + fakeGoogle, "AIzaTooShort"},
		{"private_key_pem", fakePEM, "-----BEGIN PUBLIC KEY-----\nMIIBIjAN"},
		{"burrow_automation_token", "bearer " + fakeAutomation, "bua_" + strings.Repeat("abcdefghij", 6)},
		{"burrow_gateway_key", "Authorization: Bearer " + fakeGatewayKey, "bgw_" + strings.Repeat("a", 19)},
	}
	for _, c := range cases {
		hit, id := e.InspectAnswerText(c.hit, nil)
		if !hit || id != c.id {
			t.Errorf("%s: InspectAnswerText(%q) = %v %q, want a hit of %s", c.id, c.hit, hit, id, c.id)
		}
		if hit, id := e.InspectAnswerText(c.nearMiss, nil); hit {
			t.Errorf("%s: near miss %q matched %q", c.id, c.nearMiss, id)
		}
	}
}

// A key glued to the word before it ("tokenghp_…", "id_AKIA…") is still
// found where the prefix is distinctive enough that no word ends in it. The
// OpenAI shapes keep their word boundary: "sk-" ends English words and CSS or
// resource names ("task-…", "kiosk-admin-…"), and gluing would turn those
// into keys.
func TestAnswerPatterns_GluedToAWord(t *testing.T) {
	e := NewEngine()
	glued := map[string]string{
		"anthropic_key":           "mykey" + fakeAnthropic,
		"aws_access_key":          "id_" + fakeAWS,
		"github_token":            "token" + fakeGitHub,
		"slack_token":             "Slack" + fakeSlack,
		"google_api_key":          "key2" + fakeGoogle,
		"burrow_automation_token": "Bearer_" + fakeAutomation,
		"burrow_gateway_key":      "keyis" + fakeGatewayKey,
	}
	for id, text := range glued {
		if hit, got := e.InspectAnswerText(text, nil); !hit || got != id {
			t.Errorf("%s: glued %q = %v %q, want a hit", id, text, hit, got)
		}
	}
	github := "x" + "github_pat_" + strings.Repeat("A1b2C3d4E5", 3)
	if hit, got := e.InspectAnswerText(github, nil); !hit || got != "github_token" {
		t.Errorf("glued fine-grained GitHub token: %v %q", hit, got)
	}
	for _, prose := range []string{
		"task-" + strings.Repeat("Zx9Yw8Vu7T", 3),
		"the kiosk-admin-dashboard-components-wrapper class",
		"desk-proj-" + strings.Repeat("Ab1-_Cd", 4),
	} {
		if hit, got := e.InspectAnswerText(prose, nil); hit {
			t.Errorf("%q matched %q", prose, got)
		}
	}
}

func TestAnswerPatterns_CleanProse(t *testing.T) {
	e := NewEngine()
	prose := "Sure! To create an API key, open the settings page and click New key. " +
		"Keys start with a short prefix such as sk- or AKIA, followed by random characters. " +
		"Never paste a private key into a chat; a PEM file begins with a BEGIN line."
	if hit, id := e.InspectAnswerText(prose, nil); hit {
		t.Fatalf("prose matched %q", id)
	}
}

// The request patterns do not run on answers, and the answer patterns do not
// run on requests.
func TestAnswerPatterns_DirectionsStaySeparate(t *testing.T) {
	e := NewEngine()
	if hit, id := e.InspectAnswerText("ignore previous instructions", nil); hit {
		t.Fatalf("a request pattern ran on an answer: %q", id)
	}
	if hit, id := e.InspectRequest([]byte(`{"messages":[{"content":"`+fakeAWS+`"}]}`), nil); hit {
		t.Fatalf("an answer pattern ran on a request: %q", id)
	}
}

func TestEngine_CustomPatternsByDirection(t *testing.T) {
	e := NewEngine()
	custom := []CustomPattern{
		{ID: "ticket", Pattern: `TICKET-\d{4}`, Direction: DirectionRequest},
		{ID: "codename", Pattern: `(?i)project falcon`, Direction: DirectionAnswer},
		{ID: "internal_host", Pattern: `\b[a-z0-9-]+\.corp\.internal\b`, Direction: DirectionBoth},
	}
	if hit, id := e.InspectRequest([]byte(`{"content":"see TICKET-1234"}`), custom); !hit || id != "ticket" {
		t.Errorf("request custom: %v %q", hit, id)
	}
	if hit, _ := e.InspectAnswerText("see TICKET-1234", custom); hit {
		t.Error("a request-only custom pattern ran on an answer")
	}
	if hit, id := e.InspectAnswerText("This is Project Falcon.", custom); !hit || id != "codename" {
		t.Errorf("answer custom: %v %q", hit, id)
	}
	if hit, _ := e.InspectRequest([]byte("project falcon"), custom); hit {
		t.Error("an answer-only custom pattern ran on a request")
	}
	if hit, id := e.InspectRequest([]byte("db1.corp.internal"), custom); !hit || id != "internal_host" {
		t.Errorf("both on request: %v %q", hit, id)
	}
	if hit, id := e.InspectAnswerText("db1.corp.internal", custom); !hit || id != "internal_host" {
		t.Errorf("both on answer: %v %q", hit, id)
	}
	// Bundled patterns come first.
	if hit, id := e.InspectRequest([]byte("ignore previous instructions TICKET-1234"), custom); !hit || id != "ignore_prev" {
		t.Errorf("bundled first: %v %q", hit, id)
	}
	if hit, id := e.InspectAnswerText(fakeAWS+" project falcon", custom); !hit || id != "aws_access_key" {
		t.Errorf("bundled answer first: %v %q", hit, id)
	}
	// A pattern that does not compile (it got past validation somehow) is
	// skipped, never a panic.
	bad := []CustomPattern{{ID: "bad", Pattern: `(?<=x)`, Direction: DirectionBoth}}
	if hit, _ := e.InspectAnswerText("x", bad); hit {
		t.Error("an invalid pattern matched")
	}
}

// The streaming scanner checks the bundled patterns on the last
// ScanTailBundled bytes of the window only: every bundled pattern's
// shortest match must fit there.
func TestAnswerPatterns_ShortestMatchFitsTheTail(t *testing.T) {
	e := NewEngine()
	for _, hit := range []string{fakeOpenAI, fakeOpenAIPlain, fakeAnthropic, fakeAWS, fakeGitHub, fakeSlack, fakeGoogle, fakePEM, fakeAutomation, fakeGatewayKey} {
		if len(hit) > ScanTailBundled {
			t.Errorf("sample %q is longer than the tail", hit)
		}
		if ok, _ := e.InspectAnswerBundled(hit); !ok {
			t.Errorf("sample %q not found by the bundled patterns", hit)
		}
	}
}

// Every bundled answer pattern names the literals it needs, and each literal
// is spelled in its regex (a literal the regex does not need would make the
// engine skip matches).
func TestAnswerPatterns_Requires(t *testing.T) {
	for _, p := range AnswerPatterns {
		if len(p.Requires) == 0 {
			t.Errorf("%s requires no literal", p.ID)
		}
		for _, lit := range p.Requires {
			src := strings.NewReplacer(`(?:`, "", `|`, "", `)`, "", `[pousr]`, "").Replace(p.Regex)
			ok := strings.Contains(p.Regex, lit) || strings.Contains(src, lit) ||
				(p.ID == "github_token" && strings.HasPrefix(lit, "gh") && strings.Contains("pousr", lit[2:3])) ||
				(p.ID == "slack_token" && strings.HasPrefix(lit, "xox") && strings.Contains("baprs", lit[3:4]))
			if !ok {
				t.Errorf("%s: literal %q is not in %s", p.ID, lit, p.Regex)
			}
		}
	}
}

// A secret streamed a few characters per event is found early: a block stops
// the stream at the event that completes the shortest match, so what reached
// the app before it is the prefix and fewer than 20 key characters.
func TestAnswerPatterns_StreamedSecretFoundEarly(t *testing.T) {
	e := NewEngine()
	cases := []struct{ id, prefix, secret string }{
		{"openai_key", "sk-proj-", fakeOpenAI},
		{"openai_key", "sk-", fakeOpenAIPlain},
		{"anthropic_key", "sk-ant-", fakeAnthropic},
		{"aws_access_key", "AKIA", fakeAWS},
		{"github_token", "ghp_", fakeGitHub},
		{"github_token", "github_pat_", "github_pat_" + strings.Repeat("Ab3_xQ", 10)},
		{"slack_token", "xoxb-", fakeSlack},
		{"google_api_key", "AIza", fakeGoogle},
		{"burrow_automation_token", "bua_", fakeAutomation},
		{"burrow_gateway_key", "bgw_", fakeGatewayKey},
	}
	for _, c := range cases {
		s := NewStreamScanner(e.InspectAnswerBundled, nil)
		text := "here it is: " + c.secret + " done"
		start := strings.Index(text, c.secret)
		delivered := -1
		for i := 0; i < len(text); i += 3 {
			end := min(i+3, len(text))
			if hit, id := s.Add(text[i:end]); hit {
				if id != c.id {
					t.Errorf("%s: matched %q", c.id, id)
				}
				delivered = max(0, i-start) // bytes of the secret sent before this event
				break
			}
		}
		if delivered < 0 {
			t.Errorf("%s: streamed secret not found", c.id)
			continue
		}
		if limit := len(c.prefix) + 20; delivered >= limit {
			t.Errorf("%s: %d characters of the secret delivered before the hit, want fewer than %d", c.id, delivered, limit)
		}
	}
}

var answerPlaceholders = []string{
	"ghp" + "_" + strings.Repeat("x", 20),
	"ghp" + "_" + strings.Repeat("x", 36),
	"github_pat" + "_" + strings.Repeat("0", 30),
	"sk" + "-" + strings.Repeat("X", 24),
	"sk" + "-proj-" + strings.Repeat("X", 24),
	"sk" + "-ant-api03-" + strings.Repeat("x", 24),
	"AIza" + "Sy" + strings.Repeat("0", 18),
	"AKIA" + strings.Repeat("X", 16),
	"xoxb" + "-" + strings.Repeat("0", 12),
	"bua" + "_" + strings.Repeat("A", 40),
	"bgw" + "_" + strings.Repeat("a", 40),
	"sk" + "-" + "abababababababababababab",
}

// A placeholder (a secret part of fewer than 6 distinct characters) is no
// secret.
func TestAnswerPatterns_PlaceholdersAreNoSecrets(t *testing.T) {
	e := NewEngine()
	for _, p := range answerPlaceholders {
		if ok, id := e.InspectAnswerBundled("set KEY=" + p + " in your env"); ok {
			t.Errorf("placeholder %q matched %s", p, id)
		}
	}
	// A real key next to a placeholder is still found.
	if ok, _ := e.InspectAnswerBundled(answerPlaceholders[0] + " and " + fakeGitHub); !ok {
		t.Error("real key after a placeholder not found")
	}
}

// A placeholder streamed in pieces is never blocked, whatever has arrived;
// a real key is, at the same early bound as before.
func TestAnswerPatterns_PlaceholdersStreamedAreNeverBlocked(t *testing.T) {
	e := NewEngine()
	for _, p := range answerPlaceholders {
		for _, step := range []int{1, 3, 7} {
			s := NewStreamScanner(e.InspectAnswerBundled, nil)
			text := "use " + p + " here"
			for i := 0; i < len(text); i += step {
				if hit, id := s.Add(text[i:min(i+step, len(text))]); hit {
					t.Fatalf("placeholder %q (step %d) blocked as %s", p, step, id)
				}
			}
		}
	}
}

// The drop of a placeholder is not sticky: a key that starts like one (its
// first 20 secret characters have fewer than 6 distinct ones) and goes on
// with random characters is blocked as soon as the match has 6, before the
// key ends.
func TestAnswerPatterns_StreamedKeyWithPlaceholderStartIsBlocked(t *testing.T) {
	e := NewEngine()
	key := "ghp" + "_" + strings.Repeat("a", 20) + "Q7zK9mX2pL4vB8nR1sT5wY3"
	for _, step := range []int{1, 3} {
		s := NewStreamScanner(e.InspectAnswerBundled, nil)
		text := "token: " + key + " end"
		start := strings.Index(text, key)
		hitAt := -1
		for i := 0; i < len(text); i += step {
			end := min(i+step, len(text))
			if hit, id := s.Add(text[i:end]); hit {
				if id != "github_token" {
					t.Fatalf("matched %q", id)
				}
				hitAt = end - start // bytes of the key delivered with this event
				break
			}
		}
		if hitAt < 0 {
			t.Fatalf("step %d: key not found", step)
		}
		if hitAt >= len(key) {
			t.Errorf("step %d: blocked only after %d of %d key bytes", step, hitAt, len(key))
		}
	}
}

// A key padded with a placeholder run is not let past the scanner: once the
// secret part is longer than a placeholder can be, the match counts whatever
// its distinct characters, so it is found while its prefix is in the tail.
func TestAnswerPatterns_PaddedKeyStreamedIsBlocked(t *testing.T) {
	e := NewEngine()
	real := "Ab3dE5gH7jK9mN1pQ3sT5vX7zB9cD1fG3hJ5"
	for _, prefix := range []string{"sk-proj-", "ghp" + "_", "bgw" + "_"} {
		text := "x " + prefix + strings.Repeat("x", 600) + real
		if ok, _ := e.InspectAnswerBundled(text); !ok {
			t.Fatalf("%s: padded key not blocked as a whole", prefix)
		}
		for _, step := range []int{1, 7} {
			s := NewStreamScanner(e.InspectAnswerBundled, nil)
			hit := false
			for i := 0; i < len(text) && !hit; i += step {
				hit, _ = s.Add(text[i:min(i+step, len(text))])
			}
			if !hit {
				t.Errorf("%s step %d: padded key streamed past the scanner", prefix, step)
			}
		}
	}
	// A placeholder alone, also a long one within the cap, is still no key.
	for _, n := range []int{20, MaxPlaceholderSecret} {
		text := "ghp" + "_" + strings.Repeat("x", n)
		if ok, _ := e.InspectAnswerBundled(text); ok {
			t.Errorf("placeholder of %d characters blocked", n)
		}
	}
}
