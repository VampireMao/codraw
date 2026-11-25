package room

import (
	"github.com/codraw/codraw/backend/internal/protocol"
)

// Permission model (docs/PERMISSIONS.md).
//
// # Ownership
//
// The room owns three pieces of state, all of them written only by the room
// goroutine (the zero-lock model from room.go):
//
//	ownerClientID  the current owner, "" when the room is unowned
//	grants         cid -> editor|viewer, the durable authorisation table
//	joinOrder      cids in the order they first joined, used for inheritance
//	lastRoles      cid -> role, used to restore a role after a reconnect
//
// The owner role is *not* stored in grants: ownership changes only through
// room.transfer or inheritance (§2.2, §2.3), never through room.grant.
//
// # Why the role is derived, not cached
//
// op.add authorisation must observe a revocation immediately (§5). The room
// therefore derives the role from ownerClientID/grants on every op instead of
// trusting a role captured at handshake time. The Member's published User.Role
// is only a cache for presence/UI; it is refreshed from the same source.

// roleOfCid returns the room-level role of a client id, ignoring the read-only
// credential. It is the single source of truth for authorisation.
//
// lastRoles participates here but NOT in roleForJoin: it exists so that the
// current session of a demoted owner keeps editor (a handover should not lock
// them out mid-session), while a *re-join* falls through to viewer because the
// role was never persisted as a grant. See transferOwnership for the reasoning.
func (r *room) roleOfCid(cid string) string {
	if cid == "" {
		return protocol.RoleViewer
	}
	if r.ownerClientID != "" && cid == r.ownerClientID {
		return protocol.RoleOwner
	}
	if granted, ok := r.grants[cid]; ok && granted != "" {
		return granted
	}
	if last, ok := r.lastRoles[cid]; ok && last != "" {
		return last
	}
	return protocol.RoleViewer
}

// effectiveRole caps a room-level role with the connection's credential: a
// readonly token is always a viewer, even when its clientId is the owner's
// (docs/PERMISSIONS.md §4.2 rule 1, §9 P11).
func effectiveRole(cid, role string, readonly bool) string {
	if readonly || cid == "" {
		return protocol.RoleViewer
	}
	return role
}

// cidHasLiveMember reports whether any connection with the client id is online.
func (r *room) cidHasLiveMember(cid string) bool {
	for m := range r.members {
		if m.User().ClientID == cid {
			return true
		}
	}
	return false
}

// hasWritableMember reports whether the client id has at least one connection
// that is not pinned to viewer by a readonly token. Grant/transfer must use this,
// not cidHasLiveMember: a readonly connection is a real member but can never act
// as editor or owner (PERMISSIONS §4.2 rule 1).
func (r *room) hasWritableMember(cid string) bool {
	for _, ms := range r.members {
		if ms.user.ClientID == cid && !ms.readonly {
			return true
		}
	}
	return false
}

// roleForJoin implements the handshake order from docs/PERMISSIONS.md §4.2 and
// §4.4 literally:
//
//  1. rt valid                  -> viewer (never promoted; the safety floor)
//  2. cid == ownerClientID      -> owner
//  3. grants[room][cid] exists  -> that role
//  4. lastRoles[cid]            -> that role (reconnect restore; skips 5 and 6)
//  5. room has no owner         -> owner (first entrant takes over)
//  6. otherwise                 -> viewer
//
// Ownership restore is deliberately narrower than step 4 suggests: an ex-owner
// whose room was taken over by someone else is restored as editor, never as a
// second owner. Two live owners would be an invariant violation.
//
// The deadlock the literal order could create ("unowned room + a stale editor
// grant for the next entrant" would leave nobody able to grant) is prevented at
// the source: becoming unowned clears both grants and role memory, because both
// are authorisations an owner granted. See clearAuthorisationsLocked.
func (r *room) roleForJoin(cid string, readonly bool) string {
	if readonly || cid == "" {
		return protocol.RoleViewer
	}
	if r.ownerClientID != "" && cid == r.ownerClientID {
		return protocol.RoleOwner
	}
	// Only explicit grants restore authority. Being an ex-owner is NOT a grant:
	// once ownership has moved on, the previous owner re-enters as a plain viewer
	// and must be authorised like anyone else. Restoring editor here let a user
	// who had merely left the room walk back in with write access, which
	// contradicts "viewer by default" (docs/PERMISSIONS.md §1).
	if granted, ok := r.grants[cid]; ok && granted != "" {
		return granted
	}
	if r.ownerClientID == "" {
		// Unowned room: the first entrant takes over (§2.1 / §2.2).
		return protocol.RoleOwner
	}
	return protocol.RoleViewer
}

// clearAuthorisationsLocked forgets every grant and every remembered role.
//
// It runs when the room becomes unowned: an unowned room has no one who could
// have granted anything, so a surviving row would be a stale authorisation. It is
// also what keeps the literal §4.2 order deadlock-free — the next entrant must
// reach rule 5 ("no owner -> owner") rather than rule 3 with an orphan grant.
func (r *room) clearAuthorisationsLocked() {
	for cid := range r.grants {
		delete(r.grants, cid)
		r.persistGrantDelete(cid)
	}
	for _, cid := range r.lastRoleOrder {
		delete(r.lastRoles, cid)
	}
	r.lastRoleOrder = nil
}

// recordRole remembers a role for reconnect restore (§4.4). The map is bounded:
// a hostile client could churn client ids, and an unbounded map would be a slow
// leak for the lifetime of the room.
func (r *room) recordRole(cid, role string) {
	if cid == "" {
		return
	}
	if _, exists := r.lastRoles[cid]; !exists {
		r.lastRoleOrder = append(r.lastRoleOrder, cid)
		if len(r.lastRoleOrder) > lastRolesMax {
			oldest := r.lastRoleOrder[0]
			r.lastRoleOrder = r.lastRoleOrder[1:]
			delete(r.lastRoles, oldest)
		}
	}
	r.lastRoles[cid] = role
}

// lastRolesMax bounds the reconnect-restore map. A room is capped at 200 live
// members, so this holds several generations of reconnects.
const lastRolesMax = 512

// setOwnerLocked makes cid the owner and demotes the previous owner to editor.
//
// It updates the in-memory maps, persists the change best-effort and pushes the
// new role to every live connection of both client ids. It does not emit the
// member.role frames: the caller decides the broadcast order.
//
// When cid is "" the room becomes unowned and every authorisation is forgotten
// (§2.2): nobody is left who could have granted them, and a surviving grant
// would shadow the "first entrant takes over" rule for the very client that
// comes back.
func (r *room) setOwnerLocked(cid string) {
	prev := r.ownerClientID
	if prev == cid {
		return
	}
	r.ownerClientID = cid
	if cid == "" {
		r.clearAuthorisationsLocked()
		r.persistOwner()
		r.refreshRoles()
		return
	}
	// Ownership is exclusive: a grant row would shadow nothing while the cid is
	// the owner, but leaving it behind would resurrect an old role after a
	// transfer away. Clearing it keeps roleOfCid unambiguous.
	delete(r.grants, cid)
	r.recordRole(cid, protocol.RoleOwner)

	if prev != "" && prev != cid {
		// The previous owner is demoted for the *current session only*: their
		// live connection keeps editor so the handover is not a jarring lock-out,
		// but nothing is persisted and no grant row is written.
		//
		// Why: if we wrote grants[prev] = editor, that row would outlive the
		// session, so the ex-owner could leave and re-enter with write access —
		// contradicting "viewer by default" (docs/PERMISSIONS.md §1). Role
		// memory alone gives them editor while connected, and a re-join falls
		// through to viewer like any other visitor.
		r.recordRole(prev, protocol.RoleEditor)
	}
	r.persistOwner()
	r.refreshRoles()
}

// setGrantLocked applies a room.grant. viewer is how a grant is revoked (§3.2).
func (r *room) setGrantLocked(cid, role, grantedBy string) {
	if cid == "" || role == protocol.RoleOwner {
		return
	}
	r.grants[cid] = role
	r.recordRole(cid, role)
	r.persistGrant(cid, role, grantedBy)
	r.refreshRoles()
}

// refreshRoles pushes the authoritative role to every live connection.
//
// A readonly connection is pinned to viewer: the credential caps the role for
// that socket only, so a cid with one normal tab and one readonly tab keeps the
// granted role on the normal tab and viewer on the readonly one.
func (r *room) refreshRoles() {
	for m, ms := range r.members {
		role := effectiveRole(ms.user.ClientID, r.roleOfCid(ms.user.ClientID), ms.readonly)
		if role != ms.user.Role {
			m.SetRole(role)
			ms.user = m.User()
		}
	}
	// Pending leaves are not members any more, but they are still in the
	// presence list until the grace window expires; keep their cached role in
	// step so a leave broadcast does not carry a stale role.
	for m, lt := range r.pendingLeave {
		role := effectiveRole(lt.user.ClientID, r.roleOfCid(lt.user.ClientID), lt.readonly)
		if role != lt.user.Role {
			lt.user.Role = role
			r.pendingLeave[m] = lt
		}
	}
}

// persistOwner saves the owner. A failure is logged, never fatal: the session
// stays correct in memory and the next owner change retries.
func (r *room) persistOwner() {
	if r.opts.Persist == nil {
		return
	}
	if err := r.opts.Persist.SaveOwner(r.ctx, r.code, r.ownerClientID); err != nil {
		r.log.Warn("persist owner failed", "owner", r.ownerClientID, "err", err)
	}
}

// persistGrant saves one authorisation row.
func (r *room) persistGrant(cid, role, grantedBy string) {
	if r.opts.Persist == nil || cid == "" {
		return
	}
	if err := r.opts.Persist.SaveGrant(r.ctx, r.code, cid, role, grantedBy, r.opts.Now()); err != nil {
		r.log.Warn("persist grant failed", "client", cid, "role", role, "err", err)
	}
}

// persistGrantDelete removes an authorisation row. It is used when the room
// becomes unowned: a leftover editor grant would otherwise shadow §2.2's "next
// entrant takes over" rule for the client that just left.
func (r *room) persistGrantDelete(cid string) {
	if r.opts.Persist == nil || cid == "" {
		return
	}
	if err := r.opts.Persist.DeleteGrant(r.ctx, r.code, cid); err != nil {
		r.log.Warn("delete grant failed", "client", cid, "err", err)
	}
}

// inheritOwnership is the presence.leave path from docs/PERMISSIONS.md §2.2:
//
//	owner left
//	  -> if the same clientId still has another connection, do nothing
//	  -> otherwise the first still-online client in joinOrder becomes owner
//	  -> if nobody is online the room becomes unowned
func (r *room) inheritOwnership(leavingCid string) {
	if r.ownerClientID == "" || r.ownerClientID != leavingCid {
		return
	}
	if r.cidHasLiveMember(leavingCid) {
		return // another tab of the same member
	}
	next := r.firstOnlineInJoinOrder()
	if next == "" {
		r.setOwnerLocked("")
		return
	}
	prev := r.ownerClientID
	r.setOwnerLocked(next)
	// Broadcast both sides of the change (§9 P7: "广播 member.role（新 owner 与原
	// owner）"). The old owner is offline; the frame still updates every client's
	// member list so their "房主" badge moves immediately.
	r.emitRoleChange(protocol.MemberRolePayload{ClientID: prev, Role: protocol.RoleEditor, GrantedBy: next})
	r.emitRoleChange(protocol.MemberRolePayload{ClientID: next, Role: protocol.RoleOwner, GrantedBy: prev})
}

// firstOnlineInJoinOrder returns the earliest-joined client id that still has a
// live connection, or "".
func (r *room) firstOnlineInJoinOrder() string {
	for _, cid := range r.joinOrder {
		if r.cidHasLiveMember(cid) {
			return cid
		}
	}
	// Defensive: a member present but missing from joinOrder (e.g. a restored
	// record) must still be eligible rather than leaving the room stuck ownerless.
	for m := range r.members {
		return m.User().ClientID
	}
	return ""
}

// noteJoinOrder records the order in which client ids first joined. Only the
// first live connection of a client id appends; extra tabs share the slot.
func (r *room) noteJoinOrder(cid string) {
	if cid == "" || r.cidHasLiveMember(cid) {
		return
	}
	for _, existing := range r.joinOrder {
		if existing == cid {
			return
		}
	}
	r.joinOrder = append(r.joinOrder, cid)
}

// pruneJoinOrder forgets a client id once its last connection is gone, so the
// slice stays bounded by the number of live members and a rejoin counts as a new
// entrance (which is what "按入场顺序" means for the current presence list).
func (r *room) pruneJoinOrder(cid string) {
	if cid == "" || r.cidHasLiveMember(cid) {
		return
	}
	for i, existing := range r.joinOrder {
		if existing == cid {
			r.joinOrder = append(r.joinOrder[:i], r.joinOrder[i+1:]...)
			return
		}
	}
}

// emitRoleChange appends a member.role to the durable room stream and fans it
// out. It is a persistent message (§3.1): it consumes a seq and lands in the
// in-memory resync ring so a client that missed it can be repaired.
//
// Pending ops are flushed first so the role event cannot jump ahead of an op
// that arrived earlier and is still waiting for its batch.
func (r *room) emitRoleChange(p protocol.MemberRolePayload) {
	if p.ClientID == "" {
		return
	}
	r.flushPending()

	seq := r.seq + 1
	if r.opts.SeqSource != nil {
		hi, err := r.opts.SeqSource.NextSeqRange(r.ctx, r.code, 1)
		if err != nil {
			r.log.Warn("allocate seq for role change failed", "err", err)
			return
		}
		seq = hi
	}
	if seq <= r.seq {
		seq = r.seq + 1
	}
	r.seq = seq
	now := r.opts.Now().UnixMilli()
	r.log_.put(logEntry{
		Seq:       seq,
		Epoch:     r.epoch,
		Type:      protocol.TypeMemberRole,
		ClientID:  p.ClientID,
		Role:      p.Role,
		GrantedBy: p.GrantedBy,
		TS:        now,
	})
	r.dropPendingBelow(seq)

	frame, err := protocol.Marshal(protocol.TypeMemberRole, r.code, p.ClientID, seq, now, p)
	if err != nil {
		r.log.Error("encode member.role", "err", err)
		return
	}
	r.fanout(frame, true)
	// Other instances must see the grant too, or their clients keep the old
	// permission (multi-instance is a documented deployment).
	if r.opts.Broadcast != nil {
		if err := r.opts.Broadcast.PublishOpBatch(r.ctx, r.code, []int64{seq}, [][]byte{frame}); err != nil {
			r.log.Warn("broker publish for member.role failed", "err", err)
		}
	}
	r.publishStats()
}
