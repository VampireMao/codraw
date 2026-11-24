// Package metrics implements the small, dependency-free Prometheus exporter used
// by GET /metrics and the /ops panel.
//
// The project needs a handful of metric families, a stable text format and no
// runtime registration magic, so a hand-written exporter beats
// prometheus/client_golang: fewer dependencies in the final image and full
// control over label cardinality.
//
// Label values are constrained to fixed enumerations for exactly that reason — an
// unbounded label (a message type taken from a client, a room code, a nickname)
// is a denial-of-service vector against the scraper.
package metrics

import (
	"fmt"
	"io"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Direction label values for codraw_msgs_total.
const (
	DirIn  = "in"
	DirOut = "out"
)

// messageTypes is the closed set of envelope `type` values from
// docs/PROTOCOL.md §3/§4, plus a bucket for anything unrecognised.
var messageTypes = []string{
	// client → server
	"hello", "op.add", "presence.cursor", "resync.request", "ping", "bye",
	"room.grant", "room.transfer",
	// server → client
	"joined", "ack", "resync.snapshot", "resync.events",
	"presence.join", "presence.leave", "room.meta", "member.role", "error", "pong",
	"unknown",
}

var messageTypeIndex = func() map[string]int {
	m := make(map[string]int, len(messageTypes))
	for i, t := range messageTypes {
		m[t] = i
	}
	return m
}()

// rejectionReasons is the closed set of protocol error codes
// (docs/PROTOCOL.md §8).
var rejectionReasons = []string{
	"bad_request", "unsupported_version", "unsupported_type", "unauthenticated",
	"forbidden", "invalid_op", "rate_limited", "message_too_large",
	"shape_not_found", "shape_deleted", "shape_too_large", "duplicate_shape",
	"room_full_shapes", "room_not_found", "room_full", "server_busy",
	"snapshot_too_large", "internal", "not_leader",
	"target_offline", "invalid_grant", "other",
}

var rejectionReasonIndex = func() map[string]int {
	m := make(map[string]int, len(rejectionReasons))
	for i, r := range rejectionReasons {
		m[r] = i
	}
	return m
}()

// broadcastBuckets are the codraw_broadcast_seconds histogram bounds. They
// straddle the p95 < 80ms product target so the target is readable from one
// scrape.
var broadcastBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.08, 0.1, 0.25, 0.5, 1, 2.5, 5,
}

// Registry is the process-wide metric store. It is safe for concurrent use.
//
// Ownership: one *Registry is created in main and passed to every component that
// records something. Nothing reads metrics except /metrics, /ops and tests; no
// metric drives control flow.
type Registry struct {
	started time.Time
	build   string

	wsConns atomic.Int64
	rooms   atomic.Int64

	// msgTotal is indexed [direction][type]: direction 0 = in, 1 = out.
	msgTotal []atomic.Int64
	rejected []atomic.Int64
	cursorD  atomic.Int64
	kicks    atomic.Int64
	seqGaps  atomic.Int64

	// Histogram state: non-cumulative bucket counts, accumulated when rendered.
	bcBucket []atomic.Int64
	bcSumNs  atomic.Int64
	bcCount  atomic.Int64

	httpReqs atomic.Int64
	httpDur  atomic.Int64

	// Per-second message rates, sampled by a background goroutine in main.
	// Stored as scaled integers (milli-messages) so the read path is lock-free.
	rateIn  atomic.Int64
	rateOut atomic.Int64

	// lastSample holds the counters at the previous sample so a rate can be
	// derived without a second set of atomics per type.
	lastMsgIn    atomic.Int64
	lastMsgOut   atomic.Int64
	lastSampleAt atomic.Int64
}

// New returns an empty registry. build is reported by codraw_build_info.
func New(build string) *Registry {
	return &Registry{
		started:  time.Now(),
		build:    build,
		msgTotal: make([]atomic.Int64, len(messageTypes)*2),
		rejected: make([]atomic.Int64, len(rejectionReasons)),
		bcBucket: make([]atomic.Int64, len(broadcastBuckets)+1),
	}
}

// SetWSConnections records the number of upgraded WebSocket connections (gauge).
func (r *Registry) SetWSConnections(n int) { r.wsConns.Store(int64(n)) }

// SetRooms records the number of live rooms (gauge).
func (r *Registry) SetRooms(n int) { r.rooms.Store(int64(n)) }

// ObserveMessage counts one WebSocket frame. Unknown types collapse into the
// "unknown" label so a hostile client cannot create new series.
func (r *Registry) ObserveMessage(dir, msgType string) {
	slot, ok := messageTypeIndex[msgType]
	if !ok {
		slot = messageTypeIndex["unknown"]
	}
	if dir == DirOut {
		r.msgTotal[len(messageTypes)+slot].Add(1)
		return
	}
	r.msgTotal[slot].Add(1)
}

// IncOpsRejected counts a rejected write. Reasons outside the protocol table
// collapse into "other".
func (r *Registry) IncOpsRejected(reason string) {
	i, ok := rejectionReasonIndex[reason]
	if !ok {
		i = len(rejectionReasons) - 1
	}
	r.rejected[i].Add(1)
}

// IncCursorDropped counts cursor frames discarded by throttling.
func (r *Registry) IncCursorDropped() { r.cursorD.Add(1) }

// IncSlowConsumerKick counts connections evicted for backpressure.
func (r *Registry) IncSlowConsumerKick() { r.kicks.Add(1) }

// IncSeqGap counts detected sequence gaps (multi-instance health signal).
func (r *Registry) IncSeqGap() { r.seqGaps.Add(1) }

// ObserveBroadcast records the wall time from accepting a write to having
// enqueued it on every subscriber.
func (r *Registry) ObserveBroadcast(d time.Duration) {
	if d < 0 {
		d = 0
	}
	secs := d.Seconds()
	idx := len(broadcastBuckets)
	for i, ub := range broadcastBuckets {
		if secs <= ub {
			idx = i
			break
		}
	}
	r.bcBucket[idx].Add(1)
	r.bcSumNs.Add(d.Nanoseconds())
	r.bcCount.Add(1)
}

// ObserveHTTP records one HTTP request and its duration.
func (r *Registry) ObserveHTTP(d time.Duration) {
	r.httpReqs.Add(1)
	r.httpDur.Add(d.Nanoseconds())
}

// SampleRates recomputes the per-second message rates. It is called once per
// second from a goroutine in main; the very first call only seeds the baseline.
func (r *Registry) SampleRates(now time.Time) {
	var in, out int64
	for i := range messageTypes {
		in += r.msgTotal[i].Load()
		out += r.msgTotal[len(messageTypes)+i].Load()
	}
	prevIn := r.lastMsgIn.Swap(in)
	prevOut := r.lastMsgOut.Swap(out)
	prevAtNs := r.lastSampleAt.Swap(now.UnixNano())
	if prevAtNs == 0 {
		return
	}
	elapsed := float64(now.UnixNano()-prevAtNs) / float64(time.Second)
	if elapsed <= 0 {
		return
	}
	r.rateIn.Store(int64(float64(in-prevIn) / elapsed * 1000))
	r.rateOut.Store(int64(float64(out-prevOut) / elapsed * 1000))
}

// Rates is the sampled message rate in messages per second.
type Rates struct {
	InPerSec  float64
	OutPerSec float64
}

// CurrentRates returns the last sampled rates.
func (r *Registry) CurrentRates() Rates {
	return Rates{
		InPerSec:  float64(r.rateIn.Load()) / 1000,
		OutPerSec: float64(r.rateOut.Load()) / 1000,
	}
}

// Snapshot is a point-in-time copy for /ops and tests.
type Snapshot struct {
	WSConnections     int64
	Rooms             int64
	Messages          map[string]int64 // key: dir + "\x00" + type
	Rejected          map[string]int64
	BroadcastP95      time.Duration
	Uptime            time.Duration
	SlowConsumerKicks int64
	SeqGaps           int64
	CursorDropped     int64
	Rates             Rates
}

// MessagesIn returns the inbound counts by type.
func (s Snapshot) MessagesIn() map[string]int64 { return s.filter(DirIn) }

// MessagesOut returns the outbound counts by type.
func (s Snapshot) MessagesOut() map[string]int64 { return s.filter(DirOut) }

func (s Snapshot) filter(dir string) map[string]int64 {
	out := make(map[string]int64, len(messageTypes))
	prefix := dir + "\x00"
	for k, v := range s.Messages {
		if strings.HasPrefix(k, prefix) {
			out[strings.TrimPrefix(k, prefix)] = v
		}
	}
	return out
}

// Snapshot copies the registry. It allocates and is therefore only for the
// low-frequency /ops panel and tests.
func (r *Registry) Snapshot() Snapshot {
	s := Snapshot{
		WSConnections:     r.wsConns.Load(),
		Rooms:             r.rooms.Load(),
		Messages:          make(map[string]int64, len(messageTypes)*2),
		Rejected:          make(map[string]int64, len(rejectionReasons)),
		Uptime:            time.Since(r.started),
		SlowConsumerKicks: r.kicks.Load(),
		SeqGaps:           r.seqGaps.Load(),
		CursorDropped:     r.cursorD.Load(),
		Rates:             r.CurrentRates(),
	}
	for i, t := range messageTypes {
		s.Messages[DirIn+"\x00"+t] = r.msgTotal[i].Load()
		s.Messages[DirOut+"\x00"+t] = r.msgTotal[len(messageTypes)+i].Load()
	}
	for i, reason := range rejectionReasons {
		s.Rejected[reason] = r.rejected[i].Load()
	}
	s.BroadcastP95 = r.quantile(0.95)
	return s
}

// quantile approximates a quantile from the histogram buckets. Only the
// human-facing /ops panel uses it; Prometheus computes exact quantiles itself.
//
// The target rank is ceiled, not truncated: with 4 observations the 95th
// percentile is the 4th value, and truncating would report the 3rd — for a single
// slow outlier that understates p95 by two orders of magnitude.
func (r *Registry) quantile(q float64) time.Duration {
	total := r.bcCount.Load()
	if total == 0 {
		return 0
	}
	target := int64(math.Ceil(float64(total) * q))
	if target < 1 {
		target = 1
	}
	var cumulative int64
	for i := 0; i < len(broadcastBuckets); i++ {
		cumulative += r.bcBucket[i].Load()
		if cumulative >= target {
			return time.Duration(broadcastBuckets[i] * float64(time.Second))
		}
	}
	// Everything is above the largest finite bucket; report that bound rather than
	// 0, which would read as "no latency".
	return time.Duration(broadcastBuckets[len(broadcastBuckets)-1] * float64(time.Second))
}

// WriteTo renders every metric family in Prometheus text exposition format 0.0.4.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	cw := &countingWriter{w: w}

	fmt.Fprintf(cw, "# HELP codraw_ws_connections Currently upgraded WebSocket connections on this instance.\n")
	fmt.Fprintf(cw, "# TYPE codraw_ws_connections gauge\n")
	fmt.Fprintf(cw, "codraw_ws_connections %d\n", r.wsConns.Load())

	fmt.Fprintf(cw, "# HELP codraw_rooms Rooms currently held in memory by this instance.\n")
	fmt.Fprintf(cw, "# TYPE codraw_rooms gauge\n")
	fmt.Fprintf(cw, "codraw_rooms %d\n", r.rooms.Load())

	fmt.Fprintf(cw, "# HELP codraw_uptime_seconds Seconds since process start.\n")
	fmt.Fprintf(cw, "# TYPE codraw_uptime_seconds gauge\n")
	fmt.Fprintf(cw, "codraw_uptime_seconds %s\n", formatFloat(time.Since(r.started).Seconds()))

	fmt.Fprintf(cw, "# HELP codraw_build_info Build metadata, always 1.\n")
	fmt.Fprintf(cw, "# TYPE codraw_build_info gauge\n")
	fmt.Fprintf(cw, "codraw_build_info{version=\"%s\",go_version=\"%s\"} 1\n",
		escapeLabel(r.build), escapeLabel(runtime.Version()))

	fmt.Fprintf(cw, "# HELP codraw_msgs_total WebSocket frames handled, by direction and message type.\n")
	fmt.Fprintf(cw, "# TYPE codraw_msgs_total counter\n")
	for i, t := range messageTypes {
		fmt.Fprintf(cw, "codraw_msgs_total{dir=%q,type=%q} %d\n", DirIn, escapeLabel(t), r.msgTotal[i].Load())
	}
	for i, t := range messageTypes {
		fmt.Fprintf(cw, "codraw_msgs_total{dir=%q,type=%q} %d\n", DirOut, escapeLabel(t), r.msgTotal[len(messageTypes)+i].Load())
	}

	fmt.Fprintf(cw, "# HELP codraw_ops_rejected_total Write operations rejected, by protocol error code.\n")
	fmt.Fprintf(cw, "# TYPE codraw_ops_rejected_total counter\n")
	for i, reason := range rejectionReasons {
		fmt.Fprintf(cw, "codraw_ops_rejected_total{reason=%q} %d\n", escapeLabel(reason), r.rejected[i].Load())
	}

	fmt.Fprintf(cw, "# HELP codraw_dropped_connections_total Connections closed by the server for backpressure.\n")
	fmt.Fprintf(cw, "# TYPE codraw_dropped_connections_total counter\n")
	fmt.Fprintf(cw, "codraw_dropped_connections_total %d\n", r.kicks.Load())

	fmt.Fprintf(cw, "# HELP codraw_cursor_dropped_total Cursor frames dropped by the per-connection throttle.\n")
	fmt.Fprintf(cw, "# TYPE codraw_cursor_dropped_total counter\n")
	fmt.Fprintf(cw, "codraw_cursor_dropped_total %d\n", r.cursorD.Load())

	fmt.Fprintf(cw, "# HELP codraw_seq_gap_total Remote sequence gaps detected (multi-instance health).\n")
	fmt.Fprintf(cw, "# TYPE codraw_seq_gap_total counter\n")
	fmt.Fprintf(cw, "codraw_seq_gap_total %d\n", r.seqGaps.Load())

	fmt.Fprintf(cw, "# HELP codraw_broadcast_seconds Time from accepting a write to enqueueing it for every subscriber.\n")
	fmt.Fprintf(cw, "# TYPE codraw_broadcast_seconds histogram\n")
	var cumulative int64
	for i, ub := range broadcastBuckets {
		cumulative += r.bcBucket[i].Load()
		fmt.Fprintf(cw, "codraw_broadcast_seconds_bucket{le=\"%s\"} %d\n", formatFloat(ub), cumulative)
	}
	cumulative += r.bcBucket[len(broadcastBuckets)].Load()
	fmt.Fprintf(cw, "codraw_broadcast_seconds_bucket{le=\"+Inf\"} %d\n", cumulative)
	fmt.Fprintf(cw, "codraw_broadcast_seconds_sum %s\n", formatFloat(float64(r.bcSumNs.Load())/1e9))
	fmt.Fprintf(cw, "codraw_broadcast_seconds_count %d\n", r.bcCount.Load())

	fmt.Fprintf(cw, "# HELP codraw_http_requests_total HTTP requests served.\n")
	fmt.Fprintf(cw, "# TYPE codraw_http_requests_total counter\n")
	fmt.Fprintf(cw, "codraw_http_requests_total %d\n", r.httpReqs.Load())

	fmt.Fprintf(cw, "# HELP codraw_http_request_seconds_sum Total wall time spent serving HTTP requests.\n")
	fmt.Fprintf(cw, "# TYPE codraw_http_request_seconds_sum counter\n")
	fmt.Fprintf(cw, "codraw_http_request_seconds_sum %s\n", formatFloat(float64(r.httpDur.Load())/1e9))

	fmt.Fprintf(cw, "# HELP go_goroutines Number of goroutines that currently exist.\n")
	fmt.Fprintf(cw, "# TYPE go_goroutines gauge\n")
	fmt.Fprintf(cw, "go_goroutines %d\n", runtime.NumGoroutine())

	return cw.n, cw.err
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeLabel escapes a Prometheus label value per the exposition format.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

type countingWriter struct {
	w   io.Writer
	n   int64
	err error
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.err = err
	return n, err
}
