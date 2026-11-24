package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RequestIDHeader is the response header carrying the request ID. The SPA uses it
// to correlate a user-visible error with a server log line.
const RequestIDHeader = "X-Request-Id"

// RequestIDKey is the context key for the request ID. It is exported because the
// WebSocket layer reads it from the same context without importing this package.
type RequestIDKey struct{}

// RequestIDFrom returns the request ID stored in ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(RequestIDKey{}).(string)
	return id
}

// WithRequestID stores id in ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, RequestIDKey{}, id)
}

type loggerKey struct{}

// LoggerFrom returns the request-scoped logger carrying the request ID.
func LoggerFrom(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return fallback
}

// requestID middleware. An inbound X-Request-Id is trusted only when it looks
// like one of ours; otherwise a client could inject arbitrary text into logs
// (log forging), so the value is restricted to a safe charset and length.
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(RequestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		ctx := WithRequestID(r.Context(), id)
		ctx = context.WithValue(ctx, loggerKey{}, s.deps.Logger.With("request_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failed CSPRNG must not take request handling down; the clock is still
		// unique enough to correlate a single request.
		return "req_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "req_" + hex.EncodeToString(b[:])
}

func sanitizeRequestID(in string) string {
	if len(in) < 8 || len(in) > 64 {
		return ""
	}
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return ""
		}
	}
	return in
}

// statusRecorder captures the status code and byte count for the access log
// without disturbing optional interfaces the handler may rely on.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

// Flush forwards to the underlying writer; /ops uses it if it ever streams.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer. This is required for WebSocket
// upgrades: gorilla/websocket type-asserts the ResponseWriter to http.Hijacker,
// and a wrapper that does not forward it makes every upgrade fail with
// "response does not implement http.Hijacker".
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("api: response writer does not support hijacking")
	}
	// The connection is taken over by the WebSocket layer, so the status recorded
	// here is the pre-upgrade one (101 is written by gorilla directly).
	r.wroteHeader = true
	r.status = http.StatusSwitchingProtocols
	return h.Hijack()
}

// Unwrap exposes the original writer to http.ResponseController.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// accessLog emits one structured line per request.
//
// Privacy: the query string is logged with key/rt/name/cid redacted. Edit and
// viewer keys appear in share URLs by design and would otherwise land in every
// log sink (docs/PROTOCOL.md §2.1 requires exactly this).
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.deps.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		dur := s.deps.Now().Sub(start)
		s.deps.Metrics.ObserveHTTP(dur)

		// Health and metric probes are noisy at 1 line / 15s / instance; keep them
		// at debug so an incident view is not drowned.
		lvl := slog.LevelInfo
		switch {
		case rec.status >= 500:
			lvl = slog.LevelError
		case rec.status >= 400:
			lvl = slog.LevelWarn
		case r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics":
			lvl = slog.LevelDebug
		}
		LoggerFrom(r.Context(), s.deps.Logger).Log(r.Context(), lvl, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", redactQuery(r.URL.RawQuery),
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", float64(dur.Microseconds())/1000.0,
			"ip", s.clientIP(r),
			"user_agent", truncate(r.UserAgent(), 200),
		)
	})
}

// redactedParams are query parameters whose values must never be logged.
var redactedParams = map[string]struct{}{
	"key":       {}, // editorKey / viewerKey
	"rt":        {}, // readonly token
	"k":         {}, // legacy short form used in share URLs
	"ro":        {},
	"name":      {}, // nickname is personal data (PRD §2)
	"cid":       {}, // stable per-tab device identifier
	"editorKey": {},
	"token":     {}, // /ops and /metrics bearer token passed as a query parameter
}

// redactQuery rewrites sensitive query values to "***" and truncates the result
// so a hostile long query cannot blow up a log line.
func redactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		name := p
		if j := strings.IndexByte(p, '='); j >= 0 {
			name = p[:j]
		}
		if _, ok := redactedParams[strings.ToLower(name)]; ok {
			parts[i] = name + "=***"
		}
	}
	return truncate(strings.Join(parts, "&"), 300)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary so the log stays valid UTF-8.
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

// recoverPanic converts a handler panic into a 500 with a JSON body, and records
// it with the request ID so the operator can find the full stack.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rv := recover()
			if rv == nil {
				return
			}
			if rv == http.ErrAbortHandler {
				// Documented sentinel: the handler wants the connection dropped.
				panic(rv)
			}
			LoggerFrom(r.Context(), s.deps.Logger).Error("panic recovered",
				"panic", rv,
				"path", r.URL.Path,
				"stack", stackTrace(3<<10),
			)
			writeError(w, r, http.StatusInternalServerError, "internal", "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}

// cors answers preflight and echoes the origin only when it is explicitly
// allowed. Wildcard is supported for local development but rejected in production
// by config validation.
func (s *Server) cors(next http.Handler) http.Handler {
	allowed := s.deps.Config.CORS.AllowedOrigins
	if len(allowed) == 0 {
		return next
	}
	set := make(map[string]struct{}, len(allowed))
	star := false
	for _, o := range allowed {
		if o == "*" {
			star = true
			continue
		}
		set[o] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			_, ok := set[origin]
			if star || ok {
				h := w.Header()
				if star {
					h.Set("Access-Control-Allow-Origin", "*")
				} else {
					h.Set("Access-Control-Allow-Origin", origin)
					// Vary is mandatory when echoing an origin, otherwise a shared
					// cache can serve one origin's response to another.
					h.Add("Vary", "Origin")
				}
				h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Room-Key, "+RequestIDHeader)
				h.Set("Access-Control-Expose-Headers", RequestIDHeader)
				h.Set("Access-Control-Max-Age", "600")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP extracts the peer address. Forwarded headers are honoured only when
// TRUST_PROXY_HEADERS is set, because otherwise any client can spoof its IP and
// evade per-IP limits.
func (s *Server) clientIP(r *http.Request) string {
	if s.deps.Config.TrustProxyHeaders {
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			first, _, _ := strings.Cut(v, ",")
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				return ip.String()
			}
		}
		if v := r.Header.Get("X-Real-Ip"); v != "" {
			if ip := net.ParseIP(strings.TrimSpace(v)); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// bucket is one token bucket.
type bucket struct {
	tokens float64
	last   time.Time
	seen   time.Time
}

// ipLimiter is a map of per-key token buckets with lazy expiry.
//
// It is in-process by design: a Redis-backed limiter would put a network round
// trip on every request, and a per-instance bound still caps the damage one
// client can do to one process. The mutex is fine because the critical section is
// a few arithmetic operations.
type ipLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*bucket
	rate       float64
	burst      float64
	ttl        time.Duration
	calls      uint64
	sweepEvery uint64
}

func newIPLimiter(rate float64, burst int, ttl time.Duration) *ipLimiter {
	if rate <= 0 {
		rate = 1
	}
	if burst <= 0 {
		burst = 1
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &ipLimiter{
		buckets:    make(map[string]*bucket),
		rate:       rate,
		burst:      float64(burst),
		ttl:        ttl,
		sweepEvery: 4096,
	}
}

// allow consumes a token for key. It returns whether the request may proceed and
// how long the caller should wait, which becomes Retry-After.
func (l *ipLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.calls++
	if l.calls%l.sweepEvery == 0 {
		l.sweep(now)
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.seen = now
	if now.After(b.last) {
		b.tokens += now.Sub(b.last).Seconds() * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
		if wait < time.Second {
			wait = time.Second
		}
		return false, wait
	}
	b.tokens--
	return true, 0
}

// sweep drops buckets idle longer than ttl; at that point a full bucket is
// indistinguishable from no bucket at all.
func (l *ipLimiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.seen) > l.ttl {
			delete(l.buckets, k)
		}
	}
}

// isLimitedPath reports whether p belongs to the surface the per-IP bucket
// protects.
func isLimitedPath(p string) bool {
	switch {
	case p == "/ws", p == "/ops", p == "/ops/data":
		return true
	case p == "/api" || strings.HasPrefix(p, "/api/"):
		return true
	default:
		return false
	}
}

// rateLimit applies the per-IP bucket to the API surface only.
//
// Static assets are exempt: one SPA page load legitimately fetches dozens of
// hashed chunks in parallel, and throttling them shows up as a half-styled page.
// Health probes are exempt because a throttled probe looks exactly like an outage
// to the orchestrator.
func (s *Server) rateLimit(next http.Handler) http.Handler {
	rl := s.deps.Config.RateLimit
	limiter := newIPLimiter(rl.APIPerSecond, rl.APIBurst, rl.BucketIdleExpiry)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLimitedPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ok, wait := limiter.allow(s.clientIP(r), s.deps.Now())
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.5)))
			writeError(w, r, http.StatusTooManyRequests, "rate_limited", "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}
