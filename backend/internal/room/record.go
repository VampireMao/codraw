package room

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/codraw/codraw/backend/internal/protocol"
)

// ErrNotFound is returned by a Store when the room does not exist.
var ErrNotFound = errors.New("room: not found")

// ErrDuplicateRoom is returned when a room code is already persisted.
var ErrDuplicateRoom = errors.New("store: duplicate room code")

// Record is the persisted room state a live room is rebuilt from.
type Record struct {
	Code       string
	Title      string
	Epoch      int64
	Seq        int64
	ShapeCount int
	CreatedAt  time.Time
	// OwnerClientID / Grants restore the permission model (docs/PERMISSIONS.md §7).
	// An empty owner means an unowned room, whose next entrant takes over.
	OwnerClientID string
	Grants        map[string]string
	// Snapshot is the newest materialised snapshot, if any.
	Snapshot *SnapshotRecord
	// ShapeHash maps shape id → sha256 of the shape row, used by the sync cache.
	// Nil for the memory store.
	Shapes []protocol.Shape
	// Ops are the log entries after Snapshot.Seq (or from 1 when there is no
	// snapshot), already ordered by seq. They are replayed to rebuild state.
	Ops []ReplayOp
}

// ReplayOp is one stored op as needed for rebuild.
type ReplayOp struct {
	Seq      int64
	Epoch    int64
	OpID     string
	ClientID string
	Action   string
	Shape    json.RawMessage
	ID       string
	Points   [][]float64
	By       protocol.By
	TS       int64
}

// Store is the persistence port used by the hub to materialise rooms. It is a
// superset of the room actor's Persister, kept separate so the actor does not
// depend on room listing APIs.
type Store interface {
	Persister
	// LoadRoom returns the persisted record, or ErrNotFound.
	LoadRoom(ctx context.Context, code string) (*Record, error)
	// CreateRoom persists a new room, returning ErrDuplicateRoom when taken.
	CreateRoom(ctx context.Context, rec *Record) error
	// GetRoomMeta returns persisted metadata without the op log.
	GetRoomMeta(ctx context.Context, code string) (*Meta, error)
	// VerifyKeyHash returns the room's key hashes and revocation watermark.
	VerifyKeyHash(ctx context.Context, code string) (editorHash, viewerHash [32]byte, revokedAt int64, err error)
	// SetKeys stores the key hashes generated at creation.
	SetKeys(ctx context.Context, code string, editorHash, viewerHash [32]byte) error
	// BumpReadonlyRevocation advances the revocation watermark to now.
	BumpReadonlyRevocation(ctx context.Context, code string) (int64, error)
	// SetTitle persists a room title.
	SetTitle(ctx context.Context, code, title string) error
	// DeleteRoom removes a room and its history.
	DeleteRoom(ctx context.Context, code string) error
	// TouchRoom updates the room's last-active time and shape count. It is
	// advisory: a failure must never fail a write op.
	TouchRoom(ctx context.Context, code string, shapeCount int, at time.Time) error
	// Close releases resources. Idempotent.
	Close() error
	// Backend reports the backend name.
	Backend() string
	// Ping checks connectivity.
	Ping(ctx context.Context) error
}

// Meta is persisted room metadata.
type Meta struct {
	Code         string
	Title        string
	Epoch        int64
	Seq          int64
	ShapeCount   int
	CreatedAt    time.Time
	LastActiveAt time.Time
	RevokedAt    int64
	// OwnerClientID is the persisted owner ("" when unowned).
	OwnerClientID string
}

// rebuild replays a persisted record into the live shape map and log ring.
//
// Snapshot-first, then ops: the snapshot is the state at its seq, and the ops
// after it are the delta. Anything the snapshot already contains is not replayed
// twice because ops are read strictly after Snapshot.Seq.
func (r *room) rebuild(rec *Record) error {
	if rec == nil {
		return nil
	}
	r.title = rec.Title
	if rec.Epoch > 0 {
		r.epoch = rec.Epoch
	}
	for i := range rec.Shapes {
		s := rec.Shapes[i]
		r.setShape(&s)
	}
	if rec.Snapshot != nil && len(rec.Snapshot.Payload) > 0 {
		var shapes []protocol.Shape
		if err := json.Unmarshal(rec.Snapshot.Payload, &shapes); err != nil {
			return err
		}
		for i := range shapes {
			s := shapes[i]
			r.setShape(&s)
		}
	}
	for _, op := range rec.Ops {
		// Replay into the shape map, not just the log ring: a room rebuilt from the
		// store must serve the same shapes a snapshot would, and putting ops only in
		// the ring yields a room whose seq is N but whose shape map is empty.
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
		if op.Seq > r.seq {
			r.seq = op.Seq
		}
	}
	if rec.Seq > r.seq {
		r.seq = rec.Seq
	}
	return nil
}

// NewWithRecord creates a room and replays a persisted record into it.
func NewWithRecord(opts Options, rec *Record) (*Handle, error) {
	if rec != nil {
		if rec.Code != "" {
			opts.Code = rec.Code
		}
		if rec.Title != "" {
			opts.Title = rec.Title
		}
		if rec.Epoch > 0 {
			opts.Epoch = rec.Epoch
		}
		if rec.Seq > 0 {
			opts.Seq = rec.Seq
		}
		opts.OwnerClientID = rec.OwnerClientID
		if len(rec.Grants) > 0 {
			opts.Grants = rec.Grants
		}
	}
	h := New(opts)
	if rec != nil {
		if err := h.r.rebuild(rec); err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = h.Close(ctx)
			return nil, err
		}
		h.r.publishStats()
	}
	return h, nil
}
