package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/codraw/codraw/backend/internal/protocol"
	"github.com/codraw/codraw/backend/internal/room"
)

// SnapshotShapes returns the live shapes at the newest snapshot, sorted by id.
//
// This is the REST fallback for rooms too large for a WebSocket snapshot
// (docs/PROTOCOL.md §1.5). The live room, when open, is asked first by the caller;
// this path serves a room that is not currently loaded on this instance.
func (s *Store) SnapshotShapes(ctx context.Context, code string) ([]protocol.Shape, int64, error) {
	snap, err := s.LatestSnapshot(ctx, code)
	if err != nil {
		if errors.Is(err, room.ErrNoSnapshot) {
			// No snapshot yet: rebuild from the log rather than returning an empty
			// room, which would look like data loss to the client.
			return s.rebuildShapes(ctx, code)
		}
		return nil, 0, err
	}
	var shapes []protocol.Shape
	if len(snap.Payload) > 0 {
		if err := json.Unmarshal(snap.Payload, &shapes); err != nil {
			return nil, 0, fmt.Errorf("postgres: decode snapshot shapes: %w", err)
		}
	}
	sort.Slice(shapes, func(i, j int) bool { return shapes[i].ID < shapes[j].ID })
	return shapes, snap.Seq, nil
}

// rebuildShapes replays the whole event log into a shape map. It is only used when
// no snapshot exists, so the cost is bounded by the retention window.
func (s *Store) rebuildShapes(ctx context.Context, code string) ([]protocol.Shape, int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT seq, action, payload FROM events
		 WHERE room_code = $1 ORDER BY seq`, code)
	if err != nil {
		return nil, 0, fmt.Errorf("postgres: replay events: %w", err)
	}
	defer rows.Close()

	shapes := make(map[string]*protocol.Shape)
	var lastSeq int64
	for rows.Next() {
		var (
			seq    int64
			action string
			body   []byte
		)
		if err := rows.Scan(&seq, &action, &body); err != nil {
			return nil, 0, fmt.Errorf("postgres: scan replay row: %w", err)
		}
		lastSeq = seq
		var op struct {
			Shape  json.RawMessage `json:"shape"`
			ID     string          `json:"id"`
			Points [][]float64     `json:"points"`
		}
		if err := json.Unmarshal(body, &op); err != nil {
			continue
		}
		switch action {
		case protocol.ActionCreate, protocol.ActionUpdate, protocol.ActionDelete:
			if len(op.Shape) == 0 {
				continue
			}
			var sh protocol.Shape
			if err := json.Unmarshal(op.Shape, &sh); err != nil {
				continue
			}
			shapes[sh.ID] = &sh
		case protocol.ActionAppend:
			existing, ok := shapes[op.ID]
			if !ok || existing.Deleted {
				continue
			}
			data, err := existing.DecodeData()
			if err != nil {
				continue
			}
			switch existing.Kind {
			case protocol.KindFreehand:
				data.Points = append(data.Points, op.Points...)
				existing.Geom = protocol.BoundsForPoints(data.Points)
			case protocol.KindArrow:
				if len(op.Points) > 0 {
					last := op.Points[len(op.Points)-1]
					data.X2, data.Y2 = last[0], last[1]
					existing.Geom = protocol.BoundsForArrow(data.X1, data.Y1, data.X2, data.Y2)
				}
			}
			if blob, err := json.Marshal(data); err == nil {
				existing.Data = blob
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("postgres: iterate replay: %w", err)
	}

	out := make([]protocol.Shape, 0, len(shapes))
	for _, sh := range shapes {
		if sh.Deleted {
			continue
		}
		out = append(out, *sh)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, lastSeq, nil
}

// RoomExists reports whether a room row exists, used by the idle reaper.
func (s *Store) RoomExists(ctx context.Context, code string) (bool, error) {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM rooms WHERE code = $1`, code).Scan(&one)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// DeleteExpiredRooms removes rooms whose expires_at has passed, returning the
// number deleted. Cascades remove events, snapshots and members.
func (s *Store) DeleteExpiredRooms(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = 100
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM rooms
		 WHERE code IN (
		   SELECT code FROM rooms
		    WHERE expires_at IS NOT NULL AND expires_at < now()
		    ORDER BY expires_at
		    LIMIT $1)`, limit)
	if err != nil {
		return 0, fmt.Errorf("postgres: delete expired rooms: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ListActiveRoomCodes returns codes active since a timestamp, used for warmup.
func (s *Store) ListActiveRoomCodes(ctx context.Context, activeSince time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT code FROM rooms
		 WHERE last_active_at >= $1
		 ORDER BY last_active_at DESC
		 LIMIT $2`, activeSince, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list rooms: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("postgres: scan room code: %w", err)
		}
		out = append(out, code)
	}
	return out, rows.Err()
}
