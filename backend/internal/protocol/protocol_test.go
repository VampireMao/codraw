package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEnvelopeValidation(t *testing.T) {
	cases := []struct {
		name string
		env  Envelope
		want ErrCode
	}{
		{"valid", Envelope{V: 1, Type: "ping", RoomID: "K7M2QX", Payload: json.RawMessage(`{}`)}, ""},
		{"missing v defaults to 1", Envelope{V: 0, Type: "ping", RoomID: "K7M2QX", Payload: json.RawMessage(`{}`)}, ""},
		{"future version", Envelope{V: 2, Type: "ping", RoomID: "K7M2QX", Payload: json.RawMessage(`{}`)}, CodeUnsupportedVer},
		{"missing type", Envelope{V: 1, RoomID: "K7M2QX", Payload: json.RawMessage(`{}`)}, CodeBadRequest},
		{"missing room", Envelope{V: 1, Type: "ping", Payload: json.RawMessage(`{}`)}, CodeBadRequest},
		{"null payload", Envelope{V: 1, Type: "ping", RoomID: "K7M2QX", Payload: json.RawMessage(`null`)}, CodeBadRequest},
		{"empty payload", Envelope{V: 1, Type: "ping", RoomID: "K7M2QX"}, CodeBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateEnvelope(&c.env)
			if c.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			pe, ok := AsError(err)
			if !ok {
				t.Fatalf("error is not a *protocol.Error: %v", err)
			}
			if pe.Code != c.want {
				t.Fatalf("code = %q, want %q", pe.Code, c.want)
			}
		})
	}
}

func TestMarshalOnlyPutsSeqOnPersistentTypes(t *testing.T) {
	// op.add is persistent: the envelope carries the seq.
	frame, err := Marshal(TypeOpAdd, "K7M2QX", "c_1", 42, 1000, map[string]any{"opId": "x"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(frame), `"seq":42`) {
		t.Fatalf("op.add is missing the envelope seq: %s", frame)
	}

	// ack is NOT persistent: the confirmed seq lives in the payload instead
	// (CONTRACT-CHECKLIST §8 rule 1).
	frame, err = Marshal(TypeAck, "K7M2QX", "c_1", 42, 1000, AckPayload{OpID: "x", Status: AckApplied, Seq: 42})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var env Envelope
	if err := json.Unmarshal(frame, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Seq != 0 {
		t.Fatalf("ack envelope carries seq=%d; it must be in the payload only", env.Seq)
	}
	var ack AckPayload
	if err := json.Unmarshal(env.Payload, &ack); err != nil {
		t.Fatalf("ack payload: %v", err)
	}
	if ack.Seq != 42 {
		t.Fatalf("ack payload seq = %d, want 42", ack.Seq)
	}

	// A non-persistent message with a nonzero seq argument must ignore it.
	frame, _ = Marshal(TypePong, "K7M2QX", "c_1", 7, 1000, PongPayload{TS: 1})
	if strings.Contains(string(frame), `"seq"`) {
		t.Fatalf("pong must not carry a seq: %s", frame)
	}
}

func TestMarshalFillsRequiredFields(t *testing.T) {
	frame, err := Marshal(TypeJoined, "K7M2QX", "c_abc", 0, 1732358400000, JoinedPayload{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(frame, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"v", "type", "roomId", "clientId", "ts", "payload"} {
		if _, ok := m[field]; !ok {
			t.Errorf("envelope is missing %q: %s", field, frame)
		}
	}
	if m["v"] != float64(1) {
		t.Errorf("v = %v, want 1", m["v"])
	}
}

func TestOpAddValidate(t *testing.T) {
	validShape := json.RawMessage(`{"id":"sh_abc123","kind":"rect"}`)
	cases := []struct {
		name string
		p    OpAddPayload
		want ErrCode
	}{
		{"create ok", OpAddPayload{OpID: "op_12345678", Action: ActionCreate, Shape: validShape}, ""},
		{"update ok", OpAddPayload{OpID: "op_12345678", Action: ActionUpdate, Shape: validShape}, ""},
		{"delete ok", OpAddPayload{OpID: "op_12345678", Action: ActionDelete, ID: "sh_abc123"}, ""},
		{"append ok", OpAddPayload{OpID: "op_12345678", Action: ActionAppend, ID: "sh_abc123", Points: [][]float64{{1, 2}}}, ""},
		{"bad opId", OpAddPayload{OpID: "x", Action: ActionCreate, Shape: validShape}, CodeInvalidOp},
		{"missing shape", OpAddPayload{OpID: "op_12345678", Action: ActionCreate}, CodeInvalidOp},
		{"unknown action", OpAddPayload{OpID: "op_12345678", Action: "move"}, CodeInvalidOp},
		{"delete without id", OpAddPayload{OpID: "op_12345678", Action: ActionDelete}, CodeInvalidOp},
		{"append without points", OpAddPayload{OpID: "op_12345678", Action: ActionAppend, ID: "sh_abc123"}, CodeInvalidOp},
		{"append too many points", OpAddPayload{
			OpID: "op_12345678", Action: ActionAppend, ID: "sh_abc123",
			Points: make([][]float64, MaxAppendPoints+1),
		}, CodeInvalidOp},
		{"point out of range", OpAddPayload{
			OpID: "op_12345678", Action: ActionAppend, ID: "sh_abc123",
			Points: [][]float64{{2e6, 0}},
		}, CodeInvalidOp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.p.Validate(32768)
			if c.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			pe, ok := AsError(err)
			if !ok {
				t.Fatalf("not a protocol error: %v", err)
			}
			if pe.Code != c.want {
				t.Fatalf("code = %q, want %q", pe.Code, c.want)
			}
		})
	}
}

func TestShapeKindEnumeration(t *testing.T) {
	want := []string{"freehand", "rect", "ellipse", "arrow", "text"}
	if len(ShapeKinds) != len(want) {
		t.Fatalf("ShapeKinds = %v", ShapeKinds)
	}
	for i := range want {
		if ShapeKinds[i] != want[i] {
			t.Fatalf("ShapeKinds[%d] = %q, want %q", i, ShapeKinds[i], want[i])
		}
	}
}

func TestShapeIDs(t *testing.T) {
	cases := []struct {
		id string
		ok bool
	}{
		{"sh_abc123", true},
		{"sh_aB3dE9fGh1Kl", true},
		{"sh_abc", false},                        // too short after the prefix
		{"abc123456", false},                     // missing prefix
		{"sh_" + strings.Repeat("a", 40), false}, // too long
		{"sh_abc 123", false},                    // illegal character
	}
	for _, c := range cases {
		if got := ValidateShapeID(c.id); got != c.ok {
			t.Errorf("ValidateShapeID(%q) = %v, want %v", c.id, got, c.ok)
		}
	}
}

func TestSanitizeNickname(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  小林  ", "小林"},
		{"a\x00b\x07c", "abc"},
		{"a\nb", "a b"},
		{"一二三四五", "一二三四五"},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := SanitizeNickname(c.in, 24); got != c.want {
			t.Errorf("SanitizeNickname(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := SanitizeNickname("abcdefgh", 4); got != "abcd" {
		t.Errorf("truncation = %q", got)
	}
	if got := SanitizeNickname("一二三四五", 3); got != "一二三" {
		t.Errorf("rune truncation = %q", got)
	}
}

func TestTruncateTitle(t *testing.T) {
	if got := TruncateTitle("", 60); got != "未命名白板" {
		t.Fatalf("empty title = %q", got)
	}
	if got := TruncateTitle("  架构评审  ", 60); got != "架构评审" {
		t.Fatalf("title = %q", got)
	}
	long := strings.Repeat("字", 100)
	if got := TruncateTitle(long, 60); len([]rune(got)) != 60 {
		t.Fatalf("truncated length = %d, want 60", len([]rune(got)))
	}
}

func TestAllTypesIsTheUnion(t *testing.T) {
	// Every client type must be in AllTypes, and AllTypes must have no duplicates.
	seen := map[string]bool{}
	for _, typ := range AllTypes {
		if seen[typ] {
			t.Errorf("duplicate type %q in AllTypes", typ)
		}
		seen[typ] = true
	}
	for typ := range ClientTypes {
		if !seen[typ] {
			t.Errorf("client type %q is missing from AllTypes", typ)
		}
	}
	// Persistent messages per docs/PROTOCOL.md §2.2 plus `member.role`, which
	// changes who may write and therefore must be resyncable
	// (docs/PERMISSIONS.md §3.1).
	if len(PersistentTypes) != 3 {
		t.Fatalf("PersistentTypes = %v, want op.add, room.meta and member.role", PersistentTypes)
	}
	if !PersistentTypes[TypeOpAdd] || !PersistentTypes[TypeRoomMeta] || !PersistentTypes[TypeMemberRole] {
		t.Fatalf("PersistentTypes = %v", PersistentTypes)
	}
	if PersistentTypes[TypeAck] {
		t.Fatal("ack must not be persistent: its seq lives in the payload")
	}
	if !ClientTypes[TypeRoomGrant] || !ClientTypes[TypeRoomTransfer] {
		t.Fatalf("room.grant / room.transfer must be accepted from clients: %v", ClientTypes)
	}
}

func TestErrorCodesCoverTheProtocolTable(t *testing.T) {
	// Every code in docs/PROTOCOL.md §8 must exist here, otherwise a handler cannot
	// emit it without string-typing.
	want := []ErrCode{
		CodeBadRequest, CodeUnsupportedVer, CodeUnsupportedType, CodeUnauthenticated,
		CodeForbidden, CodeInvalidOp, CodeRateLimited, CodeMessageTooLarge,
		CodeShapeNotFound, CodeShapeDeleted, CodeShapeTooLarge, CodeDuplicateShape,
		CodeRoomFullShapes, CodeRoomNotFound, CodeRoomFull, CodeServerBusy,
		CodeSnapshotTooLarge, CodeInternal,
		// docs/PERMISSIONS.md §8
		CodeTargetOffline, CodeInvalidGrant,
	}
	for _, c := range want {
		if c == "" {
			t.Errorf("a protocol error code is empty")
		}
	}
	if len(FatalCodes) == 0 {
		t.Error("FatalCodes must not be empty")
	}
}

func TestAsErrorOnNilAndForeignError(t *testing.T) {
	if _, ok := AsError(nil); ok {
		t.Fatal("AsError(nil) reported ok")
	}
	if _, ok := AsError(errors.New("plain")); ok {
		t.Fatal("AsError(plain) reported ok")
	}
}

func TestCursorValidate(t *testing.T) {
	if err := (&CursorPayload{X: 1, Y: 2}).Validate(); err != nil {
		t.Fatalf("valid cursor rejected: %v", err)
	}
	if err := (&CursorPayload{X: 2e6, Y: 0}).Validate(); err == nil {
		t.Fatal("out-of-range cursor accepted")
	}
	if err := (*CursorPayload)(nil).Validate(); err == nil {
		t.Fatal("nil cursor accepted")
	}
}
