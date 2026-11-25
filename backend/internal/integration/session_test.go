package integration

import (
	"testing"

	"github.com/codraw/codraw/backend/internal/protocol"
)

// TestSessionIdentityCannotBeSpoofed is the regression test for the clientId
// privilege escalation: the room resolves owner/editor by clientId, and the
// clientId used to be whatever the client put in the URL. With sessions enabled,
// a tokenless connection gets a server-generated id, so presenting the owner's
// id must not inherit ownership.
func TestSessionIdentityCannotBeSpoofed(t *testing.T) {
	s := newStack(t, nil)
	s.enableSessions()

	created := s.createRoom("session-identity")
	const requestedCID = "c_owner_0000000001"

	owner := s.dial(created.Code, requestedCID, "owner", "", "")
	joined := owner.join(created.Code, requestedCID, 0, 0)
	if joined.You.Role != protocol.RoleOwner {
		t.Fatalf("first entrant role = %q, want owner", joined.You.Role)
	}
	ownerCID := joined.You.ClientID
	if ownerCID == requestedCID {
		t.Fatalf("server trusted the client-supplied cid %q", requestedCID)
	}
	if joined.SessionToken == "" {
		t.Fatal("joined did not carry a session token")
	}

	// The attack: present the owner's cid, no token. The server must generate a
	// different identity; otherwise this connection is a second owner tab.
	attacker := s.dial(created.Code, ownerCID, "attacker", "", "")
	attackJoined := attacker.join(created.Code, ownerCID, 0, 0)
	if attackJoined.You.ClientID == ownerCID {
		t.Fatalf("spoofed cid %q was accepted as the owner", ownerCID)
	}
	if attackJoined.You.Role != protocol.RoleViewer {
		t.Fatalf("tokenless spoof got role %q, want viewer", attackJoined.You.Role)
	}

	// A valid token keeps the identity (this is what a page refresh uses).
	revived := s.dialWithSession(created.Code, "ignored-by-server", "owner-refresh", joined.SessionToken)
	rejoined := revived.join(created.Code, "ignored-by-server", 0, 0)
	if rejoined.You.ClientID != ownerCID {
		t.Fatalf("session reconnect cid = %q, want %q", rejoined.You.ClientID, ownerCID)
	}
	if rejoined.You.Role != protocol.RoleOwner {
		t.Fatalf("session reconnect role = %q, want owner", rejoined.You.Role)
	}

	// A token minted for another room must not be replayable.
	other := s.createRoom("session-other")
	otherOwner := s.dial(other.Code, "c_other_owner_0001", "other", "", "")
	otherJoined := otherOwner.join(other.Code, "c_other_owner_0001", 0, 0)
	if otherJoined.You.Role != protocol.RoleOwner {
		t.Fatalf("other room first entrant role = %q", otherJoined.You.Role)
	}
	crossRoom := s.dialWithSession(other.Code, ownerCID, "cross-room", joined.SessionToken)
	crossJoined := crossRoom.join(other.Code, ownerCID, 0, 0)
	if crossJoined.You.ClientID == ownerCID {
		t.Fatalf("session token for %s was accepted by %s", created.Code, other.Code)
	}
	if crossJoined.You.Role != protocol.RoleViewer {
		t.Fatalf("cross-room token got role %q, want viewer", crossJoined.You.Role)
	}
}
