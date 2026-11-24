package room_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/codraw/codraw/backend/internal/room"
	"github.com/codraw/codraw/backend/internal/store/postgres"
)

// TestBatchBreakdown times AppendBatch at several sizes to confirm the single-statement
// rewrite made small batches cheap (one round trip + one fsync per batch).
func TestBatchBreakdown(t *testing.T) {
	if os.Getenv("ROOMBENCH") != "1" {
		t.Skip("set ROOMBENCH=1")
	}
	ctx := context.Background()
	st, err := postgres.New(ctx, postgres.Options{DatabaseURL: os.Getenv("PGURL"), ConnectTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer func() { _ = st.Close() }()

	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	// Exactly six characters from the legal alphabet.
	code := roomCodeSafe(fmt.Sprintf("N%03d%02d", int(rnd[0])%1000, int(rnd[1])%100))
	if err := st.CreateRoom(ctx, &room.Record{Code: code, Title: "bd"}); err != nil {
		t.Fatalf("create %q: %v", code, err)
	}
	payload := []byte(`{"opId":"x","action":"create","shape":{}}`)

	var base int64
	for _, n := range []int{1, 4, 16, 64, 128, 256} {
		recs := make([]room.OpRecord, n)
		start := time.Now()
		const iters = 40
		for k := 0; k < iters; k++ {
			for i := range recs {
				recs[i] = room.OpRecord{
					Code: code, Seq: base + int64(i) + 1, Epoch: 1,
					OpID:     fmt.Sprintf("op_%d_%d_%d", n, k, i),
					ClientID: "c_bd", AuthorID: "u", Action: "create", ShapeID: "sh_x", Payload: payload,
				}
			}
			res, err := st.AppendBatch(ctx, recs)
			if err != nil {
				t.Fatalf("append n=%d: %v", n, err)
			}
			base = res[len(res)-1].Seq
		}
		per := time.Since(start) / iters
		t.Logf("AppendBatch n=%-3d  %6.2fms/batch  %6.1fµs/op", n,
			float64(per.Microseconds())/1000, float64(per.Microseconds())/float64(n))
	}
}

func roomCodeSafe(s string) string {
	repl := map[rune]rune{'0': '2', '1': '3', 'O': '4', 'I': '5', 'L': '6'}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if v, ok := repl[r]; ok {
			out = append(out, v)
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
