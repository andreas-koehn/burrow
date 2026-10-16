package guardrails

// AnswerPatterns is the bundled list of answer patterns: the shapes of
// secrets a model should never hand back. They run on the text of an answer
// (see AnswerText and DeltaText), never on its JSON, and never on requests.
// Like Patterns, the ids are stable and the regex sources are not exposed
// over the API.
//
// Every pattern needs the shape's fixed prefix and a run of key characters
// long enough that ordinary prose does not reach it. The prefix starts at a
// word boundary, so "task-…" is no "sk-…". The order matters where two
// shapes overlap: an Anthropic key also has the OpenAI shape and is listed
// first, so it is named for what it is.
var AnswerPatterns = []Pattern{
	// Anthropic API keys: "sk-ant-api03-…", "sk-ant-admin01-…".
	{ID: "anthropic_key", Description: "Anthropic API key", Regex: `\bsk-ant-[A-Za-z0-9_-]{20,}`, Requires: []string{"sk-ant-"}},
	// OpenAI API keys: the project, service-account and admin keys
	// ("sk-proj-…", "sk-svcacct-…", "sk-admin-…", 40 or more key characters
	// after the kind) and legacy keys ("sk-" and 32 or more letters and
	// digits, no hyphen). Hyphenated words after "sk-" (CSS classes such as
	// "sk-fading-circle-container-big") are no key.
	{ID: "openai_key", Description: "OpenAI API key", Regex: `\bsk-(?:proj|svcacct|admin)-[A-Za-z0-9_-]{40,}|\bsk-[A-Za-z0-9]{32,}`, Requires: []string{"sk-"}},
	// AWS access key ids: "AKIA" and 16 upper-case letters or digits.
	{ID: "aws_access_key", Description: "AWS access key id", Regex: `\bAKIA[0-9A-Z]{16}\b`, Requires: []string{"AKIA"}},
	// GitHub tokens: classic (ghp_, gho_, ghu_, ghs_, ghr_) and fine-grained
	// (github_pat_).
	{ID: "github_token", Description: "GitHub token", Regex: `\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`, Requires: []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}},
	// Slack tokens: bot, app, user, refresh and legacy (xoxb-, xoxa-, xoxp-,
	// xoxr-, xoxs-).
	{ID: "slack_token", Description: "Slack token", Regex: `\bxox[baprs]-[A-Za-z0-9-]{10,}`, Requires: []string{"xoxb-", "xoxa-", "xoxp-", "xoxr-", "xoxs-"}},
	// Google API keys: "AIza" and 35 characters.
	{ID: "google_api_key", Description: "Google API key", Regex: `\bAIza[0-9A-Za-z_-]{35}`, Requires: []string{"AIza"}},
	// The header line of a PEM private key of any kind (RSA, EC, OPENSSH,
	// ENCRYPTED, PKCS#8).
	{ID: "private_key_pem", Description: "private key (PEM)", Regex: `-----BEGIN [A-Z ]*PRIVATE KEY-----`, Requires: []string{"PRIVATE KEY-----"}},
	// Burrow automation tokens: "bua_" and 32 random bytes in unpadded
	// standard base32 (52 characters of A-Z and 2-7), see
	// store.generateBearerToken.
	{ID: "burrow_automation_token", Description: "Burrow automation token", Regex: `\bbua_[A-Z2-7]{52}`, Requires: []string{"bua_"}},
	// Burrow gateway keys: "bgw_" and 32 random bytes in unpadded base64url
	// (43 characters), see store.newGatewayKey.
	{ID: "burrow_gateway_key", Description: "Burrow gateway key", Regex: `\bbgw_[A-Za-z0-9_-]{43}`, Requires: []string{"bgw_"}},
}
