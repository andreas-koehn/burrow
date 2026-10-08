package chat

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

// maxChoices is the largest number of choices an answer or a chunk may list.
// Only the first is read; the bound keeps a list of millions of empty
// objects from being decoded into memory many times its size.
const maxChoices = 64

// ErrMalformed is wrapped by the errors of DecodeResponse and
// StreamDecoder.Feed for an upstream answer that is not a usable Chat
// Completions answer: not JSON, a field of the wrong type, no choice, a tool
// call without a name or an id, tool arguments that are not a JSON object.
// The provider is at fault, so the gateway answers 502. An answer over a
// limit is reported with ir.ErrLimit instead.
var ErrMalformed = errors.New("chat: the provider's answer is not a usable Chat Completions answer")

func malformed(what string) error {
	return fmt.Errorf("%w: %s", ErrMalformed, what)
}

func overLimit(what string, limit int) error {
	return fmt.Errorf("%w: %s over %d", ir.ErrLimit, what, limit)
}

// jsonError turns an encoding/json error into one that carries no content
// of the answer: json's syntax errors quote the offending character, its
// type errors only name the field.
func jsonError(err error) error {
	if errors.Is(err, ir.ErrLimit) || errors.Is(err, ErrMalformed) {
		return err // from one of the bounded lists below
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) && te.Field != "" {
		return malformed("field " + te.Field + " has the wrong type")
	}
	return malformed("not valid JSON")
}

// count is a whole number that is 0 or more, read without going through a
// float: token counts and indexes. Providers are sloppy with these, so a
// quoted number and a number with an exponent or ".0" are taken too.
// Anything else (a negative number, a fraction, a number too large, another
// type) sets bad and never fails the decoding: the reader decides whether
// that matters.
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
	// 1e9, 12.0: exact up to 2^53.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || f > 1<<53 || f != math.Trunc(f) {
		c.bad = true
		return nil
	}
	c.n, c.set = int(f), true
	return nil
}

// decodeList decodes a JSON list of at most limit elements, one element at
// a time: a list over the limit is refused before it is held in memory.
// (Decoding into a plain slice first would turn a few megabytes of "{},{},…"
// into hundreds of megabytes of empty structs.) null gives nil.
func decodeList[T any](raw []byte, limit int, what string) ([]T, error) {
	if isNull(raw) {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, malformed(what + " is not a list")
	}
	var out []T
	for dec.More() {
		if len(out) >= limit {
			return nil, overLimit(what, limit)
		}
		var v T
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

type choiceList []wireChoice

func (l *choiceList) UnmarshalJSON(b []byte) (err error) {
	*l, err = decodeList[wireChoice](b, maxChoices, "choices")
	return err
}

type toolCallList []wireToolCall

func (l *toolCallList) UnmarshalJSON(b []byte) (err error) {
	*l, err = decodeList[wireToolCall](b, ir.MaxToolCalls, "tool calls")
	return err
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// The wire types name only what is read; every other field of an answer is
// ignored. A field declared as string takes a string or null, anything else
// is a type error; fields that providers fill with different types are raw.
type wireAnswer struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Choices choiceList      `json:"choices"`
	Usage   *wireUsage      `json:"usage"`
	Error   json.RawMessage `json:"error"`
}

type wireChoice struct {
	Index        count        `json:"index"`
	Message      *wireMessage `json:"message"` // a complete answer
	Delta        *wireMessage `json:"delta"`   // a stream chunk
	FinishReason string       `json:"finish_reason"`
}

type wireMessage struct {
	Content          json.RawMessage `json:"content"`           // a string; in a complete answer also a list of text parts
	ReasoningContent json.RawMessage `json:"reasoning_content"` // a string where the provider sends its reasoning
	Reasoning        json.RawMessage `json:"reasoning"`         // the same under another name; an object elsewhere
	Refusal          json.RawMessage `json:"refusal"`           // a string
	FunctionCall     json.RawMessage `json:"function_call"`     // the legacy form of a tool call: refused, see DecodeResponse
	ToolCalls        toolCallList    `json:"tool_calls"`
}

type wireToolCall struct {
	Index    count         `json:"index"` // streams only
	ID       string        `json:"id"`
	Function *wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // a string holding JSON text; an object with some providers
}

type wireUsage struct {
	Prompt     count `json:"prompt_tokens"`
	Completion count `json:"completion_tokens"`
}

func (u *wireUsage) usage() ir.Usage {
	return ir.Usage{InputTokens: u.Prompt.n, OutputTokens: u.Completion.n}
}

func isNull(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || string(raw) == "null"
}

// rawString reads a field that should be a string: its value, with ok false
// when it is present and something else. Absent and null give "".
func rawString(raw json.RawMessage) (s string, ok bool) {
	if isNull(raw) {
		return "", true
	}
	if bytes.TrimSpace(raw)[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// reasoningText returns the reasoning text of a message or delta: the first
// of "reasoning_content" and "reasoning" that is a non-empty string.
func (m *wireMessage) reasoningText() string {
	if s, _ := rawString(m.ReasoningContent); s != "" {
		return s
	}
	s, _ := rawString(m.Reasoning)
	return s
}

// argumentsText returns the JSON text of a complete tool call's arguments:
// the string's content, or the bytes of an object a provider sent in its
// place. Arguments are passed on or refused, never repaired: bytes that are
// not UTF-8 (a lone continuation byte, a surrogate escape without its
// partner) are an error here, where encoding/json would put U+FFFD in their
// place. The text "null" is refused as it is in a stream; JSON null, an empty
// string and white space are "no arguments".
func argumentsText(raw json.RawMessage) (string, error) {
	b, err := argumentsBytes(raw)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", malformed("tool call arguments are not valid UTF-8")
	}
	if string(bytes.TrimSpace(b)) == "null" {
		return "", malformed("tool call arguments are not a JSON object")
	}
	return string(b), nil
}

// toolInput is ir.ToolInput with this package's error for broken JSON.
func toolInput(text string) (json.RawMessage, error) {
	input, err := ir.ToolInput([]byte(text))
	if errors.Is(err, ir.ErrBadJSON) {
		return nil, malformed("tool call arguments are not a JSON object")
	}
	return input, err
}

func stopReason(finish string) ir.StopReason {
	switch finish {
	case "stop":
		return ir.StopEnd
	case "length":
		return ir.StopMaxTokens
	case "tool_calls", "function_call":
		return ir.StopToolUse
	case "content_filter":
		return ir.StopRefusal
	}
	return ir.StopUnknown
}

// settleStop corrects a stop reason by what the answer holds. tool_use
// without a tool call becomes end: a caller would wait for a call that is
// not there. A refusal text makes it a refusal. A tool call makes it
// tool_use: several providers say "stop" while they ask for tools, and a
// caller acts on the stop reason. An answer that was cut off (max_tokens)
// or withheld stays what it is.
func settleStop(stop ir.StopReason, refused, toolCalls bool) ir.StopReason {
	if stop == ir.StopToolUse && !toolCalls {
		stop = ir.StopEnd
	}
	if stop != ir.StopEnd && stop != ir.StopUnknown {
		return stop
	}
	switch {
	case refused:
		return ir.StopRefusal
	case toolCalls:
		return ir.StopToolUse
	}
	return stop
}

// firstChoice returns the choice with index 0 (or without an index): the
// only one that is read.
func firstChoice(choices []wireChoice) (*wireChoice, error) {
	for i := range choices {
		if choices[i].Index.bad {
			return nil, malformed("a choice's index is not a number")
		}
		if choices[i].Index.n == 0 {
			return &choices[i], nil
		}
	}
	return nil, nil
}

// DecodeResponse reads a complete Chat Completions answer. Only the first
// choice is read. Parts come in the order thinking, text, tool calls: the
// reasoning text ("reasoning_content" or "reasoning"), the content (a string
// or a list of text parts) followed by a refusal text, one ToolUse per tool
// call. Empty texts make no part.
//
// A tool call's arguments are kept as the bytes the provider sent ("{}" when
// it sent none); when they are not one JSON object the whole answer is an
// error, because a client cannot act on a broken tool call. A tool call
// needs a name and an id of its own for the same reason. Token counts that
// are missing or unreadable are 0.
//
// The legacy "function_call" of a message is not read as a tool call: it has
// no id, and the "function" message that would have to answer it is not
// something EncodeRequest writes. An answer that holds one is an error. A
// stop reason that announces tools ("tool_calls", "function_call") on an
// answer without a tool call is read as the end of the answer.
//
// A body that holds an "error" object (some providers send one with status
// 200) gives a *ir.StreamError with the provider's message in its field.
// Other errors wrap ErrMalformed, or ir.ErrLimit for an answer over
// MaxResponseBytes, ir.MaxDepth, ir.MaxParts, ir.MaxToolCalls,
// ir.MaxToolArgsBytes or ir.MaxTotalToolArgsBytes. No error's text holds
// content of the answer; the value returned with an error is empty.
func DecodeResponse(body []byte) (ir.Response, error) {
	if len(body) > MaxResponseBytes {
		return ir.Response{}, overLimit("answer bytes", MaxResponseBytes)
	}
	if ir.Depth(body) > ir.MaxDepth {
		return ir.Response{}, overLimit("answer nesting", ir.MaxDepth)
	}
	var a wireAnswer
	if err := json.Unmarshal(body, &a); err != nil {
		return ir.Response{}, jsonError(err)
	}
	if !isNull(a.Error) {
		return ir.Response{}, &ir.StreamError{Message: errorMessage(a.Error)}
	}
	choice, err := firstChoice(a.Choices)
	if err != nil {
		return ir.Response{}, err
	}
	if choice == nil || choice.Message == nil {
		return ir.Response{}, malformed("no first choice with a message")
	}
	msg := choice.Message
	if !isNull(msg.FunctionCall) {
		return ir.Response{}, malformed("a legacy function_call")
	}
	refusal, ok := rawString(msg.Refusal)
	if !ok {
		return ir.Response{}, malformed("refusal is not a string")
	}
	resp := ir.Response{ID: a.ID, Model: a.Model}
	if a.Usage != nil {
		resp.Usage = a.Usage.usage()
	}
	if s := msg.reasoningText(); s != "" {
		resp.Parts = append(resp.Parts, ir.Part{Kind: ir.Thinking, Text: s})
	}
	content, err := contentText(msg.Content)
	if err != nil {
		return ir.Response{}, err
	}
	if s := content + refusal; s != "" {
		resp.Parts = append(resp.Parts, ir.Part{Kind: ir.Text, Text: s})
	}
	ids := make(map[string]bool, len(msg.ToolCalls))
	argBytes := 0
	for _, tc := range msg.ToolCalls {
		if tc.Function == nil || tc.Function.Name == "" {
			return ir.Response{}, malformed("a tool call without a name")
		}
		if tc.ID == "" || ids[tc.ID] {
			return ir.Response{}, malformed("a tool call without an id of its own")
		}
		if len(tc.ID) > ir.MaxToolIDBytes {
			return ir.Response{}, overLimit("tool call id bytes", ir.MaxToolIDBytes)
		}
		if len(tc.Function.Name) > ir.MaxToolNameBytes {
			return ir.Response{}, overLimit("tool call name bytes", ir.MaxToolNameBytes)
		}
		ids[tc.ID] = true
		text, err := argumentsText(tc.Function.Arguments)
		if err != nil {
			return ir.Response{}, err
		}
		input, err := toolInput(text)
		if err != nil {
			return ir.Response{}, err
		}
		if argBytes += len(input); argBytes > ir.MaxTotalToolArgsBytes {
			return ir.Response{}, overLimit("tool call argument bytes in all", ir.MaxTotalToolArgsBytes)
		}
		resp.Parts = append(resp.Parts, ir.Part{Kind: ir.ToolUse, ToolID: tc.ID, ToolName: tc.Function.Name, Input: input})
	}
	resp.Stop = settleStop(stopReason(choice.FinishReason), refusal != "", len(msg.ToolCalls) > 0)
	return resp, nil
}

// contentText reads a complete message's content: a string, null, or a list
// of text parts, which are joined as they are.
func contentText(raw json.RawMessage) (string, error) {
	if s, ok := rawString(raw); ok {
		return s, nil
	}
	parts, err := decodeList[textPart](raw, ir.MaxParts, "content")
	if errors.Is(err, ir.ErrLimit) {
		return "", err
	}
	if err != nil {
		return "", malformed("content is neither a string nor a list of text parts")
	}
	var b []byte
	for _, p := range parts {
		if p.Type != "text" {
			return "", malformed("a content part that is not text")
		}
		b = append(b, p.Text...)
	}
	return string(b), nil
}
