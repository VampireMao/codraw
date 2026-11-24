// Command server is the CoDraw backend: HTTP API, embedded SPA, metrics, operator
// panel and the WebSocket collaboration hub.
//
// Startup order is dependency-first; shutdown is the exact reverse. The reasons
// are documented on run().
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/codraw/codraw/backend/internal/api"
	"github.com/codraw/codraw/backend/internal/auth"
	"github.com/codraw/codraw/backend/internal/broker"
	brokermemory "github.com/codraw/codraw/backend/internal/broker/memory"
	"github.com/codraw/codraw/backend/internal/broker/redis"
	"github.com/codraw/codraw/backend/internal/config"
	"github.com/codraw/codraw/backend/internal/hub"
	"github.com/codraw/codraw/backend/internal/logging"
	"github.com/codraw/codraw/backend/internal/metrics"
	"github.com/codraw/codraw/backend/internal/protocol"
	"github.com/codraw/codraw/backend/internal/room"
	"github.com/codraw/codraw/backend/internal/roomsvc"
	"github.com/codraw/codraw/backend/internal/store/memory"
	"github.com/codraw/codraw/backend/internal/store/postgres"
	"github.com/codraw/codraw/backend/internal/webui"
	"github.com/codraw/codraw/backend/internal/ws"
)

// Build metadata is injected at build time via -ldflags.
var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	// Command-line flags exist because the common case is double-clicking the
	// binary, where there is no shell to set environment variables in. Flags are
	// applied on top of the environment, so `PORT=8090 codraw` and
	// `codraw -p 8090` both work and the latter wins.
	overrides, rest, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, errShowHelp) {
			fmt.Fprint(os.Stdout, usageText())
			os.Exit(0)
		}
		if errors.Is(err, errShowVersion) {
			fmt.Printf("codraw %s (built %s, %s)\n", version, buildTime, runtime.Version())
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "codraw: %v\n\n%s", err, usageText())
		os.Exit(2)
	}

	// Subcommands are checked after flag parsing so that `codraw -p 8080
	// healthcheck` works too: the probe needs to know the port.
	if len(rest) > 0 {
		switch rest[0] {
		case "healthcheck":
			os.Exit(healthcheck(overrides))
		case "version":
			fmt.Printf("codraw %s (built %s, %s)\n", version, buildTime, runtime.Version())
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "codraw: unknown command %q\n\n%s", rest[0], usageText())
			os.Exit(2)
		}
	}

	code, err := run(overrides)
	if err != nil {
		// The logger may not exist yet (config failure), so report to stderr.
		fmt.Fprintf(os.Stderr, "codraw: fatal: %v\n", err)
	}
	os.Exit(code)
}

// errShowHelp and errShowVersion are sentinel "not really an error" results from
// flag parsing; main turns them into a normal exit.
var (
	errShowHelp    = errors.New("show help")
	errShowVersion = errors.New("show version")
)

// overrides carries flag values that take precedence over the environment. A nil
// field means "not specified on the command line".
type overrides struct {
	port        *string
	host        *string
	store       *string
	broker      *string
	databaseURL *string
	redisURL    *string
	secret      *string
	baseURL     *string
	origins     *string
	devMode     *bool
	prodMode    *bool
	logLevel    *string
}

// parseFlags is hand-written rather than using the flag package because the flag
// package's help output and error messages are unfriendly for a binary that users
// double-click, and because we need `-p 8080` and `--port=8080` to both work
// alongside subcommands.
func parseFlags(args []string) (overrides, []string, error) {
	var o overrides
	var rest []string

	// takeValue accepts both "-p 8080" and "-p=8080" / "--port=8080".
	i := 0
	next := func(flagName string) (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("flag %s needs a value", flagName)
		}
		i++
		return args[i], nil
	}

	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			continue
		}

		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		get := func() (string, error) {
			if hasValue {
				return value, nil
			}
			return next(a)
		}

		switch name {
		case "h", "help":
			return o, nil, errShowHelp
		case "v", "version":
			return o, nil, errShowVersion
		case "p", "port":
			v, err := get()
			if err != nil {
				return o, nil, err
			}
			o.port = &v
		case "host", "addr":
			v, err := get()
			if err != nil {
				return o, nil, err
			}
			o.host = &v
		case "store":
			v, err := get()
			if err != nil {
				return o, nil, err
			}
			o.store = &v
		case "broker":
			v, err := get()
			if err != nil {
				return o, nil, err
			}
			o.broker = &v
		case "db", "database-url":
			v, err := get()
			if err != nil {
				return o, nil, err
			}
			o.databaseURL = &v
		case "redis", "redis-url":
			v, err := get()
			if err != nil {
				return o, nil, err
			}
			o.redisURL = &v
		case "secret", "readonly-secret":
			v, err := get()
			if err != nil {
				return o, nil, err
			}
			o.secret = &v
		case "base-url":
			v, err := get()
			if err != nil {
				return o, nil, err
			}
			o.baseURL = &v
		case "origins", "allowed-origins":
			v, err := get()
			if err != nil {
				return o, nil, err
			}
			o.origins = &v
		case "log-level":
			v, err := get()
			if err != nil {
				return o, nil, err
			}
			o.logLevel = &v
		case "dev":
			t := true
			o.devMode = &t
		case "prod", "production":
			t := true
			o.prodMode = &t
		default:
			return o, nil, fmt.Errorf("unknown flag -%s", name)
		}
	}
	return o, rest, nil
}

// applyToEnv copies flag values into the process environment so that config.Load
// (which only reads the environment) sees them. This keeps a single source of
// truth for parsing and validation instead of duplicating every rule for flags.
func (o overrides) applyToEnv() {
	set := func(key string, v *string) {
		if v != nil {
			_ = os.Setenv(key, *v)
		}
	}
	set("PORT", o.port)
	set("HTTP_HOST", o.host)
	set("STORE", o.store)
	set("BROKER", o.broker)
	set("DATABASE_URL", o.databaseURL)
	set("REDIS_URL", o.redisURL)
	set("READONLY_SECRET", o.secret)
	set("PUBLIC_BASE_URL", o.baseURL)
	set("ALLOWED_ORIGINS", o.origins)
	set("LOG_LEVEL", o.logLevel)
	if o.devMode != nil && *o.devMode {
		_ = os.Setenv("APP_ENV", "development")
	}
	if o.prodMode != nil && *o.prodMode {
		_ = os.Setenv("APP_ENV", "production")
	}
}

// usageText is written by hand so it fits a terminal and answers "how do I just
// run this?" in the first three lines. It stays pure ASCII: the Windows console
// defaults to a legacy code page, where an em dash or curly quote renders as
// mojibake, and a help screen that looks broken undermines the whole first
// impression.
func usageText() string {
	return `CoDraw - realtime collaborative whiteboard (single binary)

Usage:
  ` + binaryName() + ` [flags]
  ` + binaryName() + ` healthcheck [-p PORT]
  ` + binaryName() + ` version

Run with no flags to start on http://localhost:8080 with in-memory storage.
Every flag below overrides the matching environment variable.

Flags:
  -p, --port <n>          HTTP port (default 8080, env PORT)
      --host <addr>       bind address (default all interfaces, env HTTP_HOST)
      --store <memory|postgres>       room persistence (env STORE)
      --broker <memory|redis>         cross-instance fan-out (env BROKER)
      --db <url>          Postgres DSN (env DATABASE_URL)
      --redis <url>       Redis URL (env REDIS_URL)
      --secret <s>        readonly-token HMAC key, min 32 bytes (env READONLY_SECRET)
      --base-url <url>    public origin for share links (env PUBLIC_BASE_URL)
      --origins <list>    comma-separated WebSocket origin allowlist (env ALLOWED_ORIGINS)
      --log-level <l>     debug|info|warn|error (env LOG_LEVEL)
      --dev               development mode: relaxed security, localhost origins
      --prod              production mode: strict security, requires --secret
  -h, --help              show this help
  -v, --version           show version

Examples:
  ` + binaryName() + `                          # just run it
  ` + binaryName() + ` -p 9000                  # different port
  ` + binaryName() + ` --prod --port 80 --base-url https://draw.example.com \
      --secret "$(openssl rand -base64 48)" --store postgres --db "postgres://..."
`
}

// isAddrInUse reports whether err is a "port already bound" failure. The check is
// string-based on purpose: syscall.EADDRINUSE exists on every platform Go supports,
// but wrapping and comparison differ between Windows and Unix error paths.
func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "only one usage of each socket address") // Windows
}

// portOf extracts the port from a listen address like ":8080" or "0.0.0.0:8080".
func portOf(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 && i+1 < len(addr) {
		return addr[i+1:]
	}
	return addr
}

// binaryName returns the executable name so the hint can be copy-pasted verbatim.
func binaryName() string {
	if len(os.Args) > 0 {
		if base := filepath.Base(os.Args[0]); base != "" && base != "." {
			return base
		}
	}
	return "codraw"
}

// healthcheck probes /healthz over loopback.
//
// It reads the port the same way the server does, so `codraw -p 9000 healthcheck`
// probes the instance that was started with `-p 9000`.
func healthcheck(o overrides) int {
	addr := os.Getenv("HTTP_ADDR")
	host, port := "", "8080"
	if addr != "" {
		if h, p, err := net.SplitHostPort(addr); err == nil {
			host, port = h, p
		}
	}
	if o.port != nil {
		port = *o.port
	} else if p := os.Getenv("PORT"); p != "" && addr == "" {
		port = p
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", res.StatusCode)
		return 1
	}
	return 0
}

// run is the composition root.
//
// Shutdown order (reverse of startup) is load-bearing:
//
//  1. Stop accepting new connections (HTTP server Shutdown) and flip /readyz to
//     503 by marking the hub closed, so a load balancer drains this instance.
//  2. Close every WebSocket with 1001 so clients reconnect elsewhere rather than
//     mistaking the shutdown for a network blip.
//  3. Close rooms: each room goroutine writes its final snapshot and exits. This
//     must happen before the store closes, or the last ops of the session are
//     lost.
//  4. Close the broker (unsubscribe) and then the store.
//
// Steps 3 and 4 are deferred below, in LIFO order, which gives exactly the order
// above without a hand-written teardown list.
func run(o overrides) (int, error) {
	// Flags are materialised into the environment before Load so that validation
	// lives in exactly one place (config.Load) rather than being duplicated.
	o.applyToEnv()

	cfg, err := config.Load()
	if err != nil {
		return 2, err
	}
	cfg.Version = version
	logger := logging.New(logging.Options{Level: cfg.Log.Level, Format: cfg.Log.Format})
	slog.SetDefault(logger)

	start := time.Now()
	logger.Info("starting codraw",
		"version", version,
		"build_time", buildTime,
		"go_version", runtime.Version(),
		"env", cfg.AppEnv,
		"store", cfg.Store.Backend,
		"broker", cfg.Broker.Backend,
		"addr", cfg.HTTP.Addr,
	)
	if cfg.Readonly.SecretGenerated {
		// Say it loudly: readonly links issued by this process die on restart, and
		// the operator has no idea a key was invented for them.
		logger.Warn("READONLY_SECRET was not set; generated an ephemeral one for this run",
			"impact", "readonly share links will stop working after restart",
			"fix", "set READONLY_SECRET to a stable value (openssl rand -base64 48)")
	}

	// One signal source for the process. NotifyContext cancels on the first
	// SIGINT/SIGTERM so a signal during startup also aborts it. A second
	// registration records which signal it was, because NotifyContext reports only
	// context.Canceled and the shutdown log needs the distinction.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	recv := make(chan os.Signal, 1)
	signal.Notify(recv, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(recv)
	// signalName is written before sigCtx is cancelled and read only after
	// <-sigCtx.Done(); the channel close inside cancel establishes the
	// happens-before edge, so no lock is needed.
	var signalName string
	go func() {
		select {
		case s := <-recv:
			signalName = s.String()
		case <-sigCtx.Done():
		}
	}()

	// --- Dependencies -----------------------------------------------------

	reg := metrics.New(version)
	instanceID := newInstanceID()

	st, err := openStore(sigCtx, cfg, logger)
	if err != nil {
		return 2, fmt.Errorf("open store: %w", err)
	}
	// Multi-instance coherence depends on a shared, durable log: an INCR gap (a peer
	// crashed between allocating a seq and publishing it) can only be repaired by
	// replaying from Postgres. With an in-memory store there is nothing to replay, so
	// the room stays permanently behind and clients resync into a hole. Warn rather
	// than refuse: this combination is legitimate for load testing, where a single
	// writer and disposable state are expected.
	warnIfIncoherent(logger, cfg)
	defer func() {
		if cerr := st.Close(); cerr != nil {
			logger.Error("store close failed", "err", cerr)
		} else {
			logger.Debug("store closed")
		}
	}()

	br, err := openBroker(sigCtx, cfg, instanceID, logger)
	if err != nil {
		return 2, fmt.Errorf("open broker: %w", err)
	}
	defer func() {
		if cerr := br.Close(); cerr != nil {
			logger.Error("broker close failed", "err", cerr)
		} else {
			logger.Debug("broker closed")
		}
	}()

	signer, err := auth.NewTokenSigner(cfg.Readonly.Secret)
	if err != nil {
		return 2, fmt.Errorf("readonly tokens: %w", err)
	}

	// --- Room runtime -----------------------------------------------------

	roomLimits := room.Limits{
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

	// roomFactory builds a live room from a persisted record. It wires the two
	// optional backends: Persist writes the event log, Broadcast fans out to other
	// instances. Both are nil/Noop in memory mode.
	roomFactory := func(ctx context.Context, rec *room.Record) (*room.Handle, error) {
		opts := room.Options{
			Code:               rec.Code,
			Title:              rec.Title,
			Epoch:              rec.Epoch,
			Seq:                rec.Seq,
			ShapeCount:         rec.ShapeCount,
			Limits:             roomLimits,
			Retention:          cfg.Rooms.EventRetention,
			DedupeSize:         8192,
			DedupeTTL:          10 * time.Minute,
			LeaveGrace:         cfg.WS.LeaveGrace,
			CursorThrottle:     cfg.WS.CursorThrottle,
			InboxSize:          cfg.WS.InboxQueue,
			SnapshotEvery:      cfg.Rooms.SnapshotEvery,
			SnapshotChunkShape: cfg.Rooms.SnapshotShapesPerChunk,
			SnapshotChunkBytes: cfg.Rooms.SnapshotMaxBytes,
			Persist:            st,
			InstanceID:         instanceID,
			Logger:             logger.With("room", rec.Code),
			Metrics:            reg,
			OnShapeCount: func(code string, n int) {
				// Best effort: a failed touch must never fail an op.
				if err := st.TouchRoom(context.Background(), code, n, time.Time{}); err != nil {
					logger.Debug("touch room failed", "room", code, "err", err)
				}
			},
		}
		// Only Redis needs the room to ask for a globally unique seq; in memory
		// mode the broker returns 0 and the room keeps its own counter.
		if cfg.Broker.Backend != config.BackendMemory {
			opts.SeqSource = br
			// Multi-instance rooms can miss frames published before their
			// subscription existed, so they must be able to replay a range from the
			// store. Single-instance mode has no such gaps.
			if catchUp, ok := st.(room.CatchUp); ok {
				opts.CatchUp = catchUp
			} else {
			}
		}
		opts.Broadcast = brokerAdapter{br: br, memoryMode: cfg.Broker.Backend == config.BackendMemory}
		rh, err := room.NewWithRecord(opts, rec)
		if err != nil {
			return nil, err
		}
		// Subscribe this room to broker-delivered frames from other instances. In
		// memory mode there are no peers, so the subscription is skipped entirely
		// rather than paying for a no-op channel.
		if cfg.Broker.Backend != config.BackendMemory {
			// context.Background, not ctx: this subscription is owned by the room's
			// lifetime, and ctx is the HTTP request context, which is cancelled the
			// moment the WebSocket upgrade returns. Binding to it would silently tear
			// down cross-instance delivery, leaving every instance with only its own
			// writes.
			_, err := br.Subscribe(context.Background(), rec.Code, func(msg broker.Message) {
				rh.BroadcastRemote(msg.Seq, msg.Payload)
			})
			if err != nil {
				// A room that cannot hear its peers would serve stale state; fail the
				// open so the client retries against a healthier instance.
				_ = rh.Close(ctx)
				return nil, fmt.Errorf("subscribe room %s: %w", rec.Code, err)
			}
		}
		return rh, nil
	}

	h, err := hub.New(hub.Options{
		Metrics:    reg,
		Logger:     logger.With("component", "hub"),
		Limits:     roomLimits,
		InstanceID: instanceID,
		Source:     roomSource{store: st},
		Factory:    roomFactory,
	})
	if err != nil {
		return 2, fmt.Errorf("init hub: %w", err)
	}
	defer func() {
		if cerr := h.Close(); cerr != nil {
			logger.Error("hub close failed", "err", cerr)
		} else {
			logger.Debug("hub closed")
		}
	}()

	// Idle-room reaper: releases rooms nobody has touched for ROOM_TTL. Without it
	// a long-lived demo instance keeps one shape map per room forever.
	reaperCtx, stopReaper := context.WithCancel(context.Background())
	defer stopReaper()
	if reaper := hub.NewReaper(h, cfg.Rooms.TTL, logger.With("component", "reaper"), nil); reaper != nil {
		go reaper.Run(reaperCtx)
	}

	svc, err := roomsvc.New(roomsvc.Options{
		Store:   st,
		Hub:     h,
		Factory: roomFactory,
		Config:  cfg,
		Logger:  logger.With("component", "roomsvc"),
		Signer:  signer,
	})
	if err != nil {
		return 2, fmt.Errorf("init room service: %w", err)
	}

	// --- WebSocket + HTTP -------------------------------------------------

	wsSrv, err := ws.New(h, ws.Config{
		AllowedOrigins:   cfg.WS.AllowedOrigins,
		AllowEmptyOrigin: cfg.WS.AllowEmptyOrigin,
		Limits:           roomLimits,
		LimitsPayload: protocol.LimitsPayload{
			MaxMessageBytes:  cfg.Limits.MaxMessageBytes,
			MaxShapeBytes:    cfg.Limits.MaxShapeBytes,
			MaxShapesPerRoom: cfg.Limits.MaxShapesPerRoom,
			OpsPerSecond:     cfg.Limits.OpsPerSecond,
			OpsBurst:         cfg.Limits.OpsBurst,
			CursorPerSecond:  cfg.Limits.CursorPerSecond,
		},
		InstanceID:     instanceID,
		MessageBytes:   int64(cfg.Limits.MaxMessageBytes),
		HelloTimeout:   cfg.WS.HelloTimeout,
		PongWait:       cfg.WS.PongWait,
		PingPeriod:     20 * time.Second,
		WriteWait:      10 * time.Second,
		SendQueue:      cfg.WS.SendQueue,
		SendQueueBytes: cfg.WS.SendQueueBytes,
		LeaveGrace:     cfg.WS.LeaveGrace,
		Logger:         logger.With("component", "ws"),
	})
	if err != nil {
		return 2, fmt.Errorf("init websocket: %w", err)
	}
	wsSrv.SetRoleResolver(func(ctx context.Context, code, key, rt string) (string, error) {
		return roleResolver{store: st, signer: signer}.Resolve(ctx, code, key, rt)
	})
	wsSrv.SetMetrics(reg)

	srvAPI, err := api.New(api.Deps{
		Config:    cfg,
		Logger:    logger.With("component", "http"),
		Metrics:   reg,
		Hub:       h,
		Broker:    br,
		Rooms:     svc,
		Signer:    signer,
		WebWS:     wsSrv,
		WebUI:     webui.Handler(webui.Options{DevHint: "cd web && npm ci && npm run build"}),
		Version:   version,
		BuildTime: buildTime,
		// Profiling is opt-in via DEBUG_PPROF=true so a production image never
		// exposes goroutine stacks.
		EnablePprof: os.Getenv("DEBUG_PPROF") == "true",
	})
	if err != nil {
		return 2, fmt.Errorf("init http api: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           srvAPI.Handler(),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		MaxHeaderBytes:    cfg.HTTP.MaxHeaderBytes,
		// No ReadTimeout/WriteTimeout: /ws connections are long-lived and a global
		// write deadline would kill them. Per-connection deadlines live in
		// internal/ws; ReadHeaderTimeout covers Slowloris.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	ln, err := net.Listen("tcp", cfg.HTTP.Addr)
	if err != nil {
		// Port conflicts are the single most common first-run failure, and the raw
		// "bind: address already in use" gives no hint about what to do. Spell out
		// the remedy, because the operator is usually double-clicking a binary.
		if isAddrInUse(err) {
			return 2, fmt.Errorf(
				"cannot listen on %s: the port is already in use by another program\n"+
					"  choose a free port:   PORT=8090 %s\n"+
					"  or find the owner:    (Linux) ss -ltnp | grep %s\n"+
					"                        (Windows) netstat -ano | findstr %s",
				cfg.HTTP.Addr, binaryName(), portOf(cfg.HTTP.Addr), portOf(cfg.HTTP.Addr))
		}
		return 2, fmt.Errorf("listen on %s: %w", cfg.HTTP.Addr, err)
	}
	logger.Info("listening", "addr", ln.Addr().String(), "startup_ms", time.Since(start).Milliseconds())

	// Print the two values that decide whether browsers can connect. A mismatch
	// between the configured public origin and the address people actually
	// visit is the most common deployment failure, and its only symptom is a
	// WebSocket 403 that looks like "the whiteboard is read-only".
	if cfg.PublicBaseURL != "" {
		logger.Info("public origin", "url", cfg.PublicBaseURL)
	}
	if len(cfg.WS.AllowedOrigins) > 0 {
		logger.Info("websocket origin allowlist", "origins", strings.Join(cfg.WS.AllowedOrigins, ","))
	}

	// Rate sampling for /ops: one goroutine, one atomic pair per second.
	sampleCtx, stopSampling := context.WithCancel(context.Background())
	defer stopSampling()
	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-sampleCtx.Done():
				return
			case now := <-t.C:
				reg.SampleRates(now)
			}
		}
	}()

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	// --- Running ----------------------------------------------------------

	var runErr error
	select {
	case err := <-serveErr:
		if err != nil {
			runErr = fmt.Errorf("http serve: %w", err)
		}
	case <-sigCtx.Done():
		if signalName == "" {
			signalName = "unknown"
		}
		logger.Info("shutdown signal received", "signal", signalName)
	}

	// --- Shutdown ---------------------------------------------------------

	// Detach from sigCtx so a second Ctrl+C cannot cancel the shutdown itself.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	shutdownStart := time.Now()

	// 1. Stop accepting; drain in-flight REST handlers.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful http shutdown failed, forcing close", "err", err)
		if cerr := srv.Close(); cerr != nil {
			logger.Error("forced http close failed", "err", cerr)
		}
	}
	if err := srvAPI.Shutdown(shutdownCtx); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		logger.Warn("http layer shutdown hook failed", "err", err)
	}

	// 2. Disconnect clients with 1001 and let each room write its final snapshot.
	// The hub's Close is called by the deferred closer below, but KillAll runs
	// here first so the drain has the full shutdown budget.
	stopSampling()
	stopReaper()
	samplerWG.Wait()
	h.KillAll(shutdownCtx)

	logger.Info("shutdown complete",
		"shutdown_ms", time.Since(shutdownStart).Milliseconds(),
		"uptime_s", int64(time.Since(start).Seconds()),
	)
	if runErr != nil {
		return 1, runErr
	}
	return 0, nil
}

// roomSource adapts the store to hub.RoomSource.
type roomSource struct{ store room.Store }

// LoadOrCreateRoom implements hub.RoomSource. It never creates: rooms are created
// by the REST API, so an unknown code is a 404 rather than an empty room.
func (s roomSource) LoadOrCreateRoom(ctx context.Context, code string) (*room.Record, error) {
	return s.store.LoadRoom(ctx, code)
}

// brokerAdapter adapts broker.Broker to room.Broadcaster. In memory mode the
// adapter is a no-op, because the broker's own in-process pub/sub is only used by
// tests: a single instance has no peers.
type brokerAdapter struct {
	br         broker.Broker
	memoryMode bool
}

// PublishOp implements room.Broadcaster.
func (a brokerAdapter) PublishOp(ctx context.Context, code string, seq int64, frame []byte) error {
	if a.memoryMode || a.br == nil {
		return nil
	}
	return a.br.Publish(ctx, code, seq, frame)
}

// PublishOpBatch implements room.Broadcaster with one broker round trip per batch.
func (a brokerAdapter) PublishOpBatch(ctx context.Context, code string, seqs []int64, frames [][]byte) error {
	if a.memoryMode || a.br == nil {
		return nil
	}
	out := make([]broker.Outbound, len(seqs))
	for i := range seqs {
		out[i] = broker.Outbound{Seq: seqs[i], Frame: frames[i]}
	}
	return a.br.PublishBatch(ctx, code, out)
}

// roleResolver resolves a WebSocket handshake's credentials to an authoritative
// role.
type roleResolver struct {
	store  room.Store
	signer *auth.TokenSigner
}

// Resolve implements ws.RoleResolver.
//
// Since the permission model, a room key no longer grants edit: it is an
// invitation credential and the entrant starts as a viewer. Only the readonly
// token still changes anything here, because an invalid one must be refused at
// the handshake rather than silently downgraded.
//
// The room then decides the effective role (`owner` for the first entrant or the
// current owner) inside its own goroutine, where ownership state lives.
func (r roleResolver) Resolve(ctx context.Context, code, key, rt string) (string, error) {
	editorHash, viewerHash, revokedAt, err := r.store.VerifyKeyHash(ctx, code)
	if err != nil {
		return "", err
	}
	if rt != "" {
		if _, err := r.signer.Verify(rt, code, revokedAt, time.Now()); err != nil {
			return "", ws.ErrForbidden
		}
		return protocol.RoleViewer, nil
	}
	// Validate the key so a typo'd link is still just a viewer, and keep the
	// viewer key working as before.
	_, _ = auth.RoleForKey(key, editorHash, viewerHash)
	return protocol.RoleViewer, nil
}

// warnIfIncoherent logs the deployment combinations that silently degrade
// correctness. It is a warning, not a startup failure: BROKER=redis with
// STORE=memory is the right setup for a throwaway load test, and refusing to boot
// would take that tool away.
func warnIfIncoherent(logger *slog.Logger, cfg *config.Config) {
	if cfg.Broker.Backend == config.BackendRedis && cfg.Store.Backend == config.BackendMemory {
		logger.Warn("BROKER=redis with STORE=memory: cross-instance seq gaps cannot self-heal " +
			"(no durable log to replay); use STORE=postgres for any multi-instance deployment. " +
			"This combination is intended for load testing only.")
	}
}

// openStore builds the configured store.
//
// The switch is exhaustive over the config constants, so an unknown value fails
// startup instead of silently falling back to memory: a production deployment that
// lost STORE=postgres must not come up as a volatile demo.
func openStore(ctx context.Context, cfg *config.Config, logger *slog.Logger) (room.Store, error) {
	switch cfg.Store.Backend {
	case config.BackendMemory:
		logger.Warn("using in-memory store: rooms are lost on restart")
		return memory.New(memory.Options{}), nil
	case config.BackendPostgres:
		st, err := postgres.New(ctx, postgres.Options{
			DatabaseURL:    cfg.Store.DatabaseURL,
			MaxConns:       cfg.Store.MaxConns,
			MinConns:       cfg.Store.MinConns,
			ConnectTimeout: cfg.Store.ConnectTimeout,
		})
		if err != nil {
			return nil, err
		}
		// The TTL is attached to the context used for creation so the store does
		// not need the whole config.
		logger.Info("postgres store connected", "max_conns", cfg.Store.MaxConns)
		return &ttlStore{Store: st, ttl: cfg.Rooms.TTL}, nil
	default:
		return nil, fmt.Errorf("unknown store backend %q", cfg.Store.Backend)
	}
}

// ttlStore attaches the room TTL to every CreateRoom context, keeping the store
// port free of configuration concerns.
type ttlStore struct {
	*postgres.Store
	ttl time.Duration
}

// CreateRoom implements room.Store with the configured TTL.
func (s *ttlStore) CreateRoom(ctx context.Context, rec *room.Record) error {
	return s.Store.CreateRoom(postgres.WithRoomTTL(ctx, s.ttl), rec)
}

// openBroker builds the configured broker.
func openBroker(ctx context.Context, cfg *config.Config, instanceID string, logger *slog.Logger) (broker.Broker, error) {
	switch cfg.Broker.Backend {
	case config.BackendMemory:
		logger.Warn("using in-process broker: multi-instance fan-out is disabled")
		return brokermemory.New(), nil
	case config.BackendRedis:
		br, err := redis.New(ctx, redis.Options{
			URL:         cfg.Broker.RedisURL,
			InstanceID:  instanceID,
			Logger:      logger.With("component", "broker"),
			DialTimeout: cfg.Broker.ConnectTimeout,
		})
		if err != nil {
			return nil, err
		}
		logger.Info("redis broker connected", "backend", br.Backend())
		return br, nil
	default:
		return nil, fmt.Errorf("unknown broker backend %q", cfg.Broker.Backend)
	}
}

// newInstanceID returns a short random ID used in joined and for broker
// self-echo filtering. It is not a secret.
func newInstanceID() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	var b [8]byte
	if _, err := cryptoRandRead(b[:]); err != nil {
		return "i_00000000"
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "i_" + string(b[:])
}
