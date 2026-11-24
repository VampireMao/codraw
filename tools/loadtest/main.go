// Command loadtest is CoDraw's independent load generator (PM acceptance tool,
// not product code).
//
// Why Go: the first version was written in Node. 100 clients x 5 op/s in one room
// produces roughly 50k inbound broadcasts per second, which single-threaded JS
// cannot parse fast enough - it reported a p50 of 28 seconds because the load
// generator itself was the bottleneck. One goroutine per connection parses in
// parallel and does not mask the server's real behaviour.
//
// Latency definition: the instant client A writes an op -> the instant client B
// receives that op. All clients live in this process and share one clock, so no
// NTP alignment is needed.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// envelope mirrors docs/PROTOCOL.md section 2.2. Only the fields this tool needs.
type envelope struct {
	V        int             `json:"v"`
	Type     string          `json:"type"`
	Seq      int64           `json:"seq,omitempty"`
	RoomID   string          `json:"roomId"`
	ClientID string          `json:"clientId"`
	TS       int64           `json:"ts"`
	Payload  json.RawMessage `json:"payload"`
}

type ackPayload struct {
	OpID   string `json:"opId"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
	Seq    int64  `json:"seq"`
}

type opPayload struct {
	Shape struct {
		ID string `json:"id"`
	} `json:"shape"`
}

type shape struct {
	ID    string         `json:"id"`
	Kind  string         `json:"kind"`
	Geom  geom           `json:"geom"`
	Style style          `json:"style"`
	Data  map[string]any `json:"data"`
	Text  *string        `json:"text"`
}

type geom struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type style struct {
	Stroke      string  `json:"stroke"`
	Fill        string  `json:"fill"`
	StrokeWidth float64 `json:"strokeWidth"`
	Opacity     float64 `json:"opacity"`
	Dash        string  `json:"dash"`
}

// stats aggregates counters and the latency samples.
type stats struct {
	mu        sync.Mutex
	latencies []time.Duration
	sent      atomic.Int64
	acks      atomic.Int64
	broadcast atomic.Int64
	errors    atomic.Int64
	dropped   atomic.Int64
	connected atomic.Int64
}

func (s *stats) record(d time.Duration) {
	s.mu.Lock()
	s.latencies = append(s.latencies, d)
	s.mu.Unlock()
}

func (s *stats) percentiles() (p50, p95, p99, max time.Duration, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.latencies) == 0 {
		return
	}
	cp := make([]time.Duration, len(s.latencies))
	copy(cp, s.latencies)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	at := func(q float64) time.Duration {
		idx := int(float64(len(cp))*q + 0.5)
		if idx >= len(cp) {
			idx = len(cp) - 1
		}
		return cp[idx]
	}
	return at(0.50), at(0.95), at(0.99), cp[len(cp)-1], len(cp)
}

// sentIndex records when each shape was written and by which client, so the
// receiving side can compute an end-to-end latency without any clock sync.
type sentIndex struct {
	mu sync.Mutex
	m  map[string]sentRec
}

type sentRec struct {
	at      time.Time
	byIndex int
}

func (s *sentIndex) put(id string, rec sentRec) {
	s.mu.Lock()
	s.m[id] = rec
	s.mu.Unlock()
}

func (s *sentIndex) take(id string) (sentRec, bool) {
	s.mu.Lock()
	rec, ok := s.m[id]
	if ok {
		delete(s.m, id)
	}
	s.mu.Unlock()
	return rec, ok
}

type client struct {
	idx    int
	room   string
	key    string
	cid    string
	origin string
	conn   *websocket.Conn
	ready  atomic.Bool
	opSeq  atomic.Int64
	closed atomic.Bool
}

type harness struct {
	base   string
	st     *stats
	idx    *sentIndex
	shapeI func(clientIdx int, n int64) string
}

func (h *harness) connect(c *client) error {
	u := strings.Replace(h.base, "http", "ws", 1) +
		"/ws?room=" + c.room +
		"&cid=" + c.cid +
		"&name=" + fmt.Sprintf("lt%d", c.idx) +
		"&key=" + c.key

	// Production mode validates Origin (CSWSH protection). gorilla's dialer does
	// not send an Origin header by default, which the server rejects with 403.
	var hdr http.Header
	if c.origin != "" {
		hdr = http.Header{"Origin": []string{c.origin}}
	}

	conn, _, err := websocket.DefaultDialer.Dial(u, hdr)
	if err != nil {
		return err
	}
	c.conn = conn
	conn.SetReadLimit(1 << 20)

	hello, _ := json.Marshal(envelope{
		V: 1, Type: "hello", RoomID: c.room, ClientID: c.cid, TS: time.Now().UnixMilli(),
		Payload: json.RawMessage(`{"lastSeq":0,"epoch":1}`),
	})
	if err := conn.WriteMessage(websocket.TextMessage, hello); err != nil {
		return err
	}
	h.st.connected.Add(1)

	readyCh := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				if !c.closed.Load() {
					h.st.dropped.Add(1)
				}
				return
			}
			var env envelope
			if json.Unmarshal(data, &env) != nil {
				continue
			}
			switch env.Type {
			case "resync.snapshot", "resync.events":
				c.ready.Store(true)
				once.Do(func() { close(readyCh) })
			case "ack":
				var p ackPayload
				if json.Unmarshal(env.Payload, &p) == nil {
					h.st.acks.Add(1)
					if p.Status == "rejected" {
						h.st.errors.Add(1)
					}
				}
			case "op.add":
				h.st.broadcast.Add(1)
				var p opPayload
				if json.Unmarshal(env.Payload, &p) == nil && p.Shape.ID != "" {
					if rec, ok := h.idx.take(p.Shape.ID); ok && rec.byIndex != c.idx {
						h.st.record(time.Since(rec.at))
					}
				}
			case "error":
				h.st.errors.Add(1)
			}
		}
	}()

	select {
	case <-readyCh:
		return nil
	case <-time.After(20 * time.Second):
		return fmt.Errorf("client %d sync timeout", c.idx)
	}
}

func (h *harness) sendOp(c *client) {
	if !c.ready.Load() {
		return
	}
	n := c.opSeq.Add(1)
	sid := h.shapeI(c.idx, n)
	opID := fmt.Sprintf("op_lt_%d_%d", c.idx, n)

	h.idx.put(sid, sentRec{at: time.Now(), byIndex: c.idx})

	sh := shape{
		ID:    sid,
		Kind:  "rect",
		Geom:  geom{X: float64((c.idx*37 + int(n)*3) % 4000), Y: float64((c.idx*53 + int(n)*7) % 4000), W: 20, H: 20},
		Style: style{Stroke: "#16181d", Fill: "none", StrokeWidth: 2, Opacity: 1, Dash: "solid"},
		Data:  map[string]any{},
	}
	pj, _ := json.Marshal(map[string]any{"opId": opID, "action": "create", "shape": sh})
	env, _ := json.Marshal(envelope{
		V: 1, Type: "op.add", RoomID: c.room, ClientID: c.cid, TS: time.Now().UnixMilli(), Payload: pj,
	})
	h.st.sent.Add(1)
	_ = c.conn.WriteMessage(websocket.TextMessage, env)
}

func createRoom(base, title string) (code, key string, err error) {
	body, _ := json.Marshal(map[string]string{"title": title})
	res, err := http.Post(base+"/api/rooms", "application/json", strings.NewReader(string(body)))
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 201 {
		return "", "", fmt.Errorf("create room failed status=%d", res.StatusCode)
	}
	var out struct {
		Room struct {
			Code string `json:"code"`
		} `json:"room"`
		Keys struct {
			EditorKey string `json:"editorKey"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", "", err
	}
	return out.Room.Code, out.Keys.EditorKey, nil
}

type opsData struct {
	Connections           int     `json:"connections"`
	Rooms                 int     `json:"rooms"`
	HeapMB                float64 `json:"heapMB"`
	Goroutines            int     `json:"goroutines"`
	BroadcastLatencyP95Ms float64 `json:"broadcastLatencyP95Ms"`
	SlowConsumerKicks     int     `json:"slowConsumerKicks"`
	MsgPerSecIn           float64 `json:"msgPerSecIn"`
	MsgPerSecOut          float64 `json:"msgPerSecOut"`
}

func fetchOps(base string) opsData {
	var d opsData
	res, err := http.Get(base + "/ops/data")
	if err != nil {
		return d
	}
	defer res.Body.Close()
	_ = json.NewDecoder(res.Body).Decode(&d)
	return d
}

func main() {
	base := flag.String("base", "http://localhost:8080", "server base URL")
	scenario := flag.String("scenario", "s1", "s1|s2|s3")
	clientsN := flag.Int("clients", 100, "number of clients")
	connsN := flag.Int("conns", 1000, "number of connections (s2)")
	roomsN := flag.Int("rooms", 1, "number of rooms")
	rate := flag.Int("rate", 5, "ops per client per second")
	duration := flag.Duration("duration", 60*time.Second, "duration")
	ramp := flag.Duration("ramp", 5*time.Second, "total connection ramp time")
	drawers := flag.Int("drawers", 0, "number of actual drawers (0 = all)")
	origin := flag.String("origin", "", "WS handshake Origin (required in production mode)")
	flag.Parse()

	fmt.Printf("CoDraw loadtest  scenario=%s  target=%s\n", *scenario, *base)
	before := fetchOps(*base)
	fmt.Printf("baseline: conn=%d heap=%.2fMB goroutines=%d\n",
		before.Connections, before.HeapMB, before.Goroutines)

	h := &harness{
		base: *base,
		st:   &stats{},
		idx:  &sentIndex{m: make(map[string]sentRec)},
	}
	h.shapeI = func(ci int, n int64) string { return fmt.Sprintf("sh_lt_%d_%d", ci, n) }

	var clients []*client
	var mu sync.Mutex
	var rooms []struct{ code, key string }

	newClient := func(i int, code, key, tag string) *client {
		return &client{
			idx:    i,
			room:   code,
			key:    key,
			cid:    fmt.Sprintf("c_%s_%d_%d", tag, i, time.Now().UnixNano()%1e9),
			origin: *origin,
		}
	}

	switch *scenario {
	case "s1":
		code, key, err := createRoom(*base, "loadtest-s1")
		if err != nil {
			fmt.Fprintf(os.Stderr, "create room: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("  room %s  clients %d  rate %d op/s  duration %s\n", code, *clientsN, *rate, *duration)
		rooms = append(rooms, struct{ code, key string }{code, key})

		per := *ramp / time.Duration(*clientsN)
		if per <= 0 {
			per = time.Millisecond
		}
		for i := 0; i < *clientsN; i++ {
			c := newClient(i, code, key, "lt")
			if err := h.connect(c); err != nil {
				fmt.Fprintf(os.Stderr, "  client %d: %v\n", i, err)
				h.st.errors.Add(1)
				continue
			}
			clients = append(clients, c)
			time.Sleep(per)
		}
		fmt.Printf("  %d connections ready\n", len(clients))

	case "s2":
		fmt.Printf("  conns %d  rooms %d  duration %s\n", *connsN, *roomsN, *duration)
		for i := 0; i < *roomsN; i++ {
			code, key, err := createRoom(*base, fmt.Sprintf("loadtest-s2-%d", i))
			if err != nil {
				fmt.Fprintf(os.Stderr, "create room: %v\n", err)
				os.Exit(1)
			}
			rooms = append(rooms, struct{ code, key string }{code, key})
		}
		fmt.Printf("  %d rooms created\n", len(rooms))

		per := *ramp / time.Duration(*connsN)
		if per <= 0 {
			per = time.Millisecond
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, 64)
		for i := 0; i < *connsN; i++ {
			r := rooms[i%len(rooms)]
			c := newClient(i, r.code, r.key, "s2")
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if err := h.connect(c); err != nil {
					h.st.errors.Add(1)
					return
				}
				mu.Lock()
				clients = append(clients, c)
				mu.Unlock()
			}()
			time.Sleep(per)
		}
		wg.Wait()
		fmt.Printf("  %d connections ready\n", len(clients))

	case "s3":
		perRoom := (*clientsN + *roomsN - 1) / *roomsN
		fmt.Printf("  rooms %d x %d clients  rate %d msg/s  duration %s\n", *roomsN, perRoom, *rate, *duration)
		for i := 0; i < *roomsN; i++ {
			code, key, err := createRoom(*base, fmt.Sprintf("loadtest-s3-%d", i))
			if err != nil {
				fmt.Fprintf(os.Stderr, "create room: %v\n", err)
				os.Exit(1)
			}
			rooms = append(rooms, struct{ code, key string }{code, key})
		}
		per := *ramp / time.Duration(*clientsN)
		if per <= 0 {
			per = time.Millisecond
		}
		for i := 0; i < *clientsN; i++ {
			r := rooms[(i/perRoom)%len(rooms)]
			c := newClient(i, r.code, r.key, "s3")
			if err := h.connect(c); err != nil {
				h.st.errors.Add(1)
				continue
			}
			clients = append(clients, c)
			time.Sleep(per)
		}
		fmt.Printf("  %d connections ready\n", len(clients))
	}

	drawLimit := len(clients)
	if *drawers > 0 && *drawers < len(clients) {
		drawLimit = *drawers
	}
	if *scenario == "s2" {
		drawLimit = 0
	}
	fmt.Printf("  drawers %d / connections %d\n", drawLimit, len(clients))

	stopCh := make(chan struct{})
	var tickWG sync.WaitGroup
	// Every drawing goroutine needs its own ticker. A shared ticker delivers each
	// tick to exactly one receiver, so 100 goroutines on one ticker send far fewer
	// ops than intended (the first version emitted 299 instead of 15000).
	for i := 0; i < drawLimit; i++ {
		tickWG.Add(1)
		go func(c *client) {
			defer tickWG.Done()
			tk := time.NewTicker(time.Second / time.Duration(max(1, *rate)))
			defer tk.Stop()
			for {
				select {
				case <-stopCh:
					return
				case <-tk.C:
					h.sendOp(c)
				}
			}
		}(clients[i])
	}

	if *scenario == "s2" {
		go func() {
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stopCh:
					return
				case <-t.C:
					mu.Lock()
					snapshot := append([]*client(nil), clients...)
					mu.Unlock()
					for _, c := range snapshot {
						pj := []byte(fmt.Sprintf(`{"x":%d,"y":%d}`, rand.Intn(1000), rand.Intn(1000)))
						env, _ := json.Marshal(envelope{
							V: 1, Type: "presence.cursor", RoomID: c.room, ClientID: c.cid,
							TS: time.Now().UnixMilli(), Payload: pj,
						})
						_ = c.conn.WriteMessage(websocket.TextMessage, env)
					}
				}
			}
		}()
	}

	done := make(chan struct{})
	var peak opsData
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		start := time.Now()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				d := fetchOps(*base)
				if d.Connections > peak.Connections {
					peak = d
				}
				fmt.Printf("\r  %3.0fs sent=%d ack=%d bcast=%d heap=%.1fMB conn=%d   ",
					time.Since(start).Seconds(), h.st.sent.Load(), h.st.acks.Load(),
					h.st.broadcast.Load(), d.HeapMB, d.Connections)
			}
		}
	}()

	time.Sleep(*duration)
	close(stopCh)
	close(done)
	tickWG.Wait()
	time.Sleep(2 * time.Second)

	after := fetchOps(*base)
	p50, p95, p99, mx, n := h.st.percentiles()

	fmt.Printf("\n\nRESULT\n")
	fmt.Printf("  clients           %d\n", len(clients))
	fmt.Printf("  ops sent          %d\n", h.st.sent.Load())
	fmt.Printf("  acks              %d\n", h.st.acks.Load())
	fmt.Printf("  broadcasts recv   %d\n", h.st.broadcast.Load())
	fmt.Printf("  errors            %d\n", h.st.errors.Load())
	fmt.Printf("  unexpected drop   %d\n", h.st.dropped.Load())
	if n > 0 {
		fmt.Printf("  latency p50       %.2f ms\n", float64(p50.Microseconds())/1000)
		fmt.Printf("  latency p95       %.2f ms\n", float64(p95.Microseconds())/1000)
		fmt.Printf("  latency p99       %.2f ms\n", float64(p99.Microseconds())/1000)
		fmt.Printf("  latency max       %.2f ms\n", float64(mx.Microseconds())/1000)
		fmt.Printf("  latency samples   %d\n", n)
	} else {
		fmt.Printf("  latency           no samples (s2 sends no ops)\n")
	}
	fmt.Printf("  server heap       %.2f MB\n", after.HeapMB)
	fmt.Printf("  server goroutines %d\n", after.Goroutines)
	fmt.Printf("  server conns      %d\n", after.Connections)
	fmt.Printf("  server p95 hist   %.2f ms\n", after.BroadcastLatencyP95Ms)
	fmt.Printf("  slow kicks        %d\n", after.SlowConsumerKicks)
	fmt.Printf("  server msg/s in   %.0f\n", after.MsgPerSecIn)
	fmt.Printf("  server msg/s out  %.0f\n", after.MsgPerSecOut)
	if peak.Connections > 0 {
		fmt.Printf("  server conn peak  %d\n", peak.Connections)
	}

	for _, c := range clients {
		c.closed.Store(true)
		_ = c.conn.Close()
	}

	if h.st.errors.Load() > 0 || h.st.dropped.Load() > 0 {
		fmt.Printf("\nERRORS OR DROPS PRESENT\n")
		os.Exit(2)
	}
	fmt.Printf("\nNO ERRORS\n")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
