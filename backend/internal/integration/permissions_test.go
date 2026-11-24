package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/codraw/codraw/backend/internal/config"
	"github.com/codraw/codraw/backend/internal/protocol"
)

// This file covers docs/PERMISSIONS.md §9 P1–P12 end to end over real sockets.
//
// The assertions deliberately read the wire rather than internal state wherever a
// client could observe the difference: the product contract is what the browser
// sees, and several past bugs (a missing role, a stale member list, a seq gap)
// only existed on the wire.

// newStackShortGrace starts a stack with a 150ms presence grace, so inheritance
// tests do not wait the production 10s. The grace window is exactly when a
// disconnected owner hands over (§2.2), so every inheritance test needs it short.
func newStackShortGrace(t *testing.T) *stack {
	t.Helper()
	return newStack(t, func(c *config.Config) { c.WS.LeaveGrace = 150 * time.Millisecond })
}

// roleOf unwraps joined.payload.you.role with a helpful failure message.
func roleOf(t *testing.T, joined protocol.JoinedPayload, want string) {
	t.Helper()
	if joined.You.Role != want {
		t.Fatalf("joined role = %q, want %q", joined.You.Role, want)
	}
}

// ownerOf asserts joined.payload.ownerClientId.
func ownerOf(t *testing.T, joined protocol.JoinedPayload, want string) {
	t.Helper()
	if want == "" {
		if joined.OwnerClientID != nil {
			t.Fatalf("ownerClientId = %q, want null", *joined.OwnerClientID)
		}
		return
	}
	if joined.OwnerClientID == nil || *joined.OwnerClientID != want {
		got := "<nil>"
		if joined.OwnerClientID != nil {
			got = *joined.OwnerClientID
		}
		t.Fatalf("ownerClientId = %q, want %q", got, want)
	}
}

// closeCodeOf reads until the socket closes and returns the close code.
func closeCodeOf(t *testing.T, c *client, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := c.read(time.Until(deadline)); err != nil {
			var ce *websocket.CloseError
			if ok := asCloseError(err, &ce); ok {
				return ce.Code
			}
			return -1
		}
	}
	t.Fatalf("connection did not close within %s", timeout)
	return -1
}

// asCloseError is errors.As without importing errors twice in this file.
func asCloseError(err error, target **websocket.CloseError) bool {
	ce, ok := err.(*websocket.CloseError)
	if ok {
		*target = ce
	}
	return ok
}

// P1: creating a room makes the first connection the owner.
func TestP1CreatorIsOwner(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("P1 房主")

	a := s.dial(room.Code, "c_p1_owner", "房主", room.EditorKey, "")
	joined := a.join(room.Code, "c_p1_owner", 0, 0)
	roleOf(t, joined, protocol.RoleOwner)
	ownerOf(t, joined, "c_p1_owner")
	if !joined.You.IsOwner {
		t.Fatal("you.isOwner must be true for the owner")
	}

	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if got := rh.Stats().OwnerClientID; got != "c_p1_owner" {
		t.Fatalf("room owner = %q, want c_p1_owner", got)
	}
}

// P2: joining by room code with no key is viewer.
func TestP2JoinWithoutKeyIsViewer(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("P2 无 key")

	owner := s.dial(room.Code, "c_p2_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_p2_owner", 0, 0)

	b := s.dial(room.Code, "c_p2_guest", "访客", "", "")
	joined := b.join(room.Code, "c_p2_guest", 0, 0)
	roleOf(t, joined, protocol.RoleViewer)
	ownerOf(t, joined, "c_p2_owner")
	if joined.You.IsOwner {
		t.Fatal("you.isOwner must be false for a viewer")
	}

	b.send(protocol.TypeOpAdd, room.Code, "c_p2_guest", protocol.OpAddPayload{
		OpID: opID("p2write"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("p2write")),
	})
	ack := expectAckStatus(t, b, opID("p2write"), protocol.AckRejected)
	if ack.Code != protocol.CodeForbidden {
		t.Fatalf("viewer write code = %q, want forbidden", ack.Code)
	}
}

// P3: the edit link no longer grants edit; it is still a valid credential.
func TestP3EditorKeyDoesNotGrantEdit(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("P3 编辑链接")

	owner := s.dial(room.Code, "c_p3_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_p3_owner", 0, 0)

	b := s.dial(room.Code, "c_p3_editorlink", "编辑链接", room.EditorKey, "")
	joined := b.join(room.Code, "c_p3_editorlink", 0, 0)
	roleOf(t, joined, protocol.RoleViewer)
	ownerOf(t, joined, "c_p3_owner")

	b.send(protocol.TypeOpAdd, room.Code, "c_p3_editorlink", protocol.OpAddPayload{
		OpID: opID("p3write"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("p3write")),
	})
	ack := expectAckStatus(t, b, opID("p3write"), protocol.AckRejected)
	if ack.Code != protocol.CodeForbidden {
		t.Fatalf("code = %q, want forbidden", ack.Code)
	}
}

// P4: the owner grants B editor, B can write immediately, and the owner sees it.
func TestP4GrantEditor(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("P4 授权")

	owner := s.dial(room.Code, "c_p4_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_p4_owner", 0, 0)
	b := s.dial(room.Code, "c_p4_guest", "成员B", room.ViewerKey, "")
	b.join(room.Code, "c_p4_guest", 0, 0)

	grantEditorOverWS(t, owner, room.Code, "c_p4_owner", "c_p4_guest")

	// Both sides observe the role change, and it carries a seq.
	_, p := waitForRole(t, b, room.Code, "c_p4_guest", protocol.RoleEditor)
	if p.GrantedBy != "c_p4_owner" {
		t.Fatalf("grantedBy = %q, want c_p4_owner", p.GrantedBy)
	}
	waitForRole(t, owner, room.Code, "c_p4_guest", protocol.RoleEditor)

	// B can draw now: authorisation is re-evaluated per op, not cached from the
	// handshake (docs/PERMISSIONS.md §5).
	b.send(protocol.TypeOpAdd, room.Code, "c_p4_guest", protocol.OpAddPayload{
		OpID: opID("p4write"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("p4write")),
	})
	ack := expectAckStatus(t, b, opID("p4write"), protocol.AckApplied)
	if ack.Seq <= 0 {
		t.Fatalf("applied ack has no seq: %+v", ack)
	}
	owner.waitFor(protocol.TypeOpAdd, 5*time.Second)
}

// P5: revoking an editor turns the very next op into a rejected ack, and the
// connection survives (docs/PERMISSIONS.md §3.2, §8).
func TestP5RevokeKeepsConnection(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("P5 撤销")

	owner := s.dial(room.Code, "c_p5_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_p5_owner", 0, 0)
	b := s.dial(room.Code, "c_p5_guest", "成员B", room.ViewerKey, "")
	b.join(room.Code, "c_p5_guest", 0, 0)

	grantEditorOverWS(t, owner, room.Code, "c_p5_owner", "c_p5_guest")
	waitForRole(t, b, room.Code, "c_p5_guest", protocol.RoleEditor)
	b.send(protocol.TypeOpAdd, room.Code, "c_p5_guest", protocol.OpAddPayload{
		OpID: opID("p5first"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("p5first")),
	})
	expectAckStatus(t, b, opID("p5first"), protocol.AckApplied)

	// Revoke by granting viewer.
	owner.send(protocol.TypeRoomGrant, room.Code, "c_p5_owner", protocol.GrantPayload{
		TargetClientID: "c_p5_guest", Role: protocol.RoleViewer,
	})
	_, revoked := waitForRole(t, b, room.Code, "c_p5_guest", protocol.RoleViewer)
	if revoked.GrantedBy != "c_p5_owner" {
		t.Fatalf("revocation grantedBy = %q", revoked.GrantedBy)
	}

	// The next write must be rejected without closing the socket.
	b.send(protocol.TypeOpAdd, room.Code, "c_p5_guest", protocol.OpAddPayload{
		OpID: opID("p5after"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("p5after")),
	})
	ack := expectAckStatus(t, b, opID("p5after"), protocol.AckRejected)
	if ack.Code != protocol.CodeForbidden {
		t.Fatalf("post-revocation code = %q, want forbidden", ack.Code)
	}

	// Proven alive: ping → pong.
	b.send(protocol.TypePing, room.Code, "c_p5_guest", protocol.PingPayload{TS: 55})
	if _, err := b.waitFor(protocol.TypePong, 5*time.Second); err != nil {
		t.Fatalf("connection died after a rejected write: %v", err)
	}

	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if got := rh.Stats().ShapeCount; got != 1 {
		t.Fatalf("shape count = %d, want 1: the rejected op mutated the room", got)
	}
}

// P6: transfer makes the target owner and demotes the caller to editor.
func TestP6TransferOwnership(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("P6 转移")

	owner := s.dial(room.Code, "c_p6_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_p6_owner", 0, 0)
	b := s.dial(room.Code, "c_p6_guest", "成员B", room.ViewerKey, "")
	b.join(room.Code, "c_p6_guest", 0, 0)

	owner.send(protocol.TypeRoomTransfer, room.Code, "c_p6_owner", protocol.TransferPayload{
		TargetClientID: "c_p6_guest",
	})

	waitForRole(t, b, room.Code, "c_p6_guest", protocol.RoleOwner)
	waitForRole(t, owner, room.Code, "c_p6_owner", protocol.RoleEditor)

	// The new owner can grant; the old owner cannot.
	b.send(protocol.TypeRoomGrant, room.Code, "c_p6_guest", protocol.GrantPayload{
		TargetClientID: "c_p6_owner", Role: protocol.RoleEditor,
	})
	// A grant of the same role is still a successful grant: the target is online
	// and the role is legal, so the only observable effect is another member.role.
	waitForRole(t, owner, room.Code, "c_p6_owner", protocol.RoleEditor)

	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if got := rh.Stats().OwnerClientID; got != "c_p6_guest" {
		t.Fatalf("room owner = %q, want c_p6_guest", got)
	}
}

// P7: closing the owner's page transfers ownership to the second entrant.
func TestP7OwnerCloseInheritsByJoinOrder(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("P7 继承")

	owner := s.dial(room.Code, "c_p7_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_p7_owner", 0, 0)
	second := s.dial(room.Code, "c_p7_second", "第二", room.ViewerKey, "")
	second.join(room.Code, "c_p7_second", 0, 0)
	third := s.dial(room.Code, "c_p7_third", "第三", room.ViewerKey, "")
	third.join(room.Code, "c_p7_third", 0, 0)

	// Abrupt close (the tab was killed), not `bye`: this is the interesting path.
	_ = owner.conn.Close()

	// Second joined earlier than third, so it inherits.
	waitForRole(t, second, room.Code, "c_p7_second", protocol.RoleOwner)
	waitForRole(t, third, room.Code, "c_p7_second", protocol.RoleOwner)

	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if got := rh.Stats().OwnerClientID; got != "c_p7_second" {
		t.Fatalf("room owner = %q, want c_p7_second", got)
	}

	// The new owner can act; the third member still cannot.
	third.send(protocol.TypeOpAdd, room.Code, "c_p7_third", protocol.OpAddPayload{
		OpID: opID("p7third"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("p7third")),
	})
	expectAckStatus(t, third, opID("p7third"), protocol.AckRejected)
	second.send(protocol.TypeOpAdd, room.Code, "c_p7_second", protocol.OpAddPayload{
		OpID: opID("p7second"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("p7second")),
	})
	expectAckStatus(t, second, opID("p7second"), protocol.AckApplied)
}

// P7b: the same clientId with two tabs must not hand over ownership when one tab
// closes (docs/PERMISSIONS.md §2.4).
func TestP7MultiTabDoesNotInherit(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("P7b 多标签")

	tab1 := s.dial(room.Code, "c_p7b_owner", "房主", room.EditorKey, "")
	tab1.join(room.Code, "c_p7b_owner", 0, 0)
	tab2 := s.dial(room.Code, "c_p7b_owner", "房主", room.EditorKey, "")
	tab2.join(room.Code, "c_p7b_owner", 0, 0)

	other := s.dial(room.Code, "c_p7b_other", "他人", room.ViewerKey, "")
	other.join(room.Code, "c_p7b_other", 0, 0)

	_ = tab1.conn.Close()
	time.Sleep(300 * time.Millisecond)

	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if got := rh.Stats().OwnerClientID; got != "c_p7b_owner" {
		t.Fatalf("owner after closing one tab = %q, want c_p7b_owner: a second tab is still live", got)
	}

	// Closing the last tab does transfer.
	_ = tab2.conn.Close()
	waitForRole(t, other, room.Code, "c_p7b_other", protocol.RoleOwner)
}

// P8: the owner refreshing reclaims ownership when nobody took it over.
func TestP8OwnerRefreshRestoresOwner(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("P8 刷新")

	owner := s.dial(room.Code, "c_p8_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_p8_owner", 0, 0)
	other := s.dial(room.Code, "c_p8_other", "他人", room.ViewerKey, "")
	other.join(room.Code, "c_p8_other", 0, 0)

	// Refresh: the old socket dies and the same cid reconnects inside the grace
	// window, which is exactly the refresh case (§4.4).
	_ = owner.conn.Close()
	refreshed := s.dial(room.Code, "c_p8_owner", "房主", room.EditorKey, "")
	joined := refreshed.join(room.Code, "c_p8_owner", 0, 0)
	roleOf(t, joined, protocol.RoleOwner)
	ownerOf(t, joined, "c_p8_owner")

	// Give the grace reaper time to run: it must not transfer ownership now.
	time.Sleep(300 * time.Millisecond)
	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if got := rh.Stats().OwnerClientID; got != "c_p8_owner" {
		t.Fatalf("owner after refresh = %q, want c_p8_owner", got)
	}
}

// P9: a viewer sending room.grant is cut off with 4003 after an error frame.
func TestP9ViewerGrantForbiddenAndDisconnected(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("P9 越权")

	owner := s.dial(room.Code, "c_p9_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_p9_owner", 0, 0)
	v := s.dial(room.Code, "c_p9_viewer", "只读", room.ViewerKey, "")
	v.join(room.Code, "c_p9_viewer", 0, 0)

	v.send(protocol.TypeRoomGrant, room.Code, "c_p9_viewer", protocol.GrantPayload{
		TargetClientID: "c_p9_owner", Role: protocol.RoleViewer,
	})

	// The error frame must arrive before the close, and say forbidden + fatal.
	env, err := v.waitFor(protocol.TypeError, 5*time.Second)
	if err != nil {
		t.Fatalf("error frame: %v", err)
	}
	var ep protocol.ErrorPayload
	if err := json.Unmarshal(env.Payload, &ep); err != nil {
		t.Fatalf("error payload: %v", err)
	}
	if ep.Code != protocol.CodeForbidden || !ep.Fatal {
		t.Fatalf("error = %+v, want forbidden + fatal", ep)
	}
	if got := closeCodeOf(t, v, 5*time.Second); got != protocol.CloseForbidden {
		t.Fatalf("close code = %d, want %d", got, protocol.CloseForbidden)
	}
}

// P10: an unowned room hands ownership to whoever enters first.
func TestP10UnownedRoomFirstEntrantOwns(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("P10 无主")

	// Make the room unowned the same way production does: the owner leaves and
	// nobody else is online (§2.2 "无人在线：owner 置空").
	owner := s.dial(room.Code, "c_p10_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_p10_owner", 0, 0)
	_ = owner.conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rh, err := s.hub.Get(room.Code); err == nil && rh.Stats().OwnerClientID == "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if got := rh.Stats().OwnerClientID; got != "" {
		t.Fatalf("owner after the last member left = %q, want empty", got)
	}

	first := s.dial(room.Code, "c_p10_first", "第一个", room.ViewerKey, "")
	joined := first.join(room.Code, "c_p10_first", 0, 0)
	roleOf(t, joined, protocol.RoleOwner)
	ownerOf(t, joined, "c_p10_first")
}

// P11: a readonly token is always viewer, even with the owner's clientId.
func TestP11ReadonlyTokenNeverPromoted(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("P11 只读永不提升")

	owner := s.dial(room.Code, "c_p11_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_p11_owner", 0, 0)

	// Mint a readonly token through the real REST endpoint.
	roToken := issueReadonlyToken(t, s.baseURL, room.Code, room.EditorKey)

	// Same cid as the owner: rule 1 must still win (§4.2).
	ro := s.dial(room.Code, "c_p11_owner", "房主只读页", "", roToken)
	joined := ro.join(room.Code, "c_p11_owner", 0, 0)
	roleOf(t, joined, protocol.RoleViewer)
	ownerOf(t, joined, "c_p11_owner")

	ro.send(protocol.TypeOpAdd, room.Code, "c_p11_owner", protocol.OpAddPayload{
		OpID: opID("p11write"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("p11write")),
	})
	ack := expectAckStatus(t, ro, opID("p11write"), protocol.AckRejected)
	if ack.Code != protocol.CodeForbidden {
		t.Fatalf("readonly write code = %q, want forbidden", ack.Code)
	}

	// A grant must not lift a readonly connection either: the owner can only
	// target other cids, so use a separate readonly member and grant it.
	ro2CID := "c_p11_guest"
	ro2 := s.dial(room.Code, ro2CID, "只读者", "", roToken)
	ro2.join(room.Code, ro2CID, 0, 0)
	owner.send(protocol.TypeRoomGrant, room.Code, "c_p11_owner", protocol.GrantPayload{
		TargetClientID: ro2CID, Role: protocol.RoleEditor,
	})
	// The room records the grant, but the readonly socket stays viewer.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rh, err := s.hub.Get(room.Code)
		if err != nil {
			t.Fatalf("hub.Get: %v", err)
		}
		_ = rh
		ro2.send(protocol.TypeOpAdd, room.Code, ro2CID, protocol.OpAddPayload{
			OpID: opID("p11ro2"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("p11ro2")),
		})
		ack2, err := ro2.waitForAck(opID("p11ro2"), 2*time.Second)
		if err != nil {
			t.Fatalf("ack for readonly member: %v", err)
		}
		if ack2.Status != protocol.AckRejected || ack2.Code != protocol.CodeForbidden {
			t.Fatalf("readonly grant lifted the cap: %+v", ack2)
		}
		break
	}
}

// P12: a grant survives a refresh because it is persisted (grants table / memory
// map), not re-derived from the connection.
func TestP12GrantSurvivesRefresh(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("P12 授权持久")

	owner := s.dial(room.Code, "c_p12_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_p12_owner", 0, 0)
	b := s.dial(room.Code, "c_p12_guest", "成员B", room.ViewerKey, "")
	b.join(room.Code, "c_p12_guest", 0, 0)

	grantEditorOverWS(t, owner, room.Code, "c_p12_owner", "c_p12_guest")
	waitForRole(t, b, room.Code, "c_p12_guest", protocol.RoleEditor)

	_ = b.conn.Close()
	rejoined := s.dial(room.Code, "c_p12_guest", "成员B", room.ViewerKey, "")
	joined := rejoined.join(room.Code, "c_p12_guest", 0, 0)
	roleOf(t, joined, protocol.RoleEditor)

	rejoined.send(protocol.TypeOpAdd, room.Code, "c_p12_guest", protocol.OpAddPayload{
		OpID: opID("p12write"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("p12write")),
	})
	expectAckStatus(t, rejoined, opID("p12write"), protocol.AckApplied)
}

// Target offline must be a non-fatal error, and the connection must stay usable.
func TestGrantTargetOfflineKeepsConnection(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("目标离线")

	owner := s.dial(room.Code, "c_off_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_off_owner", 0, 0)

	owner.send(protocol.TypeRoomGrant, room.Code, "c_off_owner", protocol.GrantPayload{
		TargetClientID: "c_off_ghost", Role: protocol.RoleEditor,
	})
	env, err := owner.waitFor(protocol.TypeError, 5*time.Second)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var ep protocol.ErrorPayload
	if err := json.Unmarshal(env.Payload, &ep); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if ep.Code != protocol.CodeTargetOffline || ep.Fatal {
		t.Fatalf("error = %+v, want target_offline + non-fatal", ep)
	}
	owner.send(protocol.TypePing, room.Code, "c_off_owner", protocol.PingPayload{TS: 7})
	if _, err := owner.waitFor(protocol.TypePong, 5*time.Second); err != nil {
		t.Fatalf("connection died on target_offline: %v", err)
	}
}

// Granting owner or yourself is invalid_grant (§3.3, §8) and non-fatal.
func TestGrantInvalidCases(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("非法授权")

	owner := s.dial(room.Code, "c_inv_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_inv_owner", 0, 0)
	b := s.dial(room.Code, "c_inv_guest", "成员B", room.ViewerKey, "")
	b.join(room.Code, "c_inv_guest", 0, 0)

	cases := []struct {
		name string
		p    protocol.GrantPayload
	}{
		{"role owner", protocol.GrantPayload{TargetClientID: "c_inv_guest", Role: protocol.RoleOwner}},
		{"self", protocol.GrantPayload{TargetClientID: "c_inv_owner", Role: protocol.RoleEditor}},
		{"unknown role", protocol.GrantPayload{TargetClientID: "c_inv_guest", Role: "admin"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			owner.send(protocol.TypeRoomGrant, room.Code, "c_inv_owner", c.p)
			env, err := owner.waitFor(protocol.TypeError, 5*time.Second)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			var ep protocol.ErrorPayload
			if err := json.Unmarshal(env.Payload, &ep); err != nil {
				t.Fatalf("payload: %v", err)
			}
			if ep.Code != protocol.CodeInvalidGrant || ep.Fatal {
				t.Fatalf("error = %+v, want invalid_grant + non-fatal", ep)
			}
		})
	}
	// Non-fatal throughout: the owner can still act.
	owner.send(protocol.TypeRoomGrant, room.Code, "c_inv_owner", protocol.GrantPayload{
		TargetClientID: "c_inv_guest", Role: protocol.RoleEditor,
	})
	waitForRole(t, b, room.Code, "c_inv_guest", protocol.RoleEditor)
}

// A missing target must not silently succeed.
func TestTransferMissingTarget(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("转移离线")

	owner := s.dial(room.Code, "c_tr_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_tr_owner", 0, 0)

	owner.send(protocol.TypeRoomTransfer, room.Code, "c_tr_owner", protocol.TransferPayload{
		TargetClientID: "c_tr_ghost",
	})
	env, err := owner.waitFor(protocol.TypeError, 5*time.Second)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	var ep protocol.ErrorPayload
	if err := json.Unmarshal(env.Payload, &ep); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if ep.Code != protocol.CodeTargetOffline {
		t.Fatalf("code = %q, want target_offline", ep.Code)
	}
	rh, err := s.hub.Get(room.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if got := rh.Stats().OwnerClientID; got != "c_tr_owner" {
		t.Fatalf("owner = %q, want unchanged c_tr_owner", got)
	}
}

// Unsupported check: a member.role must be replayable through resync.events so a
// client that missed it can repair its view (docs/PERMISSIONS.md §3.1, §10).
func TestMemberRoleIsResyncable(t *testing.T) {
	s := newStackShortGrace(t)
	room := s.createRoom("角色补齐")

	owner := s.dial(room.Code, "c_rs_owner", "房主", room.EditorKey, "")
	owner.join(room.Code, "c_rs_owner", 0, 0)
	b := s.dial(room.Code, "c_rs_guest", "成员B", room.ViewerKey, "")
	b.join(room.Code, "c_rs_guest", 0, 0)

	// An op first, so the grant lands on seq 2 and the "lastSeq = 1" request below
	// exercises the delta path rather than a full snapshot.
	owner.send(protocol.TypeOpAdd, room.Code, "c_rs_owner", protocol.OpAddPayload{
		OpID: opID("rsop1"), Action: protocol.ActionCreate, Shape: rectShape(shapeID("rsop1")),
	})
	expectAckStatus(t, owner, opID("rsop1"), protocol.AckApplied)

	grantEditorOverWS(t, owner, room.Code, "c_rs_owner", "c_rs_guest")
	roleEnv, _ := waitForRole(t, b, room.Code, "c_rs_guest", protocol.RoleEditor)
	if roleEnv.Seq != 2 {
		t.Fatalf("grant seq = %d, want 2", roleEnv.Seq)
	}

	// A fresh client asks for everything after the last op before the grant: it
	// must receive the member.role as an event, in the same seq stream.
	fresh := s.dial(room.Code, "c_rs_late", "迟到", room.ViewerKey, "")
	fresh.send(protocol.TypeHello, room.Code, "c_rs_late", protocol.HelloPayload{
		LastSeq: roleEnv.Seq - 1, Epoch: 1,
	})
	if _, err := fresh.waitFor(protocol.TypeJoined, 5*time.Second); err != nil {
		t.Fatalf("joined: %v", err)
	}
	env, err := fresh.waitFor(protocol.TypeResyncEvents, 5*time.Second)
	if err != nil {
		t.Fatalf("resync.events: %v", err)
	}
	var events protocol.ResyncEventsPayload
	if err := json.Unmarshal(env.Payload, &events); err != nil {
		t.Fatalf("events payload: %v", err)
	}
	var found bool
	for _, ev := range events.Events {
		if ev.Type != protocol.TypeMemberRole {
			continue
		}
		if ev.Seq == roleEnv.Seq && ev.ClientID == "c_rs_guest" && ev.Role == protocol.RoleEditor {
			found = true
		}
	}
	if !found {
		raw, _ := json.Marshal(events.Events)
		t.Fatalf("the grant was not replayable as a member.role event: %s", raw)
	}
}

// issueReadonlyToken mints a token through the real REST endpoint.
func issueReadonlyToken(t *testing.T, baseURL, code, editorKey string) string {
	t.Helper()
	res, err := httpPostJSON(baseURL+"/api/rooms/"+code+"/readonly-token",
		`{"editorKey":"`+editorKey+`"}`)
	if err != nil {
		t.Fatalf("issue readonly token: %v", err)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("issue JSON: %v", err)
	}
	if strings.TrimSpace(out.Token) == "" {
		t.Fatalf("empty readonly token in %s", res)
	}
	return out.Token
}
