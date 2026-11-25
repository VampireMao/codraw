package auth

import (
	"strings"
	"testing"
	"time"
)

func newTestSigner(t *testing.T) *TokenSigner {
	t.Helper()
	s, err := NewTokenSigner([]byte("session-test-secret-at-least-32-bytes!"))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return s
}

func TestSessionRoundTrip(t *testing.T) {
	s := newTestSigner(t)
	now := time.UnixMilli(1_800_000_000_000)

	token, claims, err := s.IssueSession("ABC123", "c_owner", time.Hour, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if claims.Room != "ABC123" || claims.ClientID != "c_owner" {
		t.Fatalf("claims = %+v", claims)
	}
	got, err := s.VerifySession(token, "ABC123", now.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.ClientID != "c_owner" {
		t.Fatalf("cid = %q", got.ClientID)
	}
}

func TestSessionRejectsTampering(t *testing.T) {
	s := newTestSigner(t)
	now := time.UnixMilli(1_800_000_000_000)
	token, _, _ := s.IssueSession("ABC123", "c_owner", time.Hour, now)

	// Flip one character in the payload: the signature must fail.
	parts := strings.Split(token, ".")
	body := []byte(parts[1])
	body[len(body)/2] ^= 1
	parts[1] = string(body)
	if _, err := s.VerifySession(strings.Join(parts, "."), "ABC123", now); err == nil {
		t.Fatal("tampered token accepted")
	}
}

func TestSessionBoundToRoom(t *testing.T) {
	s := newTestSigner(t)
	now := time.UnixMilli(1_800_000_000_000)
	token, _, _ := s.IssueSession("ABC123", "c_owner", time.Hour, now)
	if _, err := s.VerifySession(token, "ZZZ999", now); err == nil {
		t.Fatal("session accepted for another room")
	}
}

func TestSessionExpires(t *testing.T) {
	s := newTestSigner(t)
	now := time.UnixMilli(1_800_000_000_000)
	token, _, _ := s.IssueSession("ABC123", "c_owner", time.Minute, now)
	if _, err := s.VerifySession(token, "ABC123", now.Add(2*time.Minute)); err == nil {
		t.Fatal("expired session accepted")
	}
}

func TestSessionAndReadonlyTokensAreNotInterchangeable(t *testing.T) {
	s := newTestSigner(t)
	now := time.UnixMilli(1_800_000_000_000)

	session, _, err := s.IssueSession("ABC123", "c_owner", time.Hour, now)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	if _, err := s.Verify(session, "ABC123", 0, now); err == nil {
		t.Fatal("session token accepted as readonly token")
	}

	readonly, _, err := s.Issue("ABC123", time.Hour, now)
	if err != nil {
		t.Fatalf("issue readonly: %v", err)
	}
	if _, err := s.VerifySession(readonly, "ABC123", now); err == nil {
		t.Fatal("readonly token accepted as session token")
	}
}
