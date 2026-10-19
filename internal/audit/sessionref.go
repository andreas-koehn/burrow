package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// SessionRef is the public, non-secret name of a browser session: a hash of
// the session id, which is the value of the session cookie. The sessions
// API, the re-authentication windows and the audit rows name a session by it,
// so that nothing which can be read from them is a credential.
func SessionRef(sid string) string {
	sum := sha256.Sum256([]byte("burrow-session-ref\x00" + sid))
	return hex.EncodeToString(sum[:16])
}

// SessionRefCutoffKey is the settings key holding the audit id (a ULID) that
// the relay minted the first time it ran a version that stores session
// subjects hashed. Rows with a smaller id predate that.
const SessionRefCutoffKey = "audit.session_ref_cutoff"

// EnsureSessionRefCutoff returns the cutoff, minting and storing it on the
// first call. It must run at start-up, before any session row is written.
func EnsureSessionRefCutoff(ctx context.Context, ss SettingsStore) (string, error) {
	m, err := ss.GetSettings(ctx)
	if err != nil {
		return "", fmt.Errorf("audit: load settings: %w", err)
	}
	if c := m[SessionRefCutoffKey]; c != "" {
		return c, nil
	}
	c, err := NewULID()
	if err != nil {
		return "", err
	}
	if err := ss.SaveSettings(ctx, map[string]string{SessionRefCutoffKey: c}); err != nil {
		return "", fmt.Errorf("audit: persist session-ref cutoff: %w", err)
	}
	return c, nil
}

// RedactSubject is what a reader of the audit log may see as the subject of a
// row. Relays before the fix stored the raw session id (a uuid, the value of
// the session cookie) as the subject of session.create and session.delete
// rows; those come back as SessionRef. Only rows older than the cutoff count
// as such (a row written later with a uuid-shaped subject is shown as it is
// and cannot claim the export's verification skip); an empty cutoff, which
// only a relay that never ran the start-up step has, treats every such row as
// old, the safe side for the secret. The stored row and its chain hash are
// not touched: this runs on the way out only, and redacted reports whether it
// changed anything.
func RedactSubject(action, subjectID, rowID, cutoff string) (out string, redacted bool) {
	if (action == ActionSessionCreate || action == ActionSessionDelete) &&
		(cutoff == "" || rowID < cutoff) && looksLikeUUID(subjectID) {
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
