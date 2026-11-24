package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func TestNewKeysShape(t *testing.T) {
	k, err := NewKeys()
	if err != nil {
		t.Fatalf("NewKeys: %v", err)
	}
	// base64url of 32 bytes without padding is 43 characters.
	if len(k.EditorKey) != 43 || len(k.ViewerKey) != 43 {
		t.Fatalf("key lengths = %d/%d, want 43", len(k.EditorKey), len(k.ViewerKey))
	}
	if k.EditorKey == k.ViewerKey {
		t.Fatal("editor and viewer keys must differ")
	}
	if k.EditorHash != HashKey(k.EditorKey) {
		t.Fatal("editor hash does not match the key")
	}
	if k.ViewerHash != HashKey(k.ViewerKey) {
		t.Fatal("viewer hash does not match the key")
	}
	// A room key presents as viewer; the editor key's privilege is the separate
	// readonly-token endpoints, tested in TestIsEditorKey.
	role, ok := RoleForKey(k.EditorKey, k.EditorHash, k.ViewerHash)
	if role != "viewer" || !ok {
		t.Fatalf("editor key = (%q, %v), want a valid viewer credential", role, ok)
	}
}

func TestNewKeysAreUniquePerCall(t *testing.T) {
	a, _ := NewKeys()
	b, _ := NewKeys()
	if a.EditorKey == b.EditorKey || a.ViewerKey == b.ViewerKey {
		t.Fatal("two calls produced the same key; randomness is broken")
	}
}

func TestRoleForKey(t *testing.T) {
	k, _ := NewKeys()

	// Since docs/PERMISSIONS.md §2.1 a key never grants edit: the editor key is an
	// invitation credential and everyone entering through a key starts as a
	// viewer. ok still reports key validity, which the readonly-token endpoints
	// rely on.
	cases := []struct {
		name   string
		key    string
		want   string
		wantOK bool
	}{
		{"editor key presents as viewer", k.EditorKey, "viewer", true},
		{"viewer key", k.ViewerKey, "viewer", true},
		{"empty is viewer", "", "viewer", false},
		{"garbage is viewer", "not-a-key", "viewer", false},
		{"truncated key is viewer", k.EditorKey[:42], "viewer", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			role, ok := RoleForKey(c.key, k.EditorHash, k.ViewerHash)
			if role != c.want || ok != c.wantOK {
				t.Fatalf("RoleForKey = (%q, %v), want (%q, %v)", role, ok, c.want, c.wantOK)
			}
		})
	}
}

// TestIsEditorKey pins the one privilege the editor key still has: it is the
// credential that authorises readonly-token issuance (docs/PROTOCOL.md §1.3).
func TestIsEditorKey(t *testing.T) {
	k, _ := NewKeys()
	if !IsEditorKey(k.EditorKey, k.EditorHash) {
		t.Fatal("the real editor key was not recognised")
	}
	for _, bad := range []string{"", k.ViewerKey, k.EditorKey[:42], "not-a-key"} {
		if IsEditorKey(bad, k.EditorHash) {
			t.Fatalf("IsEditorKey(%q) = true, want false", bad)
		}
	}
}

func TestTokenIssueAndVerify(t *testing.T) {
	s, err := NewTokenSigner(testSecret)
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

	token, claims, err := s.Issue("K7M2QX", time.Hour, now)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if claims.Room != "K7M2QX" || claims.Role != "viewer" {
		t.Fatalf("claims = %+v", claims)
	}
	if claims.ExpiresAt-claims.IssuedAt != int64(time.Hour/time.Millisecond) {
		t.Fatalf("ttl = %d ms", claims.ExpiresAt-claims.IssuedAt)
	}
	if claims.JTI == "" {
		t.Fatal("JTI must be set so two tokens in the same millisecond differ")
	}
	if strings.Count(token, ".") != 2 {
		t.Fatalf("token is not three dot-separated parts: %q", token)
	}

	got, err := s.Verify(token, "K7M2QX", 0, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.JTI != claims.JTI {
		t.Fatalf("JTI = %q, want %q", got.JTI, claims.JTI)
	}
}

func TestTokenVerifyFailures(t *testing.T) {
	s, _ := NewTokenSigner(testSecret)
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	token, _, _ := s.Issue("K7M2QX", time.Hour, now)

	cases := []struct {
		name    string
		token   string
		room    string
		revoked int64
		at      time.Time
		want    error
	}{
		{"wrong room", token, "ZZZZZZ", 0, now, ErrTokenRoomMismatch},
		{"expired", token, "K7M2QX", 0, now.Add(2 * time.Hour), ErrTokenExpired},
		{"revoked", token, "K7M2QX", now.UnixMilli() + 1, now, ErrTokenRevoked},
		{"garbage", "a.b.c", "K7M2QX", 0, now, ErrBadToken},
		{"two parts", "a.b", "K7M2QX", 0, now, ErrBadToken},
		{"empty", "", "K7M2QX", 0, now, ErrBadToken},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Verify(c.token, c.room, c.revoked, c.at)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestTokenTamperingIsRejected(t *testing.T) {
	s, _ := NewTokenSigner(testSecret)
	now := time.Now()
	token, _, _ := s.Issue("K7M2QX", time.Hour, now)

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("unexpected token shape")
	}

	t.Run("modified payload", func(t *testing.T) {
		// Swap a character in the payload; the signature no longer matches.
		payload := []byte(parts[1])
		if payload[0] == 'A' {
			payload[0] = 'B'
		} else {
			payload[0] = 'A'
		}
		forged := parts[0] + "." + string(payload) + "." + parts[2]
		if _, err := s.Verify(forged, "K7M2QX", 0, now); !errors.Is(err, ErrBadToken) {
			t.Fatalf("tampered payload accepted: %v", err)
		}
	})

	t.Run("different secret", func(t *testing.T) {
		other, _ := NewTokenSigner([]byte("ffffffffffffffffffffffffffffffff"))
		if _, err := other.Verify(token, "K7M2QX", 0, now); !errors.Is(err, ErrBadToken) {
			t.Fatalf("token verified under the wrong secret: %v", err)
		}
	})

	t.Run("wrong header", func(t *testing.T) {
		// A token from a different system (different typ) must not be accepted even
		// with a matching secret.
		forged := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." + parts[1] + "." + parts[2]
		if _, err := s.Verify(forged, "K7M2QX", 0, now); !errors.Is(err, ErrBadToken) {
			t.Fatalf("foreign header accepted: %v", err)
		}
	})
}

func TestWeakSecretRejected(t *testing.T) {
	for _, n := range []int{0, 1, 16, 31} {
		if _, err := NewTokenSigner(make([]byte, n)); !errors.Is(err, ErrWeakSecret) {
			t.Errorf("secret of %d bytes accepted: %v", n, err)
		}
	}
	if _, err := NewTokenSigner(make([]byte, 32)); err != nil {
		t.Errorf("32-byte secret rejected: %v", err)
	}
}

func TestIssuedTokensAreUnique(t *testing.T) {
	s, _ := NewTokenSigner(testSecret)
	now := time.Now()
	a, _, _ := s.Issue("K7M2QX", time.Hour, now)
	b, _, _ := s.Issue("K7M2QX", time.Hour, now)
	if a == b {
		t.Fatal("two tokens issued in the same millisecond are identical; JTI is not random")
	}
}

func TestSignerCopiesSecret(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	s, _ := NewTokenSigner(secret)
	token, _, _ := s.Issue("K7M2QX", time.Hour, time.Now())
	// Mutating the caller's slice must not change the signer's key.
	secret[0] = 'X'
	if _, err := s.Verify(token, "K7M2QX", 0, time.Now()); err != nil {
		t.Fatalf("mutating the caller's secret broke the signer: %v", err)
	}
}

func TestTokenCannotEscalateToEditor(t *testing.T) {
	// A hand-crafted payload claiming role=editor must be refused even with a valid
	// signature, so a leaked secret cannot be used to mint edit credentials.
	s, _ := NewTokenSigner(testSecret)
	now := time.Now()
	claims := TokenClaims{
		Room: "K7M2QX", Role: "editor",
		IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(time.Hour).UnixMilli(), JTI: "x",
	}
	token := signRaw(t, s, claims)
	if _, err := s.Verify(token, "K7M2QX", 0, now); !errors.Is(err, ErrBadToken) {
		t.Fatalf("an editor token was accepted: %v", err)
	}
}
