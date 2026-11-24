package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/codraw/codraw/backend/internal/protocol"
)

// jsonMarshal is a variable so tests can exercise the encoder failure path; by
// default it is encoding/json.
var jsonMarshal = func(v any) ([]byte, error) { return json.Marshal(v) }

// stackTrace captures the current stack for panic logs, clamped so a deep stack
// cannot blow up a log line.
func stackTrace(limit int) string {
	b := debug.Stack()
	if len(b) > limit {
		b = b[:limit]
	}
	return string(b)
}

// contextWithTimeout derives a bounded context for dependency pings.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = 3 * time.Second
	}
	return context.WithTimeout(r.Context(), d)
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// constantTimeEqual compares two secrets without leaking their length through
// timing.
func constantTimeEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// handleHealthz is the liveness probe. It must not touch the store or broker: a
// dead database should not make the orchestrator restart a healthy process and
// turn an outage into a restart loop (docs/PROTOCOL.md §1.6).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleReadyz is the readiness probe. It pings the store and the broker and
// returns 503 with a reason when either is unusable, so a load balancer can stop
// routing traffic without killing the process.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, s.deps.Config.DependencyTimeout)
	defer cancel()

	storeState, brokerState, reason := "unknown", "unknown", ""

	if s.deps.Rooms == nil {
		storeState, reason = "none", "store"
	} else if pinger, ok := s.deps.Rooms.(interface{ Backend() string }); ok {
		storeState = pinger.Backend()
	}
	if reason == "" {
		if checker, ok := s.deps.Rooms.(interface{ Ping(context.Context) error }); ok {
			if err := checker.Ping(ctx); err != nil {
				storeState, reason = "error", "store"
				LoggerFrom(r.Context(), s.deps.Logger).Warn("readiness: store ping failed", "err", err)
			}
		}
	}
	if reason == "" {
		if err := s.deps.Broker.Ping(ctx); err != nil {
			brokerState, reason = "error", "broker"
			LoggerFrom(r.Context(), s.deps.Logger).Warn("readiness: broker ping failed", "err", err)
		} else {
			brokerState = s.deps.Broker.Backend()
		}
	}

	if reason != "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready",
			"reason": reason,
			"store":  storeState,
			"broker": brokerState,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ready",
		"store":  storeState,
		"broker": brokerState,
	})
}

// handleMetrics serves the Prometheus text exposition.
//
// When METRICS_TOKEN is set the endpoint requires a bearer token. The protocol
// recommends exposing metrics only on an internal network; the token is the
// fallback for the public demo deployment.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if tok := s.deps.Config.Metrics.Token; tok != "" {
		if !constantTimeEqual(bearerToken(r), tok) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			writeError(w, r, http.StatusUnauthorized, protocol.CodeUnauthorized, "metrics token required")
			return
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := s.deps.Metrics.WriteTo(w); err != nil {
		LoggerFrom(r.Context(), s.deps.Logger).Warn("metrics write failed", "err", err)
	}
}

// opsData is the JSON body of GET /ops/data (docs/PROTOCOL.md §1.6).
type opsData struct {
	InstanceID            string  `json:"instanceId"`
	UptimeSec             int64   `json:"uptimeSec"`
	Connections           int64   `json:"connections"`
	Rooms                 int     `json:"rooms"`
	MsgPerSecIn           float64 `json:"msgPerSecIn"`
	MsgPerSecOut          float64 `json:"msgPerSecOut"`
	BroadcastLatencyP95Ms float64 `json:"broadcastLatencyP95Ms"`
	SlowConsumerKicks     int64   `json:"slowConsumerKicks"`
	Goroutines            int     `json:"goroutines"`
	HeapMB                float64 `json:"heapMB"`
	Store                 string  `json:"store"`
	Broker                string  `json:"broker"`
	Version               string  `json:"version"`
}

// handleOpsData serves the polling source for the operator panel.
func (s *Server) handleOpsData(w http.ResponseWriter, r *http.Request) {
	if tok := s.deps.Config.Metrics.Token; tok != "" {
		// Same credential acceptance as /ops: a bearer header for tools, or ?token=
		// for the panel's own fetch (a browser navigation cannot set a header).
		// constantEqualQuery compares in constant time.
		if !constantEqualQuery(r, tok) {
			writeError(w, r, http.StatusUnauthorized, protocol.CodeUnauthorized, "metrics token required")
			return
		}
	}
	writeJSON(w, http.StatusOK, s.opsSnapshot())
}

// opsSnapshot builds the panel payload from live counters.
func (s *Server) opsSnapshot() opsData {
	snap := s.deps.Metrics.Snapshot()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	storeName := "none"
	// The room service wraps the store, so it is the component that knows which
	// backend is in use.
	if backend, ok := s.deps.Rooms.(interface{ Backend() string }); ok {
		storeName = backend.Backend()
	}
	return opsData{
		InstanceID:            s.deps.Hub.InstanceID(),
		UptimeSec:             int64(snap.Uptime.Seconds()),
		Connections:           snap.WSConnections,
		Rooms:                 s.deps.Hub.RoomCount(),
		MsgPerSecIn:           snap.Rates.InPerSec,
		MsgPerSecOut:          snap.Rates.OutPerSec,
		BroadcastLatencyP95Ms: float64(snap.BroadcastP95.Microseconds()) / 1000.0,
		SlowConsumerKicks:     snap.SlowConsumerKicks,
		Goroutines:            runtime.NumGoroutine(),
		HeapMB:                float64(mem.HeapAlloc) / (1 << 20),
		Store:                 storeName,
		Broker:                s.deps.Broker.Backend(),
		Version:               s.deps.Version,
	}
}

// handleOps renders the operator panel. It is server-rendered HTML with a tiny
// inline poller: no framework, no CDN, so it keeps working when the SPA bundle is
// missing or broken.
func (s *Server) handleOps(w http.ResponseWriter, r *http.Request) {
	if tok := s.deps.Config.Metrics.Token; tok != "" {
		if !constantEqualQuery(r, tok) {
			writeError(w, r, http.StatusUnauthorized, protocol.CodeUnauthorized, "metrics token required")
			return
		}
	}
	data := s.opsSnapshot()
	snap := s.deps.Metrics.Snapshot()
	in := sortedRows(snap.MessagesIn())
	out := sortedRows(snap.MessagesOut())
	rej := sortedRows(snap.Rejected)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := opsTemplate.Execute(w, opsView{
		opsData:     data,
		MessagesIn:  in,
		MessagesOut: out,
		Rejected:    rej,
		RequestID:   RequestIDFrom(r.Context()),
	}); err != nil {
		LoggerFrom(r.Context(), s.deps.Logger).Error("ops template render failed", "err", err)
	}
}

// constantEqualQuery allows the metrics token in a query parameter for the ops
// panel, where a browser cannot easily set a bearer header on a navigation.
func constantEqualQuery(r *http.Request, want string) bool {
	got := r.URL.Query().Get("token")
	if got == "" {
		got = bearerToken(r)
	}
	return constantTimeEqual(got, want)
}

// opsView is the template model.
type opsView struct {
	opsData
	MessagesIn  []kv
	MessagesOut []kv
	Rejected    []kv
	RequestID   string
}

// kv is a sorted template row.
type kv struct {
	Key   string
	Value int64
}

// sortedRows converts a map into rows ordered by key, so the panel does not
// reshuffle on every refresh.
func sortedRows(m map[string]int64) []kv {
	if len(m) == 0 {
		return nil
	}
	out := make([]kv, 0, len(m))
	for k, v := range m {
		if v == 0 {
			continue
		}
		out = append(out, kv{Key: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// logError logs a handler error with the request ID but without the request body
// (which may contain an editor key).
func (s *Server) logError(r *http.Request, msg string, err error) {
	LoggerFrom(r.Context(), s.deps.Logger).Error(msg, "err", err, "path", r.URL.Path)
}

// writeJSON writes v with the shared content type.
func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := jsonMarshal(v)
	if err != nil {
		http.Error(w, `{"error":{"code":"internal","message":"encode failed"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// writeError emits the uniform error envelope from docs/PROTOCOL.md §1.
func writeError(w http.ResponseWriter, r *http.Request, status int, code protocol.ErrCode, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":      code,
			"message":   msg,
			"requestId": RequestIDFrom(r.Context()),
		},
	})
}

var _ = errors.Is
