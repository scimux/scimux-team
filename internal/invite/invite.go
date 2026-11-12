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
	"errors"
	"io"
	"time"
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

// ErrFormat is returned by Parse for input that is not a join code.
var ErrFormat = errors.New("invalid join code")

// Generate draws EntropyBytes from r, or from crypto/rand when r is nil,
// and returns the normalised code: uppercase, no separators, length CodeLength.
func Generate(r io.Reader) (string, error) {
	return "", nil
}

// Format groups a normalised code by GroupSize. A wrong-length input is
// returned unchanged.
func Format(normalized string) string {
	return ""
}

// Parse accepts a join code and returns the normalised form.
func Parse(s string) (string, error) {
	return "", nil
}

// Verifier is the domain-separated hash of a normalised code.
func Verifier(normalized string) []byte {
	return nil
}

// CodeID is the domain-separated identifier of a verifier.
func CodeID(verifier []byte) string {
	return ""
}

func encode(src []byte) string {
	return ""
}
