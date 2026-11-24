// Package auth implements the two credential mechanisms in docs/PROTOCOL.md:
//
//   - Room keys (§0.1): create returns a 32-byte editorKey and viewerKey; the
//     server stores only sha256(key) and resolves a role by comparing hashes.
//   - Readonly share tokens (§1.7): a stateless HMAC-SHA256 token naming a room
//     and an issued-at time, revocable per room by bumping readonlyMinIssuedAt.
//
// Nothing here touches the network or the store: every function is pure so it can
// be tested exhaustively and reused by both the REST layer and the WebSocket
// handshake.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// KeyBytes is the entropy of a room key (32 bytes → 43 base64url chars).
const KeyBytes = 32

// Errors returned by this package.
var (
	// ErrBadKey means a presented key matched neither hash.
	ErrBadKey = errors.New("auth: bad key")
	// ErrBadToken means the readonly token is malformed or the signature failed.
	ErrBadToken = errors.New("auth: bad readonly token")
	// ErrTokenExpired means the token's exp is in the past.
	ErrTokenExpired = errors.New("auth: readonly token expired")
	// ErrTokenRevoked means the token predates the room's revocation watermark.
	ErrTokenRevoked = errors.New("auth: readonly token revoked")
	// ErrTokenRoomMismatch means the token was issued for a different room.
	ErrTokenRoomMismatch = errors.New("auth: readonly token is for another room")
	// ErrWeakSecret means the HMAC key is too short to be meaningful.
	ErrWeakSecret = errors.New("auth: READONLY_SECRET is too short")
)

// Keys are the two plaintext credentials handed out once at room creation.
//
// They are never persisted: the caller writes the hashes and returns the
// plaintext to the creator exactly once (docs/PROTOCOL.md §1.1).
type Keys struct {
	EditorKey string
	ViewerKey string
	// EditorHash / ViewerHash are what the store keeps.
	EditorHash [32]byte
	ViewerHash [32]byte
}

// NewKeys generates a fresh editor/viewer key pair.
func NewKeys() (Keys, error) {
	editor, err := randomKey()
	if err != nil {
		return Keys{}, err
	}
	viewer, err := randomKey()
	if err != nil {
		return Keys{}, err
	}
	// Both must be distinct; a collision would give every editor link to a viewer
	// and vice versa, so regenerate rather than accept the astronomically small
	// chance.
	if editor == viewer {
		return NewKeys()
	}
	return Keys{
		EditorKey:  editor,
		ViewerKey:  viewer,
		EditorHash: sha256.Sum256([]byte(editor)),
		ViewerHash: sha256.Sum256([]byte(viewer)),
	}, nil
}

// randomKey returns 32 bytes of base64url without padding.
func randomKey() (string, error) {
	var b [KeyBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashKey returns the sha256 of a presented key. Comparing hashes (not the raw
// strings) is what lets the store hold only digests.
func HashKey(key string) [32]byte { return sha256.Sum256([]byte(key)) }

// RoleForKey resolves the *view* role for a presented key against a room's
// hashes.
//
// Since docs/PERMISSIONS.md §2.1 the editor key no longer grants edit rights: it
// is an invitation credential, and everyone who enters through a key starts as a
// viewer until the owner grants editor. The role therefore no longer depends on
// the key; ok only reports whether the key is a valid room credential.
//
// The comparison is constant-time so a timing side channel cannot be used to
// recover a key byte by byte. An empty key, or one matching neither hash, is a
// viewer: the minimum-privilege default from §0.1.
func RoleForKey(presented string, editorHash, viewerHash [32]byte) (role string, ok bool) {
	if presented == "" {
		return "viewer", false
	}
	got := HashKey(presented)
	if subtle.ConstantTimeCompare(got[:], editorHash[:]) == 1 {
		return "viewer", true
	}
	if subtle.ConstantTimeCompare(got[:], viewerHash[:]) == 1 {
		return "viewer", true
	}
	return "viewer", false
}

// IsEditorKey reports whether presented matches the room's editor key.
//
// The editor key survives as the invitation credential that authorises the
// readonly-token endpoints (docs/PERMISSIONS.md §2.1, docs/PROTOCOL.md §1.3); it
// does not confer the editor role.
func IsEditorKey(presented string, editorHash [32]byte) bool {
	if presented == "" {
		return false
	}
	got := HashKey(presented)
	return subtle.ConstantTimeCompare(got[:], editorHash[:]) == 1
}

// --- Readonly tokens -----------------------------------------------------

// tokenHeader is the fixed JOSE-style header. "CDRT" marks it as a CoDraw
// Readonly Token so a token from another system cannot be replayed here even if
// the HMAC key leaks across services.
var tokenHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"CDRT"}`))

// TokenClaims is the payload of a readonly token (docs/PROTOCOL.md §1.7).
type TokenClaims struct {
	Room string `json:"code"`
	Role string `json:"role"`
	// IssuedAt and ExpiresAt are milliseconds, matching every other timestamp on
	// the wire.
	IssuedAt  int64 `json:"iat"`
	ExpiresAt int64 `json:"exp"`
	// JTI makes each token unique so two tokens issued in the same millisecond
	// are still distinguishable in logs.
	JTI string `json:"jti"`
}

// TokenSigner issues and verifies readonly tokens.
//
// It is safe for concurrent use: it holds only the secret, which is never
// mutated after construction.
type TokenSigner struct {
	secret []byte
}

// NewTokenSigner validates and wraps the HMAC secret.
func NewTokenSigner(secret []byte) (*TokenSigner, error) {
	if len(secret) < 32 {
		return nil, ErrWeakSecret
	}
	// Copy so a caller mutating its config byte slice cannot change the key.
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &TokenSigner{secret: cp}, nil
}

// Issue creates a signed token for a room.
func (s *TokenSigner) Issue(room string, ttl time.Duration, now time.Time) (string, TokenClaims, error) {
	if ttl <= 0 {
		return "", TokenClaims{}, errors.New("auth: ttl must be positive")
	}
	jti, err := randomJTI()
	if err != nil {
		return "", TokenClaims{}, err
	}
	claims := TokenClaims{
		Room:      room,
		Role:      "viewer",
		IssuedAt:  now.UnixMilli(),
		ExpiresAt: now.Add(ttl).UnixMilli(),
		JTI:       jti,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", TokenClaims{}, fmt.Errorf("auth: encode claims: %w", err)
	}
	signingInput := tokenHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	sig := s.sign(signingInput)
	return signingInput + "." + sig, claims, nil
}

// Verify checks signature, expiry, room binding and the revocation watermark.
//
// The order is deliberate: signature first (cheap, and nothing else can be
// trusted before it), then expiry, then room, then revocation. A revoked token
// must fail even if it is otherwise valid.
func (s *TokenSigner) Verify(token, wantRoom string, revokedBefore int64, now time.Time) (TokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return TokenClaims{}, ErrBadToken
	}
	if parts[0] != tokenHeader {
		return TokenClaims{}, ErrBadToken
	}
	signingInput := parts[0] + "." + parts[1]
	if subtle.ConstantTimeCompare([]byte(parts[2]), []byte(s.sign(signingInput))) != 1 {
		return TokenClaims{}, ErrBadToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return TokenClaims{}, ErrBadToken
	}
	var claims TokenClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return TokenClaims{}, ErrBadToken
	}
	if claims.Room != wantRoom {
		return TokenClaims{}, ErrTokenRoomMismatch
	}
	if claims.ExpiresAt > 0 && now.UnixMilli() >= claims.ExpiresAt {
		return TokenClaims{}, ErrTokenExpired
	}
	if claims.Role != "" && claims.Role != "viewer" {
		// A token can never grant more than view: refusing unknown roles also
		// refuses a hand-crafted token that says "editor".
		return TokenClaims{}, ErrBadToken
	}
	if revokedBefore > 0 && claims.IssuedAt < revokedBefore {
		return TokenClaims{}, ErrTokenRevoked
	}
	return claims, nil
}

// sign computes base64url(HMAC-SHA256(secret, input)).
func (s *TokenSigner) sign(input string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(input))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// randomJTI returns a short unique token id.
func randomJTI() (string, error) {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
