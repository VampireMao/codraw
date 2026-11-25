package integration

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/codraw/codraw/backend/internal/protocol"
)

// TestReadonlyMemberCannotBecomeOwnerOrEditor guards the deadlock where an owner
// transfers ownership (or editor) to a member whose only connection is a readonly
// token. Such a connection is permanently capped to viewer, so accepting the
// action would leave the room with an owner that cannot manage it.
func TestReadonlyMemberCannotBecomeOwnerOrEditor(t *testing.T) {
	s := newStack(t, nil)
	room := s.createRoom("readonly grant")

	owner := s.dial(room.Code, "c_ro_owner", "owner", room.EditorKey, "")
	owner.join(room.Code, "c_ro_owner", 0, 0)

	token, _, err := s.signer.Issue(room.Code, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("issue readonly token: %v", err)
	}
	readonly := s.dial(room.Code, "c_ro_guest", "guest", "", token)
	readonlyJoined := readonly.join(room.Code, "c_ro_guest", 0, 0)
	if readonlyJoined.You.Role != protocol.RoleViewer {
		t.Fatalf("readonly role = %q, want viewer", readonlyJoined.You.Role)
	}

	owner.send(protocol.TypeRoomGrant, room.Code, "c_ro_owner", protocol.GrantPayload{
		TargetClientID: readonlyJoined.You.ClientID,
		Role:           protocol.RoleEditor,
	})
	expectErrorCode(t, owner, protocol.CodeTargetReadonly)

	owner.send(protocol.TypeRoomTransfer, room.Code, "c_ro_owner", protocol.TransferPayload{
		TargetClientID: readonlyJoined.You.ClientID,
	})
	expectErrorCode(t, owner, protocol.CodeTargetReadonly)

	// The owner must still be the owner after both rejections.
	owner.send(protocol.TypeRoomGrant, room.Code, "c_ro_owner", protocol.GrantPayload{
		TargetClientID: "c_missing_target",
		Role:           protocol.RoleEditor,
	})
	expectErrorCode(t, owner, protocol.CodeTargetOffline)
}

// expectErrorCode reads until an error frame with the wanted code arrives.
func expectErrorCode(t *testing.T, c *client, want protocol.ErrCode) protocol.ErrorPayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		env, err := c.read(time.Until(deadline))
		if err != nil {
			t.Fatalf("waiting for %s: %v", want, err)
		}
		if env.Type != protocol.TypeError {
			continue
		}
		var payload protocol.ErrorPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			t.Fatalf("error payload: %v", err)
		}
		if payload.Code == want {
			return payload
		}
	}
	t.Fatalf("did not receive error %s", want)
	return protocol.ErrorPayload{}
}
