package ir

import (
	"bytes"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// This file holds what the stream decoders share: reading the text of a
// delta without losing the half of a character a provider cut in two, and
// putting the halves together again.

// StringBytes reads a JSON value that should be a string and keeps the
// bytes as they are (absent and null give nothing; ok is false for a value
// of another type): where encoding/json would turn each byte that is
// not UTF-8 into U+FFFD, the half of a character that a provider cut in two
// stays a half, so that Whole and Complete can join it with the other.
func StringBytes(raw []byte) (b []byte, ok bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, false
	}
	return unquote(raw[1 : len(raw)-1])
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

// Whole joins the bytes of a text delta with what the previous delta of the
// same part left over (see Complete) and returns the text that is complete.
// Bytes that are no UTF-8 become U+FFFD, as they do in a complete answer.
func Whole(carry *[]byte, b []byte) string {
	b = Complete(carry, b)
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "\ufffd")
}

// Complete joins the bytes of a delta with what the previous delta of the
// same part left over, and returns the bytes that are complete, as they are.
// The first bytes of a character at the very end — a UTF-8 sequence that is
// not finished, or the first half of a surrogate pair — are kept in *carry
// for the next delta.
func Complete(carry *[]byte, b []byte) []byte {
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
