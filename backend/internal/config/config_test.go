package config

import (
	"strings"
	"testing"
	"time"
)

// env builds a getenv from a map.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// baseEnv is the minimum needed for a successful load in test env.
func baseEnv() map[string]string {
	return map[string]string{
		"APP_ENV":         "test",
		"READONLY_SECRET": "unit-test-secret-at-least-32-bytes!",
	}
}

func mustLoad(t *testing.T, extra map[string]string) *Config {
	t.Helper()
	m := baseEnv()
	for k, v := range extra {
		m[k] = v
	}
	cfg, err := LoadFrom(env(m))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	return cfg
}

func TestDefaultsMatchProtocol(t *testing.T) {
	cfg := mustLoad(t, nil)

	// These values are quoted verbatim in docs/PROTOCOL.md §1.1 / §7. If one of
	// them changes here without changing the doc, the server advertises limits it
	// does not enforce.
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"MaxMessageBytes", cfg.Limits.MaxMessageBytes, 65536},
		{"MaxShapeBytes", cfg.Limits.MaxShapeBytes, 32768},
		{"MaxShapesPerRoom", cfg.Limits.MaxShapesPerRoom, 20000},
		{"OpsPerSecond", cfg.Limits.OpsPerSecond, 60},
		{"OpsBurst", cfg.Limits.OpsBurst, 120},
		{"CursorPerSecond", cfg.Limits.CursorPerSecond, 20},
		{"ResumeMaxOps", cfg.Limits.ResumeMaxOps, 2000},
		{"MaxConnPerRoom", cfg.Limits.MaxConnPerRoom, 200},
		{"MaxConnTotal", cfg.Limits.MaxConnTotal, 5000},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d (docs/PROTOCOL.md)", c.name, c.got, c.want)
		}
	}
	if cfg.Limits.ResumeMaxBytes != 4194304 {
		t.Errorf("ResumeMaxBytes = %d, want 4194304", cfg.Limits.ResumeMaxBytes)
	}
	if cfg.Limits.MaxTitleRunes != 60 || cfg.Limits.MaxNameRunes != 24 {
		t.Errorf("title/name runes = %d/%d, want 60/24", cfg.Limits.MaxTitleRunes, cfg.Limits.MaxNameRunes)
	}
	if cfg.Rooms.SnapshotShapesPerChunk != 500 {
		t.Errorf("snapshot chunk = %d, want 500", cfg.Rooms.SnapshotShapesPerChunk)
	}
	if cfg.Rooms.SnapshotMaxBytes != 48<<10 {
		t.Errorf("snapshot chunk bytes = %d, want 48KB", cfg.Rooms.SnapshotMaxBytes)
	}
	if cfg.Readonly.DefaultTTL != 24*time.Hour {
		t.Errorf("readonly default TTL = %s", cfg.Readonly.DefaultTTL)
	}
	if cfg.Readonly.MaxTTL != 7*24*time.Hour || cfg.Readonly.MinTTL != time.Minute {
		t.Errorf("readonly TTL bounds = %s..%s", cfg.Readonly.MinTTL, cfg.Readonly.MaxTTL)
	}
	if cfg.Store.Backend != BackendMemory || cfg.Broker.Backend != BackendMemory {
		t.Errorf("defaults must be zero-dependency: store=%q broker=%q", cfg.Store.Backend, cfg.Broker.Backend)
	}
	if cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("ShutdownTimeout = %s, want 5s", cfg.ShutdownTimeout)
	}
	if cfg.TrustProxyHeaders {
		t.Error("TrustProxyHeaders must default to false")
	}
}

// TestComposeEnvContract asserts every variable docker-compose.yml passes is
// accepted and applied. compose is what actually runs in the demo, so a typo here
// is a production incident.
func TestComposeEnvContract(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		"PORT":               "9090",
		"APP_ENV":            "production",
		"PUBLIC_BASE_URL":    "http://localhost:8080",
		"ALLOWED_ORIGINS":    "https://codraw.example.com",
		"ALLOW_EMPTY_ORIGIN": "false",
		"STORE":              "memory",
		"BROKER":             "memory",
		"DATABASE_URL":       "postgres://codraw:codraw@postgres:5432/codraw?sslmode=disable",
		"REDIS_URL":          "redis://redis:6379/0",
		"ROOM_TTL":           "720h",
		"READONLY_SECRET":    "compose-contract-secret-at-least-32-bytes",
		"READONLY_TTL":       "86400",
		"MAX_CONN_PER_ROOM":  "150",
		"MAX_CONN_TOTAL":     "4000",
		"OPS_PER_SECOND":     "60",
		"OPS_BURST":          "120",
		"EVENT_RETENTION":    "6000",
		"SNAPSHOT_EVERY":     "400",
		"LOG_LEVEL":          "debug",
		"METRICS_TOKEN":      "secret-token",
	})

	if cfg.HTTP.Addr != ":9090" {
		t.Errorf("PORT was not applied: addr = %q", cfg.HTTP.Addr)
	}
	if cfg.Rooms.TTL != 720*time.Hour {
		t.Errorf("ROOM_TTL = %s", cfg.Rooms.TTL)
	}
	if cfg.Readonly.DefaultTTL != 24*time.Hour {
		t.Errorf("READONLY_TTL = %s", cfg.Readonly.DefaultTTL)
	}
	if cfg.Limits.MaxConnPerRoom != 150 || cfg.Limits.MaxConnTotal != 4000 {
		t.Errorf("conn limits = %d/%d", cfg.Limits.MaxConnPerRoom, cfg.Limits.MaxConnTotal)
	}
	if cfg.Rooms.EventRetention != 6000 || cfg.Rooms.SnapshotEvery != 400 {
		t.Errorf("retention/snapshot = %d/%d", cfg.Rooms.EventRetention, cfg.Rooms.SnapshotEvery)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("LOG_LEVEL = %q", cfg.Log.Level)
	}
	if cfg.Metrics.Token != "secret-token" {
		t.Errorf("METRICS_TOKEN = %q", cfg.Metrics.Token)
	}
	if len(cfg.WS.AllowedOrigins) < 2 {
		t.Errorf("origins = %v, want the public base url plus the extra one", cfg.WS.AllowedOrigins)
	}
	if cfg.WS.AllowEmptyOrigin {
		t.Error("ALLOW_EMPTY_ORIGIN=false should be honoured")
	}
}

// TestShutdownTimeoutBareSeconds covers the root .env.example documenting
// SHUTDOWN_TIMEOUT in seconds.
func TestShutdownTimeoutBareSeconds(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"SHUTDOWN_TIMEOUT": "7"})
	if cfg.ShutdownTimeout != 7*time.Second {
		t.Fatalf("ShutdownTimeout = %s, want 7s", cfg.ShutdownTimeout)
	}
	cfg = mustLoad(t, map[string]string{"SHUTDOWN_TIMEOUT": "1500ms"})
	if cfg.ShutdownTimeout != 1500*time.Millisecond {
		t.Fatalf("ShutdownTimeout = %s", cfg.ShutdownTimeout)
	}
}

func TestLoadFailsLoudly(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantSub string
	}{
		{"postgres without url", map[string]string{"STORE": "postgres"}, "DATABASE_URL is required"},
		{"redis without url", map[string]string{"BROKER": "redis"}, "REDIS_URL is required"},
		{"unknown store", map[string]string{"STORE": "sqlite"}, "STORE="},
		{"unknown broker", map[string]string{"BROKER": "kafka"}, "BROKER="},
		{"bad log level", map[string]string{"LOG_LEVEL": "verbose"}, "LOG_LEVEL="},
		{"non numeric limit", map[string]string{"OPS_PER_SECOND": "fast"}, "OPS_PER_SECOND"},
		{"zero limit", map[string]string{"MAX_CONN_TOTAL": "0"}, "MAX_CONN_TOTAL"},
		{"bad duration", map[string]string{"ROOM_TTL": "30"}, "ROOM_TTL"},
		{"relative public url", map[string]string{"PUBLIC_BASE_URL": "/r"}, "PUBLIC_BASE_URL"},
		{"public url with query", map[string]string{"PUBLIC_BASE_URL": "https://x.dev/?a=1"}, "must not contain a query"},
		{"shape larger than message", map[string]string{"MAX_SHAPE_BYTES": "70000"}, "MAX_SHAPE_BYTES"},
		{"burst below rate", map[string]string{"OPS_PER_SECOND": "100", "OPS_BURST": "10"}, "OPS_BURST"},
		{"short readonly secret", map[string]string{"READONLY_SECRET": "short"}, "READONLY_SECRET"},
		{"retention below resume", map[string]string{"EVENT_RETENTION": "100"}, "EVENT_RETENTION"},
		{"bad port", map[string]string{"PORT": "eighty"}, "PORT="},
		{"out of range port", map[string]string{"PORT": "99999"}, "PORT="},
		{"wildcard cors in production", map[string]string{
			"APP_ENV": "production", "PUBLIC_BASE_URL": "https://x.dev", "ALLOWED_ORIGINS": "*",
		}, "ALLOWED_ORIGINS=*"},
		{"missing public base url in production", map[string]string{"APP_ENV": "production"}, "PUBLIC_BASE_URL is required"},
		{"default readonly secret in production", map[string]string{
			"APP_ENV": "production", "PUBLIC_BASE_URL": "https://x.dev",
			"READONLY_SECRET": DefaultReadonlySecret,
		}, "example value"},
		{"empty origin allowed in production", map[string]string{
			"APP_ENV": "production", "PUBLIC_BASE_URL": "https://x.dev", "ALLOW_EMPTY_ORIGIN": "true",
		}, "ALLOW_EMPTY_ORIGIN"},
		{"plaintext postgres in production", map[string]string{
			"APP_ENV": "production", "PUBLIC_BASE_URL": "https://x.dev",
			"STORE": "postgres", "DATABASE_URL": "postgres://u:p@db:5432/codraw",
		}, "sslmode=require"},
		{"invalid origin list", map[string]string{"ALLOWED_ORIGINS": "https://ok.dev,not-an-origin"}, "ALLOWED_ORIGINS"},
		{"origin with a path", map[string]string{"ALLOWED_ORIGINS": "https://ok.dev/path"}, "ALLOWED_ORIGINS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := baseEnv()
			for k, v := range tc.env {
				m[k] = v
			}
			_, err := LoadFrom(env(m))
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestLoadReportsAllErrorsAtOnce(t *testing.T) {
	m := baseEnv()
	m["STORE"] = "postgres"
	m["BROKER"] = "redis"
	m["OPS_PER_SECOND"] = "nope"
	m["MAX_CONN_TOTAL"] = "0"
	_, err := LoadFrom(env(m))
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"DATABASE_URL", "REDIS_URL", "OPS_PER_SECOND", "MAX_CONN_TOTAL"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error is missing %q:\n%s", want, msg)
		}
	}
}

func TestOriginListParsing(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		"ALLOWED_ORIGINS": " https://a.dev , https://b.dev:8443 ,",
	})
	found := map[string]bool{}
	for _, o := range cfg.WS.AllowedOrigins {
		found[o] = true
	}
	for _, want := range []string{"https://a.dev", "https://b.dev:8443"} {
		if !found[want] {
			t.Errorf("origin %q missing from %v", want, cfg.WS.AllowedOrigins)
		}
	}
}

func TestDevOriginsIncludeViteAndSelf(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"APP_ENV": "development", "PORT": "8080"})
	found := map[string]bool{}
	for _, o := range cfg.WS.AllowedOrigins {
		found[o] = true
	}
	// The Vite dev server and the server's own origin must both be allowed, or the
	// browser dev flow 403s on its own handshake.
	for _, want := range []string{"http://localhost:5173", "http://localhost:8080"} {
		if !found[want] {
			t.Errorf("development origin %q missing from %v", want, cfg.WS.AllowedOrigins)
		}
	}
}

func TestProductionHasNoDevOrigins(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		"APP_ENV":         "production",
		"PUBLIC_BASE_URL": "https://codraw.example.com",
	})
	for _, o := range cfg.WS.AllowedOrigins {
		if strings.Contains(o, "5173") {
			t.Errorf("production allowlist contains the Vite dev origin: %v", cfg.WS.AllowedOrigins)
		}
	}
	found := false
	for _, o := range cfg.WS.AllowedOrigins {
		if o == "https://codraw.example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("public base url origin missing from %v", cfg.WS.AllowedOrigins)
	}
}

func TestHTTPAddrBeatsPort(t *testing.T) {
	cfg := mustLoad(t, map[string]string{"HTTP_ADDR": "127.0.0.1:9999", "PORT": "8080"})
	if cfg.HTTP.Addr != "127.0.0.1:9999" {
		t.Fatalf("addr = %q, HTTP_ADDR should win", cfg.HTTP.Addr)
	}
}

func TestDurations(t *testing.T) {
	cfg := mustLoad(t, map[string]string{
		"DEPENDENCY_TIMEOUT":  "1s",
		"READ_HEADER_TIMEOUT": "250ms",
		"IDLE_TIMEOUT":        "2m",
		"LEAVE_GRACE":         "3s",
		"PONG_WAIT":           "90s",
	})
	if cfg.DependencyTimeout != time.Second {
		t.Errorf("DependencyTimeout = %s", cfg.DependencyTimeout)
	}
	if cfg.HTTP.ReadHeaderTimeout != 250*time.Millisecond || cfg.HTTP.IdleTimeout != 2*time.Minute {
		t.Errorf("http timeouts = %s/%s", cfg.HTTP.ReadHeaderTimeout, cfg.HTTP.IdleTimeout)
	}
	if cfg.WS.LeaveGrace != 3*time.Second || cfg.WS.PongWait != 90*time.Second {
		t.Errorf("ws timings = %s/%s", cfg.WS.LeaveGrace, cfg.WS.PongWait)
	}
}
