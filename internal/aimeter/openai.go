package aimeter

import (
	"bytes"
	"encoding/json"
	"math"
)

// openAIParser handles OpenAI-shaped SSE streams. Each frame is a single
// line of the form:
//
//	data: {"id":"…","choices":[{"delta":{"content":"x"}}]}
//
// followed by an empty line. The terminal frame is "data: [DONE]". When
// stream_options.include_usage=true, the *penultimate* frame carries a
// "usage" object (with choices:[]). The parser forwards each completed
// line plus its terminating "\n" immediately to the visitor writer, and
// only after forwarding does it speculatively JSON-parse the payload to
// look for a usage object.
type openAIParser struct {
	s     *Stream
	lines lineBuffer // the line being read
}

func newOpenAIParser(s *Stream) *openAIParser { return &openAIParser{s: s, lines: lineBuffer{s: s}} }

// write feeds bytes to the parser: each completed line is forwarded to the
// visitor and then read for usage (see lineBuffer).
func (p *openAIParser) write(b []byte) (forwarded int, err error) {
	return p.lines.write(b, p.inspect)
}

// close forwards any pending partial line as a final fragment.
func (p *openAIParser) close() error { return p.lines.close(p.inspect) }

// inspect speculatively parses a forwarded SSE line for an OpenAI usage
// object. Lines that are not "data: {…}" (e.g. blank separators, "data:
// [DONE]", comments) are ignored.
func (p *openAIParser) inspect(line []byte) {
	// Trim CR (some implementations use CRLF) and trailing LF.
	trim := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trim, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(trim[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if payload[0] != '{' {
		return
	}
	// Chat Completions puts usage at the top level of a chunk. The Responses
	// API puts it inside the response object of its closing event
	// ({"type":"response.completed","response":{"usage":{…}}}); earlier events
	// carry that object with no usage yet.
	var env struct {
		Usage    *openAIUsage `json:"usage"`
		Response *struct {
			Usage *openAIUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	usage := env.Usage
	if usage == nil && env.Response != nil {
		usage = env.Response.Usage
	}
	if usage == nil {
		return
	}
	p.s.recordTokens(usage.counts())
	// Cost and tokens come from the same chunk: a usage chunk without a
	// valid cost takes back what an earlier one reported.
	if usd, ok := reportedCost(usage.Cost); ok {
		p.s.recordCost(usd)
	} else {
		p.s.clearCost()
	}
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// The Responses API names the same two counts differently.
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// Cost is the amount the upstream charged for this request, in USD.
	// OpenRouter sets it; most upstreams do not. Kept raw so that a value of
	// the wrong type does not make the token counts fail to decode;
	// reportedCost validates it.
	Cost json.RawMessage `json:"cost"`
}

// counts returns the token counts of a usage object in either OpenAI spelling:
// Chat Completions (prompt/completion) or Responses (input/output). The chat
// spelling wins when both are present.
func (u *openAIUsage) counts() (in, out, total int) {
	in, out = u.PromptTokens, u.CompletionTokens
	if in == 0 && out == 0 {
		in, out = u.InputTokens, u.OutputTokens
	}
	total = u.TotalTokens
	if total == 0 {
		total = in + out
	}
	return in, out, total
}

// maxReportedCostUSD rejects figures that can only be a bug or an attack on
// the accounting (a single request never costs this much).
const maxReportedCostUSD = 10_000

// reportedCost validates an upstream-reported cost. The field is untrusted
// input: anything but a JSON number in [0, maxReportedCostUSD] is treated as
// "no cost reported", and the price table applies.
func reportedCost(raw json.RawMessage) (float64, bool) {
	// A JSON number starts with a digit or a minus sign. This also turns away
	// null, which Unmarshal would accept and leave at zero.
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0, false
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, false
	}
	if !saneCost(v) {
		return 0, false
	}
	if v == 0 {
		return 0, true // not -0
	}
	return v, true
}

// saneCost reports whether usd can be the cost of one request.
func saneCost(usd float64) bool {
	return !math.IsNaN(usd) && !math.IsInf(usd, 0) && usd >= 0 && usd <= maxReportedCostUSD
}

// ParseOpenAICost returns the cost a non-streamed OpenAI-style response
// reports in usage.cost, if any.
func ParseOpenAICost(body []byte) (float64, bool) {
	var env struct {
		Usage *struct {
			Cost json.RawMessage `json:"cost"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &env) != nil || env.Usage == nil {
		return 0, false
	}
	return reportedCost(env.Usage.Cost)
}

// ParseOpenAIBody parses a fully-buffered non-streaming OpenAI response (Chat
// Completions or Responses) and returns its token counts. Returns the zero
// Tokens if the body is not valid JSON or has no usage object.
func ParseOpenAIBody(body []byte) Tokens {
	var env struct {
		Usage *openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return Tokens{}
	}
	in, out, total := env.Usage.counts()
	return Tokens{In: in, Out: out, Total: total}
}
