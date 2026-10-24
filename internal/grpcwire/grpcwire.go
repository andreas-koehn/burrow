// Package grpcwire holds the small, dependency-free pieces of the gRPC wire
// protocol the relay needs to treat gRPC calls as more than HTTP requests:
// telling a gRPC request from other traffic, the grpc-timeout header, the
// HTTP-to-gRPC status map, trailers-only error replies, and reading
// grpc-status from a response (HTTP/2, gRPC and SNI on 443, H5 to H9).
//
// It uses only the standard library.
package grpcwire

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Class is what Classify says about a request.
type Class int

// The classes of request.
const (
	ClassNone Class = iota
	// ClassGRPC: content type application/grpc or application/grpc+….
	ClassGRPC
	// ClassGRPCWeb: content type application/grpc-web….
	ClassGRPCWeb
	// ClassConnectStream: content type application/connect+… (Connect
	// streaming). It is streamed like gRPC but gets no gRPC-shaped denials.
	ClassConnectStream
)

func (c Class) String() string {
	switch c {
	case ClassGRPC:
		return "grpc"
	case ClassGRPCWeb:
		return "grpc-web"
	case ClassConnectStream:
		return "connect-stream"
	}
	return "none"
}

// GRPCShaped reports whether refusals to a request of this class are
// answered as a gRPC status (H7): grpc and grpc-web only.
func (c Class) GRPCShaped() bool { return c == ClassGRPC || c == ClassGRPCWeb }

// Streaming reports whether requests of this class take the unbuffered path
// (H6): every class but none.
func (c Class) Streaming() bool { return c != ClassNone }

// Classify names the kind of RPC call r carries, from its Content-Type.
func Classify(r *http.Request) Class {
	return ClassifyContentType(r.Header.Get("Content-Type"))
}

// ClassifyContentType is Classify for a Content-Type value.
func ClassifyContentType(ct string) Class {
	mt, _, _ := strings.Cut(ct, ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	switch {
	case strings.HasPrefix(mt, "application/grpc-web"):
		return ClassGRPCWeb
	case mt == "application/grpc" || strings.HasPrefix(mt, "application/grpc+"):
		return ClassGRPC
	case strings.HasPrefix(mt, "application/connect+"):
		return ClassConnectStream
	}
	return ClassNone
}

// Code is a gRPC status code.
type Code int

// The gRPC status codes.
const (
	CodeOK Code = iota
	CodeCanceled
	CodeUnknown
	CodeInvalidArgument
	CodeDeadlineExceeded
	CodeNotFound
	CodeAlreadyExists
	CodePermissionDenied
	CodeResourceExhausted
	CodeFailedPrecondition
	CodeAborted
	CodeOutOfRange
	CodeUnimplemented
	CodeInternal
	CodeUnavailable
	CodeDataLoss
	CodeUnauthenticated
)

var codeNames = [...]string{
	"OK", "CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED",
	"NOT_FOUND", "ALREADY_EXISTS", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED",
	"FAILED_PRECONDITION", "ABORTED", "OUT_OF_RANGE", "UNIMPLEMENTED",
	"INTERNAL", "UNAVAILABLE", "DATA_LOSS", "UNAUTHENTICATED",
}

func (c Code) String() string {
	if c >= 0 && int(c) < len(codeNames) {
		return codeNames[c]
	}
	return "CODE_" + strconv.Itoa(int(c))
}

// StatusForHTTP maps the HTTP status of a refusal to a gRPC code (H7), after
// the gRPC HTTP-to-gRPC mapping with one deliberate change: 429 is
// RESOURCE_EXHAUSTED, not UNAVAILABLE, because a rate limit is not a
// retryable outage.
func StatusForHTTP(status int) Code {
	switch status {
	case http.StatusBadRequest:
		return CodeInternal
	case http.StatusUnauthorized:
		return CodeUnauthenticated
	case http.StatusForbidden:
		return CodePermissionDenied
	case http.StatusNotFound:
		return CodeUnimplemented
	case http.StatusTooManyRequests:
		return CodeResourceExhausted
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return CodeUnavailable
	}
	return CodeUnknown
}

// ErrBadTimeout: a grpc-timeout value that is not 1 to 8 digits and a unit.
var ErrBadTimeout = errors.New("grpcwire: malformed grpc-timeout")

// ParseTimeout parses a grpc-timeout header value: 1 to 8 ASCII digits and
// one unit of H (hours), M (minutes), S (seconds), m (milliseconds), u
// (microseconds) or n (nanoseconds). A value too large for a time.Duration
// is clamped to the largest one.
func ParseTimeout(s string) (time.Duration, error) {
	if len(s) < 2 || len(s) > 9 {
		return 0, ErrBadTimeout
	}
	var unit time.Duration
	switch s[len(s)-1] {
	case 'H':
		unit = time.Hour
	case 'M':
		unit = time.Minute
	case 'S':
		unit = time.Second
	case 'm':
		unit = time.Millisecond
	case 'u':
		unit = time.Microsecond
	case 'n':
		unit = time.Nanosecond
	default:
		return 0, ErrBadTimeout
	}
	var n int64
	for _, c := range []byte(s[:len(s)-1]) {
		if c < '0' || c > '9' {
			return 0, ErrBadTimeout
		}
		n = n*10 + int64(c-'0')
	}
	if n > math.MaxInt64/int64(unit) {
		return math.MaxInt64, nil
	}
	return time.Duration(n) * unit, nil
}

const maxTimeoutValue = 99999999

// FormatTimeout formats d as a grpc-timeout value of at most 8 digits, in the
// finest unit that fits and rounding up, so the result is never shorter than
// d. Zero and negative durations give "0n". A duration beyond 99999999 hours
// is clamped to that.
func FormatTimeout(d time.Duration) string {
	if d <= 0 {
		return "0n"
	}
	units := []struct {
		d time.Duration
		c byte
	}{
		{time.Nanosecond, 'n'}, {time.Microsecond, 'u'}, {time.Millisecond, 'm'},
		{time.Second, 'S'}, {time.Minute, 'M'}, {time.Hour, 'H'},
	}
	for _, u := range units {
		n := int64(d / u.d)
		if d%u.d != 0 {
			n++
		}
		if n <= maxTimeoutValue {
			return strconv.FormatInt(n, 10) + string(u.c)
		}
	}
	return strconv.Itoa(maxTimeoutValue) + "H"
}

const upperHex = "0123456789ABCDEF"

// EncodeMessage percent-encodes a grpc-message value: every byte outside
// 0x20 to 0x7E, and '%' itself, becomes %XX (upper case hex).
func EncodeMessage(s string) string {
	need := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c > 0x7e || c == '%' {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e || c == '%' {
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&15])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// DecodeMessage reverses EncodeMessage. A '%' that does not start two hex
// digits is kept as it is.
func DecodeMessage(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

// WriteTrailersOnly answers a call with a trailers-only response: HTTP 200,
// the content type (application/grpc when empty), and grpc-status and
// grpc-message as headers, with no body. Other headers already set, such as
// Retry-After, are kept; headers that described a body, and an earlier
// grpc-status or grpc-message, are removed, and so are trailers set with
// http.TrailerPrefix: a trailers-only reply has no trailers.
func WriteTrailersOnly(w http.ResponseWriter, contentType string, code Code, message string) {
	h := w.Header()
	for _, k := range []string{"Content-Length", "Content-Encoding", "Content-Range", "Etag", "Trailer", "Grpc-Status", "Grpc-Message", "Grpc-Status-Details-Bin"} {
		h.Del(k)
	}
	for k := range h {
		if hasTrailerPrefix(k) {
			delete(h, k)
		}
	}
	if contentType == "" {
		contentType = "application/grpc"
	}
	h.Set("Content-Type", contentType)
	h.Set("Grpc-Status", strconv.Itoa(int(code)))
	if message != "" {
		h.Set("Grpc-Message", EncodeMessage(message))
	}
	w.WriteHeader(http.StatusOK)
}

// Status reads grpc-status from h, which may be response headers
// (trailers-only), a trailer map, or a ResponseWriter's header map in which
// trailers set after the headers went out carry http.TrailerPrefix. ok is
// false when there is none or it is not a number; a number beyond the known
// codes is CodeUnknown.
func Status(h http.Header) (Code, bool) {
	if h == nil {
		return 0, false
	}
	for _, k := range []string{"Grpc-Status", http.TrailerPrefix + "Grpc-Status"} {
		if v := lookup(h, k); len(v) > 0 {
			if c, ok := parseCode(v[0]); ok {
				return c, true
			}
		}
	}
	return 0, false
}

// lookup is h[k], and for a key with http.TrailerPrefix also any key that
// differs from it in case only: the name after the prefix is whatever the
// handler wrote, canonical or not.
func lookup(h http.Header, k string) []string {
	if v, ok := h[k]; ok || !hasTrailerPrefix(k) {
		return v
	}
	for hk, v := range h {
		if hasTrailerPrefix(hk) && strings.EqualFold(hk, k) {
			return v
		}
	}
	return nil
}

// hasTrailerPrefix reports whether k starts with http.TrailerPrefix, in any
// case.
func hasTrailerPrefix(k string) bool {
	return len(k) >= len(http.TrailerPrefix) && strings.EqualFold(k[:len(http.TrailerPrefix)], http.TrailerPrefix)
}

func parseCode(s string) (Code, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, false
	}
	if n >= len(codeNames) {
		return CodeUnknown, true
	}
	return Code(n), true
}

// ResponseStatus is the status of a finished response: the headers first (a
// trailers-only reply), then the trailers. A header that holds no valid
// grpc-status does not hide a valid trailer.
func ResponseStatus(header, trailer http.Header) (Code, bool) {
	if c, ok := Status(header); ok {
		return c, true
	}
	return Status(trailer)
}

// Message reads and decodes grpc-message from h, as Status reads grpc-status.
func Message(h http.Header) string {
	for _, k := range []string{"Grpc-Message", http.TrailerPrefix + "Grpc-Message"} {
		if v := lookup(h, k); len(v) > 0 {
			return DecodeMessage(v[0])
		}
	}
	return ""
}
