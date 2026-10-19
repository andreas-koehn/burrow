package redact

// Rule is one redaction rule. The same shape is used for built-ins (this
// file's `BuiltIn`) and for custom rules persisted under the settings key
// "redaction.custom_rules" (JSON array). Action ∈ {mask,drop,hash};
// Scope ∈ {request_body,response_body,both,tool_arguments,tool_result}.
//
// Wire shape (custom rules round-trip as):
//
//	{"id":"…","name":"…","pattern":"…","action":"mask","scope":"both"}
//
// IDs for built-ins are stable identifiers (e.g. "email"); IDs for custom
// rules are server-assigned UUIDs by the JSON-API handler.
type Rule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Action  string `json:"action"`
	Scope   string `json:"scope"`
}

// Action enum.
const (
	ActionMask = "mask"
	ActionDrop = "drop"
	ActionHash = "hash"
)

// Scope enum. tool_arguments and tool_result are the arguments and the
// result of an MCP tool call on the tool gateway (D16); "both" matches them
// as well, request_body and response_body do not.
const (
	ScopeRequestBody   = "request_body"
	ScopeResponseBody  = "response_body"
	ScopeBoth          = "both"
	ScopeToolArguments = "tool_arguments"
	ScopeToolResult    = "tool_result"
)

// validActions / validScopes are the closed enums NewEngine validates against.
var validActions = map[string]bool{
	ActionMask: true,
	ActionDrop: true,
	ActionHash: true,
}
var validScopes = map[string]bool{
	ScopeRequestBody:   true,
	ScopeResponseBody:  true,
	ScopeBoth:          true,
	ScopeToolArguments: true,
	ScopeToolResult:    true,
}

// BuiltIn is the bundled regex pack shipped with v0.4.0. These rules are
// always loaded (in addition to any custom rules passed to NewEngine) and
// run in name order — deterministic across runs.
//
// NOTE on the credit_card_luhn pattern: the regex matches any digit-shaped
// substring 13–16 digits long (allowing optional spaces/dashes); a Luhn
// post-filter (engine.go:luhnValid) rejects false positives so order/lot
// numbers don't get redacted.
//
// NOTE on aws_secret: AWS secret access keys are exactly 40 base64-ish
// characters with no distinguishing prefix, so this regex will fire on
// any 40-char run of [A-Za-z0-9/+=]. That's the spec — false positives
// are expected and tolerable because the action is `drop` (request is
// refused with 400, not silently rewritten).
var BuiltIn = []Rule{
	{
		ID:      "email",
		Name:    "email",
		Pattern: `\b[\w.+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`,
		Action:  ActionMask,
		Scope:   ScopeBoth,
	},
	{
		ID:      "ipv4",
		Name:    "ipv4",
		Pattern: `\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`,
		Action:  ActionMask,
		Scope:   ScopeBoth,
	},
	{
		ID:      "ipv6",
		Name:    "ipv6",
		Pattern: `\b(?:[0-9A-Fa-f]{1,4}:){2,7}[0-9A-Fa-f]{1,4}\b`,
		Action:  ActionMask,
		Scope:   ScopeBoth,
	},
	{
		ID:      "aws_access_key",
		Name:    "aws_access_key",
		Pattern: `\bAKIA[0-9A-Z]{16}\b`,
		Action:  ActionDrop,
		Scope:   ScopeRequestBody,
	},
	{
		ID:      "aws_secret",
		Name:    "aws_secret",
		Pattern: `\b[A-Za-z0-9/+=]{40}\b`,
		Action:  ActionDrop,
		Scope:   ScopeRequestBody,
	},
	{
		ID:      "credit_card_luhn",
		Name:    "credit_card_luhn",
		Pattern: `\b(?:\d[ -]*?){13,16}\b`,
		Action:  ActionMask,
		Scope:   ScopeBoth,
	},
	{
		ID:      "ssn_us",
		Name:    "ssn_us",
		Pattern: `\b\d{3}-\d{2}-\d{4}\b`,
		Action:  ActionMask,
		Scope:   ScopeBoth,
	},
	{
		ID:      "github_pat",
		Name:    "github_pat",
		Pattern: `\bghp_[A-Za-z0-9]{36}\b`,
		Action:  ActionDrop,
		Scope:   ScopeBoth,
	},
	{
		ID:      "slack_token",
		Name:    "slack_token",
		Pattern: `\bxox[abop]-[A-Za-z0-9\-]+\b`,
		Action:  ActionDrop,
		Scope:   ScopeBoth,
	},
}

// builtInIDs is a quick lookup used by the DELETE handler to refuse deleting
// a built-in rule (spec Part B.4: 409 on built_in delete).
var builtInIDs = func() map[string]bool {
	m := make(map[string]bool, len(BuiltIn))
	for _, r := range BuiltIn {
		m[r.ID] = true
	}
	return m
}()

// IsBuiltIn reports whether the rule id refers to a bundled built-in.
func IsBuiltIn(id string) bool { return builtInIDs[id] }
