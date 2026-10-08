// Package translate joins the format codecs into pairs: a caller that
// speaks one wire format is served by a target that speaks another. A pair
// rewrites the request (Request) and wraps the caller's response writer so
// that what the target answers reaches the caller in its own format
// (Response), streamed or whole.
//
// Nothing in this package logs. No error text and no value handed to a hook
// holds request or answer content; what a provider says in an error goes to
// the caller's response and nowhere else.
package translate

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/aigw/translate/chat"
	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
	"github.com/ankoehn/burrow/internal/aigw/translate/messages"
	"github.com/ankoehn/burrow/internal/aigw/translate/responses"
)

// Format is a wire format a caller or a target speaks.
type Format string

const (
	Messages  Format = "messages"  // Anthropic Messages
	Chat      Format = "chat"      // OpenAI Chat Completions
	Responses Format = "responses" // OpenAI Responses
)

// CallerFormat maps a dialect endpoint path to the format a caller speaks
// there; ok is false for paths that are never translated (embeddings,
// completions, count_tokens, models).
func CallerFormat(dialect, path string) (Format, bool) {
	switch {
	case dialect == "anthropic" && path == "/v1/messages":
		return Messages, true
	case dialect == "openai" && path == "/v1/chat/completions":
		return Chat, true
	case dialect == "openai" && path == "/v1/responses":
		return Responses, true
	}
	return "", false
}

// TargetFormat is the format a provider of that api_format is called in:
// "openai" → Chat, "anthropic" → Messages, anything else "".
func TargetFormat(apiFormat string) Format {
	switch apiFormat {
	case "openai":
		return Chat
	case "anthropic":
		return Messages
	}
	return ""
}

// Pair translates one caller format to one target format.
type Pair interface {
	// ID names the pair, e.g. "messages-chat": the value of Burrow-Translated.
	ID() string
	// Released reports whether the pair's tool-call checks pass (see Lookup).
	Released() bool
	// UpstreamPath is the target's endpoint, e.g. "/v1/chat/completions".
	UpstreamPath() string
	// Request rewrites the caller's body for the target model. header is the
	// caller's request header (nil is fine): a pair reports what it reads
	// there and the target cannot be given ("anthropic-beta"); the gateway
	// still decides which headers go upstream. stream reports whether the
	// caller asked for a stream. dropped names everything that was left out,
	// sorted and without duplicates, nil when nothing was. An error is a
	// client error when BadRequest says so.
	Request(body []byte, header http.Header, targetModel string) (out []byte, stream bool, dropped []string, err error)
	// Response wraps the caller's writer: what is written to the result is
	// the target's answer, what reaches w is the caller's.
	Response(w http.ResponseWriter, o ResponseOptions) ResponseWriter
}

// ResponseOptions tells a response writer about the call it answers.
type ResponseOptions struct {
	// Stream says the caller asked for a streamed answer (Request told).
	Stream bool
	// RequestedModel is named in the answer when the target names no model.
	RequestedModel string
	// CallerRequest is the caller's request body, as it was given to
	// Request. A caller format reads from it what shapes its answer and
	// Request does not tell: for a Chat Completions caller that is
	// "stream_options.include_usage" — the usage chunk of a stream is
	// written only when it was asked for, because a client that did not ask
	// need not expect a chunk without choices. The other formats do not
	// read it. nil is fine: nothing was asked for then.
	CallerRequest []byte
	// OnError, when set, is called once when the answer the caller gets is
	// an error the writer made: before the header of an error response
	// (status is the one about to be written: the upstream's own when it
	// was an error, else 502), or when a stream that had begun is ended
	// with an error event (status is the one already sent). code is
	// CodeUpstreamError or CodeUpstreamInvalid. Neither holds anything the
	// provider said, so both may be logged and stored. An error response
	// carries the code in its body (burrow_code in the Messages shape), as
	// the gateway's own errors do; the Burrow-Error-Code header that goes
	// with it is the gateway's to set, here, on its own writer — the
	// response writer sets no Burrow header. An error event that ends a
	// stream has the plain shape of the caller's format and no header.
	OnError func(status int, code string)
}

// The codes a response writer reports.
const (
	// CodeUpstreamError: the provider answered with an error — a status
	// that is no success, or an error object in place of an answer.
	CodeUpstreamError = "upstream_error"
	// CodeUpstreamInvalid: the provider's answer cannot be used — malformed,
	// over a limit, cut off, or not in the form that was asked for.
	CodeUpstreamInvalid = "upstream_invalid"
)

// ResponseWriter takes the target's answer and writes the caller's. The
// upstream handler writes to it as to any http.ResponseWriter; Finish must
// be called after that handler returned.
//
// The caller's WriteHeader is called exactly once. A streamed answer is
// committed when its first event is written — not when the upstream's
// header arrives — and flushed after every upstream write from then on; a
// buffered answer and every error response are written by Finish. So a
// failure before the first byte is an HTTP error response in the caller's
// error shape (the upstream's status when that was an error, else 502), and
// only a failure after it ends the caller's stream with the caller's error
// event. Failure tells the two apart.
//
// Flush, Unwrap and NoteUpstreamTimeout are passed on to the caller's
// writer, so http.ResponseController and the gateway's own wrappers work
// through it.
type ResponseWriter interface {
	http.ResponseWriter
	// Finish ends the answer. It is harmless to call twice; writes after it
	// are ignored.
	Finish()
	// Failure reports, after Finish, whether the caller got an error the
	// writer made: code is "" for a translated answer, else one of the
	// Code… constants. midStream is true when the error ended a stream
	// whose header (the upstream's success status) had already been sent.
	Failure() (code string, midStream bool)
	// Discarding reports, at any time, that nothing more of the upstream's
	// body will be used: the answer has failed, or it outgrew its limit.
	// What the upstream still sends is thrown away, so whoever reads the
	// upstream should stop reading. It is false for an answer that is going
	// well, also after its last event (a usage figure may still follow).
	Discarding() bool
}

// Lookup returns the pair that serves a caller of format from with a target
// of format to. Only a released pair is handed out: a pair whose tool-call
// checks do not pass behaves as if translation were off.
func Lookup(from, to Format) (Pair, bool) {
	p := pairs[[2]Format{from, to}]
	if p == nil || !p.Released() {
		return nil, false
	}
	return p, true
}

// BadRequest reports whether err, returned by Pair.Request, is a client
// error, and returns a message for the caller: it names the field and the
// reason and holds nothing of the request.
func BadRequest(err error) (message string, ok bool) {
	var bad *ir.BadRequestError
	if errors.As(err, &bad) || errors.Is(err, chat.ErrUnsupported) || errors.Is(err, messages.ErrUnsupported) {
		return err.Error(), true
	}
	return "", false
}

// pairs is the registry, filled once below and only read afterwards.
var pairs = map[[2]Format]*pair{
	{Messages, Chat}:      newMessagesChat(),
	{Responses, Chat}:     newResponsesChat(),
	{Chat, Messages}:      newChatMessages(),
	{Responses, Messages}: newResponsesMessages(),
}

// pair is a Pair made of a request function and the two halves of a codec.
type pair struct {
	id, path string
	request  func(body []byte, header http.Header, model string) ([]byte, bool, []string, error)
	codec    codec

	// check runs the pair's tool-call checks against the pair itself.
	// Released is its verdict, taken once.
	check    func(p *pair) error
	once     sync.Once
	released bool
}

func (p *pair) ID() string           { return p.id }
func (p *pair) UpstreamPath() string { return p.path }

// Released runs the pair's tool-call checks the first time it is asked and
// reports whether they passed: the pair's own Request and Response are fed
// tool definitions, tool calls split across stream chunks and interleaved,
// and tool results, and what comes out is compared with what must. It is
// not a constant: a pair whose code no longer carries a tool call is not
// released, in the tests and in a running gateway alike.
func (p *pair) Released() bool {
	p.once.Do(func() { p.released = p.check != nil && p.check(p) == nil })
	return p.released
}

func (p *pair) Request(body []byte, header http.Header, model string) ([]byte, bool, []string, error) {
	return p.request(body, header, model)
}

func (p *pair) Response(w http.ResponseWriter, o ResponseOptions) ResponseWriter {
	return newWriter(w, o, p.codec)
}

// newMessagesChat makes the pair "messages-chat": Anthropic Messages
// callers (Claude Code) on Chat Completions targets.
func newMessagesChat() *pair {
	return &pair{
		id:      "messages-chat",
		path:    "/v1/chat/completions",
		request: messagesToChat,
		check:   checkMessagesChat,
		codec:   codec{chatTarget, messagesCaller},
	}
}

func messagesToChat(body []byte, header http.Header, model string) ([]byte, bool, []string, error) {
	req, err := messages.DecodeRequest(body)
	if err != nil {
		return nil, false, nil, err
	}
	if len(header.Values("Anthropic-Beta")) > 0 {
		req.Dropped = append(req.Dropped, ir.DroppedAnthropicBeta)
	}
	out, dropped, err := chat.EncodeRequest(req, model)
	if err != nil {
		return nil, false, nil, err
	}
	return out, req.Stream, dropped, nil
}

// newResponsesChat makes the pair "responses-chat": OpenAI Responses callers
// (Codex, which speaks nothing else) on Chat Completions targets. Only the
// call itself is translated: no response is stored, so the endpoints below
// /v1/responses/ that work on a stored one have no pair (see CallerFormat).
func newResponsesChat() *pair {
	return &pair{
		id:      "responses-chat",
		path:    "/v1/chat/completions",
		request: responsesToChat,
		check:   checkResponsesChat,
		codec:   codec{chatTarget, responsesCaller},
	}
}

// responsesToChat rewrites a Responses request. Nothing is read from the
// caller's header: what Codex sends there (OpenAI-Beta, its session and
// conversation ids) asks the model for nothing.
func responsesToChat(body []byte, _ http.Header, model string) ([]byte, bool, []string, error) {
	req, err := responses.DecodeRequest(body)
	if err != nil {
		return nil, false, nil, err
	}
	out, dropped, err := chat.EncodeRequest(req, model)
	if err != nil {
		return nil, false, nil, err
	}
	return out, req.Stream, dropped, nil
}

// responsesCaller writes answers for a Responses caller. A response's
// "created_at" is the moment the gateway begins to write it.
var responsesCaller = callerCodec{
	encodeResponse: func(resp ir.Response, model string) ([]byte, error) {
		return responses.EncodeResponse(resp, model, time.Now())
	},
	encodeError: responses.EncodeError,
	newStreamEncoder: func(w io.Writer, o ResponseOptions) streamEncoder {
		return responses.NewStreamEncoder(w, o.RequestedModel, time.Now())
	},
}

// newChatMessages makes the pair "chat-messages": Chat Completions callers
// (the OpenAI SDKs and what is built on them) on Anthropic Messages targets.
// The request it writes is for the "anthropic-version" messages.Version; the
// header is the gateway's to set.
func newChatMessages() *pair {
	return &pair{
		id:      "chat-messages",
		path:    "/v1/messages",
		request: chatToMessages,
		check:   checkChatMessages,
		codec:   codec{messagesTarget, chatCaller},
	}
}

// chatToMessages rewrites a Chat Completions request. Nothing is read from
// the caller's header.
func chatToMessages(body []byte, _ http.Header, model string) ([]byte, bool, []string, error) {
	req, err := chat.DecodeRequest(body)
	if err != nil {
		return nil, false, nil, err
	}
	out, dropped, err := messages.EncodeRequest(req, model)
	if err != nil {
		return nil, false, nil, err
	}
	return out, req.Stream, dropped, nil
}

// newResponsesMessages makes the pair "responses-messages": OpenAI Responses
// callers (Codex) on Anthropic Messages targets. As with responses-chat only
// the call itself is translated (see CallerFormat).
func newResponsesMessages() *pair {
	return &pair{
		id:      "responses-messages",
		path:    "/v1/messages",
		request: responsesToMessages,
		check:   checkResponsesMessages,
		codec:   codec{messagesTarget, responsesCaller},
	}
}

func responsesToMessages(body []byte, _ http.Header, model string) ([]byte, bool, []string, error) {
	req, err := responses.DecodeRequest(body)
	if err != nil {
		return nil, false, nil, err
	}
	out, dropped, err := messages.EncodeRequest(req, model)
	if err != nil {
		return nil, false, nil, err
	}
	return out, req.Stream, dropped, nil
}
