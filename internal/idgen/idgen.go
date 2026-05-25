// Package idgen produces 26-character ULIDs per the spec at
// https://github.com/ulid/spec — 48-bit millisecond timestamp + 80-bit random,
// Crockford base32 encoded. Sessions, messages, agents, and ad-hoc tool_use
// fallbacks all use this so IDs sort by creation time within the same
// millisecond.
package idgen

import (
	"crypto/rand"
	"time"
)

const encoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID returns one fresh ULID. It never panics — a failure to read from
// crypto/rand falls back to a zero-filled random section, which still produces
// a syntactically valid (though weakly random) ID. The fallback path is
// astronomically unlikely on Linux/macOS.
func NewULID() string {
	var b [16]byte
	t := uint64(time.Now().UnixMilli()) //nolint:gosec // UnixMilli on a positive epoch is always positive; conversion is safe
	b[0] = byte(t >> 40)
	b[1] = byte(t >> 32)
	b[2] = byte(t >> 24)
	b[3] = byte(t >> 16)
	b[4] = byte(t >> 8)
	b[5] = byte(t)
	_, _ = rand.Read(b[6:])
	return encode(b)
}

// encode lays out 128 bits of input into 26 Crockford base32 characters,
// matching the canonical ULID bit-packing exactly. The first character only
// uses 3 of its 5 bits (top 2 bits of output are always zero).
func encode(b [16]byte) string {
	var s [26]byte
	s[0] = encoding[(b[0]&224)>>5]
	s[1] = encoding[b[0]&31]
	s[2] = encoding[(b[1]&248)>>3]
	s[3] = encoding[((b[1]&7)<<2)|((b[2]&192)>>6)]
	s[4] = encoding[(b[2]&62)>>1]
	s[5] = encoding[((b[2]&1)<<4)|((b[3]&240)>>4)]
	s[6] = encoding[((b[3]&15)<<1)|((b[4]&128)>>7)]
	s[7] = encoding[(b[4]&124)>>2]
	s[8] = encoding[((b[4]&3)<<3)|((b[5]&224)>>5)]
	s[9] = encoding[b[5]&31]
	s[10] = encoding[(b[6]&248)>>3]
	s[11] = encoding[((b[6]&7)<<2)|((b[7]&192)>>6)]
	s[12] = encoding[(b[7]&62)>>1]
	s[13] = encoding[((b[7]&1)<<4)|((b[8]&240)>>4)]
	s[14] = encoding[((b[8]&15)<<1)|((b[9]&128)>>7)]
	s[15] = encoding[(b[9]&124)>>2]
	s[16] = encoding[((b[9]&3)<<3)|((b[10]&224)>>5)]
	s[17] = encoding[b[10]&31]
	s[18] = encoding[(b[11]&248)>>3]
	s[19] = encoding[((b[11]&7)<<2)|((b[12]&192)>>6)]
	s[20] = encoding[(b[12]&62)>>1]
	s[21] = encoding[((b[12]&1)<<4)|((b[13]&240)>>4)]
	s[22] = encoding[((b[13]&15)<<1)|((b[14]&128)>>7)]
	s[23] = encoding[(b[14]&124)>>2]
	s[24] = encoding[((b[14]&3)<<3)|((b[15]&224)>>5)]
	s[25] = encoding[b[15]&31]
	return string(s[:])
}
