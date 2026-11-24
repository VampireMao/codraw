// Package broker defines cross-instance fan-out and the global seq source.
//
// Two capabilities are required when more than one instance serves a room
// (docs/ARCHITECTURE.md §4):
//
//   - A globally monotonic sequence per room, because seq must be unique across
//     instances. Redis INCR supplies it.
//   - A pub/sub channel per room delivering already-sequenced frames to every
//     instance, so each fans out to its own local members.
//
// With BROKER=memory both are provided trivially: the instance is the only
// writer, so it allocates seq itself and there is nobody to publish to. That is
// the zero-dependency default required by the PRD.
package broker

import (
	"context"
	"errors"
	"time"
)

// Errors returned by broker implementations.
var (
	// ErrClosed means the broker is shutting down.
	ErrClosed = errors.New("broker: closed")
	// ErrUnavailable means the backend cannot be reached.
	ErrUnavailable = errors.New("broker: unavailable")
)

// Broker is the cross-instance port.
//
// Implementations must be safe for concurrent use.
type Broker interface {
	// NextSeq atomically allocates and returns the next seq for a room.
	//
	// Memory mode returns 0, meaning "the caller is the only writer, keep your
	// local counter". Redis mode returns a globally unique value.
	NextSeq(ctx context.Context, room string) (int64, error)

	// NextSeqRange reserves n consecutive seqs and returns the upper bound, so the
	// room can hand out [hi-n+1, hi] locally with one round trip instead of n.
	// Memory mode returns 0 for the same reason as NextSeq.
	NextSeqRange(ctx context.Context, room string, n int) (int64, error)

	// Publish sends an already-sequenced frame to every instance subscribed to
	// the room, including this one. Subscribers must drop their own messages by
	// comparing Message.Origin with their instance ID: the publishing instance
	// has already fanned out locally.
	Publish(ctx context.Context, room string, seq int64, frame []byte) error

	// PublishBatch publishes several sequenced frames for one room in a single round
	// trip. Order within the batch is preserved, which matters because remote rooms
	// apply frames by seq. Implementations may pipeline or coalesce internally.
	PublishBatch(ctx context.Context, room string, msgs []Outbound) error

	// Subscribe registers h for every frame published for a room. The returned
	// cancel function unsubscribes; calling it twice is safe.
	//
	// h is invoked from a dedicated goroutine, never concurrently for the same
	// subscription.
	Subscribe(ctx context.Context, room string, h func(Message)) (cancel func(), err error)

	// Ping verifies connectivity for /readyz.
	Ping(ctx context.Context) error

	// Backend reports "memory" or "redis".
	Backend() string

	// Close stops all subscriptions. It is idempotent.
	Close() error
}

// Outbound is one frame queued for cross-instance publication.
type Outbound struct {
	Seq   int64
	Frame []byte
}

// Message is one broker delivery.
type Message struct {
	Room string
	Seq  int64
	// Origin is the instance ID that published the frame. A subscriber compares
	// it with its own ID to avoid double delivery.
	Origin  string
	Payload []byte
	// TS is the publisher's clock, diagnostics only.
	TS time.Time
}

// Noop is a Broker for single-instance mode. NextSeq returns 0 so the caller
// keeps its local counter, Publish succeeds without doing anything, and
// Subscribe never fires.
type Noop struct{}

// NextSeq implements Broker.
func (Noop) NextSeq(context.Context, string) (int64, error) { return 0, nil }

// NextSeqRange implements Broker.
func (Noop) NextSeqRange(context.Context, string, int) (int64, error) { return 0, nil }

// Publish implements Broker.
func (Noop) Publish(context.Context, string, int64, []byte) error { return nil }

// PublishBatch implements Broker.
func (Noop) PublishBatch(context.Context, string, []Outbound) error { return nil }

// Subscribe implements Broker.
func (Noop) Subscribe(context.Context, string, func(Message)) (func(), error) {
	return func() {}, nil
}

// Ping implements Broker.
func (Noop) Ping(context.Context) error { return nil }

// Backend implements Broker.
func (Noop) Backend() string { return "memory" }

// Close implements Broker.
func (Noop) Close() error { return nil }

var _ Broker = Noop{}
