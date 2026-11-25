package room

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/codraw/codraw/backend/internal/protocol"
)

// command kinds routed into the room goroutine.
type cmdKind int

const (
	cmdJoin cmdKind = iota
	cmdJoined
	cmdLeave
	cmdHello
	cmdOp
	cmdCursor
	cmdResync
	cmdPing
	cmdBye
	cmdClose
	cmdRemote
	cmdTick
	cmdShapes
	cmdGrant
	cmdTransfer
)

type command struct {
	kind     cmdKind
	member   Member
	env      *protocol.Envelope
	hello    *protocol.HelloPayload
	op       *protocol.OpAddPayload
	cursor   *protocol.CursorPayload
	resync   *protocol.ResyncRequestPayload
	ping     *protocol.PingPayload
	grant    *protocol.GrantPayload
	transfer *protocol.TransferPayload
	readonly bool
	raw      []byte
	// reply carries the admission result for cmdJoin.
	reply chan joinReply
	// remoteSeq / frame carry a broker-delivered op; remoteReply reports whether
	// the room accepted it.
	remoteSeq   int64
	frame       []byte
	remoteReply chan bool
	// shapesReply carries a copy of the live shape list for the REST snapshot.
	shapesReply chan []protocol.Shape
}

// joinReply is the room's answer to cmdJoin.
type joinReply struct {
	role   string
	owner  string
	online int
	err    error
}

// tickInterval drives the periodic housekeeping (cursor flush, leave grace
// expiry, snapshot due-check).
const tickInterval = 25 * time.Millisecond

// loop is the room goroutine. It exits only on cmdClose or context cancellation,
// and it never blocks on a member.
func (r *room) loop() {
	defer close(r.done)
	defer r.cancel()

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	// flushC is nil while no batch is pending, which disables the select case. This
	// is the idiomatic way to express "wait for a batch window only when there is a
	// batch": no extra goroutine, no channel gymnastics.
	var flushTimer *time.Timer
	var flushC <-chan time.Time

	for {
		select {
		case <-r.ctx.Done():
			r.flushPending()
			r.shutdown()
			return

		case cmd := <-r.commands:
			if cmd.kind == cmdClose {
				// Drain staged work so a shutdown does not drop ops clients were
				// already acked for.
				r.flushPending()
				r.shutdown()
				return
			}
			r.handle(cmd)
			// Arm the batch window on the first staged op; a full batch flushes
			// inside handle, in which case the timer must be disarmed below.
			if len(r.pending) > 0 && flushC == nil {
				if flushTimer == nil {
					flushTimer = time.NewTimer(groupCommitWindow)
				} else {
					flushTimer.Reset(groupCommitWindow)
				}
				flushC = flushTimer.C
			}
			if len(r.pending) == 0 && flushC != nil {
				flushTimer.Stop()
				flushC = nil
			}
			r.flushCursors()
			r.reapPendingLeaves()

		case <-flushC:
			r.flushPending()
			flushC = nil
			flushTimer.Stop()

		case <-ticker.C:
			r.flushCursors()
			r.reapPendingLeaves()
			r.maybeSnapshot()
			r.reportBatchStats()
		}
	}
}

// reportBatchStats logs group-commit throughput every ~5s when there was traffic.
func (r *room) reportBatchStats() {
	r.reportTicks++
	if r.reportTicks < 200 { // 200 * 25ms = 5s
		return
	}
	r.reportTicks = 0
	b := r.batchStats
	if b.batches == 0 {
		return
	}
	r.log.Info("batch report",
		"batches", b.batches,
		"ops", b.ops,
		"avg_size", b.ops/b.batches,
		"append_ms", float64(b.appendNs/1e6)/float64(b.batches),
		"publish_ms", float64(b.publishNs/1e6)/float64(b.batches),
		"pending", len(r.pending),
		"inbox_len", len(r.commands),
	)
	r.batchStats = batchStats{}
}

// handle dispatches one command. It must stay non-blocking.
func (r *room) handle(cmd command) {
	switch cmd.kind {
	case cmdJoin:
		r.handleJoin(cmd)
	case cmdJoined:
		r.handleJoined(cmd)
	case cmdLeave:
		r.handleDisconnect(cmd.member, false)
	case cmdHello:
		r.handleHello(cmd)
	case cmdOp:
		r.handleOp(cmd)
	case cmdCursor:
		r.handleCursor(cmd)
	case cmdResync:
		r.handleResync(cmd)
	case cmdPing:
		r.handlePing(cmd)
	case cmdBye:
		r.handleDisconnect(cmd.member, true)
	case cmdRemote:
		r.handleRemote(cmd)
	case cmdShapes:
		r.handleShapes(cmd)
	case cmdGrant:
		r.handleGrant(cmd)
	case cmdTransfer:
		r.handleTransfer(cmd)
	}
}

// handleShapes replies with a copy of the live shapes.
func (r *room) handleShapes(cmd command) {
	shapes := sortedShapes(r.shapes)
	if cmd.shapesReply != nil {
		cmd.shapesReply <- shapes
	}
}

// shutdown flushes the final snapshot and closes every member with 1001 so
// clients know to reconnect elsewhere.
func (r *room) shutdown() {
	r.closed = true
	// Final snapshot so a restart can rebuild without replaying the whole log.
	r.writeSnapshot()

	for m := range r.members {
		m.Kick(protocol.CloseGoingAway, "server shutting down")
	}
	r.members = make(map[Member]*memberState)
	r.publishStats()
}

// --- join / leave -------------------------------------------------------

func (r *room) handleJoin(cmd command) {
	reply := cmd.reply
	if reply == nil {
		return
	}
	if r.closed {
		reply <- joinReply{err: ErrClosed}
		return
	}
	if len(r.members) >= r.opts.Limits.MaxConnPerRoom {
		reply <- joinReply{err: ErrFull}
		return
	}
	if _, ok := r.members[cmd.member]; ok {
		u := cmd.member.User()
		reply <- joinReply{role: r.effectiveRoleOf(u.ClientID, cmd.readonly), owner: r.ownerClientID, online: len(r.members)}
		return
	}
	now := r.opts.Now()
	user := cmd.member.User()
	readonly := cmd.readonly
	// If this client had a pending leave inside the grace window, cancel it: a
	// reconnect within 10s must not produce leave+join chatter for everyone else.
	if _, ok := r.pendingLeave[cmd.member]; ok {
		delete(r.pendingLeave, cmd.member)
	}
	// A different connection from the same clientId (page refresh) supersedes the
	// old one only after the grace window; here we simply cancel a pending leave
	// for the same clientId by matching users.
	for m, lt := range r.pendingLeave {
		if lt.user.ClientID == user.ClientID {
			delete(r.pendingLeave, m)
		}
	}

	// The handshake role is decided here, while holding the room's state
	// (docs/PERMISSIONS.md §4.2). joinOrder must be updated before the member is
	// inserted, or the cid would count as already online.
	role := r.roleForJoin(user.ClientID, readonly)
	if role == protocol.RoleOwner && r.ownerClientID != user.ClientID {
		// First entrant into an unowned room takes ownership (§2.1 / §2.2).
		r.ownerClientID = user.ClientID
		r.persistOwner()
	}
	if role == protocol.RoleOwner {
		// An ownership row would shadow nothing while the cid is the owner, but it
		// must not survive to resurrect an old role after a transfer away.
		delete(r.grants, user.ClientID)
	}
	r.noteJoinOrder(user.ClientID)
	r.recordRole(user.ClientID, role)
	user.Role = role
	cmd.member.SetRole(role)

	r.members[cmd.member] = &memberState{
		member:   cmd.member,
		user:     user,
		readonly: readonly,
		limiter:  newTokenBucket(float64(r.opts.Limits.OpsPerSecond), r.opts.Limits.OpsBurst, now),
		joinedAt: now,
	}

	reply <- joinReply{role: role, owner: r.ownerClientID, online: len(r.members)}

	// joined is sent by the connection layer immediately after this returns, not
	// here: the protocol requires it before any other frame, and the transport is
	// the only place that can guarantee the order. announced is false until
	// NotifyJoined, so no broadcast can reach the socket before joined.
	r.publishStats()
}

// effectiveRoleOf caps a room-level role with the readonly credential.
func (r *room) effectiveRoleOf(cid string, readonly bool) string {
	return effectiveRole(cid, r.roleOfCid(cid), readonly)
}

// handleJoined announces a new member to everyone (including itself, so the
// client's member list is authoritative from the first frame).
func (r *room) handleJoined(cmd command) {
	ms, ok := r.members[cmd.member]
	if !ok {
		return
	}
	ms.announced = true
	user := ms.user
	// A connection's effective role can change between Join and joined only if a
	// broadcast raced the handshake, so refresh the cached value from the
	// authoritative state.
	role := effectiveRole(user.ClientID, r.roleOfCid(user.ClientID), ms.readonly)
	if role != user.Role {
		cmd.member.SetRole(role)
		ms.user.Role = role
		user.Role = role
	}
	r.broadcastPresence(protocol.TypePresenceJoin, &user)
	r.publishStats()
}

// handleRoleChange applies room.grant or room.transfer on behalf of a member.
//
// Authorization is evaluated here from the room's live state, never from a role
// captured at handshake (docs/PERMISSIONS.md §5): the sender's cached role is
// only a UI hint.
func (r *room) handleGrant(cmd command) {
	m := cmd.member
	ms, ok := r.members[m]
	if !ok {
		return
	}
	if cmd.grant == nil {
		r.sendPermissionError(m, protocol.CodeBadRequest, "missing payload", false)
		return
	}
	if !r.isOwnerOf(ms) {
		r.denyPermission(m, "only the room owner may grant roles")
		return
	}
	target := cmd.grant.TargetClientID
	if target == "" {
		r.sendPermissionError(m, protocol.CodeInvalidGrant, "targetClientId is required", false)
		return
	}
	if target == ms.user.ClientID {
		// §3.3: an owner cannot grant themselves; demoting yourself is transfer.
		r.sendPermissionError(m, protocol.CodeInvalidGrant, "use room.transfer to hand over ownership", false)
		return
	}
	if !protocol.ValidGrantRole(cmd.grant.Role) {
		r.sendPermissionError(m, protocol.CodeInvalidGrant, "role must be editor or viewer", false)
		return
	}
	if !r.cidHasLiveMember(target) {
		r.sendPermissionError(m, protocol.CodeTargetOffline, "target is not online", false)
		return
	}
	if target == r.ownerClientID {
		// Granting the owner is a no-op that must not create a shadow row.
		r.sendPermissionError(m, protocol.CodeInvalidGrant, "the owner's role is not grantable", false)
		return
	}
	if cmd.grant.Role == protocol.RoleEditor && !r.hasWritableMember(target) {
		// A readonly connection is pinned to viewer forever (PERMISSIONS §4.2
		// rule 1). Granting editor to it would show a success in the owner's UI
		// while the target stays unable to draw.
		r.sendPermissionError(m, protocol.CodeTargetReadonly, "target has only a read-only connection", false)
		return
	}
	r.setGrantLocked(target, cmd.grant.Role, ms.user.ClientID)
	r.emitRoleChange(protocol.MemberRolePayload{
		ClientID:  target,
		Role:      cmd.grant.Role,
		GrantedBy: ms.user.ClientID,
	})
}

func (r *room) handleTransfer(cmd command) {
	m := cmd.member
	ms, ok := r.members[m]
	if !ok {
		return
	}
	if cmd.transfer == nil {
		r.sendPermissionError(m, protocol.CodeBadRequest, "missing payload", false)
		return
	}
	if !r.isOwnerOf(ms) {
		r.denyPermission(m, "only the room owner may transfer ownership")
		return
	}
	target := cmd.transfer.TargetClientID
	if target == "" || target == ms.user.ClientID {
		r.sendPermissionError(m, protocol.CodeInvalidGrant, "target must be another member", false)
		return
	}
	if !r.cidHasLiveMember(target) {
		r.sendPermissionError(m, protocol.CodeTargetOffline, "target is not online", false)
		return
	}
	if !r.hasWritableMember(target) {
		// Transferring to a readonly-only member would make ownerClientID point
		// at a connection that is permanently capped to viewer: nobody could
		// grant or transfer until that member leaves.
		r.sendPermissionError(m, protocol.CodeTargetReadonly, "target has only a read-only connection", false)
		return
	}
	prev := r.ownerClientID
	r.setOwnerLocked(target)
	// Broadcast both sides, in order: the target first so a client applying them
	// in seq order never sees the room briefly ownerless in its own view.
	r.emitRoleChange(protocol.MemberRolePayload{ClientID: target, Role: protocol.RoleOwner, GrantedBy: prev})
	r.emitRoleChange(protocol.MemberRolePayload{ClientID: prev, Role: protocol.RoleEditor, GrantedBy: target})
}

// isOwnerOf reports whether the connection currently owns the room. A readonly
// connection is never the owner, even when its clientId matches
// (docs/PERMISSIONS.md §4.2 rule 1).
func (r *room) isOwnerOf(ms *memberState) bool {
	if ms.readonly || r.ownerClientID == "" {
		return false
	}
	return ms.user.ClientID == r.ownerClientID
}

// denyPermission is the §8 rule for a non-owner attempting an owner-only
// action: error{forbidden} followed by a 4003 close.
func (r *room) denyPermission(m Member, msg string) {
	ms, ok := r.members[m]
	if !ok {
		return
	}
	if r.opts.Metrics != nil {
		r.opts.Metrics.IncOpsRejected(string(protocol.CodeForbidden))
	}
	// Remove the member and announce it exactly like a bye: the transport
	// suppresses its own Leave for a kicked connection, so the room owns the
	// bookkeeping (presence list, join order, ownership inheritance).
	delete(r.members, m)
	user := ms.user
	r.pruneJoinOrder(user.ClientID)
	r.broadcastPresence(protocol.TypePresenceLeave, &user)
	r.inheritOwnership(user.ClientID)
	m.KickWithError(protocol.CloseForbidden, protocol.CodeForbidden, msg)
	r.publishStats()
}

// sendPermissionError emits a non-fatal (or fatal) error frame for a permission
// failure without touching the connection state.
func (r *room) sendPermissionError(m Member, code protocol.ErrCode, msg string, fatal bool) {
	payload := protocol.ErrorPayload{Code: code, Message: msg, Fatal: fatal}
	r.send(m, protocol.TypeError, 0, m.User().ClientID, payload)
	if code != protocol.CodeForbidden && r.opts.Metrics != nil {
		r.opts.Metrics.IncOpsRejected(string(code))
	}
}

// handleDisconnect removes a member. explicit (bye) means skip the grace window.
func (r *room) handleDisconnect(m Member, explicit bool) {
	ms, ok := r.members[m]
	if !ok {
		return
	}
	delete(r.members, m)
	user := ms.user
	r.pruneJoinOrder(user.ClientID)

	if explicit {
		r.broadcastPresence(protocol.TypePresenceLeave, &user)
		r.inheritOwnership(user.ClientID)
		m.Kick(protocol.CloseNormal, "bye")
		r.publishStats()
		return
	}

	// Grace window: keep the member in the presence list so a refresh does not
	// flash for everyone else.
	r.pendingLeave[m] = leaveTimer{
		deadline: r.opts.Now().Add(r.opts.LeaveGrace),
		user:     user,
		readonly: ms.readonly,
	}
	r.publishStats()
}

// reapPendingLeaves broadcasts presence.leave for expired grace windows and
// performs ownership inheritance for the client ids that really left.
func (r *room) reapPendingLeaves() {
	if len(r.pendingLeave) == 0 {
		return
	}
	now := r.opts.Now()
	for m, lt := range r.pendingLeave {
		if now.Before(lt.deadline) {
			continue
		}
		delete(r.pendingLeave, m)
		u := lt.user
		// Do not announce a leave if the same client reconnected meanwhile.
		if r.hasMemberWithClientID(u.ClientID) {
			continue
		}
		r.broadcastPresence(protocol.TypePresenceLeave, &u)
		// The last connection of this client id is gone: hand ownership on if
		// this was the owner (§2.2). Doing it here (not at socket teardown) keeps
		// a refresh from transferring the room.
		r.inheritOwnership(u.ClientID)
	}
	r.publishStats()
}

func (r *room) hasMemberWithClientID(cid string) bool {
	for m := range r.members {
		if m.User().ClientID == cid {
			return true
		}
	}
	return false
}

// --- hello / resync -----------------------------------------------------

func (r *room) handleHello(cmd command) {
	m := cmd.member
	if _, ok := r.members[m]; !ok {
		return
	}
	p := cmd.hello
	if p == nil {
		p = &protocol.HelloPayload{}
	}

	// Optimistically send pending presence.join for other members? No: the
	// protocol says joined carries the online list, and the client rebuilds from
	// it. The room only sends the resync payload here.
	switch {
	case p.LastSeq <= 0:
		r.sendSnapshot(m)
	case p.Epoch != 0 && p.Epoch != r.epoch:
		// The client comes from a different state generation; only a full
		// snapshot is meaningful (docs/PROTOCOL.md §6.6).
		r.sendSnapshot(m)
	default:
		r.sendEventsOrSnapshot(m, p.LastSeq+1)
	}
}

func (r *room) handleResync(cmd command) {
	m := cmd.member
	if _, ok := r.members[m]; !ok {
		return
	}
	p := cmd.resync
	if p == nil {
		r.sendSnapshot(m)
		return
	}
	if p.Epoch != 0 && p.Epoch != r.epoch {
		r.sendSnapshot(m)
		return
	}
	r.sendEventsOrSnapshot(m, p.FromSeq)
}

// sendEventsOrSnapshot replays the log from fromSeq when it is still fully
// retained, otherwise falls back to a snapshot.
//
// The check is deliberately conservative: a gap anywhere between fromSeq and the
// current seq means a delta would violate "events[i].seq == fromSeq + i", and a
// client that detects a gap will immediately ask again — an avoidable round trip.
func (r *room) sendEventsOrSnapshot(m Member, fromSeq int64) {
	if fromSeq <= 0 {
		r.sendSnapshot(m)
		return
	}
	if fromSeq > r.seq {
		// Client claims to be ahead of us: it is from another generation or a
		// buggy build. A snapshot is the only safe answer.
		r.sendSnapshot(m)
		return
	}
	if !r.log_.covers(fromSeq, r.seq) {
		r.sendSnapshot(m)
		return
	}

	maxOps := r.opts.Limits.ResumeMaxOps
	if maxOps <= 0 {
		maxOps = 2000
	}
	events := make([]protocol.Event, 0, maxOps)
	var bytes int64
	for s := fromSeq; s <= r.seq && len(events) < maxOps; s++ {
		rec, ok := r.log_.get(s)
		if !ok {
			// Should not happen after covers(); if it does, snapshot.
			r.sendSnapshot(m)
			return
		}
		ev := rec.toEvent()
		blob, err := json.Marshal(ev)
		if err != nil {
			r.sendSnapshot(m)
			return
		}
		if len(events) > 0 && bytes+int64(len(blob)) > r.opts.Limits.ResumeMaxBytes {
			break
		}
		bytes += int64(len(blob))
		events = append(events, ev)
	}

	toSeq := r.seq
	if len(events) > 0 {
		toSeq = events[len(events)-1].Seq
	}
	payload := protocol.ResyncEventsPayload{
		Epoch:   r.epoch,
		FromSeq: fromSeq,
		ToSeq:   toSeq,
		Events:  events,
		HasMore: toSeq < r.seq,
	}
	r.send(m, protocol.TypeResyncEvents, 0, m.User().ClientID, payload)
}

// sendSnapshot chunks the live shapes per docs/PROTOCOL.md §4.2.
//
// Chunk boundaries respect both the shape count and the 48KB serialised budget;
// the seq is identical in every chunk so a client can apply the whole set at
// once. Above 5000 shapes the protocol forbids WS snapshots entirely and the
// client must use REST paging.
func (r *room) sendSnapshot(m Member) {
	shapes := sortedShapes(r.shapes)
	if len(shapes) > 10000 {
		payload := protocol.ErrorPayload{
			Code:    protocol.CodeSnapshotTooLarge,
			Message: fmt.Sprintf("room has %d shapes; use GET /api/rooms/%s/snapshot", len(shapes), r.code),
		}
		r.send(m, protocol.TypeError, 0, m.User().ClientID, payload)
		return
	}

	maxShapes := r.opts.SnapshotChunkShape
	if maxShapes <= 0 {
		maxShapes = 500
	}
	maxBytes := r.opts.SnapshotChunkBytes
	if maxBytes <= 0 {
		maxBytes = 48 << 10
	}

	// Build the chunks first so `total` is known before the first frame is sent.
	var chunks [][]protocol.Shape
	cur := make([]protocol.Shape, 0, maxShapes)
	var curBytes int
	for _, s := range shapes {
		blob, err := json.Marshal(s)
		if err != nil {
			continue
		}
		if len(cur) > 0 && (len(cur) >= maxShapes || curBytes+len(blob) > maxBytes) {
			chunks = append(chunks, cur)
			cur = make([]protocol.Shape, 0, maxShapes)
			curBytes = 0
		}
		cur = append(cur, s)
		curBytes += len(blob)
	}
	if len(cur) > 0 || len(chunks) == 0 {
		chunks = append(chunks, cur)
	}

	seq := r.seq
	cid := m.User().ClientID
	for i, chunk := range chunks {
		payload := protocol.SnapshotPayload{
			Epoch:    r.epoch,
			Seq:      seq,
			Chunk:    i,
			Total:    len(chunks),
			Shapes:   chunk,
			Complete: i == len(chunks)-1,
		}
		r.send(m, protocol.TypeResyncSnapshot, 0, cid, payload)
		if !r.isMember(m) {
			return
		}
	}
}

// --- op.add -------------------------------------------------------------

// handleOp is the single write path. Validation and staging happen here; the
// actual store write and broadcast are deferred to a group-commit batch
// (see groupcommit.go), which is what keeps the room goroutine from paying one DB
// round trip per op.
func (r *room) handleOp(cmd command) {
	r.enqueueOp(cmd)
}

// opBroadcast is the op.add payload as broadcast to clients.
type opBroadcast struct {
	OpID   string          `json:"opId"`
	Action string          `json:"action"`
	Shape  json.RawMessage `json:"shape,omitempty"`
	ID     string          `json:"id,omitempty"`
	Points [][]float64     `json:"points,omitempty"`
	By     protocol.By     `json:"by"`
	Rev    int             `json:"rev,omitempty"`
	TS     int64           `json:"ts"`

	// payload is the pre-encoded form, filled by stageOp so the commit path does not
	// re-marshal and so the store and the wire see byte-identical payloads.
	payload []byte
}

// appendPoints merges new points into a freehand shape or moves an arrow's
// endpoint. It returns a copy, never mutating the stored shape.
func appendPoints(existing *protocol.Shape, points [][]float64) (*protocol.Shape, error) {
	next := *existing
	data, err := existing.DecodeData()
	if err != nil {
		return nil, err
	}
	switch existing.Kind {
	case protocol.KindFreehand:
		if len(data.Points)+len(points) > protocol.MaxShapePoints {
			return nil, protocol.NewError(protocol.CodeShapeTooLarge, "freehand would exceed %d points", protocol.MaxShapePoints)
		}
		merged := make([][]float64, 0, len(data.Points)+len(points))
		merged = append(merged, data.Points...)
		merged = append(merged, points...)
		data.Points = merged
		next.Geom = protocol.BoundsForPoints(merged)
	case protocol.KindArrow:
		// Appending to an arrow drags its end point; the last submitted point wins.
		last := points[len(points)-1]
		data.X2, data.Y2 = last[0], last[1]
		next.Geom = protocol.BoundsForArrow(data.X1, data.Y1, data.X2, data.Y2)
	default:
		return nil, protocol.NewError(protocol.CodeInvalidOp, "append is not supported for %s", existing.Kind)
	}
	blob, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode shape data: %w", err)
	}
	next.Data = blob
	return &next, nil
}

// --- presence -----------------------------------------------------------

func (r *room) handleCursor(cmd command) {
	m := cmd.member
	ms, ok := r.members[m]
	if !ok || cmd.cursor == nil {
		return
	}
	// Per-connection throttle: keep only the newest position and flush it on the
	// next tick. Silent drop (no error frame) is mandated by the protocol to
	// avoid an error storm from a chatty client.
	if cmd.cursor.X < -1e6 || cmd.cursor.X > 1e6 || cmd.cursor.Y < -1e6 || cmd.cursor.Y > 1e6 {
		if r.opts.Metrics != nil {
			r.opts.Metrics.IncCursorDropped()
		}
		return
	}
	now := r.opts.Now()
	if ms.cursorPending && now.Sub(ms.cursorAt) < r.cursorThrottle() {
		// Still inside the window: overwrite the pending value, do not send.
		ms.cursorX, ms.cursorY = cmd.cursor.X, cmd.cursor.Y
		if r.opts.Metrics != nil {
			r.opts.Metrics.IncCursorDropped()
		}
		return
	}
	ms.cursorPending = true
	ms.cursorX, ms.cursorY = cmd.cursor.X, cmd.cursor.Y
	ms.cursorAt = now
}

// cursorThrottle returns the coalescing window, defaulting to 50ms.
func (r *room) cursorThrottle() time.Duration {
	if r.opts.CursorThrottle > 0 {
		return r.opts.CursorThrottle
	}
	return 50 * time.Millisecond
}

// flushCursors emits one coalesced cursor frame per member that moved.
func (r *room) flushCursors() {
	now := r.opts.Now()
	for _, ms := range r.members {
		if !ms.cursorPending {
			continue
		}
		if now.Sub(ms.cursorAt) < r.cursorThrottle() {
			continue
		}
		ms.cursorPending = false
		user := ms.user
		payload := protocol.CursorBroadcastPayload{
			X: ms.cursorX,
			Y: ms.cursorY,
			From: protocol.User{
				ClientID: user.ClientID,
				UserID:   user.UserID,
				Name:     user.Name,
				Color:    user.Color,
			},
		}
		// Frame is built once and shared by every recipient.
		frame, err := protocol.Marshal(protocol.TypePresenceCur, r.code, user.ClientID, 0, now.UnixMilli(), payload)
		if err != nil {
			continue
		}
		r.fanoutExcept(frame, ms.member)
	}
}

// broadcastPresence sends a presence.join/leave with the full member list.
func (r *room) broadcastPresence(typ string, user *protocol.User) {
	users := make([]protocol.User, 0, len(r.members))
	for _, ms := range r.members {
		if !ms.announced {
			continue
		}
		users = append(users, ms.user)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].ClientID < users[j].ClientID })
	payload := protocol.PresencePayload{
		User:   user,
		Users:  users,
		Online: len(users),
	}
	// clientId in the envelope identifies the subject of the event, not the
	// recipient, matching how op.add identifies its author.
	cid := "srv"
	if user != nil {
		cid = user.ClientID
	}
	frame, err := protocol.Marshal(typ, r.code, cid, 0, r.opts.Now().UnixMilli(), payload)
	if err != nil {
		return
	}
	r.fanout(frame, true)
}

// --- ping / remote ------------------------------------------------------

func (r *room) handlePing(cmd command) {
	m := cmd.member
	if _, ok := r.members[m]; !ok {
		return
	}
	var ts int64
	if cmd.ping != nil {
		ts = cmd.ping.TS
	}
	now := r.opts.Now()
	payload := protocol.PongPayload{TS: ts, ServerTime: now.UnixMilli()}
	frame, err := protocol.Marshal(protocol.TypePong, r.code, m.User().ClientID, 0, now.UnixMilli(), payload)
	if err != nil {
		return
	}
	m.Send(frame)
}

// handleRemote applies an op sequenced by another instance.
//
// The frame is already a complete envelope and is forwarded as-is, so every instance
// produces byte-identical broadcasts.
//
// Ordering is the hard part. With a shared INCR counter two instances can allocate
// in one order and publish in another, so instance B may write seq 2 while A's seq 1
// is still in flight. Blindly skipping "remoteSeq <= our seq" would then drop A's
// shape from B's view forever. The rule implemented here is:
//
//   - seq > our seq + 1: a hole. Buffer the frame, bounded by remotePendingMax.
//   - seq == our seq + 1: apply, then drain any buffered frames that became
//     contiguous.
//   - seq <= our seq: apply it *only if this instance never applied it*, which is
//     detected against the retained log. A local write is recorded there too, so a
//     genuine duplicate is still skipped. Sequence numbers never move backwards.
//
// A hole that never fills (a peer crashed between INCR and publish) leaves the room
// behind; its clients see the seq gap and resync from the store, which is the
// documented INCR trade-off (ARCHITECTURE.md §4.1).
func (r *room) handleRemote(cmd command) {
	if cmd.remoteReply != nil {
		defer func() { cmd.remoteReply <- true }()
	}
	switch {
	case cmd.remoteSeq > r.seq+1:
		if r.opts.Metrics != nil {
			r.opts.Metrics.IncSeqGap()
		}
		r.log.Warn("remote seq gap, buffering",
			"have", r.seq, "got", cmd.remoteSeq, "buffered", len(r.remotePending))
		if len(r.remotePending) >= remotePendingMax {
			// Bounded: drop the oldest and let clients repair through resync. An
			// unbounded buffer fed by a broken peer would be an OOM vector.
			var oldest int64 = -1
			for seq := range r.remotePending {
				if oldest < 0 || seq < oldest {
					oldest = seq
				}
			}
			delete(r.remotePending, oldest)
		}
		r.remotePending[cmd.remoteSeq] = cmd.frame
		// The missing frame may predate this instance's subscription, in which case it
		// will never arrive. Ask the store for the gap; a no-op in memory mode.
		r.catchUpTo(cmd.remoteSeq - 1)

	case cmd.remoteSeq == r.seq+1:
		r.applyRemoteFrame(cmd.remoteSeq, cmd.frame)
		for {
			frame, ok := r.remotePending[r.seq+1]
			if !ok {
				return
			}
			delete(r.remotePending, r.seq+1)
			r.applyRemoteFrame(r.seq+1, frame)
		}

	default: // cmd.remoteSeq <= r.seq
		if _, seen := r.log_.get(cmd.remoteSeq); seen {
			return // genuine duplicate delivery
		}
		// Never applied here: restore the shape without moving seq backwards. The
		// frame is still fanned out so local clients converge too.
		r.applyRemoteBody(cmd.remoteSeq, cmd.frame)
	}
}

// remotePendingMax bounds the out-of-order buffer: far above any realistic pub/sub
// reordering window, small enough that a broken peer costs a few hundred KB.
const remotePendingMax = 256

// applyRemoteFrame applies a frame that continues the sequence, advancing seq.
func (r *room) applyRemoteFrame(seq int64, frame []byte) {
	r.applyRemoteBody(seq, frame)
	r.seq = seq
	r.publishStats()
}

// applyRemoteBody applies a remote op's shape mutation, records it in the log ring
// and fans the original frame out to local members. It never touches r.seq, so it is
// safe for a late frame that arrives after a local write advanced the sequence.
func (r *room) applyRemoteBody(seq int64, frame []byte) {
	var env protocol.Envelope
	if err := json.Unmarshal(frame, &env); err != nil {
		return
	}
	if env.Type == protocol.TypeMemberRole {
		// Permission changes travel on the same stream so instances converge. The
		// originating instance persisted the change, so this one only mirrors it
		// in memory (no second store write) and fans it out unchanged.
		var p protocol.MemberRolePayload
		if err := json.Unmarshal(env.Payload, &p); err != nil || p.ClientID == "" {
			return
		}
		r.applyRemoteRole(p)
		r.log_.put(logEntry{
			Seq: seq, Epoch: r.epoch, Type: protocol.TypeMemberRole,
			ClientID: p.ClientID, Role: p.Role, GrantedBy: p.GrantedBy, TS: env.TS,
		})
		r.fanout(frame, true)
		return
	}
	var p opBroadcast
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return
	}
	switch p.Action {
	case protocol.ActionCreate, protocol.ActionUpdate, protocol.ActionDelete:
		if len(p.Shape) > 0 {
			var s protocol.Shape
			if err := json.Unmarshal(p.Shape, &s); err == nil {
				r.setShape(&s)
			} else {
			}
		} else {
		}
	case protocol.ActionAppend:
		if existing, ok := r.shapes[p.ID]; ok {
			if appended, err := appendPoints(existing, p.Points); err == nil {
				r.setShape(appended)
			}
		}
	}
	r.log_.put(logEntry{
		Seq:      seq,
		OpID:     p.OpID,
		ClientID: env.ClientID,
		Action:   p.Action,
		Shape:    p.Shape,
		ID:       p.ID,
		Points:   p.Points,
		By:       p.By,
		TS:       p.TS,
	})
	// The origin instance already fanned out to its own members; this room serves
	// only its local members.
	r.fanout(frame, true)
}

// catchUpTo replays every stored op between the last seq this room actually applied
// and target, then applies any buffered frames that became contiguous.
//
// It is deliberately best-effort: a store failure leaves the gap, and clients repair
// it through resync. Silently ignoring the gap would instead leave this instance's
// shape map permanently incomplete, which is a correctness bug rather than a
// latency one.
func (r *room) catchUpTo(target int64) {
	if r.opts.CatchUp == nil || target <= r.seq {
		return
	}
	// Bound the replay: a cold room has its own path (LoadRoom) and should not
	// replay thousands of ops here.
	const maxReplay = 2000
	from := r.seq + 1
	if target-from+1 > maxReplay {
		from = target - maxReplay + 1
	}
	ops, err := r.opts.CatchUp.ReplayRange(r.ctx, r.code, from, target)
	if err != nil {
		r.log.Warn("catch-up replay failed", "from", from, "to", target, "err", err)
		return
	}
	for _, op := range ops {
		// Skip anything already applied (a race with the live stream is normal).
		if op.Seq <= r.seq {
			continue
		}
		if op.Seq != r.seq+1 {
			// Still a hole inside the store range; stop and let the next attempt or a
			// client resync resolve it.
			break
		}
		r.applyReplayOp(op)
	}
	// Drain frames that the replay has now made contiguous.
	for {
		frame, ok := r.remotePending[r.seq+1]
		if !ok {
			return
		}
		delete(r.remotePending, r.seq+1)
		r.applyRemoteFrame(r.seq+1, frame)
	}
}

// applyRemoteRole mirrors a permission change decided by another instance.
//
// It deliberately does not persist or re-broadcast: the originating instance
// already wrote the row and published the frame, so a second write would be
// redundant and the fan-out at the call site already reaches local clients.
func (r *room) applyRemoteRole(p protocol.MemberRolePayload) {
	if p.ClientID == "" {
		return
	}
	if p.Role == protocol.RoleOwner {
		prev := r.ownerClientID
		r.ownerClientID = p.ClientID
		delete(r.grants, p.ClientID)
		r.recordRole(p.ClientID, protocol.RoleOwner)
		if prev != "" && prev != p.ClientID {
			r.grants[prev] = protocol.RoleEditor
			r.recordRole(prev, protocol.RoleEditor)
		}
		r.refreshRoles()
		return
	}
	if protocol.ValidGrantRole(p.Role) {
		r.grants[p.ClientID] = p.Role
		r.recordRole(p.ClientID, p.Role)
		r.refreshRoles()
	}
}

// applyReplayOp applies one stored op to the in-memory state without fanning it out:
// the op is history that local clients either already have or will request.
func (r *room) applyReplayOp(op ReplayOp) {
	switch op.Action {
	case protocol.ActionCreate, protocol.ActionUpdate, protocol.ActionDelete:
		if len(op.Shape) > 0 {
			var s protocol.Shape
			if err := json.Unmarshal(op.Shape, &s); err == nil {
				r.setShape(&s)
			}
		}
	case protocol.ActionAppend:
		if existing, ok := r.shapes[op.ID]; ok {
			if appended, err := appendPoints(existing, op.Points); err == nil {
				r.setShape(appended)
			}
		}
	}
	r.seq = op.Seq
	r.log_.put(logEntry{
		Seq:      op.Seq,
		Epoch:    op.Epoch,
		OpID:     op.OpID,
		ClientID: op.ClientID,
		Action:   op.Action,
		Shape:    op.Shape,
		ID:       op.ID,
		Points:   op.Points,
		By:       op.By,
		TS:       op.TS,
	})
	r.publishStats()
}

// dropPendingBelow discards buffered remote frames at or below seq.
//
// Those frames arrived ahead of the frame that closed the gap, but a local write has
// now advanced the room past them, so they can never be applied in order. The shapes
// they carried are not lost to clients: a client that sees the seq jump issues
// resync.request and receives the authoritative range from the store, and a client
// that reconnects gets a snapshot built from the store.
func (r *room) dropPendingBelow(seq int64) {
	for pendingSeq := range r.remotePending {
		if pendingSeq <= seq {
			delete(r.remotePending, pendingSeq)
		}
	}
}

// --- fan-out primitives -------------------------------------------------

// fanout delivers frame to every member. Persistent frames that cannot be
// queued mark the member lagging; it is kicked on the spot so it reconnects and
// catches up via resync, rather than silently missing an op forever.
//
// A member that has been admitted but whose joined frame is not queued yet is
// skipped: sending a fan-out frame first would violate "joined is the first
// server frame" (docs/PROTOCOL.md §3).
func (r *room) fanout(frame []byte, persistent bool) {
	var kicked []Member
	for m, ms := range r.members {
		if !ms.announced {
			continue
		}
		if !m.Send(frame) {
			ms.lagging = true
			if persistent {
				kicked = append(kicked, m)
			}
		}
	}
	for _, m := range kicked {
		delete(r.members, m)
		if r.opts.Metrics != nil {
			r.opts.Metrics.IncSlowConsumerKick()
		}
		// A kick is a policy decision worth a log line: it means a client could not
		// keep up and will be told to reconnect and resync (close 4001).
		r.log.Warn("evicting slow consumer", "client", m.User().ClientID)
		m.Kick(protocol.CloseSlowConsumer, "send buffer overflow")
	}
	if len(kicked) > 0 {
		r.publishStats()
	}
}

// fanoutExcept delivers to everyone but one member (cursor echo suppression).
func (r *room) fanoutExcept(frame []byte, skip Member) {
	for m, ms := range r.members {
		if m == skip || !ms.announced {
			continue
		}
		m.Send(frame)
	}
}

// send marshals and delivers one frame to one member.
func (r *room) send(m Member, typ string, seq int64, cid string, payload any) {
	ts := r.opts.Now().UnixMilli()
	frame, err := protocol.Marshal(typ, r.code, cid, seq, ts, payload)
	if err != nil {
		r.log.Error("encode frame", "type", typ, "err", err)
		return
	}
	if !m.Send(frame) {
		r.log.Warn("dropping frame for slow consumer", "type", typ)
	}
}

// sendAck sends an ack to the originating member only.
func (r *room) sendAck(m Member, opID string, status protocol.AckStatus, seq int64, code protocol.ErrCode, msg string) {
	payload := protocol.AckPayload{OpID: opID, Status: status, Seq: seq, Code: code, Message: msg}
	if status == protocol.AckRejected {
		// Keep the payload.seq empty for a rejection: there is no seq.
		payload.Seq = 0
	}
	r.send(m, protocol.TypeAck, 0, m.User().ClientID, payload)
}

// reject reports a rejected write as an ack (not an error frame), which is what
// the client expects for op-level failures. The connection stays open.
func (r *room) reject(m Member, opID string, code protocol.ErrCode, msg string) {
	r.rejectOp(m, opID, code, msg)
}

func (r *room) rejectOp(m Member, opID string, code protocol.ErrCode, msg string) {
	r.sendAck(m, opID, protocol.AckRejected, 0, code, msg)
	if r.opts.Metrics != nil {
		r.opts.Metrics.IncOpsRejected(string(code))
	}
}

func (r *room) isMember(m Member) bool {
	_, ok := r.members[m]
	return ok
}

// --- snapshots ----------------------------------------------------------

// maybeSnapshot writes a snapshot when the op count or the wall clock says so.
func (r *room) maybeSnapshot() {
	if r.opsSinceSnapshot < r.opts.SnapshotEvery {
		return
	}
	r.opsSinceSnapshot = 0
	r.writeSnapshot()
}

// writeSnapshot serialises the live shapes and hands them to the store.
//
// A failure is logged, never fatal: the event log remains the source of truth
// and the next attempt will succeed (docs/DATA-MODEL.md §1).
func (r *room) writeSnapshot() {
	if r.opts.SnapshotHook == nil && r.opts.Persist == nil {
		return
	}
	shapes := sortedShapes(r.shapes)
	blob, err := json.Marshal(shapes)
	if err != nil {
		r.log.Error("snapshot encode failed", "err", err)
		return
	}
	if r.opts.SnapshotHook != nil {
		r.opts.SnapshotHook(r.code, r.epoch, r.seq, len(shapes), blob)
	}
	if r.opts.Persist != nil {
		if err := r.opts.Persist.SaveSnapshot(r.ctx, r.code, r.epoch, r.seq, len(shapes), blob); err != nil {
			r.log.Warn("snapshot save failed", "err", err)
		}
	}
}
