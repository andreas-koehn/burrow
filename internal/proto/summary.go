package proto

import (
	"unicode"
	"unicode/utf8"
)

const (
	// MaxSummaryPath is the longest path of a request summary, in bytes.
	MaxSummaryPath = 256
	// MaxSummaryMethod is the longest method of a request summary, in bytes.
	MaxSummaryMethod = 16
)

// SummaryPath makes the path of a request fit to be sent in a summary and to
// be printed on a terminal: at most MaxSummaryPath bytes, cut between two
// characters, and "?" in place of every control character, every formatting
// character (the ones that turn the writing direction, the ones without
// width), every line or paragraph separator and every byte that is not UTF-8.
// An empty path is "/". The relay applies it before sending and the client
// again after receiving.
func SummaryPath(p string) string {
	if p == "" {
		return "/"
	}
	return summaryText(p, MaxSummaryPath, unprintable)
}

// SummaryMethod does the same for the method, which is a short word of
// letters, digits, "-" and "_": anything else in it becomes "?".
func SummaryMethod(m string) string {
	return summaryText(m, MaxSummaryMethod, func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_'
	})
}

func unprintable(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
}

// summaryText replaces what bad names and cuts the result at max bytes. A
// replacement is one byte and never longer than what it replaces.
func summaryText(s string, max int, bad func(rune) bool) string {
	clean := len(s) <= max
	for i := 0; clean && i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		clean = (r != utf8.RuneError || size > 1) && !bad(r)
		i += size
	}
	if clean {
		return s
	}
	out := make([]byte, 0, min(len(s), max))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 || bad(r) {
			if len(out)+1 > max {
				break
			}
			out = append(out, '?')
		} else {
			if len(out)+size > max {
				break
			}
			out = append(out, s[i:i+size]...)
		}
		i += size
	}
	return string(out)
}
