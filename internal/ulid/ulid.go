// Package ulid generates ULIDs: 48-bit millisecond timestamps followed by
// 80 bits of randomness, encoded as 26 characters of Crockford base32.
// Lexicographic order of the strings matches creation order.
package ulid

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

const encoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	mu       sync.Mutex
	lastMS   uint64
	lastRand [10]byte
)

// New returns a new ULID for the current time.
func New() string { return newAt(time.Now()) }

// newAt returns a ULID stamped with t. Within the same millisecond the random
// component is incremented rather than redrawn, so IDs minted in a tight loop
// still sort in creation order.
func newAt(t time.Time) string {
	ms := uint64(t.UnixMilli())

	mu.Lock()
	var entropy [10]byte
	switch {
	case ms == lastMS:
		entropy = lastRand
		incr(&entropy)
	case ms < lastMS:
		// Clock moved backwards; keep monotonicity by reusing the last stamp.
		ms = lastMS
		entropy = lastRand
		incr(&entropy)
	default:
		if _, err := rand.Read(entropy[:]); err != nil {
			// crypto/rand does not fail on the supported platforms; fall back to
			// a timestamp-derived value rather than panicking in a library.
			binary.BigEndian.PutUint64(entropy[:8], uint64(t.UnixNano()))
		}
	}
	lastMS, lastRand = ms, entropy
	mu.Unlock()

	var b [16]byte
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	copy(b[6:], entropy[:])

	return encode(b)
}

func incr(e *[10]byte) {
	for i := len(e) - 1; i >= 0; i-- {
		e[i]++
		if e[i] != 0 {
			return
		}
	}
}

// encode writes the 128 bits of b as 26 base32 characters, 5 bits at a time
// (the leading character carries the top 2 bits, padded with zeroes).
func encode(b [16]byte) string {
	var out [26]byte
	bits := uint(0) // accumulator
	var acc uint32
	pos := 26
	for i := 15; i >= 0; i-- {
		acc |= uint32(b[i]) << bits
		bits += 8
		for bits >= 5 {
			pos--
			out[pos] = encoding[acc&0x1f]
			acc >>= 5
			bits -= 5
		}
	}
	if pos > 0 {
		pos--
		out[pos] = encoding[acc&0x1f]
	}
	return string(out[:])
}
