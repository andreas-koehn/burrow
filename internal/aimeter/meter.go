// Package aimeter implements an SSE-aware usage accumulator for AI
// upstream traffic (OpenAI- and Anthropic-shaped responses).
//
// # Surface
//
// The package exposes a single visitor-side wrapping constructor:
//
//	stream := aimeter.WrapResponse(visitorWriter, aimeter.KindOpenAI)
//	io.Copy(stream, upstreamBody)        // forwards each SSE frame immediately
//	t := stream.Tokens()                 // observed token counts
//	n := stream.Bytes()                  // bytes forwarded to the visitor
//
// WrapResponse returns a single Stream value that implements both [io.Writer]
// and [Accumulator]. The two-return shape proposed in earlier drafts of the
// v0.4.0 plan (Wrap returning (Writer, Accumulator)) was rejected in favor of
// this single-return shape because tests and call sites consistently need to
// hold one value that does both — and a single value rules out the bug of
// passing the writer half without the accumulator half. The Stream type
// remains documented (so callers can store it in a struct field) and concrete
// (so callers can switch on Kind() for diagnostics).
//
// # SSE no-buffering invariant
//
// Stream.Write parses upstream-shaped SSE frames in line-buffered mode and
// forwards each completed frame to the visitor-side writer immediately, then
// calls [http.Flusher.Flush] if the writer implements it. The accumulator
// never holds an SSE frame to wait for the next one — observing token usage is
// strictly best-effort on the side of forwarding bytes.
//
// # Non-stream responses
//
// For non-streaming responses, callers may use ParseOpenAIBody /
// ParseAnthropicBody to extract tokens from a fully-buffered response body.
// Streaming detection (presence of "data: " or "event: " framing) is left to
// the caller — this package does not auto-detect framing from a single Write.
//
// # Byte-estimate fallback
//
// If a stream completes without ever observing a usage object, Stream.Tokens
// returns the byte-estimate fallback (bytes_in / 4, bytes_out / 4) per the
// v0.4.0 spec Q-mtr. Callers writing a usage_events row should treat the
// returned Tokens as authoritative regardless.
//
// # Non-blocking metering
//
// SQLSink.Record logs and swallows errors via [log/slog]; a metering failure
// must never break the proxy stream.
package aimeter

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
)

// Kind names a supported upstream response shape.
type Kind string

const (
	KindOpenAI    Kind = "openai"
	KindAnthropic Kind = "anthropic"
	KindMCP       Kind = "mcp"
	KindUnknown   Kind = "unknown"
)

// Tokens carries observed token counts. Total is filled when present in the
// upstream payload; otherwise it equals In+Out.
type Tokens struct {
	In, Out, Total int
}

// Sample is the per-request metering record handed to a Sink.
type Sample struct {
	ServiceID, APIKeyID, Model string
	Kind                       Kind
	TokensIn, TokensOut        int
	BytesIn, BytesOut          int64
	Streamed                   bool
	CacheHit                   bool
	UpstreamStatus             int
	// CostUSD is the cost the upstream reported for this request. nil means
	// it reported none and the price table applies.
	CostUSD *float64
	// GatewayKeyID is the gateway key that made the request; "" when it did
	// not come through a gateway key.
	GatewayKeyID string
	// Dialect is the API format the caller spoke ("openai" or "anthropic").
	Dialect string
	// ProviderSlug is the provider that answered.
	ProviderSlug string
	// RequestedModel is the model name the caller asked for.
	RequestedModel string
	// TargetModel is the provider's model that answered.
	TargetModel string
	// RequestID is the Burrow-Request-Id of the request.
	RequestID string
	// LatencyMs is the time from the request's arrival to the end of the response.
	LatencyMs int64
	// Translated is the id of the pair that translated the request (for
	// example "messages-chat"); "" when it was served in its own format.
	Translated string
	// Dropped is what the translation left out: names joined by JoinDropped,
	// never values. "" when nothing was dropped or nothing was translated.
	Dropped string
}

// Accumulator is the read-side of a Stream: it exposes the running token
// counts and forwarded byte counts. After the upstream body has been fully
// copied through the Stream, Tokens() returns the final observed counts (or
// the byte-estimate fallback when no usage object was ever observed).
type Accumulator interface {
	Tokens() Tokens
	Bytes() (in, out int64)
}

// Stream is the visitor-side wrapper returned by WrapResponse. It implements
// both io.Writer (for io.Copy from the upstream body) and Accumulator (for
// reading the final token/byte counts).
//
// One Stream value handles one upstream response. Concurrent Write calls are
// not supported (the underlying reverse-proxy copy loop is single-goroutine).
type Stream struct {
	w       io.Writer
	flusher http.Flusher // nil if w doesn't implement http.Flusher
	kind    Kind
	parser  parser

	// counters
	bytesIn  int64 // bytes seen on the upstream side (i.e. p in Write)
	bytesOut int64 // bytes successfully forwarded to w
	tokens   Tokens
	gotUsage bool
	costUSD  float64 // what the upstream says it charged; valid when hasCost
	hasCost  bool
}

// parser is the per-kind line-buffered SSE parser. It receives raw bytes
// from Stream.Write, splits them into SSE frames, forwards each completed
// frame to the underlying writer immediately, and reports observed tokens
// via the supplied callback.
type parser interface {
	// write feeds bytes; returns the number of bytes successfully forwarded
	// to the underlying writer (used to update bytesOut accurately even when
	// the writer returns a short write) and any error.
	write(p []byte) (forwarded int, err error)
	// close flushes any pending buffered line as a final frame.
	close() error
}

// WrapResponse returns a Stream that wraps the visitor-side writer w and
// applies the SSE parser appropriate for the named upstream kind.
//
// If w implements http.Flusher, each forwarded SSE frame is flushed before
// the next is read — this is the SSE no-buffering invariant. If w does not
// implement http.Flusher (e.g. tests, plain net.Conn-backed writers), frames
// are still forwarded one-at-a-time but without an explicit Flush.
func WrapResponse(w io.Writer, kind Kind) *Stream {
	s := &Stream{
		w:    w,
		kind: kind,
	}
	if f, ok := w.(http.Flusher); ok {
		s.flusher = f
	}
	switch kind {
	case KindOpenAI:
		s.parser = newOpenAIParser(s)
	case KindAnthropic:
		s.parser = newAnthropicParser(s)
	default:
		s.parser = newPassthroughParser(s)
	}
	return s
}

// Write implements io.Writer. It forwards bytes through the per-kind parser,
// which in turn forwards each completed SSE frame to the visitor writer and
// flushes when possible. The returned n always reflects how many of the
// input bytes were consumed (parser-side accounting), so io.Copy sees a
// well-formed Writer.
func (s *Stream) Write(p []byte) (int, error) {
	s.bytesIn += int64(len(p))
	fwd, err := s.parser.write(p)
	s.bytesOut += int64(fwd)
	if err != nil {
		return len(p), err
	}
	return len(p), nil
}

// Close flushes any pending buffered line as a final frame. Safe to call
// more than once. Callers should invoke Close at end-of-body when wrapping
// a streaming response; non-streaming callers may skip it.
func (s *Stream) Close() error {
	return s.parser.close()
}

// Tokens returns the observed token counts. If no usage object was ever
// observed, it returns the byte-estimate fallback (bytes_out/4 for Out,
// bytes_in/4 for In) per the v0.4.0 spec Q-mtr.
//
// "bytes_in" here means bytes seen on the upstream side (i.e. the upstream
// response body fed into Write), and "bytes_out" means bytes forwarded to
// the visitor. For the byte-estimate fallback we use the upstream side for
// Out (server-generated content) and… well, only one side exists at this
// layer. The caller (Task 10) supplies request-body bytes for In separately
// when constructing the Sample.
func (s *Stream) Tokens() Tokens {
	if s.gotUsage {
		return s.tokens
	}
	out := int(s.bytesIn / 4)
	return Tokens{In: 0, Out: out, Total: out}
}

// Bytes returns the running byte counters: bytes seen from the upstream
// side and bytes successfully forwarded to the visitor.
func (s *Stream) Bytes() (in, out int64) {
	return s.bytesIn, s.bytesOut
}

// Kind returns the configured upstream kind (for diagnostics).
func (s *Stream) Kind() Kind { return s.kind }

// recordTokens is called by parsers when a usage object is observed.
// It marks the stream as having authoritative token data.
func (s *Stream) recordTokens(in, out, total int) {
	s.tokens.In = in
	s.tokens.Out = out
	if total > 0 {
		s.tokens.Total = total
	} else {
		s.tokens.Total = in + out
	}
	s.gotUsage = true
}

// recordCost is called by parsers when the upstream reports what it charged.
// Like the token counters it is written from the copy loop only.
func (s *Stream) recordCost(usd float64) {
	s.costUSD, s.hasCost = usd, true
}

// clearCost is called by parsers when a usage chunk carries no valid cost.
func (s *Stream) clearCost() {
	s.costUSD, s.hasCost = 0, false
}

// Cost returns the cost the upstream reported for this response, if any. A
// stream that was cut off before its usage chunk reports none.
func (s *Stream) Cost() (float64, bool) {
	return s.costUSD, s.hasCost
}

// flush calls http.Flusher.Flush if the writer implements it. Safe to call
// when no flusher is available (no-op).
func (s *Stream) flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// passthroughParser is used for Kind values that have no SSE parser (mcp,
// unknown). It still preserves the no-buffering invariant by forwarding
// bytes immediately and flushing after each Write.
type passthroughParser struct{ s *Stream }

func newPassthroughParser(s *Stream) *passthroughParser { return &passthroughParser{s: s} }

func (p *passthroughParser) write(b []byte) (int, error) {
	n, err := p.s.w.Write(b)
	if n > 0 {
		p.s.flush()
	}
	return n, err
}

func (p *passthroughParser) close() error { return nil }

// Sink is the interface implemented by SQLSink and test fakes.
type Sink interface {
	Record(ctx context.Context, s Sample) error
}

// Bounds of what a usage row stores about a translation. The names of what
// was dropped start as field names of a request, which a client chooses.
const (
	maxTranslationName = 64  // bytes of the pair id and of one dropped name
	maxDroppedEntries  = 32  // names in one list
	maxDroppedBytes    = 512 // bytes of the joined list
)

// cleanTranslationName keeps the letters, digits and "_.:-" of s, at most
// maxTranslationName bytes of them. A comma, a space, a control character or
// a non-ASCII character is left out.
func cleanTranslationName(s string) string {
	b := make([]byte, 0, min(len(s), maxTranslationName))
	for i := 0; i < len(s) && len(b) < maxTranslationName; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '.', c == ':', c == '-':
			b = append(b, c)
		}
	}
	return string(b)
}

// CleanPair returns a translation pair id as a usage row stores it: letters,
// digits and "_.:-" only, at most 64 bytes. The ids are constants of the
// gateway ("messages-chat"); this keeps the column bounded whoever sets it.
func CleanPair(pair string) string { return cleanTranslationName(pair) }

// JoinDropped turns the names of what a translation left out into the string
// a usage row stores and the Burrow-Dropped header carries: each name reduced
// to letters, digits and "_.:-" and cut to 64 bytes, empty names left out,
// sorted, without duplicates, joined by ",". The result holds at most 32
// names and 512 bytes and never ends in a cut name. Pass names only, never
// values. Joining the names of a joined list again gives the same list.
func JoinDropped(names []string) string {
	clean := make([]string, 0, len(names))
	for _, n := range names {
		if n = cleanTranslationName(n); n != "" {
			clean = append(clean, n)
		}
	}
	slices.Sort(clean)
	clean = slices.Compact(clean)
	if len(clean) > maxDroppedEntries {
		clean = clean[:maxDroppedEntries]
	}
	var b strings.Builder
	for _, n := range clean {
		if b.Len()+len(n)+1 > maxDroppedBytes && b.Len() > 0 {
			break
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
	}
	return b.String()
}
