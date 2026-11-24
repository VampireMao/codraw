package hub

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codraw/codraw/backend/internal/metrics"
	"github.com/codraw/codraw/backend/internal/protocol"
	"github.com/codraw/codraw/backend/internal/room"
)

func testLimits() room.Limits {
	return room.Limits{
		MaxConnPerRoom: 2, MaxConnTotal: 4,
		MaxMessageBytes: 65536, MaxShapeBytes: 32768, MaxShapesPerRoom: 100,
		OpsPerSecond: 60, OpsBurst: 120, CursorPerSecond: 20,
		ResumeMaxOps: 100, ResumeMaxBytes: 1 << 20, MaxNameRunes: 24,
	}
}

// fakeSource returns a fixed room record.
type fakeSource struct {
	rec *room.Record
	err error
}

func (s fakeSource) LoadOrCreateRoom(context.Context, string) (*room.Record, error) {
	return s.rec, s.err
}

// newHub builds a hub with a factory that returns a real room over memory state.
func newHub(t *testing.T, opts ...func(*Options)) (*Hub, *metrics.Registry) {
	t.Helper()
	reg := metrics.New("test")
	limits := testLimits()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// now is shared by the hub and every room it builds, so the reaper and the room's
	// LastActiveAt agree on what "idle" means. It is read from the room goroutine, so
	// it lives behind an atomic pointer to keep the race detector quiet.
	var nowVal atomic.Pointer[time.Time]
	base := time.Now()
	nowVal.Store(&base)
	now := func() time.Time { return *nowVal.Load() }
	o := Options{
		Metrics:    reg,
		Logger:     logger,
		Limits:     limits,
		InstanceID: "i_test",
		Source:     fakeSource{rec: &room.Record{Code: "K7M2QX", Title: "t", Epoch: 1}},
		Factory: func(ctx context.Context, rec *room.Record) (*room.Handle, error) {
			return room.NewWithRecord(room.Options{
				Code: rec.Code, Title: rec.Title, Epoch: rec.Epoch, Seq: rec.Seq,
				Limits: limits, Retention: 100, Logger: logger, Now: func() time.Time { return now() },
			}, rec)
		},
	}
	for _, fn := range opts {
		fn(&o)
	}
	if o.Now != nil {
		fixed := o.Now()
		nowVal.Store(&fixed)
		// o.Now is a moving clock in the reaper tests; wrap it so both the hub and the
		// rooms observe the same value.
		now = o.Now
	}
	h, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, reg
}

func TestNewRequiresDeps(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New without deps should fail")
	}
}

func TestAdmitReservesAndReleases(t *testing.T) {
	h, reg := newHub(t)

	rh, _, release, err := h.Admit(context.Background(), "K7M2QX")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if rh == nil {
		t.Fatal("nil room handle")
	}
	if got := h.ConnectionCount(); got != 1 {
		t.Fatalf("connections = %d, want 1", got)
	}
	if got := reg.Snapshot().WSConnections; got != 1 {
		t.Fatalf("gauge = %d, want 1", got)
	}
	release()
	release() // must be idempotent
	if got := h.ConnectionCount(); got != 0 {
		t.Fatalf("after release = %d, want 0", got)
	}
}

func TestAdmitIsIdempotentForSameRoom(t *testing.T) {
	h, _ := newHub(t)
	a, _, relA, err := h.Admit(context.Background(), "K7M2QX")
	if err != nil {
		t.Fatalf("Admit 1: %v", err)
	}
	defer relA()
	b, _, relB, err := h.Admit(context.Background(), "K7M2QX")
	if err != nil {
		t.Fatalf("Admit 2: %v", err)
	}
	defer relB()
	if a != b {
		t.Fatal("two admits for the same code produced different room handles")
	}
	if got := h.RoomCount(); got != 1 {
		t.Fatalf("rooms = %d, want 1", got)
	}
}

func TestAdmitUnknownRoomIs404(t *testing.T) {
	h, _ := newHub(t, func(o *Options) {
		o.Source = fakeSource{err: room.ErrNotFound}
	})
	_, _, _, err := h.Admit(context.Background(), "ZZZZZZ")
	if err != ErrRoomNotFound {
		t.Fatalf("err = %v, want ErrRoomNotFound", err)
	}
	status, code, _ := ErrorForCode(err)
	if status != 404 || code != "room_not_found" {
		t.Fatalf("mapping = %d/%s", status, code)
	}
}

func TestErrorForCodeMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{ErrRoomNotFound, 404, "room_not_found"},
		{ErrRoomFull, 429, "room_full"},
		{ErrServerBusy, 503, "server_busy"},
		{ErrClosed, 503, "server_busy"},
		{io.ErrUnexpectedEOF, 500, "internal"},
	}
	for _, c := range cases {
		status, code, _ := ErrorForCode(c.err)
		if status != c.status || string(code) != c.code {
			t.Errorf("ErrorForCode(%v) = %d/%s, want %d/%s", c.err, status, code, c.status, c.code)
		}
	}
}

func TestGetAndStats(t *testing.T) {
	h, _ := newHub(t)
	if _, err := h.Get("K7M2QX"); err != ErrRoomNotFound {
		t.Fatalf("Get before admit = %v", err)
	}
	if _, _, rel, err := h.Admit(context.Background(), "K7M2QX"); err == nil {
		defer rel()
	} else {
		t.Fatalf("Admit: %v", err)
	}
	if _, err := h.Get("K7M2QX"); err != nil {
		t.Fatalf("Get after admit = %v", err)
	}
	stats, ok := h.Stats("K7M2QX")
	if !ok || stats.Code != "K7M2QX" {
		t.Fatalf("Stats = (%+v, %v)", stats, ok)
	}
}

func TestRegisterAndRemoveRoom(t *testing.T) {
	h, _ := newHub(t)
	rh, err := room.NewWithRecord(room.Options{
		Code: "ABCDEF", Limits: testLimits(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, &room.Record{Code: "ABCDEF"})
	if err != nil {
		t.Fatalf("NewWithRecord: %v", err)
	}
	if err := h.RegisterRoom("ABCDEF", rh); err != nil {
		t.Fatalf("RegisterRoom: %v", err)
	}
	if err := h.RegisterRoom("ABCDEF", rh); err == nil {
		t.Fatal("double register should fail")
	}
	if got := h.RoomCount(); got != 1 {
		t.Fatalf("rooms = %d", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.RemoveRoom(ctx, "ABCDEF"); err != nil {
		t.Fatalf("RemoveRoom: %v", err)
	}
	if err := h.RemoveRoom(ctx, "ABCDEF"); err != ErrRoomNotFound {
		t.Fatalf("second RemoveRoom = %v", err)
	}
}

func TestCloseIsIdempotentAndRejectsWork(t *testing.T) {
	h, _ := newHub(t)
	if _, _, rel, err := h.Admit(context.Background(), "K7M2QX"); err == nil {
		rel()
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, _, err := h.Admit(context.Background(), "K7M2QX"); err != ErrClosed {
		t.Fatalf("Admit after Close = %v", err)
	}
	if _, err := h.Get("K7M2QX"); err != ErrClosed {
		t.Fatalf("Get after Close = %v", err)
	}
}

func TestKillAllClosesRooms(t *testing.T) {
	h, _ := newHub(t)
	_, _, rel, err := h.Admit(context.Background(), "K7M2QX")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	defer rel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.KillAll(ctx)
	// The handle stays in the map (KillAll only closes clients); the room goroutine
	// must have exited.
	rh, err := h.Get("K7M2QX")
	if err != nil {
		t.Fatalf("Get after KillAll = %v", err)
	}
	select {
	case <-rh.Done():
	case <-time.After(time.Second):
		t.Fatal("room goroutine did not exit after KillAll")
	}
}

func TestReaperEvictsIdleRoom(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	h, _ := newHub(t, func(o *Options) {
		o.Now = clock.Now
	})
	_, _, rel, err := h.Admit(context.Background(), "K7M2QX")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	rel()

	r := NewReaper(h, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), clock.Now)
	if r == nil {
		t.Fatal("NewReaper returned nil for a positive ttl")
	}
	// Not idle yet.
	r.sweep(context.Background())
	if got := h.RoomCount(); got != 1 {
		t.Fatalf("room evicted before its TTL: %d rooms", got)
	}
	// Move the clock past the TTL.
	clock.Advance(2 * time.Hour)
	r.sweep(context.Background())
	if got := h.RoomCount(); got != 0 {
		t.Fatalf("room not evicted after TTL: %d rooms", got)
	}
}

func TestReaperKeepsRoomWithMembers(t *testing.T) {
	// A room with a live socket must never be reaped, no matter how long it has been
	// idle in terms of drawing.
	clock := &testClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	h, _ := newHub(t, func(o *Options) {
		o.Now = clock.Now
	})
	_, _, rel, err := h.Admit(context.Background(), "K7M2QX")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	defer rel()
	rh, _ := h.Get("K7M2QX")
	// Join a fake member so Online > 0.
	if err := rh.Join(context.Background(), fakeMember{}); err != nil {
		t.Fatalf("Join: %v", err)
	}

	// Join is an asynchronous command: it is queued to the room goroutine and
	// returns immediately, while Stats() reads an atomic snapshot published by
	// that goroutine. Without waiting here the reaper can observe Online == 0
	// and reap a room that does in fact have a member — which made this test
	// fail intermittently (it passed under -race only because the race detector
	// slows everything down).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && rh.Stats().Online == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := rh.Stats().Online; got != 1 {
		t.Fatalf("member did not register, Online = %d", got)
	}

	clock.Advance(48 * time.Hour)
	r := NewReaper(h, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), clock.Now)
	r.sweep(context.Background())
	if got := h.RoomCount(); got != 1 {
		t.Fatalf("room with a live member was reaped")
	}
}

// testClock is a movable clock shared by the hub, its rooms and the reaper.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeMember is a minimal room.Member.
type fakeMember struct{}

func (fakeMember) User() protocol.User {
	return protocol.User{ClientID: "c_fake01", Role: protocol.RoleViewer}
}
func (fakeMember) SetRole(string)                              {}
func (fakeMember) Send([]byte) bool                            { return true }
func (fakeMember) Kick(int, string)                            {}
func (fakeMember) KickWithError(int, protocol.ErrCode, string) {}

func TestNewReaperDisabledForZeroTTL(t *testing.T) {
	h, _ := newHub(t)
	if r := NewReaper(h, 0, slog.New(slog.NewTextHandler(io.Discard, nil)), nil); r != nil {
		t.Fatal("NewReaper(ttl=0) should be nil")
	}
	if r := NewReaper(nil, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), nil); r != nil {
		t.Fatal("NewReaper(nil hub) should be nil")
	}
}
