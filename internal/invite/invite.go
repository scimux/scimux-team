// Package invite generates and parses scimux-team join codes.
//
// A code is 15 bytes from the caller's entropy, encoded as 24 Crockford
// Base32 characters with no padding. The alphabet omits I, L, O and U.
// Parse accepts lowercase, drops hyphens and spaces, and maps O to 0 and
// I or L to 1.
//
// Verifier and CodeID are domain-separated SHA-256 over the normalised
// code. The domain strings belong to scimux-team, so a code issued by
// scimux-rv does not verify here. There is no server pepper. The server
// stores the verifier and never the plaintext.
package invite

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"
)

// Alphabet is Crockford Base32 without the check symbol. I, L, O and U are
// omitted so a generated code cannot contain an ambiguous glyph. Parse maps
// O to 0 and I or L to 1.
const Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	// EntropyBytes is the number of bytes drawn per code. 15 bytes is 120
	// bits, which is exactly 24 alphabet characters and no remainder.
	EntropyBytes = 15

	// CodeLength is the number of alphabet characters in a normalised code.
	CodeLength = 24

	// GroupSize is the display grouping. The issued plaintext is six groups
	// of four, separated by hyphens.
	GroupSize = 4

	// CodeIDLen is the hex length of a code identifier.
	CodeIDLen = 32

	// CollisionRetries is how many times a caller redraws after an
	// identifier collision before failing.
	CollisionRetries = 8

	// UnusedTTL is how long an unused code remains redeemable. It is a
	// friction bound: the security is the entropy and one-time redemption.
	UnusedTTL = 30 * 24 * time.Hour
)

// Domain-separation prefixes. Verifier and CodeID insert a 0x00 byte
// between the prefix and the payload; that byte is not part of these strings.
const (
	verifierDomain = "scimux-team/join-verifier/v1"
	codeIDDomain   = "scimux-team/join-id/v1"
)

// ErrFormat is returned by Parse for input that is not a join code.
var ErrFormat = errors.New("invalid join code")

// Generate draws EntropyBytes from r, or from crypto/rand when r is nil,
// and returns the normalised code: uppercase, no separators, length CodeLength.
func Generate(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	buf := make([]byte, EntropyBytes)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return encode(buf), nil
}

// Format inserts hyphens every GroupSize characters. It is defined on
// already-normalised input only. A string of any other length is returned
// unchanged.
func Format(normalized string) string {
	if len(normalized) != CodeLength {
		return normalized
	}
	var b strings.Builder
	b.Grow(CodeLength + CodeLength/GroupSize - 1)
	for i := 0; i < len(normalized); i += GroupSize {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(normalized[i : i+GroupSize])
	}
	return b.String()
}

// Parse normalises a join code. It accepts lowercase, hyphens and spaces,
// and the look-alike letters O, I and L. On success it returns the canonical
// form, which is what Generate returns and what Verifier hashes.
func Parse(s string) (string, error) {
	var b strings.Builder
	b.Grow(CodeLength)
	for _, r := range s {
		switch r {
		case '-', ' ':
			continue
		case 'o', 'O':
			r = '0'
		case 'i', 'I', 'l', 'L':
			r = '1'
		default:
			r = unicode.ToUpper(r)
		}
		if strings.IndexRune(Alphabet, r) < 0 {
			return "", ErrFormat
		}
		b.WriteRune(r)
	}
	if b.Len() != CodeLength {
		return "", ErrFormat
	}
	return b.String(), nil
}

// Verifier is SHA-256(verifierDomain || 0x00 || normalised).
func Verifier(normalized string) []byte {
	h := sha256.New()
	h.Write([]byte(verifierDomain))
	h.Write([]byte{0})
	h.Write([]byte(normalized))
	return h.Sum(nil)
}

// CodeID is a stable non-secret identifier derived from the verifier,
// never from the plaintext. It is 32 lowercase hex characters.
func CodeID(verifier []byte) string {
	h := sha256.New()
	h.Write([]byte(codeIDDomain))
	h.Write([]byte{0})
	h.Write(verifier)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

// encode packs src into Crockford Base32, most significant bits first.
// EntropyBytes is an exact number of 5-bit characters. A shorter input,
// which production never draws, keeps the leftover bits as a final character.
func encode(src []byte) string {
	var acc uint64
	var bits uint
	out := make([]byte, 0, CodeLength)
	for _, b := range src {
		acc = (acc << 8) | uint64(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, Alphabet[(acc>>bits)&31])
		}
	}
	if bits > 0 {
		out = append(out, Alphabet[(acc<<(5-bits))&31])
	}
	return string(out)
}
