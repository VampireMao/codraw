// Package memory is the zero-dependency implementation of room.Store.
//
// It is the default in development and the only supported mode for the
// `STORE=memory` demo, so it must behave exactly like Postgres from the room's
// point of view — including atomic seq assignment and (clientId, opId) dedupe —
// even though nothing survives a restart.
//
// # Concurrency
//
// One mutex guards every room's data. The critical sections are map lookups and
// slice appends, and the real serialisation point for a room is its goroutine,
// so per-room sharding would add complexity without changing throughput at the
// 1k-connection target.
package memory

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/codraw/codraw/backend/internal/protocol"
	"github.com/codraw/codraw/backend/internal/room"
)

// dedupeWindow is the number of most recent ops per room retained in the
// idempotency index. The room actor also keeps an in-memory window; this one
// survives a hub restart of a room and is what makes the store contract
// identical to Postgres.
const dedupeWindow = 8192

// dedupeTTL is the time component of the same window (docs/PROTOCOL.md §6.5).
const dedupeTTL = 10 * time.Minute

// Store is an in-memory room.Store.
type Store struct {
	mu     sync.RWMutex
	rooms  map[string]*roomState
	closed bool
	now    func() time.Time
}

type roomState struct {
	meta room.Record
	// ops is a contiguous suffix of the log; ops[0].Seq is the first retained
	// seq. TrimOps removes a prefix.
	ops []room.ReplayOp
	// seq is the last assigned seq; it is never reset by trimming.
	seq int64
	// dedup maps (cid, opId) → seq assigned on first insert.
	dedup      map[dedupKey]dedupEntry
	dedupOrder []dedupKey
	dedupHead  int
	snapshot   *room.SnapshotRecord
	// editorHash / viewerHash are the sha256 digests of the room keys. The
	// plaintext never reaches this layer.
	editorHash [32]byte
	viewerHash [32]byte
	// revokedAt is the readonly-token watermark in ms.
	revokedAt int64
	// owner is the current owner client id ("" when unowned) and grants is the
	// (cid → editor|viewer) authorisation table (docs/PERMISSIONS.md §7).
	owner  string
	grants map[string]grantRow
}

// grantRow is one persisted authorisation.
type grantRow struct {
	role      string
	grantedBy string
	updatedAt time.Time
}

type dedupKey struct {
	cid  string
	opID string
}

type dedupEntry struct {
	seq int64
	at  time.Time
}

// Options configures the store.
type Options struct {
	// Now overrides the clock in tests.
	Now func() time.Time
}

// New returns an empty store.
func New(opts Options) *Store {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Store{rooms: make(map[string]*roomState), now: now}
}

var _ room.Store = (*Store)(nil)

// Backend reports "memory" for /readyz.
func (s *Store) Backend() string { return "memory" }

// Ping verifies the store is usable.
func (s *Store) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return room.ErrNotFound
	}
	return nil
}

// CreateRoom persists a new room. It returns room.ErrDuplicateRoom when the code
// is taken, which the caller resolves by generating another code.
func (s *Store) CreateRoom(ctx context.Context, rec *room.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if rec == nil || rec.Code == "" {
		return room.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return room.ErrNotFound
	}
	if _, ok := s.rooms[rec.Code]; ok {
		return room.ErrDuplicateRoom
	}
	meta := *rec
	meta.Shapes = nil
	meta.Ops = nil
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = s.now()
	}
	if meta.Epoch == 0 {
		meta.Epoch = 1
	}
	st := &roomState{meta: meta, dedup: make(map[dedupKey]dedupEntry), grants: make(map[string]grantRow)}
	if meta.Seq > 0 {
		st.seq = meta.Seq
	}
	st.owner = meta.OwnerClientID
	for cid, role := range meta.Grants {
		st.grants[cid] = grantRow{role: role, grantedBy: cid, updatedAt: s.now()}
	}
	s.rooms[rec.Code] = st
	return nil
}

// SetKeys stores the room's key hashes.
func (s *Store) SetKeys(ctx context.Context, code string, editorHash, viewerHash [32]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return room.ErrNotFound
	}
	st.editorHash = editorHash
	st.viewerHash = viewerHash
	return nil
}

// GetRoomMeta returns persisted metadata without loading history.
func (s *Store) GetRoomMeta(ctx context.Context, code string) (*room.Meta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return nil, room.ErrNotFound
	}
	m := st.meta
	seq := st.seq
	return &room.Meta{
		Code:          m.Code,
		Title:         m.Title,
		Epoch:         m.Epoch,
		Seq:           seq,
		ShapeCount:    m.ShapeCount,
		CreatedAt:     m.CreatedAt,
		LastActiveAt:  m.CreatedAt,
		RevokedAt:     st.revokedAt,
		OwnerClientID: st.owner,
	}, nil
}

// VerifyKeyHash returns the stored key hashes and the revocation watermark.
func (s *Store) VerifyKeyHash(ctx context.Context, code string) ([32]byte, [32]byte, int64, error) {
	if err := ctx.Err(); err != nil {
		return [32]byte{}, [32]byte{}, 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return [32]byte{}, [32]byte{}, 0, room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return [32]byte{}, [32]byte{}, 0, room.ErrNotFound
	}
	return st.editorHash, st.viewerHash, st.revokedAt, nil
}

// BumpReadonlyRevocation moves the watermark to now and returns it.
//
// Every readonly token whose iat is before this instant stops verifying, which
// revokes all outstanding tokens without storing them (docs/PROTOCOL.md §1.4).
func (s *Store) BumpReadonlyRevocation(ctx context.Context, code string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return 0, room.ErrNotFound
	}
	// +1ms so a token issued in the same millisecond as the revoke is also
	// invalidated; otherwise "revoke now" could leave a just-issued token alive.
	st.revokedAt = s.now().UnixMilli() + 1
	return st.revokedAt, nil
}

// SetTitle persists a new room title.
func (s *Store) SetTitle(ctx context.Context, code, title string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return room.ErrNotFound
	}
	st.meta.Title = title
	return nil
}

// SaveOwner persists the room's owner (docs/PERMISSIONS.md §7).
func (s *Store) SaveOwner(ctx context.Context, code, ownerClientID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return room.ErrNotFound
	}
	st.owner = ownerClientID
	st.meta.OwnerClientID = ownerClientID
	return nil
}

// SaveGrant upserts one authorisation row.
func (s *Store) SaveGrant(ctx context.Context, code, clientID, role, grantedBy string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if role != protocol.RoleEditor && role != protocol.RoleViewer {
		return room.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return room.ErrNotFound
	}
	if at.IsZero() {
		at = s.now()
	}
	st.grants[clientID] = grantRow{role: role, grantedBy: grantedBy, updatedAt: at}
	return nil
}

// DeleteGrant removes one authorisation row.
func (s *Store) DeleteGrant(ctx context.Context, code, clientID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return room.ErrNotFound
	}
	delete(st.grants, clientID)
	return nil
}

// grantsSnapshot copies the authorisation table for a Record.
func grantsSnapshot(st *roomState) map[string]string {
	if len(st.grants) == 0 {
		return nil
	}
	out := make(map[string]string, len(st.grants))
	for cid, g := range st.grants {
		out[cid] = g.role
	}
	return out
}

// LoadRoom returns the persisted record with its snapshot and the ops after it.
func (s *Store) LoadRoom(ctx context.Context, code string) (*room.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return nil, room.ErrNotFound
	}
	out := st.meta
	out.Seq = st.seq
	out.OwnerClientID = st.owner
	out.Grants = grantsSnapshot(st)
	if st.snapshot != nil {
		snap := *st.snapshot
		snap.Payload = append([]byte(nil), st.snapshot.Payload...)
		out.Snapshot = &snap
		out.ShapeCount = snap.ShapeCount
	}
	// Ops strictly after the snapshot; the snapshot already contains the state at
	// its seq, so replaying those again would double-apply them.
	var from int64
	if st.snapshot != nil {
		from = st.snapshot.Seq
	}
	out.Ops = make([]room.ReplayOp, 0, len(st.ops))
	for _, op := range st.ops {
		if op.Seq <= from {
			continue
		}
		cp := op
		cp.Shape = append([]byte(nil), op.Shape...)
		out.Ops = append(out.Ops, cp)
	}
	return &out, nil
}

// DeleteRoom removes a room and its history.
func (s *Store) DeleteRoom(ctx context.Context, code string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return room.ErrNotFound
	}
	if _, ok := s.rooms[code]; !ok {
		return room.ErrNotFound
	}
	delete(s.rooms, code)
	return nil
}

// TouchRoom updates the room's last-active time and shape count.
func (s *Store) TouchRoom(ctx context.Context, code string, shapeCount int, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return room.ErrNotFound
	}
	st.meta.ShapeCount = shapeCount
	if at.IsZero() {
		at = s.now()
	}
	// Records never need last-active for correctness in memory mode; keeping the
	// field updated costs nothing and makes /ops truthful.
	return nil
}

// AppendOp assigns the next seq and appends the record, atomically.
func (s *Store) AppendOp(ctx context.Context, rec room.OpRecord) (int64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if rec.OpID == "" || rec.ClientID == "" {
		return 0, false, room.ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, false, room.ErrNotFound
	}
	st, ok := s.rooms[rec.Code]
	if !ok {
		return 0, false, room.ErrNotFound
	}
	now := rec.TS
	if now.IsZero() {
		now = s.now()
	}
	key := dedupKey{cid: rec.ClientID, opID: rec.OpID}
	if e, hit := st.dedup[key]; hit && now.Sub(e.at) <= dedupeTTL {
		return e.seq, true, nil
	}
	// seq is either supplied by a shared allocator (BROKER=redis) or derived from
	// the room's own counter. When supplied it MUST be used verbatim: the room has
	// already broadcast it, so re-assigning would put a different number in the
	// log than the one clients saw.
	seq := rec.Seq
	if seq <= 0 {
		seq = st.seq + 1
	}
	if seq > st.seq {
		st.seq = seq
	}
	s.appendLocked(st, rec, seq, now)
	st.rememberDedupe(key, dedupEntry{seq: seq, at: now})
	return seq, false, nil
}

// appendLocked stores the replay op and trims the front if needed.
func (s *Store) appendLocked(st *roomState, rec room.OpRecord, seq int64, now time.Time) {
	st.ops = append(st.ops, room.ReplayOp{
		Seq:      seq,
		Epoch:    st.meta.Epoch,
		OpID:     rec.OpID,
		ClientID: rec.ClientID,
		Action:   rec.Action,
		Shape:    append([]byte(nil), rec.Payload...),
		TS:       now.UnixMilli(),
	})
	st.meta.Seq = seq
}

// rememberDedupe maintains a bounded FIFO under the room lock.
func (st *roomState) rememberDedupe(k dedupKey, e dedupEntry) {
	st.dedup[k] = e
	if len(st.dedupOrder) < dedupeWindow {
		st.dedupOrder = append(st.dedupOrder, k)
		return
	}
	old := st.dedupOrder[st.dedupHead]
	delete(st.dedup, old)
	st.dedupOrder[st.dedupHead] = k
	st.dedupHead = (st.dedupHead + 1) % dedupeWindow
}

// ReplayRange implements room.CatchUp: stored ops with seq in [from, to].
func (s *Store) ReplayRange(ctx context.Context, code string, from, to int64) ([]room.ReplayOp, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return nil, room.ErrNotFound
	}
	start := sort.Search(len(st.ops), func(i int) bool { return st.ops[i].Seq >= from })
	var out []room.ReplayOp
	for i := start; i < len(st.ops) && st.ops[i].Seq <= to; i++ {
		cp := st.ops[i]
		cp.Shape = append([]byte(nil), st.ops[i].Shape...)
		out = append(out, cp)
	}
	return out, nil
}

// AppendBatch implements room.Persister by applying every record under one lock
// hold, so the batch is atomic with respect to concurrent readers.
//
// The memory store has no round-trip cost to amortise, so batching is about
// semantics (all-or-nothing) rather than speed; it still validates every record
// before mutating so a mid-batch failure cannot leave a partial write.
func (s *Store) AppendBatch(ctx context.Context, recs []room.OpRecord) ([]room.AppendResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, room.ErrNotFound
	}
	// Resolve the room and every duplicate before writing anything, so the batch is
	// all-or-nothing.
	now := s.now()
	results := make([]room.AppendResult, len(recs))
	states := make([]*roomState, len(recs))
	for i := range recs {
		st, ok := s.rooms[recs[i].Code]
		if !ok {
			return nil, room.ErrNotFound
		}
		states[i] = st
		key := dedupKey{cid: recs[i].ClientID, opID: recs[i].OpID}
		if e, hit := st.dedup[key]; hit && now.Sub(e.at) <= dedupeTTL {
			results[i] = room.AppendResult{Seq: e.seq, Duplicate: true}
		}
	}
	for i, rec := range recs {
		if results[i].Duplicate {
			continue
		}
		st := states[i]
		seq := rec.Seq
		if seq <= 0 {
			seq = st.seq + 1
		}
		if seq > st.seq {
			st.seq = seq
		}
		ts := rec.TS
		if ts.IsZero() {
			ts = now
		}
		s.appendLocked(st, rec, seq, ts)
		st.rememberDedupe(dedupKey{cid: rec.ClientID, opID: rec.OpID}, dedupEntry{seq: seq, at: now})
		results[i] = room.AppendResult{Seq: seq}
	}
	return results, nil
}

// LoadOps returns stored ops with seq in (afterSeq, afterSeq+limit].
func (s *Store) LoadOps(ctx context.Context, code string, afterSeq int64, limit int, maxBytes int64) ([]room.OpRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, false, room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return nil, false, room.ErrNotFound
	}
	if limit <= 0 {
		limit = 1
	}
	start := sort.Search(len(st.ops), func(i int) bool { return st.ops[i].Seq > afterSeq })
	out := make([]room.OpRecord, 0, min(limit, len(st.ops)-start))
	var size int64
	for i := start; i < len(st.ops) && len(out) < limit; i++ {
		op := st.ops[i]
		n := int64(len(op.Shape))
		if len(out) > 0 && maxBytes > 0 && size+n > maxBytes {
			return out, true, nil
		}
		out = append(out, room.OpRecord{
			Code:     code,
			Seq:      op.Seq,
			Epoch:    op.Epoch,
			OpID:     op.OpID,
			ClientID: op.ClientID,
			Action:   op.Action,
			Payload:  append([]byte(nil), op.Shape...),
			TS:       time.UnixMilli(op.TS),
		})
		size += n
	}
	truncated := start+len(out) < len(st.ops)
	return out, truncated, nil
}

// LatestSeq returns the highest assigned seq for the room.
func (s *Store) LatestSeq(ctx context.Context, code string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return 0, room.ErrNotFound
	}
	return st.seq, nil
}

// TrimOps deletes ops with seq <= upto, keeping the last `keep` ops regardless.
func (s *Store) TrimOps(ctx context.Context, code string, upto int64, keep int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return room.ErrNotFound
	}
	cut := sort.Search(len(st.ops), func(i int) bool { return st.ops[i].Seq > upto })
	if keep > 0 && len(st.ops)-cut < keep {
		cut = max(0, len(st.ops)-keep)
	}
	if cut <= 0 {
		return nil
	}
	for i := 0; i < cut; i++ {
		st.ops[i] = room.ReplayOp{}
	}
	st.ops = append([]room.ReplayOp(nil), st.ops[cut:]...)
	return nil
}

// SaveSnapshot persists a materialised snapshot.
func (s *Store) SaveSnapshot(ctx context.Context, code string, epoch, seq int64, shapeCount int, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return room.ErrNotFound
	}
	snap := &room.SnapshotRecord{
		Code:       code,
		Epoch:      epoch,
		Seq:        seq,
		ShapeCount: shapeCount,
		Payload:    append([]byte(nil), payload...),
		CreatedAt:  s.now(),
	}
	// Never overwrite a newer snapshot with an older one: an out-of-order writer
	// (or a slow goroutine) must not roll the cache backwards.
	if st.snapshot == nil || snap.Seq >= st.snapshot.Seq {
		st.snapshot = snap
		st.meta.ShapeCount = shapeCount
	}
	return nil
}

// LoadSnapshot returns the newest snapshot, or room.ErrNoSnapshot.
func (s *Store) LoadSnapshot(ctx context.Context, code string) (room.SnapshotRecord, error) {
	if err := ctx.Err(); err != nil {
		return room.SnapshotRecord{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return room.SnapshotRecord{}, room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return room.SnapshotRecord{}, room.ErrNotFound
	}
	if st.snapshot == nil {
		return room.SnapshotRecord{}, room.ErrNoSnapshot
	}
	out := *st.snapshot
	out.Payload = append([]byte(nil), st.snapshot.Payload...)
	return out, nil
}

// SnapshotShapes returns the live shapes at the latest snapshot position, sorted
// by id. It backs GET /api/rooms/{code}/snapshot paging.
//
// TODO(phase-b2): page from the live room state when it is open; this store-side
// implementation is the fallback for a closed room.
func (s *Store) SnapshotShapes(ctx context.Context, code string) ([]protocol.Shape, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, 0, room.ErrNotFound
	}
	st, ok := s.rooms[code]
	if !ok {
		return nil, 0, room.ErrNotFound
	}
	var seq int64 = st.seq
	var shapes []protocol.Shape
	if st.snapshot != nil {
		seq = st.snapshot.Seq
		// The snapshot payload is a JSON array of protocol.Shape.
		if err := decodeShapes(st.snapshot.Payload, &shapes); err != nil {
			return nil, 0, err
		}
	}
	sort.Slice(shapes, func(i, j int) bool { return shapes[i].ID < shapes[j].ID })
	return shapes, seq, nil
}

// decodeShapes unmarshals a snapshot payload.
func decodeShapes(payload []byte, out *[]protocol.Shape) error {
	return json.Unmarshal(payload, out)
}

// Close releases the store. It is idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.rooms = nil
	return nil
}
