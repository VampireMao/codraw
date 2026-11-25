package integration

import (
	"testing"
	"time"

	"github.com/codraw/codraw/backend/internal/protocol"
)

// TestSameBatchCreateThenDeleteIsApplied reproduces the note-tool failure mode:
// a create followed immediately by a delete lands in the same 20ms group-commit
// batch. Validation must see the staged create; otherwise the delete is rejected
// as shape_not_found and the shape survives as a ghost after reload.
func TestSameBatchCreateThenDeleteIsApplied(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("same-batch delete")
	const cid = "c_batch_owner1"

	owner := s.dial(room.Code, cid, "owner", room.EditorKey, "")
	owner.join(room.Code, cid, 0, 0)

	id := shapeID("batchdel")
	owner.send(protocol.TypeOpAdd, room.Code, cid, protocol.OpAddPayload{
		OpID: opID("batchdel-create"), Action: protocol.ActionCreate, Shape: rectShape(id),
	})
	owner.send(protocol.TypeOpAdd, room.Code, cid, protocol.OpAddPayload{
		OpID: opID("batchdel-delete"), Action: protocol.ActionDelete, ID: id,
	})

	createAck, err := owner.waitForAck(opID("batchdel-create"), 5*time.Second)
	if err != nil {
		t.Fatalf("create ack: %v", err)
	}
	deleteAck, err := owner.waitForAck(opID("batchdel-delete"), 5*time.Second)
	if err != nil {
		t.Fatalf("delete ack: %v", err)
	}
	if createAck.Status != protocol.AckApplied {
		t.Fatalf("create status = %q (%s)", createAck.Status, createAck.Code)
	}
	if deleteAck.Status != protocol.AckApplied {
		t.Fatalf("delete status = %q, code = %q", deleteAck.Status, deleteAck.Code)
	}
	if deleteAck.Seq != createAck.Seq+1 {
		t.Fatalf("delete seq = %d, want %d", deleteAck.Seq, createAck.Seq+1)
	}
}

// TestSameBatchCreateThenUpdateIsApplied covers the same overlay for updates:
// the update must see the staged create's rev/author instead of shape_not_found.
func TestSameBatchCreateThenUpdateIsApplied(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("same-batch update")
	const cid = "c_batch_owner2"

	owner := s.dial(room.Code, cid, "owner", room.EditorKey, "")
	owner.join(room.Code, cid, 0, 0)

	id := shapeID("batchupd")
	owner.send(protocol.TypeOpAdd, room.Code, cid, protocol.OpAddPayload{
		OpID: opID("batchupd-create"), Action: protocol.ActionCreate, Shape: rectShape(id),
	})
	owner.send(protocol.TypeOpAdd, room.Code, cid, protocol.OpAddPayload{
		OpID: opID("batchupd-update"), Action: protocol.ActionUpdate, Shape: rectShape(id),
	})

	createAck, err := owner.waitForAck(opID("batchupd-create"), 5*time.Second)
	if err != nil {
		t.Fatalf("create ack: %v", err)
	}
	updateAck, err := owner.waitForAck(opID("batchupd-update"), 5*time.Second)
	if err != nil {
		t.Fatalf("update ack: %v", err)
	}
	if updateAck.Status != protocol.AckApplied {
		t.Fatalf("update status = %q, code = %q", updateAck.Status, updateAck.Code)
	}
	if updateAck.Seq != createAck.Seq+1 {
		t.Fatalf("update seq = %d, want %d", updateAck.Seq, createAck.Seq+1)
	}
}
