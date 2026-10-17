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
// MaxCustomPatternLen characters that does not match empty text; the direction one of request, answer,
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
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return fmt.Errorf("custom pattern %q: not a valid regular expression (Go RE2 syntax): %v", p.ID, err)
		}
		if re.MatchString("") {
			// It would match every request and every answer.
			return fmt.Errorf("custom pattern %q: the pattern matches empty text, so it would match everything", p.ID)
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

// Effective returns the guardrail settings that apply to a service, from its
// own guardrails section (nil when it has none) and the global settings.
//
// Enabled is the switch of the request patterns only. A section with enabled
// true overrides the global enabled and action; a section with enabled false
// counts as no override (the dashboard writes one with every AI-config save,
// so it does not mean the service opted out).
//
// Answer inspection, custom patterns and the judge are always the global
// ones and run on their own switches (Answers.Enabled, Judge.Enabled),
// whether or not the request patterns are on: a service cannot be exempted
// from them.
//
// The result is nil when the service has no section and nothing is on
// globally: the service then has no guardrails at all. The result is a copy;
// the custom patterns are shared and must not be changed.
func Effective(own *Settings, global Settings) *Settings {
	switch {
	case own != nil && own.Enabled:
		s := *own
		s.Answers, s.CustomPatterns, s.Judge = global.Answers, global.CustomPatterns, global.Judge
		return &s
	case global.Enabled:
		s := global
		return &s
	case own != nil:
		s := *own
		s.Answers, s.CustomPatterns, s.Judge = global.Answers, global.CustomPatterns, global.Judge
		return &s
	case global.Answers.Enabled || global.Judge.Enabled:
		s := global
		return &s
	}
	return nil
}
