// Package api wires the HTTP routes and middleware.
//
// Route table (Go 1.22+ method patterns), docs/PROTOCOL.md §1:
//
//	POST /api/rooms                                    create
//	GET  /api/rooms/{code}                             public metadata
//	POST /api/rooms/{code}/readonly-token              issue share token
//	POST /api/rooms/{code}/readonly-token/revoke       revoke all tokens
//	GET  /api/rooms/{code}/snapshot                    paged snapshot
//	GET  /api/meta                                     protocol metadata
//	GET  /healthz  /readyz  /metrics  /ops  /ops/data  operations
//	GET  /ws                                           WebSocket upgrade
//	*    /                                             embedded SPA
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"time"

	"github.com/codraw/codraw/backend/internal/auth"
	"github.com/codraw/codraw/backend/internal/broker"
	"github.com/codraw/codraw/backend/internal/config"
	"github.com/codraw/codraw/backend/internal/hub"
	"github.com/codraw/codraw/backend/internal/metrics"
)

// ErrMissingDeps is returned by New when a required collaborator is nil.
var ErrMissingDeps = errors.New("api: Logger, Metrics, Hub, Broker are required")

// RoomService is the room persistence/creation surface the REST layer needs. It
// is implemented by the store layer, keeping internal/api free of SQL and of the
// room aggregate's internals.
type RoomService interface {
	// CreateRoom generates a unique code, persists the room with hashed keys and
	// returns the plaintext keys exactly once.
	CreateRoom(ctx context.Context, title string) (*CreatedRoom, error)
	// GetRoom returns persisted metadata, or room.ErrNotFound.
	GetRoom(ctx context.Context, code string) (*RoomMeta, error)
	// VerifyKey resolves the role a presented key would hand out. Since
	// docs/PERMISSIONS.md §2.1 a room key never grants edit rights, so this is
	// always viewer; it is kept for the REST role echo.
	VerifyKey(ctx context.Context, code, key string) (role string, err error)
	// VerifyEditorKey reports whether the presented key is the room's editor
	// key, which is what authorises the readonly-token endpoints.
	VerifyEditorKey(ctx context.Context, code, key string) (bool, error)
	// VerifyCredential reports whether a room key or a readonly token is a valid
	// credential for the room. It backs the paged snapshot endpoint, which
	// exposes the drawing and therefore needs a non-anonymous credential.
	VerifyCredential(ctx context.Context, code, key, rt string) (bool, error)
	// BumpReadonlyRevocation makes every readonly token issued before now invalid.
	BumpReadonlyRevocation(ctx context.Context, code string) (revokedAt int64, err error)
	// ReadonlyWatermark returns the room's revocation watermark (ms).
	ReadonlyWatermark(ctx context.Context, code string) (int64, error)
	// Snapshot returns live shapes at a seq, paged by id.
	Snapshot(ctx context.Context, code, after string, limit int) (*SnapshotPage, error)
	// SetTitle updates the room title (P1, used by room.meta).
	SetTitle(ctx context.Context, code, title string) error
}

// CreatedRoom is the result of a successful create.
type CreatedRoom struct {
	Code      string
	Title     string
	CreatedAt time.Time
	EditorKey string
	ViewerKey string
}

// RoomMeta is the persisted room metadata exposed by GET /api/rooms/{code}.
type RoomMeta struct {
	Code         string
	Title        string
	Epoch        int64
	Seq          int64
	ShapeCount   int
	CreatedAt    time.Time
	LastActiveAt time.Time
	RevokedAt    int64
}

// SnapshotPage is one page of GET /api/rooms/{code}/snapshot.
type SnapshotPage struct {
	Seq      int64
	Epoch    int64
	Shapes   []ShapeJSON
	Next     string
	Complete bool
}

// ShapeJSON is an opaque, already-encoded shape. Keeping it as raw JSON avoids a
// decode/encode round trip on a paging endpoint whose only job is to relay bytes.
type ShapeJSON = []byte

// Deps are the collaborators the HTTP layer needs.
type Deps struct {
	Config  *config.Config
	Logger  *slog.Logger
	Metrics *metrics.Registry
	Hub     *hub.Hub
	Broker  broker.Broker
	Rooms   RoomService
	// Signer issues readonly tokens; nil disables the readonly-token endpoints.
	Signer *auth.TokenSigner
	// WebWS is the /ws handler; nil leaves /ws returning 501.
	WebWS http.Handler
	// WebUI serves the embedded SPA; nil disables static serving.
	WebUI     http.Handler
	Version   string
	BuildTime string
	Now       func() time.Time
	// EnablePprof mounts /debug/pprof/* for profiling. Off unless explicitly
	// enabled: it exposes goroutine stacks and heap profiles.
	EnablePprof bool
}

// Server is the assembled HTTP handler plus its dependencies.
type Server struct {
	deps      Deps
	mux       *http.ServeMux
	handler   http.Handler
	startedAt time.Time
	// createLimiter is the per-IP room-creation bucket (10/hour, burst 10).
	createLimiter *ipLimiter
	// readonlyLimiter protects token issuance from abuse.
	readonlyLimiter *ipLimiter
}

// New builds the HTTP handler.
//
// Middleware order, outermost first:
//
//  1. access log — logs exactly one line per request, with the request ID that
//     recover assigned, and masks secrets in the query string.
//  2. recover    — turns a panic into a 500 JSON error instead of a dropped
//     connection.
//  3. request ID — before CORS and the limiter so their rejections carry an ID.
//  4. CORS       — answers preflight before the limiter; a throttled preflight
//     would fail the request the browser has not even sent yet.
//  5. rate limit — per IP, innermost, so rejected requests cost the least.
func New(deps Deps) (*Server, error) {
	if deps.Logger == nil || deps.Metrics == nil || deps.Hub == nil || deps.Broker == nil {
		return nil, ErrMissingDeps
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	s := &Server{deps: deps, mux: http.NewServeMux(), startedAt: deps.Now()}
	rl := deps.Config.RateLimit
	s.createLimiter = newIPLimiter(rl.CreatePerHour/3600.0, rl.CreateBurst, rl.BucketIdleExpiry)
	// Token issuance shares the create bucket's shape: it is equally abusable and
	// equally rare.
	s.readonlyLimiter = newIPLimiter(rl.CreatePerHour/3600.0, rl.CreateBurst, rl.BucketIdleExpiry)
	s.routes()

	var h http.Handler = s.mux
	h = s.rateLimit(h)
	h = s.cors(h)
	h = s.requestID(h)
	h = s.recoverPanic(h)
	h = s.accessLog(h)
	s.handler = h
	return s, nil
}

// Handler returns the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// Shutdown is the hook for background workers owned by the HTTP layer.
func (s *Server) Shutdown(ctx context.Context) error {
	s.deps.Logger.Debug("http layer shutdown complete")
	return ctx.Err()
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", s.handleHealthz)
	m.HandleFunc("GET /readyz", s.handleReadyz)
	m.HandleFunc("GET /metrics", s.handleMetrics)
	m.HandleFunc("GET /ops", s.handleOps)
	m.HandleFunc("GET /ops/data", s.handleOpsData)
	m.HandleFunc("GET /api/meta", s.handleMeta)
	if s.deps.EnablePprof {
		// net/http/pprof registers on DefaultServeMux; forward only the pprof paths so
		// the rest of the default mux is not exposed.
		m.Handle("/debug/pprof/", http.DefaultServeMux)
		m.Handle("/debug/pprof/cmdline", http.DefaultServeMux)
		m.Handle("/debug/pprof/profile", http.DefaultServeMux)
		m.Handle("/debug/pprof/symbol", http.DefaultServeMux)
		m.Handle("/debug/pprof/trace", http.DefaultServeMux)
	}

	m.HandleFunc("POST /api/rooms", s.handleCreateRoom)
	m.HandleFunc("GET /api/rooms/{code}", s.handleGetRoom)
	m.HandleFunc("POST /api/rooms/{code}/readonly-token", s.handleIssueReadonly)
	m.HandleFunc("POST /api/rooms/{code}/readonly-token/revoke", s.handleRevokeReadonly)
	m.HandleFunc("GET /api/rooms/{code}/snapshot", s.handleSnapshot)

	// Unknown /api paths must be JSON 404, never the SPA (PROTOCOL §1.6).
	m.HandleFunc("/api/", s.handleAPINotFound)

	if s.deps.WebWS != nil {
		m.Handle("/ws", s.deps.WebWS)
	} else {
		m.HandleFunc("/ws", s.handleWSUnavailable)
	}
	if s.deps.WebUI != nil {
		m.Handle("/", s.deps.WebUI)
	}
}
