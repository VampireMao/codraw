package room

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/codraw/codraw/backend/internal/protocol"
)

// These tests pin the room-level permission state machine from
// docs/PERMISSIONS.md §2/§4. The transport-level behaviour (frames, close codes)
// is covered by internal/integration/permissions_test.go; here the point is the
// invariant "who is the owner and who may write", without a socket.

// joinAs joins a member and announces it, returning the decided role.
func joinAs(t *testing.T, h *Handle, m *fakeMember, readonly bool) string {
	t.Helper()
	res, err := h.JoinConn(context.Background(), m, readonly)
	if err != nil {
		t.Fatalf("join %s: %v", m.user.ClientID, err)
	}
	h.NotifyJoined(m)
	return res.Role
}

// waitForOwner polls until the room reports the wanted owner.
func waitForOwner(t *testing.T, h *Handle, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.Stats().OwnerClientID == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("owner = %q, want %q", h.Stats().OwnerClientID, want)
}

// waitForRole polls until the member's published role reaches want.
func waitForRole(t *testing.T, m *fakeMember, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.User().Role == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("role = %q, want %q", m.User().Role, want)
}

func newEmptyRoom(t *testing.T) *Handle {
	t.Helper()
	h := New(testRoomOptions("K7M2QX"))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Close(ctx)
	})
	return h
}

// TestFirstEntrantOwnsRoomRegardlessOfRoleHint proves §2.1: the *first* entrant
// owns the room, and the role the transport resolved beforehand (editor from a
// key, viewer from nothing) does not decide it.
func TestFirstEntrantOwnsRoomRegardlessOfRoleHint(t *testing.T) {
	h := newEmptyRoom(t)

	// A member whose cached role says "editor" still becomes owner: the handshake
	// hint is not authority.
	first := newFakeMember("c_first001", "第一", protocol.RoleEditor, true)
	if got := joinAs(t, h, first, false); got != protocol.RoleOwner {
		t.Fatalf("first entrant role = %q, want owner", got)
	}
	waitForOwner(t, h, "c_first001")
	if first.User().Role != protocol.RoleOwner {
		t.Fatalf("cached role = %q, want owner", first.User().Role)
	}

	// A member presenting an editor key second stays viewer: the key is only an
	// invitation.
	second := newFakeMember("c_second01", "第二", protocol.RoleEditor, true)
	if got := joinAs(t, h, second, false); got != protocol.RoleViewer {
		t.Fatalf("second entrant role = %q, want viewer", got)
	}
}

// TestReadonlyNeverOwnsEvenWithOwnerCID is §4.2 rule 1: the readonly credential
// wins over every other rule, including the owner's own clientId.
func TestReadonlyNeverOwnsEvenWithOwnerCID(t *testing.T) {
	h := newEmptyRoom(t)

	if got := joinAs(t, h, newFakeMember("c_owner001", "房主", protocol.RoleViewer, true), false); got != protocol.RoleOwner {
		t.Fatalf("owner role = %q", got)
	}
	// Same cid, readonly credential.
	ro := newFakeMember("c_owner001", "房主只读", protocol.RoleViewer, true)
	if got := joinAs(t, h, ro, true); got != protocol.RoleViewer {
		t.Fatalf("readonly role = %q, want viewer", got)
	}
	waitForOwner(t, h, "c_owner001")

	// And it cannot write.
	opf, env := rectPayload(t, "sh_ro000001")
	h.Op(ro, env, opf, nil)
	time.Sleep(40 * time.Millisecond)
	if got := h.Stats().Seq; got != 0 {
		t.Fatalf("readonly member consumed seq %d", got)
	}
	if got := h.Stats().ShapeCount; got != 0 {
		t.Fatalf("readonly member created %d shapes", got)
	}
}

// TestInheritanceFollowsJoinOrder covers §2.2 with three members.
func TestInheritanceFollowsJoinOrder(t *testing.T) {
	h := newEmptyRoom(t)
	owner := newFakeMember("c_join0001", "房主", protocol.RoleViewer, true)
	b := newFakeMember("c_join0002", "第二", protocol.RoleViewer, true)
	c := newFakeMember("c_join0003", "第三", protocol.RoleViewer, true)
	joinAs(t, h, owner, false)
	joinAs(t, h, b, false)
	joinAs(t, h, c, false)

	// Owner leaves: the second entrant must inherit, not the third.
	h.Leave(owner)
	waitForOwner(t, h, "c_join0002")
	if got := b.User().Role; got != protocol.RoleOwner {
		t.Fatalf("second role = %q, want owner", got)
	}
	if got := c.User().Role; got != protocol.RoleViewer {
		t.Fatalf("third role = %q, want viewer", got)
	}

	// The new owner leaves too: the third inherits.
	h.Leave(b)
	waitForOwner(t, h, "c_join0003")

	// The last member leaves: the room becomes unowned.
	h.Leave(c)
	waitForOwner(t, h, "")
}

// TestInheritanceSkipsStillConnectedTab is §2.4: closing one tab of a multi-tab
// member must not hand the room to someone else.
func TestInheritanceSkipsStillConnectedTab(t *testing.T) {
	h := newEmptyRoom(t)
	tab1 := newFakeMember("c_tab00001", "房主", protocol.RoleViewer, true)
	tab2 := newFakeMember("c_tab00001", "房主", protocol.RoleViewer, true)
	other := newFakeMember("c_tab00002", "他人", protocol.RoleViewer, true)
	joinAs(t, h, tab1, false)
	joinAs(t, h, tab2, false)
	joinAs(t, h, other, false)

	h.Leave(tab1)
	time.Sleep(200 * time.Millisecond) // longer than the 50ms test grace
	waitForOwner(t, h, "c_tab00001")

	// Closing the last tab does hand over.
	h.Leave(tab2)
	waitForOwner(t, h, "c_tab00002")
}

// TestRefreshInsideGraceKeepsOwnership covers the P8 mechanism at the state level:
// the leave timer is cancelled by the reconnect, so no handover happens.
func TestRefreshInsideGraceKeepsOwnership(t *testing.T) {
	h := newEmptyRoom(t)
	old := newFakeMember("c_ref00001", "房主", protocol.RoleViewer, true)
	other := newFakeMember("c_ref00002", "他人", protocol.RoleViewer, true)
	joinAs(t, h, old, false)
	joinAs(t, h, other, false)

	// Socket dies; the reconnect (same cid) arrives inside the grace window.
	h.Leave(old)
	refreshed := newFakeMember("c_ref00001", "房主", protocol.RoleViewer, true)
	if got := joinAs(t, h, refreshed, false); got != protocol.RoleOwner {
		t.Fatalf("refreshed role = %q, want owner", got)
	}
	time.Sleep(200 * time.Millisecond)
	waitForOwner(t, h, "c_ref00001")
	if got := other.User().Role; got != protocol.RoleViewer {
		t.Fatalf("bystander role = %q, want viewer: ownership moved on a refresh", got)
	}
}

// TestTransferAndGrantStateMachine pins the in-room effects of both messages.
func TestTransferAndGrantStateMachine(t *testing.T) {
	h := newEmptyRoom(t)
	owner := newFakeMember("c_sm000001", "房主", protocol.RoleViewer, true)
	b := newFakeMember("c_sm000002", "B", protocol.RoleViewer, true)
	c := newFakeMember("c_sm000003", "C", protocol.RoleViewer, true)
	joinAs(t, h, owner, false)
	joinAs(t, h, b, false)
	joinAs(t, h, c, false)

	h.Grant(owner, &protocol.GrantPayload{TargetClientID: "c_sm000002", Role: protocol.RoleEditor})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && b.User().Role != protocol.RoleEditor {
		time.Sleep(2 * time.Millisecond)
	}
	if got := b.User().Role; got != protocol.RoleEditor {
		t.Fatalf("B role = %q, want editor", got)
	}

	// Transfer: B becomes owner, the caller becomes editor.
	h.Transfer(owner, &protocol.TransferPayload{TargetClientID: "c_sm000002"})
	waitForOwner(t, h, "c_sm000002")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && owner.User().Role != protocol.RoleEditor {
		time.Sleep(2 * time.Millisecond)
	}
	if got := owner.User().Role; got != protocol.RoleEditor {
		t.Fatalf("former owner role = %q, want editor", got)
	}
	if got := c.User().Role; got != protocol.RoleViewer {
		t.Fatalf("C role = %q, want viewer", got)
	}

	// The demoted owner may no longer transfer.
	// (Authorization is exercised at the transport level; here we assert the
	// state did not move.)
	h.Transfer(owner, &protocol.TransferPayload{TargetClientID: "c_sm000003"})
	time.Sleep(60 * time.Millisecond)
	waitForOwner(t, h, "c_sm000002")
}

// TestGrantIsReEvaluatedPerOp is §5: a revoke must reject the very next op, not
// only on reconnect.
func TestGrantIsReEvaluatedPerOp(t *testing.T) {
	h := newEmptyRoom(t)
	owner := newFakeMember("c_re000001", "房主", protocol.RoleViewer, true)
	b := newFakeMember("c_re000002", "B", protocol.RoleViewer, true)
	joinAs(t, h, owner, false)
	joinAs(t, h, b, false)
	h.Grant(owner, &protocol.GrantPayload{TargetClientID: "c_re000002", Role: protocol.RoleEditor})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && b.User().Role != protocol.RoleEditor {
		time.Sleep(2 * time.Millisecond)
	}

	opf, env := rectPayload(t, "sh_re000001")
	h.Op(b, env, opf, nil)
	waitForSeq(t, h, 1)

	// Revoke, then write again: the op must be rejected. The revocation itself is
	// a persistent message, so the sequence advances; the point is that the op
	// after it does not.
	seqAfterFirstOp := h.Stats().Seq
	h.Grant(owner, &protocol.GrantPayload{TargetClientID: "c_re000002", Role: protocol.RoleViewer})
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && b.User().Role != protocol.RoleViewer {
		time.Sleep(2 * time.Millisecond)
	}
	seqAfterRevoke := h.Stats().Seq
	if seqAfterRevoke <= seqAfterFirstOp {
		t.Fatalf("the revocation did not consume a seq (%d → %d); it must be resyncable", seqAfterFirstOp, seqAfterRevoke)
	}

	opf2, env2 := rectPayload(t, "sh_re000002")
	h.Op(b, env2, opf2, nil)
	time.Sleep(60 * time.Millisecond)
	if got := h.Stats().Seq; got != seqAfterRevoke {
		t.Fatalf("revoked member's op consumed seq %d, want the log to stay at %d", got, seqAfterRevoke)
	}
	if got := h.Stats().ShapeCount; got != 1 {
		t.Fatalf("shape count = %d, want 1", got)
	}
	// And the socket is still there: the rejection is an ack, not a kick.
	if got := b.kickCount(); got != 0 {
		t.Fatalf("revoked member was kicked %d times", got)
	}
}

// TestUnownedRoomWithStaleEditorGrantStillHandsOverOwnership guards a deadlock
// the naive reading of §4.2 would create: if a room becomes unowned while an
// editor grant for the next entrant still exists, that entrant must become owner,
// not editor, or the room stays ownerless with nobody able to grant.
func TestUnownedRoomWithStaleEditorGrantStillHandsOverOwnership(t *testing.T) {
	rec := &recordingPersister{}
	opts := testRoomOptions("K7M2QX")
	opts.Persist = rec
	// Restore a room that has a stale editor grant but no owner, as a restart
	// between "owner left" and "owner row written" could leave it.
	opts.Grants = map[string]string{"c_stale001": protocol.RoleEditor}
	h := New(opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Close(ctx)
	})

	m := newFakeMember("c_stale001", "遗留", protocol.RoleViewer, true)
	if got := joinAs(t, h, m, false); got != protocol.RoleOwner {
		t.Fatalf("entrant with a stale editor grant in an unowned room = %q, want owner", got)
	}
	waitForOwner(t, h, "c_stale001")
}

// TestNewEntrantTakesUnownedRoom is §2.2's last branch: after the owner left and
// nobody was online, the next entrant owns.
func TestNewEntrantTakesUnownedRoom(t *testing.T) {
	h := newEmptyRoom(t)
	owner := newFakeMember("c_un000001", "房主", protocol.RoleViewer, true)
	joinAs(t, h, owner, false)
	h.Leave(owner)
	waitForOwner(t, h, "")

	next := newFakeMember("c_un000002", "下一个", protocol.RoleViewer, true)
	if got := joinAs(t, h, next, false); got != protocol.RoleOwner {
		t.Fatalf("entrant into an unowned room = %q, want owner", got)
	}
}

// TestGrantDoesNotSurviveTransferBack asserts the demoted owner does not keep a
// stale grant that would make them an editor twice over or, worse, shadow the
// owner role.
func TestGrantDoesNotSurviveTransferBack(t *testing.T) {
	h := newEmptyRoom(t)
	owner := newFakeMember("c_gb000001", "A", protocol.RoleViewer, true)
	b := newFakeMember("c_gb000002", "B", protocol.RoleViewer, true)
	joinAs(t, h, owner, false)
	joinAs(t, h, b, false)

	// A → owner, B → owner, B → owner(A).
	h.Transfer(owner, &protocol.TransferPayload{TargetClientID: "c_gb000002"})
	waitForOwner(t, h, "c_gb000002")
	h.Transfer(b, &protocol.TransferPayload{TargetClientID: "c_gb000001"})
	waitForOwner(t, h, "c_gb000001")

	if got := owner.User().Role; got != protocol.RoleOwner {
		t.Fatalf("A role = %q, want owner", got)
	}
	if got := b.User().Role; got != protocol.RoleEditor {
		t.Fatalf("B role = %q, want editor", got)
	}
}

// TestOwnerAndGrantsRestoreFromRecord proves a room rebuilt from a persisted
// record keeps its owner and its grants (docs/PERMISSIONS.md §7 / §9 P12 after a
// process restart).
func TestOwnerAndGrantsRestoreFromRecord(t *testing.T) {
	rec := &Record{
		Code:          "K7M2QX",
		Title:         "重启恢复",
		Epoch:         1,
		OwnerClientID: "c_owner001",
		Grants:        map[string]string{"c_editor01": protocol.RoleEditor},
	}
	h, err := NewWithRecord(testRoomOptions("K7M2QX"), rec)
	if err != nil {
		t.Fatalf("NewWithRecord: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Close(ctx)
	})
	if got := h.Stats().OwnerClientID; got != "c_owner001" {
		t.Fatalf("restored owner = %q, want c_owner001", got)
	}

	// The restored owner rejoins as owner, the restored editor as editor, and a
	// stranger as viewer.
	owner := newFakeMember("c_owner001", "房主", protocol.RoleViewer, true)
	if got := joinAs(t, h, owner, false); got != protocol.RoleOwner {
		t.Fatalf("restored owner joined as %q", got)
	}
	editor := newFakeMember("c_editor01", "编辑", protocol.RoleViewer, true)
	if got := joinAs(t, h, editor, false); got != protocol.RoleEditor {
		t.Fatalf("restored editor joined as %q", got)
	}
	stranger := newFakeMember("c_other001", "陌生人", protocol.RoleViewer, true)
	if got := joinAs(t, h, stranger, false); got != protocol.RoleViewer {
		t.Fatalf("stranger joined as %q", got)
	}
}

// TestResyncIncludesMemberRole proves the role event is replayable through the
// room's retained log, which is what makes a missed grant recoverable.
func TestResyncIncludesMemberRole(t *testing.T) {
	h := newEmptyRoom(t)
	owner := newFakeMember("c_rr000001", "房主", protocol.RoleViewer, true)
	b := newFakeMember("c_rr000002", "B", protocol.RoleViewer, true)
	late := newFakeMember("c_rr000003", "迟到", protocol.RoleViewer, true)
	joinAs(t, h, owner, false)
	joinAs(t, h, b, false)

	opf, env := rectPayload(t, "sh_rr000001")
	h.Op(owner, env, opf, nil)
	waitForSeq(t, h, 1)
	h.Grant(owner, &protocol.GrantPayload{TargetClientID: "c_rr000002", Role: protocol.RoleEditor})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && h.Stats().Seq < 2 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := h.Stats().Seq; got != 2 {
		t.Fatalf("seq after grant = %d, want 2", got)
	}

	// A late client asks for everything from seq 2.
	joinAs(t, h, late, false)
	h.Resync(late, &protocol.ResyncRequestPayload{FromSeq: 2, Epoch: 1})
	deadline = time.Now().Add(2 * time.Second)
	var events protocol.ResyncEventsPayload
	for time.Now().Before(deadline) {
		for _, frame := range late.frames() {
			var e protocol.Envelope
			if err := json.Unmarshal(frame, &e); err != nil || e.Type != protocol.TypeResyncEvents {
				continue
			}
			if err := json.Unmarshal(e.Payload, &events); err == nil {
				goto decoded
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no resync.events frame")
decoded:
	if len(events.Events) != 1 {
		t.Fatalf("events = %d, want the single grant", len(events.Events))
	}
	ev := events.Events[0]
	if ev.Type != protocol.TypeMemberRole || ev.ClientID != "c_rr000002" || ev.Role != protocol.RoleEditor {
		t.Fatalf("event = %+v, want member.role editor for B", ev)
	}
}

// TestOwnerIsPersisted checks that owner changes reach the Persister, since that
// is what makes ownership survive a process restart (docs/PERMISSIONS.md §7).
func TestOwnerIsPersisted(t *testing.T) {
	rec := &recordingPersister{}
	opts := testRoomOptions("K7M2QX")
	opts.Persist = rec
	h := New(opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Close(ctx)
	})

	owner := newFakeMember("c_ps000001", "房主", protocol.RoleViewer, true)
	b := newFakeMember("c_ps000002", "B", protocol.RoleViewer, true)
	joinAs(t, h, owner, false)
	joinAs(t, h, b, false)
	h.Grant(owner, &protocol.GrantPayload{TargetClientID: "c_ps000002", Role: protocol.RoleEditor})
	h.Transfer(owner, &protocol.TransferPayload{TargetClientID: "c_ps000002"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && rec.owner() != "c_ps000002" {
		time.Sleep(2 * time.Millisecond)
	}
	if got := rec.owner(); got != "c_ps000002" {
		t.Fatalf("persisted owner = %q, want c_ps000002", got)
	}
	// The demoted owner keeps editor for the current session but that role must
	// NOT be persisted: re-joining should be viewer by default
	// (docs/PERMISSIONS.md §1). Persisting it here would let the ex-owner leave
	// and walk back in with write access.
	if got := rec.grant("c_ps000001"); got != "" {
		t.Fatalf("persisted grant for the demoted owner = %q, want none", got)
	}
	// In-session the demoted owner is still editor, so the handover is smooth.
	waitForRole(t, owner, protocol.RoleEditor)
}

// recordingPersister captures the permission writes without doing I/O.
//
// All fields are guarded by mu: the room goroutine writes them, the test
// goroutine reads them. Without the mutex this test fails under -race, which is
// exactly what happened — the persister is called from the room loop while the
// assertions run on the test goroutine.
type recordingPersister struct {
	nopPersister
	mu       sync.Mutex
	ownerVal string
	grants   map[string]string
}

func (p *recordingPersister) SaveOwner(_ context.Context, _, owner string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ownerVal = owner
	return nil
}

func (p *recordingPersister) SaveGrant(_ context.Context, _, cid, role, _ string, _ time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.grants == nil {
		p.grants = make(map[string]string)
	}
	p.grants[cid] = role
	return nil
}

func (p *recordingPersister) DeleteGrant(_ context.Context, _, cid string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.grants, cid)
	return nil
}

func (p *recordingPersister) owner() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ownerVal
}

func (p *recordingPersister) grant(cid string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.grants[cid]
}
