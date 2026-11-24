package metrics

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRenderEmptyRegistry(t *testing.T) {
	r := New("test")
	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := b.String()
	for _, fam := range []string{
		"codraw_ws_connections",
		"codraw_rooms",
		"codraw_msgs_total",
		"codraw_ops_rejected_total",
		"codraw_broadcast_seconds",
		"codraw_dropped_connections_total",
		"codraw_cursor_dropped_total",
		"codraw_seq_gap_total",
		"codraw_uptime_seconds",
		"go_goroutines",
	} {
		if !strings.Contains(out, fam) {
			t.Errorf("output is missing family %q", fam)
		}
	}
}

// TestMessageTypeVocabulary pins the metric label set to the protocol's type
// vocabulary. A missing type means that message is invisible in dashboards; a
// stale one (op, snapshot, sync.*) means the label set was not updated with the
// contract, which is exactly what CONTRACT-ALIGNMENT.md §2.2 item 8 called out.
func TestMessageTypeVocabulary(t *testing.T) {
	want := []string{
		"hello", "op.add", "presence.cursor", "resync.request", "ping", "bye",
		"joined", "ack", "resync.snapshot", "resync.events",
		"presence.join", "presence.leave", "room.meta", "error", "pong",
	}
	have := map[string]bool{}
	for _, typ := range messageTypes {
		have[typ] = true
	}
	for _, typ := range want {
		if !have[typ] {
			t.Errorf("metric vocabulary is missing protocol type %q", typ)
		}
	}
	for _, stale := range []string{"op", "snapshot", "sync.ops", "sync.reset", "presence", "welcome"} {
		if have[stale] {
			t.Errorf("metric vocabulary still contains the obsolete type %q", stale)
		}
	}
	if !have["unknown"] {
		t.Error("metric vocabulary needs an unknown bucket so clients cannot create series")
	}
}

func TestGaugesAndCounters(t *testing.T) {
	r := New("test")
	r.SetWSConnections(42)
	r.SetRooms(3)
	r.ObserveMessage(DirIn, "op.add")
	r.ObserveMessage(DirIn, "op.add")
	r.ObserveMessage(DirOut, "op.add")
	r.ObserveMessage(DirIn, "totally-unknown-type")
	r.IncOpsRejected("forbidden")
	r.IncOpsRejected("not-a-real-reason")
	r.IncSlowConsumerKick()
	r.IncCursorDropped()
	r.IncSeqGap()

	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		"codraw_ws_connections 42",
		"codraw_rooms 3",
		`codraw_msgs_total{dir="in",type="op.add"} 2`,
		`codraw_msgs_total{dir="out",type="op.add"} 1`,
		`codraw_msgs_total{dir="in",type="unknown"} 1`,
		`codraw_ops_rejected_total{reason="forbidden"} 1`,
		`codraw_ops_rejected_total{reason="other"} 1`,
		"codraw_dropped_connections_total 1",
		"codraw_cursor_dropped_total 1",
		"codraw_seq_gap_total 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
}

func TestHistogramBuckets(t *testing.T) {
	r := New("test")
	r.ObserveBroadcast(500 * time.Microsecond)
	r.ObserveBroadcast(30 * time.Millisecond)
	r.ObserveBroadcast(3 * time.Second)

	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		`codraw_broadcast_seconds_bucket{le="0.001"} 1`,
		`codraw_broadcast_seconds_bucket{le="0.05"} 2`,
		`codraw_broadcast_seconds_bucket{le="+Inf"} 3`,
		"codraw_broadcast_seconds_count 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

func TestQuantileBucketSelection(t *testing.T) {
	cases := []struct {
		name      string
		latencies []float64
		q         float64
		wantAt    int
	}{
		{"empty", nil, 0.95, -1},
		{"all fast", []float64{0.0005, 0.0008, 0.0009, 0.001}, 0.95, 0},
		{"one slow outlier is inside p95", []float64{0.0005, 0.0006, 0.0007, 0.09}, 0.95, 7},
		{"just over the product target", []float64{0.2}, 0.95, 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := New("test")
			for _, s := range c.latencies {
				r.ObserveBroadcast(time.Duration(s * float64(time.Second)))
			}
			got := r.quantile(c.q)
			if c.wantAt < 0 {
				if got != 0 {
					t.Fatalf("quantile = %s, want 0", got)
				}
				return
			}
			want := time.Duration(broadcastBuckets[c.wantAt] * float64(time.Second))
			if got != want {
				t.Fatalf("quantile = %s, want %s", got, want)
			}
		})
	}
}

func TestNegativeBroadcastIsClamped(t *testing.T) {
	r := New("test")
	r.ObserveBroadcast(-time.Second)
	var b strings.Builder
	_, _ = r.WriteTo(&b)
	if !strings.Contains(b.String(), `codraw_broadcast_seconds_bucket{le="0.001"} 1`) {
		t.Fatalf("a negative duration must land in the first bucket:\n%s", b.String())
	}
}

func TestLabelEscaping(t *testing.T) {
	r := New(`ver"sion\with`)
	var b strings.Builder
	_, _ = r.WriteTo(&b)
	out := b.String()
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "codraw_build_info") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("codraw_build_info missing:\n%s", out)
	}
	if !strings.Contains(line, `version="ver\"sion\\with"`) {
		t.Fatalf("label not escaped:\n%s", line)
	}
	unescaped := 0
	for i := 0; i < len(line); i++ {
		if line[i] == '\\' {
			i++
			continue
		}
		if line[i] == '"' {
			unescaped++
		}
	}
	if unescaped != 4 {
		t.Fatalf("%d unescaped quotes, want 4:\n%s", unescaped, line)
	}
}

func TestSampleRates(t *testing.T) {
	r := New("test")
	t0 := time.Now()
	r.SampleRates(t0) // seed
	for i := 0; i < 100; i++ {
		r.ObserveMessage(DirIn, "op.add")
	}
	r.SampleRates(t0.Add(time.Second))
	rates := r.CurrentRates()
	if rates.InPerSec < 90 || rates.InPerSec > 110 {
		t.Fatalf("in rate = %.1f/s, want ~100/s", rates.InPerSec)
	}

	// The first call cannot compute a rate; it must not report Inf.
	r2 := New("test")
	r2.SampleRates(time.Now())
	if got := r2.CurrentRates(); got.InPerSec != 0 {
		t.Fatalf("first sample rate = %v, want 0", got.InPerSec)
	}
}

func TestConcurrentRecording(t *testing.T) {
	r := New("test")
	const goroutines, per = 16, 500
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				r.ObserveMessage(DirIn, "op.add")
				r.ObserveBroadcast(time.Millisecond)
				r.IncOpsRejected("invalid_op")
				r.SetWSConnections(j)
			}
		}()
	}
	wg.Wait()

	snap := r.Snapshot()
	if got := snap.Messages[DirIn+"\x00op.add"]; got != goroutines*per {
		t.Fatalf("in op.add count = %d, want %d", got, goroutines*per)
	}
	if got := snap.Rejected["invalid_op"]; got != goroutines*per {
		t.Fatalf("rejected count = %d, want %d", got, goroutines*per)
	}
	if got := snap.MessagesIn()["op.add"]; got != goroutines*per {
		t.Fatalf("MessagesIn = %d", got)
	}
}
