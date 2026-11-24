package room

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/codraw/codraw/backend/internal/protocol"
)

// fakeMember is a room.Member whose Send behaviour the test controls.
type fakeMember struct {
	user protocol.User

	mu    sync.Mutex
	sent  [][]byte
	kicks []kick
	// allowSend lets the test flip a member into "slow consumer" mode.
	allowSend bool
}

type kick struct {
	code   int
	reason string
}

func newFakeMember(cid, name, role string, allow bool) *fakeMember {
	return &fakeMember{
		user: protocol.User{
			ClientID: cid, UserID: "u_" + cid, Name: name,
			Color: "#16181d", Role: role,
		},
		allowSend: allow,
	}
}

func (f *fakeMember) User() protocol.User {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.user
}

// SetRole records the role the room assigned. It is what makes a mid-session
// grant observable at the Member level.
func (f *fakeMember) SetRole(role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.user.Role = role
}

func (f *fakeMember) Send(frame []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.allowSend {
		return false
	}
	f.sent = append(f.sent, frame)
	return true
}

func (f *fakeMember) Kick(code int, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kicks = append(f.kicks, kick{code: code, reason: reason})
}

// KickWithError records the kick and the error frame the room wanted delivered.
func (f *fakeMember) KickWithError(closeCode int, code protocol.ErrCode, msg string) {
	f.mu.Lock()
	f.kicks = append(f.kicks, kick{code: closeCode, reason: msg})
	f.mu.Unlock()
	payload := protocol.ErrorPayload{Code: code, Message: msg, Fatal: true}
	frame, err := protocol.Marshal(protocol.TypeError, "K7M2QX", f.user.ClientID, 0, 1, payload)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.sent = append(f.sent, frame)
	f.mu.Unlock()
}

func (f *fakeMember) setAllow(v bool) {
	f.mu.Lock()
	f.allowSend = v
	f.mu.Unlock()
}

func (f *fakeMember) frames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.sent...)
}

func (f *fakeMember) kickCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.kicks)
}

// testRoomOptions builds a room with a 50ms leave grace for fast tests.
func testRoomOptions(code string) Options {
	return Options{
		Code:               code,
		Title:              "test",
		Epoch:              1,
		Limits:             Limits{MaxConnPerRoom: 10, MaxShapeBytes: 32768, MaxShapesPerRoom: 100, OpsPerSecond: 1000, OpsBurst: 1000, CursorPerSecond: 20, ResumeMaxOps: 100, ResumeMaxBytes: 1 << 20},
		Retention:          100,
		DedupeSize:         100,
		DedupeTTL:          time.Minute,
		LeaveGrace:         50 * time.Millisecond,
		CursorThrottle:     5 * time.Millisecond,
		SnapshotEvery:      1000,
		SnapshotChunkShape: 10,
		SnapshotChunkBytes: 48 << 20,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// newTestRoom starts a room and joins the given members. Each join is announced,
// as the transport does after queueing `joined`.
//
// The first member becomes the owner (docs/PERMISSIONS.md §2.1); the rest start
// as viewers, so a test that wants writers must grant them explicitly.
func newTestRoom(t *testing.T, members ...*fakeMember) *Handle {
	t.Helper()
	h := New(testRoomOptions("K7M2QX"))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Close(ctx)
	})
	for _, m := range members {
		if _, err := h.JoinConn(context.Background(), m, false); err != nil {
			t.Fatalf("join %s: %v", m.user.ClientID, err)
		}
		h.NotifyJoined(m)
	}
	// Let the room process the joins before the caller starts writing.
	waitForOnline(t, h, len(members))
	return h
}

// waitForOnline polls until the room reports the expected member count.
func waitForOnline(t *testing.T, h *Handle, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.Stats().Online == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("room online = %d, want %d", h.Stats().Online, want)
}

// grantEditor makes target an editor, using owner as the actor. It is how a test
// sets up a writer under the permission model.
func grantEditor(t *testing.T, h *Handle, owner, target *fakeMember) {
	t.Helper()
	h.Grant(owner, &protocol.GrantPayload{TargetClientID: target.user.ClientID, Role: protocol.RoleEditor})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if target.User().Role == protocol.RoleEditor {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("grant to %s did not take effect (role=%q)", target.user.ClientID, target.User().Role)
}

// waitForSeq polls until the room reaches a seq, so tests do not depend on
// goroutine scheduling.
func waitForSeq(t *testing.T, h *Handle, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.Stats().Seq >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("room seq = %d, want >= %d", h.Stats().Seq, want)
}

func rectPayload(t *testing.T, id string) (*protocol.OpAddPayload, *protocol.Envelope) {
	t.Helper()
	blob, err := json.Marshal(map[string]any{
		"id": id, "kind": "rect",
		"geom":  map[string]any{"x": 0, "y": 0, "w": 10, "h": 10},
		"style": map[string]any{"stroke": "#16181d", "fill": "none", "strokeWidth": 2, "opacity": 1, "dash": "solid"},
		"data":  map[string]any{},
	})
	if err != nil {
		t.Fatalf("marshal shape: %v", err)
	}
	return &protocol.OpAddPayload{OpID: "op_test" + id, Action: protocol.ActionCreate, Shape: blob},
		&protocol.Envelope{V: 1, Type: protocol.TypeOpAdd, RoomID: "K7M2QX", ClientID: "c_author01"}
}

// TestSlowConsumerKickedOnFullQueue is the room-level backpressure contract: a
// member whose Send fails is evicted with 4001, and the write still succeeds for
// everyone else.
//
// This is tested at the room level rather than over a real socket because a real
// TCP connection has megabytes of kernel buffering: filling it would make the test
// slow and dependent on the host's net.ipv4.tcp_rmem.
func TestSlowConsumerKickedOnFullQueue(t *testing.T) {
	// The author joins first so it is the room's owner and may write
	// (docs/PERMISSIONS.md §2.1); the others stay viewers.
	author := newFakeMember("c_author01", "作者", protocol.RoleEditor, true)
	slow := newFakeMember("c_slow0001", "慢", protocol.RoleViewer, true)
	healthy := newFakeMember("c_fast0001", "快", protocol.RoleViewer, true)

	h := newTestRoom(t, author, slow, healthy)
	// The failure only starts now: the member accepts the handshake frames and
	// then stops reading, which is what a stalled tab does.
	slow.setAllow(false)

	opf, env := rectPayload(t, "sh_test0001")
	h.Op(author, env, opf, nil)
	waitForSeq(t, h, 1)

	// The author's write must have succeeded despite the slow member.
	if len(healthy.frames()) == 0 {
		t.Fatal("healthy member received nothing")
	}
	// The slow member must be kicked with 4001 (slow consumer).
	if got := slow.kickCount(); got != 1 {
		t.Fatalf("slow member kicked %d times, want 1", got)
	}
	slow.mu.Lock()
	code := slow.kicks[0].code
	slow.mu.Unlock()
	if code != protocol.CloseSlowConsumer {
		t.Fatalf("close code = %d, want %d (slow consumer)", code, protocol.CloseSlowConsumer)
	}
	// And the room stays usable: a second op still lands.
	opf2, env2 := rectPayload(t, "sh_test0002")
	h.Op(author, env2, opf2, nil)
	waitForSeq(t, h, 2)
	if got := h.Stats().ShapeCount; got != 2 {
		t.Fatalf("shape count = %d, want 2: the kick broke the room", got)
	}
}

// TestViewerRejectedAtRoomLevel asserts the authorisation check lives in the room,
// not only in the transport.
func TestViewerRejectedAtRoomLevel(t *testing.T) {
	// The first member becomes the owner; the second is a genuine viewer.
	owner := newFakeMember("c_owner001", "房主", protocol.RoleViewer, true)
	viewer := newFakeMember("c_viewer01", "只读", protocol.RoleViewer, true)
	h := newTestRoom(t, owner, viewer)

	opf, env := rectPayload(t, "sh_view0001")
	h.Op(viewer, env, opf, nil)

	// Give the room goroutine a moment, then assert nothing changed.
	time.Sleep(50 * time.Millisecond)
	if got := h.Stats().Seq; got != 0 {
		t.Fatalf("viewer write consumed seq %d", got)
	}
	if got := h.Stats().ShapeCount; got != 0 {
		t.Fatalf("viewer write created %d shapes", got)
	}
	// The rejection must be an ack, not a close.
	if got := viewer.kickCount(); got != 0 {
		t.Fatalf("viewer was kicked %d times; a rejected write must not close the connection", got)
	}
	frames := viewer.frames()
	if len(frames) == 0 {
		t.Fatal("viewer received no ack")
	}
	var found bool
	for _, f := range frames {
		var e protocol.Envelope
		if err := json.Unmarshal(f, &e); err != nil {
			continue
		}
		if e.Type != protocol.TypeAck {
			continue
		}
		var ack protocol.AckPayload
		if err := json.Unmarshal(e.Payload, &ack); err != nil {
			continue
		}
		if ack.Status == protocol.AckRejected && ack.Code == protocol.CodeForbidden {
			found = true
		}
	}
	if !found {
		t.Fatal("viewer did not receive ack{rejected, forbidden}")
	}
}

// TestDedupeWindowEvictsByCount pins the two-dimensional eviction rule (§6.5).
func TestDedupeWindowEvictsByCount(t *testing.T) {
	d := newDedupRing(3, time.Minute)
	now := time.Now()
	for i := 1; i <= 5; i++ {
		d.put("c1", "op"+string(rune('0'+i)), int64(i), now)
	}
	if got := d.len(); got != 3 {
		t.Fatalf("dedupe size = %d, want 3", got)
	}
	// The oldest two are gone.
	if _, ok := d.get("c1", "op1", now); ok {
		t.Fatal("op1 should have been evicted")
	}
	if seq, ok := d.get("c1", "op5", now); !ok || seq != 5 {
		t.Fatalf("op5 = (%d, %v), want (5, true)", seq, ok)
	}
}

// TestDedupeWindowEvictsByAge pins the time-based half of the same rule.
func TestDedupeWindowEvictsByAge(t *testing.T) {
	d := newDedupRing(100, 50*time.Millisecond)
	now := time.Now()
	d.put("c1", "op1", 1, now)
	if _, ok := d.get("c1", "op1", now); !ok {
		t.Fatal("entry should be live immediately")
	}
	if _, ok := d.get("c1", "op1", now.Add(100*time.Millisecond)); ok {
		t.Fatal("entry should have expired")
	}
}

// TestOpRingCovers pins the resync window arithmetic.
func TestOpRingCovers(t *testing.T) {
	r := newOpRing(3)
	for i := int64(1); i <= 5; i++ {
		r.put(logEntry{Seq: i})
	}
	if r.lastSeq() != 5 {
		t.Fatalf("lastSeq = %d, want 5", r.lastSeq())
	}
	if r.covers(3, 5) != true {
		t.Fatal("3..5 should be covered")
	}
	if r.covers(2, 5) {
		t.Fatal("2..5 should not be covered: seq 2 was evicted")
	}
	if _, ok := r.get(2); ok {
		t.Fatal("evicted entry returned")
	}
	if e, ok := r.get(4); !ok || e.Seq != 4 {
		t.Fatal("get(4) failed")
	}
}

// TestShapeValidation covers the protocol's shape rules that the room enforces.
func TestShapeValidation(t *testing.T) {
	valid := func() *protocol.Shape {
		return &protocol.Shape{
			ID: "sh_valid001", Kind: protocol.KindRect,
			Geom:  protocol.Geom{X: 0, Y: 0, W: 10, H: 10},
			Style: protocol.Style{Stroke: "#16181d", Fill: "none", StrokeWidth: 2, Opacity: 1, Dash: "solid"},
			Data:  json.RawMessage(`{}`),
		}
	}

	t.Run("accepts a valid shape", func(t *testing.T) {
		s := valid()
		if err := protocol.ValidateShape(s, 32768); err != nil {
			t.Fatalf("valid shape rejected: %v", err)
		}
	})

	t.Run("rejects a bad id", func(t *testing.T) {
		s := valid()
		s.ID = "bad"
		if err := protocol.ValidateShape(s, 32768); err == nil {
			t.Fatal("bad id accepted")
		}
	})

	t.Run("rejects an unknown kind", func(t *testing.T) {
		s := valid()
		s.Kind = "triangle"
		if err := protocol.ValidateShape(s, 32768); err == nil {
			t.Fatal("unknown kind accepted")
		}
	})

	t.Run("rejects NaN geometry", func(t *testing.T) {
		s := valid()
		s.Geom.W = 0
		s.Geom.X = 0
		// NaN cannot be produced by JSON, but the validator must still refuse it.
		s.Geom.Y = nan()
		if err := protocol.ValidateShape(s, 32768); err == nil {
			t.Fatal("NaN accepted")
		}
	})

	t.Run("rejects an uppercase colour", func(t *testing.T) {
		s := valid()
		s.Style.Stroke = "#16181D"
		if err := protocol.ValidateShape(s, 32768); err == nil {
			t.Fatal("uppercase colour accepted; the contract requires lowercase #rrggbb")
		}
	})

	t.Run("clamps strokeWidth rather than rejecting", func(t *testing.T) {
		s := valid()
		s.Style.StrokeWidth = 999
		if err := protocol.ValidateShape(s, 32768); err != nil {
			t.Fatalf("clamping should not reject: %v", err)
		}
		if s.Style.StrokeWidth != 32 {
			t.Fatalf("strokeWidth = %v, want 32", s.Style.StrokeWidth)
		}
	})

	t.Run("recomputes freehand bounds", func(t *testing.T) {
		s := valid()
		s.Kind = protocol.KindFreehand
		s.Data = json.RawMessage(`{"points":[[5,6],[15,26]]}`)
		s.Geom = protocol.Geom{X: 999, Y: 999, W: 1, H: 1}
		if err := protocol.ValidateShape(s, 32768); err != nil {
			t.Fatalf("freehand rejected: %v", err)
		}
		if s.Geom.X != 5 || s.Geom.Y != 6 || s.Geom.W != 10 || s.Geom.H != 20 {
			t.Fatalf("geom = %+v, want a server-computed box {5 6 10 20}", s.Geom)
		}
	})

	t.Run("truncates long text instead of rejecting", func(t *testing.T) {
		s := valid()
		s.Kind = protocol.KindText
		long := ""
		for i := 0; i < 2500; i++ {
			long += "字"
		}
		s.Text = &long
		if err := protocol.ValidateShape(s, 32768); err != nil {
			t.Fatalf("long text rejected instead of truncated: %v", err)
		}
		if n := len([]rune(*s.Text)); n != protocol.MaxTextRunes {
			t.Fatalf("text length = %d runes, want %d", n, protocol.MaxTextRunes)
		}
	})

	t.Run("rejects an oversize shape", func(t *testing.T) {
		s := valid()
		s.Kind = protocol.KindText
		huge := ""
		for i := 0; i < 40000; i++ {
			huge += "x"
		}
		s.Text = &huge
		err := protocol.ValidateShape(s, 1024) // a tiny limit forces the check
		if err == nil {
			t.Fatal("oversize shape accepted")
		}
		if pe, ok := protocol.AsError(err); !ok || pe.Code != protocol.CodeShapeTooLarge {
			t.Fatalf("error = %v, want shape_too_large", err)
		}
	})
}

// nan returns a NaN without importing math at every call site.
func nan() float64 {
	var zero float64
	return zero / zero
}

// TestRoomCodeMatchesDDL asserts the Go alphabet and the SQL CHECK constraint agree.
func TestRoomCodeMatchesDDL(t *testing.T) {
	for i := 0; i < 500; i++ {
		code, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if len(code) != 6 {
			t.Fatalf("length = %d", len(code))
		}
		for j := 0; j < len(code); j++ {
			if !ddlAccepts(code[j]) {
				t.Fatalf("generated code %q violates the DDL CHECK ^[2-9A-HJKMNP-Z]{6}$", code)
			}
		}
	}
	if len(CodeAlphabet) != 31 {
		t.Fatalf("alphabet has %d characters, want 31 (docs/PROTOCOL.md §0)", len(CodeAlphabet))
	}
	if containsRune(CodeAlphabet, 'L') {
		t.Fatal("alphabet contains L, which the protocol excludes")
	}
}

// ddlAccepts mirrors the DDL character class.
func ddlAccepts(c byte) bool {
	return (c >= '2' && c <= '9') ||
		(c >= 'A' && c <= 'H') ||
		(c >= 'J' && c <= 'K') ||
		(c >= 'M' && c <= 'N') ||
		(c >= 'P' && c <= 'Z')
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
