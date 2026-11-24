// Package hub owns every live room on this instance and the instance-wide
// connection budget.
//
// # Ownership
//
//	Hub.mu guards rooms, conns and closed. It is a short-lived lock: every
//	critical section is a map lookup or a counter update, never I/O.
//
//	A *room.Handle is safe for concurrent use; the hub keeps the map and hands
//	handles out. Room goroutines never call back into the hub, so there is no
//	lock-order cycle.
//
// # Admission
//
// Admission happens in two steps, deliberately:
//
//	Admit()     reserves a slot under the lock and returns the room handle.
//	Release()   returns the slot; must be called exactly once per Admit.
//
// Splitting them means the WebSocket upgrade (slow, involves a syscall) does not
// hold the hub lock while it runs.
package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/codraw/codraw/backend/internal/metrics"
	"github.com/codraw/codraw/backend/internal/protocol"
	"github.com/codraw/codraw/backend/internal/room"
)

// Errors returned by the hub.
var (
	// ErrRoomNotFound means no such room exists (or it expired).
	ErrRoomNotFound = errors.New("hub: room not found")
	// ErrRoomFull means the room's connection budget is exhausted.
	ErrRoomFull = errors.New("hub: room full")
	// ErrServerBusy means the instance-wide connection budget is exhausted.
	ErrServerBusy = errors.New("hub: server busy")
	// ErrClosed means the hub is shutting down.
	ErrClosed = errors.New("hub: closed")
)

// RoomSource loads or creates a room's persisted state. It is implemented by the
// store layer; the hub only needs this one operation.
//
// The hub calls it outside its own lock because it may hit Postgres.
type RoomSource interface {
	// LoadOrCreateRoom returns the persisted room, creating it if it does not
	// exist, and the ops needed to rebuild its state.
	LoadOrCreateRoom(ctx context.Context, code string) (*room.Record, error)
}

// RoomFactory builds a live room handle from persisted state.
type RoomFactory func(ctx context.Context, rec *room.Record) (*room.Handle, error)

// Options are the hub's dependencies.
type Options struct {
	Metrics *metrics.Registry
	Logger  *slog.Logger
	// Limits are the protocol limits, advertised in `joined`.
	Limits room.Limits
	// InstanceID appears in joined and in broker self-echo filtering.
	InstanceID string
	// Source loads persisted rooms. Nil means rooms exist only in memory and are
	// created on first join (STORE=memory without pre-creation).
	Source RoomSource
	// Factory builds the live room. Required.
	Factory RoomFactory
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Hub is the process-wide room registry.
type Hub struct {
	opts Options
	log  *slog.Logger

	mu     sync.RWMutex
	rooms  map[string]*room.Handle
	conns  int
	closed bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a hub.
func New(opts Options) (*Hub, error) {
	if opts.Metrics == nil || opts.Factory == nil {
		return nil, errors.New("hub: Metrics and Factory are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		opts:   opts,
		log:    opts.Logger,
		rooms:  make(map[string]*room.Handle),
		ctx:    ctx,
		cancel: cancel,
	}
	h.syncGauges()
	return h, nil
}

// InstanceID returns the ID advertised to clients in `joined`.
func (h *Hub) InstanceID() string { return h.opts.InstanceID }

// RoomCount returns the number of live rooms.
func (h *Hub) RoomCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms)
}

// ConnectionCount returns the number of admitted WebSocket connections.
func (h *Hub) ConnectionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conns
}

// Get returns the live room handle for code, or ErrRoomNotFound.
func (h *Hub) Get(code string) (*room.Handle, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return nil, ErrClosed
	}
	r, ok := h.rooms[code]
	if !ok {
		return nil, ErrRoomNotFound
	}
	return r, nil
}

// Admit reserves a connection slot for code and returns the room handle,
// starting the room if this is the first client.
//
// The returned release function must be called exactly once, after the
// connection is torn down.
func (h *Hub) Admit(ctx context.Context, code string) (*room.Handle, int, func(), error) {
	// Fast path: the room already exists.
	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return nil, 0, nil, ErrClosed
	}
	rh, ok := h.rooms[code]
	total := h.conns
	h.mu.RUnlock()

	if ok {
		if rh.Online() >= h.opts.Limits.MaxConnPerRoom {
			return nil, 0, nil, ErrRoomFull
		}
		return h.reserve(rh, total)
	}

	// Slow path: materialise the room. Load outside the lock because it may hit
	// the database, then double-check under the write lock so two concurrent
	// first-joiners do not create two rooms.
	rh, err := h.openRoom(ctx, code)
	if err != nil {
		return nil, 0, nil, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, 0, nil, ErrClosed
	}
	if existing, ok := h.rooms[code]; ok {
		// Someone won the race; use theirs and let the discarded one exit.
		if existing != rh {
			_ = rh.Close(context.Background())
		}
		return h.reserveLocked(existing)
	}
	if rh.Online() >= h.opts.Limits.MaxConnPerRoom {
		return nil, 0, nil, ErrRoomFull
	}
	h.rooms[code] = rh
	return h.reserveLocked(rh)
}

// reserveLocked performs the budget checks and increments the counter. The
// caller must hold h.mu for writing.
func (h *Hub) reserveLocked(rh *room.Handle) (*room.Handle, int, func(), error) {
	if h.conns >= h.opts.Limits.MaxConnTotal {
		return nil, 0, nil, ErrServerBusy
	}
	h.conns++
	h.syncGauges()
	online := rh.Online()
	release := h.releaseFunc()
	return rh, online, release, nil
}

// reserve is reserveLocked for the fast path, which only had a read lock.
func (h *Hub) reserve(rh *room.Handle, _ int) (*room.Handle, int, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, 0, nil, ErrClosed
	}
	return h.reserveLocked(rh)
}

// releaseFunc returns an idempotent slot releaser.
func (h *Hub) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			if h.conns > 0 {
				h.conns--
			}
			h.syncGauges()
			h.mu.Unlock()
		})
	}
}

// openRoom loads persisted state (if a Source is configured) and starts the room.
func (h *Hub) openRoom(ctx context.Context, code string) (*room.Handle, error) {
	var rec *room.Record
	if h.opts.Source != nil {
		loaded, err := h.opts.Source.LoadOrCreateRoom(ctx, code)
		if err != nil {
			if errors.Is(err, room.ErrNotFound) {
				return nil, ErrRoomNotFound
			}
			return nil, fmt.Errorf("load room %s: %w", code, err)
		}
		rec = loaded
	} else {
		// No source: a room only exists once it was created through the REST API.
		// The hub cannot invent one, otherwise a typo'd code would silently create
		// an empty room instead of 404ing.
		return nil, ErrRoomNotFound
	}
	rh, err := h.opts.Factory(ctx, rec)
	if err != nil {
		return nil, err
	}
	h.log.Info("room opened", "room", code, "seq", rec.Seq, "shapes", rec.ShapeCount)
	return rh, nil
}

// RegisterRoom publishes a freshly created room handle without admitting a
// connection. Used by the REST create endpoint so the creator's WebSocket finds
// the room already there.
func (h *Hub) RegisterRoom(code string, rh *room.Handle) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrClosed
	}
	if _, ok := h.rooms[code]; ok {
		return fmt.Errorf("hub: room %s already registered", code)
	}
	h.rooms[code] = rh
	h.syncGauges()
	return nil
}

// RemoveRoom closes and forgets a room handle. Used by the idle reaper and by
// tests.
func (h *Hub) RemoveRoom(ctx context.Context, code string) error {
	h.mu.Lock()
	rh, ok := h.rooms[code]
	if ok {
		delete(h.rooms, code)
	}
	h.syncGauges()
	h.mu.Unlock()

	if !ok {
		return ErrRoomNotFound
	}
	return rh.Close(ctx)
}

// Rooms returns a snapshot of the live handles, for the reaper and /ops.
func (h *Hub) Rooms() []*room.Handle {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*room.Handle, 0, len(h.rooms))
	for _, rh := range h.rooms {
		out = append(out, rh)
	}
	return out
}

// Stats returns per-room stats for the REST API.
func (h *Hub) Stats(code string) (room.Stats, bool) {
	h.mu.RLock()
	rh, ok := h.rooms[code]
	h.mu.RUnlock()
	if !ok {
		return room.Stats{}, false
	}
	return rh.Stats(), true
}

// KillAll disconnects every member of every room with 1001 and waits for the
// rooms to exit. It is the first step of graceful shutdown, so clients learn to
// reconnect rather than attributing the outage to the network.
func (h *Hub) KillAll(ctx context.Context) {
	rooms := h.Rooms()
	var wg sync.WaitGroup
	for _, rh := range rooms {
		wg.Add(1)
		go func(rh *room.Handle) {
			defer wg.Done()
			if err := rh.Close(ctx); err != nil {
				h.log.Warn("room close timed out", "room", rh.Code(), "err", err)
			}
		}(rh)
	}
	wg.Wait()
}

// Close stops every room and releases resources. It is idempotent.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	rooms := make([]*room.Handle, 0, len(h.rooms))
	for code, rh := range h.rooms {
		rooms = append(rooms, rh)
		delete(h.rooms, code)
	}
	h.conns = 0
	h.syncGauges()
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, rh := range rooms {
		wg.Add(1)
		go func(rh *room.Handle) {
			defer wg.Done()
			_ = rh.Close(ctx)
		}(rh)
	}
	wg.Wait()
	h.cancel()
	return nil
}

// syncGauges publishes the two gauge values the hub owns. Gauge updates are cheap
// (atomic stores), so they happen on every mutation rather than from a poller:
// a poller would make /metrics lag the truth for up to one interval, which is
// exactly the window an operator stares at during an incident.
//
// The caller must hold h.mu.
func (h *Hub) syncGauges() {
	h.opts.Metrics.SetRooms(len(h.rooms))
	h.opts.Metrics.SetWSConnections(h.conns)
}

// ErrorForCode maps a hub admission error onto the protocol close/HTTP code and
// error code. Keeping the mapping here means the WS handshake and the REST layer
// cannot disagree about what "full" means.
func ErrorForCode(err error) (httpStatus int, code protocol.ErrCode, msg string) {
	switch {
	case errors.Is(err, ErrRoomNotFound):
		return 404, protocol.CodeRoomNotFound, "room not found"
	case errors.Is(err, ErrRoomFull):
		return 429, protocol.CodeRoomFull, "room is full"
	case errors.Is(err, ErrServerBusy):
		return 503, protocol.CodeServerBusy, "server busy"
	case errors.Is(err, ErrClosed):
		return 503, protocol.CodeServerBusy, "server shutting down"
	default:
		return 500, protocol.CodeInternal, "internal error"
	}
}
