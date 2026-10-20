package guardrails

// AnswerPatterns is the bundled list of answer patterns: the shapes of
// secrets a model should never hand back. They run on the text of an answer
// (see AnswerText and DeltaText), never on its JSON, and never on requests.
// Like Patterns, the ids are stable and the regex sources are not exposed
// over the API.
//
// Every pattern needs the shape's fixed prefix and a run of key characters
// long enough that ordinary prose does not reach it, and no longer: 20 key
// characters after the prefix (AWS: its fixed 16), never the whole token. A
// streamed answer is blocked at the event that completes the shortest match,
// so the shorter it is, the less of a secret reaches the app before the
// stream stops.
//
// A key may come glued to the word before it ("Slackxoxb-…"), which a model
// told to evade the check can do on purpose. Where the prefix is distinctive
// enough that no ordinary word ends in it, the pattern does not ask for a
// word boundary before it. The OpenAI shapes keep theirs: "sk-" ends English
// words and CSS or resource names ("task-…", "kiosk-admin-…"), which would
// turn into keys. For the same reason the Anthropic shape accepts a digit or
// punctuation before "sk-ant-" but no letter ("task-ant-design-…" is a CSS
// name). The AWS shape accepts an underscore before "AKIA" ("KEY_AKIA…") but
// no letter or digit: a longer upper-case token is no key. The Google,
// GitHub and Burrow shapes accept no base64 character before their prefix
// (see base64Before): base64 text (data URIs, JWTs) holds "AIza", "ghp_" or
// "bgw_" by chance, so a key glued to a letter or digit is not found there,
// only one glued to punctuation.
//
// The order matters where two shapes overlap: an Anthropic key also has the
// OpenAI shape and is listed first, so it is named for what it is.
//
// A match whose secret part (what follows the prefix and the kind, see
// Pattern.SecretPrefix) has fewer than MinSecretDistinct distinct characters
// is a placeholder ("ghp_xxxxxxxxxxxxxxxxxxxx") and is dropped. The rule
// looks at the match as far as it has arrived, so a placeholder streamed in
// pieces is never blocked, and a real key, which has that many distinct
// characters within its first 20, is still found at the same early bound.
// base64Before is what may come before the prefix of the Google, GitHub and
// Burrow shapes: the start of the text or one character outside the base64
// and base64url alphabets and the JWT separator (not A-Z, a-z, 0-9, +, /, -,
// _ or .). Go's regexp has no lookbehind, so the match includes it.
const base64Before = `(?:^|[^0-9A-Za-z+/_.-])`

var AnswerPatterns = []Pattern{
	// Anthropic API keys: "sk-ant-api03-…", "sk-ant-admin01-…".
	{ID: "anthropic_key", Description: "Anthropic API key", Regex: `(?:^|[^A-Za-z])sk-ant-[A-Za-z0-9_-]{20,}`, Requires: []string{"sk-ant-"}, SecretPrefix: `^[^A-Za-z]?sk-ant-(?:[a-z]+[0-9]+-)?`},
	// OpenAI API keys: the project, service-account and admin keys
	// ("sk-proj-…", "sk-svcacct-…", "sk-admin-…", 20 or more key characters
	// after the kind) and legacy keys ("sk-" and 20 or more letters and
	// digits, no hyphen). Hyphenated words after "sk-" (CSS classes such as
	// "sk-fading-circle-container-big") are no key.
	{ID: "openai_key", Description: "OpenAI API key", Regex: `\bsk-(?:proj|svcacct|admin)-[A-Za-z0-9_-]{20,}|\bsk-[A-Za-z0-9]{20,}`, Requires: []string{"sk-"}, SecretPrefix: `^sk-(?:(?:proj|svcacct|admin)-)?`},
	// AWS access key ids: "AKIA" and 16 upper-case letters or digits.
	{ID: "aws_access_key", Description: "AWS access key id", Regex: `(?:\b|_)AKIA[0-9A-Z]{16}\b`, Requires: []string{"AKIA"}, SecretPrefix: `^_?AKIA`},
	// GitHub tokens: classic (ghp_, gho_, ghu_, ghs_, ghr_) and fine-grained
	// (github_pat_), after base64Before.
	{ID: "github_token", Description: "GitHub token", Regex: base64Before + `(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`, Requires: []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}, SecretPrefix: `^` + base64Before + `?(?:gh[pousr]_|github_pat_)`},
	// Slack tokens: bot, app, user, refresh and legacy (xoxb-, xoxa-, xoxp-,
	// xoxr-, xoxs-).
	{ID: "slack_token", Description: "Slack token", Regex: `xox[baprs]-[A-Za-z0-9-]{10,}`, Requires: []string{"xoxb-", "xoxa-", "xoxp-", "xoxr-", "xoxs-"}, SecretPrefix: `^xox[baprs]-`},
	// Google API keys: "AIza" and 35 characters; 20 are matched. The prefix
	// follows base64Before, so "AIza" inside a data URI or a JWT is no key;
	// "?key=AIza…" and a quoted key are.
	{ID: "google_api_key", Description: "Google API key", Regex: base64Before + `AIza[0-9A-Za-z_-]{20,}`, Requires: []string{"AIza"}, SecretPrefix: `^` + base64Before + `?AIza`},
	// The header line of a PEM private key of any kind (RSA, EC, OPENSSH,
	// ENCRYPTED, PKCS#8).
	{ID: "private_key_pem", Description: "private key (PEM)", Regex: `-----BEGIN [A-Z ]*PRIVATE KEY-----`, Requires: []string{"PRIVATE KEY-----"}},
	// Burrow automation tokens: "bua_" and 32 random bytes in unpadded
	// standard base32 (52 characters of A-Z and 2-7), see
	// store.generateBearerToken; 20 are matched. After base64Before.
	{ID: "burrow_automation_token", Description: "Burrow automation token", Regex: base64Before + `bua_[A-Z2-7]{20,}`, Requires: []string{"bua_"}, SecretPrefix: `^` + base64Before + `?bua_`},
	// Burrow gateway keys: "bgw_" and 32 random bytes in unpadded base64url
	// (43 characters), see store.newGatewayKey; 20 are matched. After
	// base64Before.
	{ID: "burrow_gateway_key", Description: "Burrow gateway key", Regex: base64Before + `bgw_[A-Za-z0-9_-]{20,}`, Requires: []string{"bgw_"}, SecretPrefix: `^` + base64Before + `?bgw_`},
}
