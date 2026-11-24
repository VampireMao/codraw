package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/codraw/codraw/backend/internal/auth"
	"github.com/codraw/codraw/backend/internal/protocol"
	"github.com/codraw/codraw/backend/internal/room"
)

// maxJSONBody caps a REST request body. The only bodies accepted are a title and
// an editor key, so anything above a few KB is an attack.
const maxJSONBody = 8 << 10

// createRoomRequest is the body of POST /api/rooms.
type createRoomRequest struct {
	Title string `json:"title"`
}

// createRoomResponse is the 201 body (docs/PROTOCOL.md §1.1).
type createRoomResponse struct {
	Room struct {
		Code         string `json:"code"`
		Title        string `json:"title"`
		CreatedAt    int64  `json:"createdAt"`
		LastActiveAt int64  `json:"lastActiveAt"`
		ShapeCount   int    `json:"shapeCount"`
	} `json:"room"`
	Keys struct {
		EditorKey string `json:"editorKey"`
		ViewerKey string `json:"viewerKey"`
	} `json:"keys"`
	URLs struct {
		Edit string `json:"edit"`
		View string `json:"view"`
	} `json:"urls"`
	Limits protocol.LimitsPayload `json:"limits"`
}

// handleCreateRoom implements POST /api/rooms.
//
// Rate limiting is a separate, much stricter bucket than the general API limit
// (docs/PROTOCOL.md §7): creating rooms is the cheapest way to fill the database.
func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if ok, wait := s.createLimiter.allow(ip, s.deps.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.5)))
		writeError(w, r, http.StatusTooManyRequests, protocol.CodeTooManyRequests, "room creation rate limit exceeded")
		return
	}

	var req createRoomRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	if s.deps.Rooms == nil {
		writeError(w, r, http.StatusNotImplemented, protocol.CodeInternal, "room persistence is not configured")
		return
	}

	created, err := s.deps.Rooms.CreateRoom(r.Context(), req.Title)
	if err != nil {
		s.logError(r, "create room failed", err)
		writeError(w, r, http.StatusInternalServerError, protocol.CodeInternal, "could not create room")
		return
	}

	var resp createRoomResponse
	resp.Room.Code = created.Code
	resp.Room.Title = created.Title
	resp.Room.CreatedAt = created.CreatedAt.UnixMilli()
	resp.Room.LastActiveAt = created.CreatedAt.UnixMilli()
	resp.Room.ShapeCount = 0
	// The plaintext keys leave the system exactly once, here.
	resp.Keys.EditorKey = created.EditorKey
	resp.Keys.ViewerKey = created.ViewerKey
	base := s.baseURL(r)
	resp.URLs.Edit = base + "/r/" + created.Code + "?k=" + created.EditorKey
	resp.URLs.View = base + "/r/" + created.Code + "?k=" + created.ViewerKey
	resp.Limits = s.limitsPayload()

	writeJSON(w, http.StatusCreated, resp)
}

// getRoomResponse is the body of GET /api/rooms/{code} (PROTOCOL §1.2).
type getRoomResponse struct {
	Code         string `json:"code"`
	Title        string `json:"title"`
	CreatedAt    int64  `json:"createdAt"`
	LastActiveAt int64  `json:"lastActiveAt"`
	ShapeCount   int    `json:"shapeCount"`
	Online       int    `json:"online"`
	Role         string `json:"role"`
	IsFull       bool   `json:"isFull"`
}

// handleGetRoom implements GET /api/rooms/{code}.
func (s *Server) handleGetRoom(w http.ResponseWriter, r *http.Request) {
	code, ok := s.pathCode(w, r)
	if !ok {
		return
	}
	if s.deps.Rooms == nil {
		writeError(w, r, http.StatusNotImplemented, protocol.CodeInternal, "room persistence is not configured")
		return
	}
	meta, err := s.deps.Rooms.GetRoom(r.Context(), code)
	if err != nil {
		if errors.Is(err, room.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, protocol.CodeRoomNotFound, "room not found")
			return
		}
		s.logError(r, "get room failed", err)
		writeError(w, r, http.StatusInternalServerError, protocol.CodeInternal, "could not load room")
		return
	}

	// Role is resolved from the presented key; never from a client-declared field.
	role := protocol.RoleViewer
	if key := r.URL.Query().Get("key"); key != "" {
		if resolved, err := s.deps.Rooms.VerifyKey(r.Context(), code, key); err == nil {
			role = resolved
		}
	}

	resp := getRoomResponse{
		Code:         meta.Code,
		Title:        meta.Title,
		CreatedAt:    meta.CreatedAt.UnixMilli(),
		LastActiveAt: meta.LastActiveAt.UnixMilli(),
		ShapeCount:   meta.ShapeCount,
		Role:         role,
	}
	// Online is this instance's count, which the protocol explicitly calls an
	// approximation (PROTOCOL §1.2).
	if stats, ok := s.deps.Hub.Stats(code); ok {
		resp.Online = stats.Online
		// A live room is authoritative for its shape count: the persisted counter is
		// updated asynchronously and can lag, and with multiple instances each
		// instance's in-memory room is ahead of the store between touches.
		if stats.ShapeCount > 0 {
			resp.ShapeCount = stats.ShapeCount
		}
	}
	resp.IsFull = resp.Online >= s.deps.Config.Limits.MaxConnPerRoom
	writeJSON(w, http.StatusOK, resp)
}

// issueReadonlyRequest is the body of POST /api/rooms/{code}/readonly-token.
type issueReadonlyRequest struct {
	EditorKey  string `json:"editorKey"`
	TTLSeconds int    `json:"ttlSeconds"`
}

// issueReadonlyResponse is the body of a successful token issuance.
type issueReadonlyResponse struct {
	Token     string `json:"token"`
	Role      string `json:"role"`
	ExpiresAt int64  `json:"expiresAt"`
	URL       string `json:"url"`
}

// handleIssueReadonly implements POST /api/rooms/{code}/readonly-token
// (docs/PROTOCOL.md §1.3).
//
// Only an editor may mint a viewer token. The editor key may arrive in the body
// or in X-Room-Key, so a non-browser client does not have to put a secret in a
// URL.
func (s *Server) handleIssueReadonly(w http.ResponseWriter, r *http.Request) {
	code, ok := s.pathCode(w, r)
	if !ok {
		return
	}
	if s.deps.Signer == nil {
		writeError(w, r, http.StatusNotImplemented, protocol.CodeInternal, "readonly tokens are not configured")
		return
	}
	if ok, wait := s.readonlyLimiter.allow(s.clientIP(r), s.deps.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.5)))
		writeError(w, r, http.StatusTooManyRequests, protocol.CodeTooManyRequests, "rate limit exceeded")
		return
	}

	var req issueReadonlyRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	key := strings.TrimSpace(req.EditorKey)
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("X-Room-Key"))
	}
	if key == "" {
		writeError(w, r, http.StatusUnauthorized, protocol.CodeUnauthorized, "editorKey is required")
		return
	}

	okKey, err := s.deps.Rooms.VerifyEditorKey(r.Context(), code, key)
	if err != nil {
		if errors.Is(err, room.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, protocol.CodeRoomNotFound, "room not found")
			return
		}
		s.logError(r, "verify key failed", err)
		writeError(w, r, http.StatusInternalServerError, protocol.CodeInternal, "internal error")
		return
	}
	if !okKey {
		// Deliberately the same response as a wrong key: distinguishing them would
		// tell an attacker that a viewer key is valid.
		writeError(w, r, http.StatusForbidden, protocol.CodeForbidden, "editorKey required to issue readonly tokens")
		return
	}

	ttl := s.readonlyTTL(req.TTLSeconds)
	token, claims, err := s.deps.Signer.Issue(code, ttl, s.deps.Now())
	if err != nil {
		s.logError(r, "issue readonly token failed", err)
		writeError(w, r, http.StatusInternalServerError, protocol.CodeInternal, "could not issue token")
		return
	}

	writeJSON(w, http.StatusOK, issueReadonlyResponse{
		Token:     token,
		Role:      protocol.RoleViewer,
		ExpiresAt: claims.ExpiresAt,
		URL:       s.baseURL(r) + "/r/" + code + "?rt=" + token,
	})
}

// revokeReadonlyRequest is the body of .../revoke.
type revokeReadonlyRequest struct {
	EditorKey string `json:"editorKey"`
	All       bool   `json:"all"`
}

// handleRevokeReadonly implements POST /api/rooms/{code}/readonly-token/revoke
// (§1.4). Revocation is a room-level watermark, so no per-token storage is
// needed and revoked tokens stay revoked across restarts.
func (s *Server) handleRevokeReadonly(w http.ResponseWriter, r *http.Request) {
	code, ok := s.pathCode(w, r)
	if !ok {
		return
	}
	var req revokeReadonlyRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	key := strings.TrimSpace(req.EditorKey)
	if key == "" {
		key = strings.TrimSpace(r.Header.Get("X-Room-Key"))
	}
	if key == "" {
		writeError(w, r, http.StatusUnauthorized, protocol.CodeUnauthorized, "editorKey is required")
		return
	}
	okKey, err := s.deps.Rooms.VerifyEditorKey(r.Context(), code, key)
	if err != nil {
		if errors.Is(err, room.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, protocol.CodeRoomNotFound, "room not found")
			return
		}
		s.logError(r, "verify key failed", err)
		writeError(w, r, http.StatusInternalServerError, protocol.CodeInternal, "internal error")
		return
	}
	if !okKey {
		writeError(w, r, http.StatusForbidden, protocol.CodeForbidden, "editorKey required to revoke readonly tokens")
		return
	}
	if _, err := s.deps.Rooms.BumpReadonlyRevocation(r.Context(), code); err != nil {
		s.logError(r, "revoke readonly failed", err)
		writeError(w, r, http.StatusInternalServerError, protocol.CodeInternal, "could not revoke tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

// snapshotResponse is the body of GET /api/rooms/{code}/snapshot (§1.5).
type snapshotResponse struct {
	Seq      int64             `json:"seq"`
	Epoch    int64             `json:"epoch"`
	Shapes   []json.RawMessage `json:"shapes"`
	Next     *string           `json:"next"`
	Complete bool              `json:"complete"`
}

// handleSnapshot implements the paged snapshot fallback for large rooms.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	code, ok := s.pathCode(w, r)
	if !ok {
		return
	}
	if s.deps.Rooms == nil {
		writeError(w, r, http.StatusNotImplemented, protocol.CodeInternal, "room persistence is not configured")
		return
	}
	// Reading a snapshot requires a valid credential (room key or readonly
	// token): it exposes the full room contents (docs/PROTOCOL.md §1.5).
	key := r.URL.Query().Get("key")
	rt := r.URL.Query().Get("rt")
	if key == "" && rt == "" {
		writeError(w, r, http.StatusUnauthorized, protocol.CodeUnauthorized, "key or rt is required")
		return
	}
	okCred, err := s.deps.Rooms.VerifyCredential(r.Context(), code, key, rt)
	if err != nil {
		if errors.Is(err, room.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, protocol.CodeRoomNotFound, "room not found")
			return
		}
		s.logError(r, "verify credential failed", err)
		writeError(w, r, http.StatusInternalServerError, protocol.CodeInternal, "internal error")
		return
	}
	if !okCred {
		writeError(w, r, http.StatusForbidden, protocol.CodeForbidden, "invalid key")
		return
	}

	limit := 500
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, r, http.StatusBadRequest, protocol.CodeBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(n, 1000)
	}
	page, err := s.deps.Rooms.Snapshot(r.Context(), code, r.URL.Query().Get("after"), limit)
	if err != nil {
		if errors.Is(err, room.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, protocol.CodeRoomNotFound, "room not found")
			return
		}
		s.logError(r, "snapshot failed", err)
		writeError(w, r, http.StatusInternalServerError, protocol.CodeInternal, "could not read snapshot")
		return
	}

	resp := snapshotResponse{
		Seq:      page.Seq,
		Epoch:    page.Epoch,
		Shapes:   make([]json.RawMessage, 0, len(page.Shapes)),
		Complete: page.Complete,
	}
	for _, shape := range page.Shapes {
		resp.Shapes = append(resp.Shapes, json.RawMessage(shape))
	}
	if page.Next != "" {
		next := page.Next
		resp.Next = &next
	}
	writeJSON(w, http.StatusOK, resp)
}

// metaResponse is the body of GET /api/meta.
type metaResponse struct {
	APIVersion       int    `json:"apiVersion"`
	ProtocolVersion  int    `json:"protocolVersion"`
	ServerVersion    string `json:"serverVersion"`
	BuildTime        string `json:"buildTime"`
	MinClientVersion string `json:"minClientVersion"`
	MaxMessageBytes  int    `json:"maxMessageBytes"`
}

// handleMeta implements GET /api/meta (PROTOCOL §10).
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, metaResponse{
		APIVersion:       1,
		ProtocolVersion:  protocol.Version,
		ServerVersion:    s.deps.Version,
		BuildTime:        s.deps.BuildTime,
		MinClientVersion: "1.0.0",
		MaxMessageBytes:  s.deps.Config.Limits.MaxMessageBytes,
	})
}

// --- helpers ------------------------------------------------------------

// pathCode validates {code} and writes a 400 when it is malformed. Checking the
// format before any lookup is what makes room-code scanning useless: a bot
// hammering random strings costs only a regex, never a query (PROTOCOL §1.2).
func (s *Server) pathCode(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := r.PathValue("code")
	code, err := room.NormalizeCode(raw)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, protocol.CodeBadRequest, "room code must be 6 characters from the room alphabet")
		return "", false
	}
	return code, true
}

// decodeJSONBody enforces a size limit, a JSON content type and a single value,
// so trailing garbage cannot be smuggled past validation.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	ct := r.Header.Get("Content-Type")
	if ct != "" {
		mediaType := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
		if mediaType != "application/json" {
			return errors.New("Content-Type must be application/json")
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // a typo in a field name must fail loudly
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			// An empty body is legal for endpoints whose fields are all optional.
			return nil
		}
		return errors.New("invalid JSON body: " + err.Error())
	}
	// Reject a second JSON document.
	if dec.More() {
		return errors.New("body must contain exactly one JSON object")
	}
	return nil
}

// baseURL returns the absolute origin used for share links: PUBLIC_BASE_URL when
// configured, otherwise derived from the request (PROTOCOL §1.1).
func (s *Server) baseURL(r *http.Request) string {
	if s.deps.Config.PublicBaseURL != "" {
		return s.deps.Config.PublicBaseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" && s.deps.Config.TrustProxyHeaders {
		scheme = strings.TrimSpace(strings.Split(proto, ",")[0])
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" && s.deps.Config.TrustProxyHeaders {
		host = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	return scheme + "://" + host
}

// readonlyTTL clamps the requested token lifetime to [1m, 7d].
func (s *Server) readonlyTTL(requested int) (ttl timeDuration) {
	cfg := s.deps.Config.Readonly
	if requested <= 0 {
		return cfg.DefaultTTL
	}
	d := secs(requested)
	if d < cfg.MinTTL {
		return cfg.MinTTL
	}
	if d > cfg.MaxTTL {
		return cfg.MaxTTL
	}
	return d
}

// limitsPayload renders the limits block advertised by REST and `joined`.
func (s *Server) limitsPayload() protocol.LimitsPayload {
	lim := s.deps.Config.Limits
	return protocol.LimitsPayload{
		MaxMessageBytes:  lim.MaxMessageBytes,
		MaxShapeBytes:    lim.MaxShapeBytes,
		MaxShapesPerRoom: lim.MaxShapesPerRoom,
		OpsPerSecond:     lim.OpsPerSecond,
		OpsBurst:         lim.OpsBurst,
		CursorPerSecond:  lim.CursorPerSecond,
	}
}

// handleAPINotFound keeps unknown /api paths out of the SPA fallback.
func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, protocol.CodeNotFound, "no such API endpoint")
}

// handleWSUnavailable is used when no WebSocket handler was injected.
func (s *Server) handleWSUnavailable(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotImplemented, protocol.CodeNotImplemented, "websocket transport is not configured")
}

// verifyReadonly is the shared token verification used by the WS handshake and
// the REST snapshot endpoint. It returns the viewer role on success.
func (s *Server) verifyReadonly(r *http.Request, code, token string) error {
	if s.deps.Signer == nil {
		return errors.New("readonly tokens are not configured")
	}
	watermark := int64(0)
	if s.deps.Rooms != nil {
		if wm, err := s.deps.Rooms.ReadonlyWatermark(r.Context(), code); err == nil {
			watermark = wm
		}
	}
	_, err := s.deps.Signer.Verify(token, code, watermark, s.deps.Now())
	return err
}

// ensure the auth import is used even if the readonly endpoints are compiled out
// by a future build tag.
var _ = auth.ErrBadToken
