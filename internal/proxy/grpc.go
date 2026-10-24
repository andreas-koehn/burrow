package proxy

// gRPC through http services (HTTP/2, gRPC and SNI on 443, H6–H9, H14).
//
// A gRPC call is an HTTP/2 POST whose body and response are streams of
// length-prefixed messages and whose outcome is the grpc-status trailer. The
// proxy forwards it like any request, unbuffered (FlushInterval -1, full
// duplex, trailers copied by the reverse proxy), and adds four things:
//
//   - refusals as gRPC status (H7): for grpc and grpc-web calls the writer
//     is wrapped at ServeHTTP's entry, and a response that is not a 200 and
//     carries no grpc-status, whoever wrote it, goes out as a trailers-only
//     reply with the mapped code;
//   - deadlines (H8): grpc-timeout is forwarded, the relay's own deadline is
//     a second after it, and the service's grpc_max_duration_s caps both;
//   - a native gRPC call to a service whose upstream is HTTP/1.1 is
//     answered UNAVAILABLE at once (H4);
//   - the call's method and grpc-status in the connection log, the
//     observer, the metrics and the request summary (H9).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/grpcwire"
	"github.com/ankoehn/burrow/internal/proto"
)

// ReasonGRPCNoStatus is the connection-log reason of a gRPC call that ended
// without any grpc-status: a stream reset, an upstream that broke off.
const ReasonGRPCNoStatus = "grpc_no_status"

// grpcDeadlineSlack is how much longer than the call's grpc-timeout the
// relay waits: the upstream and the caller enforce the timeout themselves,
// the relay only ends a call that both left running (H8).
const grpcDeadlineSlack = time.Second

// maxDenyBody is how much of a refusal's body is kept to become its
// grpc-message.
const maxDenyBody = 512

// maxDenyMessage bounds the grpc-message of a refusal.
const maxDenyMessage = 200

// msgHTTP1Upstream is the grpc-message of a native gRPC call to a service
// whose upstream protocol is HTTP/1.1 (H4).
const msgHTTP1Upstream = "service upstream is HTTP/1.1; set upstream_protocol h2c or h2 for gRPC"

// msgDeadline is the grpc-message of a call the relay's deadline ended
// before the upstream answered.
const msgDeadline = "the call's deadline passed before the service answered"

// grpcWriter is the visitor's writer for a grpc or grpc-web call (H7). A
// response with status 200, or one that carries a grpc-status header, passes
// through untouched: headers, every write and flush, and the trailers the
// reverse proxy sets in the header map. Any other response is held: its
// status picks the gRPC code, the start of its body becomes the message, and
// finish sends it as a trailers-only reply, keeping headers such as
// Retry-After. 1xx responses pass through.
//
// Header is the writer's own map throughout, so the wrappers further in
// (countingResponseWriter, httpduplex) and the reverse proxy see the same
// headers and trailers.
type grpcWriter struct {
	http.ResponseWriter
	contentType string // the request's, echoed in a trailers-only reply

	started  bool          // a response is passing through
	denied   bool          // a refusal is held for finish
	status   int           // the held refusal's HTTP status
	code     grpcwire.Code // and its gRPC code
	body     []byte        // the start of its body
	finished bool
}

func newGRPCWriter(w http.ResponseWriter, r *http.Request) *grpcWriter {
	return &grpcWriter{ResponseWriter: w, contentType: r.Header.Get("Content-Type")}
}

func (g *grpcWriter) WriteHeader(code int) {
	switch {
	case g.denied:
		return
	case g.started:
		g.ResponseWriter.WriteHeader(code) // net/http notes the superfluous call
		return
	case code >= 100 && code < 200:
		g.ResponseWriter.WriteHeader(code)
		return
	case code == http.StatusOK || g.Header().Get("Grpc-Status") != "":
		g.started = true
		g.ResponseWriter.WriteHeader(code)
		return
	}
	g.denied, g.status, g.code = true, code, grpcwire.StatusForHTTP(code)
}

func (g *grpcWriter) Write(p []byte) (int, error) {
	if g.denied {
		if room := maxDenyBody - len(g.body); room > 0 {
			g.body = append(g.body, p[:min(room, len(p))]...)
		}
		return len(p), nil
	}
	g.started = true
	return g.ResponseWriter.Write(p)
}

// Flush implements http.Flusher; a held refusal is not flushed.
func (g *grpcWriter) Flush() { _ = g.FlushError() }

// FlushError is the form of Flush http.ResponseController asks for.
func (g *grpcWriter) FlushError() error {
	if g.denied {
		return nil
	}
	g.started = true
	return http.NewResponseController(g.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the server's writer for what
// this wrapper does not do itself (full duplex, deadlines).
func (g *grpcWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// heldCode is the gRPC code of the refusal this writer holds.
func (g *grpcWriter) heldCode() (grpcwire.Code, bool) { return g.code, g.denied }

// finish sends the held refusal, if there is one, as a trailers-only reply.
// It is called once the handler is done.
func (g *grpcWriter) finish() {
	if !g.denied || g.finished {
		return
	}
	g.finished = true
	grpcwire.WriteTrailersOnly(g.ResponseWriter, g.contentType, g.code, denyMessage(g.status, g.body))
}

// denyMessage is the grpc-message of a refusal: the "error" (or "message")
// field of a JSON body, else the first line of a text body, else the status
// text.
func denyMessage(status int, body []byte) string {
	body = bytes.TrimSpace(body)
	msg := ""
	if len(body) > 0 && body[0] == '{' {
		var v struct {
			Error   any    `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &v) == nil {
			switch e := v.Error.(type) {
			case string:
				msg = e
			case map[string]any: // {"error":{"message":…}}
				msg, _ = e["message"].(string)
			}
			if msg == "" {
				msg = v.Message
			}
		}
	} else if len(body) > 0 && body[0] != '<' {
		line, _, _ := strings.Cut(string(body), "\n")
		msg = strings.TrimSpace(line)
	}
	if msg == "" {
		msg = strings.ToLower(http.StatusText(status))
	}
	if len(msg) > maxDenyMessage {
		msg = msg[:maxDenyMessage]
	}
	return msg
}

// grpcWriterOf finds the grpcWriter under w's wrappers, or nil.
func grpcWriterOf(w http.ResponseWriter) *grpcWriter {
	for w != nil {
		if g, ok := w.(*grpcWriter); ok {
			return g
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
	return nil
}

// h2Upstream reports whether res's upstream protocol speaks HTTP/2.
func h2Upstream(res *Resolved) bool {
	switch effectiveUpstream(res) {
	case proto.UpstreamH2C, proto.UpstreamH2:
		return true
	}
	return false
}

// prepareUpstream readies r for the forward once the access check passed:
// the idle timeout for ordinary requests, the deadline for gRPC calls (H8).
// A native gRPC call to a service without an HTTP/2 upstream is answered
// here (H4), and ok is false. release must be called when the request is
// done, also when ok is false.
func (p *Proxy) prepareUpstream(w http.ResponseWriter, r *http.Request, res *Resolved) (_ *http.Request, release func(), ok bool) {
	release = func() {}
	cls := grpcwire.Classify(r)
	if !cls.Streaming() {
		// v0.5.1 P2.3: the per-request idle timeout when configured. It does
		// not apply to gRPC-like calls, which may stream for as long as they
		// like (H8).
		if p.idleTimeout > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), p.idleTimeout)
			return r.WithContext(ctx), cancel, true
		}
		return r, release, true
	}
	if cls == grpcwire.ClassGRPC && !h2Upstream(res) {
		grpcwire.WriteTrailersOnly(w, r.Header.Get("Content-Type"), grpcwire.CodeUnavailable, msgHTTP1Upstream)
		return r, release, false
	}
	if !cls.GRPCShaped() {
		return r, release, true
	}
	d, ok := callTimeout(r.Header, res.GRPCMaxDuration)
	if !ok {
		// No grpc-timeout and no cap: no relay deadline. Dead peers are
		// found by HTTP/2 pings and the tunnel's heartbeat.
		return r, release, true
	}
	ctx, cancel := context.WithTimeout(r.Context(), d+grpcDeadlineSlack)
	return r.WithContext(ctx), cancel, true
}

// callTimeout is a call's timeout from its grpc-timeout header and the
// service's cap (0 = none). A missing, malformed or longer grpc-timeout is
// rewritten down to the cap in h, so the upstream knows the deadline too; a
// valid one within the cap is left as it is. ok is false when the call has
// neither.
func callTimeout(h http.Header, maxDuration time.Duration) (time.Duration, bool) {
	d, err := grpcwire.ParseTimeout(h.Get("Grpc-Timeout"))
	has := h.Get("Grpc-Timeout") != "" && err == nil
	if maxDuration > 0 && (!has || d > maxDuration) {
		h.Set("Grpc-Timeout", grpcwire.FormatTimeout(maxDuration))
		return maxDuration, true
	}
	return d, has
}

// answerDeadline answers a gRPC call whose relay deadline passed before the
// upstream answered with DEADLINE_EXCEEDED (H8), and reports whether it did:
// false for any other request or error.
func answerDeadline(w http.ResponseWriter, r *http.Request, err error) bool {
	if !grpcwire.Classify(r).GRPCShaped() {
		return false
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(r.Context().Err(), context.DeadlineExceeded) {
		return false
	}
	grpcwire.WriteTrailersOnly(w, r.Header.Get("Content-Type"), grpcwire.CodeDeadlineExceeded, msgDeadline)
	return true
}

// grpcOutcome is what is recorded of a finished request besides its status:
// the protocol, and for a gRPC call its method and grpc-status (H9).
type grpcOutcome struct {
	protocol string
	call     bool // a grpc or grpc-web call
	method   string
	status   *int
}

// grpcErrorCode reports whether c promotes a call's row to closed_error, as a 5xx does (H9).
func grpcErrorCode(c grpcwire.Code) bool {
	switch c {
	case grpcwire.CodeUnknown, grpcwire.CodeDeadlineExceeded, grpcwire.CodeInternal,
		grpcwire.CodeUnavailable, grpcwire.CodeDataLoss:
		return true
	}
	return false
}

// grpcResult reads the outcome of a finished request from its writer: the
// code of a refusal the grpcWriter holds, else grpc-status from the headers
// of a trailers-only reply or from the trailers (announced or set with
// http.TrailerPrefix), which the reverse proxy put in the same map. It
// updates the log status: an error code promotes it to closed_error, and a
// call without any grpc-status is closed_error with ReasonGRPCNoStatus. A
// refusal stays rejected.
func (p *Proxy) grpcResult(r *http.Request, res *Resolved, started time.Time, ww *countingResponseWriter, status *string, ost *obsState) grpcOutcome {
	out := grpcOutcome{protocol: "http/1.1"}
	if r.ProtoMajor == 2 {
		out.protocol = "h2"
	}
	cls := grpcwire.Classify(r)
	if !cls.GRPCShaped() {
		return out
	}
	out.protocol, out.call, out.method = cls.String(), true, r.URL.Path
	code, ok := grpcwire.Code(0), false
	if g := grpcWriterOf(ww.ResponseWriter); g != nil {
		code, ok = g.heldCode()
	}
	if !ok && !ww.hijacked {
		code, ok = grpcwire.Status(ww.Header())
	}
	switch {
	case ok:
		c := int(code)
		out.status = &c
		if *status != StatusRejected && grpcErrorCode(code) {
			*status = StatusClosedError
		}
	case *status != StatusRejected:
		*status = StatusClosedError
		if ost.reason == "" {
			ost.reason = ReasonGRPCNoStatus
		}
	}
	codeText := "none"
	if ok {
		codeText = code.String()
	}
	p.log.Debug("grpc call", "service_id", res.ServiceID, "method", proto.SummaryPath(out.method),
		"code", codeText, "duration", time.Since(started).String())
	return out
}
