package assistantserver

import (
	"crypto/rand"
	"time"
)

// ULID alphabet (Crockford base32, letters I/L/O/U excluded) — the
// assistant-server audit contract keys every event by a 26-char ULID.
const ulidABC = ulidAlphabet("0123456789ABCDEFGHJKMNPQRSTVWXYZ")

type ulidAlphabet string

// indexOf returns the value of a Crockford character (parallels PHP fixtured
// checks), or -1 when the character is not in the alphabet.
func (a ulidAlphabet) indexOf(c byte) int {
	for i := 0; i < len(a); i++ {
		if a[i] == c {
			return i
		}
	}
	return -1
}

// NewULID returns a 26-character Crockford ULID: 48-bit millisecond epoch +
// 80-bit crypto/rand entropy. Monotonicity within the same millisecond is not
// guaranteed, matching the audit contract's tolerance for same-ms events.
func NewULID() string {
	var b [16]byte
	ms := time.Now().UnixMilli()
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	var r [10]byte
	if _, err := rand.Read(r[:]); err != nil {
		// crypto/rand failure is unrecoverable; fall back to unix-nano entropy.
		nano := time.Now().UnixNano()
		r[0] = byte(nano >> 56)
		r[1] = byte(nano >> 48)
		r[2] = byte(nano >> 40)
		r[3] = byte(nano >> 32)
		r[4] = byte(nano >> 24)
		r[5] = byte(nano >> 16)
		r[6] = byte(nano >> 8)
		r[7] = byte(nano)
		r[8] = byte(nano >> 16)
		r[9] = byte(nano)
	}

	id := make([]byte, 26)
	for i := 9; i >= 0; i-- {
		id[i] = ulidABC[ms&0x1F]
		ms >>= 5
	}
	// 80-bit random as a shift register: hi carries bits 79..16, lo bits 15..0.
	vHi := be(r[0:8])
	vLo := be(r[8:10])
	for k := 0; k < 16; k++ {
		id[10+k] = ulidABC[byte((vHi>>59)&0x1F)]
		vHi = vHi<<5 | vLo>>59
		vLo <<= 5
	}
	return string(id)
}

// be reads a big-endian unsigned integer of 8 or 2 bytes.
func be(b []byte) uint64 {
	var out uint64
	for _, c := range b {
		out = out<<8 | uint64(c)
	}
	return out
}
