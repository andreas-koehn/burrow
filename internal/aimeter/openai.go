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
	s   *Stream
	buf bytes.Buffer // pending partial line
}

func newOpenAIParser(s *Stream) *openAIParser { return &openAIParser{s: s} }

// write feeds bytes to the parser. It splits on '\n', forwards each
// completed line (including the trailing '\n') to the visitor writer, and
// flushes after each line. Partial trailing data is kept in buf.
//
// The returned forwarded count is the number of bytes successfully written
// to the visitor (so Stream.bytesOut stays accurate even on short writes).
func (p *openAIParser) write(b []byte) (forwarded int, err error) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			// no newline yet — buffer and wait
			p.buf.Write(b)
			return forwarded, nil
		}
		// Forward the buffered prefix + the segment up to and including '\n'.
		var frame []byte
		if p.buf.Len() > 0 {
			p.buf.Write(b[:i+1])
			frame = p.buf.Bytes()
		} else {
			frame = b[:i+1]
		}
		n, werr := p.s.w.Write(frame)
		forwarded += n
		if werr != nil {
			return forwarded, werr
		}
		p.s.flush()
		p.inspect(frame)
		p.buf.Reset()
		b = b[i+1:]
	}
	return forwarded, nil
}

// close forwards any pending partial line as a final fragment.
func (p *openAIParser) close() error {
	if p.buf.Len() == 0 {
		return nil
	}
	n, err := p.s.w.Write(p.buf.Bytes())
	p.s.bytesOut += int64(n)
	p.s.flush()
	p.inspect(p.buf.Bytes())
	p.buf.Reset()
	return err
}

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
	var env struct {
		Usage *openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	if env.Usage == nil {
		return
	}
	p.s.recordTokens(env.Usage.PromptTokens, env.Usage.CompletionTokens, env.Usage.TotalTokens)
	if usd, ok := reportedCost(env.Usage.Cost); ok {
		p.s.recordCost(usd)
	}
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Cost is the amount the upstream charged for this request, in USD.
	// OpenRouter sets it; most upstreams do not. Kept raw so that a value of
	// the wrong type does not make the token counts fail to decode;
	// reportedCost validates it.
	Cost json.RawMessage `json:"cost"`
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

// ParseOpenAIBody parses a fully-buffered non-streaming OpenAI chat
// completion response and returns its token counts. Returns the zero Tokens
// if the body is not valid JSON or has no usage object.
func ParseOpenAIBody(body []byte) Tokens {
	var env struct {
		Usage *openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return Tokens{}
	}
	total := env.Usage.TotalTokens
	if total == 0 {
		total = env.Usage.PromptTokens + env.Usage.CompletionTokens
	}
	return Tokens{
		In:    env.Usage.PromptTokens,
		Out:   env.Usage.CompletionTokens,
		Total: total,
	}
}
