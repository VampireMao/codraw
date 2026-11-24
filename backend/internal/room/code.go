// Package room contains the room aggregate: its identity (code), lifecycle state
// and the in-memory coordination primitives.
package room

import (
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
)

// CodeAlphabet is the 31 character room code alphabet from docs/PROTOCOL.md §0:
// 0/O/1/I/L are removed, so a code read off a screen or spoken aloud is
// unambiguous. 31^6 ≈ 8.87e8 combinations; with per-IP creation limits that is
// not brute-forceable.
//
// NOTE: docs/PROTOCOL.md §0 is the single source of truth. The DDL CHECK
// constraint `^[2-9A-HJKMNP-Z]{6}$` must match this constant; there is a test
// that asserts the regex accepts every generated code.
const CodeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// CodeLength is the fixed room code length.
const CodeLength = 6

// ErrInvalidCode is returned by Normalize when the input cannot be a room code.
var ErrInvalidCode = errors.New("room: invalid room code")

// Normalize canonicalises user input into the stored representation: it strips
// the cosmetic "ABC-DEF" grouping dash and optional surrounding whitespace,
// upper-cases ASCII letters, then validates the character set and length.
//
// Room codes are matched case-insensitively (docs/PROTOCOL.md §0), but they are
// always stored and echoed upper-case, so every lookup goes through this
// function first.
func Normalize(in string) (string, error) {
	s := strings.TrimSpace(in)
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ToUpper(s)
	if len(s) != CodeLength {
		return "", ErrInvalidCode
	}
	for _, r := range s {
		if !strings.ContainsRune(CodeAlphabet, r) {
			return "", ErrInvalidCode
		}
	}
	return s, nil
}

// NormalizeCode is Normalize under the name the transport layer uses, so the
// call site reads as a validation rather than a transformation.
func NormalizeCode(in string) (string, error) { return Normalize(in) }

// IsValid reports whether s is already a normalised room code.
func IsValid(s string) bool {
	if len(s) != CodeLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(CodeAlphabet, rune(s[i])) {
			return false
		}
	}
	return true
}

// Generate returns a new random room code drawn uniformly from CodeAlphabet.
//
// crypto/rand is used rather than math/rand because a guessable code is an
// authorisation bypass: the code alone identifies the room. The alphabet length
// is 31, which is not a power of two, so rand.Int is used to reject the biased
// tail rather than taking a modulo of a random byte.
func Generate() (string, error) {
	maximum := big.NewInt(int64(len(CodeAlphabet)))
	out := make([]byte, CodeLength)
	for i := range out {
		n, err := rand.Int(rand.Reader, maximum)
		if err != nil {
			return "", err
		}
		out[i] = CodeAlphabet[n.Int64()]
	}
	return string(out), nil
}

// Display formats a code for humans with the "ABC-DEF" grouping used by the UI.
// It does not validate; callers have already normalised.
func Display(code string) string {
	if len(code) != CodeLength {
		return code
	}
	return code[:3] + "-" + code[3:]
}
