package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/codraw/codraw/backend/internal/room"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s := New(Options{})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func createRoom(t *testing.T, s *Store, code string) {
	t.Helper()
	if err := s.CreateRoom(context.Background(), &room.Record{Code: code, Title: "t"}); err != nil {
		t.Fatalf("CreateRoom(%q): %v", code, err)
	}
}

func TestCreateAndLoadRoom(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := New(Options{Now: func() time.Time { return now }})
	defer func() { _ = s.Close() }()

	if err := s.CreateRoom(ctx, &room.Record{Code: "K7M2QX", Title: "白板"}); err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if err := s.CreateRoom(ctx, &room.Record{Code: "K7M2QX"}); !errors.Is(err, room.ErrDuplicateRoom) {
		t.Fatalf("second create = %v, want ErrDuplicateRoom", err)
	}

	rec, err := s.LoadRoom(ctx, "K7M2QX")
	if err != nil {
		t.Fatalf("LoadRoom: %v", err)
	}
	if rec.Title != "白板" || rec.Epoch != 1 {
		t.Fatalf("record = %+v", rec)
	}
	if !rec.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %s", rec.CreatedAt)
	}

	if _, err := s.LoadRoom(ctx, "ZZZZZZ"); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("missing room = %v", err)
	}
}

func TestKeysAndRevocation(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	createRoom(t, s, "K7M2QX")

	var editor, viewer [32]byte
	editor[0], viewer[0] = 1, 2
	if err := s.SetKeys(ctx, "K7M2QX", editor, viewer); err != nil {
		t.Fatalf("SetKeys: %v", err)
	}
	gotE, gotV, revoked, err := s.VerifyKeyHash(ctx, "K7M2QX")
	if err != nil {
		t.Fatalf("VerifyKeyHash: %v", err)
	}
	if gotE != editor || gotV != viewer || revoked != 0 {
		t.Fatalf("hashes = %v/%v revoked=%d", gotE[0], gotV[0], revoked)
	}

	at, err := s.BumpReadonlyRevocation(ctx, "K7M2QX")
	if err != nil {
		t.Fatalf("BumpReadonlyRevocation: %v", err)
	}
	if at <= 0 {
		t.Fatalf("watermark = %d, want a positive ms timestamp", at)
	}
	_, _, revoked, _ = s.VerifyKeyHash(ctx, "K7M2QX")
	if revoked != at {
		t.Fatalf("watermark = %d, want %d", revoked, at)
	}
}

func TestAppendAssignsContiguousSeq(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	createRoom(t, s, "K7M2QX")

	for i := 1; i <= 5; i++ {
		seq, dup, err := s.AppendOp(ctx, room.OpRecord{
			Code: "K7M2QX", OpID: "op" + string(rune('0'+i)), ClientID: "c1",
			Action: "create", Payload: []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("AppendOp #%d: %v", i, err)
		}
		if dup || seq != int64(i) {
			t.Fatalf("append #%d = (%d,%v), want (%d,false)", i, seq, dup, i)
		}
	}
	latest, err := s.LatestSeq(ctx, "K7M2QX")
	if err != nil || latest != 5 {
		t.Fatalf("LatestSeq = (%d,%v)", latest, err)
	}

	ops, truncated, err := s.LoadOps(ctx, "K7M2QX", 0, 100, 1<<20)
	if err != nil || truncated || len(ops) != 5 {
		t.Fatalf("LoadOps = (%d ops, truncated=%v, %v)", len(ops), truncated, err)
	}
	for i, op := range ops {
		if op.Seq != int64(i+1) {
			t.Errorf("ops[%d].Seq = %d", i, op.Seq)
		}
	}
}

func TestAppendHonoursSuppliedSeq(t *testing.T) {
	// In BROKER=redis mode the seq comes from a shared allocator; the store must use
	// it verbatim, otherwise the log would disagree with what clients were told.
	ctx := context.Background()
	s := newStore(t)
	createRoom(t, s, "K7M2QX")

	seq, dup, err := s.AppendOp(ctx, room.OpRecord{
		Code: "K7M2QX", Seq: 42, OpID: "a", ClientID: "c", Action: "create", Payload: []byte(`{}`),
	})
	if err != nil || dup || seq != 42 {
		t.Fatalf("supplied seq = (%d,%v,%v), want (42,false,nil)", seq, dup, err)
	}
	latest, _ := s.LatestSeq(ctx, "K7M2QX")
	if latest != 42 {
		t.Fatalf("LatestSeq = %d, want 42", latest)
	}
	// A following local allocation must continue past the supplied value.
	next, _, err := s.AppendOp(ctx, room.OpRecord{
		Code: "K7M2QX", OpID: "b", ClientID: "c", Action: "create", Payload: []byte(`{}`),
	})
	if err != nil || next != 43 {
		t.Fatalf("next seq = (%d,%v), want 43", next, err)
	}
}

func TestAppendIdempotency(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	createRoom(t, s, "K7M2QX")

	rec := room.OpRecord{Code: "K7M2QX", OpID: "abc", ClientID: "c1", Action: "create", Payload: []byte(`{}`)}
	first, dup, err := s.AppendOp(ctx, rec)
	if err != nil || dup {
		t.Fatalf("first = (%d,%v,%v)", first, dup, err)
	}
	again, dup, err := s.AppendOp(ctx, rec)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !dup || again != first {
		t.Fatalf("duplicate = (%d,%v), want (%d,true)", again, dup, first)
	}
	latest, _ := s.LatestSeq(ctx, "K7M2QX")
	if latest != 1 {
		t.Fatalf("a duplicate consumed a seq: latest = %d", latest)
	}

	// Same opID from a different client is a distinct op.
	other := room.OpRecord{Code: "K7M2QX", OpID: "abc", ClientID: "c2", Action: "create", Payload: []byte(`{}`)}
	seq, dup, err := s.AppendOp(ctx, other)
	if err != nil || dup || seq != 2 {
		t.Fatalf("different cid = (%d,%v,%v)", seq, dup, err)
	}
}

func TestLoadOpsBytesAndLimit(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	createRoom(t, s, "K7M2QX")
	payload := make([]byte, 100)
	for i := 0; i < 10; i++ {
		if _, _, err := s.AppendOp(ctx, room.OpRecord{
			Code: "K7M2QX", OpID: string(rune('a' + i)), ClientID: "c", Action: "create", Payload: payload,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	ops, truncated, err := s.LoadOps(ctx, "K7M2QX", 0, 3, 1<<20)
	if err != nil || len(ops) != 3 || !truncated {
		t.Fatalf("limit: %d ops truncated=%v err=%v", len(ops), truncated, err)
	}
	ops, truncated, err = s.LoadOps(ctx, "K7M2QX", 0, 100, 250)
	if err != nil || len(ops) != 2 || !truncated {
		t.Fatalf("bytes: %d ops truncated=%v err=%v", len(ops), truncated, err)
	}
	ops, truncated, err = s.LoadOps(ctx, "K7M2QX", 8, 100, 1<<20)
	if err != nil || len(ops) != 2 || truncated || ops[0].Seq != 9 {
		t.Fatalf("after=8: %d ops first=%d truncated=%v", len(ops), ops[0].Seq, truncated)
	}
}

func TestPayloadIsCopied(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	createRoom(t, s, "K7M2QX")

	payload := []byte("original")
	if _, _, err := s.AppendOp(ctx, room.OpRecord{
		Code: "K7M2QX", OpID: "a", ClientID: "c", Action: "create", Payload: payload,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	payload[0] = 'X' // caller reuses its buffer (the ws read pump does)
	ops, _, _ := s.LoadOps(ctx, "K7M2QX", 0, 10, 1<<20)
	if string(ops[0].Payload) != "original" {
		t.Fatalf("stored payload was aliased: %q", ops[0].Payload)
	}
	ops[0].Payload[0] = 'Y'
	again, _, _ := s.LoadOps(ctx, "K7M2QX", 0, 10, 1<<20)
	if string(again[0].Payload) != "original" {
		t.Fatalf("LoadOps returned an aliased copy: %q", again[0].Payload)
	}
}

func TestTrimOps(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	createRoom(t, s, "K7M2QX")
	for i := 0; i < 10; i++ {
		if _, _, err := s.AppendOp(ctx, room.OpRecord{
			Code: "K7M2QX", OpID: string(rune('a' + i)), ClientID: "c", Action: "create", Payload: []byte("x"),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := s.TrimOps(ctx, "K7M2QX", 5, 0); err != nil {
		t.Fatalf("TrimOps: %v", err)
	}
	ops, _, _ := s.LoadOps(ctx, "K7M2QX", 0, 100, 1<<20)
	if len(ops) != 5 || ops[0].Seq != 6 {
		t.Fatalf("after trim: %d ops first=%d", len(ops), ops[0].Seq)
	}
	// Trimming must not reset the counter.
	seq, _, err := s.AppendOp(ctx, room.OpRecord{Code: "K7M2QX", OpID: "zz", ClientID: "c", Action: "create", Payload: []byte("x")})
	if err != nil || seq != 11 {
		t.Fatalf("post-trim append = (%d,%v), want 11", seq, err)
	}
	// keep overrides an aggressive upto.
	if err := s.TrimOps(ctx, "K7M2QX", 100, 3); err != nil {
		t.Fatalf("TrimOps(keep): %v", err)
	}
	ops, _, _ = s.LoadOps(ctx, "K7M2QX", 0, 100, 1<<20)
	if len(ops) != 3 || ops[0].Seq != 9 {
		t.Fatalf("after keep=3: %d ops first=%d", len(ops), ops[0].Seq)
	}
}

func TestReplayRange(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	createRoom(t, s, "K7M2QX")
	for i := 1; i <= 6; i++ {
		if _, _, err := s.AppendOp(ctx, room.OpRecord{
			Code: "K7M2QX", OpID: "op" + string(rune('0'+i)), ClientID: "c", Action: "create", Payload: []byte("x"),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	ops, err := s.ReplayRange(ctx, "K7M2QX", 3, 5)
	if err != nil {
		t.Fatalf("ReplayRange: %v", err)
	}
	if len(ops) != 3 || ops[0].Seq != 3 || ops[2].Seq != 5 {
		t.Fatalf("replay = %d ops, seqs %v", len(ops), seqsOf(ops))
	}
}

func seqsOf(ops []room.ReplayOp) []int64 {
	out := make([]int64, len(ops))
	for i, op := range ops {
		out[i] = op.Seq
	}
	return out
}

func TestSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	createRoom(t, s, "K7M2QX")

	if _, err := s.LoadSnapshot(ctx, "K7M2QX"); !errors.Is(err, room.ErrNoSnapshot) {
		t.Fatalf("empty snapshot = %v, want ErrNoSnapshot", err)
	}
	if err := s.SaveSnapshot(ctx, "K7M2QX", 1, 10, 3, []byte("[]")); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	// An older snapshot must not replace a newer one.
	if err := s.SaveSnapshot(ctx, "K7M2QX", 1, 5, 1, []byte("old")); err != nil {
		t.Fatalf("SaveSnapshot(old): %v", err)
	}
	got, err := s.LoadSnapshot(ctx, "K7M2QX")
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if got.Seq != 10 || got.ShapeCount != 3 {
		t.Fatalf("snapshot = %+v", got)
	}
	got.Payload[0] = 'X'
	again, _ := s.LoadSnapshot(ctx, "K7M2QX")
	if string(again.Payload) != "[]" {
		t.Fatal("LoadSnapshot returned an aliased payload")
	}
}

func TestLoadRoomReplaysOpsAfterSnapshot(t *testing.T) {
	// The room is rebuilt from the newest snapshot plus the ops after it. Replaying
	// an op the snapshot already contains would double-apply it.
	ctx := context.Background()
	s := newStore(t)
	createRoom(t, s, "K7M2QX")
	for i := 1; i <= 3; i++ {
		if _, _, err := s.AppendOp(ctx, room.OpRecord{
			Code: "K7M2QX", OpID: "op" + string(rune('0'+i)), ClientID: "c", Action: "create", Payload: []byte("x"),
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := s.SaveSnapshot(ctx, "K7M2QX", 1, 2, 2, []byte("[]")); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	rec, err := s.LoadRoom(ctx, "K7M2QX")
	if err != nil {
		t.Fatalf("LoadRoom: %v", err)
	}
	if rec.Snapshot == nil || rec.Snapshot.Seq != 2 {
		t.Fatalf("snapshot = %+v", rec.Snapshot)
	}
	if len(rec.Ops) != 1 || rec.Ops[0].Seq != 3 {
		t.Fatalf("ops = %v, want only seq 3", seqsOf(rec.Ops))
	}
	if rec.Seq != 3 {
		t.Fatalf("rec.Seq = %d, want 3", rec.Seq)
	}
}

func TestMetaAndTitle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	createRoom(t, s, "K7M2QX")

	meta, err := s.GetRoomMeta(ctx, "K7M2QX")
	if err != nil {
		t.Fatalf("GetRoomMeta: %v", err)
	}
	if meta.Code != "K7M2QX" || meta.Epoch != 1 {
		t.Fatalf("meta = %+v", meta)
	}
	if err := s.SetTitle(ctx, "K7M2QX", "新标题"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	meta, _ = s.GetRoomMeta(ctx, "K7M2QX")
	if meta.Title != "新标题" {
		t.Fatalf("title = %q", meta.Title)
	}
	if err := s.SetTitle(ctx, "ZZZZZZ", "x"); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("SetTitle on a missing room = %v", err)
	}
	if err := s.TouchRoom(ctx, "K7M2QX", 7, time.Now()); err != nil {
		t.Fatalf("TouchRoom: %v", err)
	}
	meta, _ = s.GetRoomMeta(ctx, "K7M2QX")
	if meta.ShapeCount != 7 {
		t.Fatalf("ShapeCount = %d", meta.ShapeCount)
	}
}

func TestDeleteAndClose(t *testing.T) {
	ctx := context.Background()
	s := New(Options{})
	createRoom(t, s, "K7M2QX")
	if err := s.DeleteRoom(ctx, "K7M2QX"); err != nil {
		t.Fatalf("DeleteRoom: %v", err)
	}
	if _, err := s.LoadRoom(ctx, "K7M2QX"); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("after delete = %v", err)
	}
	if err := s.DeleteRoom(ctx, "K7M2QX"); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := s.Ping(ctx); err == nil {
		t.Fatal("Ping after Close should fail")
	}
	if _, err := s.LoadRoom(ctx, "K7M2QX"); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("LoadRoom after close = %v", err)
	}
}

func TestContextCancellation(t *testing.T) {
	s := newStore(t)
	createRoom(t, s, "K7M2QX")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.LoadRoom(ctx, "K7M2QX"); !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadRoom with cancelled ctx = %v", err)
	}
	if _, _, err := s.AppendOp(ctx, room.OpRecord{Code: "K7M2QX", OpID: "a", ClientID: "c"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("AppendOp with cancelled ctx = %v", err)
	}
}

func TestBackendName(t *testing.T) {
	if got := newStore(t).Backend(); got != "memory" {
		t.Fatalf("Backend = %q", got)
	}
}

// TestOwnerAndGrantsPersistInMemory covers docs/PERMISSIONS.md §7 for STORE=memory:
// owner and grants must round-trip through LoadRoom with the same semantics as
// Postgres, even though a restart loses them.
func TestOwnerAndGrantsPersistInMemory(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	createRoom(t, s, "K7M2QX")

	if err := s.SaveOwner(ctx, "K7M2QX", "c_owner001"); err != nil {
		t.Fatalf("SaveOwner: %v", err)
	}
	if err := s.SaveGrant(ctx, "K7M2QX", "c_editor01", "editor", "c_owner001", time.Now()); err != nil {
		t.Fatalf("SaveGrant: %v", err)
	}
	// An upsert must replace, not duplicate.
	if err := s.SaveGrant(ctx, "K7M2QX", "c_editor01", "viewer", "c_owner001", time.Now()); err != nil {
		t.Fatalf("SaveGrant upsert: %v", err)
	}

	meta, err := s.GetRoomMeta(ctx, "K7M2QX")
	if err != nil {
		t.Fatalf("GetRoomMeta: %v", err)
	}
	if meta.OwnerClientID != "c_owner001" {
		t.Fatalf("meta owner = %q", meta.OwnerClientID)
	}

	rec, err := s.LoadRoom(ctx, "K7M2QX")
	if err != nil {
		t.Fatalf("LoadRoom: %v", err)
	}
	if rec.OwnerClientID != "c_owner001" {
		t.Fatalf("record owner = %q", rec.OwnerClientID)
	}
	if got := rec.Grants["c_editor01"]; got != "viewer" {
		t.Fatalf("grant = %q, want the upserted viewer", got)
	}

	// DeleteGrant removes it (used when the room becomes unowned).
	if err := s.DeleteGrant(ctx, "K7M2QX", "c_editor01"); err != nil {
		t.Fatalf("DeleteGrant: %v", err)
	}
	rec, _ = s.LoadRoom(ctx, "K7M2QX")
	if _, ok := rec.Grants["c_editor01"]; ok {
		t.Fatal("grant survived DeleteGrant")
	}

	// Clearing the owner is meaningful: "" is the unowned state, not an error.
	if err := s.SaveOwner(ctx, "K7M2QX", ""); err != nil {
		t.Fatalf("SaveOwner(\"\"): %v", err)
	}
	if got := s.rooms["K7M2QX"].owner; got != "" {
		t.Fatalf("owner after clearing = %q", got)
	}

	// A missing room is ErrNotFound for all three writes.
	if err := s.SaveOwner(ctx, "ZZZZZZ", "c_x"); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("SaveOwner on a missing room = %v", err)
	}
	if err := s.SaveGrant(ctx, "ZZZZZZ", "c_x", "editor", "c_y", time.Now()); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("SaveGrant on a missing room = %v", err)
	}
	if err := s.SaveGrant(ctx, "K7M2QX", "c_x", "owner", "c_y", time.Now()); err == nil {
		t.Fatal("SaveGrant accepted the owner role; only room.transfer may set it")
	}
}
