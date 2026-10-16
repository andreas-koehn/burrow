package guardrails

import (
	"fmt"
	"strings"
	"testing"
)

func validCustom() []CustomPattern {
	return []CustomPattern{
		{ID: "ticket", Pattern: `TICKET-\d{4}`, Direction: "request", Description: "internal ticket numbers"},
		{ID: "codename_2", Pattern: `(?i)project falcon`, Direction: "answer"},
		{ID: "host-name", Pattern: `\b[a-z0-9-]+\.corp\.internal\b`, Direction: "both", Description: strings.Repeat("d", 200)},
	}
}

func TestValidateCustomPatterns_Accepts(t *testing.T) {
	if err := ValidateCustomPatterns(validCustom()); err != nil {
		t.Fatalf("valid set refused: %v", err)
	}
	if err := ValidateCustomPatterns(nil); err != nil {
		t.Fatalf("empty set refused: %v", err)
	}
	full := make([]CustomPattern, MaxCustomPatterns)
	for i := range full {
		full[i] = CustomPattern{ID: fmt.Sprintf("p%d", i), Pattern: "x", Direction: "both"}
	}
	if err := ValidateCustomPatterns(full); err != nil {
		t.Fatalf("%d patterns refused: %v", MaxCustomPatterns, err)
	}
	long := []CustomPattern{{ID: "long", Pattern: strings.Repeat("é", MaxCustomPatternLen), Direction: "both"}}
	if err := ValidateCustomPatterns(long); err != nil {
		t.Fatalf("a pattern of %d characters refused: %v", MaxCustomPatternLen, err)
	}
}

func TestValidateCustomPatterns_Refuses(t *testing.T) {
	tooMany := make([]CustomPattern, MaxCustomPatterns+1)
	for i := range tooMany {
		tooMany[i] = CustomPattern{ID: fmt.Sprintf("p%d", i), Pattern: "x", Direction: "both"}
	}
	with := func(p CustomPattern) []CustomPattern { return append(validCustom(), p) }
	cases := []struct {
		name string
		in   []CustomPattern
		want []string // all must appear in the message
	}{
		{"51 patterns", tooMany, []string{"50"}},
		{"513 characters", with(CustomPattern{ID: "big", Pattern: strings.Repeat("a", 513), Direction: "both"}), []string{`"big"`, "512"}},
		{"invalid RE2", with(CustomPattern{ID: "lookbehind", Pattern: `(?<=x)`, Direction: "both"}), []string{`"lookbehind"`, "regular expression"}},
		{"duplicate id", with(CustomPattern{ID: "ticket", Pattern: "y", Direction: "both"}), []string{`"ticket"`, "more than once"}},
		{"bundled request id", with(CustomPattern{ID: "ignore_prev", Pattern: "y", Direction: "both"}), []string{`"ignore_prev"`, "bundled"}},
		{"bundled answer id", with(CustomPattern{ID: "openai_key", Pattern: "y", Direction: "both"}), []string{`"openai_key"`, "bundled"}},
		{"bad direction", with(CustomPattern{ID: "dir", Pattern: "y", Direction: "sideways"}), []string{`"dir"`, "direction"}},
		{"empty direction", with(CustomPattern{ID: "nodir", Pattern: "y"}), []string{`"nodir"`, "direction"}},
		{"bad id", with(CustomPattern{ID: "Bad ID", Pattern: "y", Direction: "both"}), []string{`"Bad ID"`, "id"}},
		{"empty id", with(CustomPattern{ID: "", Pattern: "y", Direction: "both"}), []string{`""`, "id"}},
		{"long id", with(CustomPattern{ID: strings.Repeat("a", 65), Pattern: "y", Direction: "both"}), []string{"id"}},
		{"empty pattern", with(CustomPattern{ID: "empty", Pattern: "", Direction: "both"}), []string{`"empty"`, "pattern"}},
		{"long description", with(CustomPattern{ID: "desc", Pattern: "y", Direction: "both", Description: strings.Repeat("d", 201)}), []string{`"desc"`, "200"}},
	}
	for _, c := range cases {
		err := ValidateCustomPatterns(c.in)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: message %q does not contain %q", c.name, err.Error(), w)
			}
		}
	}
}
