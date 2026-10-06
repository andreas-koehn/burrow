package aimeter

import (
	"bytes"
	"encoding/json"
)

// anthropicParser handles Anthropic-shaped SSE streams. Frames are pairs of
// the form:
//
//	event: message_delta
//	data:  {"type":"message_delta","usage":{"input_tokens":12,"output_tokens":7}}
//
// followed by a blank separator line. The parser is line-buffered just like
// the OpenAI variant: each completed line is forwarded immediately, then
// inspected. We track the most recent "event:" name so that when a
// subsequent "data:" line arrives we know which envelope shape to expect.
//
// Anthropic streams put usage in TWO places per the SDK:
//   - message_start.message.usage carries an initial input_tokens count.
//   - message_delta.usage carries cumulative output_tokens (and the final
//     input_tokens, which equals the message_start value in most cases).
//
// We record both: each counter from whichever event carried a non-zero value
// last, output_tokens from message_delta.usage.
//
// The input side has three counters: input_tokens (not cached),
// cache_read_input_tokens (read from the prompt cache) and
// cache_creation_input_tokens (written to it). All three are tokens the
// client used and are counted as input tokens; the usage row has no separate
// columns for them and the price table no separate prices.
type anthropicParser struct {
	s         *Stream
	buf       bytes.Buffer
	lastEvent string // most recent "event: NAME" value
	// seen is the last non-zero value of each counter.
	seen anthropicUsage
}

func newAnthropicParser(s *Stream) *anthropicParser { return &anthropicParser{s: s} }

func (p *anthropicParser) write(b []byte) (forwarded int, err error) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			p.buf.Write(b)
			return forwarded, nil
		}
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

func (p *anthropicParser) close() error {
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

func (p *anthropicParser) inspect(line []byte) {
	trim := bytes.TrimRight(line, "\r\n")
	if len(trim) == 0 {
		return
	}
	switch {
	case bytes.HasPrefix(trim, []byte("event:")):
		p.lastEvent = string(bytes.TrimSpace(trim[len("event:"):]))
	case bytes.HasPrefix(trim, []byte("data:")):
		payload := bytes.TrimSpace(trim[len("data:"):])
		if len(payload) == 0 || payload[0] != '{' {
			return
		}
		switch p.lastEvent {
		case "message_start":
			var env struct {
				Message struct {
					Usage *anthropicUsage `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal(payload, &env); err != nil {
				return
			}
			if env.Message.Usage != nil {
				p.mergeUsage(env.Message.Usage)
			}
		case "message_delta":
			var env struct {
				Usage *anthropicUsage `json:"usage"`
			}
			if err := json.Unmarshal(payload, &env); err != nil {
				return
			}
			if env.Usage != nil {
				p.mergeUsage(env.Usage)
			}
		}
	}
}

// mergeUsage records token counts, preferring non-zero values from the
// most-recent envelope: a counter an envelope does not carry (output-only
// deltas, deltas without the cache counters) keeps its earlier value.
// Anthropic's message_delta.usage is the authoritative final count.
func (p *anthropicParser) mergeUsage(u *anthropicUsage) {
	keep := func(seen *int, v int) {
		if v != 0 {
			*seen = v
		}
	}
	keep(&p.seen.InputTokens, u.InputTokens)
	keep(&p.seen.CacheCreationInputTokens, u.CacheCreationInputTokens)
	keep(&p.seen.CacheReadInputTokens, u.CacheReadInputTokens)
	keep(&p.seen.OutputTokens, u.OutputTokens)
	p.s.recordTokens(p.seen.input(), p.seen.OutputTokens, 0)
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// input is every token of the request: not cached, read from the prompt
// cache, and written to it. A negative counter is not counted.
func (u anthropicUsage) input() int {
	return max(u.InputTokens, 0) + max(u.CacheCreationInputTokens, 0) + max(u.CacheReadInputTokens, 0)
}

// ParseAnthropicBody parses a fully-buffered non-streaming Anthropic
// /v1/messages response and returns its token counts; prompt-cache tokens
// count as input tokens. Returns the zero Tokens if the body is not valid
// JSON or has no usage object.
func ParseAnthropicBody(body []byte) Tokens {
	var env struct {
		Usage *anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return Tokens{}
	}
	in := env.Usage.input()
	return Tokens{
		In:    in,
		Out:   env.Usage.OutputTokens,
		Total: in + env.Usage.OutputTokens,
	}
}
