package integration

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codraw/codraw/backend/internal/config"
	"github.com/codraw/codraw/backend/internal/protocol"
)

// TestPacedWriterFloodWithoutSlowClient is the control for
// TestSlowConsumerDoesNotAffectOthers: a single paced writer must be able to send
// hundreds of ops and receive every ack, with no other clients at all. If this
// fails, the problem is the pipeline, not the slow consumer.
func TestPacedWriterFloodWithoutSlowClient(t *testing.T) {
	s := newStack(t, func(c *config.Config) {
		// The flood is a pipeline test, not a rate-limit test.
		c.Limits.OpsPerSecond = 5000
		c.Limits.OpsBurst = 5000
	})
	room := s.createRoom("纯写")

	writer := s.dial(room.Code, "c_write001", "写", room.EditorKey, "")
	writer.join(room.Code, "c_write001", 0, 0)

	const totalOps = 300
	applied := make(chan struct{}, totalOps)
	var acks atomic.Int64

	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			env, err := writer.read(10 * time.Second)
			if err != nil {
				t.Logf("reader exited after %d acks: %v", acks.Load(), err)
				return
			}
			if env.Type == protocol.TypeAck {
				var p protocol.AckPayload
				if err := json.Unmarshal(env.Payload, &p); err == nil && p.Status == protocol.AckApplied {
					acks.Add(1)
					select {
					case applied <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	for i := 0; i < totalOps; i++ {
		id := shapeID(fmt.Sprintf("pure%d", i))
		oid := opID(fmt.Sprintf("pure%d", i))
		writer.send(protocol.TypeOpAdd, room.Code, "c_write001", protocol.OpAddPayload{
			OpID: oid, Action: protocol.ActionCreate, Shape: rectShape(id),
		})
		select {
		case <-applied:
		case <-time.After(15 * time.Second):
			rh, _ := s.hub.Get(room.Code)
			t.Fatalf("stalled at op %d: room seq=%d online=%d acks=%d",
				i+1, rh.Stats().Seq, rh.Stats().Online, acks.Load())
		}
	}
	if got := acks.Load(); got != totalOps {
		t.Fatalf("acks = %d, want %d", got, totalOps)
	}
	_ = writer.conn.Close()
	readers.Wait()
}
