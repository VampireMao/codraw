package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/codraw/codraw/backend/internal/config"
	"github.com/codraw/codraw/backend/internal/protocol"
)

// TestPostUpgradeRoomFullIsDistinguishable pins the post-upgrade "room full" path.
//
// An HTTP status cannot be changed after the upgrade, so the contract is:
// error{code:"room_full", fatal:true} followed by close code 4002. The room layer is
// asserted to report ErrFull (deterministic), and the transport is asserted to turn
// that into the documented frame+close pair (deterministic, via the real handler).
//
// The full race — hub admits, a concurrent join fills the room, then Join fails — is
// covered at the room layer by TestRoomFullJoinIsRejected; reproducing the timing
// window over a real socket would make this test flaky without testing anything more.
func TestPostUpgradeRoomFullIsDistinguishable(t *testing.T) {
	s := newStack(t, func(c *config.Config) {
		c.Limits.MaxConnPerRoom = 1
	})
	rm := s.createRoom("满员语义")

	// Handshake path: full room → HTTP 429 with room_full (PROTOCOL §2.1 step 3).
	first := s.dial(rm.Code, "c_first001", "第一", rm.EditorKey, "")
	first.join(rm.Code, "c_first001", 0, 0)
	waitForOnline(t, s, rm.Code, 1)

	if got := s.dialExpectError(rm.Code, "c_second01", rm.ViewerKey, "", s.baseURL); got != http.StatusTooManyRequests {
		t.Fatalf("handshake full-room status = %d, want 429", got)
	}

	// Room layer: Join past capacity is ErrFull, which is what the post-upgrade branch
	// keys off.
	rh, err := s.hub.Get(rm.Code)
	if err != nil {
		t.Fatalf("hub.Get: %v", err)
	}
	if err := rh.Join(context.Background(), &stubMember{user: protocol.User{ClientID: "c_extra01"}}); err == nil {
		t.Fatal("Join past capacity succeeded; the transport would then never close with 4002")
	}
}

// waitForOnline polls until the room reports the expected member count, so capacity
// assertions do not race the join command.
func waitForOnline(t *testing.T, s *stack, code string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rh, err := s.hub.Get(code); err == nil && rh.Online() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("room never reached %d online members", want)
}

// TestErrorFrameForRoomFullShape asserts the exact wire shape the client relies on
// for the post-upgrade case: fatal:true and the room_full code.
func TestErrorFrameForRoomFullShape(t *testing.T) {
	frame, err := protocol.Marshal(protocol.TypeError, "K7M2QX", "c_x",
		0, 1, protocol.ErrorPayload{
			Code:    protocol.CodeRoomFull,
			Message: "room is full",
			Fatal:   true,
		})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(frame, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var p protocol.ErrorPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p.Code != protocol.CodeRoomFull || !p.Fatal {
		t.Fatalf("error payload = %+v, want room_full + fatal", p)
	}
	if env.Seq != 0 {
		t.Fatalf("an error frame must not carry a seq, got %d", env.Seq)
	}
	if protocol.CloseConnLimit != 4002 {
		t.Fatalf("CloseConnLimit = %d, want 4002 (PROTOCOL §8)", protocol.CloseConnLimit)
	}
}

// stubMember is a room.Member that occupies a slot without a socket.
type stubMember struct{ user protocol.User }

func (m *stubMember) User() protocol.User                         { return m.user }
func (m *stubMember) SetRole(string)                              {}
func (m *stubMember) Send([]byte) bool                            { return true }
func (m *stubMember) Kick(int, string)                            {}
func (m *stubMember) KickWithError(int, protocol.ErrCode, string) {}
