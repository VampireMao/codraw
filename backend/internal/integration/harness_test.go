// Package integration spins up the real HTTP + WebSocket stack in-process and
// drives it with real client connections.
//
// The tests deliberately go through HTTP and a WebSocket dial rather than calling
// internal functions: the bugs this project actually hit (a missing Hijacker, an
// ack sent twice, a 500 instead of a 403) were all wiring bugs that only a real
// round trip catches.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/codraw/codraw/backend/internal/api"
	"github.com/codraw/codraw/backend/internal/auth"
	brokermemory "github.com/codraw/codraw/backend/internal/broker/memory"
	"github.com/codraw/codraw/backend/internal/config"
	"github.com/codraw/codraw/backend/internal/hub"
	"github.com/codraw/codraw/backend/internal/metrics"
	"github.com/codraw/codraw/backend/internal/protocol"
	"github.com/codraw/codraw/backend/internal/room"
	"github.com/codraw/codraw/backend/internal/roomsvc"
	storememory "github.com/codraw/codraw/backend/internal/store/memory"
	"github.com/codraw/codraw/backend/internal/ws"
)

// stack is a running server for one test.
type stack struct {
	t       *testing.T
	htt     *httptest.Server
	hub     *hub.Hub
	store   *storememory.Store
	broker  *brokermemory.Broker
	reg     *metrics.Registry
	cfg     *config.Config
	signer  *auth.TokenSigner
	wsSrv   *ws.Server
	baseURL string
}

// newStack builds the production wiring, including the real WebSocket handler.
// The origin allowlist defaults to the test server's own origin.
func newStack(t *testing.T, mutate func(*config.Config)) *stack {
	t.Helper()
	return newStackFull(t, mutate, nil, io.Discard)
}

// newStackWithLogWriter is newStack with the server log written to w, so a test can
// assert on access-log redaction.
func newStackWithLogWriter(t *testing.T, mutate func(*config.Config), w io.Writer) *stack {
	t.Helper()
	return newStackFull(t, mutate, nil, w)
}

// newStackOrigins is newStack with an explicit origin allowlist, for the tests that
// exercise the CSWSH policy.
func newStackOrigins(t *testing.T, mutate func(*config.Config), origins []string) *stack {
	t.Helper()
	return newStackFull(t, mutate, origins, io.Discard)
}

// newStackFull is the single implementation. origins == nil means "use the test
// server's own origin"; logWriter receives the server's structured log.
func newStackFull(t *testing.T, mutate func(*config.Config), origins []string, logWriter io.Writer) *stack {
	t.Helper()

	const secret = "test-only-readonly-secret-32-bytes!"
	cfg, err := config.LoadFrom(func(key string) string {
		switch key {
		case "APP_ENV":
			return "test"
		case "STORAGE":
			return ""
		case "STORE":
			return "memory"
		case "BROKER":
			return "memory"
		case "READONLY_SECRET":
			return secret
		case "PUBLIC_BASE_URL":
			return "http://localhost"
		case "LOG_LEVEL":
			return "error"
		case "ALLOW_EMPTY_ORIGIN":
			return "true"
		case "ALLOWED_ORIGINS":
			// The test server binds an ephemeral port, so the allowed origin is only
			// known after it starts; mutated below.
			return ""
		}
		return ""
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if mutate != nil {
		mutate(cfg)
	}

	reg := metrics.New("test")
	st := storememory.New(storememory.Options{})
	br := brokermemory.New()
	signer, err := auth.NewTokenSigner(cfg.Readonly.Secret)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	limits := room.Limits{
		MaxConnPerRoom:   cfg.Limits.MaxConnPerRoom,
		MaxConnTotal:     cfg.Limits.MaxConnTotal,
		MaxMessageBytes:  cfg.Limits.MaxMessageBytes,
		MaxShapeBytes:    cfg.Limits.MaxShapeBytes,
		MaxShapesPerRoom: cfg.Limits.MaxShapesPerRoom,
		OpsPerSecond:     cfg.Limits.OpsPerSecond,
		OpsBurst:         cfg.Limits.OpsBurst,
		CursorPerSecond:  cfg.Limits.CursorPerSecond,
		ResumeMaxOps:     cfg.Limits.ResumeMaxOps,
		ResumeMaxBytes:   cfg.Limits.ResumeMaxBytes,
		MaxNameRunes:     cfg.Limits.MaxNameRunes,
	}
	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: slog.LevelInfo}))

	factory := func(ctx context.Context, rec *room.Record) (*room.Handle, error) {
		return room.NewWithRecord(room.Options{
			Code:               rec.Code,
			Title:              rec.Title,
			Epoch:              rec.Epoch,
			Seq:                rec.Seq,
			Limits:             limits,
			Retention:          cfg.Rooms.EventRetention,
			LeaveGrace:         cfg.WS.LeaveGrace,
			CursorThrottle:     cfg.WS.CursorThrottle,
			SnapshotEvery:      cfg.Rooms.SnapshotEvery,
			SnapshotChunkShape: cfg.Rooms.SnapshotShapesPerChunk,
			SnapshotChunkBytes: cfg.Rooms.SnapshotMaxBytes,
			Persist:            st,
			InstanceID:         "i_test",
			Logger:             logger,
			Metrics:            reg,
			OnShapeCount:       func(string, int) {},
		}, rec)
	}

	h, err := hub.New(hub.Options{
		Metrics:    reg,
		Logger:     logger,
		Limits:     limits,
		InstanceID: "i_test",
		Source:     testSource{st: st},
		Factory:    factory,
	})
	if err != nil {
		t.Fatalf("hub: %v", err)
	}

	svc, err := roomsvc.New(roomsvc.Options{
		Store:   st,
		Hub:     h,
		Factory: factory,
		Config:  cfg,
		Logger:  logger,
		Signer:  signer,
	})
	if err != nil {
		t.Fatalf("roomsvc: %v", err)
	}

	// Start the listener first: the WebSocket origin allowlist must contain the
	// exact origin the test clients send, and httptest picks the port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	origin := "http://" + ln.Addr().String()
	// The config loader seeds development defaults (localhost:8080, :5173), which do
	// not include this ephemeral port, so the allowlist is replaced with either the
	// caller's explicit list or the test server's own origin.
	if origins != nil {
		cfg.WS.AllowedOrigins = origins
	} else {
		cfg.WS.AllowedOrigins = []string{origin}
	}

	wsSrv, err := ws.New(h, ws.Config{
		AllowedOrigins:   cfg.WS.AllowedOrigins,
		AllowEmptyOrigin: cfg.WS.AllowEmptyOrigin,
		Limits:           limits,
		LimitsPayload: protocol.LimitsPayload{
			MaxMessageBytes:  cfg.Limits.MaxMessageBytes,
			MaxShapeBytes:    cfg.Limits.MaxShapeBytes,
			MaxShapesPerRoom: cfg.Limits.MaxShapesPerRoom,
			OpsPerSecond:     cfg.Limits.OpsPerSecond,
			OpsBurst:         cfg.Limits.OpsBurst,
			CursorPerSecond:  cfg.Limits.CursorPerSecond,
		},
		InstanceID:     "i_test",
		MessageBytes:   int64(cfg.Limits.MaxMessageBytes),
		HelloTimeout:   cfg.WS.HelloTimeout,
		PongWait:       cfg.WS.PongWait,
		PingPeriod:     20 * time.Second,
		WriteWait:      5 * time.Second,
		SendQueue:      cfg.WS.SendQueue,
		SendQueueBytes: cfg.WS.SendQueueBytes,
		LeaveGrace:     50 * time.Millisecond, // shorten so leave tests are fast
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	wsSrv.SetMetrics(reg)
	wsSrv.SetRoleResolver(func(ctx context.Context, code, key, rt string) (string, error) {
		editorHash, viewerHash, revokedAt, err := st.VerifyKeyHash(ctx, code)
		if err != nil {
			return "", err
		}
		if rt != "" {
			if _, err := signer.Verify(rt, code, revokedAt, time.Now()); err != nil {
				return "", ws.ErrForbidden
			}
			return protocol.RoleViewer, nil
		}
		// A room key is an invitation credential only; the room decides the
		// effective role once it sees its own ownership state.
		_, _ = auth.RoleForKey(key, editorHash, viewerHash)
		return protocol.RoleViewer, nil
	})

	srv, err := api.New(api.Deps{
		Config:  cfg,
		Logger:  logger,
		Metrics: reg,
		Hub:     h,
		Broker:  br,
		Rooms:   svc,
		Signer:  signer,
		WebWS:   wsSrv,
		Version: "test",
	})
	if err != nil {
		t.Fatalf("api: %v", err)
	}

	htt := &httptest.Server{Listener: ln, Config: &http.Server{Handler: srv.Handler()}}
	htt.Start()
	t.Cleanup(func() {
		htt.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = h.Close()
		_ = br.Close()
		_ = st.Close()
		_ = ctx
	})

	return &stack{
		t: t, htt: htt, hub: h, store: st, broker: br, reg: reg,
		cfg: cfg, signer: signer, wsSrv: wsSrv, baseURL: htt.URL,
	}
}

// testSource adapts the memory store to hub.RoomSource.
type testSource struct{ st *storememory.Store }

func (s testSource) LoadOrCreateRoom(ctx context.Context, code string) (*room.Record, error) {
	return s.st.LoadRoom(ctx, code)
}

// createdRoom is the parsed response of POST /api/rooms.
type createdRoom struct {
	Code      string
	EditorKey string
	ViewerKey string
}

// createRoom calls the real REST endpoint.
func (s *stack) createRoom(title string) createdRoom {
	s.t.Helper()
	body := strings.NewReader(fmt.Sprintf(`{"title":%q}`, title))
	res, err := http.Post(s.baseURL+"/api/rooms", "application/json", body)
	if err != nil {
		s.t.Fatalf("create room: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusCreated {
		s.t.Fatalf("create room status = %d: %s", res.StatusCode, raw)
	}
	var payload struct {
		Room struct {
			Code string `json:"code"`
		} `json:"room"`
		Keys struct {
			EditorKey string `json:"editorKey"`
			ViewerKey string `json:"viewerKey"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		s.t.Fatalf("create room JSON: %v", err)
	}
	return createdRoom{Code: payload.Room.Code, EditorKey: payload.Keys.EditorKey, ViewerKey: payload.Keys.ViewerKey}
}

// client is one real WebSocket connection.
type client struct {
	t    *testing.T
	conn *websocket.Conn
	// frames is every envelope received so far, in order.
	frames []protocol.Envelope
	mu     sync.Mutex
}

// enableSessions turns on server-issued client identities for this stack. Tests
// that do not call it keep the legacy cid-based handshake, so the existing
// suite stays focused on its own subject.
func (s *stack) enableSessions() {
	s.t.Helper()
	s.wsSrv.SetSessionSigner(s.signer, time.Hour)
}

// dial opens a connection with the given query.
func (s *stack) dial(roomCode, cid, name, key, rt string) *client {
	s.t.Helper()
	return s.dialQuery(roomCode, cid, name, key, rt, "")
}

// dialWithSession opens a connection presenting a session token.
func (s *stack) dialWithSession(roomCode, cid, name, st string) *client {
	s.t.Helper()
	return s.dialQuery(roomCode, cid, name, "", "", st)
}

func (s *stack) dialQuery(roomCode, cid, name, key, rt, st string) *client {
	s.t.Helper()
	u := strings.Replace(s.baseURL, "http://", "ws://", 1) + "/ws"
	q := fmt.Sprintf("?room=%s&cid=%s&name=%s", roomCode, cid, name)
	if rt != "" {
		q += "&rt=" + rt
	} else if key != "" {
		q += "&key=" + key
	}
	if st != "" {
		q += "&st=" + st
	}
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	hdr := http.Header{"Origin": []string{s.baseURL}}
	conn, res, err := dialer.Dial(u+q, hdr)
	if err != nil {
		body := ""
		if res != nil {
			raw, _ := io.ReadAll(io.LimitReader(res.Body, 512))
			_ = res.Body.Close()
			body = fmt.Sprintf(" status=%d body=%s", res.StatusCode, raw)
		}
		s.t.Fatalf("dial: %v%s", err, body)
	}
	c := &client{t: s.t, conn: conn}
	s.t.Cleanup(func() { _ = c.conn.Close() })
	return c
}

// dialExpectError opens a connection and returns the HTTP status of a rejected
// handshake. It fails the test if the handshake unexpectedly succeeds.
func (s *stack) dialExpectError(roomCode, cid, key, rt, origin string) int {
	s.t.Helper()
	u := strings.Replace(s.baseURL, "http://", "ws://", 1) + "/ws"
	q := fmt.Sprintf("?room=%s&cid=%s", roomCode, cid)
	if rt != "" {
		q += "&rt=" + rt
	} else if key != "" {
		q += "&key=" + key
	}
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	hdr := http.Header{}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	conn, res, err := dialer.Dial(u+q, hdr)
	if err == nil {
		_ = conn.Close()
		s.t.Fatalf("expected handshake to fail, but it succeeded")
	}
	if res == nil {
		s.t.Fatalf("handshake failed without an HTTP response: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	return res.StatusCode
}

// send writes one envelope.
func (c *client) send(typ, roomCode, cid string, payload any) {
	c.t.Helper()
	frame, err := protocol.Marshal(typ, roomCode, cid, 0, time.Now().UnixMilli(), payload)
	if err != nil {
		c.t.Fatalf("encode %s: %v", typ, err)
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		c.t.Fatalf("write %s: %v", typ, err)
	}
}

// sendRaw writes raw bytes (for oversize-frame tests).
func (c *client) sendRaw(b []byte) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.conn.WriteMessage(websocket.TextMessage, b)
}

// read returns the next envelope.
func (c *client) read(timeout time.Duration) (protocol.Envelope, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	_, raw, err := c.conn.ReadMessage()
	if err != nil {
		return protocol.Envelope{}, err
	}
	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return protocol.Envelope{}, fmt.Errorf("bad JSON: %w", err)
	}
	c.mu.Lock()
	c.frames = append(c.frames, env)
	c.mu.Unlock()
	return env, nil
}

// waitFor reads until an envelope of the given type arrives, discarding others.
func (c *client) waitFor(typ string, timeout time.Duration) (protocol.Envelope, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		env, err := c.read(time.Until(deadline))
		if err != nil {
			return protocol.Envelope{}, err
		}
		if env.Type == typ {
			return env, nil
		}
	}
	return protocol.Envelope{}, fmt.Errorf("timed out waiting for %s", typ)
}

// waitForAck reads until an ack for opID arrives.
func (c *client) waitForAck(opID string, timeout time.Duration) (protocol.AckPayload, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		env, err := c.read(time.Until(deadline))
		if err != nil {
			return protocol.AckPayload{}, err
		}
		if env.Type != protocol.TypeAck {
			continue
		}
		var p protocol.AckPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return protocol.AckPayload{}, err
		}
		if p.OpID == opID {
			return p, nil
		}
	}
	return protocol.AckPayload{}, fmt.Errorf("timed out waiting for ack %s", opID)
}

// join performs the handshake and drives the connection to `live`: send hello
// immediately (concurrently with the server's joined), then consume the resync.
func (c *client) join(roomCode, cid string, lastSeq, epoch int64) protocol.JoinedPayload {
	c.t.Helper()
	// Hello goes out immediately: the protocol says neither side waits.
	c.send(protocol.TypeHello, roomCode, cid, protocol.HelloPayload{LastSeq: lastSeq, Epoch: epoch})

	env, err := c.waitFor(protocol.TypeJoined, 5*time.Second)
	if err != nil {
		c.t.Fatalf("joined: %v", err)
	}
	var joined protocol.JoinedPayload
	if err := json.Unmarshal(env.Payload, &joined); err != nil {
		c.t.Fatalf("joined payload: %v", err)
	}
	// Drain until the resync response is complete/absent.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		env, err := c.read(time.Until(deadline))
		if err != nil {
			break
		}
		switch env.Type {
		case protocol.TypeResyncSnapshot:
			var p protocol.SnapshotPayload
			_ = json.Unmarshal(env.Payload, &p)
			if p.Complete {
				return joined
			}
		case protocol.TypeResyncEvents:
			return joined
		}
	}
	c.t.Fatalf("no resync response after joined")
	return joined
}

// rectShape builds a valid rect shape with a unique id.
func rectShape(id string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"id":%q,"kind":"rect",
		"geom":{"x":10,"y":20,"w":100,"h":50},
		"style":{"stroke":"#16181d","fill":"none","strokeWidth":2,"opacity":1,"dash":"solid"},
		"data":{}
	}`, id))
}

// shapeID makes a unique shape id matching ^sh_[A-Za-z0-9_-]{6,32}$.
func shapeID(suffix string) string {
	return "sh_test" + strings.Repeat("0", max(0, 8-len(suffix))) + suffix
}

// opID makes an id matching ^[A-Za-z0-9_-]{8,40}$.
func opID(suffix string) string {
	return "op_" + strings.Repeat("0", max(0, 10-len(suffix))) + suffix
}

// textShape builds a valid text shape; used by the backpressure test because a
// text shape can carry kilobytes of payload per op.
func textShape(id, text string) json.RawMessage {
	blob, _ := json.Marshal(map[string]any{
		"id":   id,
		"kind": "text",
		"geom": map[string]any{"x": 0, "y": 0, "w": 200, "h": 100},
		"style": map[string]any{
			"stroke": "#16181d", "fill": "none", "strokeWidth": 2, "opacity": 1, "dash": "solid",
		},
		"data": map[string]any{},
		"text": text,
	})
	return blob
}
