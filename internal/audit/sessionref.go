package audit

import (
	"crypto/sha256"
	"encoding/hex"
)

// SessionRef is the public, non-secret name of a browser session: a hash of
// the session id, which is the value of the session cookie. The sessions
// API, the re-authentication windows and the audit rows name a session by it,
// so that nothing which can be read from them is a credential.
func SessionRef(sid string) string {
	sum := sha256.Sum256([]byte("burrow-session-ref\x00" + sid))
	return hex.EncodeToString(sum[:16])
}

// RedactSubject is what a reader of the audit log may see as the subject of a
// row. Relays before the fix stored the raw session id (a uuid, the value of
// the session cookie) as the subject of session.create and session.delete
// rows; those come back as SessionRef. The stored row and its chain hash are
// not touched: this runs on the way out only, and redacted reports whether it
// changed anything.
func RedactSubject(action, subjectID string) (out string, redacted bool) {
	if (action == ActionSessionCreate || action == ActionSessionDelete) && looksLikeUUID(subjectID) {
		return SessionRef(subjectID), true
	}
	return subjectID, false
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
