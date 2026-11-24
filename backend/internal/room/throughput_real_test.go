package room_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/codraw/codraw/backend/internal/broker"
	"github.com/codraw/codraw/backend/internal/broker/redis"
	"github.com/codraw/codraw/backend/internal/protocol"
	"github.com/codraw/codraw/backend/internal/room"
	"github.com/codraw/codraw/backend/internal/store/postgres"
)

// countingMember accepts every frame and never blocks, so the measurement includes
// only the room, the store and the broker — not socket I/O.
type countingMember struct{ user protocol.User }

func (m *countingMember) User() protocol.User                         { return m.user }
func (m *countingMember) SetRole(role string)                         { m.user.Role = role }
func (m *countingMember) Send([]byte) bool                            { return true }
func (m *countingMember) Kick(int, string)                            {}
func (m *countingMember) KickWithError(int, protocol.ErrCode, string) {}

// realBroadcaster adapts broker.Broker to room.Broadcaster.
type realBroadcaster struct{ br broker.Broker }

func (b realBroadcaster) PublishOp(ctx context.Context, code string, seq int64, frame []byte) error {
	return b.br.Publish(ctx, code, seq, frame)
}

func (b realBroadcaster) PublishOpBatch(ctx context.Context, code string, seqs []int64, frames [][]byte) error {
	out := make([]broker.Outbound, len(seqs))
	for i := range seqs {
		out[i] = broker.Outbound{Seq: seqs[i], Frame: frames[i]}
	}
	return b.br.PublishBatch(ctx, code, out)
}

// TestRoomThroughputReal measures end-to-end room throughput with the real Postgres
// and Redis, including every round trip the production path pays. It answers the
// question "is the store the bottleneck or the room" with numbers.
//
// Run inside the compose network:
//
//	ROOMBENCH=1 PGURL=... RURL=... go test -run TestRoomThroughputReal -v ./internal/room/
func TestRoomThroughputReal(t *testing.T) {
	if os.Getenv("ROOMBENCH") != "1" {
		t.Skip("set ROOMBENCH=1 and run inside the compose network")
	}
	pgURL, redisURL := os.Getenv("PGURL"), os.Getenv("RURL")
	if pgURL == "" || redisURL == "" {
		t.Skip("PGURL and RURL are required")
	}
	ctx := context.Background()
	st, err := postgres.New(ctx, postgres.Options{DatabaseURL: pgURL, ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer func() { _ = st.Close() }()
	br, err := redis.New(ctx, redis.Options{URL: redisURL, InstanceID: "i_bench", DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	defer func() { _ = br.Close() }()

	code := fmt.Sprintf("B%05d", time.Now().UnixNano()%100000)
	if err := st.CreateRoom(ctx, &room.Record{Code: code, Title: "bench"}); err != nil {
		t.Fatalf("create room: %v", err)
	}

	const members = 100
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
	h := room.New(room.Options{
		Code: code, Title: "bench", Epoch: 1,
		Limits: room.Limits{
			MaxConnPerRoom: 200, MaxConnTotal: 5000,
			MaxMessageBytes: 65536, MaxShapeBytes: 32768, MaxShapesPerRoom: 100000,
			OpsPerSecond: 1e9, OpsBurst: 1e9, CursorPerSecond: 20,
			ResumeMaxOps: 100, ResumeMaxBytes: 1 << 20, MaxNameRunes: 24,
		},
		Retention: 5000, LeaveGrace: time.Second,
		SnapshotEvery: 1e9, SnapshotChunkShape: 500, SnapshotChunkBytes: 48 << 10,
		Persist:   st,
		SeqSource: br,
		Broadcast: realBroadcaster{br: br},
		InboxSize: 65536,
		Logger:    logger,
	})
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = h.Close(c)
	})

	subs := make([]*countingMember, members)
	for i := range subs {
		subs[i] = &countingMember{user: protocol.User{
			ClientID: fmt.Sprintf("c_br%04d", i), UserID: "u", Name: "n",
			Color: "#16181d", Role: protocol.RoleEditor,
		}}
		if err := h.Join(ctx, subs[i]); err != nil {
			t.Fatalf("join: %v", err)
		}
	}

	const totalOps = 5000
	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(members)
	for i := range subs {
		go func(i int) {
			defer wg.Done()
			for n := 0; n < totalOps/members; n++ {
				shape, _ := json.Marshal(map[string]any{
					"id": fmt.Sprintf("sh_br%04d_%04d", i, n), "kind": "rect",
					"geom":  map[string]any{"x": 0, "y": 0, "w": 10, "h": 10},
					"style": map[string]any{"stroke": "#16181d", "fill": "none", "strokeWidth": 2, "opacity": 1, "dash": "solid"},
					"data":  map[string]any{},
				})
				h.Op(subs[i], &protocol.Envelope{}, &protocol.OpAddPayload{
					OpID: fmt.Sprintf("op_br%04d_%04d", i, n), Action: protocol.ActionCreate, Shape: shape,
				}, nil)
			}
		}(i)
	}
	wg.Wait()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && h.Stats().Seq < totalOps {
		time.Sleep(10 * time.Millisecond)
	}
	elapsed := time.Since(start)
	seq := h.Stats().Seq
	t.Logf("REAL room applied %d ops in %s = %.0f ops/s", seq, elapsed.Round(time.Millisecond), float64(seq)/elapsed.Seconds())
	if seq < totalOps {
		t.Fatalf("only %d of %d ops applied", seq, totalOps)
	}
}
