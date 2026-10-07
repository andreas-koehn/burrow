package audit

import (
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Crockford's base32 alphabet (no I L O U) — the alphabet specified by the
// canonical ULID spec (https://github.com/ulid/spec). Lex-sorted output is
// time-sorted because the timestamp is the high-order 48 bits.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulidMu serialises ULID generation so two events created in the same
// millisecond are still strictly increasing (we monotonically bump the low
// bytes — small enough collision domain that crypto/rand handles it in
// practice). The mutex also gates an internal "last ts" we use to keep
// in-millisecond ids monotone.
var (
	ulidMu     sync.Mutex
	ulidLastMs int64
	ulidLastB  [10]byte // random tail of the previous id
)

// NewULID returns a 26-character ULID for use as audit_events.id. Encoding
// is Crockford base32; ids generated in time order sort lexicographically
// in time order, which lets the chain walk simply ORDER BY id.
//
// Within a single millisecond, the random tail is treated as an 80-bit
// counter and incremented by 1 so two ids minted in the same instant are
// strictly increasing (a freshly-rolled random tail could collide with the
// previous random tail or sort backwards). This matches the canonical ULID
// "monotonic random" behaviour.
func NewULID() (string, error) {
	ms := time.Now().UTC().UnixMilli()
	if ms < 0 {
		return "", fmt.Errorf("ulid: negative timestamp")
	}

	ulidMu.Lock()
	defer ulidMu.Unlock()

	var tail [10]byte
	if ms == ulidLastMs {
		// Same millisecond — bump the previous tail by 1 to preserve
		// monotonicity. If it would overflow we advance ms by 1 instead
		// (rare; matches the canonical spec).
		tail = ulidLastB
		overflow := true
		for i := len(tail) - 1; i >= 0; i-- {
			tail[i]++
			if tail[i] != 0 {
				overflow = false
				break
			}
		}
		if overflow {
			ms++
			if _, err := rand.Read(tail[:]); err != nil {
				return "", fmt.Errorf("ulid: rand: %w", err)
			}
		}
	} else {
		if _, err := rand.Read(tail[:]); err != nil {
			return "", fmt.Errorf("ulid: rand: %w", err)
		}
	}
	ulidLastMs = ms
	ulidLastB = tail

	// Build a 16-byte payload: 6 bytes ms (big-endian) + 10 bytes tail.
	var raw [16]byte
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	copy(raw[6:], tail[:])

	return encodeCrockford(raw), nil
}

// encodeCrockford encodes 16 raw bytes as a 26-character Crockford base32
// string. The first char is always 0-7 because 128 bits / 5 = 25.6, i.e. the
// top character only uses 3 bits of the original 128.
func encodeCrockford(raw [16]byte) string {
	out := make([]byte, 26)
	// Top character: top 3 bits of byte 0.
	out[0] = crockford[(raw[0]&0xE0)>>5]
	out[1] = crockford[raw[0]&0x1F]
	out[2] = crockford[(raw[1]&0xF8)>>3]
	out[3] = crockford[((raw[1]&0x07)<<2)|((raw[2]&0xC0)>>6)]
	out[4] = crockford[(raw[2]&0x3E)>>1]
	out[5] = crockford[((raw[2]&0x01)<<4)|((raw[3]&0xF0)>>4)]
	out[6] = crockford[((raw[3]&0x0F)<<1)|((raw[4]&0x80)>>7)]
	out[7] = crockford[(raw[4]&0x7C)>>2]
	out[8] = crockford[((raw[4]&0x03)<<3)|((raw[5]&0xE0)>>5)]
	out[9] = crockford[raw[5]&0x1F]
	// Random 80-bit tail: bytes 6..15 → out[10..25].
	out[10] = crockford[(raw[6]&0xF8)>>3]
	out[11] = crockford[((raw[6]&0x07)<<2)|((raw[7]&0xC0)>>6)]
	out[12] = crockford[(raw[7]&0x3E)>>1]
	out[13] = crockford[((raw[7]&0x01)<<4)|((raw[8]&0xF0)>>4)]
	out[14] = crockford[((raw[8]&0x0F)<<1)|((raw[9]&0x80)>>7)]
	out[15] = crockford[(raw[9]&0x7C)>>2]
	out[16] = crockford[((raw[9]&0x03)<<3)|((raw[10]&0xE0)>>5)]
	out[17] = crockford[raw[10]&0x1F]
	out[18] = crockford[(raw[11]&0xF8)>>3]
	out[19] = crockford[((raw[11]&0x07)<<2)|((raw[12]&0xC0)>>6)]
	out[20] = crockford[(raw[12]&0x3E)>>1]
	out[21] = crockford[((raw[12]&0x01)<<4)|((raw[13]&0xF0)>>4)]
	out[22] = crockford[((raw[13]&0x0F)<<1)|((raw[14]&0x80)>>7)]
	out[23] = crockford[(raw[14]&0x7C)>>2]
	out[24] = crockford[((raw[14]&0x03)<<3)|((raw[15]&0xE0)>>5)]
	out[25] = crockford[raw[15]&0x1F]
	return string(out)
}

// ulidAfter returns the ULID that follows prev: prev plus one, read as a
// number. ok is false when prev is not a ULID or is the last one there is.
func ulidAfter(prev string) (next string, ok bool) {
	if len(prev) != 26 || prev[0] > '7' {
		return "", false
	}
	out := []byte(prev)
	carry := true
	for i := len(out) - 1; i >= 0; i-- {
		d := strings.IndexByte(crockford, out[i])
		if d < 0 {
			return "", false
		}
		if carry {
			d++
			if carry = d == len(crockford); carry {
				d = 0
			}
			out[i] = crockford[d]
		}
	}
	if carry || out[0] > '7' {
		return "", false
	}
	return string(out), true
}

// nextULID returns a new id that sorts after prev, the id of the row the new
// one chains to. NewULID alone is ahead of prev whenever this process wrote
// it and the clock did not go back; when prev came from another instance
// whose clock is ahead, or from the same millisecond there, the new id is
// the one right after prev. A prev that is not a ULID (ids are only ever set
// by hand in tests) is not taken into account.
func nextULID(prev string) (string, error) {
	id, err := NewULID()
	if err != nil {
		return "", err
	}
	if prev == "" || id > prev {
		return id, nil
	}
	if next, ok := ulidAfter(prev); ok {
		return next, nil
	}
	return id, nil
}
