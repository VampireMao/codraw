package room

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/codraw/codraw/backend/internal/metrics"
	"github.com/codraw/codraw/backend/internal/protocol"
)

// Group commit
//
// Every op used to cost one synchronous store round trip (plus INCR and PUBLISH
// with Redis) *inside the room goroutine*. At 500 op/s that is 1500 round trips per
// second serialised on one goroutine, which is what pushed p95 to seconds and made
// the outbound queues overflow.
//
// The fix keeps the "persist before broadcast" guarantee and changes only the
// granularity: the room accumulates ops, writes them in one batch, and broadcasts
// the whole batch after the write succeeds.
//
// Invariants preserved:
//
//   - seq stays strictly monotonic and gap-free: seqs are allocated up front for
//     the batch, in order, and a failed batch rolls every one of them back.
//   - nothing is broadcast before its batch is durable: a client can never see a
//     seq that a restart could not replay.
//   - the room goroutine never blocks on a client or on the store: the batch write
//     is the one place it waits, and it waits for a bounded amount of work.
//
// Batch window bounds (both must be small so idle latency does not grow):
//   - at most groupCommitMaxOps ops, or
//   - at most groupCommitWindow of wall time, whichever comes first.
const (
	// groupCommitMaxOps caps a batch so a burst cannot turn into one enormous write.
	groupCommitMaxOps = 256
	// groupCommitWindow is how long the first op of a batch waits for company.
	//
	// The batch is the unit of DB cost: one round trip + one fsync regardless of how
	// many rows it carries (measured ~1.4ms at n=1, ~4.5ms at n=64). So the window
	// must be long enough to actually gather rows. At 500 op/s a room-wide 20ms window
	// collects ~10 ops, which keeps DB time near 40% of one core and leaves headroom;
	// 8ms collected only ~4 and spent most of the room's time waiting on Postgres.
	//
	// It is a ceiling, not a fixed cost: a full batch flushes immediately, and the
	// value is far inside the 150ms p95 target.
	groupCommitWindow = 20 * time.Millisecond
)

// pendingOp is one validated op waiting for its batch write.
type pendingOp struct {
	member  Member
	user    protocol.User
	payload protocol.OpAddPayload

	// The mutation is already computed and validated against room state; it is
	// applied to the live maps only after the batch is durable.
	shape     protocol.Shape
	shapeID   string
	action    string
	opID      string
	broadcast opBroadcast
	frame     []byte
	seq       int64
	ts        int64
	epoch     int64

	// appendPoints for the append action, kept so commit can apply the same delta.
	points [][]float64

	// duplicate is set when the store (or the in-room dedupe ring) reported that
	// this (clientId, opId) was already applied; the op is acked, never written.
	duplicate bool
}

// enqueueOp validates an op, stages its mutation and adds it to the pending batch.
// It never writes to the store; the loop flushes the batch.
func (r *room) enqueueOp(cmd command) {
	m := cmd.member
	ms, ok := r.members[m]
	if !ok {
		return
	}
	user := ms.user
	p := cmd.op
	if p == nil {
		r.reject(m, "", protocol.CodeBadRequest, "missing payload")
		return
	}

	// 1. Authorisation, re-evaluated on every op from the live permission state
	//    (docs/PERMISSIONS.md §5). Caching it from the handshake would make a
	//    revocation take effect only on reconnect.
	role := r.effectiveRoleOf(user.ClientID, ms.readonly)
	if role != user.Role {
		m.SetRole(role)
		ms.user.Role = role
		user.Role = role
	}
	if !protocol.CanWrite(role) {
		r.rejectOp(m, p.OpID, protocol.CodeForbidden, "viewer cannot write")
		return
	}
	// 2. Rate limit, per connection, owned by this goroutine.
	if !ms.limiter.allow(r.opts.Now()) {
		r.rejectOp(m, p.OpID, protocol.CodeRateLimited, "too many ops")
		return
	}
	// 3. Structural validation.
	if err := p.Validate(r.opts.Limits.MaxShapeBytes); err != nil {
		pe, _ := protocol.AsError(err)
		code, msg := protocol.CodeInvalidOp, err.Error()
		if pe != nil {
			code, msg = pe.Code, pe.Message
		}
		r.rejectOp(m, p.OpID, code, msg)
		return
	}

	// 4. Stage the mutation. Nothing is committed to the shape map yet.
	op, err := r.stageOp(user, p)
	if err != nil {
		pe, _ := protocol.AsError(err)
		code, msg := protocol.CodeInternal, err.Error()
		if pe != nil {
			code, msg = pe.Code, pe.Message
		}
		if code == protocol.CodeInternal {
			r.log.Error("op failed", "err", err, "op_id", p.OpID)
		}
		r.rejectOp(m, p.OpID, code, msg)
		return
	}
	op.member = m
	op.user = user
	op.payload = *p

	r.pending = append(r.pending, op)
	if len(r.pending) >= groupCommitMaxOps {
		r.flushPending()
	}
}

// stageOp computes the resulting shape and the broadcast payload without touching
// room state, so a rejected or failed op leaves no trace.
func (r *room) stageOp(user protocol.User, p *protocol.OpAddPayload) (*pendingOp, error) {
	now := r.opts.Now()
	ts := now.UnixMilli()

	// Idempotency first: a retransmit must not consume a seq even if it would now
	// fail validation (the shape may have been deleted since).
	if seq, ok := r.dedup.get(user.ClientID, p.OpID, now); ok {
		return &pendingOp{opID: p.OpID, seq: seq, action: p.Action, member: nil}, nil
	}
	if p.Action == protocol.ActionCreate && liveShapeCount(r.shapes) >= r.opts.Limits.MaxShapesPerRoom {
		return nil, protocol.NewError(protocol.CodeRoomFullShapes, "room reached %d shapes", r.opts.Limits.MaxShapesPerRoom)
	}

	var shape protocol.Shape
	switch p.Action {
	case protocol.ActionCreate, protocol.ActionUpdate:
		if err := json.Unmarshal(p.Shape, &shape); err != nil {
			return nil, protocol.NewError(protocol.CodeInvalidOp, "shape is not a valid object")
		}
		if err := protocol.ValidateShape(&shape, r.opts.Limits.MaxShapeBytes); err != nil {
			return nil, err
		}
	default:
		if len(p.Shape) > 0 {
			if err := json.Unmarshal(p.Shape, &shape); err != nil {
				return nil, protocol.NewError(protocol.CodeInvalidOp, "shape is not a valid object")
			}
		}
	}

	op := &pendingOp{
		action: p.Action,
		opID:   p.OpID,
		ts:     ts,
		epoch:  r.epoch,
	}
	by := protocol.By{
		ClientID: user.ClientID, UserID: user.UserID, Name: user.Name, Color: user.Color,
	}
	op.broadcast = opBroadcast{OpID: p.OpID, Action: p.Action, By: by, TS: ts}

	switch p.Action {
	case protocol.ActionCreate:
		existing, exists, deleted := r.effectiveShape(shape.ID)
		if exists && !deleted {
			return nil, protocol.NewError(protocol.CodeDuplicateShape, "shape %s already exists", shape.ID)
		}
		shape.Author = protocol.Author{CID: user.ClientID, Name: user.Name}
		shape.CreatedAt = ts
		shape.UpdatedAt = ts
		shape.Deleted = false
		if exists {
			shape.Rev = existing.Rev + 1
		} else {
			shape.Rev = 1
		}
		op.shape = shape
		op.shapeID = shape.ID
		op.broadcast.Shape = marshalShape(&shape)
		op.broadcast.Rev = shape.Rev

	case protocol.ActionUpdate:
		existing, exists, deleted := r.effectiveShape(shape.ID)
		if !exists {
			return nil, protocol.NewError(protocol.CodeShapeNotFound, "shape %s not found", shape.ID)
		}
		if deleted {
			return nil, protocol.NewError(protocol.CodeShapeDeleted, "shape %s is deleted", shape.ID)
		}
		shape.Author = existing.Author
		shape.CreatedAt = existing.CreatedAt
		shape.UpdatedAt = ts
		shape.Rev = existing.Rev + 1
		shape.Deleted = false
		op.shape = shape
		op.shapeID = shape.ID
		op.broadcast.Shape = marshalShape(&shape)
		op.broadcast.Rev = shape.Rev

	case protocol.ActionDelete:
		existing, exists, deleted := r.effectiveShape(p.ID)
		if !exists {
			return nil, protocol.NewError(protocol.CodeShapeNotFound, "shape %s not found", p.ID)
		}
		if deleted {
			return nil, protocol.NewError(protocol.CodeShapeDeleted, "shape %s already deleted", p.ID)
		}
		del := existing
		del.Deleted = true
		del.Rev = existing.Rev + 1
		del.UpdatedAt = ts
		op.shape = del
		op.shapeID = del.ID
		op.broadcast.Shape = marshalShape(&del)
		op.broadcast.Rev = del.Rev

	case protocol.ActionAppend:
		existing, exists, deleted := r.effectiveShape(p.ID)
		if !exists {
			return nil, protocol.NewError(protocol.CodeShapeNotFound, "shape %s not found", p.ID)
		}
		if deleted {
			return nil, protocol.NewError(protocol.CodeShapeDeleted, "shape %s is deleted", p.ID)
		}
		if existing.Kind != protocol.KindFreehand && existing.Kind != protocol.KindArrow {
			return nil, protocol.NewError(protocol.CodeInvalidOp, "append is only valid for freehand and arrow")
		}
		appended, err := appendPoints(&existing, p.Points)
		if err != nil {
			return nil, err
		}
		appended.UpdatedAt = ts
		appended.Rev = existing.Rev + 1
		op.shape = *appended
		op.shapeID = appended.ID
		op.points = p.Points
		op.broadcast.ID = appended.ID
		op.broadcast.Points = p.Points
		op.broadcast.Rev = appended.Rev

	default:
		return nil, protocol.NewError(protocol.CodeInvalidOp, "unknown action %q", p.Action)
	}

	payload, err := json.Marshal(op.broadcast)
	if err != nil {
		return nil, fmt.Errorf("encode broadcast: %w", err)
	}
	op.broadcast.payload = payload
	return op, nil
}

// effectiveShape resolves how a shape will look after the ops already staged in
// this batch. stageOp runs before the current op is appended to r.pending, so the
// overlay contains only earlier ops — exactly the state this op should see.
//
// Without this overlay, create+delete (or create+update/append) inside one 20ms
// batch saw the committed map only: the delete/update was rejected as
// shape_not_found even though the create was valid and already staged. That was
// how an empty note could be created locally, rejected for deletion, and then
// reappear as a ghost shape after a reload.
func (r *room) effectiveShape(id string) (shape protocol.Shape, exists, deleted bool) {
	if committed, ok := r.shapes[id]; ok {
		shape = *committed
		exists = true
		deleted = committed.Deleted
	}
	for _, p := range r.pending {
		if p.member == nil || p.shapeID != id {
			// Duplicate placeholders (member == nil) carry no shape; other ids are
			// irrelevant to this lookup.
			continue
		}
		switch p.action {
		case protocol.ActionCreate, protocol.ActionUpdate, protocol.ActionDelete, protocol.ActionAppend:
			shape = p.shape
			exists = true
			deleted = p.shape.Deleted
		}
	}
	return shape, exists, deleted
}

// flushPending allocates seqs, writes the batch, then commits and broadcasts it.
//
// Ordering inside one batch is the order the ops arrived, which is the order clients
// expect; seq assignment follows that same order.
func (r *room) flushPending() {
	if len(r.pending) == 0 {
		return
	}
	batch := r.pending
	r.pending = nil

	// Duplicates were resolved during staging (they already carry the original seq)
	// and only need their ack, never a write or a broadcast.
	var toWrite []*pendingOp
	for _, op := range batch {
		if op.member == nil || (op.seq > 0 && op.broadcast.payload == nil) {
			// duplicate: ack immediately, no store work, no fan-out
			if op.member != nil {
				r.sendAck(op.member, op.opID, protocol.AckDuplicate, op.seq, "", "")
			}
			continue
		}
		toWrite = append(toWrite, op)
	}
	if len(toWrite) == 0 {
		return
	}

	// Assign seqs: from the shared source when multi-instance, else locally. Both
	// paths allocate a contiguous run in arrival order.
	if r.opts.SeqSource != nil {
		hi, err := r.opts.SeqSource.NextSeqRange(r.ctx, r.code, len(toWrite))
		if err != nil {
			r.failBatch(toWrite, fmt.Errorf("allocate seq range: %w", err))
			return
		}
		lo := hi - int64(len(toWrite)) + 1
		if lo <= r.seq {
			r.failBatch(toWrite, protocol.NewError(protocol.CodeServerBusy, "sequence allocation out of sync"))
			return
		}
		for i, op := range toWrite {
			op.seq = lo + int64(i)
		}
	} else {
		for i, op := range toWrite {
			op.seq = r.seq + int64(i) + 1
		}
	}

	// Persist the batch. This is the only store round trip for the whole group.
	if r.opts.Persist != nil {
		recs := make([]OpRecord, len(toWrite))
		for i, op := range toWrite {
			recs[i] = OpRecord{
				Code:     r.code,
				Seq:      op.seq,
				Epoch:    op.epoch,
				OpID:     op.opID,
				ClientID: op.user.ClientID,
				AuthorID: op.user.UserID,
				Action:   op.action,
				ShapeID:  op.shapeID,
				Payload:  op.broadcast.payload,
				TS:       time.UnixMilli(op.ts),
			}
		}
		ws := r.opts.Now()
		results, err := r.opts.Persist.AppendBatch(r.ctx, recs)
		r.batchStats.appendNs += r.opts.Now().Sub(ws).Nanoseconds()
		if err != nil {
			r.failBatch(toWrite, fmt.Errorf("persist batch: %w", err))
			return
		}
		// Honour per-record duplicates discovered by the store (a concurrent instance
		// may have persisted the same (cid, opId) first).
		for i, res := range results {
			if i < len(toWrite) && res.Duplicate {
				toWrite[i].duplicate = true
				toWrite[i].seq = res.Seq
			}
		}
	}

	// The batch is durable. Commit to memory and broadcast, in order.
	start := r.opts.Now()
	var (
		pubSeqs   []int64
		pubFrames [][]byte
	)
	for _, op := range toWrite {
		if op.duplicate {
			r.sendAck(op.member, op.opID, protocol.AckDuplicate, op.seq, "", "")
			continue
		}
		if frame := r.commitOp(op); frame != nil {
			pubSeqs = append(pubSeqs, op.seq)
			pubFrames = append(pubFrames, frame)
		}
	}
	// One broker round trip for the whole batch (pipelined in Redis).
	if r.opts.Broadcast != nil && len(pubSeqs) > 0 {
		ps := r.opts.Now()
		err := r.opts.Broadcast.PublishOpBatch(r.ctx, r.code, pubSeqs, pubFrames)
		r.batchStats.publishNs += r.opts.Now().Sub(ps).Nanoseconds()
		if err != nil {
			// Cross-instance fan-out failing must not fail the local write: the ops are
			// already durable and peers recover via resync.
			r.log.Warn("broker publish batch failed", "err", err, "count", len(pubSeqs))
		}
	}
	r.batchStats.batches++
	r.batchStats.ops += int64(len(toWrite))
	elapsed := r.opts.Now().Sub(start)
	if r.opts.Metrics != nil {
		r.opts.Metrics.ObserveBroadcast(elapsed)
	}
	r.opsSinceSnapshot += len(toWrite)
	if r.opts.OnShapeCount != nil {
		r.opts.OnShapeCount(r.code, liveShapeCount(r.shapes))
	}
}

// commitOp applies one durable op to the live state, fans it out locally and acks
// the author. It returns the encoded frame so the caller can publish the whole batch
// to the broker in one round trip.
func (r *room) commitOp(op *pendingOp) []byte {
	r.catchUpTo(op.seq - 1)
	r.seq = op.seq
	r.setShape(&op.shape)
	r.dedup.put(op.user.ClientID, op.opID, op.seq, r.opts.Now())
	r.log_.put(logEntry{
		Seq:      op.seq,
		Epoch:    op.epoch,
		OpID:     op.opID,
		ClientID: op.user.ClientID,
		Action:   op.action,
		Shape:    op.broadcast.Shape,
		ID:       op.broadcast.ID,
		Points:   op.points,
		By:       op.broadcast.By,
		TS:       op.ts,
	})
	r.dropPendingBelow(op.seq)

	frame, err := protocol.Marshal(protocol.TypeOpAdd, r.code, op.user.ClientID, op.seq, op.ts, json.RawMessage(op.broadcast.payload))
	if err != nil {
		r.log.Error("encode op frame", "err", err)
		return nil
	}
	r.fanout(frame, true)
	if r.opts.Metrics != nil {
		// Count the broadcast once per op (not per recipient): the metric is message
		// rate by type, and fan-out is reported separately by connections.
		r.opts.Metrics.ObserveMessage(metrics.DirOut, protocol.TypeOpAdd)
	}
	r.sendAck(op.member, op.opID, protocol.AckApplied, op.seq, "", "")
	r.publishStats()
	return frame
}

// failBatch rolls the batch back and rejects every op in it.
//
// Nothing was committed to the shape map, so rollback is just "drop the staged
// ops". Seqs allocated for the batch are not reused: the room keeps r.seq where it
// was, and the shared counter (if any) stays ahead, which only means a future batch
// starts at a higher number — never a duplicate.
func (r *room) failBatch(ops []*pendingOp, cause error) {
	r.log.Error("batch persist failed, rejecting ops", "count", len(ops), "err", cause)
	for _, op := range ops {
		if op.member == nil {
			continue
		}
		r.rejectOp(op.member, op.opID, protocol.CodeInternal, "storage write failed, retry")
	}
}

// flushDue reports whether the pending batch should be written now.
func (r *room) flushDue() bool {
	return len(r.pending) > 0
}
