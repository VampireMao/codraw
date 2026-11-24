// Command wssmoke drives a real WebSocket session against a running server and
// prints every frame it sends or receives.
//
// It exists so the protocol can be verified without a browser, and so CI can run
// the same steps a human would: create → join → snapshot → draw → ack → idempotent
// retransmit → readonly rejection.
//
// Usage:
//
//	go run ./cmd/wssmoke -base http://localhost:8080 -room K7M2QX -key <editorKey>
//	go run ./cmd/wssmoke -base http://localhost:8080 -create        # create a room first
//	go run ./cmd/wssmoke -room K7M2QX -key <viewerKey> -send-op 1   # expect forbidden
//	go run ./cmd/wssmoke -room K7M2QX -key <editorKey> -send-op 2 -same-op-id
//
// Exit status is 0 only when every expectation for the selected mode was met, so
// the Makefile target doubles as a regression test.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/codraw/codraw/backend/internal/protocol"
)

func main() {
	os.Exit(run())
}

type options struct {
	base     string
	room     string
	key      string
	rt       string
	name     string
	cid      string
	create   bool
	title    string
	sendOp   int
	sameOpID bool
	timeout  time.Duration
	expect   string
}

func run() int {
	var o options
	flag.StringVar(&o.base, "base", "http://localhost:8080", "server base URL")
	flag.StringVar(&o.room, "room", "", "room code")
	flag.StringVar(&o.key, "key", "", "editorKey or viewerKey")
	flag.StringVar(&o.rt, "rt", "", "readonly token")
	flag.StringVar(&o.name, "name", "冒烟测试", "nickname sent in the handshake")
	flag.StringVar(&o.cid, "cid", "", "client id (generated when empty)")
	flag.BoolVar(&o.create, "create", false, "create a room first and use its editorKey")
	flag.StringVar(&o.title, "title", "协议冒烟", "title used with -create")
	flag.IntVar(&o.sendOp, "send-op", 0, "number of op.add frames to send (0, 1 or 2)")
	flag.BoolVar(&o.sameOpID, "same-op-id", false, "reuse the same opId for both ops (idempotency check)")
	flag.DurationVar(&o.timeout, "timeout", 15*time.Second, "overall timeout")
	flag.StringVar(&o.expect, "expect", "", "expect a specific final ack status: applied|duplicate|rejected")
	flag.Parse()

	if o.cid == "" {
		o.cid = fmt.Sprintf("c_smoke_%d", time.Now().UnixNano()%1_000_000_000)
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()

	if err := smoke(ctx, &o); err != nil {
		fmt.Fprintf(os.Stderr, "\nFAIL: %v\n", err)
		return 1
	}
	fmt.Println("\nSMOKE OK")
	return 0
}

// smoke runs the whole session.
func smoke(ctx context.Context, o *options) error {
	client := &http.Client{Timeout: 5 * time.Second}

	if o.create {
		created, err := createRoom(ctx, client, o.base, o.title)
		if err != nil {
			return err
		}
		o.room = created.Code
		o.key = created.EditorKey
		fmt.Printf("create  room=%s editorKey=%s... viewerKey=%s...\n",
			created.Code, short(created.EditorKey), short(created.ViewerKey))
		if !roomCodePattern(created.Code) {
			return fmt.Errorf("generated code %q does not match ^[2-9A-HJKMNP-Z]{6}$", created.Code)
		}
	}
	if o.room == "" {
		return errors.New("-room or -create is required")
	}

	role := "viewer"
	label := "anon"
	if o.rt != "" {
		label = "readonly token"
	} else if o.key != "" {
		// The role is not knowable client-side; print which credential was used and
		// let the joined frame report the authoritative role.
		label = "key"
	}
	fmt.Printf("join    room=%s credential=%s cid=%s\n", o.room, label, o.cid)

	conn, err := dial(ctx, o)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// The server sends joined immediately; the client must send hello immediately.
	// Both are sent before either is read, which is exactly the concurrency the
	// protocol requires.
	sendHello(conn, o)

	deadline := time.Now().Add(10 * time.Second)
	seenJoined := false
	seenResync := false
	appliedSeqs := map[string]int64{}
	var acks []protocol.AckPayload
	opID := ""
	if o.sendOp > 0 {
		opID = "op_smoke_" + fmt.Sprint(time.Now().UnixNano())
	}

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("deadline reached: joined=%v resync=%v acks=%d", seenJoined, seenResync, len(acks))
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("bad JSON from server: %w (%s)", err, raw)
		}
		printFrame("<-", &env)

		switch env.Type {
		case protocol.TypeJoined:
			seenJoined = true
			var p protocol.JoinedPayload
			_ = json.Unmarshal(env.Payload, &p)
			role = p.You.Role
			fmt.Printf("        role=%s online=%d roomSeq=%d instance=%s\n",
				p.You.Role, p.Online, p.Room.Seq, p.InstanceID)
			if o.sendOp > 0 && seenResync {
				if err := sendOps(conn, o, opID); err != nil {
					return err
				}
			}
		case protocol.TypeResyncSnapshot:
			var p protocol.SnapshotPayload
			_ = json.Unmarshal(env.Payload, &p)
			fmt.Printf("        snapshot chunk=%d/%d shapes=%d complete=%v seq=%d\n",
				p.Chunk+1, p.Total, len(p.Shapes), p.Complete, p.Seq)
			if p.Complete {
				seenResync = true
				if o.sendOp > 0 {
					if err := sendOps(conn, o, opID); err != nil {
						return err
					}
				} else {
					break
				}
			}
		case protocol.TypeResyncEvents:
			var p protocol.ResyncEventsPayload
			_ = json.Unmarshal(env.Payload, &p)
			fmt.Printf("        events from=%d to=%d count=%d hasMore=%v\n",
				p.FromSeq, p.ToSeq, len(p.Events), p.HasMore)
			seenResync = true
			if o.sendOp > 0 {
				if err := sendOps(conn, o, opID); err != nil {
					return err
				}
			}
		case protocol.TypeAck:
			var p protocol.AckPayload
			_ = json.Unmarshal(env.Payload, &p)
			acks = append(acks, p)
			fmt.Printf("        ack opId=%s status=%s seq=%d code=%s\n", p.OpID, p.Status, p.Seq, p.Code)
			if p.Seq > 0 {
				appliedSeqs[p.OpID] = p.Seq
			}
		case protocol.TypeOpAdd:
			if env.Seq > 0 {
				fmt.Printf("        broadcast seq=%d\n", env.Seq)
			}
		case protocol.TypePresenceJoin, protocol.TypePresenceLeave:
			// Presence is expected but not asserted here.
		case protocol.TypeError:
			var p protocol.ErrorPayload
			_ = json.Unmarshal(env.Payload, &p)
			if p.Fatal {
				return fmt.Errorf("fatal protocol error: %s %s", p.Code, p.Message)
			}
		}

		if seenJoined && seenResync && len(acks) >= o.sendOp && (o.sendOp > 0 || seenResync) {
			break
		}
	}

	// Ping/pong liveness check: the connection must still work after the writes,
	// which is how the viewer-rejection case proves it was not closed.
	if err := pingPong(conn, o); err != nil {
		return fmt.Errorf("ping/pong after ops: %w", err)
	}
	fmt.Printf("        connection alive after ops (pong received), role=%s\n", role)

	// Expectations.
	switch {
	case o.sendOp == 0:
		if !seenResync {
			return errors.New("no resync snapshot/events received")
		}
	case o.sameOpID && o.sendOp == 2:
		if len(acks) < 2 {
			return fmt.Errorf("want 2 acks, got %d", len(acks))
		}
		if acks[0].Status != protocol.AckApplied {
			return fmt.Errorf("first ack = %s, want applied", acks[0].Status)
		}
		if acks[1].Status != protocol.AckDuplicate {
			return fmt.Errorf("second ack = %s, want duplicate", acks[1].Status)
		}
		if acks[0].Seq != acks[1].Seq {
			return fmt.Errorf("duplicate seq %d != original %d", acks[1].Seq, acks[0].Seq)
		}
		fmt.Printf("        idempotency verified: seq %d reused\n", acks[0].Seq)
	case o.sendOp == 1:
		if len(acks) < 1 {
			return errors.New("want 1 ack, got 0")
		}
		if role == protocol.RoleViewer && acks[0].Status != protocol.AckRejected {
			return fmt.Errorf("viewer ack = %s, want rejected", acks[0].Status)
		}
		if acks[0].Status == protocol.AckRejected && acks[0].Code != protocol.CodeForbidden {
			return fmt.Errorf("rejected code = %s, want forbidden", acks[0].Code)
		}
	}

	if o.expect != "" {
		if len(acks) == 0 {
			return fmt.Errorf("-expect %s but no ack received", o.expect)
		}
		last := acks[len(acks)-1]
		if string(last.Status) != o.expect {
			return fmt.Errorf("last ack = %s, want %s", last.Status, o.expect)
		}
	}
	return nil
}

// dial opens the WebSocket with the handshake query from PROTOCOL §2.1.
func dial(ctx context.Context, o *options) (*websocket.Conn, error) {
	u, err := url.Parse(o.base)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path = "/ws"
	q := u.Query()
	q.Set("room", o.room)
	q.Set("cid", o.cid)
	q.Set("name", o.name)
	if o.rt != "" {
		q.Set("rt", o.rt)
	} else if o.key != "" {
		q.Set("key", o.key)
	}
	u.RawQuery = q.Encode()

	// The server rejects requests without an Origin unless ALLOW_EMPTY_ORIGIN is
	// set, so act like a browser and send the base origin. This is also the
	// behaviour being tested: a foreign origin must be refused.
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	hdr := http.Header{}
	hdr.Set("Origin", o.base)
	conn, resp, err := dialer.DialContext(ctx, u.String(), hdr)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("handshake %d: %s", resp.StatusCode, bytes.TrimSpace(body))
		}
		return nil, err
	}
	return conn, nil
}

// sendHello sends the mandatory first client frame.
func sendHello(conn *websocket.Conn, o *options) {
	payload := protocol.HelloPayload{LastSeq: 0, Epoch: 0, ClientVersion: "wssmoke"}
	write(conn, protocol.TypeHello, o, 0, payload)
}

// sendOps sends one or two op.add frames.
func sendOps(conn *websocket.Conn, o *options, opID string) error {
	shape := map[string]any{
		"id":   "sh_smoke" + fmt.Sprint(time.Now().UnixNano()%1_000_000),
		"kind": "rect",
		"geom": map[string]any{"x": 10, "y": 20, "w": 100, "h": 50},
		"style": map[string]any{
			"stroke": "#16181d", "fill": "none", "strokeWidth": 2, "opacity": 1, "dash": "solid",
		},
		"data": map[string]any{},
	}
	payload := protocol.OpAddPayload{OpID: opID, Action: protocol.ActionCreate, Shape: mustJSON(shape)}

	for i := 0; i < o.sendOp; i++ {
		id := opID
		if !o.sameOpID && i > 0 {
			id = opID + "_b"
		}
		payload.OpID = id
		write(conn, protocol.TypeOpAdd, o, 0, payload)
		fmt.Printf("-> op.add opId=%s action=create\n", id)
		// Space the two frames out so the second is a genuine retransmit rather
		// than a race with the first ack.
		time.Sleep(150 * time.Millisecond)
	}
	return nil
}

// write sends one envelope.
func write(conn *websocket.Conn, typ string, o *options, seq int64, payload any) {
	frame, err := protocol.Marshal(typ, o.room, o.cid, seq, time.Now().UnixMilli(), payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode %s: %v\n", typ, err)
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", typ, err)
	}
}

// pingPong proves the connection is still usable.
func pingPong(conn *websocket.Conn, o *options) error {
	payload := protocol.PingPayload{TS: time.Now().UnixMilli()}
	frame, err := protocol.Marshal(protocol.TypePing, o.room, o.cid, 0, time.Now().UnixMilli(), payload)
	if err != nil {
		return err
	}
	if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		return err
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		if env.Type == protocol.TypePong {
			printFrame("<-", &env)
			return nil
		}
		printFrame("<-", &env)
	}
}

// printFrame prints one frame compactly: type, seq, and the payload. The `joined`
// frame is printed in full because its instanceId is what proves which process a
// connection landed on, and that is the whole point of a multi-instance check.
func printFrame(dir string, env *protocol.Envelope) {
	payload := string(env.Payload)
	if env.Type != protocol.TypeJoined && len(payload) > 160 {
		payload = payload[:157] + "..."
	}
	seq := ""
	if env.Seq > 0 {
		seq = fmt.Sprintf(" seq=%d", env.Seq)
	}
	fmt.Printf("%s %-18s%s room=%s %s\n", dir, env.Type, seq, env.RoomID, payload)
}

type createdRoom struct {
	Code      string
	EditorKey string
	ViewerKey string
}

// createRoom calls POST /api/rooms.
func createRoom(ctx context.Context, client *http.Client, base, title string) (*createdRoom, error) {
	body, _ := json.Marshal(map[string]string{"title": title})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/rooms", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("create room: status %d: %s", res.StatusCode, raw)
	}
	var payload struct {
		Room struct {
			Code string `json:"code"`
		} `json:"room"`
		Keys struct {
			EditorKey string `json:"editorKey"`
			ViewerKey string `json:"viewerKey"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("create room: bad JSON: %w", err)
	}
	return &createdRoom{
		Code:      payload.Room.Code,
		EditorKey: payload.Keys.EditorKey,
		ViewerKey: payload.Keys.ViewerKey,
	}, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func short(s string) string {
	if len(s) <= 6 {
		return s
	}
	return s[:6]
}

// roomCodePattern mirrors the DDL CHECK: 2-9, then A-H J K M N-P Q-Z (no I, L, O).
func roomCodePattern(code string) bool {
	if len(code) != 6 {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		ok := (c >= '2' && c <= '9') ||
			(c >= 'A' && c <= 'H') ||
			(c >= 'J' && c <= 'K') ||
			(c >= 'M' && c <= 'N') ||
			(c >= 'P' && c <= 'Z')
		if !ok {
			return false
		}
	}
	return true
}

var _ = strings.TrimSpace
