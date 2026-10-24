package docs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"time"
)

// crockford is the ULID alphabet: base32 without I, L, O and U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns a new ULID: 48 bits of millisecond time and 80 random bits, 26 characters.
//
// The module takes ids from the client for every creation, which is what makes a creation safe
// to retry: the same id with the same content returns what was already created. So generate the
// id ONCE per intended creation and reuse it on every retry — a fresh id per attempt is a
// duplicate per attempt.
func NewID() string {
	return newULID(time.Now())
}

func newULID(t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand does not fail on the platforms Go supports; a panic beats an id that
		// silently collides.
		panic("docs: no randomness for a ULID: " + err.Error())
	}
	return encodeULID(b)
}

// encodeULID writes 128 bits as 26 base32 characters, most significant first. 26 × 5 is 130
// bits, so the first character carries only three — which is why a ULID never starts above 7.
func encodeULID(b [16]byte) string {
	hi := binary.BigEndian.Uint64(b[:8])
	lo := binary.BigEndian.Uint64(b[8:])
	var out [26]byte
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// ValidID reports whether s is a ULID as the module writes them: 26 upper-case Crockford
// characters. It is how a CLI tells an id from a path.
func ValidID(s string) bool {
	if len(s) != 26 || s[0] > '7' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(crockford, rune(s[i])) {
			return false
		}
	}
	return true
}

// Checksum is the module's checksum format: "sha256:" and the lower-case hex digest.
func Checksum(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}
