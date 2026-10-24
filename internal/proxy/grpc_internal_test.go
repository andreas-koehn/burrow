package proxy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The grpc-message of a refusal is cut on a rune boundary.
func TestDenyMessage_CutIsUTF8Safe(t *testing.T) {
	long := strings.Repeat("a", maxDenyMessage-1) + "é and more"
	for _, body := range []string{`{"error":"` + long + `"}`, long} {
		msg := denyMessage(503, []byte(body))
		if !utf8.ValidString(msg) || len(msg) > maxDenyMessage {
			t.Fatalf("message %q (%d bytes) is not a valid cut", msg, len(msg))
		}
		if msg != strings.Repeat("a", maxDenyMessage-1) {
			t.Fatalf("cut at %d bytes, want before the é", len(msg))
		}
	}
}
