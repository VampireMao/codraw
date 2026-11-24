package hub

import (
	"context"
	"time"
)

// Reaper closes and releases rooms that have been idle longer than ttl.
//
// Why this exists: a room handle keeps its shapes in memory, and with
// STORE=memory nothing else would ever free them. Without a reaper a long-lived
// demo instance leaks one map entry per room ever visited.
//
// The room goroutine writes its final snapshot on Close, so evicting a room is
// lossless with STORE=postgres and is the documented loss point with
// STORE=memory.
type Reaper struct {
	hub *Hub
	ttl time.Duration
	log logger
	now func() time.Time
	// every is the sweep period; derived from ttl so a short TTL in tests still
	// gets swept promptly.
	every time.Duration
}

// logger is the tiny slice of slog the reaper needs.
type logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// NewReaper builds a reaper. ttl <= 0 disables it (returns nil).
func NewReaper(h *Hub, ttl time.Duration, log logger, now func() time.Time) *Reaper {
	if ttl <= 0 || h == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	every := ttl / 10
	if every < time.Second {
		every = time.Second
	}
	if every > time.Minute {
		every = time.Minute
	}
	return &Reaper{hub: h, ttl: ttl, log: log, now: now, every: every}
}

// Run sweeps until ctx is cancelled. The caller should run it in its own
// goroutine.
func (r *Reaper) Run(ctx context.Context) {
	if r == nil {
		return
	}
	t := time.NewTicker(r.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweep(ctx)
		}
	}
}

// sweep closes every room whose last activity is older than ttl and that has no
// live members.
//
// A room with members is never evicted even if it looks idle: the presence list
// is the authority on "someone is here", and a room with an open socket but no
// drawing for an hour must not vanish under the user.
func (r *Reaper) sweep(ctx context.Context) {
	cutoff := r.now().Add(-r.ttl)
	for _, rh := range r.hub.Rooms() {
		stats := rh.Stats()
		if stats.Online > 0 || stats.PendingLeaves > 0 {
			continue
		}
		if stats.LastActiveAt.After(cutoff) {
			continue
		}
		code := rh.Code()
		if err := r.hub.RemoveRoom(ctx, code); err != nil {
			r.log.Warn("reaper: room close failed", "room", code, "err", err)
			continue
		}
		r.log.Info("reaper: released idle room", "room", code,
			"idle_s", int64(r.now().Sub(stats.LastActiveAt).Seconds()))
	}
}
