// Package config loads and validates the server configuration from environment
// variables.
//
// The process deliberately does not read .env files: the binary is meant to be a
// self-contained artifact, so configuration comes from the process environment
// (systemd EnvironmentFile, a shell, Kubernetes, or the Windows environment).
// Only os.Getenv is consulted, so behaviour is identical in every environment and
// no secret file can leak in by accident.
//
// Variable names are dictated by the root .env.example, which the architect owns.
package config

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment names accepted by APP_ENV.
const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
	EnvTest        = "test"
)

// Backend names accepted by STORE and BROKER.
const (
	BackendMemory   = "memory"
	BackendPostgres = "postgres"
	BackendRedis    = "redis"
)

// Defaults. Protocol-derived limits come from docs/PROTOCOL.md §1.1 and §7 and
// must match the `limits` block returned by POST /api/rooms; changing one
// without the other makes the server advertise limits it does not enforce.
const (
	DefaultPort              = 8080
	DefaultHTTPAddr          = ":8080"
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultIdleTimeout       = 120 * time.Second
	DefaultShutdownTimeout   = 5 * time.Second
	DefaultDependencyTimeout = 3 * time.Second
	DefaultRoomTTL           = 720 * time.Hour // 30 days, matches ROOM_TTL=720h
	DefaultMaxConnPerRoom    = 200
	DefaultMaxConnTotal      = 5000
	DefaultMaxMessageBytes   = 65536
	DefaultMaxShapeBytes     = 32768
	DefaultMaxShapesPerRoom  = 20000
	DefaultOpsPerSecond      = 60
	DefaultOpsBurst          = 120
	DefaultCursorPerSecond   = 20
	DefaultResumeMaxOps      = 2000
	DefaultResumeMaxBytes    = 4 << 20
	DefaultSnapshotShapes    = 500
	DefaultEventRetention    = 5000
	DefaultSnapshotEvery     = 500
	DefaultSnapshotKeep      = 3
	DefaultReadonlyTTL       = 86400 // seconds
	DefaultAPIRatePerSecond  = 20.0
	DefaultAPIRateBurst      = 40
	DefaultCreatePerHour     = 10.0
	DefaultCreateBurst       = 10
	DefaultMaxTitleRunes     = 60
	DefaultMaxNameRunes      = 24
	DefaultMinMessageBytes   = 1024
	DefaultMaxHeaderBytes    = 64 << 10
	DefaultDBMaxConns        = 20
	DefaultDBMinConns        = 2
	// DefaultReadonlySecret is the placeholder shipped in .env.example. It exists
	// so a fresh clone runs without extra setup; production must never run with
	// it, and Load() rejects it explicitly when APP_ENV=production.
	DefaultReadonlySecret = "dev-only-change-me-please-32bytes-min"

	// MinReadonlySecretBytes is the smallest acceptable HMAC key. A short key
	// makes issued readonly tokens forgeable by brute force.
	MinReadonlySecretBytes = 32
)

// randomSecret returns a fresh 32-byte HMAC key encoded as hex. It is used only
// when a development run omits READONLY_SECRET; production still requires an
// operator-supplied value so that tokens stay valid across restarts and cannot
// be forged by anyone who has read this source tree.
func randomSecret() []byte {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is not recoverable in any meaningful way, but we
		// must not panic during config loading; fall back to a time-seeded value
		// and let the caller's warning make the situation visible.
		binary.BigEndian.PutUint64(buf, uint64(time.Now().UnixNano()))
	}
	return []byte(hex.EncodeToString(buf))
}

// isDefaultReadonlySecret reports whether secret is the shipped placeholder. The
// comparison is deliberately exact: we only refuse the known-bad value rather than
// trying to judge entropy, which we cannot do reliably.
func isDefaultReadonlySecret(secret string) bool {
	return secret == DefaultReadonlySecret
}

// Config is the fully validated server configuration. It is treated as
// immutable after Load returns; handlers only read from it.
type Config struct {
	AppEnv        string
	PublicBaseURL string
	HTTP          HTTPConfig
	Log           LogConfig
	CORS          CORSConfig
	Store         StoreConfig
	Broker        BrokerConfig
	Metrics       MetricsConfig
	Limits        Limits
	Rooms         RoomsConfig
	RateLimit     RateLimitConfig
	Readonly      ReadonlyConfig
	WS            WSConfig
	// ShutdownTimeout bounds the whole graceful shutdown.
	ShutdownTimeout time.Duration
	// DependencyTimeout bounds startup and readiness pings to store/broker so a
	// hung dependency cannot block orchestrator probes forever.
	DependencyTimeout time.Duration
	// TrustProxyHeaders enables X-Forwarded-For / X-Real-IP for client IP
	// extraction. False by default: trusting these headers without a reverse
	// proxy in front lets clients bypass per-IP rate limits.
	TrustProxyHeaders bool
	// Version is injected at build time.
	Version string
}

// HTTPConfig holds net/http server tuning.
type HTTPConfig struct {
	Addr string
	// ReadHeaderTimeout guards against Slowloris. ReadTimeout and WriteTimeout
	// are intentionally zero: WebSocket connections are long-lived and a global
	// write deadline would kill them; per-connection deadlines live in internal/ws.
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
}

// LogConfig controls the slog handler.
type LogConfig struct {
	Level  string // debug|info|warn|error
	Format string // json|text
}

// CORSConfig controls the development-only CORS middleware.
type CORSConfig struct {
	AllowedOrigins []string
}

// StoreConfig selects the persistence backend.
type StoreConfig struct {
	Backend        string
	DatabaseURL    string
	ConnectTimeout time.Duration
	MaxConns       int32
	MinConns       int32
}

// BrokerConfig selects the cross-instance broadcast backend.
type BrokerConfig struct {
	Backend        string
	RedisURL       string
	ConnectTimeout time.Duration
}

// MetricsConfig protects /metrics and /ops when the process is publicly
// reachable.
type MetricsConfig struct {
	Token string
}

// Limits mirrors the protocol limits block (docs/PROTOCOL.md §1.1) so the REST
// response and the enforcement code can never drift apart.
type Limits struct {
	MaxConnPerRoom   int
	MaxConnTotal     int
	MaxMessageBytes  int
	MaxShapeBytes    int
	MaxShapesPerRoom int
	OpsPerSecond     int
	OpsBurst         int
	CursorPerSecond  int
	ResumeMaxOps     int
	ResumeMaxBytes   int64
	MaxTitleRunes    int
	MaxNameRunes     int
}

// RoomsConfig holds room lifecycle and snapshot policy.
type RoomsConfig struct {
	TTL time.Duration
	// EventRetention is the number of ops kept per room. It must be >=
	// ResumeMaxOps, otherwise a client that just reconnected cannot be caught up
	// from the log and is forced into a full snapshot.
	EventRetention int
	// SnapshotEvery is the number of applied ops between snapshots.
	SnapshotEvery int
	// SnapshotKeep is the number of snapshots retained per room.
	SnapshotKeep int
	// SnapshotShapesPerChunk is the protocol cap of shapes per resync.snapshot
	// frame (docs/PROTOCOL.md §4.2).
	SnapshotShapesPerChunk int
	// SnapshotMaxBytes caps one serialised snapshot chunk (48KB per protocol).
	SnapshotMaxBytes int
}

// RateLimitConfig holds per-IP token buckets for the REST API.
type RateLimitConfig struct {
	APIPerSecond     float64
	APIBurst         int
	CreatePerHour    float64
	CreateBurst      int
	BucketIdleExpiry time.Duration
}

// ReadonlyConfig configures signed readonly share tokens.
type ReadonlyConfig struct {
	// Secret is the HMAC-SHA256 key for readonly tokens (PROTOCOL §1.7).
	Secret []byte
	// SecretGenerated reports that Secret was auto-generated for a development
	// run instead of being supplied by the operator. Readonly links then stop
	// working after a restart, so main logs a warning.
	SecretGenerated bool
	// DefaultTTL is applied when a request omits ttlSeconds.
	DefaultTTL time.Duration
	// MaxTTL is the protocol ceiling (7 days).
	MaxTTL time.Duration
	// MinTTL rejects nonsensical short-lived tokens.
	MinTTL time.Duration
}

// WSConfig holds WebSocket-specific behavioural knobs.
type WSConfig struct {
	// AllowedOrigins is the exact-match origin allowlist assembled from
	// PUBLIC_BASE_URL, ALLOWED_ORIGINS and (in development) the Vite dev server.
	AllowedOrigins []string
	// AllowEmptyOrigin permits clients that send no Origin header. Off by
	// default because its absence is how browsers signal a non-browser client;
	// enabling it in production disables CSWSH protection.
	AllowEmptyOrigin bool
	// LeaveGrace delays presence.leave after a disconnect so a page refresh does
	// not flash the member list (docs/PROTOCOL.md §4.6).
	LeaveGrace time.Duration
	// HelloTimeout closes a connection that never sends hello.
	HelloTimeout time.Duration
	// PongWait closes a connection that sent nothing for this long (60s).
	PongWait time.Duration
	// CursorThrottle coalesces cursor frames per connection (50ms).
	CursorThrottle time.Duration
	// SendQueue is the per-connection outbound buffer depth.
	SendQueue int
	// SendQueueBytes caps the per-connection outbound buffer in bytes.
	SendQueueBytes int
	// InboxQueue is the room inbox depth.
	InboxQueue int
}

// Load reads configuration from the process environment and validates it.
// A missing required variable or an unparsable value produces an error that
// names the offending variable; startup must fail rather than fall back to an
// insecure default.
func Load() (*Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom is Load with an injectable environment lookup. It exists so tests can
// exercise every branch without mutating the process environment.
func LoadFrom(getenv func(string) string) (*Config, error) {
	l := &loader{get: getenv}

	publicBase := l.absoluteURL("PUBLIC_BASE_URL", "")
	appEnv := l.enum("APP_ENV", EnvDevelopment, EnvDevelopment, EnvProduction, EnvTest)

	cfg := &Config{
		AppEnv:        appEnv,
		PublicBaseURL: publicBase,
		HTTP: HTTPConfig{
			Addr:              l.httpAddr(),
			ReadHeaderTimeout: l.duration("READ_HEADER_TIMEOUT", DefaultReadHeaderTimeout),
			IdleTimeout:       l.duration("IDLE_TIMEOUT", DefaultIdleTimeout),
			MaxHeaderBytes:    l.positiveInt("MAX_HEADER_BYTES", DefaultMaxHeaderBytes),
		},
		Log: LogConfig{
			Level:  l.enum("LOG_LEVEL", "info", "debug", "info", "warn", "error"),
			Format: l.enum("LOG_FORMAT", "json", "json", "text"),
		},
		Store: StoreConfig{
			Backend:        l.enum("STORE", BackendMemory, BackendMemory, BackendPostgres),
			DatabaseURL:    l.optionalURL("DATABASE_URL", "postgres", "postgresql"),
			ConnectTimeout: l.duration("STORE_CONNECT_TIMEOUT", DefaultDependencyTimeout),
			MaxConns:       int32(l.positiveInt("DB_MAX_CONNS", DefaultDBMaxConns)),
			MinConns:       int32(l.positiveInt("DB_MIN_CONNS", DefaultDBMinConns)),
		},
		Broker: BrokerConfig{
			Backend:        l.enum("BROKER", BackendMemory, BackendMemory, BackendRedis),
			RedisURL:       l.optionalURL("REDIS_URL", "redis", "rediss"),
			ConnectTimeout: l.duration("BROKER_CONNECT_TIMEOUT", DefaultDependencyTimeout),
		},
		Metrics: MetricsConfig{
			Token: l.value("METRICS_TOKEN"),
		},
		Limits: Limits{
			MaxConnPerRoom:   l.positiveInt("MAX_CONN_PER_ROOM", DefaultMaxConnPerRoom),
			MaxConnTotal:     l.positiveInt("MAX_CONN_TOTAL", DefaultMaxConnTotal),
			MaxMessageBytes:  l.positiveInt("MAX_MESSAGE_BYTES", DefaultMaxMessageBytes),
			MaxShapeBytes:    l.positiveInt("MAX_SHAPE_BYTES", DefaultMaxShapeBytes),
			MaxShapesPerRoom: l.positiveInt("MAX_SHAPES_PER_ROOM", DefaultMaxShapesPerRoom),
			OpsPerSecond:     l.positiveInt("OPS_PER_SECOND", DefaultOpsPerSecond),
			OpsBurst:         l.positiveInt("OPS_BURST", DefaultOpsBurst),
			CursorPerSecond:  l.positiveInt("CURSOR_PER_SECOND", DefaultCursorPerSecond),
			ResumeMaxOps:     l.positiveInt("RESUME_MAX_OPS", DefaultResumeMaxOps),
			ResumeMaxBytes:   l.positiveInt64("RESUME_MAX_BYTES", DefaultResumeMaxBytes),
			MaxTitleRunes:    DefaultMaxTitleRunes,
			MaxNameRunes:     DefaultMaxNameRunes,
		},
		Rooms: RoomsConfig{
			TTL:                    l.durationFirst([]string{"ROOM_TTL", "ROOM_IDLE_TTL"}, DefaultRoomTTL),
			EventRetention:         l.positiveInt("EVENT_RETENTION", DefaultEventRetention),
			SnapshotEvery:          l.positiveInt("SNAPSHOT_EVERY", DefaultSnapshotEvery),
			SnapshotKeep:           l.positiveInt("SNAPSHOT_KEEP", DefaultSnapshotKeep),
			SnapshotShapesPerChunk: DefaultSnapshotShapes,
			SnapshotMaxBytes:       48 << 10,
		},
		RateLimit: RateLimitConfig{
			APIPerSecond:     l.positiveFloat("API_RATE_LIMIT_RPS", DefaultAPIRatePerSecond),
			APIBurst:         l.positiveInt("API_RATE_LIMIT_BURST", DefaultAPIRateBurst),
			CreatePerHour:    l.positiveFloat("ROOM_CREATE_RATE_PER_HOUR", DefaultCreatePerHour),
			CreateBurst:      l.positiveInt("ROOM_CREATE_BURST", DefaultCreateBurst),
			BucketIdleExpiry: l.duration("RATE_LIMIT_BUCKET_IDLE_TTL", 10*time.Minute),
		},
		ShutdownTimeout:   l.durationOrSeconds("SHUTDOWN_TIMEOUT", DefaultShutdownTimeout),
		DependencyTimeout: l.duration("DEPENDENCY_TIMEOUT", DefaultDependencyTimeout),
		TrustProxyHeaders: l.boolean("TRUST_PROXY_HEADERS", false),
		WS: WSConfig{
			AllowEmptyOrigin: l.boolean("ALLOW_EMPTY_ORIGIN", false),
			LeaveGrace:       l.duration("LEAVE_GRACE", 10*time.Second),
			HelloTimeout:     l.duration("HELLO_TIMEOUT", 10*time.Second),
			PongWait:         l.duration("PONG_WAIT", 60*time.Second),
			CursorThrottle:   l.duration("CURSOR_THROTTLE", 50*time.Millisecond),
			SendQueue:        l.positiveInt("WS_SEND_QUEUE", 4096),
			SendQueueBytes:   l.positiveInt("WS_SEND_QUEUE_BYTES", 4<<20),
			InboxQueue:       l.positiveInt("WS_INBOX_QUEUE", 8192),
		},
		Readonly: ReadonlyConfig{
			Secret:     []byte(l.value("READONLY_SECRET")),
			DefaultTTL: time.Duration(l.positiveInt("READONLY_TTL", DefaultReadonlyTTL)) * time.Second,
			MaxTTL:     7 * 24 * time.Hour,
			MinTTL:     time.Minute,
		},
	}

	// A missing READONLY_SECRET must not block a local run: the product promise is
	// that the single binary starts with no configuration at all. We generate an
	// ephemeral key for development and keep it mandatory in production, where a
	// guessable key would let anyone forge readonly links.
	if len(cfg.Readonly.Secret) == 0 && cfg.AppEnv != EnvProduction {
		cfg.Readonly.Secret = randomSecret()
		cfg.Readonly.SecretGenerated = true
	}

	cfg.WS.AllowedOrigins = l.wsOrigins(cfg)
	l.validate(cfg)

	if err := errors.Join(l.errs...); err != nil {
		return nil, fmt.Errorf("invalid configuration:\n%w", err)
	}
	return cfg, nil
}

// validate performs the cross-field checks that individual parsers cannot.
func (l *loader) validate(cfg *Config) {
	lim := &cfg.Limits
	if lim.MaxShapeBytes > lim.MaxMessageBytes {
		l.errs = append(l.errs, fmt.Errorf("MAX_SHAPE_BYTES (%d) must be <= MAX_MESSAGE_BYTES (%d)", lim.MaxShapeBytes, lim.MaxMessageBytes))
	}
	if lim.MaxMessageBytes < DefaultMinMessageBytes {
		l.errs = append(l.errs, fmt.Errorf("MAX_MESSAGE_BYTES (%d) is below the protocol floor of %d", lim.MaxMessageBytes, DefaultMinMessageBytes))
	}
	if lim.OpsBurst < lim.OpsPerSecond {
		l.errs = append(l.errs, fmt.Errorf("OPS_BURST (%d) must be >= OPS_PER_SECOND (%d)", lim.OpsBurst, lim.OpsPerSecond))
	}
	if cfg.Rooms.EventRetention < lim.ResumeMaxOps {
		// Otherwise a client that reconnects with a legal lastSeq can be pushed
		// into a full snapshot even though it asked for a delta, which is a
		// protocol-level surprise the operator can avoid by configuration.
		l.errs = append(l.errs, fmt.Errorf("EVENT_RETENTION (%d) must be >= RESUME_MAX_OPS (%d)", cfg.Rooms.EventRetention, lim.ResumeMaxOps))
	}
	if cfg.Store.Backend == BackendPostgres && cfg.Store.DatabaseURL == "" {
		l.errs = append(l.errs, errors.New("DATABASE_URL is required when STORE=postgres"))
	}
	if cfg.Broker.Backend == BackendRedis && cfg.Broker.RedisURL == "" {
		l.errs = append(l.errs, errors.New("REDIS_URL is required when BROKER=redis"))
	}
	if cfg.Store.MinConns > cfg.Store.MaxConns {
		l.errs = append(l.errs, fmt.Errorf("DB_MIN_CONNS (%d) must be <= DB_MAX_CONNS (%d)", cfg.Store.MinConns, cfg.Store.MaxConns))
	}
	if len(cfg.Readonly.Secret) < MinReadonlySecretBytes {
		l.errs = append(l.errs, fmt.Errorf("READONLY_SECRET must be at least %d bytes (generate one with: openssl rand -base64 48)", MinReadonlySecretBytes))
	}
	if cfg.AppEnv == EnvProduction {
		if cfg.PublicBaseURL == "" {
			l.errs = append(l.errs, errors.New("PUBLIC_BASE_URL is required when APP_ENV=production (share links need an absolute origin)"))
		}
		// A shipped default secret makes readonly tokens forgeable by anyone who
		// has read this repository. Compose falls back to it so that `docker
		// compose up` works out of the box, which is exactly why production must
		// refuse to start rather than inherit it silently.
		if isDefaultReadonlySecret(string(cfg.Readonly.Secret)) {
			l.errs = append(l.errs, errors.New("READONLY_SECRET is still the example value; set a unique secret in production (openssl rand -base64 48)"))
		}
		if cfg.WS.AllowEmptyOrigin {
			l.errs = append(l.errs, errors.New("ALLOW_EMPTY_ORIGIN=true is not permitted when APP_ENV=production (it disables CSWSH protection)"))
		}
		for _, o := range cfg.CORS.AllowedOrigins {
			if o == "*" {
				l.errs = append(l.errs, fmt.Errorf("ALLOWED_ORIGINS=* is not permitted when APP_ENV=production"))
			}
		}
		// The WebSocket allowlist is assembled separately (PUBLIC_BASE_URL plus
		// ALLOWED_ORIGINS), so a wildcard has to be rejected there too or the CSWSH
		// protection silently becomes "any origin".
		for _, o := range cfg.WS.AllowedOrigins {
			if o == "*" {
				l.errs = append(l.errs, fmt.Errorf("ALLOWED_ORIGINS=* is not permitted when APP_ENV=production"))
				break
			}
		}
		if cfg.Store.Backend == BackendPostgres && cfg.Store.DatabaseURL != "" &&
			!strings.Contains(cfg.Store.DatabaseURL, "sslmode=") {
			l.errs = append(l.errs, errors.New("DATABASE_URL should include sslmode=require when APP_ENV=production"))
		}
	}
	// Default origins must never be empty in development or local WS testing
	// silently 403s; PUBLIC_BASE_URL is optional there, so fall back to
	// localhost:PORT.
	if len(cfg.WS.AllowedOrigins) == 0 {
		l.errs = append(l.errs, errors.New("no allowed WebSocket origins: set PUBLIC_BASE_URL or ALLOWED_ORIGINS"))
	}
}

// loader accumulates every invalid variable so the operator sees the whole list
// in one restart instead of playing whack-a-mole.
type loader struct {
	get  func(string) string
	errs []error
}

func (l *loader) value(key string) string { return strings.TrimSpace(l.get(key)) }

func (l *loader) raw(key, def string) string {
	if v := l.value(key); v != "" {
		return v
	}
	return def
}

// httpAddr accepts HTTP_ADDR ("host:port") or PORT (an int), because compose
// passes PORT while a bare binary is easier to run with HTTP_ADDR. HTTP_ADDR
// wins when both are set.
func (l *loader) httpAddr() string {
	if v := l.value("HTTP_ADDR"); v != "" {
		return v
	}
	port := l.value("PORT")
	host := l.value("HTTP_HOST")
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || n > 65535 {
			l.errs = append(l.errs, fmt.Errorf("PORT=%q must be an integer between 1 and 65535", port))
			return DefaultHTTPAddr
		}
		// HTTP_HOST lets `--host 127.0.0.1` bind loopback only, which is what a
		// desktop user running the binary locally usually wants.
		if host != "" {
			return net.JoinHostPort(host, strconv.Itoa(n))
		}
		return ":" + strconv.Itoa(n)
	}
	if host != "" {
		return net.JoinHostPort(host, strconv.Itoa(DefaultPort))
	}
	return DefaultHTTPAddr
}

func (l *loader) enum(key, def string, allowed ...string) string {
	v := l.raw(key, def)
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	l.errs = append(l.errs, fmt.Errorf("%s=%q is not one of %s", key, v, strings.Join(allowed, ", ")))
	return def
}

func (l *loader) boolean(key string, def bool) bool {
	v := l.value(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q is not a boolean", key, v))
		return def
	}
	return b
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v := l.value(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s=%q is not a duration (e.g. 15s, 2m, 720h)", key, v))
		return def
	}
	if d <= 0 {
		l.errs = append(l.errs, fmt.Errorf("%s must be > 0, got %s", key, d))
		return def
	}
	return d
}

// durationFirst reads the first variable that is set, so an old name can stay
// working as an alias (ROOM_TTL vs ROOM_IDLE_TTL).
func (l *loader) durationFirst(keys []string, def time.Duration) time.Duration {
	for _, k := range keys {
		if l.value(k) != "" {
			return l.duration(k, def)
		}
	}
	return def
}

// durationOrSeconds accepts both "5s" and "5". The root .env.example documents
// SHUTDOWN_TIMEOUT in seconds, so a bare integer must not be a startup failure.
func (l *loader) durationOrSeconds(key string, def time.Duration) time.Duration {
	v := l.value(key)
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n <= 0 {
			l.errs = append(l.errs, fmt.Errorf("%s must be > 0, got %d", key, n))
			return def
		}
		// A bare number is seconds, capped so a typo cannot hold shutdown open
		// past the orchestrator's kill grace period.
		if n > 300 {
			l.errs = append(l.errs, fmt.Errorf("%s=%d seconds is unreasonably long (max 300)", key, n))
			return def
		}
		return time.Duration(n) * time.Second
	}
	return l.duration(key, def)
}

func (l *loader) positiveInt(key string, def int) int {
	v := l.value(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		l.errs = append(l.errs, fmt.Errorf("%s=%q must be a positive integer", key, v))
		return def
	}
	return n
}

func (l *loader) positiveInt64(key string, def int64) int64 {
	v := l.value(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		l.errs = append(l.errs, fmt.Errorf("%s=%q must be a positive integer", key, v))
		return def
	}
	return n
}

func (l *loader) positiveFloat(key string, def float64) float64 {
	v := l.value(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		l.errs = append(l.errs, fmt.Errorf("%s=%q must be a positive number", key, v))
		return def
	}
	return f
}

// absoluteURL validates an optional absolute http(s) URL. It intentionally
// rejects URLs with a query or fragment: everything the server emits is built
// from the origin only.
func (l *loader) absoluteURL(key, def string) string {
	v := l.raw(key, def)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		l.errs = append(l.errs, fmt.Errorf("%s=%q must be an absolute URL", key, v))
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		l.errs = append(l.errs, fmt.Errorf("%s=%q must use http or https", key, v))
		return ""
	}
	if u.RawQuery != "" || u.Fragment != "" {
		l.errs = append(l.errs, fmt.Errorf("%s=%q must not contain a query or fragment", key, v))
		return ""
	}
	return strings.TrimRight(u.String(), "/")
}

func (l *loader) optionalURL(key string, schemes ...string) string {
	v := l.value(key)
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		l.errs = append(l.errs, fmt.Errorf("%s must be an absolute URL with a host", key))
		return ""
	}
	for _, s := range schemes {
		if u.Scheme == s {
			return v
		}
	}
	l.errs = append(l.errs, fmt.Errorf("%s must use one of the schemes %s", key, strings.Join(schemes, ", ")))
	return ""
}

// originList accepts a comma separated list of origins (and only origins:
// "https://example.com", never a path). Empty entries are ignored so a trailing
// comma is not a startup failure.
func (l *loader) originList(key, aliasValue string) []string {
	raw := l.value(key)
	if raw == "" {
		raw = aliasValue
	}
	if raw == "" {
		return nil
	}
	var out []string
	seen := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(part)
		if origin == "" {
			continue
		}
		if origin == "*" {
			out = append(out, origin)
			continue
		}
		if _, err := parseOrigin(origin); err != nil {
			l.errs = append(l.errs, fmt.Errorf("%s contains %q which is not an origin (use scheme://host[:port])", key, origin))
			continue
		}
		if _, ok := seen[origin]; ok {
			continue
		}
		seen[origin] = struct{}{}
		out = append(out, origin)
	}
	return out
}

// wsOrigins assembles the WebSocket Origin allowlist per docs/PROTOCOL.md §2.1:
// PUBLIC_BASE_URL origin + ALLOWED_ORIGINS, plus the Vite dev server outside
// production.
func (l *loader) wsOrigins(cfg *Config) []string {
	var out []string
	seen := make(map[string]struct{})
	add := func(origin string) {
		origin = strings.TrimRight(strings.TrimSpace(origin), "/")
		if origin == "" {
			return
		}
		if _, ok := seen[origin]; ok {
			return
		}
		seen[origin] = struct{}{}
		out = append(out, origin)
	}

	if cfg.PublicBaseURL != "" {
		if o, err := parseOrigin(cfg.PublicBaseURL); err == nil {
			add(o)
		}
	}
	for _, o := range l.originList("ALLOWED_ORIGINS", "") {
		add(o)
	}
	if cfg.AppEnv != EnvProduction {
		add("http://localhost:5173")
		add("http://127.0.0.1:5173")
	}
	// A bare local run has no PUBLIC_BASE_URL; without this the browser dev flow
	// (served by this very process on :8080) would be rejected by its own origin
	// check.
	if cfg.AppEnv != EnvProduction {
		port := DefaultPort
		if a := cfg.HTTP.Addr; a != "" {
			if i := strings.LastIndexByte(a, ':'); i >= 0 {
				if n, err := strconv.Atoi(a[i+1:]); err == nil {
					port = n
				}
			}
		}
		add("http://localhost:" + strconv.Itoa(port))
		add("http://127.0.0.1:" + strconv.Itoa(port))
	}
	return out
}

// parseOrigin normalises a URL into an origin and rejects anything carrying a
// path, query or fragment.
func parseOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("missing scheme or host")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("has a path")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("has a query or fragment")
	}
	return u.Scheme + "://" + u.Host, nil
}
