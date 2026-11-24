// Package redis implements broker.Broker on Redis 7.
//
// Two Redis features are used, each with a specific justification:
//
//   - INCR on codraw:seq:{room} for the room sequence. It is O(1), atomic across
//     instances, and monotonic — which is all seq requires (uniqueness +
//     monotonicity, not "allocate and persist atomically"). A crash between INCR
//     and the write leaves a hole; clients detect it and resync from the store,
//     which is the accepted trade-off documented in ARCHITECTURE.md §4.1.
//
//   - PUBLISH/SUBSCRIBE on codraw:room:{room}:events for fan-out. Pub/sub is
//     fire-and-forget: a disconnected instance misses frames and recovers through
//     the same seq-gap path, so durability is deliberately the store's job.
//
// Failure policy: a Redis outage must degrade the service, never crash it. Every
// method returns an error and the caller (the room) logs and continues serving
// locally; only cross-instance coherence is lost.
package redis

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/codraw/codraw/backend/internal/broker"
)

const (
	seqKeyPrefix  = "codraw:seq:"
	channelPrefix = "codraw:room:"
	channelSuffix = ":events"
	// publishTimeout bounds one PUBLISH so a stalled connection cannot hold a
	// room goroutine. The room calls Publish synchronously.
	publishTimeout = 500 * time.Millisecond
)

// Broker is the Redis implementation.
type Broker struct {
	client *goredis.Client
	log    *slog.Logger
	// instanceID is embedded in published frames so a subscriber can drop its own
	// messages and avoid double fan-out.
	instanceID string
	now        func() time.Time

	mu     sync.Mutex
	subs   map[string]*subscription
	closed bool
	wg     sync.WaitGroup
}

type subscription struct {
	cancel context.CancelFunc
	pubsub *goredis.PubSub
}

// Options configures the broker.
type Options struct {
	URL        string
	InstanceID string
	Logger     *slog.Logger
	// DialTimeout bounds the initial connection check.
	DialTimeout time.Duration
	// Now overrides the clock in tests.
	Now func() time.Time
}

// New connects and verifies the server with a PING.
//
// It fails fast on an unreachable Redis: starting with BROKER=redis and silently
// running single-instance would break the multi-instance promise while looking
// healthy.
func New(ctx context.Context, opts Options) (*Broker, error) {
	if opts.URL == "" {
		return nil, errors.New("redis: URL is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	parsed, err := goredis.ParseURL(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("redis: parse URL: %w", err)
	}
	if opts.DialTimeout > 0 {
		parsed.DialTimeout = opts.DialTimeout
	}
	parsed.ReadTimeout = 3 * time.Second
	parsed.WriteTimeout = 3 * time.Second
	// A small pool: this client only issues INCR and PUBLISH, never blocks on a
	// long query.
	parsed.PoolSize = 8

	client := goredis.NewClient(parsed)
	pingCtx, cancel := context.WithTimeout(ctx, max(opts.DialTimeout, 3*time.Second))
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis: ping: %w", err)
	}
	return &Broker{
		client:     client,
		log:        opts.Logger,
		instanceID: opts.InstanceID,
		now:        opts.Now,
		subs:       make(map[string]*subscription),
	}, nil
}

var _ broker.Broker = (*Broker)(nil)

// Backend implements broker.Broker.
func (b *Broker) Backend() string { return "redis" }

// Ping implements broker.Broker.
func (b *Broker) Ping(ctx context.Context) error {
	return b.client.Ping(ctx).Err()
}

// NextSeq implements broker.Broker using INCR.
func (b *Broker) NextSeq(ctx context.Context, room string) (int64, error) {
	seq, err := b.client.Incr(ctx, seqKeyPrefix+room).Result()
	if err != nil {
		return 0, fmt.Errorf("redis: incr seq: %w", err)
	}
	return seq, nil
}

// NextSeqRange implements room.SeqSource with one INCRBY, reserving n consecutive
// seqs and returning the upper bound. One round trip instead of n.
func (b *Broker) NextSeqRange(ctx context.Context, room string, n int) (int64, error) {
	if n <= 0 {
		return 0, errors.New("redis: NextSeqRange needs n > 0")
	}
	hi, err := b.client.IncrBy(ctx, seqKeyPrefix+room, int64(n)).Result()
	if err != nil {
		return 0, fmt.Errorf("redis: incrby seq: %w", err)
	}
	return hi, nil
}

// Publish implements broker.Broker.
//
// The wire format is a small binary header plus the opaque frame, so a
// subscriber can filter by origin and read the seq without parsing JSON:
//
//	[2 bytes origin length][origin][8 bytes seq][frame]
func (b *Broker) Publish(ctx context.Context, room string, seq int64, frame []byte) error {
	pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	msg := encodeMessage(b.instanceID, seq, frame)
	if err := b.client.Publish(pubCtx, channelPrefix+room+channelSuffix, msg).Err(); err != nil {
		return fmt.Errorf("redis: publish: %w", err)
	}
	return nil
}

// PublishBatch implements broker.Broker with a single pipelined round trip.
//
// Redis pub/sub has no multi-message publish primitive, so N PUBLISH commands still
// exist, but a pipeline sends them without waiting for N replies. That turns a
// group-commit fan-out from N round trips into one.
func (b *Broker) PublishBatch(ctx context.Context, room string, msgs []broker.Outbound) error {
	if len(msgs) == 0 {
		return nil
	}
	pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()

	pipe := b.client.Pipeline()
	for _, m := range msgs {
		pipe.Publish(pubCtx, channelPrefix+room+channelSuffix, encodeMessage(b.instanceID, m.Seq, m.Frame))
	}
	if _, err := pipe.Exec(pubCtx); err != nil {
		return fmt.Errorf("redis: publish batch: %w", err)
	}
	return nil
}

// encodeMessage builds the binary envelope shared by Publish and PublishBatch.
func encodeMessage(instanceID string, seq int64, frame []byte) []byte {
	msg := make([]byte, 0, 2+len(instanceID)+8+len(frame))
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(instanceID)))
	msg = append(msg, hdr[:]...)
	msg = append(msg, instanceID...)
	var seqBuf [8]byte
	binary.BigEndian.PutUint64(seqBuf[:], uint64(seq))
	msg = append(msg, seqBuf[:]...)
	msg = append(msg, frame...)
	return msg
}

// Subscribe implements broker.Broker.
func (b *Broker) Subscribe(ctx context.Context, room string, h func(broker.Message)) (func(), error) {
	if h == nil {
		return nil, errors.New("redis: nil handler")
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, broker.ErrClosed
	}
	if _, exists := b.subs[room]; exists {
		b.mu.Unlock()
		// One subscription per room per instance is the invariant: a second one
		// would deliver every frame twice.
		return nil, fmt.Errorf("redis: room %s already subscribed", room)
	}
	subCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	pubsub := b.client.Subscribe(subCtx, channelPrefix+room+channelSuffix)
	b.subs[room] = &subscription{cancel: cancel, pubsub: pubsub}
	b.mu.Unlock()

	// Wait for the subscription to be established so an immediate publish is not
	// lost, and so a broken connection surfaces here instead of silently later.
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	if _, err := pubsub.Receive(waitCtx); err != nil {
		cancel()
		_ = pubsub.Close()
		b.mu.Lock()
		delete(b.subs, room)
		b.mu.Unlock()
		return nil, fmt.Errorf("redis: subscribe %s: %w", room, err)
	}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		ch := pubsub.Channel()
		for msg := range ch {
			m, ok := decodeMessage(room, []byte(msg.Payload))
			if !ok {
				continue
			}
			if m.Origin == b.instanceID {
				continue
			}
			h(m)
		}
	}()

	return func() {
		cancel()
		_ = pubsub.Close()
		b.mu.Lock()
		delete(b.subs, room)
		b.mu.Unlock()
	}, nil
}

// decodeMessage splits the binary envelope.
func decodeMessage(room string, raw []byte) (broker.Message, bool) {
	if len(raw) < 2 {
		return broker.Message{}, false
	}
	n := int(binary.BigEndian.Uint16(raw[:2]))
	if len(raw) < 2+n+8 {
		return broker.Message{}, false
	}
	origin := string(raw[2 : 2+n])
	seq := int64(binary.BigEndian.Uint64(raw[2+n : 2+n+8]))
	frame := raw[2+n+8:]
	return broker.Message{Room: room, Origin: origin, Seq: seq, Payload: frame}, true
}

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

	for _, s := range subs {
		s.cancel()
		_ = s.pubsub.Close()
	}
	b.wg.Wait()
	return b.client.Close()
}

// SeqKey exposes the key name for tests and for the /ops diagnostics.
func SeqKey(room string) string { return seqKeyPrefix + room }

// Channels returns the channel name for a room.
func Channels(room string) string { return channelPrefix + room + channelSuffix }

var _ = strconv.Itoa
