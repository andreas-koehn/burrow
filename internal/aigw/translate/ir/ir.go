// Package ir is the neutral form the format translators meet in: one model
// call (Request), one complete answer (Response) and the steps of a streamed
// answer (Event). Each wire format (Chat Completions, Anthropic Messages,
// OpenAI Responses) has a codec that reads and writes this form; a pair is a
// decoder of one format composed with an encoder of another.
//
// # What a codec owes this form
//
// Nothing is dropped silently. A request decoder appends to Request.Dropped
// the name of everything it read and could not carry, and an encoder returns
// the names of what the neutral form holds and its format cannot express. A
// name is either fixed (the Dropped… constants here, or a field name the
// codec itself spells out, such as "metadata") or built with Unknown,
// DroppedTool or DroppedInput: a string the client chose never stands alone
// in the list, because "more" is reserved there and an empty string would
// vanish.
//
// Unknown fields are reported at four depths, without array indexes so the
// list stays short: the top level (Unknown("foo")), a message
// (Unknown("messages.foo")), a content part (Unknown("content.foo")) and a
// tool definition (Unknown("tools.foo")). Deeper than that a codec does not
// look: a tool's JSON Schema and a tool call's input are opaque and travel
// unchanged. A field that is not known is never forwarded to the target.
// Answers have no dropped list: unknown fields of an upstream answer are
// ignored.
//
// An image is carried or the request fails: an encoder whose format cannot
// take an image where it stands returns an error that names the field; it
// never leaves the image out.
//
// JSON that is only passed along (Tool.Schema, Part.Input) is kept as the
// bytes that arrived, never decoded and encoded again: key order, number
// formatting and large integers survive. Token counts and MaxTokens are
// integers.
//
// Hostile input is bounded by the Max… constants. A decoder returns an error
// (ErrLimit) when one is exceeded; it never truncates and never panics.
//
// Codecs are synchronous: a stream decoder turns the bytes it is fed into
// events and returns them, a stream encoder writes one event and returns.
// There is no goroutine and no queue between them, so the client's writer
// sets the pace.
package ir

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"
)

// Limits every codec applies to what it reads and writes.
const (
	// MaxMessages is the largest number of messages in one request.
	MaxMessages = 8192
	// MaxParts is the largest number of content parts in one message, in the
	// system prompt, or in one answer.
	MaxParts = 1024
	// MaxTools is the largest number of tool definitions in one request.
	MaxTools = 1024
	// MaxToolCalls is the largest number of tool calls in one assistant turn
	// or one answer.
	MaxToolCalls = 64
	// MaxToolArgsBytes is the largest JSON text of one tool call's input,
	// whole or added up over the chunks of a stream.
	MaxToolArgsBytes = 1 << 20
	// MaxTotalToolArgsBytes is the largest sum of the inputs of all tool
	// calls in one answer. A stream decoder keeps the arguments of the calls
	// that are open (to check them when they stop), so this is also what one
	// streamed answer may hold in memory.
	MaxTotalToolArgsBytes = 4 << 20
	// MaxDepth is the deepest nesting of objects and arrays a codec accepts
	// in a body, a stream frame, a tool's schema or a tool call's input.
	MaxDepth = 128
)

var (
	// ErrLimit reports input over one of the Max… limits.
	ErrLimit = errors.New("ir: limit exceeded")
	// ErrBadJSON reports pass-through JSON that is not one valid JSON object.
	ErrBadJSON = errors.New("ir: not a JSON object")
	// ErrStream reports an answer that is the provider's error: a stream
	// that ended with an Error event, or a body that holds an error object.
	// The error value is a *StreamError.
	ErrStream = errors.New("ir: the provider's answer is an error")
	// ErrSequence reports an event sequence that is not well formed.
	ErrSequence = errors.New("ir: malformed event sequence")
)

// StreamError is the provider's own error in place of an answer. Message is
// what the provider said, cut to a safe length: it is meant for the caller,
// in the caller's error shape, and for nothing else. Error() does not
// contain it: the text of a Go error ends up in logs, and what an upstream
// or a client wrote must not. errors.Is(err, ErrStream) holds.
type StreamError struct{ Message string }

func (e *StreamError) Error() string        { return ErrStream.Error() }
func (e *StreamError) Is(target error) bool { return target == ErrStream }

// BadRequestError is a client error a request decoder returns: the request
// is not one of its format, or holds something that cannot be translated and
// must not be left out. Format names the caller's format ("messages",
// "responses", "chat"), Field is the JSON path of what the caller sent
// ("messages[2].content[0]", "input[3].call_id"), Reason says what is wrong
// with it in fixed words. None of them holds content of the request. For a
// request over one of the limits errors.Is(err, ErrLimit) holds.
type BadRequestError struct {
	Format, Field, Reason string
	Limit                 bool
}

func (e *BadRequestError) Error() string { return e.Format + ": " + e.Field + ": " + e.Reason }

// Is reports ErrLimit for a request over a limit.
func (e *BadRequestError) Is(target error) bool { return e.Limit && target == ErrLimit }

// Fixed names for the dropped list (see the package comment for the rule).
const (
	DroppedCacheControl       = "cache_control"
	DroppedThinking           = "thinking" // thinking parts of the history, or the thinking setting
	DroppedThinkingSignature  = "thinking.signature"
	DroppedAnthropicBeta      = "anthropic-beta"
	DroppedTopK               = "top_k"
	DroppedPreviousResponseID = "previous_response_id"
	DroppedStore              = "store"
	DroppedSystemPosition     = "system.position" // see Request.System
)

// The fixed texts a request decoder writes into a conversation when a tool
// result cannot stand as the caller sent it. A tool result carries text only
// (see Part), and every tool call wants a result:
//   - a result that holds images keeps its text (ToolImageText when it has
//     none), and the images follow as Image parts of the same user message,
//     after all its ToolResult parts and after a Text part ToolImageNote(id).
//     Nothing is lost, so nothing is reported;
//   - a tool call the caller sent no result for gets a result with the text
//     ToolNoOutput (and the decoder reports it).
const (
	ToolImageText = "[image]"
	ToolNoOutput  = "[no output]"
)

// ContinueText is the user turn an encoder puts after a conversation that
// ends with the assistant, for a target that wants the user to speak last
// (and the encoder reports it).
const ContinueText = "[continue]"

// ToolImageNote is the text that stands before the images a tool call
// returned: "Image returned by tool call <id>:". The id is the caller's and
// the text is read by the model as the user's: only the characters
// [A-Za-z0-9_.:-] of the id are written, and at most 128 of them, so that
// an id cannot add lines or sentences to the conversation.
func ToolImageNote(toolID string) string {
	b := make([]byte, 0, len("Image returned by tool call :")+min(len(toolID), 128))
	b = append(b, "Image returned by tool call "...)
	for i, n := 0, 0; i < len(toolID) && n < 128; i++ {
		c := toolID[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == ':' || c == '-' {
			b = append(b, c)
			n++
		}
	}
	return string(append(b, ':'))
}

// Unknown names a field no codec knows: "unknown:<field>". The field is a
// top-level key, or "messages.<key>", "content.<key>", "tools.<key>".
func Unknown(field string) string { return "unknown:" + field }

// DroppedTool names a tool declaration that is not a plain function: "tool:<type>".
func DroppedTool(typ string) string { return "tool:" + typ }

// DroppedInput names a history item of a tool that is not emulated: "input:<type>".
func DroppedInput(typ string) string { return "input:" + typ }

// Request is a model call in neutral form.
type Request struct {
	// Model is the model name the caller sent. Every request decoder fills
	// it; encoders ignore it and write the target's model they are given.
	Model string
	// System is the system prompt: Text parts only, in order. Messages fills
	// it from "system", Responses from "instructions" and system/developer
	// items, Chat from system/developer messages. Empty means no system
	// prompt. All of them are gathered here, in order, so one that stood
	// later in the conversation loses its place: a decoder that hoists a
	// system message which came after the first message that is not one
	// reports DroppedSystemPosition.
	System []Part
	// Messages is the conversation, oldest first. A request has at least one.
	Messages []Message
	// Tools are the functions the model may call. Only plain functions with
	// a JSON Schema are carried; any other tool type is left out by the
	// decoder and reported with DroppedTool. Empty means no tools.
	Tools []Tool
	// ToolChoice says whether the model must call a tool. The zero value
	// means the caller did not say (the provider's default applies).
	ToolChoice ToolChoice
	// MaxTokens is the cap on generated tokens: "max_tokens" (Messages,
	// Chat), "max_completion_tokens" (Chat), "max_output_tokens"
	// (Responses). 0 means not set.
	MaxTokens int
	// Temperature and TopP are the sampling settings of all three formats.
	// nil means not set.
	Temperature *float64
	TopP        *float64
	// Stop lists the stop sequences: "stop_sequences" (Messages), "stop"
	// (Chat, a string or a list). Responses has none. Empty means not set.
	Stop []string
	// Stream says the caller asked for a streamed answer.
	Stream bool
	// Dropped holds the names of everything the decoder read and could not
	// carry. It may hold duplicates; Dropped (the function) tidies it.
	Dropped []string
}

// Role says who a message is from. There is no system role (see
// Request.System) and no tool role: a tool's output is a ToolResult part of
// a user message.
type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
)

// Message is one turn of the conversation.
type Message struct {
	Role Role
	// Parts is the content, in order. A user message holds Text, Image and
	// ToolResult parts; an assistant message holds Text, Thinking and
	// ToolUse parts. Any other combination is refused by the encoders.
	Parts []Part
}

// PartKind names what a Part is.
type PartKind string

const (
	Text       PartKind = "text"
	Image      PartKind = "image"       // user messages only
	ToolUse    PartKind = "tool_use"    // the assistant asks for a tool
	ToolResult PartKind = "tool_result" // the user returns a tool's output
	Thinking   PartKind = "thinking"    // the assistant's reasoning text
)

// Part is one piece of content. Only the fields of its Kind are set; the
// rest stay zero, so two parts can be compared with reflect.DeepEqual.
type Part struct {
	Kind PartKind

	// Text is the text of a Text or Thinking part, or the output of a
	// ToolResult (several text blocks are joined by "\n" by the decoder).
	// It is kept exactly: no trimming, no normalisation.
	Text string

	// MediaType and Data describe an Image. With a media type ("image/png")
	// Data is the base64 payload; with MediaType "" Data is a URL. A
	// ToolResult never carries an image: a decoder answers such a request
	// with a client error, an encoder refuses a ToolResult with Data set.
	MediaType string
	Data      string

	// ToolID is the id of a ToolUse and the id a ToolResult answers:
	// "id"/"tool_use_id" (Messages), "id"/"tool_call_id" (Chat), "call_id"
	// (Responses). ToolName is the function's name, on a ToolUse only.
	// Both are required on a ToolUse and unique within one turn.
	ToolID   string
	ToolName string
	// Input is the ToolUse's arguments: the text of one JSON object, as it
	// arrived ("{}" when the model sent none). See ToolInput.
	Input json.RawMessage

	// IsError marks a ToolResult as a failure ("is_error" in Messages). The
	// other formats have no such flag; their decoders leave it false.
	IsError bool
}

// Tool is a function the model may call.
type Tool struct {
	Name        string
	Description string // "" when the caller gave none
	// Schema is the JSON Schema of the arguments ("input_schema" in
	// Messages, "parameters" in Chat and Responses), passed through
	// unchanged. nil when the caller gave none.
	Schema json.RawMessage
}

// ToolChoiceMode says how free the model is to call tools.
type ToolChoiceMode string

const (
	ChoiceUnset    ToolChoiceMode = ""         // the caller did not say
	ChoiceAuto     ToolChoiceMode = "auto"     // the model decides
	ChoiceNone     ToolChoiceMode = "none"     // no tool call
	ChoiceRequired ToolChoiceMode = "required" // some tool ("any" in Messages)
	ChoiceTool     ToolChoiceMode = "tool"     // the tool called Name
)

// ToolChoice is the caller's tool_choice.
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string // for ChoiceTool only
}

// Response is a complete model answer in neutral form.
type Response struct {
	// ID and Model are the upstream's own; either may be "" (an encoder then
	// makes up an id and uses the model the caller asked for).
	ID    string
	Model string
	// Parts are Thinking, Text and ToolUse parts, in order. An answer may
	// have none (the model said nothing).
	Parts []Part
	Stop  StopReason
	Usage Usage
}

// StopReason says why the answer ended.
type StopReason string

const (
	StopUnknown   StopReason = ""              // the upstream gave none, or one no format knows
	StopEnd       StopReason = "end"           // the model was done
	StopMaxTokens StopReason = "max_tokens"    // the token cap was reached
	StopToolUse   StopReason = "tool_use"      // the model asks for tools
	StopSequence  StopReason = "stop_sequence" // a stop sequence matched (Messages tells; Chat reports StopEnd)
	StopRefusal   StopReason = "refusal"       // the provider withheld the answer
)

// Usage holds the upstream's token counts as whole numbers; 0 means not
// reported. InputTokens includes cached input.
type Usage struct{ InputTokens, OutputTokens int }

// Event is one step of a streamed answer. What a stream decoder emits, and
// what a caller-side stream encoder must be written for:
//
//   - A good answer is Start; then parts, each as PartStart, its deltas,
//     PartStop; then Finish. Parts are numbered from 0 in the order they
//     open, and every part that started is stopped exactly once.
//   - A Text or Thinking part is stopped before the next part of any kind
//     opens. ToolUse parts are different: they may be open side by side
//     until the finish, their argument deltas interleaved, and a Text or
//     Thinking part can open (and stop) while ToolUse parts are open. An
//     encoder for a format whose blocks are strictly one after the other
//     has to hold such parts back itself.
//   - Finish comes only when every part is stopped. Its stop reason is
//     StopToolUse only when a ToolUse part was opened.
//   - The ArgsJSON pieces of one ToolUse part add up to one JSON object, or
//     there are none: a decoder checks this when the part stops, before
//     the Finish, and fails the stream when it does not hold. The pieces of
//     one part add up to at most MaxToolArgsBytes, all parts together to at
//     most MaxTotalToolArgsBytes.
//   - Text and ArgsJSON are valid UTF-8; a character is never cut in two
//     between deltas.
//   - Failure, whatever the cause (the provider's error, a malformed or
//     oversized frame, an upstream that went away): a PartStop for every
//     part that is open, in part order, then exactly one Error, and nothing
//     after it. There is no Finish. An Error can be the only event, before
//     any Start.
type Event struct {
	Kind EventKind

	Index    int    // the part's number, for PartStart, the deltas and PartStop
	Part     Part   // PartStart: the part being opened (Text or Thinking with no text, ToolUse with id and name and no Input)
	Text     string // TextDelta, ThinkingDelta: the next piece of text
	ArgsJSON string // ToolArgsDelta: the next piece of the tool input's JSON text, never empty

	ID    string     // Start
	Model string     // Start
	Stop  StopReason // Finish
	Usage Usage      // Start (input tokens, when the format tells them early) and Finish (both; a 0 input count leaves Start's standing)
	Err   string     // Error: a message for the caller (the provider's, cut short, or a fixed text); never logged
}

// EventKind names what an Event is.
type EventKind string

const (
	Start         EventKind = "start"
	PartStart     EventKind = "part_start"
	TextDelta     EventKind = "text_delta"
	ThinkingDelta EventKind = "thinking_delta"
	ToolArgsDelta EventKind = "tool_args_delta"
	PartStop      EventKind = "part_stop"
	Finish        EventKind = "finish"
	Error         EventKind = "error"
)

// Dropped returns a sorted, de-duplicated copy of the dropped names, without
// empty strings; nil when nothing is left.
func Dropped(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != "" {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Depth returns the deepest nesting of objects and arrays in b. It reads b
// once and does not recurse, so it is safe on any input; for input that is
// not JSON the number means nothing more than "brackets outside strings".
func Depth(b []byte) int {
	depth, deepest := 0, 0
	inString, escaped := false, false
	for _, c := range b {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{' || c == '[':
			depth++
			if depth > deepest {
				deepest = depth
			}
		case (c == '}' || c == ']') && depth > 0:
			depth--
		}
	}
	return deepest
}

// CheckObject reports whether raw is exactly one valid JSON object, in UTF-8,
// nested no deeper than MaxDepth: nil, ErrBadJSON or ErrLimit. (encoding/json
// accepts bytes that are not UTF-8 inside a string and replaces them when it
// decodes; bytes that are passed along are not decoded, so they are refused
// instead of being handed to a target that would reject them.) An encoder calls it
// before it copies pass-through JSON into a body, so that a broken schema
// cannot break the body around it.
func CheckObject(raw []byte) error {
	if Depth(raw) > MaxDepth {
		return fmt.Errorf("%w: nesting deeper than %d", ErrLimit, MaxDepth)
	}
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(raw) || !utf8.Valid(raw) {
		return ErrBadJSON
	}
	return nil
}

// ToolInput turns the JSON text of a tool call's arguments into Part.Input:
// "{}" when the text is empty or null, the same bytes when it is one JSON
// object, otherwise an error (ErrBadJSON; ErrLimit for a text over
// MaxToolArgsBytes or nested deeper than MaxDepth). The result does not
// share memory with text.
func ToolInput(text []byte) (json.RawMessage, error) {
	if len(text) > MaxToolArgsBytes {
		return nil, fmt.Errorf("%w: tool arguments over %d bytes", ErrLimit, MaxToolArgsBytes)
	}
	trimmed := bytes.TrimSpace(text)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return json.RawMessage("{}"), nil
	}
	if err := CheckObject(text); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.Clone(text)), nil
}

const hexDigits = "0123456789abcdef"

// AppendString appends s to dst as a JSON string and returns the result. The
// encoders write bodies by hand with it, because encoding/json would rewrite
// pass-through JSON on the way (it compacts and HTML-escapes a RawMessage).
// Only what JSON requires is escaped: the quote, the backslash and control
// characters; "<", ">" and "&" stay as they are. A byte that is not UTF-8
// becomes U+FFFD, as with encoding/json.
func AppendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch c {
			case '"', '\\':
				dst = append(dst, '\\', c)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, `\ufffd`...)
			i++
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// Limits of what names a tool call. An upstream's answer over one of them is
// refused (ErrLimit) before the bytes are kept; a decoder of a caller's
// request cuts an id of replayed history instead (BoundToolID).
const (
	// MaxToolNameBytes is the longest name of a tool call in an answer.
	MaxToolNameBytes = 256
	// MaxToolIDBytes is the longest id of a tool call.
	MaxToolIDBytes = 4 << 10
)

// BoundToolID returns id as a tool call's id may be kept: unchanged when it
// is no longer than MaxToolIDBytes, else its beginning with a digest of the
// whole id in place of the rest, exactly MaxToolIDBytes long or a little
// less (a character is not cut in half). The same id always gives the same
// result, so a call and the result that answers it still pair, and two ids
// that differ only past the cut stay different. cut says the id was changed.
func BoundToolID(id string) (bounded string, cut bool) {
	if len(id) <= MaxToolIDBytes {
		return id, false
	}
	sum := sha256.Sum256([]byte(id))
	keep := MaxToolIDBytes - 17 // "_" and 16 hex digits
	for keep > 0 && !utf8.RuneStart(id[keep]) {
		keep--
	}
	return id[:keep] + "_" + hex.EncodeToString(sum[:8]), true
}
