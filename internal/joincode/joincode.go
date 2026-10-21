// Package joincode generates, normalizes and hashes group join codes.
//
// A code is 12 random characters from the Crockford base32 alphabet (60 bits),
// displayed as ABCD-EFGH-JKMN. It is a bearer secret: the database stores only
// Hash(normalized code), and the plaintext is shown once at creation.
//
// A plain unsalted SHA-256 is appropriate here because the input is a high
// entropy random value, not a human-chosen password. Brute force is bounded by
// the entropy, a short expiry, a use limit and the per-user rate limit on
// redemption, not by hash cost. If the table leaked, 60 bits is still not
// practical to enumerate before a code expires, but it is not "unguessable
// forever"; the README should say so.
package joincode

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
)

const (
	alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32: no I, L, O, U
	length   = 12
)

// ErrInvalid is returned by Normalize for anything that cannot be a code.
var ErrInvalid = errors.New("invalid join code format")

// Generate returns a new random code in normalized form (12 characters, no hyphens).
func Generate() string {
	var raw [length]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("joincode: crypto/rand failed: " + err.Error()) // not recoverable
	}
	var b strings.Builder
	for _, x := range raw {
		b.WriteByte(alphabet[x&31]) // 256 % 32 == 0, so no modulo bias
	}
	return b.String()
}

// Display formats a normalized code for humans: ABCD-EFGH-JKMN.
func Display(normalized string) string {
	return normalized[0:4] + "-" + normalized[4:8] + "-" + normalized[8:12]
}

// Normalize canonicalizes user input: case-insensitive, ignores hyphens and
// spaces, and applies Crockford's look-alike mapping (O->0, I and L->1).
func Normalize(s string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch r {
		case '-', ' ':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if r > 127 || !strings.ContainsRune(alphabet, r) {
			return "", ErrInvalid
		}
		b.WriteRune(r)
	}
	if b.Len() != length {
		return "", ErrInvalid
	}
	return b.String(), nil
}

// Hash is what gets stored and looked up. Pass a normalized code.
func Hash(normalized string) []byte {
	h := sha256.Sum256([]byte(normalized))
	return h[:]
}
