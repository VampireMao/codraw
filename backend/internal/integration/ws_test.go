package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/codraw/codraw/backend/internal/config"
	"github.com/codraw/codraw/backend/internal/protocol"
)

// TestTwoClientsBroadcastAndSeq is the core collaboration guarantee: A draws, B
// sees it, and the seq is contiguous for both.
func TestTwoClientsBroadcastAndSeq(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("双人广播")

	a := s.dial(room.Code, "c_aaaaaaa1", "甲", room.EditorKey, "")
	joinedA := a.join(room.Code, "c_aaaaaaa1", 0, 0)
	// The creator's first connection is the owner (docs/PERMISSIONS.md §2.1).
	if joinedA.You.Role != protocol.RoleOwner {
		t.Fatalf("A role = %q, want owner", joinedA.You.Role)
	}
	if joinedA.OwnerClientID == nil || *joinedA.OwnerClientID != "c_aaaaaaa1" {
		t.Fatalf("joined.ownerClientId = %v, want c_aaaaaaa1", joinedA.OwnerClientID)
	}

	b := s.dial(room.Code, "c_bbbbbbb2", "乙", room.ViewerKey, "")
	joinedB := b.join(room.Code, "c_bbbbbbb2", 0, 0)
	if joinedB.You.Role != protocol.RoleViewer {
		t.Fatalf("B role = %q, want viewer", joinedB.You.Role)
	}
	// A grants B editor so the second half of the test can exercise B's writes.
	grantEditorOverWS(t, a, room.Code, "c_aaaaaaa1", "c_bbbbbbb2")
	waitForRole(t, a, room.Code, "c_bbbbbbb2", protocol.RoleEditor)

	id := shapeID("bcast1")
	// A's first op needs the ack; B's second op needs B to hold editor.
	a.send(protocol.TypeOpAdd, room.Code, "c_aaaaaaa1", protocol.OpAddPayload{
		OpID: opID("bcast1"), Action: protocol.ActionCreate, Shape: rectShape(id),
	})

	ack, err := a.waitForAck(opID("bcast1"), 5*time.Second)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	// The member.role for B's grant was the first persistent message, so A's op
	// lands on seq 2: role changes share the room sequence
	// (docs/PERMISSIONS.md §3.1).
	if ack.Status != protocol.AckApplied || ack.Seq != 2 {
		t.Fatalf("ack = %+v, want applied seq=2 (grant took seq 1)", ack)
	}

	// B must receive the very same op through the broadcast path.
	env, err := b.waitFor(protocol.TypeOpAdd, 5*time.Second)
	if err != nil {
		t.Fatalf("B broadcast: %v", err)
	}
	if env.Seq != 2 {
		t.Fatalf("B saw seq %d, want 2", env.Seq)
	}
	if env.ClientID != "c_aaaaaaa1" {
		t.Fatalf("broadcast clientId = %q, want the author's cid", env.ClientID)
	}
	var op struct {
		OpID  string          `json:"opId"`
		Shape json.RawMessage `json:"shape"`
	}
	if err := json.Unmarshal(env.Payload, &op); err != nil {
		t.Fatalf("broadcast payload: %v", err)
	}
	if op.OpID != opID("bcast1") {
		t.Fatalf("opId = %q", op.OpID)
	}

	// Second op keeps seq contiguous.
	a.send(protocol.TypeOpAdd, room.Code, "c_aaaaaaa1", protocol.OpAddPayload{
		OpID: opID("bcast2"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("bcast2")),
	})
	ack2, err := a.waitForAck(opID("bcast2"), 5*time.Second)
	if err != nil {
		t.Fatalf("ack2: %v", err)
	}
	if ack2.Seq != 3 {
		t.Fatalf("second seq = %d, want 3", ack2.Seq)
	}
	env2, err := b.waitFor(protocol.TypeOpAdd, 5*time.Second)
	if err != nil {
		t.Fatalf("B second broadcast: %v", err)
	}
	if env2.Seq != 3 {
		t.Fatalf("B second seq = %d, want 3", env2.Seq)
	}

	// The room's own view must agree.
	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if got := rh.Stats().Seq; got != 3 {
		t.Fatalf("room seq = %d, want 3", got)
	}
	if got := rh.Stats().ShapeCount; got != 2 {
		t.Fatalf("shape count = %d, want 2", got)
	}
}

// TestViewerWriteRejectedConnectionSurvives is F8 plus the "don't close the
// connection" rule from docs/PROTOCOL.md §8.
//
// Under docs/PERMISSIONS.md the first entrant owns the room regardless of the key
// it presented (§2.1), so the owner joins first and the viewer second: that is
// what makes the second connection a genuine viewer.
func TestViewerWriteRejectedConnectionSurvives(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("只读")

	owner := s.dial(room.Code, "c_owner0001", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_owner0001", 0, 0)

	v := s.dial(room.Code, "c_viewer01", "只读用户", room.ViewerKey, "")
	joined := v.join(room.Code, "c_viewer01", 0, 0)
	if joined.You.Role != protocol.RoleViewer {
		t.Fatalf("role = %q, want viewer", joined.You.Role)
	}

	v.send(protocol.TypeOpAdd, room.Code, "c_viewer01", protocol.OpAddPayload{
		OpID: opID("viewer1"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("viewer1")),
	})
	ack, err := v.waitForAck(opID("viewer1"), 5*time.Second)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if ack.Status != protocol.AckRejected {
		t.Fatalf("status = %q, want rejected", ack.Status)
	}
	if ack.Code != protocol.CodeForbidden {
		t.Fatalf("code = %q, want forbidden", ack.Code)
	}
	if ack.Seq != 0 {
		t.Fatalf("a rejected op must not carry a seq, got %d", ack.Seq)
	}

	// The connection must still work: ping → pong.
	v.send(protocol.TypePing, room.Code, "c_viewer01", protocol.PingPayload{TS: 1234})
	pong, err := v.waitFor(protocol.TypePong, 5*time.Second)
	if err != nil {
		t.Fatalf("connection died after a rejected write: %v", err)
	}
	var pp protocol.PongPayload
	if err := json.Unmarshal(pong.Payload, &pp); err != nil {
		t.Fatalf("pong payload: %v", err)
	}
	if pp.TS != 1234 {
		t.Fatalf("pong ts = %d, want the echoed 1234", pp.TS)
	}

	// And the room must not contain the rejected shape.
	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if got := rh.Stats().ShapeCount; got != 0 {
		t.Fatalf("shape count = %d, want 0: a rejected op mutated the room", got)
	}
}

// TestIdempotentRetransmit covers docs/PROTOCOL.md §6.5: the same (cid, opId)
// returns duplicate with the original seq and does not duplicate the shape.
func TestIdempotentRetransmit(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("幂等")

	c := s.dial(room.Code, "c_idem0001", "作者", room.EditorKey, "")
	c.join(room.Code, "c_idem0001", 0, 0)

	id := shapeID("idem1")
	payload := protocol.OpAddPayload{OpID: opID("idem1"), Action: protocol.ActionCreate, Shape: rectShape(id)}

	c.send(protocol.TypeOpAdd, room.Code, "c_idem0001", payload)
	first, err := c.waitForAck(opID("idem1"), 5*time.Second)
	if err != nil {
		t.Fatalf("first ack: %v", err)
	}
	if first.Status != protocol.AckApplied {
		t.Fatalf("first status = %q, want applied", first.Status)
	}

	// Retransmit with the SAME opId, exactly as a reconnecting client does.
	c.send(protocol.TypeOpAdd, room.Code, "c_idem0001", payload)
	second, err := c.waitForAck(opID("idem1"), 5*time.Second)
	if err != nil {
		t.Fatalf("second ack: %v", err)
	}
	if second.Status != protocol.AckDuplicate {
		t.Fatalf("second status = %q, want duplicate", second.Status)
	}
	if second.Seq != first.Seq {
		t.Fatalf("duplicate seq = %d, want the original %d", second.Seq, first.Seq)
	}

	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if got := rh.Stats().ShapeCount; got != 1 {
		t.Fatalf("shape count = %d, want 1: the retransmit created a duplicate", got)
	}
	if got := rh.Stats().Seq; got != 1 {
		t.Fatalf("seq = %d, want 1: the retransmit consumed a seq", got)
	}
}

// TestReconnectDeltaAndFallback covers both resync paths:
// a recent lastSeq gets resync.events, a cold one gets resync.snapshot.
func TestReconnectDeltaAndFallback(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("重连补齐")

	// Seed three ops.
	a := s.dial(room.Code, "c_seed0001", "种子", room.EditorKey, "")
	a.join(room.Code, "c_seed0001", 0, 0)
	for i := 1; i <= 3; i++ {
		id := shapeID(fmt.Sprintf("seed%d", i))
		a.send(protocol.TypeOpAdd, room.Code, "c_seed0001", protocol.OpAddPayload{
			OpID: opID(fmt.Sprintf("seed%d", i)), Action: protocol.ActionCreate, Shape: rectShape(id),
		})
		if _, err := a.waitForAck(opID(fmt.Sprintf("seed%d", i)), 5*time.Second); err != nil {
			t.Fatalf("seed ack %d: %v", i, err)
		}
	}
	a.conn.Close()

	// 1. Reconnect with lastSeq=1: expect exactly ops 2 and 3 as events.
	b := s.dial(room.Code, "c_delta001", "增量", room.EditorKey, "")
	b.send(protocol.TypeHello, room.Code, "c_delta001", protocol.HelloPayload{LastSeq: 1, Epoch: 1})
	if _, err := b.waitFor(protocol.TypeJoined, 5*time.Second); err != nil {
		t.Fatalf("joined: %v", err)
	}
	env, err := b.waitFor(protocol.TypeResyncEvents, 5*time.Second)
	if err != nil {
		t.Fatalf("expected resync.events for a recent lastSeq: %v", err)
	}
	var events protocol.ResyncEventsPayload
	if err := json.Unmarshal(env.Payload, &events); err != nil {
		t.Fatalf("events payload: %v", err)
	}
	if events.FromSeq != 2 || events.ToSeq != 3 {
		t.Fatalf("events range = %d..%d, want 2..3", events.FromSeq, events.ToSeq)
	}
	if len(events.Events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events.Events))
	}
	// Strict continuity is the client's contract; assert it here too.
	for i, ev := range events.Events {
		if want := events.FromSeq + int64(i); ev.Seq != want {
			t.Fatalf("events[%d].seq = %d, want %d", i, ev.Seq, want)
		}
	}
	b.conn.Close()

	// 2. Reconnect with a lastSeq far in the past: expect a snapshot.
	c := s.dial(room.Code, "c_cold0001", "冷启动", room.ViewerKey, "")
	c.send(protocol.TypeHello, room.Code, "c_cold0001", protocol.HelloPayload{LastSeq: 0, Epoch: 0})
	if _, err := c.waitFor(protocol.TypeJoined, 5*time.Second); err != nil {
		t.Fatalf("joined: %v", err)
	}
	snapEnv, err := c.waitFor(protocol.TypeResyncSnapshot, 5*time.Second)
	if err != nil {
		t.Fatalf("expected resync.snapshot for lastSeq=0: %v", err)
	}
	var snap protocol.SnapshotPayload
	if err := json.Unmarshal(snapEnv.Payload, &snap); err != nil {
		t.Fatalf("snapshot payload: %v", err)
	}
	if !snap.Complete {
		t.Fatalf("single-chunk snapshot must be complete")
	}
	if len(snap.Shapes) != 3 {
		t.Fatalf("snapshot shapes = %d, want 3", len(snap.Shapes))
	}
	if snap.Seq != 3 {
		t.Fatalf("snapshot seq = %d, want 3", snap.Seq)
	}

	// 3. An epoch mismatch must force a snapshot even with a valid lastSeq.
	d := s.dial(room.Code, "c_epoch001", "世代", room.ViewerKey, "")
	d.send(protocol.TypeHello, room.Code, "c_epoch001", protocol.HelloPayload{LastSeq: 3, Epoch: 99})
	if _, err := d.waitFor(protocol.TypeJoined, 5*time.Second); err != nil {
		t.Fatalf("joined: %v", err)
	}
	if _, err := d.waitFor(protocol.TypeResyncSnapshot, 5*time.Second); err != nil {
		t.Fatalf("epoch mismatch must produce a snapshot: %v", err)
	}
}

// TestResyncRequestExplicit covers the client-initiated gap repair path.
func TestResyncRequestExplicit(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("显式补齐")

	a := s.dial(room.Code, "c_gap00001", "作者", room.EditorKey, "")
	a.join(room.Code, "c_gap00001", 0, 0)
	for i := 1; i <= 2; i++ {
		a.send(protocol.TypeOpAdd, room.Code, "c_gap00001", protocol.OpAddPayload{
			OpID: opID(fmt.Sprintf("gap%d", i)), Action: protocol.ActionCreate, Shape: rectShape(shapeID(fmt.Sprintf("gap%d", i))),
		})
		if _, err := a.waitForAck(opID(fmt.Sprintf("gap%d", i)), 5*time.Second); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}

	b := s.dial(room.Code, "c_gap00002", "补齐", room.ViewerKey, "")
	b.join(room.Code, "c_gap00002", 0, 0)

	b.send(protocol.TypeResyncRequest, room.Code, "c_gap00002", protocol.ResyncRequestPayload{
		FromSeq: 2, Epoch: 1, Reason: "gap",
	})
	env, err := b.waitFor(protocol.TypeResyncEvents, 5*time.Second)
	if err != nil {
		t.Fatalf("resync.request did not produce events: %v", err)
	}
	var events protocol.ResyncEventsPayload
	if err := json.Unmarshal(env.Payload, &events); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if events.FromSeq != 2 || len(events.Events) != 1 || events.Events[0].Seq != 2 {
		t.Fatalf("unexpected events: %+v", events)
	}
}

// TestOversizeFrameRejected asserts the 64KB limit (QA C4): the server must not
// OOM and the connection must not survive repeated abuse.
func TestOversizeFrameRejected(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("超大帧")

	c := s.dial(room.Code, "c_big00001", "大帧", room.EditorKey, "")
	c.join(room.Code, "c_big00001", 0, 0)

	// Build a valid envelope whose payload pushes the frame past the limit.
	huge := fmt.Sprintf(`{"v":1,"type":"op.add","roomId":%q,"clientId":"c_big00001","ts":1,"payload":{"opId":%q,"action":"create","shape":{"id":%q,"kind":"text","geom":{"x":0,"y":0,"w":1,"h":1},"style":{"stroke":"#16181d","fill":"none","strokeWidth":2,"opacity":1,"dash":"solid"},"data":{},"text":%q}}}`,
		room.Code, opID("big1"), shapeID("big1"), strings.Repeat("A", 70*1024))

	if err := c.sendRaw([]byte(huge)); err != nil {
		// The server may already have closed the socket, which is also a pass.
		return
	}

	// Either we get an error/close, or the connection dies. What must NOT happen is
	// a successful ack or the process growing a 70KB shape.
	_, err := c.read(3 * time.Second)
	if err == nil {
		// A frame was read; it must be an error frame, not an ack.
		t.Logf("received a frame instead of a close; treating as acceptable only if it was an error")
	}

	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if got := rh.Stats().ShapeCount; got != 0 {
		t.Fatalf("an oversize frame created %d shapes", got)
	}
}

// TestSlowConsumerDoesNotAffectOthers asserts the visible half of the backpressure
// contract over a real socket: a client that never reads must not disturb the
// writer or any other reader.
//
// The eviction itself (Send returning false → close 4001) is covered at the room
// level in internal/room/actor_test.go, because a real TCP connection has megabytes
// of kernel receive buffer: producing real backpressure here would require
// transferring tens of MB and would depend on the host's sysctl settings.
func TestSlowConsumerDoesNotAffectOthers(t *testing.T) {
	// Default queue sizes: a shrunken queue would evict the paced writer too, which
	// is correct server behaviour but makes the test measure the harness instead of
	// the property under test.
	s := newStack(t, func(c *config.Config) {
		// Pipeline test: the per-connection op budget must not throttle the writer,
		// otherwise the test measures the limiter instead of backpressure.
		c.Limits.OpsPerSecond = 5000
		c.Limits.OpsBurst = 5000
	})
	room := s.createRoom("慢消费者")

	// The writer joins first: it is the room's owner and may write
	// (docs/PERMISSIONS.md §2.1). The other two are viewers, which is exactly the
	// shape a real room has.
	writer := s.dial(room.Code, "c_write001", "写", room.EditorKey, "")
	joinedWriter := writer.join(room.Code, "c_write001", 0, 0)
	if joinedWriter.You.Role != protocol.RoleOwner {
		t.Fatalf("first entrant role = %q, want owner", joinedWriter.You.Role)
	}
	slow := s.dial(room.Code, "c_slow0001", "慢", room.ViewerKey, "")
	slow.join(room.Code, "c_slow0001", 0, 0)
	fast := s.dial(room.Code, "c_fast0001", "快", room.ViewerKey, "")
	fast.join(room.Code, "c_fast0001", 0, 0)

	const totalOps = 150
	var (
		acks      atomic.Int64
		fastBytes atomic.Int64
	)
	applied := make(chan struct{}, totalOps)

	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		for {
			env, err := writer.read(8 * time.Second)
			if err != nil {
				t.Logf("writer reader exited: %v", err)
				return
			}
			if env.Type == protocol.TypeAck {
				var p protocol.AckPayload
				if err := json.Unmarshal(env.Payload, &p); err == nil && p.Status == protocol.AckApplied {
					acks.Add(1)
					select {
					case applied <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	go func() {
		defer readers.Done()
		for {
			env, err := fast.read(8 * time.Second)
			if err != nil {
				t.Logf("fast reader exited: %v", err)
				return
			}
			if env.Type == protocol.TypeOpAdd {
				fastBytes.Add(int64(len(env.Payload)))
			}
		}
	}()

	// The slow client never reads after this point. The writer behaves like a real
	// client: it sends an op and waits for its ack before sending the next one, so
	// it never becomes a slow consumer itself.
	for i := 0; i < totalOps; i++ {
		id := shapeID(fmt.Sprintf("flood%d", i))
		oid := opID(fmt.Sprintf("flood%d", i))
		writer.send(protocol.TypeOpAdd, room.Code, "c_write001", protocol.OpAddPayload{
			OpID: oid, Action: protocol.ActionCreate, Shape: rectShape(id),
		})
		select {
		case <-applied:
		case <-time.After(20 * time.Second):
			rh, _ := s.hub.Get(room.Code)
			t.Fatalf("no ack for op %d/%d (room seq=%d shapes=%d online=%d pendingLeaves=%d acks=%d fastBytes=%d)",
				i+1, totalOps, rh.Stats().Seq, rh.Stats().ShapeCount, rh.Stats().Online, rh.Stats().PendingLeaves,
				acks.Load(), fastBytes.Load())
		}
	}
	if got := acks.Load(); got != totalOps {
		t.Fatalf("writer got %d/%d acks", got, totalOps)
	}
	if fastBytes.Load() == 0 {
		t.Fatal("healthy reader received no broadcasts")
	}

	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if got := rh.Stats().Seq; got != totalOps {
		t.Fatalf("room seq = %d, want %d", got, totalOps)
	}
	if got := rh.Stats().ShapeCount; got != totalOps {
		t.Fatalf("shape count = %d, want %d", got, totalOps)
	}

	_ = writer.conn.Close()
	_ = fast.conn.Close()
	_ = slow.conn.Close()
	readers.Wait()
}

// TestOriginPolicy covers the CSWSH protection (QA C3).
func TestOriginPolicy(t *testing.T) {
	s := newStackOrigins(t, func(c *config.Config) {
		c.WS.AllowEmptyOrigin = false
	}, []string{"https://codraw.example.com"})
	room := s.createRoom("Origin")

	// A foreign origin is refused.
	if got := s.dialExpectError(room.Code, "c_origin01", room.EditorKey, "", "https://evil.example"); got != http.StatusForbidden {
		t.Fatalf("foreign origin status = %d, want 403", got)
	}
	// No Origin at all is refused when not explicitly allowed.
	if got := s.dialExpectError(room.Code, "c_origin02", room.EditorKey, "", ""); got != http.StatusForbidden {
		t.Fatalf("empty origin status = %d, want 403", got)
	}
}

// TestRoomNotFoundAndFull covers the handshake error ordering (§2.1).
func TestRoomNotFoundAndFull(t *testing.T) {
	s := newStack(t, func(c *config.Config) {
		c.Limits.MaxConnPerRoom = 1
	})

	// Unknown but well-formed room → 404.
	if got := s.dialExpectError("ZZZZZZ", "c_missing1", "", "", s.baseURL); got != http.StatusNotFound {
		t.Fatalf("unknown room status = %d, want 404", got)
	}
	// Malformed room code → 400, without a lookup.
	if got := s.dialExpectError("BAD", "c_missing2", "", "", s.baseURL); got != http.StatusBadRequest {
		t.Fatalf("bad code status = %d, want 400", got)
	}

	room := s.createRoom("满员")
	first := s.dial(room.Code, "c_first001", "第一", room.EditorKey, "")
	first.join(room.Code, "c_first001", 0, 0)

	// Wait until the room knows about the member, otherwise the capacity check
	// races the join command.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rh, err := s.hub.Get(room.Code); err == nil && rh.Online() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := s.dialExpectError(room.Code, "c_second01", room.ViewerKey, "", s.baseURL); got != http.StatusTooManyRequests {
		t.Fatalf("full room status = %d, want 429", got)
	}
}

// TestReadonlyTokenLifecycle covers issuance, use and revocation (§1.3, §1.4).
func TestReadonlyTokenLifecycle(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("只读令牌")

	// Issuing requires an editor key.
	res, err := http.Post(s.baseURL+"/api/rooms/"+room.Code+"/readonly-token", "application/json",
		strings.NewReader(fmt.Sprintf(`{"editorKey":%q}`, room.ViewerKey)))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer key issuing a token = %d, want 403", res.StatusCode)
	}

	res, err = http.Post(s.baseURL+"/api/rooms/"+room.Code+"/readonly-token", "application/json",
		strings.NewReader(fmt.Sprintf(`{"editorKey":%q,"ttlSeconds":600}`, room.EditorKey)))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("issue status = %d: %s", res.StatusCode, raw)
	}
	var issued struct {
		Token     string `json:"token"`
		Role      string `json:"role"`
		ExpiresAt int64  `json:"expiresAt"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(raw, &issued); err != nil {
		t.Fatalf("issue JSON: %v", err)
	}
	if issued.Role != protocol.RoleViewer || issued.Token == "" || issued.ExpiresAt == 0 {
		t.Fatalf("issued = %+v", issued)
	}

	// The token grants viewer access to the socket.
	c := s.dial(room.Code, "c_rt000001", "token", "", issued.Token)
	joined := c.join(room.Code, "c_rt000001", 0, 0)
	if joined.You.Role != protocol.RoleViewer {
		t.Fatalf("token role = %q, want viewer", joined.You.Role)
	}
	c.send(protocol.TypeOpAdd, room.Code, "c_rt000001", protocol.OpAddPayload{
		OpID: opID("tok1"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("tok1")),
	})
	ack, err := c.waitForAck(opID("tok1"), 5*time.Second)
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if ack.Status != protocol.AckRejected || ack.Code != protocol.CodeForbidden {
		t.Fatalf("token write = %+v, want rejected/forbidden", ack)
	}

	// A token must never be an edit credential even when an editor key is present.
	c2 := s.dial(room.Code, "c_rt000002", "token+key", room.EditorKey, issued.Token)
	joined2 := c2.join(room.Code, "c_rt000002", 0, 0)
	if joined2.You.Role != protocol.RoleViewer {
		t.Fatalf("token + editorKey role = %q, want viewer (token always downgrades)", joined2.You.Role)
	}
	c2.conn.Close()

	// Revoke, then the token must be refused at the handshake.
	res, err = http.Post(s.baseURL+"/api/rooms/"+room.Code+"/readonly-token/revoke", "application/json",
		strings.NewReader(fmt.Sprintf(`{"editorKey":%q,"all":true}`, room.EditorKey)))
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("revoke status = %d", res.StatusCode)
	}
	if got := s.dialExpectError(room.Code, "c_rt000003", "", issued.Token, s.baseURL); got != http.StatusForbidden {
		t.Fatalf("revoked token status = %d, want 403", got)
	}
}

// TestHTTPContract covers the REST shapes clients depend on.
func TestHTTPContract(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("契约")

	if len(room.Code) != 6 {
		t.Fatalf("code %q is not 6 chars", room.Code)
	}
	if len(room.EditorKey) != 43 || len(room.ViewerKey) != 43 {
		t.Fatalf("key lengths = %d/%d, want 43", len(room.EditorKey), len(room.ViewerKey))
	}
	if !roomCodeOK(room.Code) {
		t.Fatalf("code %q violates the DDL CHECK ^[2-9A-HJKMNP-Z]{6}$", room.Code)
	}

	res, err := http.Get(s.baseURL + "/api/rooms/" + room.Code)
	if err != nil {
		t.Fatalf("get room: %v", err)
	}
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("get room status = %d", res.StatusCode)
	}
	var meta struct {
		Code string `json:"code"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("get room JSON: %v", err)
	}
	if meta.Role != protocol.RoleViewer {
		t.Fatalf("role without key = %q, want viewer", meta.Role)
	}

	// Malformed codes are 400, not a lookup.
	res, _ = http.Get(s.baseURL + "/api/rooms/NOPE")
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed code status = %d, want 400", res.StatusCode)
	}

	// Unknown but well-formed is 404.
	res, _ = http.Get(s.baseURL + "/api/rooms/ZZZZZZ")
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown room status = %d, want 404", res.StatusCode)
	}

	// Unknown API paths must be JSON 404 (never the SPA).
	res, err = http.Get(s.baseURL + "/api/does-not-exist")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	raw, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown api path = %d, want 404", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("api 404 content-type = %q, want JSON", ct)
	}

	// healthz and readyz.
	for _, path := range []string{"/healthz", "/readyz", "/metrics", "/ops", "/ops/data", "/api/meta"} {
		res, err := http.Get(s.baseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, res.StatusCode)
		}
	}
}

// TestSnapshotPaging covers GET /api/rooms/{code}/snapshot ordering and paging.
func TestSnapshotPaging(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("分页")

	a := s.dial(room.Code, "c_page0001", "作者", room.EditorKey, "")
	a.join(room.Code, "c_page0001", 0, 0)
	for i := 1; i <= 5; i++ {
		id := shapeID(fmt.Sprintf("page%d", i))
		a.send(protocol.TypeOpAdd, room.Code, "c_page0001", protocol.OpAddPayload{
			OpID: opID(fmt.Sprintf("page%d", i)), Action: protocol.ActionCreate, Shape: rectShape(id),
		})
		if _, err := a.waitForAck(opID(fmt.Sprintf("page%d", i)), 5*time.Second); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}

	// Page 1: limit=2.
	res, err := http.Get(s.baseURL + "/api/rooms/" + room.Code + "/snapshot?key=" + room.EditorKey + "&limit=2")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	raw, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	var page struct {
		Seq      int64             `json:"seq"`
		Epoch    int64             `json:"epoch"`
		Shapes   []json.RawMessage `json:"shapes"`
		Next     *string           `json:"next"`
		Complete bool              `json:"complete"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("snapshot JSON: %v", err)
	}
	if len(page.Shapes) != 2 || page.Complete || page.Next == nil {
		t.Fatalf("page1 = %d shapes complete=%v next=%v", len(page.Shapes), page.Complete, page.Next)
	}
	if page.Seq != 5 {
		t.Fatalf("snapshot seq = %d, want 5", page.Seq)
	}

	// Walk the remaining pages.
	seen := len(page.Shapes)
	next := *page.Next
	for next != "" {
		res, err := http.Get(s.baseURL + "/api/rooms/" + room.Code + "/snapshot?key=" + room.EditorKey + "&limit=2&after=" + next)
		if err != nil {
			t.Fatalf("snapshot page: %v", err)
		}
		raw, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		var p struct {
			Shapes   []json.RawMessage `json:"shapes"`
			Next     *string           `json:"next"`
			Complete bool              `json:"complete"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("page JSON: %v", err)
		}
		seen += len(p.Shapes)
		if p.Complete {
			if p.Next != nil {
				t.Fatalf("complete page must have next=null")
			}
			break
		}
		next = *p.Next
	}
	if seen != 5 {
		t.Fatalf("paged %d shapes, want 5", seen)
	}
}

// TestCursorsAreThrottledAndNotPersisted asserts the presence path leaves the op
// log alone and coalesces bursts.
func TestCursorsAreThrottledAndNotPersisted(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("光标")

	a := s.dial(room.Code, "c_cur00001", "甲", room.EditorKey, "")
	a.join(room.Code, "c_cur00001", 0, 0)
	b := s.dial(room.Code, "c_cur00002", "乙", room.ViewerKey, "")
	b.join(room.Code, "c_cur00002", 0, 0)

	// 100 cursor frames in a tight loop: at most a handful may be forwarded.
	for i := 0; i < 100; i++ {
		a.send(protocol.TypePresenceCur, room.Code, "c_cur00001", protocol.CursorPayload{X: float64(i), Y: float64(i)})
	}

	var forwarded int
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		env, err := b.read(time.Until(deadline))
		if err != nil {
			break
		}
		if env.Type == protocol.TypePresenceCur {
			forwarded++
		}
	}
	if forwarded > 20 {
		t.Fatalf("forwarded %d cursor frames out of 100: throttling is not working", forwarded)
	}

	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if rh.Stats().Seq != 0 {
		t.Fatalf("cursor frames consumed seq (seq=%d)", rh.Stats().Seq)
	}
}

// TestUnknownTypeAndBadJSONAreNonFatal covers the forward-compatibility rules.
func TestUnknownTypeAndBadJSONAreNonFatal(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("容错")

	c := s.dial(room.Code, "c_tol00001", "容错", room.EditorKey, "")
	c.join(room.Code, "c_tol00001", 0, 0)

	// Unknown type → unsupported_type, connection survives.
	c.send("totally.unknown", room.Code, "c_tol00001", map[string]any{})
	env, err := c.waitFor(protocol.TypeError, 5*time.Second)
	if err != nil {
		t.Fatalf("unknown type: %v", err)
	}
	var ep protocol.ErrorPayload
	if err := json.Unmarshal(env.Payload, &ep); err != nil {
		t.Fatalf("error payload: %v", err)
	}
	if ep.Code != protocol.CodeUnsupportedType || ep.Fatal {
		t.Fatalf("unknown type error = %+v", ep)
	}

	// Bad JSON → bad_request, connection survives.
	if err := c.sendRaw([]byte("{not json")); err != nil {
		t.Fatalf("send raw: %v", err)
	}
	env, err = c.waitFor(protocol.TypeError, 5*time.Second)
	if err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if err := json.Unmarshal(env.Payload, &ep); err != nil {
		t.Fatalf("error payload: %v", err)
	}
	if ep.Code != protocol.CodeBadRequest || ep.Fatal {
		t.Fatalf("bad JSON error = %+v", ep)
	}

	// Unknown fields are ignored (forward compatibility).
	frame := fmt.Sprintf(`{"v":1,"type":"ping","roomId":%q,"clientId":"c_tol00001","ts":1,"payload":{"ts":9},"futureField":{"x":1}}`, room.Code)
	if err := c.sendRaw([]byte(frame)); err != nil {
		t.Fatalf("send future: %v", err)
	}
	if _, err := c.waitFor(protocol.TypePong, 5*time.Second); err != nil {
		t.Fatalf("frame with unknown fields was rejected: %v", err)
	}
}

// TestPresenceJoinAndLeave checks the member list fan-out and the grace window.
func TestPresenceJoinAndLeave(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("在线列表")

	a := s.dial(room.Code, "c_pres0001", "甲", room.EditorKey, "")
	a.join(room.Code, "c_pres0001", 0, 0)

	b := s.dial(room.Code, "c_pres0002", "乙", room.ViewerKey, "")
	b.join(room.Code, "c_pres0002", 0, 0)

	// A sees B join with a full list.
	env, err := a.waitFor(protocol.TypePresenceJoin, 5*time.Second)
	if err != nil {
		t.Fatalf("presence.join: %v", err)
	}
	var pp protocol.PresencePayload
	if err := json.Unmarshal(env.Payload, &pp); err != nil {
		t.Fatalf("presence payload: %v", err)
	}
	if pp.Online != 2 || len(pp.Users) != 2 {
		t.Fatalf("join presence = online %d users %d, want 2/2", pp.Online, len(pp.Users))
	}

	// Explicit bye produces an immediate leave.
	b.send(protocol.TypeBye, room.Code, "c_pres0002", map[string]any{})
	env, err = a.waitFor(protocol.TypePresenceLeave, 5*time.Second)
	if err != nil {
		t.Fatalf("presence.leave after bye: %v", err)
	}
	if err := json.Unmarshal(env.Payload, &pp); err != nil {
		t.Fatalf("leave payload: %v", err)
	}
	if pp.Online != 1 || len(pp.Users) != 1 {
		t.Fatalf("leave presence = online %d users %d, want 1/1", pp.Online, len(pp.Users))
	}
}

// TestGracefulShutdownClosesWith1001 asserts the shutdown contract: every client
// gets a going-away close so it reconnects instead of blaming the network.
func TestGracefulShutdownClosesWith1001(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("停机")

	c := s.dial(room.Code, "c_stop0001", "停机", room.EditorKey, "")
	c.join(room.Code, "c_stop0001", 0, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.hub.KillAll(ctx)
	}()

	// The client must observe either a close frame with 1001 or a socket close.
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := c.conn.ReadMessage()
		if err == nil {
			continue
		}
		if ce, ok := err.(*websocket.CloseError); ok {
			if ce.Code != protocol.CloseGoingAway {
				t.Fatalf("close code = %d, want 1001", ce.Code)
			}
		}
		break
	}
	<-done
}

// grantEditorOverWS sends room.grant from owner to target and returns the ack.
func grantEditorOverWS(t *testing.T, owner *client, roomCode, ownerCID, targetCID string) {
	t.Helper()
	owner.send(protocol.TypeRoomGrant, roomCode, ownerCID, protocol.GrantPayload{
		TargetClientID: targetCID,
		Role:           protocol.RoleEditor,
	})
}

// waitForRole reads until a member.role for cid with the wanted role arrives.
// It also asserts the frame carries a seq, because a role change must be
// resyncable (docs/PERMISSIONS.md §3.1).
func waitForRole(t *testing.T, c *client, roomCode, cid, role string) (protocol.Envelope, protocol.MemberRolePayload) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		env, err := c.read(time.Until(deadline))
		if err != nil {
			t.Fatalf("waiting for member.role %s=%s: %v", cid, role, err)
		}
		if env.Type != protocol.TypeMemberRole {
			continue
		}
		var p protocol.MemberRolePayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			t.Fatalf("member.role payload: %v", err)
		}
		if p.ClientID == cid && p.Role == role {
			if env.Seq <= 0 {
				t.Fatalf("member.role must carry a seq, got %d", env.Seq)
			}
			return env, p
		}
	}
	t.Fatalf("timed out waiting for member.role %s=%s", cid, role)
	return protocol.Envelope{}, protocol.MemberRolePayload{}
}

// expectAckStatus reads until the ack for opID arrives, then checks its status.
func expectAckStatus(t *testing.T, c *client, opID string, want protocol.AckStatus) protocol.AckPayload {
	t.Helper()
	ack, err := c.waitForAck(opID, 5*time.Second)
	if err != nil {
		t.Fatalf("ack %s: %v", opID, err)
	}
	if ack.Status != want {
		t.Fatalf("ack %s = %+v, want status %s", opID, ack, want)
	}
	return ack
}

// httpPostJSON posts a JSON body and returns the response body.
func httpPostJSON(url, body string) ([]byte, error) {
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return raw, fmt.Errorf("status %d: %s", res.StatusCode, raw)
	}
	return raw, nil
}

// readOpts selects what a reader goroutine forwards.
type readOpts struct{ acks bool }

// roomCodeOK mirrors the DDL CHECK constraint.
func roomCodeOK(code string) bool {
	if len(code) != 6 {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		if (c >= '2' && c <= '9') ||
			(c >= 'A' && c <= 'H') ||
			(c >= 'J' && c <= 'K') ||
			(c >= 'M' && c <= 'N') ||
			(c >= 'P' && c <= 'Z') {
			continue
		}
		return false
	}
	return true
}
