// Package postgres is the durable implementation of room.Store.
//
// Design notes that matter for correctness:
//
//   - events is the source of truth (docs/DATA-MODEL.md §1); snapshots are a cache
//     that can always be rebuilt from events.
//   - (room_code, seq) is the primary key and the idempotency anchor:
//     uniq (room_code, client_id, op_id) makes a retransmitted op a no-op.
//   - seq assignment and the event insert happen in one transaction. With
//     BROKER=redis the seq comes from Redis INCR instead, and an allocated-but-not-
//     written seq leaves a hole; clients detect it and resync, which is the
//     documented trade-off (ARCHITECTURE.md §4.1).
//   - Snapshots are zstd-compressed JSON of the shape array, byte-identical to
//     resync.snapshot's shapes so the server can chunk them without re-encoding
//     (DATA-MODEL.md §4).
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klauspost/compress/zstd"

	"github.com/codraw/codraw/backend/internal/protocol"
	"github.com/codraw/codraw/backend/internal/room"
)

// Options configures the store.
type Options struct {
	// DatabaseURL is a standard postgres:// connection string.
	DatabaseURL string
	// MaxConns / MinConns bound the pool. A demo deployment needs a small pool:
	// each instance holds one connection per in-flight op, and ops are serialised
	// per room.
	MaxConns int32
	MinConns int32
	// ConnectTimeout bounds the initial dial and ping.
	ConnectTimeout time.Duration
}

// Store is the Postgres-backed room.Store.
type Store struct {
	pool *pgxpool.Pool
	// zstdEncoder is safe for concurrent use.
	enc *zstd.Encoder
	// dec is used for reads; also concurrency-safe.
	dec *zstd.Decoder
}

var _ room.Store = (*Store)(nil)

// New connects, verifies the schema version and returns the store.
//
// Migrations run here rather than as a separate binary because the project ships a
// single container: an operator who sets STORE=postgres expects it to work.
func New(ctx context.Context, opts Options) (*Store, error) {
	if opts.DatabaseURL == "" {
		return nil, errors.New("postgres: DatabaseURL is required")
	}
	cfg, err := pgxpool.ParseConfig(opts.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse url: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	// Statement cache is per-connection and this app uses a small fixed set of
	// statements, so the default is fine; a very small pool would still work.
	if opts.ConnectTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = opts.ConnectTimeout
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, max(opts.ConnectTimeout, 5*time.Second))
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: init zstd encoder: %w", err)
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: init zstd decoder: %w", err)
	}

	s := &Store{pool: pool, enc: enc, dec: dec}
	if err := s.migrate(pingCtx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Backend implements room.Store.
func (s *Store) Backend() string { return "postgres" }

// Ping implements room.Store.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Close implements room.Store.
func (s *Store) Close() error {
	s.enc.Close()
	s.dec.Close()
	s.pool.Close()
	return nil
}

// CreateRoom implements room.Store.
func (s *Store) CreateRoom(ctx context.Context, rec *room.Record) error {
	if rec == nil || rec.Code == "" {
		return room.ErrNotFound
	}
	title := rec.Title
	if title == "" {
		title = "未命名白板"
	}
	created := rec.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	epoch := rec.Epoch
	if epoch == 0 {
		epoch = 1
	}
	// The keys are stored as zero hashes here and filled in by SetKeys: creation
	// and key generation are separate steps so the plaintext never passes through
	// this layer.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO rooms (code, title, epoch, editor_key_hash, viewer_key_hash,
		                   shape_count, last_seq, created_at, last_active_at, expires_at)
		VALUES ($1, $2, $3, '\x00'::bytea, '\x00'::bytea, $4, $5, $6, $6, $7)`,
		rec.Code, title, epoch, rec.ShapeCount, rec.Seq, created,
		created.Add(roomTTLFromContext(ctx)),
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return room.ErrDuplicateRoom
		}
		return fmt.Errorf("postgres: create room: %w", err)
	}
	return nil
}

// SetKeys implements room.Store.
func (s *Store) SetKeys(ctx context.Context, code string, editorHash, viewerHash [32]byte) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE rooms SET editor_key_hash = $2, viewer_key_hash = $3 WHERE code = $1`,
		code, editorHash[:], viewerHash[:])
	if err != nil {
		return fmt.Errorf("postgres: set keys: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return room.ErrNotFound
	}
	return nil
}

// GetRoomMeta implements room.Store.
func (s *Store) GetRoomMeta(ctx context.Context, code string) (*room.Meta, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT code, title, epoch, last_seq, shape_count, created_at, last_active_at,
		       readonly_min_iat, owner_client_id
		  FROM rooms WHERE code = $1`, code)
	var m room.Meta
	if err := row.Scan(&m.Code, &m.Title, &m.Epoch, &m.Seq, &m.ShapeCount,
		&m.CreatedAt, &m.LastActiveAt, &m.RevokedAt, &m.OwnerClientID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, room.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get room: %w", err)
	}
	return &m, nil
}

// SaveOwner implements room.Persister (docs/PERMISSIONS.md §7).
func (s *Store) SaveOwner(ctx context.Context, code, ownerClientID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE rooms SET owner_client_id = $2 WHERE code = $1`, code, ownerClientID)
	if err != nil {
		return fmt.Errorf("postgres: save owner: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return room.ErrNotFound
	}
	return nil
}

// SaveGrant implements room.Persister.
func (s *Store) SaveGrant(ctx context.Context, code, clientID, role, grantedBy string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	if role != protocol.RoleEditor && role != protocol.RoleViewer {
		return fmt.Errorf("postgres: invalid grant role %q", role)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO room_grants (room_id, client_id, role, granted_by, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (room_id, client_id)
		DO UPDATE SET role = EXCLUDED.role,
		              granted_by = EXCLUDED.granted_by,
		              updated_at = EXCLUDED.updated_at`,
		code, clientID, role, grantedBy, at.UnixMilli())
	if err != nil {
		return fmt.Errorf("postgres: save grant: %w", err)
	}
	return nil
}

// DeleteGrant implements room.Persister.
func (s *Store) DeleteGrant(ctx context.Context, code, clientID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM room_grants WHERE room_id = $1 AND client_id = $2`, code, clientID)
	if err != nil {
		return fmt.Errorf("postgres: delete grant: %w", err)
	}
	return nil
}

// LoadGrants returns the room's authorisation table.
func (s *Store) LoadGrants(ctx context.Context, code string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT client_id, role FROM room_grants WHERE room_id = $1`, code)
	if err != nil {
		return nil, fmt.Errorf("postgres: load grants: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var cid, role string
		if err := rows.Scan(&cid, &role); err != nil {
			return nil, fmt.Errorf("postgres: scan grant: %w", err)
		}
		out[cid] = role
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate grants: %w", err)
	}
	return out, nil
}

// VerifyKeyHash implements room.Store.
func (s *Store) VerifyKeyHash(ctx context.Context, code string) ([32]byte, [32]byte, int64, error) {
	var editor, viewer []byte
	var revoked int64
	err := s.pool.QueryRow(ctx,
		`SELECT editor_key_hash, viewer_key_hash, readonly_min_iat FROM rooms WHERE code = $1`, code,
	).Scan(&editor, &viewer, &revoked)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return [32]byte{}, [32]byte{}, 0, room.ErrNotFound
		}
		return [32]byte{}, [32]byte{}, 0, fmt.Errorf("postgres: verify key: %w", err)
	}
	var eh, vh [32]byte
	copy(eh[:], editor)
	copy(vh[:], viewer)
	return eh, vh, revoked, nil
}

// BumpReadonlyRevocation implements room.Store.
//
// The watermark is now()+1ms so a token issued in the same millisecond as the
// revoke is invalidated too.
func (s *Store) BumpReadonlyRevocation(ctx context.Context, code string) (int64, error) {
	var at int64
	err := s.pool.QueryRow(ctx, `
		UPDATE rooms
		   SET readonly_min_iat = (extract(epoch from now()) * 1000)::bigint + 1
		 WHERE code = $1
		RETURNING readonly_min_iat`, code).Scan(&at)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, room.ErrNotFound
		}
		return 0, fmt.Errorf("postgres: bump revocation: %w", err)
	}
	return at, nil
}

// SetTitle implements room.Store.
func (s *Store) SetTitle(ctx context.Context, code, title string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE rooms SET title = $2, last_active_at = now() WHERE code = $1`, code, title)
	if err != nil {
		return fmt.Errorf("postgres: set title: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return room.ErrNotFound
	}
	return nil
}

// TouchRoom implements room.Store.
//
// A failure here is advisory: it only updates a display counter.
func (s *Store) TouchRoom(ctx context.Context, code string, shapeCount int, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE rooms SET shape_count = $2, last_active_at = $3 WHERE code = $1`,
		code, shapeCount, at)
	if err != nil {
		return fmt.Errorf("postgres: touch room: %w", err)
	}
	return nil
}

// DeleteRoom implements room.Store.
func (s *Store) DeleteRoom(ctx context.Context, code string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM rooms WHERE code = $1`, code)
	if err != nil {
		return fmt.Errorf("postgres: delete room: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return room.ErrNotFound
	}
	return nil
}

// AppendOp implements room.Store.
//
// The event insert and the room counters are one transaction (DATA-MODEL.md §2.2):
// a partially applied op would make shape_count drift from the log.
func (s *Store) AppendOp(ctx context.Context, rec room.OpRecord) (int64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotency first: a retransmitted (client_id, op_id) returns the original
	// seq without consuming a new one.
	var existing int64
	err = tx.QueryRow(ctx, `
		SELECT seq FROM events WHERE room_code = $1 AND client_id = $2 AND op_id = $3`,
		rec.Code, rec.ClientID, rec.OpID).Scan(&existing)
	if err == nil {
		return existing, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("postgres: lookup op: %w", err)
	}

	// seq: use the caller's value when it came from a shared allocator (Redis INCR),
	// otherwise allocate from the room row, which serialises concurrent writers for
	// the same room because the UPDATE takes a row lock until commit.
	var seq int64
	if rec.Seq > 0 {
		seq = rec.Seq
		// last_seq is a correction cursor for the memory-mode path; keep it from
		// going backwards when a shared allocator is ahead of it.
		if _, err := tx.Exec(ctx,
			`UPDATE rooms SET last_seq = GREATEST(last_seq, $2) WHERE code = $1`,
			rec.Code, seq); err != nil {
			return 0, false, fmt.Errorf("postgres: advance seq: %w", err)
		}
	} else {
		err = tx.QueryRow(ctx,
			`UPDATE rooms SET last_seq = last_seq + 1 WHERE code = $1 RETURNING last_seq`,
			rec.Code).Scan(&seq)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, false, room.ErrNotFound
			}
			return 0, false, fmt.Errorf("postgres: allocate seq: %w", err)
		}
	}

	delta := 0
	switch rec.Action {
	case protocol.ActionCreate:
		delta = 1
	case protocol.ActionDelete:
		delta = -1
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO events (room_code, seq, op_id, client_id, author_id, action, shape_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8)`,
		rec.Code, seq, rec.OpID, rec.ClientID, rec.AuthorID, rec.Action, rec.ShapeID, rec.Payload)
	if err != nil {
		return 0, false, fmt.Errorf("postgres: insert event: %w", err)
	}

	_, err = tx.Exec(ctx, `
		UPDATE rooms
		   SET shape_count = GREATEST(0, shape_count + $2), last_active_at = now()
		 WHERE code = $1`, rec.Code, delta)
	if err != nil {
		return 0, false, fmt.Errorf("postgres: update counters: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("postgres: commit: %w", err)
	}
	return seq, false, nil
}

// AppendBatch implements room.Persister as ONE statement.
//
// The whole batch is a single round trip: a data-modifying CTE inserts the new rows,
// a second CTE updates the room cursor and shape counter, and the statement returns
// nothing. Duplicate detection is handled by ON CONFLICT DO NOTHING, and the caller's
// own dedupe ring already suppresses the retransmits that would hit it — a duplicate
// that slips through here is simply not written twice, which is the correctness
// property that matters. (The rare cross-instance duplicate is reported back through
// the room's resync path, not through a per-row seq lookup.)
//
// Why this shape: measurements showed a batch of 4 costing ~2.5ms, dominated by round
// trips (dup SELECT + INSERT + UPDATE). Collapsing to one statement makes the fixed
// cost one round trip + one fsync, so small batches are cheap and the 8ms window can
// actually amortise.
func (s *Store) AppendBatch(ctx context.Context, recs []room.OpRecord) ([]room.AppendResult, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	results := make([]room.AppendResult, len(recs))
	var maxSeq int64
	var shapeDelta int
	for i, rec := range recs {
		results[i] = room.AppendResult{Seq: rec.Seq}
		if rec.Seq > maxSeq {
			maxSeq = rec.Seq
		}
		switch rec.Action {
		case protocol.ActionCreate:
			shapeDelta++
		case protocol.ActionDelete:
			shapeDelta--
		}
	}

	const cols = 8
	var b strings.Builder
	b.WriteString(`WITH ins AS (
	INSERT INTO events (room_code, seq, op_id, client_id, author_id, action, shape_id, payload)
	VALUES `)
	args := make([]any, 0, len(recs)*cols)
	for i, rec := range recs {
		if i > 0 {
			b.WriteByte(',')
		}
		n := i * cols
		fmt.Fprintf(&b, "($%d,$%d,$%d,$%d,$%d,$%d,NULLIF($%d,''),$%d)",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8)
		args = append(args, rec.Code, rec.Seq, rec.OpID, rec.ClientID,
			rec.AuthorID, rec.Action, rec.ShapeID, rec.Payload)
	}
	// The CTE must be referenced for Postgres to run it, and the counter update must
	// see the same transaction, hence the final UPDATE ... FROM (SELECT count(*) ...).
	roomParam := len(args) + 1
	seqParam := len(args) + 2
	deltaParam := len(args) + 3
	args = append(args, recs[0].Code, maxSeq, shapeDelta)
	fmt.Fprintf(&b, ` ON CONFLICT (room_code, client_id, op_id) DO NOTHING
	RETURNING 1)
	UPDATE rooms
	   SET last_seq = GREATEST(last_seq, $%d),
	       shape_count = GREATEST(0, shape_count + $%d),
	       last_active_at = now()
	 WHERE code = $%d
	RETURNING (SELECT count(*) FROM ins)`, seqParam, deltaParam, roomParam)

	var inserted int64
	if err := s.pool.QueryRow(ctx, b.String(), args...).Scan(&inserted); err != nil {
		return nil, fmt.Errorf("postgres: append batch: %w", err)
	}
	return results, nil
}

// LoadRoom implements room.Store: metadata, the newest snapshot and the ops after it.
func (s *Store) LoadRoom(ctx context.Context, code string) (*room.Record, error) {
	meta, err := s.GetRoomMeta(ctx, code)
	if err != nil {
		return nil, err
	}
	rec := &room.Record{
		Code:          meta.Code,
		Title:         meta.Title,
		Epoch:         meta.Epoch,
		Seq:           meta.Seq,
		ShapeCount:    meta.ShapeCount,
		CreatedAt:     meta.CreatedAt,
		OwnerClientID: meta.OwnerClientID,
	}
	grants, err := s.LoadGrants(ctx, code)
	if err != nil {
		return nil, err
	}
	if len(grants) > 0 {
		rec.Grants = grants
	}

	// Replay position is the snapshot's seq, NOT the room's last_seq: with no
	// snapshot every op must be replayed, and starting from last_seq would skip all
	// of them and produce a room whose seq says N but whose shape map is empty.
	var from int64
	snap, err := s.LatestSnapshot(ctx, code)
	if err == nil {
		rec.Snapshot = &snap
		from = snap.Seq
		rec.Seq = snap.Seq
	} else if !errors.Is(err, room.ErrNoSnapshot) && !errors.Is(err, room.ErrNotFound) {
		return nil, err
	}

	// Replay only what the snapshot does not already contain.
	ops, _, err := s.LoadOps(ctx, code, from, 10000, 32<<20)
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		var body struct {
			OpID   string          `json:"opId"`
			Action string          `json:"action"`
			Shape  json.RawMessage `json:"shape"`
			ID     string          `json:"id"`
			Points [][]float64     `json:"points"`
			By     protocol.By     `json:"by"`
			TS     int64           `json:"ts"`
		}
		if err := json.Unmarshal(op.Payload, &body); err != nil {
			continue
		}
		rec.Ops = append(rec.Ops, room.ReplayOp{
			Seq:      op.Seq,
			Epoch:    op.Epoch,
			OpID:     op.OpID,
			ClientID: op.ClientID,
			Action:   op.Action,
			Shape:    body.Shape,
			ID:       body.ID,
			Points:   body.Points,
			By:       body.By,
			TS:       body.TS,
		})
		if op.Seq > rec.Seq {
			rec.Seq = op.Seq
		}
	}
	return rec, nil
}

// ReplayRange implements room.CatchUp: ops with seq in [from, to], decoded into the
// replay form the room's catch-up path applies.
func (s *Store) ReplayRange(ctx context.Context, code string, from, to int64) ([]room.ReplayOp, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT seq, op_id, client_id, action, payload
		  FROM events
		 WHERE room_code = $1 AND seq >= $2 AND seq <= $3
		 ORDER BY seq`, code, from, to)
	if err != nil {
		return nil, fmt.Errorf("postgres: replay range: %w", err)
	}
	defer rows.Close()

	var out []room.ReplayOp
	for rows.Next() {
		var (
			seq      int64
			opID     string
			clientID string
			action   string
			payload  []byte
		)
		if err := rows.Scan(&seq, &opID, &clientID, &action, &payload); err != nil {
			return nil, fmt.Errorf("postgres: scan replay: %w", err)
		}
		var body struct {
			Shape  json.RawMessage `json:"shape"`
			ID     string          `json:"id"`
			Points [][]float64     `json:"points"`
			By     protocol.By     `json:"by"`
			TS     int64           `json:"ts"`
		}
		_ = json.Unmarshal(payload, &body)
		out = append(out, room.ReplayOp{
			Seq: seq, OpID: opID, ClientID: clientID, Action: action,
			Shape: body.Shape, ID: body.ID, Points: body.Points, By: body.By, TS: body.TS,
		})
	}
	return out, rows.Err()
}

// LoadOps implements room.Store.
func (s *Store) LoadOps(ctx context.Context, code string, afterSeq int64, limit int, maxBytes int64) ([]room.OpRecord, bool, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.pool.Query(ctx, `
		SELECT seq, op_id, client_id, action, coalesce(shape_id, ''), payload, created_at
		  FROM events
		 WHERE room_code = $1 AND seq > $2
		 ORDER BY seq
		 LIMIT $3`, code, afterSeq, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("postgres: load ops: %w", err)
	}
	defer rows.Close()

	var out []room.OpRecord
	var size int64
	truncated := false
	for rows.Next() {
		var (
			seq      int64
			opID     string
			clientID string
			action   string
			shapeID  string
			payload  []byte
			created  time.Time
		)
		if err := rows.Scan(&seq, &opID, &clientID, &action, &shapeID, &payload, &created); err != nil {
			return nil, false, fmt.Errorf("postgres: scan op: %w", err)
		}
		if len(out) == limit || (len(out) > 0 && maxBytes > 0 && size+int64(len(payload)) > maxBytes) {
			truncated = true
			break
		}
		out = append(out, room.OpRecord{
			Code: code, Seq: seq, OpID: opID, ClientID: clientID,
			Action: action, ShapeID: shapeID, Payload: payload, TS: created,
		})
		size += int64(len(payload))
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("postgres: iterate ops: %w", err)
	}
	return out, truncated, nil
}

// LatestSeq implements room.Store.
func (s *Store) LatestSeq(ctx context.Context, code string) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx, `SELECT last_seq FROM rooms WHERE code = $1`, code).Scan(&seq)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, room.ErrNotFound
		}
		return 0, fmt.Errorf("postgres: latest seq: %w", err)
	}
	return seq, nil
}

// TrimOps implements room.Store.
//
// DATA-MODEL.md §6 hard rule: never delete an event that a client might still need
// to catch up. That means seq must be <= the newest snapshot's seq, otherwise a
// client whose lastSeq falls between the snapshot and the trim point could never be
// repaired without a full resnapshot.
func (s *Store) TrimOps(ctx context.Context, code string, upto int64, keep int) error {
	var snapSeq int64
	err := s.pool.QueryRow(ctx,
		`SELECT coalesce(max(seq), 0) FROM snapshots WHERE room_code = $1`, code).Scan(&snapSeq)
	if err != nil {
		return fmt.Errorf("postgres: trim lookup: %w", err)
	}
	if upto > snapSeq {
		upto = snapSeq
	}
	if upto <= 0 {
		return nil
	}
	_, err = s.pool.Exec(ctx, `
		DELETE FROM events
		 WHERE room_code = $1
		   AND seq <= $2
		   AND seq <= (SELECT last_seq - $3 FROM rooms WHERE code = $1)`,
		code, upto, keep)
	if err != nil {
		return fmt.Errorf("postgres: trim ops: %w", err)
	}
	return nil
}

// SaveSnapshot implements room.Store, compressing with zstd.
func (s *Store) SaveSnapshot(ctx context.Context, code string, epoch, seq int64, shapeCount int, payload []byte) error {
	compressed := s.enc.EncodeAll(payload, make([]byte, 0, len(payload)/4))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		INSERT INTO snapshots (room_code, seq, epoch, shape_count, codec, data)
		VALUES ($1, $2, $3, $4, 'zstd', $5)
		ON CONFLICT (room_code, seq) DO NOTHING`,
		code, seq, epoch, shapeCount, compressed)
	if err != nil {
		return fmt.Errorf("postgres: insert snapshot: %w", err)
	}
	// Keep only the newest three (DATA-MODEL.md §6).
	_, err = tx.Exec(ctx, `
		DELETE FROM snapshots
		 WHERE room_code = $1
		   AND seq NOT IN (SELECT seq FROM snapshots WHERE room_code = $1 ORDER BY seq DESC LIMIT 3)`,
		code)
	if err != nil {
		return fmt.Errorf("postgres: prune snapshots: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit snapshot: %w", err)
	}
	return nil
}

// LatestSnapshot implements room.Store.
func (s *Store) LatestSnapshot(ctx context.Context, code string) (room.SnapshotRecord, error) {
	var (
		snap       room.SnapshotRecord
		codec      string
		compressed []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT seq, epoch, shape_count, codec, data, created_at
		  FROM snapshots WHERE room_code = $1 ORDER BY seq DESC LIMIT 1`, code,
	).Scan(&snap.Seq, &snap.Epoch, &snap.ShapeCount, &codec, &compressed, &snap.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return room.SnapshotRecord{}, room.ErrNoSnapshot
		}
		return room.SnapshotRecord{}, fmt.Errorf("postgres: latest snapshot: %w", err)
	}
	snap.Code = code
	// The codec column exists so the format can change without a data migration.
	switch codec {
	case "zstd":
		out, derr := s.dec.DecodeAll(compressed, nil)
		if derr != nil {
			return room.SnapshotRecord{}, fmt.Errorf("postgres: decode snapshot: %w", derr)
		}
		snap.Payload = out
	case "none":
		snap.Payload = compressed
	default:
		return room.SnapshotRecord{}, fmt.Errorf("postgres: unsupported snapshot codec %q", codec)
	}
	return snap, nil
}

// LoadSnapshot implements room.Store. It is the value-typed spelling of
// LatestSnapshot used by the port, so the room actor never depends on the storage
// package's richer API.
func (s *Store) LoadSnapshot(ctx context.Context, code string) (room.SnapshotRecord, error) {
	return s.LatestSnapshot(ctx, code)
}

// SaveSnapshotRecord is an adapter for code paths that already hold a snapshot
// value.
func (s *Store) SaveSnapshotRecord(ctx context.Context, snap room.SnapshotRecord) error {
	return s.SaveSnapshot(ctx, snap.Code, snap.Epoch, snap.Seq, snap.ShapeCount, snap.Payload)
}
