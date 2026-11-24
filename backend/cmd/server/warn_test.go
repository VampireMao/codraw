package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/codraw/codraw/backend/internal/config"
)

// captureWarnings runs openStore/openBroker-style validation through a logger and
// returns everything it wrote. It exercises the deployment-constraint warning
// without starting a server or dialling Redis.
func captureWarnings(t *testing.T, cfg *config.Config) string {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// The warning lives in run(), which needs a full process; the check is factored
	// here so the behaviour is testable and the wording is pinned.
	warnIfIncoherent(logger, cfg)
	return buf.String()
}

func TestWarnsOnRedisBrokerWithMemoryStore(t *testing.T) {
	cfg := &config.Config{
		Store:  config.StoreConfig{Backend: config.BackendMemory},
		Broker: config.BrokerConfig{Backend: config.BackendRedis},
	}
	out := captureWarnings(t, cfg)
	if !strings.Contains(out, "BROKER=redis") || !strings.Contains(out, "STORE=memory") {
		t.Fatalf("expected a warning naming both settings, got: %q", out)
	}
	if !strings.Contains(strings.ToLower(out), "postgres") {
		t.Fatalf("warning should point at STORE=postgres as the fix, got: %q", out)
	}
}

func TestNoWarningForCoherentCombinations(t *testing.T) {
	cases := []config.Config{
		{Store: config.StoreConfig{Backend: config.BackendMemory}, Broker: config.BrokerConfig{Backend: config.BackendMemory}},
		{Store: config.StoreConfig{Backend: config.BackendPostgres}, Broker: config.BrokerConfig{Backend: config.BackendRedis}},
		{Store: config.StoreConfig{Backend: config.BackendPostgres}, Broker: config.BrokerConfig{Backend: config.BackendMemory}},
	}
	for _, cfg := range cases {
		if out := captureWarnings(t, &cfg); out != "" {
			t.Errorf("store=%s broker=%s produced an unexpected warning: %q",
				cfg.Store.Backend, cfg.Broker.Backend, out)
		}
	}
}
