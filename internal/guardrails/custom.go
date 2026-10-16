package guardrails

import (
	"fmt"
	"regexp"
	"unicode/utf8"
)

// CustomPattern is a regular expression an operator adds to the guardrails.
// It runs after the bundled patterns of its direction: on the request body
// (DirectionRequest), on the text of answers (DirectionAnswer) or on both.
// Unlike the bundled ones its source is the operator's and is returned by the
// settings API.
type CustomPattern struct {
	ID          string `json:"id"`          // [a-z0-9_-]{1,64}, unique, not a bundled id
	Pattern     string `json:"pattern"`     // Go RE2, at most MaxCustomPatternLen characters
	Direction   string `json:"direction"`   // "request" | "answer" | "both"
	Description string `json:"description"` // at most MaxCustomPatternDescLen characters
}

// The directions of a custom pattern.
const (
	DirectionRequest = "request"
	DirectionAnswer  = "answer"
	DirectionBoth    = "both"
)

// Limits of the custom patterns.
const (
	MaxCustomPatterns       = 50
	MaxCustomPatternLen     = 512 // characters
	MaxCustomPatternIDLen   = 64
	MaxCustomPatternDescLen = 200 // characters
)

var customIDRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// bundledIDs holds the ids of every bundled pattern, of both directions; a
// custom pattern may not take one.
var bundledIDs = func() map[string]bool {
	m := make(map[string]bool, len(Patterns)+len(AnswerPatterns))
	for _, p := range Patterns {
		m[p.ID] = true
	}
	for _, p := range AnswerPatterns {
		m[p.ID] = true
	}
	return m
}()

// ValidateCustomPatterns checks a list of custom patterns: at most
// MaxCustomPatterns; each id of the form [a-z0-9_-]{1,64}, used once and no
// bundled id; each pattern a non-empty Go RE2 expression of at most
// MaxCustomPatternLen characters; the direction one of request, answer,
// both; the description at most MaxCustomPatternDescLen characters. The
// error names the first pattern that fails, by id, and why: it is written
// to the caller as it is.
func ValidateCustomPatterns(ps []CustomPattern) error {
	if len(ps) > MaxCustomPatterns {
		return fmt.Errorf("at most %d custom patterns are allowed, got %d", MaxCustomPatterns, len(ps))
	}
	seen := make(map[string]bool, len(ps))
	for _, p := range ps {
		switch {
		case !customIDRe.MatchString(p.ID):
			return fmt.Errorf("custom pattern %q: the id must be 1 to %d characters of a-z, 0-9, _ and -", p.ID, MaxCustomPatternIDLen)
		case bundledIDs[p.ID]:
			return fmt.Errorf("custom pattern %q: the id is taken by a bundled pattern", p.ID)
		case seen[p.ID]:
			return fmt.Errorf("custom pattern %q: the id is used more than once", p.ID)
		case p.Pattern == "":
			return fmt.Errorf("custom pattern %q: the pattern is empty", p.ID)
		case utf8.RuneCountInString(p.Pattern) > MaxCustomPatternLen:
			return fmt.Errorf("custom pattern %q: the pattern is longer than %d characters", p.ID, MaxCustomPatternLen)
		case !validDirection(p.Direction):
			return fmt.Errorf("custom pattern %q: the direction must be request, answer or both", p.ID)
		case utf8.RuneCountInString(p.Description) > MaxCustomPatternDescLen:
			return fmt.Errorf("custom pattern %q: the description is longer than %d characters", p.ID, MaxCustomPatternDescLen)
		}
		if _, err := regexp.Compile(p.Pattern); err != nil {
			return fmt.Errorf("custom pattern %q: not a valid regular expression (Go RE2 syntax): %v", p.ID, err)
		}
		seen[p.ID] = true
	}
	return nil
}

func validDirection(d string) bool {
	return d == DirectionRequest || d == DirectionAnswer || d == DirectionBoth
}

// appliesTo reports whether p runs in the direction dir (DirectionRequest or
// DirectionAnswer).
func (p CustomPattern) appliesTo(dir string) bool {
	return p.Direction == dir || p.Direction == DirectionBoth
}
