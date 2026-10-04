package auth

import (
	"crypto/rand"
	"math/big"
	"regexp"
)

// slugAlphabet is the 32-character alphabet for generated slugs.
// Ambiguous characters l, o, 0, and 1 are excluded to prevent confusion.
const slugAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

// SlugRule is the human-readable form of the slug format, used in API errors.
const SlugRule = "slug must be 3-40 characters: lowercase letters, digits and hyphens, not starting or ending with a hyphen"

// slugRe is also a valid DNS label, so a slug can serve as a subdomain if
// host routing is ever switched back on.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

// ValidSlug reports whether s is an acceptable service or provider slug.
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

// GenerateSlug returns a random 6-character slug drawn uniformly from
// slugAlphabet. It is collision-unaware: the caller retries on a UNIQUE
// constraint failure when persisting it.
func GenerateSlug() (string, error) {
	n := big.NewInt(int64(len(slugAlphabet)))
	buf := make([]byte, 6)
	for i := range buf {
		idx, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		buf[i] = slugAlphabet[idx.Int64()]
	}
	return string(buf), nil
}
