package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

const (
	// MaxFrameBytes is the largest stream frame Feed takes, and the limit to
	// give sse.NewParser for a Chat Completions stream. One frame can hold a
	// whole tool call (ir.MaxToolArgsBytes of arguments, escaped once more).
	MaxFrameBytes = 4 << 20
	// maxErrorBytes is the longest provider error message that is shown.
	maxErrorBytes = 300
)

// The texts of the Error events a decoder makes up itself. They are fixed:
// nothing of the answer is in them.
const (
	errEarlyEnd   = "the provider ended the stream early"
	errProvider   = "the provider reported an error"
	errUnreadable = "the provider sent an answer that cannot be read"
	errTooLarge   = "the provider's answer is too large"
)

// StreamDecoder turns chat.completion.chunk frames into neutral events.
//
// It is driven by its caller: Feed takes the data of one frame and returns
// the events it produces, Close ends the answer. It starts no goroutine and
// queues nothing; the caller writes the events out before it feeds again,
// so a slow client slows the upstream read. A decoder that is dropped
// half-way needs no cleanup. It is not safe for use by several goroutines.
//
// What it holds is bounded: one state per tool call (at most
// ir.MaxToolCalls) with the arguments seen so far, which are checked when
// the call stops (at most ir.MaxToolArgsBytes each and
// ir.MaxTotalToolArgsBytes together), up to three bytes of a character that
// was cut between two chunks, and counters. Text passes through and is not
// kept.
type StreamDecoder struct {
	out []ir.Event // the events of the Feed or Close that is running

	started   bool // Start was emitted
	done      bool // Finish or Error was emitted
	sawFinish bool // a finish_reason arrived
	refused   bool // a refusal text arrived

	open     int         // the open Text or Thinking part, or -1
	openKind ir.PartKind // its kind, or the kind carry belongs to
	carry    []byte      // the first bytes of a character whose rest has not arrived
	next     int         // the next part's number

	calls     []*toolCall          // in the order they first appeared
	byIndex   map[int]*toolCall    // by the chunk's "index"; a map, so an absurd index costs nothing
	byID      map[string]*toolCall // by id
	opened    bool                 // a tool call part was opened
	argsBytes int                  // argument bytes of all calls

	stop  ir.StopReason
	usage ir.Usage
}

type toolCall struct {
	id, name string
	part     int    // its part number, or -1 while it is not open
	args     []byte // the arguments so far, without white space before them
	carry    []byte // the first bytes of a character whose rest has not arrived
	closed   bool
}

// NewStreamDecoder returns a decoder for one streamed answer.
func NewStreamDecoder() *StreamDecoder {
	return &StreamDecoder{open: -1, byIndex: map[int]*toolCall{}, byID: map[string]*toolCall{}}
}

// Feed takes the data of one SSE frame ("[DONE]" included) and returns the
// events it produces; empty data produces none. The events follow the rules
// written down at ir.Event.
//
//   - The first chunk with an id, a model or a choice gives Start.
//   - Reasoning text ("reasoning_content" or "reasoning") and content text
//     each open a part when none of their kind is open, stopping the other
//     kind first; empty text opens nothing. A refusal text is content.
//   - A tool call is addressed by its "index" (or, with providers that send
//     none or number every call 0, by its id; without either a delta
//     belongs to the latest call). Its part opens as soon as id and name
//     are known, stopping an open text part first; arguments that came
//     earlier follow as one delta. Calls may interleave. White space before
//     a call's arguments is not passed on.
//   - A character cut in two between chunks (as bytes, or as the two halves
//     of an escaped surrogate pair) is put together again: the first half
//     waits for the next delta of the same part.
//   - A finish_reason stops every open part, in part order. Before that
//     each tool call is checked: it has a name and an id, and its arguments
//     are one JSON object or empty.
//   - "[DONE]" gives Finish with the stop reason and the usage of the last
//     chunk that had one.
//
// The answer fails — a PartStop for every open part, in part order, then
// one Error, and the decoder is finished — when
//   - a chunk holds an {"error": …} object: the Error carries its message,
//     cut to 300 bytes;
//   - "[DONE]" comes without a finish_reason before it (the model never
//     said it was done): "the provider ended the stream early";
//   - the frame or the answer is malformed (not JSON, a field of the wrong
//     type, a tool call without name or id or with arguments that are no
//     JSON object or hold bytes that are not UTF-8 (text is repaired with
//     U+FFFD, arguments never are), a character whose second half never
//     came, the legacy "function_call") or over a limit (MaxFrameBytes, ir.MaxDepth,
//     ir.MaxParts, ir.MaxToolCalls, ir.MaxToolArgsBytes,
//     ir.MaxTotalToolArgsBytes): the Error carries a fixed text.
//
// Only in the last case Feed returns an error as well, wrapping
// ErrMalformed or ir.ErrLimit, so that the caller can tell what happened.
// The events returned with it already end the answer: the caller writes
// them and adds no Error of its own. After the answer has ended, Feed and
// Close return nothing.
func (d *StreamDecoder) Feed(data []byte) ([]ir.Event, error) {
	if d.done {
		return nil, nil
	}
	err := d.feed(data)
	if err != nil {
		d.fail(failureText(err))
	}
	out := d.out
	d.out = nil
	return out, err
}

// Close ends the answer when the upstream stopped: nothing after a Finish or
// an Error; Finish when a finish_reason had arrived and only "[DONE]" was
// missing (several servers leave it out); otherwise a PartStop for every
// open part followed by Error "the provider ended the stream early" — just
// that Error when no chunk was ever seen.
func (d *StreamDecoder) Close() []ir.Event {
	if d.done {
		return nil
	}
	if !d.sawFinish {
		d.fail(errEarlyEnd)
	} else if err := d.closeParts(); err != nil {
		// What came after the finish_reason is broken: text cut inside a
		// character, or a late tool call that does not hold.
		d.fail(failureText(err))
	} else {
		d.finish()
	}
	out := d.out
	d.out = nil
	return out
}

// failureText picks the Error event's text for an answer that failed with err.
func failureText(err error) string {
	if errors.Is(err, ir.ErrLimit) {
		return errTooLarge
	}
	return errUnreadable
}

func (d *StreamDecoder) emit(ev ir.Event) { d.out = append(d.out, ev) }

// end marks the answer ended and lets go of what the decoder held.
func (d *StreamDecoder) end() {
	d.done = true
	d.calls, d.byIndex, d.byID, d.carry = nil, nil, nil, nil
}

// fail ends the answer with an error: a PartStop for every open part, in
// part order, then the one Error.
func (d *StreamDecoder) fail(message string) {
	var open []int
	if d.open >= 0 {
		open = append(open, d.open)
	}
	for _, c := range d.calls {
		if c.part >= 0 && !c.closed {
			open = append(open, c.part)
		}
	}
	sort.Ints(open)
	for _, n := range open {
		d.emit(ir.Event{Kind: ir.PartStop, Index: n})
	}
	d.emit(ir.Event{Kind: ir.Error, Err: message})
	d.end()
}

func (d *StreamDecoder) finish() {
	d.emit(ir.Event{Kind: ir.Finish, Stop: settleStop(d.stop, d.refused, d.opened), Usage: d.usage})
	d.end()
}

func (d *StreamDecoder) feed(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	if len(data) > MaxFrameBytes {
		return overLimit("stream frame bytes", MaxFrameBytes)
	}
	if string(data) == "[DONE]" {
		if !d.sawFinish {
			d.fail(errEarlyEnd)
			return nil
		}
		if err := d.closeParts(); err != nil {
			return err
		}
		d.finish()
		return nil
	}
	if ir.Depth(data) > ir.MaxDepth {
		return overLimit("stream frame nesting", ir.MaxDepth)
	}
	var c wireAnswer
	if err := json.Unmarshal(data, &c); err != nil {
		return jsonError(err)
	}
	if !isNull(c.Error) {
		d.fail(errorMessage(c.Error))
		return nil
	}
	choice, err := firstChoice(c.Choices)
	if err != nil {
		return err
	}
	if !d.started && (c.ID != "" || c.Model != "" || choice != nil) {
		d.started = true
		d.emit(ir.Event{Kind: ir.Start, ID: c.ID, Model: c.Model})
	}
	if c.Usage != nil {
		d.usage = c.Usage.usage()
	}
	if choice == nil {
		return nil
	}
	if delta := choice.Delta; delta != nil {
		if err := d.delta(delta); err != nil {
			return err
		}
	}
	if choice.FinishReason != "" {
		d.sawFinish = true
		d.stop = stopReason(choice.FinishReason)
		return d.closeParts()
	}
	return nil
}

func (d *StreamDecoder) delta(delta *wireMessage) error {
	if !isNull(delta.FunctionCall) {
		return malformed("a legacy function_call")
	}
	// Reasoning is read leniently, as in a complete answer: with some
	// providers "reasoning" is an object, which is no text.
	reasoning, ok := rawBytes(delta.ReasoningContent)
	if !ok || len(reasoning) == 0 {
		reasoning, _ = rawBytes(delta.Reasoning)
	}
	if err := d.text(ir.Thinking, reasoning); err != nil {
		return err
	}
	content, ok := rawBytes(delta.Content)
	if !ok {
		return malformed("a delta's content is not a string")
	}
	if err := d.text(ir.Text, content); err != nil {
		return err
	}
	refusal, ok := rawBytes(delta.Refusal)
	if !ok {
		return malformed("a delta's refusal is not a string")
	}
	if len(refusal) > 0 {
		d.refused = true
		if err := d.text(ir.Text, refusal); err != nil {
			return err
		}
	}
	for i := range delta.ToolCalls {
		if err := d.toolCall(&delta.ToolCalls[i]); err != nil {
			return err
		}
	}
	return nil
}

// newPart numbers the next part.
func (d *StreamDecoder) newPart() (int, error) {
	if d.next >= ir.MaxParts {
		return 0, overLimit("parts", ir.MaxParts)
	}
	d.next++
	return d.next - 1, nil
}

// closeText stops the open Text or Thinking part. A character whose second
// half never came is an error; the part is then left for fail to stop.
func (d *StreamDecoder) closeText() error {
	if len(d.carry) > 0 {
		d.carry = nil
		return malformed("text ends inside a character")
	}
	if d.open >= 0 {
		d.emit(ir.Event{Kind: ir.PartStop, Index: d.open})
		d.open = -1
	}
	return nil
}

// text emits a text or thinking delta, opening its part when needed.
func (d *StreamDecoder) text(kind ir.PartKind, b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if (d.open >= 0 || len(d.carry) > 0) && d.openKind != kind {
		if err := d.closeText(); err != nil {
			return err
		}
	}
	d.openKind = kind
	s := whole(&d.carry, b)
	if s == "" {
		return nil
	}
	if d.open < 0 {
		n, err := d.newPart()
		if err != nil {
			return err
		}
		d.open = n
		d.emit(ir.Event{Kind: ir.PartStart, Index: n, Part: ir.Part{Kind: kind}})
	}
	delta := ir.TextDelta
	if kind == ir.Thinking {
		delta = ir.ThinkingDelta
	}
	d.emit(ir.Event{Kind: delta, Index: d.open, Text: s})
	return nil
}

// find returns the call a tool-call delta belongs to, making a new one when
// it is the first delta of its call.
func (d *StreamDecoder) find(index count, id string) (*toolCall, error) {
	var c, latest *toolCall
	if n := len(d.calls); n > 0 {
		latest = d.calls[n-1]
	}
	switch {
	case index.bad:
		return nil, malformed("a tool call's index is not a number")
	case index.set:
		// A known index with another id is a new call: some providers number
		// every call 0.
		if c = d.byIndex[index.n]; c != nil && id != "" && c.id != "" && c.id != id {
			c = nil
		}
	case id != "":
		// No index. A new id is a new call, unless the latest call is still
		// waiting for its id: then this is it (arguments came first).
		if c = d.byID[id]; c == nil && latest != nil && latest.id == "" && !latest.closed {
			c = latest
		}
	default:
		c = latest // no index, no id: more of the latest call
	}
	if c == nil {
		if len(d.calls) >= ir.MaxToolCalls {
			return nil, overLimit("tool calls", ir.MaxToolCalls)
		}
		c = &toolCall{part: -1}
		d.calls = append(d.calls, c)
		if index.set {
			d.byIndex[index.n] = c
		}
	}
	return c, nil
}

func (d *StreamDecoder) toolCall(tc *wireToolCall) error {
	c, err := d.find(tc.Index, tc.ID)
	if err != nil {
		return err
	}
	if c.closed {
		return malformed("a tool call delta after the finish")
	}
	if tc.ID != "" && c.id == "" {
		if d.byID[tc.ID] != nil {
			return malformed("two tool calls with one id")
		}
		c.id = tc.ID
		d.byID[tc.ID] = c
	}
	fragment := ""
	if tc.Function != nil {
		if c.name == "" {
			c.name = tc.Function.Name
		}
		raw, err := argumentsBytes(tc.Function.Arguments)
		if err != nil {
			return err
		}
		// Arguments are passed on or refused, never repaired (text is).
		b := complete(&c.carry, raw)
		if !utf8.Valid(b) {
			return malformed("tool call arguments are not valid UTF-8")
		}
		fragment = string(b)
	}
	if len(c.args) == 0 {
		fragment = strings.TrimLeft(fragment, " \t\r\n")
	}
	if len(c.args)+len(fragment) > ir.MaxToolArgsBytes {
		return overLimit("tool call argument bytes", ir.MaxToolArgsBytes)
	}
	if d.argsBytes += len(fragment); d.argsBytes > ir.MaxTotalToolArgsBytes {
		return overLimit("tool call argument bytes in all", ir.MaxTotalToolArgsBytes)
	}
	c.args = append(c.args, fragment...)
	if c.part >= 0 {
		if fragment != "" {
			d.emit(ir.Event{Kind: ir.ToolArgsDelta, Index: c.part, ArgsJSON: fragment})
		}
		return nil
	}
	if c.id == "" || c.name == "" {
		return nil
	}
	if err := d.closeText(); err != nil {
		return err
	}
	if c.part, err = d.newPart(); err != nil {
		c.part = -1
		return err
	}
	d.opened = true
	d.emit(ir.Event{Kind: ir.PartStart, Index: c.part, Part: ir.Part{Kind: ir.ToolUse, ToolID: c.id, ToolName: c.name}})
	if len(c.args) > 0 {
		d.emit(ir.Event{Kind: ir.ToolArgsDelta, Index: c.part, ArgsJSON: string(c.args)})
	}
	return nil
}

// closeParts checks what is open and then emits a PartStop for every open
// part, in part order. Nothing is emitted when a check fails: the caller
// fails the answer, which stops the parts.
func (d *StreamDecoder) closeParts() error {
	if len(d.carry) > 0 {
		d.carry = nil
		return malformed("text ends inside a character")
	}
	for _, c := range d.calls {
		if c.closed {
			continue
		}
		if c.part < 0 {
			return malformed("a tool call without a name or an id")
		}
		if len(c.carry) > 0 {
			return malformed("tool call arguments end inside a character")
		}
		if len(c.args) > 0 {
			// The pieces were sent on as they came; the caller is told now
			// that they are no object, before it sees a Finish.
			if err := ir.CheckObject(c.args); errors.Is(err, ir.ErrLimit) {
				return err
			} else if err != nil {
				return malformed("tool call arguments are not a JSON object")
			}
		}
	}
	var open []int
	if d.open >= 0 {
		open = append(open, d.open)
		d.open = -1
	}
	for _, c := range d.calls {
		if !c.closed {
			c.closed = true
			c.args = nil
			open = append(open, c.part)
		}
	}
	sort.Ints(open)
	for _, n := range open {
		d.emit(ir.Event{Kind: ir.PartStop, Index: n})
	}
	return nil
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
	s, ok := rawString(raw)
	if !ok {
		var obj struct {
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(raw, &obj) == nil {
			s, _ = rawString(obj.Message)
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

// rawBytes reads a field that should be a string, like rawString, but keeps
// the bytes as they are: where encoding/json would turn each byte that is
// not UTF-8 into U+FFFD, the half of a character that a provider cut in two
// stays a half, so that whole can join it with the other.
func rawBytes(raw json.RawMessage) (b []byte, ok bool) {
	if isNull(raw) {
		return nil, true
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, false
	}
	return unquote(raw[1 : len(raw)-1])
}

// argumentsBytes is argumentsText for a stream chunk, with the bytes kept.
func argumentsBytes(raw json.RawMessage) ([]byte, error) {
	if b, ok := rawBytes(raw); ok {
		return b, nil
	}
	if trimmed := bytes.TrimSpace(raw); trimmed[0] == '{' {
		return trimmed, nil
	}
	return nil, malformed("tool call arguments are neither a string nor an object")
}

// unquote resolves the escapes of a JSON string's content (the part between
// the quotes, already checked by encoding/json). Bytes that are not escaped
// are copied as they are. A surrogate escape without its partner becomes
// the three bytes UTF-8 would use for it if it were allowed to: not valid
// UTF-8, and recognisable, so the halves of a pair that was cut between two
// chunks can be joined.
func unquote(s []byte) ([]byte, bool) {
	if bytes.IndexByte(s, '\\') < 0 {
		return s, true
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		if i+1 >= len(s) {
			return nil, false
		}
		i += 2
		switch s[i-1] {
		case '"', '\\', '/':
			out = append(out, s[i-1])
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, ok := hex4(s[i:])
			if !ok {
				return nil, false
			}
			i += 4
			if !utf16.IsSurrogate(r) {
				out = utf8.AppendRune(out, r)
				break
			}
			if len(s) >= i+6 && s[i] == '\\' && s[i+1] == 'u' {
				if r2, ok := hex4(s[i+2:]); ok {
					if pair := utf16.DecodeRune(r, r2); pair != utf8.RuneError {
						out = utf8.AppendRune(out, pair)
						i += 6
						break
					}
				}
			}
			out = append(out, 0xED, 0x80|byte(r>>6)&0x3F, 0x80|byte(r)&0x3F)
		default:
			return nil, false
		}
	}
	return out, true
}

func hex4(s []byte) (rune, bool) {
	if len(s) < 4 {
		return 0, false
	}
	var r rune
	for _, c := range s[:4] {
		switch {
		case c >= '0' && c <= '9':
			c -= '0'
		case c >= 'a' && c <= 'f':
			c -= 'a' - 10
		case c >= 'A' && c <= 'F':
			c -= 'A' - 10
		default:
			return 0, false
		}
		r = r<<4 | rune(c)
	}
	return r, true
}

// surrogate reports whether b begins with the three bytes unquote writes
// for a lone surrogate, and which half it is.
func surrogate(b []byte) (r rune, high, ok bool) {
	if len(b) < 3 || b[0] != 0xED || b[1]&0xE0 != 0xA0 || b[2]&0xC0 != 0x80 {
		return 0, false, false
	}
	r = 0xD000 | rune(b[1]&0x3F)<<6 | rune(b[2]&0x3F)
	return r, r < 0xDC00, true
}

// whole joins the bytes of a text delta with what the previous delta of the
// same part left over (see complete) and returns the text that is complete.
// Bytes that are no UTF-8 become U+FFFD, as they do in a complete answer.
func whole(carry *[]byte, b []byte) string {
	b = complete(carry, b)
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "\ufffd")
}

// complete joins the bytes of a delta with what the previous delta of the
// same part left over, and returns the bytes that are complete, as they are.
// The first bytes of a character at the very end — a UTF-8 sequence that is
// not finished, or the first half of a surrogate pair — are kept in *carry
// for the next delta.
func complete(carry *[]byte, b []byte) []byte {
	if len(*carry) > 0 {
		joined := make([]byte, 0, len(*carry)+len(b))
		joined = append(append(joined, *carry...), b...)
		if hi, isHigh, ok := surrogate(joined); ok && isHigh && len(*carry) == 3 {
			if lo, isHigh, ok := surrogate(joined[3:]); ok && !isHigh {
				joined = append(utf8.AppendRune(make([]byte, 0, len(joined)), utf16.DecodeRune(hi, lo)), joined[6:]...)
			}
		}
		b = joined
	}
	n := unfinished(b)
	*carry = append([]byte(nil), b[len(b)-n:]...)
	if n == 0 {
		*carry = nil
	}
	return b[:len(b)-n]
}

// unfinished returns how many bytes at the end of b are the beginning of a
// character that is not complete: 0 to 3.
func unfinished(b []byte) int {
	for i := 1; i <= 3 && i <= len(b); i++ {
		c := b[len(b)-i]
		if c&0xC0 == 0x80 {
			continue // a continuation byte: look for its lead
		}
		need := 0
		switch {
		case c >= 0xF0 && c < 0xF8:
			need = 4
		case c >= 0xE0:
			need = 3
		case c >= 0xC0:
			need = 2
		}
		if i < need {
			return i
		}
		if _, high, ok := surrogate(b[len(b)-i:]); ok && high && i == 3 {
			return 3
		}
		return 0
	}
	return 0
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
	s, _ := rawString(obj.Message)
	return cutMessage(s)
}
