package guardrails

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// compiled is one Pattern paired with its precompiled *regexp.Regexp.
// Engine holds these in iteration order so Inspect can short-circuit on
// the first match without re-sorting.
type compiled struct {
	Pattern
	re *regexp.Regexp
	// strip matches the start of a match before its secret part; nil when
	// the pattern has no SecretPrefix.
	strip *regexp.Regexp
}

// Engine is the prompt-injection regex tier. Construct with NewEngine
// (compiles every bundled pattern at startup); call Inspect on every
// inbound request body the caller wants to screen.
//
// Engine is concurrency-safe after NewEngine returns: the compiled rules
// are read-only and *regexp.Regexp is itself goroutine-safe.
type Engine struct {
	rules   []compiled // the bundled request patterns
	answers []compiled // the bundled answer patterns

	// custom holds the compiled custom patterns by source. The settings hand
	// the patterns to every call; each source is compiled once.
	mu     sync.RWMutex
	custom map[string]*regexp.Regexp // nil value: the source does not compile
}

// maxCompiledCustom bounds the cache of compiled custom patterns. Settings
// hold at most MaxCustomPatterns; the cache is emptied when edits over time
// have filled it with sources no longer in use.
const maxCompiledCustom = 4 * MaxCustomPatterns

// NewEngine compiles the bundled Patterns list and returns a ready-to-use
// Engine. Every bundled regex is author-vetted; a compile failure is a
// programming error (and would already break tests at CI time), so
// NewEngine panics on compile failure rather than returning err — the
// caller (cmd/server) does not need an err return path for a closed
// build-time pattern set.
//
// Pattern iteration order matches Patterns declaration order: Inspect
// returns the first match it finds, so high-priority rules are listed
// first in patterns.go.
func NewEngine() *Engine {
	return &Engine{rules: compileBundled(Patterns), answers: compileBundled(AnswerPatterns), custom: map[string]*regexp.Regexp{}}
}

func compileBundled(ps []Pattern) []compiled {
	out := make([]compiled, 0, len(ps))
	for _, p := range ps {
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			// Closed pattern set, compiled at startup — a failure here is
			// a code defect (caught by TestPatterns_AllCompile). Panic
			// makes the process refuse to start rather than silently miss
			// a rule.
			panic(fmt.Sprintf("guardrails: invalid bundled regex for %q: %v", p.ID, err))
		}
		c := compiled{Pattern: p, re: re}
		if p.SecretPrefix != "" {
			// Closed pattern set, as above.
			c.strip = regexp.MustCompile(p.SecretPrefix)
		}
		out = append(out, c)
	}
	return out
}

// Inspect screens body against every bundled pattern in declaration
// order, returning (true, pattern.ID) on the first match. body may be
// nil or empty — both yield (false, "").
//
// Inspect does NOT allocate a copy of body; it passes the input directly
// to *regexp.Regexp.Match, which is safe to call on a shared slice (the
// caller still owns the byte buffer).
//
// The returned ID is the *stable* identifier (Pattern.ID), NOT the regex
// source. Audit and inspector consumers store this ID verbatim so the
// regex source can evolve without breaking historical logs.
func (e *Engine) Inspect(body []byte) (hit bool, pattern string) {
	if e == nil || len(body) == 0 {
		return false, ""
	}
	for i := range e.rules {
		if e.rules[i].re.Match(body) {
			return true, e.rules[i].ID
		}
	}
	return false, ""
}

// InspectRequest screens a request body: the bundled request patterns in
// declaration order, then the custom patterns with direction "request" or
// "both" in the order given. It returns the id of the first that matches.
func (e *Engine) InspectRequest(body []byte, custom []CustomPattern) (hit bool, id string) {
	if e == nil || len(body) == 0 {
		return false, ""
	}
	if hit, id := e.Inspect(body); hit {
		return true, id
	}
	for _, p := range custom {
		if !p.appliesTo(DirectionRequest) {
			continue
		}
		if re := e.compiledCustom(p.Pattern); re != nil && re.Match(body) {
			return true, p.ID
		}
	}
	return false, ""
}

// InspectAnswerText screens the text of an answer (never its JSON): the
// bundled AnswerPatterns, then the custom patterns with direction "answer" or
// "both". It returns the id of the first that matches.
func (e *Engine) InspectAnswerText(text string, custom []CustomPattern) (hit bool, id string) {
	if hit, id := e.InspectAnswerBundled(text); hit {
		return true, id
	}
	return e.InspectAnswerCustom(text, custom)
}

// InspectAnswerBundled screens the text of an answer against the bundled
// AnswerPatterns only.
func (e *Engine) InspectAnswerBundled(text string) (hit bool, id string) {
	if e == nil || text == "" {
		return false, ""
	}
	for i := range e.answers {
		if e.answers[i].mayMatch(text) && e.answers[i].matches(text) {
			return true, e.answers[i].ID
		}
	}
	return false, ""
}

// matches reports whether the pattern matches text with a secret part: a
// match of a placeholder (fewer than MinSecretDistinct distinct characters
// after the prefix) does not count, and the search goes on after it.
func (c *compiled) matches(text string) bool {
	if c.strip == nil {
		return c.re.MatchString(text)
	}
	for _, loc := range c.re.FindAllStringIndex(text, -1) {
		m := text[loc[0]:loc[1]]
		secret := m[len(c.strip.FindString(m)):]
		// A longer match counts whatever it holds: padding a key with a
		// placeholder run must not push it past the scanner's tail.
		if len(secret) > MaxPlaceholderSecret || secretDistinct(secret) >= MinSecretDistinct {
			return true
		}
	}
	return false
}

// secretDistinct counts the distinct bytes of s (a secret part is ASCII).
func secretDistinct(s string) int {
	var seen [256]bool
	n := 0
	for i := 0; i < len(s); i++ {
		if !seen[s[i]] {
			seen[s[i]] = true
			n++
		}
	}
	return n
}

// mayMatch reports whether text holds one of the literals the pattern
// requires (always, for a pattern that requires none).
func (c *compiled) mayMatch(text string) bool {
	if len(c.Requires) == 0 {
		return true
	}
	for _, lit := range c.Requires {
		if strings.Contains(text, lit) {
			return true
		}
	}
	return false
}

// InspectAnswerCustom screens the text of an answer against the custom
// patterns with direction "answer" or "both" only.
func (e *Engine) InspectAnswerCustom(text string, custom []CustomPattern) (hit bool, id string) {
	if e == nil || text == "" {
		return false, ""
	}
	for _, p := range custom {
		if !p.appliesTo(DirectionAnswer) {
			continue
		}
		if re := e.compiledCustom(p.Pattern); re != nil && re.MatchString(text) {
			return true, p.ID
		}
	}
	return false, ""
}

// compiledCustom returns the compiled form of a custom pattern's source, nil
// when it does not compile. The settings API refuses such a pattern; one that
// reaches the engine anyway (a blob written past the API) is skipped.
func (e *Engine) compiledCustom(src string) *regexp.Regexp {
	e.mu.RLock()
	re, ok := e.custom[src]
	e.mu.RUnlock()
	if ok {
		return re
	}
	re, err := regexp.Compile(src)
	if err != nil {
		re = nil
	}
	e.mu.Lock()
	if e.custom == nil || len(e.custom) >= maxCompiledCustom {
		e.custom = map[string]*regexp.Regexp{}
	}
	e.custom[src] = re
	e.mu.Unlock()
	return re
}
