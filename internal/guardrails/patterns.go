// Package guardrails is Burrow's prompt-injection detection tier (v0.4.0
// Task 6). It is a *pure regex* engine — no LLM call, no Presidio, no
// network I/O — that screens an inbound request body against a closed
// bundled list of ~30 patterns and returns the first match's stable ID
// (used downstream by the inspector and audit log).
//
// Three caller-side actions are wired by the proxy hot path (Task 12):
//
//   - log_only    — engine fires, the proxy records the hit but otherwise
//     forwards the request unchanged.
//   - refuse_403  — engine fires, the proxy short-circuits with
//     403 {"error":"guardrail.refuse"}.
//   - refuse_safe — engine fires, the proxy short-circuits with a 200
//     upstream-shaped safe-refusal body (OpenAI / Anthropic). The
//     engine itself never renders the body; it only returns the (hit,
//     pattern_id) tuple. The caller picks the body shape from the
//     upstream API family.
//
// The bundled pattern list is closed (curated at build time). The public
// HTTP surface exposes IDs and human descriptions, NOT regex sources —
// the source is internal knowledge that hardens the engine against
// attacker-side adaptation.
package guardrails

// Pattern is one bundled injection-detection rule. ID is a short stable
// identifier ("ignore_prev", "role_tag_system", …) used in inspector and
// audit log; Description is a human-readable label safe to expose on
// the JSON API; Regex is the Go regexp source compiled at startup —
// never returned over the wire.
type Pattern struct {
	ID          string
	Description string
	Regex       string
	// Requires lists literals of which the regex needs at least one to match
	// (case-sensitive). The engine skips the regex when the text holds none
	// of them, which is far cheaper than running it. Empty: always run.
	Requires []string
}

// Patterns is the closed list of v0.4.0 prompt-injection rules. Exactly
// 30 entries (asserted by TestPatterns_Count) — additions are a wire
// change. IDs are STABLE: downstream consumers (inspector trace, audit
// log) key off them; renaming an ID is a breaking change.
//
// Iteration order matters: Engine.Inspect returns the first match, so
// more-specific or higher-priority rules are listed first. The list is
// ordered roughly by signal strength (a verbatim "ignore previous"
// outranks a generic role-tag heuristic).
//
// Heuristic notes:
//
//   - hidden_unicode catches BIDI / ZWSP / RTL-override injection. Such
//     codepoints appear in normal text approximately never; a single hit
//     is a strong signal.
//   - emoji_smuggle requires ≥30 consecutive non-word non-space chars,
//     which catches ASCII-art smuggling but won't fire on a couple of
//     emoji in flowing prose.
//   - long_repeat catches token-bomb attacks (>200 of the same char).
var Patterns = []Pattern{
	// 1 — verbatim "ignore previous/above/prior instructions".
	{ID: "ignore_prev", Description: "ignore previous instructions", Regex: `(?i)\bignore (?:all |the )?(?:previous|above|prior) (?:instructions?|prompts?|messages?)\b`},
	// 2 — "ignore everything above / earlier".
	{ID: "ignore_above", Description: "ignore the above", Regex: `(?i)\bignore (?:everything |all )?(?:above|earlier)\b`},
	// 3 — persona override ("you are now …", "pretend to be …", "act as …").
	{ID: "you_are_now", Description: "persona override", Regex: `(?i)\byou are now\b|\byou are no longer\b|\bact as\b|\bact like\b|\bpretend to (?:be|act)\b`},
	// 4 — bare role-tag injection: a "system:" line embedded in user input.
	{ID: "role_tag_system", Description: "injected system role tag", Regex: `(?i)(?:^|\n)\s*system\s*:\s*`},
	// 5 — bare assistant role-tag injection.
	{ID: "role_tag_assistant", Description: "injected assistant role tag", Regex: `(?i)(?:^|\n)\s*assistant\s*:\s*`},
	// 6 — bare user role-tag injection.
	{ID: "role_tag_user", Description: "injected user role tag", Regex: `(?i)(?:^|\n)\s*user\s*:\s*`},
	// 7 — bare developer role-tag injection (OpenAI o-series).
	{ID: "role_tag_developer", Description: "injected developer role tag", Regex: `(?i)(?:^|\n)\s*developer\s*:\s*`},
	// 8 — "reveal/show/print your system prompt / instructions / rules".
	{ID: "prompt_leak", Description: "reveal system prompt", Regex: `(?i)(?:repeat|show|reveal|tell me|print) (?:your |the )?(?:system |initial |original )?(?:prompt|instructions?|rules?)\b`},
	// 9 — leak the user-turn history above ("what did the user say above").
	{ID: "prompt_leak_above", Description: "reveal what was said above", Regex: `(?i)(?:what (?:were|did) (?:i|the user) (?:say|tell|write|input) (?:above|earlier|previously)|repeat what (?:i|the user) (?:said|wrote))\b`},
	// 10 — explicit "disregard / bypass / override safety rules".
	{ID: "override_rules", Description: "override safety rules", Regex: `(?i)\b(?:disregard|bypass|override|disable) (?:your |the )?(?:safety |guard ?rails?|content policy|policies|filters?|moderation|restrictions?|rules?)\b`},
	// 11 — DAN-style jailbreak slogans.
	{ID: "jailbreak_dan", Description: "DAN-style jailbreak", Regex: `(?i)\b(?:dan|do anything now|jailbreak|gpt[- ]?[34][a-z]?[- ]?jailbreak)\b`},
	// 12 — "developer mode" / "god mode" / "admin mode" / "sudo mode".
	{ID: "jailbreak_dev_mode", Description: "developer-mode jailbreak", Regex: `(?i)\b(?:developer mode|god mode|admin mode|sudo mode)\b`},
	// 13 — "new instructions follow" — common prefix to injected directives.
	{ID: "new_instructions", Description: "new instructions follow", Regex: `(?i)\bnew (?:instructions?|rules?)(?: follow)?\b`},
	// 14 — end-of-prompt markers ("### END OF PROMPT ###", "</system>", etc).
	{ID: "end_of_prompt", Description: "end-of-prompt marker injection", Regex: `(?i)(?:###\s*end\s*(?:of)?\s*(?:prompt|instructions?)\s*###|</?(?:system|assistant|user|prompt)>)`},
	// 15 — hidden bidi / ZWSP / RTL-override / LRI/RLI characters.
	{ID: "hidden_unicode", Description: "hidden bidi/zwsp characters", Regex: `[\x{200B}-\x{200F}\x{2028}-\x{202E}\x{2066}-\x{2069}]`},
	// 16 — "respond in base64 / rot13 / hex / leet / reverse" — evasion via encoding.
	{ID: "format_jailbreak", Description: "format-as-X jailbreak", Regex: `(?i)\b(?:respond|reply|answer) (?:only |solely )?(?:in|as|with) (?:a |an )?(?:base64|rot13|hex|leet|reverse)\b`},
	// 17 — smuggled chat-template markers (im_start, endoftext, [[INSTRUCTION]]).
	{ID: "payload_smuggle", Description: "smuggled chat-template markers", Regex: `(?i)<\|im_start\|>|<\|im_end\|>|<\|endoftext\|>|\[\[INSTRUCTION\]\]`},
	// 18 — "opposite day" / "reverse all" / "negate / invert all" instructions.
	{ID: "opposite_day", Description: "invert all instructions", Regex: `(?i)\b(?:opposite day|reverse all|negate all|invert all)\b`},
	// 19 — "trust me, I am the developer/owner/admin/root/operator".
	{ID: "trust_me", Description: "trust me / I am the developer", Regex: `(?i)\b(?:i am (?:the |your )?(?:developer|owner|admin|creator|operator|root)|trust me)\b`},
	// 20 — exfiltration: "write/save/upload/post the output to …".
	{ID: "output_to_file", Description: "exfiltrate output to disk/network", Regex: `(?i)\b(?:write|save|append|exfiltrate|upload|post) (?:this |your |the )?(?:output|response|answer|history|memory|prompt) to\b`},
	// 21 — "reveal / show / print the api keys / tokens / secrets / credentials".
	{ID: "reveal_keys", Description: "reveal API keys / secrets", Regex: `(?i)\b(?:reveal|show|print|give me|tell me) (?:the |any |all )?(?:api[- ]?keys?|tokens?|secrets?|credentials?)\b`},
	// 22 — "run/exec/invoke every function or tool".
	{ID: "function_override", Description: "override function/tool boundaries", Regex: `(?i)\b(?:run|exec(?:ute)?|invoke|call) (?:any|all|every) (?:function|tool)\b`},
	// 23 — "your new goal is …" / "forget your purpose".
	{ID: "goal_drift", Description: "replace primary goal", Regex: `(?i)\b(?:your (?:new |real |true )?(?:goal|purpose|mission) is|forget your (?:goal|purpose))\b`},
	// 24 — "uncensored / unfiltered / nsfw mode / no filters / no restrictions".
	{ID: "safety_off", Description: "safety off / unfiltered", Regex: `(?i)\b(?:no (?:filters?|restrictions?|safety|guard ?rails?|censorship)|uncensored|unfiltered|nsfw mode)\b`},
	// 25 — ≥30 consecutive non-word non-space chars (ASCII-art / emoji smuggle).
	{ID: "emoji_smuggle", Description: "smuggled instructions via punctuation/emoji art", Regex: `[^\w\s]{30,}`},
	// 26 — token-bomb: same char repeated ≥200 times. Go's regexp is RE2 and
	// does NOT support backreferences, so we enumerate the common
	// token-bomb characters (alphanumerics + space + punctuation likely to
	// appear in attacks). Each alternative requires ≥200 consecutive
	// identical bytes — well clear of legitimate use (markdown horizontal
	// rules, separators) which top out around a few dozen.
	{ID: "long_repeat", Description: "absurdly long character repetition (token-bomb)", Regex: `a{200,}|b{200,}|c{200,}|d{200,}|e{200,}|f{200,}|g{200,}|h{200,}|i{200,}|j{200,}|k{200,}|l{200,}|m{200,}|n{200,}|o{200,}|p{200,}|q{200,}|r{200,}|s{200,}|t{200,}|u{200,}|v{200,}|w{200,}|x{200,}|y{200,}|z{200,}|0{200,}|1{200,}|2{200,}|3{200,}|4{200,}|5{200,}|6{200,}|7{200,}|8{200,}|9{200,}| {200,}|\.{200,}|\-{200,}|_{200,}|={200,}|\*{200,}|#{200,}|\!{200,}|\?{200,}`},
	// 27 — chain-of-thought override ("think step by step but then output …").
	{ID: "override_chain", Description: "chain-of-thought override", Regex: `(?i)\b(?:think step by step|chain of thought|hidden chain|internal monologue)(?: but| and) (?:then |finally |actually )?(?:do|say|output)\b`},
	// 28 — two-phase prompt override ("first do X then output Y").
	{ID: "two_phase", Description: "two-phase prompt override", Regex: `(?i)\b(?:first .*?(?:then|after that) (?:do|say|output|reply|answer)|phase 1.*?phase 2)\b`},
	// 29 — "continue anyway / regardless / despite the rules".
	{ID: "continue_anyway", Description: "continue regardless of policy", Regex: `(?i)\b(?:continue|proceed|reply) (?:anyway|regardless|despite (?:any|all|the))\b`},
	// 30 — "respond only in <unusual language>" — evasion via low-coverage language.
	{ID: "override_lang", Description: "force a specific (often low-coverage) language", Regex: `(?i)\b(?:respond|reply|answer) (?:only )?in (?:base64|chinese|russian|esperanto|klingon|piglatin)\b`},
}

// Settings is the wire-shape of guardrail settings persisted in the
// settings table (key: "guardrails.global") and per-service in
// service_ai_config.config.guardrails.
//
// Enabled is the master toggle. When false the engine MUST be skipped on
// the proxy hot path (the regex compile is cheap but allocation-free
// matching at thousands of bytes per request is non-trivial; the toggle
// is the cleanest seam).
//
// Action selects the caller's behavior when a hit fires:
//
//   - ActionLogOnly    — record the hit, forward the request unchanged.
//   - ActionRefuse403  — short-circuit with 403 {"error":"guardrail.refuse"}.
//   - ActionRefuseSafe — short-circuit with 200 and an upstream-shaped
//     safe-refusal body (the body is the caller's responsibility — the
//     engine itself returns only (hit, pattern_id)).
//
// Answers and CustomPatterns are global: they are read from the global blob
// only. A service's own guardrails section overrides Enabled and Action when
// its Enabled is true; see Effective for how the settings of a service are
// put together. Where the guardrails are off, answers are not inspected
// either.
type Settings struct {
	Enabled        bool            `json:"enabled"`
	Action         string          `json:"action"`
	Answers        AnswerSettings  `json:"answers"`
	CustomPatterns []CustomPattern `json:"custom_patterns"`
}

// AnswerSettings switches the inspection of answers on and says what a hit
// does: AnswerActionFlag delivers the answer and records the hit,
// AnswerActionBlock withholds it (see internal/aigw's answer guard).
type AnswerSettings struct {
	Enabled bool   `json:"enabled"`
	Action  string `json:"action"` // "flag" (default when enabled) | "block"
}

// The actions on an answer.
const (
	AnswerActionFlag  = "flag"
	AnswerActionBlock = "block"
)

// Normalized returns s with the defaults of the fields an older blob does
// not have: the answer action "flag" and an empty (non-nil) list of custom
// patterns.
func (s Settings) Normalized() Settings {
	if s.Answers.Action == "" {
		s.Answers.Action = AnswerActionFlag
	}
	if s.CustomPatterns == nil {
		s.CustomPatterns = []CustomPattern{}
	}
	return s
}

// Action enum. The closed set is enforced by the JSON-API handler's
// ValidActions check; an invalid action on PUT /api/v1/guardrails/settings
// is rejected with 400.
const (
	ActionLogOnly    = "log_only"
	ActionRefuse403  = "refuse_403"
	ActionRefuseSafe = "refuse_safe"
)

// ValidActions is the closed enum the API handler validates against.
// Kept as a map so the handler can do an O(1) check.
var ValidActions = map[string]bool{
	ActionLogOnly:    true,
	ActionRefuse403:  true,
	ActionRefuseSafe: true,
}

// DefaultSettings is the v0.4.0 default applied when no row exists in the
// settings table: disabled, action=log_only. Locks "safe by default" —
// upgrading to v0.4.0 does NOT silently start refusing traffic.
var DefaultSettings = Settings{
	Enabled:        false,
	Action:         ActionLogOnly,
	Answers:        AnswerSettings{Enabled: false, Action: AnswerActionFlag},
	CustomPatterns: []CustomPattern{},
}
