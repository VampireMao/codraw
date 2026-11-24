package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// signRaw builds a correctly signed token from arbitrary claims. It exists to prove
// that signature validity is never sufficient on its own: the claims are still
// checked (role, room, expiry, revocation).
func signRaw(t *testing.T, s *TokenSigner, claims TokenClaims) string {
	t.Helper()
	blob, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	input := tokenHeader + "." + base64.RawURLEncoding.EncodeToString(blob)
	return input + "." + s.sign(input)
}
