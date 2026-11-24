package room

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/codraw/codraw/backend/internal/protocol"
)

// countingMember accepts every frame and records acks, so the test measures the
// room's own throughput with no socket involved.
type countingMember struct {
	user   protocol.User
	mu     sync.Mutex
	acks   int
	frames int
}

func (m *countingMember) User() protocol.User { return m.user }
func (m *countingMember) SetRole(role string) {
	m.mu.Lock()
	m.user.Role = role
	m.mu.Unlock()
}
func (m *countingMember) Send([]byte) bool {
	m.mu.Lock()
	m.frames++
	m.mu.Unlock()
	return true
}
func (m *countingMember) Kick(int, string)                            {}
func (m *countingMember) KickWithError(int, protocol.ErrCode, string) {}

// nopPersister satisfies Persister with no I/O, isolating room CPU from the store.
type nopPersister struct{}

func (nopPersister) AppendOp(context.Context, OpRecord) (int64, bool, error) { return 0, false, nil }
func (nopPersister) AppendBatch(_ context.Context, recs []OpRecord) ([]AppendResult, error) {
	out := make([]AppendResult, len(recs))
	for i, r := range recs {
		out[i] = AppendResult{Seq: r.Seq}
	}
	return out, nil
}
func (nopPersister) LoadOps(context.Context, string, int64, int, int64) ([]OpRecord, bool, error) {
	return nil, false, nil
}
func (nopPersister) SaveSnapshot(context.Context, string, int64, int64, int, []byte) error {
	return nil
}
func (nopPersister) LoadSnapshot(context.Context, string) (SnapshotRecord, error) {
	return SnapshotRecord{}, ErrNoSnapshot
}
func (nopPersister) SaveOwner(context.Context, string, string) error { return nil }
func (nopPersister) SaveGrant(context.Context, string, string, string, string, time.Time) error {
	return nil
}
func (nopPersister) DeleteGrant(context.Context, string, string) error { return nil }

// TestRoomThroughput measures ops/second the room loop can sustain with 100 members
// and no store cost. If this is far below 500/s the bottleneck is the room, not the
// database, and batching the store cannot help.
//
// The race detector slows every atomic and map access by roughly an order of
// magnitude, so the throughput assertion is meaningless under -race; the test still
// runs (and must not deadlock) but skips the numeric bound.
func TestRoomThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput measurement")
	}
	const (
		members = 100
		opsPer  = 2000
	)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	// Every member must be allowed to write. Under the permission model only the
	// owner writes by default, so the grants table pre-authorises all of them
	// (which is also how a restored room looks after a restart).
	grants := make(map[string]string, members)
	for i := 0; i < members; i++ {
		grants[fmt.Sprintf("c_perf%04d", i)] = "editor"
	}
	h := New(Options{
		Code: "K7M2QX", Title: "t", Epoch: 1,
		Limits: Limits{
			MaxConnPerRoom: 200, MaxConnTotal: 5000,
			MaxMessageBytes: 65536, MaxShapeBytes: 32768, MaxShapesPerRoom: 100000,
			OpsPerSecond: 1e9, OpsBurst: 1e9, CursorPerSecond: 20,
			ResumeMaxOps: 100, ResumeMaxBytes: 1 << 20, MaxNameRunes: 24,
		},
		Grants:        grants,
		OwnerClientID: "c_perf0000",
		Retention:     5000,
		LeaveGrace:    time.Second,
		SnapshotEvery: 1e9, SnapshotChunkShape: 500, SnapshotChunkBytes: 48 << 10,
		Persist:   nopPersister{},
		InboxSize: 65536,
		Logger:    logger,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Close(ctx)
	})

	subs := make([]*countingMember, members)
	for i := range subs {
		subs[i] = &countingMember{user: protocol.User{
			ClientID: fmt.Sprintf("c_perf%04d", i), UserID: "u", Name: "n",
			Color: "#16181d", Role: protocol.RoleEditor,
		}}
		if err := h.Join(context.Background(), subs[i]); err != nil {
			t.Fatalf("join: %v", err)
		}
	}

	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(members)
	for i := range subs {
		go func(i int) {
			defer wg.Done()
			for n := 0; n < opsPer/members; n++ {
				shape, _ := json.Marshal(map[string]any{
					"id": fmt.Sprintf("sh_perf%04d_%04d", i, n), "kind": "rect",
					"geom":  map[string]any{"x": 0, "y": 0, "w": 10, "h": 10},
					"style": map[string]any{"stroke": "#16181d", "fill": "none", "strokeWidth": 2, "opacity": 1, "dash": "solid"},
					"data":  map[string]any{},
				})
				p := &protocol.OpAddPayload{
					OpID:   fmt.Sprintf("op_perf%04d_%04d", i, n),
					Action: protocol.ActionCreate, Shape: shape,
				}
				h.Op(subs[i], &protocol.Envelope{}, p, nil)
			}
		}(i)
	}
	wg.Wait()

	// Wait for the room to drain its inbox.
	deadline := time.Now().Add(30 * time.Second)
	want := int64(opsPer)
	for time.Now().Before(deadline) {
		if h.Stats().Seq >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	elapsed := time.Since(start)
	seq := h.Stats().Seq
	rate := float64(seq) / elapsed.Seconds()
	t.Logf("room applied %d ops in %s = %.0f ops/s (target 500/s)", seq, elapsed.Round(time.Millisecond), rate)
	if seq < want {
		t.Fatalf("room only applied %d of %d ops", seq, want)
	}
	// The numeric bound is only meaningful without the race detector's overhead.
	if !raceBuild && rate < 5000 {
		t.Fatalf("room throughput %.0f ops/s is too low for a 500 op/s workload; the room is the bottleneck", rate)
	}
}
