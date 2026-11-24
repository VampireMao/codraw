// Package protocol is the Go mirror of docs/PROTOCOL.md v1.
//
// It has ZERO internal dependencies by design: ws, room, hub and store all
// import it, so it must never import any of them. Everything here is a plain
// data structure plus pure validation functions — no I/O, no logging, no
// goroutines.
//
// Two rules from the contract are load-bearing and repeated in code comments
// where they are enforced:
//
//   - The envelope is {v, type, seq?, roomId, clientId, ts, payload}. Only
//     persistent messages (op.add, room.meta) carry seq. ack carries the
//     confirmed seq inside payload.
//   - Unknown fields are always ignored (encoding/json default), and unknown
//     `type` values produce a non-fatal error frame.
package protocol

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Version is the only supported protocol version.
const Version = 1

// Message types, client → server (docs/PROTOCOL.md §3).
const (
	TypeHello         = "hello"
	TypeOpAdd         = "op.add"
	TypePresenceCur   = "presence.cursor"
	TypeResyncRequest = "resync.request"
	TypePing          = "ping"
	TypeBye           = "bye"
	TypeRoomGrant     = "room.grant"
	TypeRoomTransfer  = "room.transfer"
)

// Message types, server → client (docs/PROTOCOL.md §4).
const (
	TypeJoined         = "joined"
	TypePresenceJoin   = "presence.join"
	TypePresenceLeave  = "presence.leave"
	TypeAck            = "ack"
	TypeResyncSnapshot = "resync.snapshot"
	TypeResyncEvents   = "resync.events"
	TypeRoomMeta       = "room.meta"
	TypeMemberRole     = "member.role"
	TypeError          = "error"
	TypePong           = "pong"
)

// AllTypes is the closed set used to validate `type` and to bound the metrics
// label space.
var AllTypes = []string{
	TypeHello, TypeOpAdd, TypePresenceCur, TypeResyncRequest, TypePing, TypeBye,
	TypeRoomGrant, TypeRoomTransfer,
	TypeJoined, TypePresenceJoin, TypePresenceLeave, TypeAck,
	TypeResyncSnapshot, TypeResyncEvents, TypeRoomMeta, TypeMemberRole, TypeError, TypePong,
}

// ClientTypes are the only messages a client may send.
var ClientTypes = map[string]bool{
	TypeHello:         true,
	TypeOpAdd:         true,
	TypePresenceCur:   true,
	TypeResyncRequest: true,
	TypePing:          true,
	TypeBye:           true,
	TypeRoomGrant:     true,
	TypeRoomTransfer:  true,
}

// PersistentTypes are the messages that consume a seq.
var PersistentTypes = map[string]bool{
	TypeOpAdd:      true,
	TypeRoomMeta:   true,
	TypeMemberRole: true,
}

// ErrCode is a protocol error code from docs/PROTOCOL.md §8. It is a string type
// so it can be compared to literals without conversion.
type ErrCode string

// Error codes (docs/PROTOCOL.md §8). The set is closed; codes are also used as
// metric label values, so an unknown string would create a new time series.
const (
	CodeBadRequest       ErrCode = "bad_request"
	CodeUnsupportedVer   ErrCode = "unsupported_version"
	CodeUnsupportedType  ErrCode = "unsupported_type"
	CodeUnauthenticated  ErrCode = "unauthenticated"
	CodeForbidden        ErrCode = "forbidden"
	CodeInvalidOp        ErrCode = "invalid_op"
	CodeRateLimited      ErrCode = "rate_limited"
	CodeMessageTooLarge  ErrCode = "message_too_large"
	CodeShapeNotFound    ErrCode = "shape_not_found"
	CodeShapeDeleted     ErrCode = "shape_deleted"
	CodeShapeTooLarge    ErrCode = "shape_too_large"
	CodeDuplicateShape   ErrCode = "duplicate_shape"
	CodeRoomFullShapes   ErrCode = "room_full_shapes"
	CodeRoomNotFound     ErrCode = "room_not_found"
	CodeRoomFull         ErrCode = "room_full"
	CodeServerBusy       ErrCode = "server_busy"
	CodeSnapshotTooLarge ErrCode = "snapshot_too_large"
	CodeInternal         ErrCode = "internal"
	CodeNotLeader        ErrCode = "not_leader"
	CodeNotFound         ErrCode = "not_found"
	CodeUnauthorized     ErrCode = "unauthorized"
	CodeMethodNotAllowed ErrCode = "method_not_allowed"
	CodeNotImplemented   ErrCode = "not_implemented"
	CodeTooManyRequests  ErrCode = "too_many_requests"
	// Permission-model codes (docs/PERMISSIONS.md §8).
	CodeTargetOffline ErrCode = "target_offline"
	CodeInvalidGrant  ErrCode = "invalid_grant"
)

// FatalCodes are the codes that must terminate the connection.
var FatalCodes = map[ErrCode]bool{
	CodeUnsupportedVer:  true,
	CodeUnauthenticated: true,
	CodeRoomNotFound:    true,
	CodeRoomFull:        true,
	CodeServerBusy:      true,
}

// Roles.
//
// owner implies editor: the server checks canWrite = owner || editor
// (docs/PERMISSIONS.md §1, §5).
const (
	RoleOwner  = "owner"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// CanWrite reports whether a role may submit op.add.
func CanWrite(role string) bool { return role == RoleOwner || role == RoleEditor }

// ValidGrantRole reports whether role may appear in room.grant. owner is
// deliberately excluded: ownership only changes through room.transfer
// (docs/PERMISSIONS.md §3.1).
func ValidGrantRole(role string) bool { return role == RoleEditor || role == RoleViewer }

// Close codes (docs/PROTOCOL.md §8).
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	ClosePolicyViolation = 1008
	CloseMessageTooBig   = 1009
	CloseInternalError   = 1011
	CloseHeartbeat       = 4000
	CloseSlowConsumer    = 4001
	CloseConnLimit       = 4002
	// CloseForbidden terminates a connection that attempted an owner-only
	// action while not being the owner (docs/PERMISSIONS.md §8).
	CloseForbidden = 4003
)

// Envelope is the single wire shape for both directions.
//
// Payload is json.RawMessage so adding a message type never touches the
// envelope, and so the room actor can forward a payload it did not fully parse
// (op.add broadcast) without re-encoding. It must never be null on the wire; use
// `{}` for messages with no body.
type Envelope struct {
	V        int             `json:"v"`
	Type     string          `json:"type"`
	Seq      int64           `json:"seq,omitempty"`
	RoomID   string          `json:"roomId"`
	ClientID string          `json:"clientId"`
	TS       int64           `json:"ts"`
	Payload  json.RawMessage `json:"payload"`
}

// ValidateEnvelope checks the fields every message must satisfy, regardless of
// type (docs/PROTOCOL.md §2.2).
func ValidateEnvelope(env *Envelope) error {
	if env == nil {
		return &Error{Code: CodeBadRequest, Message: "empty envelope"}
	}
	if env.V != 0 && env.V != Version {
		return &Error{Code: CodeUnsupportedVer, Message: fmt.Sprintf("unsupported protocol version %d", env.V), Fatal: true}
	}
	if env.Type == "" {
		return &Error{Code: CodeBadRequest, Message: "missing type"}
	}
	if !utf8.ValidString(env.RoomID) || env.RoomID == "" {
		return &Error{Code: CodeBadRequest, Message: "missing roomId"}
	}
	if len(env.Payload) == 0 || string(env.Payload) == "null" {
		return &Error{Code: CodeBadRequest, Message: "payload must be an object"}
	}
	return nil
}

// Error is a protocol-level error. It doubles as the Go error returned by
// validation so callers can extract the code without string matching.
type Error struct {
	Code    ErrCode
	Message string
	// Fatal means the connection must be closed after reporting.
	Fatal bool
	// OpID attributes the failure to one operation when known.
	OpID string
	// RetryAfterMS is set for rate_limited.
	RetryAfterMS int64
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// AsError extracts a *protocol.Error from err, if it is one.
func AsError(err error) (*Error, bool) {
	pe, ok := err.(*Error)
	return pe, ok
}

// NewError builds a non-fatal error.
func NewError(code ErrCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// fatal builds a fatal error.
func fatal(code ErrCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Fatal: true}
}

// --- Client → server payloads -------------------------------------------

// HelloPayload is the body of `hello` (docs/PROTOCOL.md §3.1).
//
// lastSeq is carried here rather than in the handshake URL so a browser refresh
// cannot replay a stale value from a cached URL.
type HelloPayload struct {
	LastSeq       int64     `json:"lastSeq"`
	Epoch         int64     `json:"epoch,omitempty"`
	ClientVersion string    `json:"clientVersion,omitempty"`
	Viewport      *Viewport `json:"viewport,omitempty"`
}

// Viewport is accepted for forward compatibility (P1 presence culling).
type Viewport struct {
	W   float64 `json:"w"`
	H   float64 `json:"h"`
	Dpr float64 `json:"dpr,omitempty"`
}

// OpAddPayload is the body of `op.add`, the only write intent
// (docs/PROTOCOL.md §3.2).
type OpAddPayload struct {
	OpID   string          `json:"opId"`
	Action string          `json:"action"`
	Shape  json.RawMessage `json:"shape,omitempty"`
	ID     string          `json:"id,omitempty"`
	Points [][]float64     `json:"points,omitempty"`
	Rev    int             `json:"rev,omitempty"`
	// SentAt is client diagnostics only; never used for ordering.
	SentAt int64 `json:"sentAt,omitempty"`
}

// Op actions.
const (
	ActionCreate = "create"
	ActionUpdate = "update"
	ActionDelete = "delete"
	ActionAppend = "append"
)

// MaxAppendPoints is the protocol cap on points per append (docs/PROTOCOL.md §7).
const MaxAppendPoints = 512

// MaxShapePoints is the cap on total points in one freehand shape.
const MaxShapePoints = 4096

// idPattern is the client-generated ID charset from docs/PROTOCOL.md §0.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,40}$`)

// shapeIDPattern is stricter: shape IDs must be identifiable as shapes.
var shapeIDPattern = regexp.MustCompile(`^sh_[A-Za-z0-9_-]{6,32}$`)

// ValidateID checks a client-generated ID (opId).
func ValidateID(id string) bool { return idPattern.MatchString(id) }

// ValidateShapeID checks a shape ID.
func ValidateShapeID(id string) bool { return shapeIDPattern.MatchString(id) }

// Validate checks the structural rules of an op.add payload that do not depend
// on room state.
func (p *OpAddPayload) Validate(maxShapeBytes int) error {
	if p == nil {
		return NewError(CodeBadRequest, "missing payload")
	}
	if !ValidateID(p.OpID) {
		return NewError(CodeInvalidOp, "opId must match ^[A-Za-z0-9_-]{8,40}$")
	}
	switch p.Action {
	case ActionCreate, ActionUpdate:
		if len(p.Shape) == 0 {
			return NewError(CodeInvalidOp, "%s requires shape", p.Action)
		}
		if len(p.Shape) > maxShapeBytes {
			return NewError(CodeShapeTooLarge, "shape is %d bytes, limit %d", len(p.Shape), maxShapeBytes)
		}
	case ActionDelete:
		if !ValidateShapeID(p.ID) {
			return NewError(CodeInvalidOp, "delete requires a valid shape id")
		}
	case ActionAppend:
		if !ValidateShapeID(p.ID) {
			return NewError(CodeInvalidOp, "append requires a valid shape id")
		}
		if len(p.Points) == 0 {
			return NewError(CodeInvalidOp, "append requires points")
		}
		if len(p.Points) > MaxAppendPoints {
			return NewError(CodeInvalidOp, "append has %d points, limit %d", len(p.Points), MaxAppendPoints)
		}
		for _, pt := range p.Points {
			if len(pt) < 2 {
				return NewError(CodeInvalidOp, "each point needs at least [x,y]")
			}
			if err := validateCoord(pt[0]); err != nil {
				return err
			}
			if err := validateCoord(pt[1]); err != nil {
				return err
			}
		}
	default:
		return NewError(CodeInvalidOp, "unknown action %q", p.Action)
	}
	return nil
}

// CursorPayload is the body of presence.cursor (docs/PROTOCOL.md §3.3).
type CursorPayload struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Validate checks the coordinate range.
func (p *CursorPayload) Validate() error {
	if p == nil {
		return NewError(CodeBadRequest, "missing payload")
	}
	if err := validateCoord(p.X); err != nil {
		return err
	}
	return validateCoord(p.Y)
}

// ResyncRequestPayload is the body of resync.request (docs/PROTOCOL.md §3.4).
type ResyncRequestPayload struct {
	FromSeq int64  `json:"fromSeq"`
	Epoch   int64  `json:"epoch,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Validate checks the requested sequence.
func (p *ResyncRequestPayload) Validate() error {
	if p == nil {
		return NewError(CodeBadRequest, "missing payload")
	}
	if p.FromSeq < 0 {
		return NewError(CodeInvalidOp, "fromSeq must be >= 0")
	}
	return nil
}

// PingPayload is the body of ping; TS is echoed back verbatim.
type PingPayload struct {
	TS int64 `json:"ts"`
}

// GrantPayload is the body of room.grant (docs/PERMISSIONS.md §3.1).
//
// Role is editor or viewer only; viewer is how a grant is revoked.
type GrantPayload struct {
	TargetClientID string `json:"targetClientId"`
	Role           string `json:"role"`
}

// TransferPayload is the body of room.transfer (docs/PERMISSIONS.md §2.3).
type TransferPayload struct {
	TargetClientID string `json:"targetClientId"`
}

// --- Server → client payloads -------------------------------------------

// User is the public identity attached to presence and op broadcasts.
//
// Name is personal data: it is sent to peers (that is the feature) but must
// never be logged.
type User struct {
	ClientID string `json:"clientId"`
	UserID   string `json:"userId"`
	Name     string `json:"name"`
	Color    string `json:"color"`
	Role     string `json:"role,omitempty"`
}

// By is the compact author reference embedded in op broadcasts.
type By struct {
	ClientID string `json:"clientId"`
	UserID   string `json:"userId"`
	Name     string `json:"name"`
	Color    string `json:"color"`
}

// RoomInfo is the room summary inside `joined`.
type RoomInfo struct {
	Code  string `json:"code"`
	Title string `json:"title"`
	Epoch int64  `json:"epoch"`
	Seq   int64  `json:"seq"`
}

// LimitsPayload is the subset of limits advertised in `joined`.
type LimitsPayload struct {
	MaxMessageBytes  int `json:"maxMessageBytes"`
	MaxShapeBytes    int `json:"maxShapeBytes"`
	MaxShapesPerRoom int `json:"maxShapesPerRoom"`
	OpsPerSecond     int `json:"opsPerSecond"`
	OpsBurst         int `json:"opsBurst"`
	CursorPerSecond  int `json:"cursorPerSecond"`
}

// JoinedUser is `joined.payload.you`: the public identity plus the convenience
// flag from docs/PERMISSIONS.md §4.3.
type JoinedUser struct {
	User
	// IsOwner is role == "owner", spelled out so a client does not have to
	// compare strings.
	IsOwner bool `json:"isOwner"`
}

// JoinedPayload is the body of `joined` (docs/PROTOCOL.md §4.1).
type JoinedPayload struct {
	You        JoinedUser    `json:"you"`
	Room       RoomInfo      `json:"room"`
	Online     int           `json:"online"`
	InstanceID string        `json:"instanceId"`
	Limits     LimitsPayload `json:"limits"`
	ServerTime int64         `json:"serverTime"`
	// OwnerClientID is the current room owner, null when the room is unowned
	// (docs/PERMISSIONS.md §4.3).
	OwnerClientID *string `json:"ownerClientId"`
}

// MemberRolePayload is the body of `member.role` (docs/PERMISSIONS.md §3.1).
//
// It changes who may write, so it is a persistent message: the envelope carries a
// seq and the event is replayable through resync.
type MemberRolePayload struct {
	ClientID  string `json:"clientId"`
	Role      string `json:"role"`
	GrantedBy string `json:"grantedBy"`
}

// AckStatus is the three-state result of an op.
type AckStatus string

// Ack statuses.
const (
	AckApplied   AckStatus = "applied"
	AckDuplicate AckStatus = "duplicate"
	AckRejected  AckStatus = "rejected"
)

// AckPayload is the body of `ack` (docs/PROTOCOL.md §4.5). Note that Seq lives
// HERE, not in the envelope: the envelope has no seq for ack.
type AckPayload struct {
	OpID         string    `json:"opId"`
	Status       AckStatus `json:"status"`
	Seq          int64     `json:"seq,omitempty"`
	Code         ErrCode   `json:"code,omitempty"`
	Message      string    `json:"message,omitempty"`
	RetryAfterMS int64     `json:"retryAfterMs,omitempty"`
}

// SnapshotPayload is one chunk of `resync.snapshot` (docs/PROTOCOL.md §4.2).
type SnapshotPayload struct {
	Epoch    int64   `json:"epoch"`
	Seq      int64   `json:"seq"`
	Chunk    int     `json:"chunk"`
	Total    int     `json:"total"`
	Shapes   []Shape `json:"shapes"`
	Complete bool    `json:"complete"`
}

// Event is one element of `resync.events` (docs/PROTOCOL.md §4.3): the op.add
// payload plus seq and by.
//
// Role changes share the same stream so that `seq` stays gap-free and a client
// that missed a `member.role` can repair it through resync
// (docs/PERMISSIONS.md §3.1). For those elements Type is "member.role" and the
// op-specific fields are empty.
type Event struct {
	Seq    int64           `json:"seq"`
	OpID   string          `json:"opId,omitempty"`
	Action string          `json:"action,omitempty"`
	Shape  json.RawMessage `json:"shape,omitempty"`
	ID     string          `json:"id,omitempty"`
	Points [][]float64     `json:"points,omitempty"`
	By     By              `json:"by"`
	TS     int64           `json:"ts"`

	// Type discriminates a role change from an op. Empty means "op.add".
	Type string `json:"type,omitempty"`
	// ClientID / Role / GrantedBy describe a member.role element.
	ClientID  string `json:"clientId,omitempty"`
	Role      string `json:"role,omitempty"`
	GrantedBy string `json:"grantedBy,omitempty"`
}

// ResyncEventsPayload is the body of `resync.events`.
type ResyncEventsPayload struct {
	Epoch   int64   `json:"epoch"`
	FromSeq int64   `json:"fromSeq"`
	ToSeq   int64   `json:"toSeq"`
	Events  []Event `json:"events"`
	HasMore bool    `json:"hasMore"`
}

// PresencePayload is the body of presence.join / presence.leave
// (docs/PROTOCOL.md §4.6).
type PresencePayload struct {
	User   *User  `json:"user,omitempty"`
	Users  []User `json:"users"`
	Online int    `json:"online"`
}

// CursorBroadcastPayload is the body of a server presence.cursor
// (docs/PROTOCOL.md §4.7).
type CursorBroadcastPayload struct {
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	From User    `json:"from"`
}

// RoomMetaPayload is the body of room.meta (docs/PROTOCOL.md §4.8).
type RoomMetaPayload struct {
	Title             string `json:"title"`
	ReadonlyRevokedAt int64  `json:"readonlyRevokedAt"`
}

// ErrorPayload is the body of `error` (docs/PROTOCOL.md §4.9).
type ErrorPayload struct {
	Code         ErrCode `json:"code"`
	Message      string  `json:"message"`
	Fatal        bool    `json:"fatal"`
	OpID         string  `json:"opId,omitempty"`
	RetryAfterMS int64   `json:"retryAfterMs,omitempty"`
}

// PongPayload is the body of `pong` (docs/PROTOCOL.md §4.10).
type PongPayload struct {
	TS         int64 `json:"ts"`
	ServerTime int64 `json:"serverTime"`
}

// --- Shape model (docs/PROTOCOL.md §5) ----------------------------------

// Shape kinds.
const (
	KindFreehand = "freehand"
	KindRect     = "rect"
	KindEllipse  = "ellipse"
	KindArrow    = "arrow"
	KindText     = "text"
)

// ShapeKinds is the closed enumeration.
var ShapeKinds = []string{KindFreehand, KindRect, KindEllipse, KindArrow, KindText}

// Dash styles.
const (
	DashSolid  = "solid"
	DashDashed = "dashed"
	DashDotted = "dotted"
)

// Text variants (note is a text shape with a fill).
const (
	VariantPlain = "plain"
	VariantNote  = "note"
)

// ArrowHead variants.
const (
	HeadEnd  = "end"
	HeadBoth = "both"
)

// Geom is the bounding box in world coordinates. The server recomputes it from
// `data` for freehand and arrow shapes (docs/PROTOCOL.md §5.4 rule 8).
type Geom struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Style is the visual style of a shape.
type Style struct {
	Stroke      string  `json:"stroke"`
	Fill        string  `json:"fill"`
	StrokeWidth float64 `json:"strokeWidth"`
	Opacity     float64 `json:"opacity"`
	Dash        string  `json:"dash"`
}

// Author is the creator reference embedded in a shape.
type Author struct {
	CID  string `json:"cid"`
	Name string `json:"name"`
}

// Shape is the authoritative shape record (docs/PROTOCOL.md §5.1).
//
// Data holds kind-specific fields:
//
//	freehand: {"points":[[x,y],...], "closed":bool}
//	rect:     {"radius":number}
//	ellipse:  {}
//	arrow:    {"x1","y1","x2","y2":number, "head":"end"|"both"}
//	text:     {} + top-level Text, {"variant":"plain"|"note"}
//
// Data is kept as raw JSON so unknown kind-specific fields survive a round trip
// (forward compatibility), while validation reads the known ones.
type Shape struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Geom      Geom            `json:"geom"`
	Style     Style           `json:"style"`
	Data      json.RawMessage `json:"data"`
	Text      *string         `json:"text"`
	Author    Author          `json:"author"`
	Rev       int             `json:"rev"`
	Deleted   bool            `json:"deleted"`
	CreatedAt int64           `json:"createdAt"`
	UpdatedAt int64           `json:"updatedAt"`
}

// ShapeData is the decoded, kind-specific part of Data.
type ShapeData struct {
	Points  [][]float64 `json:"points,omitempty"`
	Closed  bool        `json:"closed,omitempty"`
	Radius  float64     `json:"radius,omitempty"`
	X1      float64     `json:"x1,omitempty"`
	Y1      float64     `json:"y1,omitempty"`
	X2      float64     `json:"x2,omitempty"`
	Y2      float64     `json:"y2,omitempty"`
	Head    string      `json:"head,omitempty"`
	Variant string      `json:"variant,omitempty"`
}

// maxCoord is the protocol coordinate bound (docs/PROTOCOL.md §0).
const maxCoord = 1e6

// MaxTextRunes is the protocol cap on text length.
const MaxTextRunes = 2000

// DecodeData unmarshals Shape.Data into the known fields. Unknown fields are
// ignored, as the contract requires.
func (s *Shape) DecodeData() (*ShapeData, error) {
	if len(s.Data) == 0 || string(s.Data) == "null" {
		return &ShapeData{}, nil
	}
	var d ShapeData
	if err := json.Unmarshal(s.Data, &d); err != nil {
		return nil, NewError(CodeInvalidOp, "data is not an object: %v", err)
	}
	return &d, nil
}

// ValidateShape checks every rule in docs/PROTOCOL.md §5.4 that does not depend
// on room state, and normalises the mutable parts (style clamps, text truncation,
// geom recomputation for freehand/arrow).
//
// It mutates s: validation and normalisation are one pass on purpose, because
// the caller needs the normalised shape (the broadcast must carry the same
// clamped values every peer stores).
func ValidateShape(s *Shape, maxBytes int) error {
	if s == nil {
		return NewError(CodeBadRequest, "missing shape")
	}
	if !ValidateShapeID(s.ID) {
		return NewError(CodeInvalidOp, "shape id must match ^sh_[A-Za-z0-9_-]{6,32}$")
	}
	if !isShapeKind(s.Kind) {
		return NewError(CodeInvalidOp, "unknown shape kind %q", s.Kind)
	}
	if err := validateGeom(&s.Geom); err != nil {
		return err
	}
	if err := normalizeStyle(&s.Style); err != nil {
		return err
	}
	data, err := s.DecodeData()
	if err != nil {
		return err
	}
	switch s.Kind {
	case KindFreehand:
		if len(data.Points) == 0 {
			return NewError(CodeInvalidOp, "freehand requires data.points")
		}
		if len(data.Points) > MaxShapePoints {
			return NewError(CodeShapeTooLarge, "freehand has %d points, limit %d", len(data.Points), MaxShapePoints)
		}
		for _, pt := range data.Points {
			if len(pt) < 2 {
				return NewError(CodeInvalidOp, "each point needs at least [x,y]")
			}
			if err := validateCoord(pt[0]); err != nil {
				return err
			}
			if err := validateCoord(pt[1]); err != nil {
				return err
			}
		}
		// The client must not be able to lie about the bounding box: it drives
		// hit testing and view fitting on every other client.
		s.Geom = BoundsForPoints(data.Points)
	case KindArrow:
		for _, v := range []float64{data.X1, data.Y1, data.X2, data.Y2} {
			if err := validateCoord(v); err != nil {
				return err
			}
		}
		s.Geom = BoundsForArrow(data.X1, data.Y1, data.X2, data.Y2)
		switch data.Head {
		case "", HeadEnd, HeadBoth:
		default:
			return NewError(CodeInvalidOp, "arrow head must be end or both")
		}
	case KindRect:
		if data.Radius < 0 {
			data.Radius = 0
		}
		if data.Radius > 64 {
			data.Radius = 64
		}
		if data.Radius > 0 || len(s.Data) > 0 {
			if err := s.setData(data); err != nil {
				return err
			}
		}
	case KindEllipse:
		// Geometry only.
	case KindText:
		if s.Text == nil {
			return NewError(CodeInvalidOp, "text shape requires text")
		}
		truncated := truncateRunes(*s.Text, MaxTextRunes)
		s.Text = &truncated
		switch data.Variant {
		case "", VariantPlain, VariantNote:
		default:
			return NewError(CodeInvalidOp, "text variant must be plain or note")
		}
	default:
		return NewError(CodeInvalidOp, "unknown shape kind %q", s.Kind)
	}

	if s.Rev < 0 {
		return NewError(CodeInvalidOp, "rev must be >= 0")
	}
	// Size is checked last, after clamping, so a shape that becomes legal by
	// clamping is not rejected for its pre-clamp size.
	blob, err := json.Marshal(s)
	if err != nil {
		return NewError(CodeInternal, "shape encode failed")
	}
	if len(blob) > maxBytes {
		return NewError(CodeShapeTooLarge, "shape is %d bytes, limit %d", len(blob), maxBytes)
	}
	return nil
}

// setData re-encodes the normalised kind-specific data.
func (s *Shape) setData(d *ShapeData) error {
	blob, err := json.Marshal(d)
	if err != nil {
		return NewError(CodeInternal, "shape data encode failed")
	}
	s.Data = blob
	return nil
}

// normalizeStyle clamps ranges and validates colours per
// docs/PROTOCOL.md §5.3 / §5.4 rule 3. Colour format errors are rejected rather
// than clamped because a wrong colour is a client bug, not an out-of-range value.
func normalizeStyle(st *Style) error {
	if st.Stroke == "" {
		st.Stroke = "#16181d"
	}
	if !isHexColor(st.Stroke) {
		return NewError(CodeInvalidOp, "style.stroke must be #rrggbb")
	}
	if st.Fill == "" {
		st.Fill = "none"
	}
	if st.Fill != "none" && !isHexColor(st.Fill) {
		return NewError(CodeInvalidOp, "style.fill must be #rrggbb or none")
	}
	if st.StrokeWidth == 0 {
		st.StrokeWidth = 2
	}
	if st.StrokeWidth < 1 {
		st.StrokeWidth = 1
	}
	if st.StrokeWidth > 32 {
		st.StrokeWidth = 32
	}
	if st.Opacity == 0 {
		st.Opacity = 1
	}
	if st.Opacity < 0.1 {
		st.Opacity = 0.1
	}
	if st.Opacity > 1 {
		st.Opacity = 1
	}
	if st.Dash == "" {
		st.Dash = DashSolid
	}
	switch st.Dash {
	case DashSolid, DashDashed, DashDotted:
	default:
		st.Dash = DashSolid
	}
	return nil
}

// validateGeom rejects non-finite values and enforces w,h >= 0 and the
// coordinate bound.
func validateGeom(g *Geom) error {
	for _, v := range []float64{g.X, g.Y, g.W, g.H} {
		if err := validateCoord(v); err != nil {
			return err
		}
	}
	if g.W < 0 || g.H < 0 {
		return NewError(CodeInvalidOp, "geom.w and geom.h must be >= 0")
	}
	return nil
}

// validateCoord rejects NaN, ±Inf and out-of-range coordinates.
func validateCoord(v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return NewError(CodeInvalidOp, "coordinate must be finite")
	}
	if v < -maxCoord || v > maxCoord {
		return NewError(CodeInvalidOp, "coordinate %g is outside ±1e6", v)
	}
	return nil
}

// BoundsForPoints computes the enclosing box of a point list, clamped to the
// coordinate range.
func BoundsForPoints(points [][]float64) Geom {
	if len(points) == 0 {
		return Geom{}
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range points {
		if len(p) < 2 {
			continue
		}
		minX = math.Min(minX, p[0])
		maxX = math.Max(maxX, p[0])
		minY = math.Min(minY, p[1])
		maxY = math.Max(maxY, p[1])
	}
	if math.IsInf(minX, 1) {
		return Geom{}
	}
	return Geom{X: minX, Y: minY, W: maxX - minX, H: maxY - minY}
}

// BoundsForArrow computes the enclosing box of an arrow.
func BoundsForArrow(x1, y1, x2, y2 float64) Geom {
	return Geom{
		X: math.Min(x1, x2),
		Y: math.Min(y1, y2),
		W: math.Abs(x2 - x1),
		H: math.Abs(y2 - y1),
	}
}

// isShapeKind reports whether k is one of the five P0 kinds.
func isShapeKind(k string) bool {
	for _, v := range ShapeKinds {
		if v == k {
			return true
		}
	}
	return false
}

// isHexColor checks the lowercase #rrggbb form. Uppercase is rejected on purpose:
// colour comparison elsewhere is a plain string compare, and normalising here
// would silently change data the client then re-submits.
func isHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for i := 1; i < 7; i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// truncateRunes cuts s to at most max runes without splitting UTF-8.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == max {
			// Trim trailing spaces so a truncated Chinese sentence does not end
			// with a dangling blank.
			return strings.TrimRight(s[:i], " \t")
		}
		count++
	}
	return s
}

// SanitizeNickname trims whitespace, collapses control characters and clamps the
// result to max runes. Nicknames end up in every peer's UI and in shape author
// fields, so they are cleaned once at the boundary.
func SanitizeNickname(in string, max int) string {
	s := strings.TrimSpace(in)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			continue
		default:
			b.WriteRune(r)
		}
	}
	return truncateRunes(strings.TrimSpace(b.String()), max)
}

// TruncateTitle clamps a room title to the protocol limit, returning the default
// for an empty title.
func TruncateTitle(in string, max int) string {
	s := strings.TrimSpace(in)
	if s == "" {
		return "未命名白板"
	}
	return truncateRunes(s, max)
}

// Marshal encodes an envelope, filling in defaults that every outgoing message
// must carry. It is the only place that writes the wire format, which prevents
// field drift between handlers.
func Marshal(typ, roomID, clientID string, seq int64, ts int64, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode payload: %w", err)
	}
	env := Envelope{
		V:        Version,
		Type:     typ,
		RoomID:   roomID,
		ClientID: clientID,
		TS:       ts,
		Payload:  raw,
	}
	// Only persistent messages carry a top-level seq (docs/PROTOCOL.md §2.2).
	if PersistentTypes[typ] && seq > 0 {
		env.Seq = seq
	}
	return json.Marshal(env)
}
