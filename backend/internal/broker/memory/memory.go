// Package memory is the single-instance broker: no network, no allocation on the
// hot path, and NextSeq defers to the room's own counter.
//
// It exists as a distinct type (rather than only broker.Noop) because tests need
// a broker that can actually deliver messages between two rooms on the same
// instance, which is how the multi-instance fan-out path is exercised without
// Redis.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/codraw/codraw/backend/internal/broker"
)

// Broker is an in-process pub/sub broker.
type Broker struct {
	mu     sync.RWMutex
	subs   map[string]map[*subscription]struct{}
	closed bool
	now    func() time.Time
}

type subscription struct {
	h      func(broker.Message)
	active bool
	mu     sync.Mutex
	once   sync.Once
}

// New returns an in-process broker.
func New() *Broker {
	return &Broker{
		subs: make(map[string]map[*subscription]struct{}),
		now:  time.Now,
	}
}

var _ broker.Broker = (*Broker)(nil)

// NextSeq implements broker.Broker. Returning 0 means "single writer; the room
// keeps its own counter", which is the whole point of memory mode.
func (b *Broker) NextSeq(context.Context, string) (int64, error) { return 0, nil }

// NextSeqRange implements broker.Broker with the same single-writer semantics.
func (b *Broker) NextSeqRange(context.Context, string, int) (int64, error) { return 0, nil }

// Publish implements broker.Broker. Delivery is synchronous, which is correct
// here: the publisher is a single room goroutine and subscribers only enqueue.
func (b *Broker) Publish(ctx context.Context, room string, seq int64, frame []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return broker.ErrClosed
	}
	targets := make([]*subscription, 0, len(b.subs[room]))
	for sub := range b.subs[room] {
		targets = append(targets, sub)
	}
	b.mu.RUnlock()

	msg := broker.Message{Room: room, Seq: seq, Payload: frame, TS: b.now()}
	for _, sub := range targets {
		sub.mu.Lock()
		active := sub.active
		sub.mu.Unlock()
		if active {
			sub.h(msg)
		}
	}
	return nil
}

// PublishBatch implements broker.Broker by publishing each frame in order. The
// in-process broker has no round trip to amortise, but keeping the batch API means
// callers have one code path for both backends.
func (b *Broker) PublishBatch(ctx context.Context, room string, msgs []broker.Outbound) error {
	for _, m := range msgs {
		if err := b.Publish(ctx, room, m.Seq, m.Frame); err != nil {
			return err
		}
	}
	return nil
}

// Subscribe implements broker.Broker.
func (b *Broker) Subscribe(ctx context.Context, room string, h func(broker.Message)) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h == nil {
		return nil, broker.ErrUnavailable
	}
	sub := &subscription{h: h, active: true}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, broker.ErrClosed
	}
	set, ok := b.subs[room]
	if !ok {
		set = make(map[*subscription]struct{}, 1)
		b.subs[room] = set
	}
	set[sub] = struct{}{}
	b.mu.Unlock()

	return func() {
		sub.once.Do(func() {
			b.mu.Lock()
			if set, ok := b.subs[room]; ok {
				delete(set, sub)
				if len(set) == 0 {
					delete(b.subs, room)
				}
			}
			b.mu.Unlock()
			sub.mu.Lock()
			sub.active = false
			sub.mu.Unlock()
		})
	}, nil
}

// Ping implements broker.Broker.
func (b *Broker) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return broker.ErrClosed
	}
	return nil
}

// Backend implements broker.Broker.
func (b *Broker) Backend() string { return "memory" }

// Close implements broker.Broker.
func (b *Broker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	subs := b.subs
	b.subs = nil
	b.mu.Unlock()

	for _, set := range subs {
		for sub := range set {
			sub.mu.Lock()
			sub.active = false
			sub.mu.Unlock()
		}
	}
	return nil
}
