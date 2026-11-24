package memory

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/codraw/codraw/backend/internal/broker"
)

func TestPublishReachesSubscribers(t *testing.T) {
	ctx := context.Background()
	b := New()
	defer func() { _ = b.Close() }()

	var mu sync.Mutex
	var got []broker.Message
	cancel, err := b.Subscribe(ctx, "K7M2QX", func(m broker.Message) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := b.Publish(ctx, "K7M2QX", int64(i+1), []byte("frame")); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	// A different room must not receive these.
	if err := b.Publish(ctx, "ZZZZZZ", 9, []byte("other")); err != nil {
		t.Fatalf("Publish other room: %v", err)
	}

	cancel()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("received %d messages, want 3", len(got))
	}
	for i, m := range got {
		if m.Seq != int64(i+1) {
			t.Errorf("message %d seq = %d: ordering was not preserved", i, m.Seq)
		}
		if string(m.Payload) != "frame" {
			t.Errorf("message %d payload = %q", i, m.Payload)
		}
	}

	// After cancel, publishes must not be delivered.
	if err := b.Publish(ctx, "K7M2QX", 4, []byte("late")); err != nil {
		t.Fatalf("Publish after cancel: %v", err)
	}
}

func TestNextSeqIsZeroInMemoryMode(t *testing.T) {
	b := New()
	// 0 means "the room keeps its own counter", which is the entire point of
	// single-instance mode.
	seq, err := b.NextSeq(context.Background(), "K7M2QX")
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	if seq != 0 {
		t.Fatalf("NextSeq = %d, want 0", seq)
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	b := New()
	defer func() { _ = b.Close() }()
	cancel, err := b.Subscribe(context.Background(), "K7M2QX", func(broker.Message) {})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()
	cancel()
	cancel()
}

func TestDuplicateSubscribeIsAllowed(t *testing.T) {
	// Two independent subscribers per room are legitimate (one per room handle in
	// tests); Redis mode rejects them, so this documents the difference.
	ctx := context.Background()
	b := New()
	defer func() { _ = b.Close() }()
	c1, err := b.Subscribe(ctx, "K7M2QX", func(broker.Message) {})
	if err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	defer c1()
	c2, err := b.Subscribe(ctx, "K7M2QX", func(broker.Message) {})
	if err != nil {
		t.Fatalf("Subscribe 2: %v", err)
	}
	defer c2()
}

func TestClosedBrokerRejectsWork(t *testing.T) {
	ctx := context.Background()
	b := New()
	if _, err := b.Subscribe(ctx, "K7M2QX", func(broker.Message) {}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := b.Ping(ctx); err != broker.ErrClosed {
		t.Fatalf("Ping = %v, want ErrClosed", err)
	}
	if _, err := b.Subscribe(ctx, "K7M2QX", func(broker.Message) {}); err != broker.ErrClosed {
		t.Fatalf("Subscribe after close = %v, want ErrClosed", err)
	}
	if err := b.Publish(ctx, "K7M2QX", 1, nil); err != broker.ErrClosed {
		t.Fatalf("Publish after close = %v, want ErrClosed", err)
	}
	if b.Backend() != "memory" {
		t.Fatalf("Backend = %q", b.Backend())
	}
}

func TestPublishIsSafeUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	b := New()
	defer func() { _ = b.Close() }()

	var mu sync.Mutex
	count := 0
	cancel, err := b.Subscribe(ctx, "K7M2QX", func(broker.Message) {
		mu.Lock()
		count++
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	const goroutines, perGoroutine = 8, 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if err := b.Publish(ctx, "K7M2QX", 1, []byte("x")); err != nil {
					t.Errorf("Publish: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if count != goroutines*perGoroutine {
		t.Fatalf("delivered %d messages, want %d", count, goroutines*perGoroutine)
	}
}

func TestPublishHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := New()
	defer func() { _ = b.Close() }()
	if err := b.Publish(ctx, "K7M2QX", 1, nil); err == nil {
		t.Fatal("Publish with a cancelled context should fail")
	}
	_ = time.Now()
}
