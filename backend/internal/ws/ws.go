// Package ws implements the WebSocket transport for /ws.
//
// # Goroutine layout per connection
//
//	readPump   (1 goroutine)  reads frames, enforces limits, parses, dispatches
//	writePump  (1 goroutine)  drains sendCh, writes frames, sends pings
//
// The room goroutine never writes to a socket: it calls Conn.Send, which does a
// non-blocking send on a buffered channel. A full channel is a slow consumer and
// is handled by the room (close 4001), so the room can never be stalled by a
// socket.
//
// # Ordering
//
// joined must be the first frame the server sends (docs/PROTOCOL.md §3). It is
// queued by the upgrade handler before readPump starts, which is why it cannot
// race with a resync frame that hello triggers.
package ws

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/codraw/codraw/backend/internal/auth"
	"github.com/codraw/codraw/backend/internal/hub"
	"github.com/codraw/codraw/backend/internal/metrics"
	"github.com/codraw/codraw/backend/internal/protocol"
	"github.com/codraw/codraw/backend/internal/room"
)

// Handshake failure responses are JSON envelopes so a browser's console shows
// the protocol error code rather than an HTML page.

// Server is the /ws endpoint.
type Server struct {
	hub *hub.Hub
	log *slog.Logger
	cfg Config
	now func() time.Time
	// resolveRoleFn is the injected credential resolver; nil means "viewer only".
	resolveRoleFn RoleResolver
	// sessionSigner mints/verifies the clientId credential. When set, the server
	// never trusts the cid query parameter for an unverified connection: it
	// generates a fresh clientId instead. See resolveClientIdentity.
	sessionSigner *auth.TokenSigner
	sessionTTL    time.Duration
	upgrade       websocket.Upgrader
	// metrics receives frame counters; nil disables counting.
	metrics MetricsSink
}

// MetricsSink is the subset of metrics the transport records.
type MetricsSink interface {
	ObserveMessage(dir, typ string)
	IncSlowConsumerKick()
}

// SetMetrics installs the metrics sink. Optional: nil is a valid no-op.
func (s *Server) SetMetrics(m MetricsSink) { s.metrics = m }

// SetSessionSigner enables server-issued session identities.
//
// This is the fix for clientId spoofing: once a signer is installed, a
// connection without a valid session token gets a **server-generated** clientId,
// so presenting the owner's (or an editor's) clientId in the query string no
// longer inherits that member's role. The token is returned in `joined` and is
// expected on the next reconnect (?st=...). It must be called before serving.
func (s *Server) SetSessionSigner(signer *auth.TokenSigner, ttl time.Duration) {
	s.sessionSigner = signer
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	s.sessionTTL = ttl
}

// observe counts a frame when a sink is installed.
func (s *Server) observe(dir, typ string) {
	if s.metrics != nil {
		s.metrics.ObserveMessage(dir, typ)
	}
}

// observeSlowKick counts a connection evicted by the room for backpressure.
func (s *Server) observeSlowKick() {
	if s.metrics != nil {
		s.metrics.IncSlowConsumerKick()
	}
}

// ErrForbidden is returned by a RoleResolver when a readonly token is invalid.
var ErrForbidden = errors.New("ws: forbidden")

// Config is the WebSocket-layer configuration. It is a copy of the relevant
// config fields so this package does not import internal/config and stays
// testable in isolation.
type Config struct {
	// AllowedOrigins is an exact-match origin allowlist.
	AllowedOrigins []string
	// AllowEmptyOrigin permits non-browser clients with no Origin header.
	AllowEmptyOrigin bool
	// Limits drive read limits and the payload advertised in joined.
	Limits        room.Limits
	LimitsPayload protocol.LimitsPayload
	// InstanceID is advertised in joined.
	InstanceID string
	// MessageBytes caps a single frame (65536).
	MessageBytes int64
	// HelloTimeout closes a connection that never sends hello.
	HelloTimeout time.Duration
	// PongWait closes a connection that sends nothing for this long.
	PongWait time.Duration
	// PingPeriod is the server-initiated ping interval.
	PingPeriod time.Duration
	// WriteWait bounds one frame write.
	WriteWait time.Duration
	// SendQueue is the outbound channel depth.
	SendQueue int
	// SendQueueBytes caps the outbound buffer in bytes.
	SendQueueBytes int
	// LeaveGrace is forwarded to the room for presence handling.
	LeaveGrace time.Duration
	// Logger is required.
	Logger *slog.Logger
	// Now overrides the clock in tests.
	Now func() time.Time
}

// New builds the /ws server.
func New(h *hub.Hub, cfg Config) (*Server, error) {
	if h == nil {
		return nil, errors.New("ws: hub is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MessageBytes <= 0 {
		cfg.MessageBytes = 65536
	}
	if cfg.HelloTimeout <= 0 {
		cfg.HelloTimeout = 10 * time.Second
	}
	if cfg.PongWait <= 0 {
		cfg.PongWait = 60 * time.Second
	}
	if cfg.PingPeriod <= 0 {
		cfg.PingPeriod = 20 * time.Second
	}
	if cfg.WriteWait <= 0 {
		cfg.WriteWait = 10 * time.Second
	}
	if cfg.SendQueue <= 0 {
		cfg.SendQueue = 256
	}
	if cfg.SendQueueBytes <= 0 {
		cfg.SendQueueBytes = 1 << 20
	}
	s := &Server{hub: h, log: cfg.Logger, cfg: cfg, now: cfg.Now}
	s.upgrade = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		// The origin check is ours, not the library's, so the rule lives in one
		// place and produces our error envelope.
		CheckOrigin: func(*http.Request) bool { return true },
		// permessage-deflate is deliberately off: it costs CPU per frame and the
		// protocol targets 64KB JSON frames, where compression does not pay for
		// itself at these message sizes.
		EnableCompression: false,
	}
	return s, nil
}

// ServeHTTP implements the /ws endpoint.
//
// Order of checks follows docs/PROTOCOL.md §2.1 exactly: Origin, then room code
// format, then room existence, then budgets. Each failure is an HTTP response
// with no upgrade.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Origin first: an unauthorised origin must not learn whether the room
	// exists.
	if !s.originAllowed(r) {
		// Naming both sides is deliberate: this is the #1 misconfiguration in
		// deployment (PUBLIC_BASE_URL with the wrong domain suffix, http vs
		// https). A bare "origin not allowed" leaves the operator guessing; the
		// two values tell them exactly what to fix.
		//
		// The allow-list is not secret — it is the site's own origin — and this
		// response never reveals whether the room exists, so the original
		// ordering guarantee still holds.
		msg := "origin not allowed: got " + strconv.Quote(r.Header.Get("Origin"))
		if allowed := s.cfg.AllowedOrigins; len(allowed) > 0 {
			msg += ", allowed " + strconv.Quote(strings.Join(allowed, ", "))
		} else {
			msg += ", allowed (none configured)"
		}
		s.writeHandshakeError(w, r, http.StatusForbidden, protocol.CodeForbidden, msg)
		return
	}

	q := r.URL.Query()
	code, err := room.NormalizeCode(q.Get("room"))
	if err != nil {
		s.writeHandshakeError(w, r, http.StatusBadRequest, protocol.CodeBadRequest, "room code must be 6 characters")
		return
	}
	// The clientId is an *identity*, not an input we can trust. With a session
	// signer installed, only a valid ?st= token can pin a cid; otherwise the
	// server generates a fresh one. Without a signer (tests / legacy wiring) the
	// query parameter is still accepted, and a warning is logged at startup.
	cid, sessionToken, err := s.resolveClientIdentity(code, q.Get("cid"), q.Get("st"))
	if err != nil {
		s.writeHandshakeError(w, r, http.StatusBadRequest, protocol.CodeBadRequest, err.Error())
		return
	}
	name := protocol.SanitizeNickname(q.Get("name"), s.cfg.Limits.MaxNameRunes)
	if name == "" {
		name = "访客-" + truncateID(cid, 4)
	}
	role, err := s.resolveRole(r.Context(), code, q.Get("key"), q.Get("rt"))
	if err != nil {
		// A rejected secondary credential is a policy failure, not a server error.
		// It must be 403 so the client shows "readonly link invalid" rather than
		// retrying a 5xx forever.
		if errors.Is(err, ErrForbidden) {
			s.writeHandshakeError(w, r, http.StatusForbidden, protocol.CodeForbidden, "readonly token rejected")
			return
		}
		if errors.Is(err, room.ErrNotFound) {
			s.writeHandshakeError(w, r, http.StatusNotFound, protocol.CodeRoomNotFound, "room not found")
			return
		}
		s.log.Error("resolve role", "err", err)
		s.writeHandshakeError(w, r, http.StatusInternalServerError, protocol.CodeInternal, "internal error")
		return
	}
	// A readonly token pins the connection to viewer for its whole lifetime
	// (docs/PERMISSIONS.md §4.2 rule 1). The room re-derives the effective role
	// from this flag on every operation.
	readonly := q.Get("rt") != ""

	// 2. Budgets. Admit also materialises the room.
	rh, online, release, err := s.hub.Admit(r.Context(), code)
	if err != nil {
		status, pcode, msg := hub.ErrorForCode(err)
		s.writeHandshakeError(w, r, status, pcode, msg)
		return
	}

	// 3. Upgrade. From here on, failures must be reported over the socket.
	conn, err := s.upgrade.Upgrade(w, r, nil)
	if err != nil {
		release()
		s.log.Debug("upgrade failed", "err", err, "room", code)
		return
	}
	c := newConn(s, conn, rh, connParams{
		room:     code,
		cid:      cid,
		name:     name,
		role:     role,
		online:   online,
		readonly: readonly,
	}, release)

	// Join decides the authoritative role inside the room goroutine
	// (docs/PERMISSIONS.md §4.2) before joined is built, because joined must
	// carry it. JoinConn has already called SetRole on c, so the role is visible
	// to c.User() even if a grant lands right after.
	res, err := rh.JoinConn(r.Context(), c, readonly)
	if err != nil {
		c.closeWithError(protocol.CloseConnLimit, protocol.CodeRoomFull, "room is full")
		return
	}

	// joined is queued before readPump starts so it is provably the first frame
	// on the wire.
	c.sendJoined(res, sessionToken)

	// Announce the newcomer to everyone else, including a full member list so
	// clients never need to reconcile increments (PROTOCOL §4.6).
	c.announceJoin()

	c.run()
}

// RoleResolver resolves the authoritative role from the presented credentials.
// It is injected so internal/ws does not depend on the auth implementation.
type RoleResolver func(ctx context.Context, code, key, rt string) (string, error)

// SetRoleResolver installs the role resolver. It must be called before serving.
//
// Default behaviour (no resolver) is viewer, which is the safe minimum: without
// a way to verify keys, nobody may write.
func (s *Server) SetRoleResolver(fn RoleResolver) { s.resolveRoleFn = fn }

// resolveRole delegates to the injected resolver.
func (s *Server) resolveRole(ctx context.Context, code, key, rt string) (string, error) {
	if s.resolveRoleFn == nil {
		return protocol.RoleViewer, nil
	}
	return s.resolveRoleFn(ctx, code, key, rt)
}

// errInvalidClientID is returned when the legacy (no session signer) path gets a
// malformed cid.
var errInvalidClientID = errors.New("cid is required and must be 8-40 id characters")

// resolveClientIdentity decides the authoritative clientId for a handshake and,
// when sessions are enabled, mints the token the client must present next time.
//
// Security rule: with a signer installed, the `cid` query parameter is never
// authoritative. A valid ?st= token pins the cid; otherwise the server generates
// a fresh one. This is what stops a viewer from presenting the owner's cid and
// inheriting ownership (the room resolves roles by cid).
func (s *Server) resolveClientIdentity(code, requestedCID, presented string) (cid, sessionToken string, err error) {
	if s.sessionSigner == nil {
		cid = strings.TrimSpace(requestedCID)
		if !validClientID(cid) {
			return "", "", errInvalidClientID
		}
		return cid, "", nil
	}

	if presented != "" {
		if claims, verr := s.sessionSigner.VerifySession(presented, code, s.now()); verr == nil && validClientID(claims.ClientID) {
			cid = claims.ClientID
		}
	}
	if cid == "" {
		cid, err = newClientID()
		if err != nil {
			return "", "", fmt.Errorf("generate client id: %w", err)
		}
	}
	token, _, ierr := s.sessionSigner.IssueSession(code, cid, s.sessionTTL, s.now())
	if ierr != nil {
		return "", "", fmt.Errorf("issue session token: %w", ierr)
	}
	return cid, token, nil
}

// newClientID returns 10 random bytes hex-encoded (20 characters), matching the
// `^[A-Za-z0-9_-]{8,40}$` clientId contract.
func newClientID() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// originAllowed implements the allowlist rule from docs/PROTOCOL.md §2.1.
func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// No Origin means a non-browser client. Allowed only when explicitly
		// enabled, because browsers always send it and its absence is how a
		// WebSocket CSRF check is bypassed.
		return s.cfg.AllowEmptyOrigin
	}
	if origin == "null" {
		// Sandboxed iframes and file:// pages. Never allowed.
		return false
	}
	for _, allowed := range s.cfg.AllowedOrigins {
		if strings.EqualFold(origin, allowed) {
			return true
		}
	}
	return false
}

func validClientID(cid string) bool {
	if len(cid) < 8 || len(cid) > 64 {
		return false
	}
	for i := 0; i < len(cid); i++ {
		c := cid[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func truncateID(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// writeHandshakeError writes the uniform JSON error envelope.
func (s *Server) writeHandshakeError(w http.ResponseWriter, r *http.Request, status int, code protocol.ErrCode, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// requestId is injected by the HTTP middleware via the context; the ws
	// package must not import internal/api, so it is passed as a plain string.
	reqID, _ := r.Context().Value(requestIDContextKey{}).(string)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": code, "message": msg, "requestId": reqID},
	})
	_, _ = w.Write(body)
}

// requestIDContextKey mirrors api.requestIDKey without importing internal/api.
type requestIDContextKey struct{}

// WithRequestIDKey is installed by main so the ws layer can read the request ID
// that the api middleware put in the context.
func WithRequestIDKey(ctx context.Context, key any) context.Context {
	return context.WithValue(ctx, key, key)
}

// connParams are the validated handshake values a connection needs.
type connParams struct {
	room     string
	cid      string
	name     string
	role     string
	online   int
	readonly bool
}

// conn is one WebSocket connection.
type conn struct {
	srv  *Server
	ws   *websocket.Conn
	rh   *room.Handle
	user atomic.Pointer[protocol.User]

	sendCh chan []byte
	// ctrlCh serialises close handshakes onto the write pump. It is buffered (1) so
	// Kick from the room goroutine never blocks.
	ctrlCh chan closeReq
	// sendBytes tracks queued bytes so a client that sends many tiny frames cannot
	// exceed the byte budget just because the count budget is large.
	sendBytes atomic.Int64
	// closeOnce guards the close handshake so read/write pumps do not both try.
	closeOnce sync.Once
	closed    chan struct{}
	// release returns the hub's connection slot exactly once.
	release func()

	params connParams
	hello  atomic.Bool
	// leaveOnce guards the single Leave/Bye call per connection.
	leaveOnce sync.Once
	// kicked records that the room evicted us, to suppress a duplicate leave.
	kicked atomic.Bool
	// lastRecv is used by the idle watchdog.
	lastRecv atomic.Int64
	// reassembled records a fragmented message so a text frame split across
	// continuations is handled by the library and cannot exceed the limit.
}

func newConn(s *Server, ws *websocket.Conn, rh *room.Handle, p connParams, release func()) *conn {
	c := &conn{
		srv:     s,
		ws:      ws,
		rh:      rh,
		sendCh:  make(chan []byte, s.cfg.SendQueue),
		ctrlCh:  make(chan closeReq, 1),
		closed:  make(chan struct{}),
		release: release,
		params:  p,
	}
	u := protocol.User{
		ClientID: p.cid,
		UserID:   "u_" + shortID(p.cid),
		Name:     p.name,
		Color:    paletteColor(p.cid),
		Role:     p.role,
	}
	c.user.Store(&u)
	ws.SetReadLimit(s.cfg.MessageBytes)
	c.lastRecv.Store(s.now().UnixNano())
	return c
}

// User implements room.Member.
func (c *conn) User() protocol.User { return *c.user.Load() }

// SetRole implements room.Member. The role is mutable because ownership and
// authorisation change mid-session (docs/PERMISSIONS.md §5).
func (c *conn) SetRole(role string) {
	for {
		cur := c.user.Load()
		next := *cur
		next.Role = role
		if c.user.CompareAndSwap(cur, &next) {
			return
		}
	}
}

// Send implements room.Member. It never blocks.
func (c *conn) Send(frame []byte) bool {
	if c.isClosed() {
		return false
	}
	if c.sendBytes.Load()+int64(len(frame)) > int64(c.srv.cfg.SendQueueBytes) {
		return false
	}
	select {
	case c.sendCh <- frame:
		c.sendBytes.Add(int64(len(frame)))
		return true
	default:
		return false
	}
}

// closeReq asks the write pump to emit an optional error envelope and then a close
// frame with the given code.
type closeReq struct {
	code   int
	reason string
	// envelope is an already-encoded `error` frame to flush before closing; empty
	// for a plain close.
	envelope []byte
}

// Kick implements room.Member.
//
// Why this writes the close frame itself instead of queuing it to the write pump:
// the pump is the very thing that is stuck when a client is a slow consumer. A
// blocked WriteMessage (the peer stopped reading, so the TCP window is full) holds
// the pump until WriteWait (10s), and a close request sitting in ctrlCh cannot be
// serviced until then. The result was "kicked but never told": the server dropped
// the member and closed the fd, so the peer saw a bare RST and could not tell a
// slow-consumer eviction from a network fault.
//
// gorilla permits a control frame (close) to be written concurrently with a data
// frame, which is exactly the escape hatch needed here. The deadline is short so
// Kick cannot itself block the room goroutine: if even the close frame cannot be
// written within closeKickTimeout, the socket is torn down anyway.
func (c *conn) Kick(code int, reason string) {
	c.kicked.Store(true)
	// Close frames are small and unframed from the write pump's perspective; a
	// short deadline keeps a wedged peer from holding the room goroutine.
	_ = c.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, truncateReason(reason)),
		c.srv.now().Add(closeKickTimeout))
	c.hardClose()
}

// closeKickTimeout bounds the best-effort close frame a Kick emits. It is far below
// WriteWait on purpose: Kick runs on the room goroutine and must return quickly.
const closeKickTimeout = 200 * time.Millisecond

// KickWithError implements room.Member: it flushes an `error` envelope and then
// closes with closeCode, in that order.
//
// Unlike Kick it does not write the close frame directly: the error envelope is
// queued on the same channel the write pump drains, and a direct close would
// discard it and leave the client without a code ("被拒 + 断线 + 有明确提示",
// docs/PERMISSIONS.md §9 P9). The pump therefore owns both writes, which is also
// what keeps every write on one goroutine.
func (c *conn) KickWithError(closeCode int, code protocol.ErrCode, msg string) {
	c.kicked.Store(true)
	c.closeWithError(closeCode, code, msg)
}

// requestClose queues a close request. It never blocks: if a close is already
// pending the new one is dropped (the first one wins), and if the pump is gone the
// socket is already dead.
func (c *conn) requestClose(req closeReq) {
	select {
	case c.ctrlCh <- req:
		return
	default:
	}
	select {
	case <-c.closed:
		// The socket is already gone.
	default:
		// A close request is already queued; the first one wins and the write pump
		// will finish the handshake. Dropping this one avoids cutting off an error
		// envelope that is still queued ahead of it.
	}
}

func (c *conn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// setRole publishes the authoritative handshake role on the connection.
func (c *conn) setRole(role string) { c.SetRole(role) }

// sendJoined queues the first business frame.
//
// The role and owner come from the room's admission decision, not from the
// pre-upgrade credential resolution: ownership depends on room state that only
// the room goroutine can see (docs/PERMISSIONS.md §4.2, §4.3).
//
// The role is read from the connection rather than from res: a room.grant can
// land between Join and this call, and refreshRoles has already pushed the newer
// role onto the connection. Using res.Role here would hand the client a role the
// room no longer holds, and since the grant's fan-out skips a not-yet-announced
// member, nothing else would correct it.
func (c *conn) sendJoined(res room.JoinResult, sessionToken string) {
	you := c.User()
	stats := c.rh.Stats()
	ownerCID := stats.OwnerClientID
	if ownerCID == "" {
		ownerCID = res.OwnerClientID
	}
	owner := (*string)(nil)
	if ownerCID != "" {
		cid := ownerCID
		owner = &cid
	}
	payload := protocol.JoinedPayload{
		You: protocol.JoinedUser{
			User:    you,
			IsOwner: you.Role == protocol.RoleOwner,
		},
		Room:          protocol.RoomInfo{Code: c.params.room, Title: stats.Title, Epoch: stats.Epoch, Seq: stats.Seq},
		Online:        res.Online,
		InstanceID:    c.srv.cfg.InstanceID,
		Limits:        c.srv.cfg.LimitsPayload,
		ServerTime:    c.srv.now().UnixMilli(),
		OwnerClientID: owner,
		SessionToken:  sessionToken,
	}
	frame, err := protocol.Marshal(protocol.TypeJoined, c.params.room, c.params.cid, 0, c.srv.now().UnixMilli(), payload)
	if err != nil {
		c.srv.log.Error("encode joined", "err", err)
		return
	}
	if c.Send(frame) {
		c.srv.observe(metrics.DirOut, protocol.TypeJoined)
	}
}

// announceJoin broadcasts presence.join with the full member list.
func (c *conn) announceJoin() {
	// The room owns the member list, so the announcement is issued by the room
	// itself; this indirection exists only to keep the wire order
	// joined → presence.join.
	c.rh.NotifyJoined(c)
}

// run starts the write pump and runs the read pump until the socket dies.
func (c *conn) run() {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.writePump()
	}()
	c.readPump()

	// The read pump has stopped. Ask the write pump for a normal close so the close
	// frame is flushed by the single writer, then wait for it to exit. If the room
	// already queued a kick, requestClose drops this one and that code wins.
	if !c.kicked.Load() {
		c.requestClose(closeReq{code: protocol.CloseNormal})
	}
	wg.Wait()
	// hardClose is idempotent; it covers the case where the write pump exited
	// without closing (write error).
	c.hardClose()
	c.cleanup()
}

// cleanup returns the connection slot and tells the room the member is gone.
func (c *conn) cleanup() {
	c.leaveOnce.Do(func() {
		if !c.kicked.Load() {
			c.rh.Leave(c)
		}
	})
	c.release()
}

// hardClose closes the underlying socket once. Closing the fd is what unblocks a
// write pump stuck in WriteMessage, so it must not wait for the pump.
func (c *conn) hardClose() {
	c.closeOnce.Do(func() {
		close(c.closed)
		// A short deadline makes the close frame flush best-effort without
		// blocking shutdown on a wedged peer.
		_ = c.ws.SetWriteDeadline(c.srv.now().Add(time.Second))
		_ = c.ws.Close()
	})
}

// closeWithError asks the write pump to emit the protocol `error` envelope and then
// close with closeCode. Like Kick, it performs no I/O itself so it is safe to call
// from any goroutine.
func (c *conn) closeWithError(closeCode int, code protocol.ErrCode, msg string) {
	req := closeReq{code: closeCode, reason: msg}
	if code != "" {
		payload := protocol.ErrorPayload{Code: code, Message: msg, Fatal: closeCode != protocol.CloseNormal}
		if frame, err := protocol.Marshal(protocol.TypeError, c.params.room, c.params.cid, 0, c.srv.now().UnixMilli(), payload); err == nil {
			req.envelope = frame
		}
	}
	c.requestClose(req)
}

// truncateReason keeps the close reason inside the 123-byte WebSocket limit.
func truncateReason(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	for max > 0 && (s[max]&0xC0) == 0x80 {
		return s[:max-1]
	}
	return s[:max]
}

// readPump reads and dispatches frames until the connection ends.
func (c *conn) readPump() {
	defer c.hardClose()
	// The read deadline is refreshed on every frame; pings from the client keep
	// it alive. It implements the "60s without any frame → 1001" rule.
	_ = c.ws.SetReadDeadline(c.srv.now().Add(c.srv.cfg.PongWait))
	c.ws.SetPongHandler(func(string) error {
		c.lastRecv.Store(c.srv.now().UnixNano())
		return c.ws.SetReadDeadline(c.srv.now().Add(c.srv.cfg.PongWait))
	})

	for {
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return
			}
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				return
			}
			return
		}
		c.lastRecv.Store(c.srv.now().UnixNano())
		_ = c.ws.SetReadDeadline(c.srv.now().Add(c.srv.cfg.PongWait))

		if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
			continue
		}
		if int64(len(data)) > c.srv.cfg.MessageBytes {
			// SetReadLimit usually catches this first; this is the belt-and-braces
			// path for a library version that returns oversize data.
			c.closeWithError(protocol.CloseMessageTooBig, protocol.CodeMessageTooLarge, "message too large")
			return
		}
		if !c.dispatch(data) {
			return
		}
	}
}

// dispatch parses one frame and hands it to the room. It returns false when the
// connection must stop reading.
func (c *conn) dispatch(data []byte) bool {
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		c.sendError(protocol.CodeBadRequest, "invalid JSON", false, "")
		return true
	}
	if err := protocol.ValidateEnvelope(&env); err != nil {
		pe, _ := protocol.AsError(err)
		code, msg, fatal := protocol.CodeBadRequest, err.Error(), false
		if pe != nil {
			code, msg, fatal = pe.Code, pe.Message, pe.Fatal
		}
		c.sendError(code, msg, fatal, "")
		if fatal {
			c.closeWithError(protocol.ClosePolicyViolation, code, msg)
			return false
		}
		return true
	}
	// The room code in the frame must match the connection: otherwise a client
	// could address another room's log through its own connection.
	if !strings.EqualFold(env.RoomID, c.params.room) {
		c.sendError(protocol.CodeBadRequest, "roomId does not match connection", false, "")
		return true
	}
	if !protocol.ClientTypes[env.Type] {
		c.sendError(protocol.CodeUnsupportedType, fmt.Sprintf("unsupported type %q", env.Type), false, "")
		return true
	}
	// hello must be the first frame from the client.
	if env.Type == protocol.TypeHello {
		if c.hello.Swap(true) {
			c.sendError(protocol.CodeBadRequest, "duplicate hello", false, "")
			return true
		}
		var p protocol.HelloPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			c.sendError(protocol.CodeBadRequest, "hello payload invalid", false, "")
			return true
		}
		c.srv.observe(metrics.DirIn, protocol.TypeHello)
		c.rh.Hello(c, &env, &p)
		return true
	}
	if !c.hello.Load() {
		// Anything before hello is a protocol violation; close rather than
		// silently dropping, because the client is clearly out of sync.
		c.closeWithError(protocol.ClosePolicyViolation, protocol.CodeUnauthenticated, "hello required first")
		return false
	}

	switch env.Type {
	case protocol.TypeOpAdd:
		var p protocol.OpAddPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			c.srv.observe(metrics.DirIn, protocol.TypeOpAdd)
			c.sendError(protocol.CodeBadRequest, "op.add payload invalid", false, "")
			return true
		}
		c.srv.observe(metrics.DirIn, protocol.TypeOpAdd)
		c.rh.Op(c, &env, &p, data)
	case protocol.TypePresenceCur:
		var p protocol.CursorPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return true // malformed cursor: drop silently, no error storm
		}
		c.srv.observe(metrics.DirIn, protocol.TypePresenceCur)
		c.rh.Cursor(c, &p)
	case protocol.TypeResyncRequest:
		var p protocol.ResyncRequestPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			c.sendError(protocol.CodeBadRequest, "resync.request payload invalid", false, "")
			return true
		}
		c.srv.observe(metrics.DirIn, protocol.TypeResyncRequest)
		c.rh.Resync(c, &p)
	case protocol.TypePing:
		var p protocol.PingPayload
		_ = json.Unmarshal(env.Payload, &p)
		c.srv.observe(metrics.DirIn, protocol.TypePing)
		c.rh.Ping(c, &p)
	case protocol.TypeBye:
		c.srv.observe(metrics.DirIn, protocol.TypeBye)
		c.rh.Bye(c)
		return false
	case protocol.TypeRoomGrant:
		var p protocol.GrantPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			c.sendError(protocol.CodeBadRequest, "room.grant payload invalid", false, "")
			return true
		}
		c.srv.observe(metrics.DirIn, protocol.TypeRoomGrant)
		c.rh.Grant(c, &p)
	case protocol.TypeRoomTransfer:
		var p protocol.TransferPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			c.sendError(protocol.CodeBadRequest, "room.transfer payload invalid", false, "")
			return true
		}
		c.srv.observe(metrics.DirIn, protocol.TypeRoomTransfer)
		c.rh.Transfer(c, &p)
	}
	return true
}

// sendError emits a non-fatal protocol error frame.
func (c *conn) sendError(code protocol.ErrCode, msg string, fatal bool, opID string) {
	payload := protocol.ErrorPayload{Code: code, Message: msg, Fatal: fatal, OpID: opID}
	frame, err := protocol.Marshal(protocol.TypeError, c.params.room, c.params.cid, 0, c.srv.now().UnixMilli(), payload)
	if err != nil {
		return
	}
	c.Send(frame)
}

// writePump drains sendCh and keeps the heartbeat going.
func (c *conn) writePump() {
	ticker := time.NewTicker(c.srv.cfg.PingPeriod)
	defer ticker.Stop()
	defer c.hardClose()

	for {
		select {
		case <-c.closed:
			return
		case req := <-c.ctrlCh:
			// Flush the optional error envelope, then the close frame. This is the
			// only place that performs a close handshake, which keeps all writes on
			// one goroutine.
			if len(req.envelope) > 0 {
				_ = c.ws.SetWriteDeadline(c.srv.now().Add(c.srv.cfg.WriteWait))
				_ = c.ws.WriteMessage(websocket.TextMessage, req.envelope)
			}
			_ = c.ws.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(req.code, truncateReason(req.reason)),
				c.srv.now().Add(c.srv.cfg.WriteWait))
			c.hardClose()
			return
		case frame, ok := <-c.sendCh:
			if !ok {
				return
			}
			c.sendBytes.Add(-int64(len(frame)))
			_ = c.ws.SetWriteDeadline(c.srv.now().Add(c.srv.cfg.WriteWait))
			if err := c.ws.WriteMessage(websocket.TextMessage, frame); err != nil {
				return
			}
		case <-ticker.C:
			// A control ping keeps intermediaries from dropping an idle
			// connection and gives the read deadline something to reset against.
			if err := c.ws.WriteControl(websocket.PingMessage, nil, c.srv.now().Add(c.srv.cfg.WriteWait)); err != nil {
				return
			}
		}
	}
}

// paletteColor assigns a stable colour from the UX-SPEC palette, derived from the
// client ID so a reconnect keeps the same colour without server state.
var palette = []string{
	"#3d5afe", "#e8590c", "#12b886", "#7048e8",
	"#e64980", "#0ca678", "#f59f00", "#1c7ed6",
}

func paletteColor(cid string) string {
	// FNV-1a: stable across processes and cheap.
	var h uint32 = 2166136261
	for i := 0; i < len(cid); i++ {
		h ^= uint32(cid[i])
		h *= 16777619
	}
	return palette[int(h)%len(palette)]
}

// shortID derives a short stable user id from the client id.
func shortID(cid string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(cid); i++ {
		h ^= uint32(cid[i])
		h *= 16777619
	}
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, 4)
	for i := range out {
		out[i] = alphabet[h%36]
		h /= 36
	}
	return string(out)
}

// clientIP is unused by the ws layer (limits are per connection, not per IP) but
// kept for handshake logging.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
