// Package room implements the authoritative, per-room event loop.
//
// # Concurrency model
//
// Exactly one goroutine owns a Room. It is the only code that may touch seq,
// shapes, members or the dedupe ring, and it never holds a lock while doing so:
// the fields have no lock at all. Everything else talks to the room through one
// of two channels:
//
//	commands <-chan command   inbound work (join, leave, op, resync, close)
//	done     chan struct{}    closed when the loop has flushed and exited
//
// This is what makes "broadcast in seq order" a structural property instead of a
// discipline: the seq is assigned and the frame is enqueued by the same goroutine
// that owns the ordering.
//
// # Non-blocking outbound delivery
//
// The room must never block on a slow client. Every outbound frame goes through
// Member.Send, which must not block (internal/ws implements it with a bounded
// channel and a non-blocking send). A member that cannot accept a frame is
// marked lagging and, if it misses a persistent frame, is evicted with close code
// 4001 by the connection layer — never by the room blocking.
package room

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codraw/codraw/backend/internal/protocol"
)

// Errors returned by the room API.
var (
	// ErrClosed means the room has shut down.
	ErrClosed = errors.New("room: closed")
	// ErrFull means the room's connection budget is exhausted.
	ErrFull = errors.New("room: full")
)

// Options configures a room.
type Options struct {
	Code  string
	Title string
	Epoch int64
	// Seq is the last persisted seq, so a restarted instance continues the log.
	Seq int64
	// ShapeCount persists the live-shape count for /api/rooms.
	ShapeCount int
	// OwnerClientID / Grants restore the permission model from the store
	// (docs/PERMISSIONS.md §7). Empty owner means an unowned room.
	OwnerClientID string
	Grants        map[string]string
	// Limits comes from config and is advertised verbatim in `joined`.
	Limits Limits
	// Retention is the number of ops kept in memory for resync.events.
	Retention int
	// DedupeSize / DedupeTTL bound the (clientId, opId) idempotency window.
	DedupeSize int
	DedupeTTL  time.Duration
	// LeaveGrace delays presence.leave after an unexpected disconnect.
	LeaveGrace time.Duration
	// CursorThrottle is the per-connection cursor coalescing window (50ms).
	CursorThrottle time.Duration
	// InboxSize is the room command channel depth. Larger absorbs bursts without
	// dropping write intents.
	InboxSize int
	// SnapshotEvery / SnapshotShapes / SnapshotBytes drive snapshot chunking.
	SnapshotEvery      int
	SnapshotChunkShape int
	SnapshotChunkBytes int
	// Persist appends to the authoritative store and assigns the durable seq.
	// When nil the room keeps its log in memory only (STORE=memory).
	Persist Persister
	// Broadcast publishes an already-sequenced op to other instances.
	// When nil the room only fans out locally.
	Broadcast Broadcaster
	// SeqSource allocates a globally unique seq (Redis INCR) in multi-instance
	// mode. Nil means "single writer, keep the local counter".
	SeqSource SeqSource
	// CatchUp replays a seq range from the store. Nil disables gap repair, which is
	// correct for single-instance memory mode (no gaps can exist).
	CatchUp CatchUp
	// InstanceID is used to suppress self-echo from the broker.
	InstanceID string
	// Logger is required; it is already tagged with the room code by the caller.
	Logger *slog.Logger
	// Now overrides the clock in tests.
	Now func() time.Time
	// Metrics receives counters; nil is allowed in tests.
	Metrics MetricsSink
	// OnShapeCount persists the live-shape count asynchronously.
	OnShapeCount func(code string, n int)
	// SnapshotHook is called with the serialised snapshot when one is due.
	// It must not block; the room calls it on its own goroutine's critical path,
	// so implementations copy the bytes and hand them to a background writer.
	SnapshotHook func(code string, epoch, seq int64, shapeCount int, payload []byte)
}

// Limits mirrors the protocol limits the room must enforce.
type Limits struct {
	MaxConnPerRoom   int
	MaxConnTotal     int
	MaxMessageBytes  int
	MaxShapeBytes    int
	MaxShapesPerRoom int
	OpsPerSecond     int
	OpsBurst         int
	CursorPerSecond  int
	ResumeMaxOps     int
	ResumeMaxBytes   int64
	MaxNameRunes     int
}

// Persister is the persistence port the room needs. It is deliberately narrow:
// the room never sees the whole store interface.
//
// AssignSeq+Append must be atomic per room so a seq is never issued without a
// stored op (docs/DATA-MODEL.md). Implementations return ErrDuplicate when
// (room, clientID, opID) was already stored, with the original seq.
type Persister interface {
	// AppendOp assigns seq and appends the record. duplicate is true when the
	// (clientID, opID) pair was already present, in which case seq is the seq
	// assigned the first time.
	//
	// AppendOp is the single-op path used by tests and by the slow path; the room's
	// hot path uses AppendBatch.
	AppendOp(ctx context.Context, rec OpRecord) (seq int64, duplicate bool, err error)

	// AppendBatch persists a batch of records in one round trip.
	//
	// Semantics are all-or-nothing: either every record is durable when it returns
	// nil, or nothing is. The room only broadcasts after a nil return, which is how
	// "persist before broadcast" survives batching.
	//
	// Each record arrives with its Seq already assigned by the room (or by the
	// shared SeqSource); implementations must store them under exactly those seqs
	// and must not renumber. Duplicates are reported per index in dup, and their
	// original seq is written back into results[i].Seq.
	AppendBatch(ctx context.Context, recs []OpRecord) (results []AppendResult, err error)
	// LoadOps returns stored ops with seq in (afterSeq, afterSeq+limit].
	LoadOps(ctx context.Context, code string, afterSeq int64, limit int, maxBytes int64) ([]OpRecord, bool, error)
	// SaveSnapshot persists a materialised snapshot.
	SaveSnapshot(ctx context.Context, code string, epoch, seq int64, shapeCount int, payload []byte) error
	// LoadSnapshot returns the newest snapshot, or ErrNoSnapshot.
	LoadSnapshot(ctx context.Context, code string) (SnapshotRecord, error)
	// SaveOwner persists the room's owner ("" means unowned), which is what makes
	// the ownership survive a restart (docs/PERMISSIONS.md §7).
	SaveOwner(ctx context.Context, code, ownerClientID string) error
	// SaveGrant upserts one (room, clientId) → editor|viewer authorisation.
	SaveGrant(ctx context.Context, code, clientID, role, grantedBy string, at time.Time) error
	// DeleteGrant drops one authorisation row.
	DeleteGrant(ctx context.Context, code, clientID string) error
}

// OpRecord is one persisted operation.
type OpRecord struct {
	Code     string
	Seq      int64
	Epoch    int64
	OpID     string
	ClientID string
	AuthorID string
	Action   string
	ShapeID  string
	Payload  []byte
	TS       time.Time
}

// AppendResult is the per-record outcome of AppendBatch.
type AppendResult struct {
	// Seq is the seq the record is stored under. For a duplicate this is the seq
	// assigned the first time, which may differ from the submitted one.
	Seq int64
	// Duplicate is true when (ClientID, OpID) was already persisted.
	Duplicate bool
}

// SnapshotRecord is a persisted snapshot.
type SnapshotRecord struct {
	Code       string
	Epoch      int64
	Seq        int64
	ShapeCount int
	Payload    []byte
	CreatedAt  time.Time
}

// ErrNoSnapshot is returned by LoadSnapshot when the room has never been
// snapshotted.
var ErrNoSnapshot = errors.New("room: no snapshot")

// Broadcaster publishes sequenced ops to other instances.
type Broadcaster interface {
	PublishOp(ctx context.Context, code string, seq int64, frame []byte) error
	// PublishOpBatch publishes several already-sequenced frames in one round trip,
	// preserving order. Group commit uses this so a batch costs one publish, not N.
	PublishOpBatch(ctx context.Context, code string, seqs []int64, frames [][]byte) error
}

// CatchUp is the store capability the room needs to repair a gap it cannot fill
// from the live broker stream.
//
// Two situations need it:
//
//   - An instance opened the room *after* another instance published ops. Pub/sub is
//     not a queue, so those frames are gone and only the store still has them.
//   - A frame was dropped from the bounded out-of-order buffer.
//
// Without this the instance would keep serving a room missing those shapes until a
// client forced a full snapshot, which is visible as "a shape I drew is missing on
// the other server".
type CatchUp interface {
	// ReplayRange returns stored ops with seq in [from, to], ordered by seq.
	ReplayRange(ctx context.Context, code string, from, to int64) ([]ReplayOp, error)
}

// SeqSource allocates a globally unique sequence number for a room.
//
// It exists as a separate port from Broadcaster because they are different
// concerns: seq uniqueness is required for correctness, fan-out is required for
// latency. In multi-instance mode both come from Redis, but a future broker (NATS,
// etc.) could implement either independently.
type SeqSource interface {
	NextSeq(ctx context.Context, code string) (int64, error)
	// NextSeqRange reserves n consecutive seqs and returns the upper bound, so the
	// room can hand out [hi-n+1, hi] locally. One INCRBY instead of n INCRs.
	NextSeqRange(ctx context.Context, code string, n int) (hi int64, err error)
}

// MetricsSink receives room-level counters. All methods must be safe for
// concurrent use.
type MetricsSink interface {
	ObserveBroadcast(d time.Duration)
	IncOpsRejected(reason string)
	IncCursorDropped()
	ObserveMessage(dir, typ string)
	IncSlowConsumerKick()
	IncSeqGap()
}

// Member is one connected client. Implementations must make Send non-blocking.
type Member interface {
	// User returns the current identity of the connection. Role is mutable: the
	// room updates it when ownership or authorisation changes
	// (docs/PERMISSIONS.md §5), so implementations must return the latest value.
	User() protocol.User
	// SetRole updates the cached role published by User. It must be safe to call
	// from the room goroutine and must not block.
	SetRole(role string)
	// Send enqueues a frame. It must never block; false means the outbound
	// buffer is full.
	Send(frame []byte) bool
	// Kick closes the connection with the given WebSocket close code. It must be
	// idempotent and safe to call from the room goroutine.
	Kick(code int, reason string)
	// KickWithError queues an `error` frame and then closes with closeCode.
	//
	// It exists because a permission violation must be reported *before* the
	// close (docs/PERMISSIONS.md §8): Kick alone writes the close frame directly
	// and the queued error would be discarded with the connection. The
	// implementation must flush the envelope before the close frame.
	KickWithError(closeCode int, code protocol.ErrCode, msg string)
}

// room is the live state. Every field below is owned by the loop goroutine
// except where noted.
type room struct {
	opts   Options
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	code  string
	title string
	epoch int64
	seq   int64

	shapes map[string]*protocol.Shape
	// liveShapes is the number of non-deleted shapes, maintained incrementally.
	// Recomputing it by walking the map on every op made commits O(n) each and the
	// whole session O(n^2); this counter is the fix.
	liveShapes int
	// members is keyed by member pointer; iteration order is irrelevant because
	// broadcasts fan out from a snapshot slice.
	members map[Member]*memberState
	// dedup maps (clientId|opId) to the seq it was first applied with.
	dedup *dedupRing
	// log_ is the in-memory ring of recent ops for resync.events.
	log_ *opRing

	// pendingLeave holds members that disconnected and whose presence.leave is
	// still inside the grace window, keyed by the leave deadline.
	pendingLeave map[Member]leaveTimer
	// remotePending buffers out-of-order frames from other instances, keyed by seq.
	remotePending map[int64][]byte

	// --- permission model (docs/PERMISSIONS.md §2, §3) -------------------
	//
	// ownerClientID / grants / joinOrder / lastRoles are only ever read and
	// written by the room goroutine, like every other field here.
	//
	// ownerClientID is the current owner; "" means the room is unowned and the
	// next entrant takes over.
	ownerClientID string
	// grants is the durable authorisation table: cid -> editor|viewer. The owner
	// is never stored here (ownership is a separate, exclusive concept).
	grants map[string]string
	// joinOrder lists client ids by first entrance, oldest first, and drives
	// ownership inheritance (§2.2).
	joinOrder []string
	// lastRoles restores a role after a reconnect (§4.4). Revocation writes
	// viewer here too, so a revoked grant cannot come back.
	lastRoles map[string]string
	// lastRoleOrder bounds lastRoles (oldest cid evicted first).
	lastRoleOrder []string

	// pending holds validated ops awaiting their group-commit batch write. Only the
	// room goroutine touches it, so it needs no lock. The batch-window timer lives in
	// the loop rather than here.
	pending []*pendingOp

	opsSinceSnapshot int
	closed           bool
	// batchStats accumulates group-commit timings for the periodic report.
	batchStats batchStats
	// reportTicks counts loop ticks between reports.
	reportTicks int

	// commands is the only inbound channel.
	commands  chan command
	done      chan struct{}
	closeOnce sync.Once

	// counters is read by Stats from other goroutines.
	snapshotStats atomic.Pointer[Stats]
}

type memberState struct {
	member Member
	// user is the last published identity. It is refreshed whenever the role
	// changes so a presence broadcast never carries a stale role.
	user protocol.User
	// readonly pins this connection to viewer regardless of room grants. It is
	// set when the handshake used a readonly token (docs/PERMISSIONS.md §4.2).
	readonly bool
	// limiter is the per-connection op token bucket. Owned by the loop.
	limiter tokenBucket
	// cursor is the last accepted cursor position, used for 50ms coalescing.
	cursorPending bool
	cursorX       float64
	cursorY       float64
	cursorAt      time.Time
	// lagging is set once a Send failed; the member is evicted on the next
	// persistent frame.
	lagging  bool
	joinedAt time.Time
	// announced is false until joined has been queued and presence.join may be
	// broadcast, so no frame can overtake joined on the wire.
	announced bool
}

type leaveTimer struct {
	deadline time.Time
	user     protocol.User
	// readonly pins the pending member to viewer, matching memberState.readonly.
	readonly bool
}

// batchStats accumulates group-commit counters between reports.
type batchStats struct {
	batches   int64
	ops       int64
	appendNs  int64
	publishNs int64
}

// Stats is a point-in-time view for /api/rooms and /ops.
type Stats struct {
	Code          string
	Title         string
	Epoch         int64
	Seq           int64
	Online        int
	ShapeCount    int
	CreatedAt     time.Time
	LastActiveAt  time.Time
	PendingLeaves int
	// OwnerClientID is the current owner, "" when the room is unowned
	// (docs/PERMISSIONS.md §2.2).
	OwnerClientID string
}

// Handle is the external handle to a running room.
type Handle struct {
	r *room
}

// inboxSize returns the configured command channel depth with a sane floor.
func inboxSize(n int) int {
	if n <= 0 {
		return 8192
	}
	return n
}

// New creates a room handle and starts its goroutine.
//
// The returned handle is safe to use from any goroutine; every method funnels
// into the room's command channel or reads an atomically published snapshot.
func New(opts Options) *Handle {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Retention <= 0 {
		opts.Retention = 5000
	}
	if opts.DedupeSize <= 0 {
		opts.DedupeSize = 8192
	}
	if opts.DedupeTTL <= 0 {
		opts.DedupeTTL = 10 * time.Minute
	}
	if opts.LeaveGrace <= 0 {
		opts.LeaveGrace = 10 * time.Second
	}
	if opts.SnapshotEvery <= 0 {
		opts.SnapshotEvery = 500
	}
	if opts.SnapshotChunkShape <= 0 {
		opts.SnapshotChunkShape = 500
	}
	if opts.SnapshotChunkBytes <= 0 {
		opts.SnapshotChunkBytes = 48 << 10
	}
	if opts.Epoch == 0 {
		opts.Epoch = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &room{
		opts:          opts,
		log:           opts.Logger,
		ctx:           ctx,
		cancel:        cancel,
		code:          opts.Code,
		title:         opts.Title,
		epoch:         opts.Epoch,
		seq:           opts.Seq,
		shapes:        make(map[string]*protocol.Shape),
		members:       make(map[Member]*memberState),
		dedup:         newDedupRing(opts.DedupeSize, opts.DedupeTTL),
		log_:          newOpRing(opts.Retention),
		pendingLeave:  make(map[Member]leaveTimer),
		remotePending: make(map[int64][]byte),
		ownerClientID: opts.OwnerClientID,
		grants:        make(map[string]string, len(opts.Grants)),
		joinOrder:     make([]string, 0, 16),
		lastRoles:     make(map[string]string, 16),
		commands:      make(chan command, inboxSize(opts.InboxSize)),
		done:          make(chan struct{}),
	}
	for cid, role := range opts.Grants {
		if cid == "" || !protocol.ValidGrantRole(role) {
			continue
		}
		r.grants[cid] = role
	}
	if r.ownerClientID == "" {
		// An unowned room cannot legitimately carry authorisations: they are
		// something an owner granted, and becoming unowned clears them
		// (clearAuthorisationsLocked). A record that still has rows (written by an
		// older build, or read mid-transition) must not resurrect them, or the
		// next entrant would join as editor into a room with nobody able to grant.
		r.grants = make(map[string]string)
	}
	r.publishStats()
	go r.loop()
	return &Handle{r: r}
}

// Code returns the room code.
func (h *Handle) Code() string { return h.r.code }

// Stats returns the last published stats snapshot.
func (h *Handle) Stats() Stats {
	if s := h.r.snapshotStats.Load(); s != nil {
		return *s
	}
	return Stats{Code: h.r.code}
}

// Online returns the number of live members.
func (h *Handle) Online() int { return h.Stats().Online }

// Done is closed when the room goroutine has exited.
func (h *Handle) Done() <-chan struct{} { return h.r.done }

// SnapshotShapes returns the live shapes as of the last published stats.
//
// It has to go through the room goroutine because shapes are owned by it, so the
// reply is a copy. The copy cost is bounded by the room size and this is a
// low-frequency REST endpoint.
func (h *Handle) SnapshotShapes(ctx context.Context) ([]protocol.Shape, error) {
	reply := make(chan []protocol.Shape, 1)
	select {
	case h.r.commands <- command{kind: cmdShapes, shapesReply: reply}:
	case <-h.r.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case shapes := <-reply:
		return shapes, nil
	case <-h.r.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close asks the room to flush and stop, then waits for the goroutine.
func (h *Handle) Close(ctx context.Context) error {
	select {
	case h.r.commands <- command{kind: cmdClose}:
	case <-h.r.ctx.Done():
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-h.r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Join adds a member. It blocks until the room goroutine has accepted or
// rejected it, so the caller can report the admission result synchronously.
func (h *Handle) Join(ctx context.Context, m Member) error {
	_, err := h.JoinConn(ctx, m, false)
	return err
}

// JoinResult is the authoritative handshake outcome the transport needs for
// `joined` (docs/PERMISSIONS.md §4.2, §4.3).
type JoinResult struct {
	Role          string
	OwnerClientID string
	Online        int
}

// JoinConn adds a member and returns its freshly decided role.
//
// readonly marks a connection that presented a readonly token: such a
// connection is pinned to viewer forever (§4.2 rule 1), even if its clientId
// matches the owner.
func (h *Handle) JoinConn(ctx context.Context, m Member, readonly bool) (JoinResult, error) {
	reply := make(chan joinReply, 1)
	select {
	case h.r.commands <- command{kind: cmdJoin, member: m, readonly: readonly, reply: reply}:
	case <-h.r.done:
		return JoinResult{}, ErrClosed
	case <-ctx.Done():
		return JoinResult{}, ctx.Err()
	}
	select {
	case res := <-reply:
		return JoinResult{Role: res.role, OwnerClientID: res.owner, Online: res.online}, res.err
	case <-h.r.done:
		return JoinResult{}, ErrClosed
	case <-ctx.Done():
		return JoinResult{}, ctx.Err()
	}
}

// Leave removes a member (explicit bye or connection teardown).
func (h *Handle) Leave(m Member) {
	select {
	case h.r.commands <- command{kind: cmdLeave, member: m}:
	case <-h.r.done:
	default:
		// The room is busy or gone. Dropping the leave is safe: the connection
		// layer also closes the socket, and the room reaps dead members when it
		// notices a failed Send.
	}
}

// Hello processes the client's hello frame and triggers the resync reply.
func (h *Handle) Hello(m Member, env *protocol.Envelope, p *protocol.HelloPayload) {
	h.post(command{kind: cmdHello, member: m, env: env, hello: p})
}

// Op processes an op.add frame.
func (h *Handle) Op(m Member, env *protocol.Envelope, p *protocol.OpAddPayload, raw []byte) {
	h.post(command{kind: cmdOp, member: m, env: env, op: p, raw: raw})
}

// Cursor processes a presence.cursor frame.
func (h *Handle) Cursor(m Member, p *protocol.CursorPayload) {
	h.post(command{kind: cmdCursor, member: m, cursor: p})
}

// Resync processes a resync.request frame.
func (h *Handle) Resync(m Member, p *protocol.ResyncRequestPayload) {
	h.post(command{kind: cmdResync, member: m, resync: p})
}

// Ping processes a ping frame.
func (h *Handle) Ping(m Member, p *protocol.PingPayload) {
	h.post(command{kind: cmdPing, member: m, ping: p})
}

// Bye processes an explicit leave.
func (h *Handle) Bye(m Member) {
	h.post(command{kind: cmdBye, member: m})
}

// Grant applies a room.grant. Authorisation happens inside the room goroutine
// against live state, so a revoked owner cannot keep granting.
func (h *Handle) Grant(m Member, p *protocol.GrantPayload) {
	h.post(command{kind: cmdGrant, member: m, grant: p})
}

// Transfer applies a room.transfer.
func (h *Handle) Transfer(m Member, p *protocol.TransferPayload) {
	h.post(command{kind: cmdTransfer, member: m, transfer: p})
}

// NotifyJoined broadcasts presence.join for a member that just completed the
// handshake. It exists as its own command so the transport can guarantee the
// order joined → presence.join without the room knowing about sockets.
func (h *Handle) NotifyJoined(m Member) {
	h.post(command{kind: cmdJoined, member: m})
}

// BroadcastRemote applies an op that another instance already sequenced. It is
// how a Redis subscriber feeds the room. Returns false when the room rejected it
// (closed or duplicate).
func (h *Handle) BroadcastRemote(seq int64, frame []byte) bool {
	reply := make(chan bool, 1)
	c := command{kind: cmdRemote, remoteSeq: seq, frame: frame, remoteReply: reply}
	select {
	case h.r.commands <- c:
	case <-h.r.done:
		return false
	default:
		return false
	}
	select {
	case ok := <-reply:
		return ok
	case <-h.r.done:
		return false
	}
}

// post sends a command without blocking the caller. Read pumps must not stall
// on a busy room; the caller has already applied its own rate limit, and a
// dropped command is recoverable via resync while a blocked read pump would
// stall the TCP window.
func (h *Handle) post(c command) {
	// Write intents (op.add) and permission changes must never be silently
	// dropped: the client holds them in its pending queue and waits for an ack,
	// so a dropped op turns into a stuck pending entry and an ever-growing
	// latency, not a clean failure. When the inbox is saturated, wait briefly for
	// room rather than discarding.
	//
	// Ephemeral commands (cursor, ping) are the opposite: they are re-sent by design,
	// so dropping under load is correct and keeps the room from being flooded.
	if c.kind == cmdOp || c.kind == cmdGrant || c.kind == cmdTransfer {
		// A short bounded wait, not an unbounded one: blocking a read pump on a full
		// inbox stalls that connection's socket, which prevents it from even receiving
		// the resync it needs. 20ms absorbs a burst without turning room saturation
		// into a client-visible freeze.
		timer := time.NewTimer(inboxPushTimeout)
		defer timer.Stop()
		select {
		case h.r.commands <- c:
			return
		case <-h.r.done:
			return
		case <-timer.C:
			if h.r.opts.Metrics != nil {
				h.r.opts.Metrics.IncOpsRejected(string(protocol.CodeServerBusy))
			}
			h.rejectBusy(c)
			return
		}
	}
	select {
	case h.r.commands <- c:
	case <-h.r.done:
	default:
		if h.r.opts.Metrics != nil {
			h.r.opts.Metrics.IncOpsRejected(string(protocol.CodeServerBusy))
		}
	}
}

// inboxPushTimeout bounds how long a write intent waits for room in the inbox. It is
// short enough that a truly wedged room fails fast and long enough to absorb a burst.
const inboxPushTimeout = 20 * time.Millisecond

// rejectBusy answers a write intent that could not be enqueued with a retryable ack,
// so the client's pending op is cleared instead of hanging.
func (h *Handle) rejectBusy(c command) {
	if c.member == nil {
		return
	}
	opID := ""
	if c.op != nil {
		opID = c.op.OpID
	}
	payload := protocol.AckPayload{
		OpID:    opID,
		Status:  protocol.AckRejected,
		Code:    protocol.CodeServerBusy,
		Message: "server busy, retry",
	}
	frame, err := protocol.Marshal(protocol.TypeAck, h.r.code, c.member.User().ClientID, 0, h.r.opts.Now().UnixMilli(), payload)
	if err == nil {
		c.member.Send(frame)
	}
}

// publishStats refreshes the atomically readable snapshot. Called by the loop
// after every mutation that changes a counter.
func (r *room) publishStats() {
	s := &Stats{
		Code:          r.code,
		Title:         r.title,
		Epoch:         r.epoch,
		Seq:           r.seq,
		Online:        len(r.members),
		ShapeCount:    r.liveShapes,
		LastActiveAt:  r.opts.Now(),
		PendingLeaves: len(r.pendingLeave),
		OwnerClientID: r.ownerClientID,
	}
	r.snapshotStats.Store(s)
}

// liveShapeCount counts non-deleted shapes by walking the map. It is only for
// reconciliation (startup, replay, tests); the hot path uses r.liveShapes.
func liveShapeCount(shapes map[string]*protocol.Shape) int {
	n := 0
	for _, s := range shapes {
		if !s.Deleted {
			n++
		}
	}
	return n
}

// setShape stores a shape and keeps liveShapes in step with the deleted flag. Every
// mutation of r.shapes must go through here (or adjust liveShapes explicitly) so the
// counter cannot drift.
func (r *room) setShape(s *protocol.Shape) {
	prev, existed := r.shapes[s.ID]
	wasLive := existed && !prev.Deleted
	isLive := !s.Deleted
	switch {
	case !wasLive && isLive:
		r.liveShapes++
	case wasLive && !isLive:
		r.liveShapes--
	}
	r.shapes[s.ID] = s
}

// sortedShapes returns live shapes ordered by createdAt then id, which is the
// P0 z-order (docs/PROTOCOL.md §5.5: "explicit z-order not supported"). The
// order must be stable so a snapshot and a replay produce the same stacking.
func sortedShapes(shapes map[string]*protocol.Shape) []protocol.Shape {
	out := make([]protocol.Shape, 0, len(shapes))
	for _, s := range shapes {
		if s.Deleted {
			continue
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// marshalShape re-encodes a shape, ignoring the error because a shape that came
// from our own validated map can always be encoded.
func marshalShape(s *protocol.Shape) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
