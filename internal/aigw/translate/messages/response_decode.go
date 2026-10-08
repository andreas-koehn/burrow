package messages

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// MaxResponseBytes is the largest buffered answer DecodeResponse reads.
const MaxResponseBytes = 16 << 20

// maxErrorBytes is the longest provider error message that is shown.
const maxErrorBytes = 300

// ErrMalformed is wrapped by the errors of DecodeResponse and
// StreamDecoder.Feed for an upstream answer that is not a usable Messages
// answer: not JSON, a field of the wrong type, no content, a tool_use block
// without a name or an id, an input that is not a JSON object, an event out
// of its order. The provider is at fault, so the gateway answers 502. An
// answer over a limit is reported with ir.ErrLimit instead.
var ErrMalformed = errors.New("messages: the provider's answer is not a usable Messages answer")

func malformed(what string) error {
	return fmt.Errorf("%w: %s", ErrMalformed, what)
}

func answerOver(what string, limit int) error {
	return fmt.Errorf("%w: %s over %d", ir.ErrLimit, what, limit)
}

// jsonError turns an encoding/json error into one that carries no content
// of the answer: json's syntax errors quote the offending character, its
// type errors only name the field.
func jsonError(err error) error {
	if errors.Is(err, ir.ErrLimit) || errors.Is(err, ErrMalformed) {
		return err
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) && te.Field != "" {
		return malformed("field " + te.Field + " has the wrong type")
	}
	return malformed("not valid JSON")
}

// count is a whole number that is 0 or more, read without going through a
// float: token counts and block indexes. A quoted number and a number with
// an exponent or ".0" are taken too. Anything else (a negative number, a
// fraction, a number too large, another type) sets bad and never fails the
// decoding: the reader decides whether that matters.
type count struct {
	n   int
	set bool // a usable number was present
	bad bool // something was present that is no usable number
}

func (c *count) UnmarshalJSON(b []byte) error {
	*c = count{}
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	s := string(b)
	if b[0] == '"' {
		if json.Unmarshal(b, &s) != nil {
			c.bad = true
			return nil
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 || n > math.MaxInt {
			c.bad = true
			return nil
		}
		c.n, c.set = int(n), true
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || f > 1<<53 || f != math.Trunc(f) {
		c.bad = true
		return nil
	}
	c.n, c.set = int(f), true
	return nil
}

// wireUsage is Anthropic's usage object. The tokens written to and read
// from the prompt cache are input the caller's formats have no figure of
// their own for: all three add up to ir.Usage.InputTokens, which is what the
// gateway's meter counts as input for a Messages answer as well.
type wireUsage struct {
	Input         count `json:"input_tokens"`
	CacheCreation count `json:"cache_creation_input_tokens"`
	CacheRead     count `json:"cache_read_input_tokens"`
	Output        count `json:"output_tokens"`
}

// input returns the input tokens, cached ones included; told is false when
// the object names none of the three figures.
func (u *wireUsage) input() (n int, told bool) {
	for _, c := range []count{u.Input, u.CacheCreation, u.CacheRead} {
		if n > math.MaxInt-c.n {
			return math.MaxInt, true
		}
		n += c.n
		told = told || c.set
	}
	return n, told
}

type wireBlock struct {
	Type     string          `json:"type"`
	Text     *string         `json:"text"`     // a text block
	Thinking *string         `json:"thinking"` // a thinking block
	ID       string          `json:"id"`       // a tool_use block
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

type wireAnswer struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Model      string          `json:"model"`
	Content    json.RawMessage `json:"content"`
	StopReason *string         `json:"stop_reason"`
	Usage      *wireUsage      `json:"usage"`
	Error      json.RawMessage `json:"error"`
}

// decodeStop maps Anthropic's "stop_reason". "pause_turn" (a tool the
// provider runs paused the turn) ends the answer for a caller that knows no
// such thing; the context window that ran out cut the answer like the token
// cap does.
func decodeStop(reason string) ir.StopReason {
	switch reason {
	case "end_turn", "pause_turn":
		return ir.StopEnd
	case "max_tokens", "model_context_window_exceeded":
		return ir.StopMaxTokens
	case "tool_use":
		return ir.StopToolUse
	case "stop_sequence":
		return ir.StopSequence
	case "refusal":
		return ir.StopRefusal
	}
	return ir.StopUnknown
}

// settleStop corrects a stop reason by what the answer holds. tool_use
// without a tool call becomes end: a caller would wait for a call that is
// not there. A tool call makes an answer that says it simply ended (or says
// nothing) a tool_use: a caller acts on the stop reason. An answer that was
// cut off, withheld or stopped by a sequence stays what it is.
func settleStop(stop ir.StopReason, toolCalls bool) ir.StopReason {
	switch {
	case stop == ir.StopToolUse && !toolCalls:
		return ir.StopEnd
	case (stop == ir.StopEnd || stop == ir.StopUnknown) && toolCalls:
		return ir.StopToolUse
	}
	return stop
}

// blockInput reads the "input" of a tool_use block: nothing when it is
// absent, null or an empty object, else the bytes of the object as they are.
func blockInput(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	if len(trimmed) > ir.MaxToolArgsBytes {
		return nil, answerOver("tool call argument bytes", ir.MaxToolArgsBytes)
	}
	if err := ir.CheckObject(trimmed); errors.Is(err, ir.ErrLimit) {
		return nil, err
	} else if err != nil {
		return nil, malformed("a tool call's input is not a JSON object")
	}
	return trimmed, nil
}

// DecodeResponse reads a complete Messages answer. Its content blocks become
// parts in their order: text, tool_use (the "input" kept as the bytes the
// provider sent, "{}" when it sent none) and thinking (the text; a signature
// is not carried). redacted_thinking and block types nobody here knows
// (server_tool_use, web_search_tool_result, …) are passed over, and so are
// texts that are empty.
//
// A tool_use block needs a name and an id of its own and an input that is
// one JSON object; otherwise the whole answer is an error, because a client
// cannot act on a broken tool call. "stop_reason" is read by decodeStop and
// then follows what the answer holds (see settleStop).
//
// Usage: "output_tokens" is the output; "input_tokens",
// "cache_creation_input_tokens" and "cache_read_input_tokens" add up to the
// input. Counts that are missing or unreadable are 0.
//
// A body that is an error object ({"type":"error","error":{…}}, which some
// providers send with status 200) gives a *ir.StreamError with the
// provider's message in its field. Other errors wrap ErrMalformed, or
// ir.ErrLimit for an answer over MaxResponseBytes, ir.MaxDepth, ir.MaxParts,
// ir.MaxToolCalls, ir.MaxToolArgsBytes or ir.MaxTotalToolArgsBytes. No
// error's text holds content of the answer; the value returned with an error
// is empty.
func DecodeResponse(body []byte) (ir.Response, error) {
	if len(body) > MaxResponseBytes {
		return ir.Response{}, answerOver("answer bytes", MaxResponseBytes)
	}
	if ir.Depth(body) > ir.MaxDepth {
		return ir.Response{}, answerOver("answer nesting", ir.MaxDepth)
	}
	var a wireAnswer
	if err := json.Unmarshal(body, &a); err != nil {
		return ir.Response{}, jsonError(err)
	}
	if a.Type == "error" || !isNull(a.Error) {
		return ir.Response{}, &ir.StreamError{Message: errorMessage(a.Error)}
	}
	if a.Type != "" && a.Type != "message" {
		return ir.Response{}, malformed("not a message")
	}
	if isNull(a.Content) {
		return ir.Response{}, malformed("no content")
	}
	resp := ir.Response{ID: a.ID, Model: a.Model}
	if a.Usage != nil {
		resp.Usage.InputTokens, _ = a.Usage.input()
		resp.Usage.OutputTokens = a.Usage.Output.n
	}
	dec := json.NewDecoder(bytes.NewReader(a.Content))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return ir.Response{}, malformed("content is not a list")
	}
	ids := map[string]bool{}
	blocks, argBytes := 0, 0
	for dec.More() {
		// The list is read one block at a time: one over the limit is
		// refused before it is held in memory.
		if blocks++; blocks > ir.MaxParts {
			return ir.Response{}, answerOver("content blocks", ir.MaxParts)
		}
		var b wireBlock
		if err := dec.Decode(&b); err != nil {
			return ir.Response{}, jsonError(err)
		}
		switch b.Type {
		case "text":
			if b.Text != nil && *b.Text != "" {
				resp.Parts = append(resp.Parts, ir.Part{Kind: ir.Text, Text: *b.Text})
			}
		case "thinking":
			if b.Thinking != nil && *b.Thinking != "" {
				resp.Parts = append(resp.Parts, ir.Part{Kind: ir.Thinking, Text: *b.Thinking})
			}
		case "tool_use":
			if b.Name == "" {
				return ir.Response{}, malformed("a tool call without a name")
			}
			if b.ID == "" || ids[b.ID] {
				return ir.Response{}, malformed("a tool call without an id of its own")
			}
			if len(ids) >= ir.MaxToolCalls {
				return ir.Response{}, answerOver("tool calls", ir.MaxToolCalls)
			}
			ids[b.ID] = true
			input, err := blockInput(b.Input)
			if err != nil {
				return ir.Response{}, err
			}
			if input == nil {
				input = []byte("{}")
			}
			if argBytes += len(input); argBytes > ir.MaxTotalToolArgsBytes {
				return ir.Response{}, answerOver("tool call argument bytes in all", ir.MaxTotalToolArgsBytes)
			}
			resp.Parts = append(resp.Parts, ir.Part{Kind: ir.ToolUse, ToolID: b.ID, ToolName: b.Name, Input: json.RawMessage(bytes.Clone(input))})
		}
	}
	stop := ir.StopUnknown
	if a.StopReason != nil {
		stop = decodeStop(*a.StopReason)
	}
	resp.Stop = settleStop(stop, len(ids) > 0)
	return resp, nil
}

// errorMessage returns what a provider's error object says (see errorText),
// or a fixed text when it says nothing.
func errorMessage(raw json.RawMessage) string {
	if s := errorText(raw); s != "" {
		return s
	}
	return errProvider
}

// errorText returns the "message" of an error object, or the error itself
// when it is a string, cut to maxErrorBytes at a character boundary; "" when
// it says nothing.
func errorText(raw json.RawMessage) string {
	s, ok := asString(raw)
	if !ok {
		var obj struct {
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(raw, &obj) == nil {
			s, _ = asString(obj.Message)
		}
	}
	return cutMessage(s)
}

func cutMessage(s string) string {
	if len(s) > maxErrorBytes {
		n := maxErrorBytes
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return s
}

// DecodeError returns what the body of an error answer (a status that is no
// success) says: the "message" of its "error" object, the error itself when
// it is a string, or a top-level "message"; cut to 300 bytes at a character
// boundary. It returns "" when the body is not JSON or says nothing. The
// text is the provider's own: it is meant for the caller and for nothing
// else, like StreamError.Message.
func DecodeError(body []byte) string {
	var obj struct {
		Error   json.RawMessage `json:"error"`
		Message json.RawMessage `json:"message"`
	}
	if ir.Depth(body) > ir.MaxDepth || json.Unmarshal(body, &obj) != nil {
		return ""
	}
	if !isNull(obj.Error) {
		if s := errorText(obj.Error); s != "" {
			return s
		}
	}
	s, _ := asString(obj.Message)
	return cutMessage(s)
}
