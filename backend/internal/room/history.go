package room

import (
	"time"

	"github.com/codraw/codraw/backend/internal/protocol"
)

// dedupRing is the (clientId, opId) → seq idempotency window.
//
// Ownership: only the room goroutine touches it, so there are no locks. Eviction
// is two-dimensional per docs/PROTOCOL.md §6.5: by count (capacity) and by age
// (ttl). A single map plus a FIFO slice is enough because entries are inserted
// in time order; a full LRU would add allocation on the hot path for no benefit.
type dedupRing struct {
	capacity int
	ttl      time.Duration
	seqs     map[dedupKey]dedupVal
	order    []dedupKey
	head     int
}

type dedupKey struct {
	cid  string
	opID string
}

type dedupVal struct {
	seq int64
	at  time.Time
}

func newDedupRing(capacity int, ttl time.Duration) *dedupRing {
	if capacity <= 0 {
		capacity = 8192
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &dedupRing{
		capacity: capacity,
		ttl:      ttl,
		seqs:     make(map[dedupKey]dedupVal, capacity),
		order:    make([]dedupKey, 0, capacity),
	}
}

// get returns the seq assigned to (cid, opId) if it is still inside the window.
// Expired entries are treated as absent; the caller then processes the op again,
// which is the accepted risk documented in docs/PROTOCOL.md §6.5.
func (d *dedupRing) get(cid, opID string, now time.Time) (int64, bool) {
	v, ok := d.seqs[dedupKey{cid, opID}]
	if !ok {
		return 0, false
	}
	if now.Sub(v.at) > d.ttl {
		delete(d.seqs, dedupKey{cid, opID})
		return 0, false
	}
	return v.seq, true
}

// put records (cid, opId) → seq and evicts the oldest entry when full.
func (d *dedupRing) put(cid, opID string, seq int64, now time.Time) {
	k := dedupKey{cid, opID}
	if _, exists := d.seqs[k]; exists {
		d.seqs[k] = dedupVal{seq: seq, at: now}
		return
	}
	d.seqs[k] = dedupVal{seq: seq, at: now}
	if len(d.order) < d.capacity {
		d.order = append(d.order, k)
		return
	}
	old := d.order[d.head]
	delete(d.seqs, old)
	d.order[d.head] = k
	d.head = (d.head + 1) % d.capacity
}

// len reports the number of live entries (for tests and /ops).
func (d *dedupRing) len() int { return len(d.seqs) }

// logEntry is one op or role change retained in memory for resync.events.
type logEntry struct {
	Seq      int64
	Epoch    int64
	OpID     string
	ClientID string
	Action   string
	Shape    []byte
	ID       string
	Points   [][]float64
	By       protocol.By
	TS       int64

	// Type is "" for an op and "member.role" for a permission change; the role
	// fields below are only set for the latter.
	Type      string
	Role      string
	GrantedBy string
}

// toEvent converts a retained entry into the protocol's event element.
func (e logEntry) toEvent() protocol.Event {
	if e.Type == protocol.TypeMemberRole {
		return protocol.Event{
			Seq:       e.Seq,
			Type:      protocol.TypeMemberRole,
			ClientID:  e.ClientID,
			Role:      e.Role,
			GrantedBy: e.GrantedBy,
			TS:        e.TS,
		}
	}
	return protocol.Event{
		Seq:    e.Seq,
		OpID:   e.OpID,
		Action: e.Action,
		Shape:  e.Shape,
		ID:     e.ID,
		Points: e.Points,
		By:     e.By,
		TS:     e.TS,
	}
}

// opRing is the bounded in-memory copy of the room's recent event log.
//
// It only needs to answer two questions: "what is the highest retained seq" and
// "give me entries from A to B contiguously". Because seq is gap-free and
// eviction is always from the front, a slice with a moving start index answers
// both in O(1)/O(range).
//
// Capacity is EVENT_RETENTION (default 5000), which must be >= resumeMaxOps so a
// client that just disconnected can always be caught up without a snapshot.
type opRing struct {
	capacity int
	entries  []logEntry
	// start is the index of the oldest retained entry in entries.
	start int
}

func newOpRing(capacity int) *opRing {
	if capacity <= 0 {
		capacity = 5000
	}
	return &opRing{capacity: capacity, entries: make([]logEntry, 0, capacity)}
}

// put appends an entry, dropping the oldest when at capacity.
func (o *opRing) put(e logEntry) {
	if len(o.entries) == o.capacity {
		// Shift by reusing the backing array in place; entries is small (5000
		// max) and this happens once per op at steady state.
		copy(o.entries, o.entries[1:])
		o.entries[len(o.entries)-1] = e
		return
	}
	o.entries = append(o.entries, e)
}

// get returns the entry with the given seq.
func (o *opRing) get(seq int64) (logEntry, bool) {
	if len(o.entries) == 0 {
		return logEntry{}, false
	}
	// Entries are contiguous, so the index is an offset from the first seq.
	first := o.entries[0].Seq
	idx := seq - first
	if idx < 0 || idx >= int64(len(o.entries)) {
		return logEntry{}, false
	}
	return o.entries[idx], true
}

// covers reports whether every seq in [from, to] is retained.
func (o *opRing) covers(from, to int64) bool {
	if from > to {
		return true
	}
	if len(o.entries) == 0 {
		return false
	}
	first := o.entries[0].Seq
	last := o.entries[len(o.entries)-1].Seq
	return from >= first && to <= last
}

// lastSeq returns the highest retained seq, or 0.
func (o *opRing) lastSeq() int64 {
	if len(o.entries) == 0 {
		return 0
	}
	return o.entries[len(o.entries)-1].Seq
}

// bulk inserts many entries, used when loading history from the store.
func (o *opRing) bulk(es []logEntry) {
	for _, e := range es {
		o.put(e)
	}
}

// tokenBucket is a per-connection op limiter owned by the room goroutine.
// No lock: the room goroutine is the only caller.
type tokenBucket struct {
	tokens float64
	last   time.Time
	rate   float64
	burst  float64
}

func newTokenBucket(rate float64, burst int, now time.Time) tokenBucket {
	return tokenBucket{tokens: float64(burst), last: now, rate: rate, burst: float64(burst)}
}

// allow consumes one token if available.
func (b *tokenBucket) allow(now time.Time) bool {
	if now.After(b.last) {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
