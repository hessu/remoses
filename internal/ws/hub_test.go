package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/hessu/remoses/internal/auth"
	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig"
)

const (
	testUser = "n0call"
	testPass = "hunter2"
)

// fakeClock drives ticket expiry without sleeping. It is atomic because the
// ticket store is read from the HTTP server's goroutines while the test
// advances it.
type fakeClock struct{ nanos atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.nanos.Store(time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *fakeClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

type harnessOpts struct {
	radios []string
	poll   time.Duration
	ws     config.WS
	// eventQueue sizes each session's fan-out buffer; 1 makes drops certain
	// under a flood.
	eventQueue int
}

type harness struct {
	t     *testing.T
	hub   *Hub
	srv   *httptest.Server
	mgr   *rig.Manager
	auth  *auth.Authenticator
	clock *fakeClock

	sessions map[string]*rig.Session
	devs     map[string]*fakeDevice
	cws      map[string]*fakeCW
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()

	if len(o.radios) == 0 {
		o.radios = []string{"ic7610"}
	}
	if o.poll == 0 {
		o.poll = time.Hour
	}

	h := &harness{
		t:        t,
		clock:    newFakeClock(),
		sessions: map[string]*rig.Session{},
		devs:     map[string]*fakeDevice{},
		cws:      map[string]*fakeCW{},
	}

	var sessions []*rig.Session
	for _, id := range o.radios {
		s, dev, snd := newTestSession(t, id, o.poll, o.eventQueue)
		sessions = append(sessions, s)
		h.sessions[id] = s
		h.devs[id] = dev
		h.cws[id] = snd
	}

	mgr, err := rig.NewManagerWithSessions(sessions...)
	if err != nil {
		t.Fatalf("NewManagerWithSessions: %v", err)
	}
	h.mgr = mgr

	hash, err := auth.HashPassword(testPass, 4)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	a, err := auth.New(config.Auth{
		Realm:      "remoses-test",
		BcryptCost: 4,
		CacheTTL:   config.Duration(time.Minute),
		Users:      []config.User{{Username: testUser, PasswordBcrypt: hash}},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	h.auth = a

	h.hub = NewHub(mgr, o.ws, WithLogger(discardLogger()), WithVersion("test"))
	// Installed before anything can serve a request, so the swap itself is not
	// a race with the handlers that read it.
	h.hub.tickets.now = h.clock.Now

	mux := http.NewServeMux()
	// /ws is mounted unauthenticated so the ticket path can be exercised, and
	// again behind the middleware for the header path, which is how a real
	// server has to wire it: the middleware would 401 a browser that has no
	// way to send Authorization.
	mux.Handle("/ws", h.hub.Handler())
	mux.Handle("/ws-authed", a.Middleware(h.hub.Handler()))
	mux.Handle("/ws-ticket", a.Middleware(h.hub.TicketHandler(a)))
	mux.Handle("/ws-ticket-open", h.hub.TicketHandler(a))
	h.srv = httptest.NewServer(mux)

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	go h.hub.Run(ctx)

	t.Cleanup(func() {
		h.hub.Close()
		h.srv.Close()
		cancel()
		_ = mgr.Close()
	})

	for _, id := range o.radios {
		s := h.sessions[id]
		waitFor(t, "radio "+id+" connected", s.Connected)
	}
	return h
}

func (h *harness) url(path string) string { return h.srv.URL + path }

// dial opens a client connection and fails the test if the handshake does not
// succeed.
func (h *harness) dial(path string, opts *websocket.DialOptions) *websocket.Conn {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, h.url(path), opts)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		h.t.Fatalf("dial %s: status %d: %v", path, status, err)
	}
	h.t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// dialBasic opens an authenticated connection through the middleware-wrapped
// mount, which is the programmatic client's path.
func (h *harness) dialBasic(path string) *websocket.Conn {
	h.t.Helper()
	return h.dial(path, &websocket.DialOptions{HTTPHeader: basicHeader()})
}

func basicHeader() http.Header {
	req, _ := http.NewRequest(http.MethodGet, "http://example/", nil)
	req.SetBasicAuth(testUser, testPass)
	return http.Header{"Authorization": []string{req.Header.Get("Authorization")}}
}

// readMsg reads one server frame as a generic object, so assertions can be
// written against the wire format rather than against this package's structs.
func readMsg(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("message type = %v, want text", typ)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	return m
}

// readUntil reads until pred is satisfied or the deadline passes.
func readUntil(t *testing.T, c *websocket.Conn, what string, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m := readMsg(t, c)
		if pred(m) {
			return m
		}
	}
	t.Fatalf("timed out waiting for %s", what)
	return nil
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func num(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("field %q = %v, want a number", key, m[key])
	}
	return v
}

func obj(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("field %q = %v, want an object", key, m[key])
	}
	return v
}

// TestHelloThenSnapshotsThenDeltas covers the opening contract: hello first,
// then exactly one snapshot per subscribed radio in configuration order, then
// deltas.
func TestHelloThenSnapshotsThenDeltas(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{radios: []string{"ic7610", "ts590sg"}})
	c := h.dialBasic("/ws-authed")

	hello := readMsg(t, c)
	if got := str(hello, "type"); got != "hello" {
		t.Fatalf("first message type = %q, want hello", got)
	}
	if got := str(hello, "version"); got != "test" {
		t.Errorf("hello.version = %q, want test", got)
	}
	radios, _ := hello["radios"].([]any)
	if len(radios) != 2 || radios[0] != "ic7610" || radios[1] != "ts590sg" {
		t.Fatalf("hello.radios = %v, want [ic7610 ts590sg]", hello["radios"])
	}
	if _, err := time.Parse(time.RFC3339Nano, str(hello, "server_time")); err != nil {
		t.Errorf("hello.server_time = %q: %v", str(hello, "server_time"), err)
	}

	for _, want := range []string{"ic7610", "ts590sg"} {
		m := readMsg(t, c)
		if str(m, "type") != "state" || str(m, "radio") != want {
			t.Fatalf("got %v, want a state snapshot for %s", m, want)
		}
		st := obj(t, m, "state")
		if st["frequency"].(float64) != 14025000 {
			t.Errorf("%s snapshot frequency = %v, want 14025000", want, st["frequency"])
		}
		if num(t, m, "seq") != st["seq"].(float64) {
			t.Errorf("%s envelope seq %v disagrees with state.seq %v", want, m["seq"], st["seq"])
		}
	}

	h.devs["ic7610"].tune(14025300)

	m := readMsg(t, c)
	if str(m, "type") != "delta" || str(m, "radio") != "ic7610" {
		t.Fatalf("got %v, want a delta for ic7610", m)
	}
	changed := obj(t, m, "changed")
	if changed["frequency"] != float64(14025300) {
		t.Fatalf("delta changed = %v, want frequency 14025300", changed)
	}
	if len(changed) != 1 {
		t.Errorf("delta carried %v, want only the changed field", changed)
	}
}

// TestRadiosFilter covers ?radios=: the connection must carry that radio and
// nothing else.
func TestRadiosFilter(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{radios: []string{"ic7610", "ts590sg"}})
	c := h.dialBasic("/ws-authed?radios=ts590sg")

	hello := readMsg(t, c)
	radios, _ := hello["radios"].([]any)
	if len(radios) != 1 || radios[0] != "ts590sg" {
		t.Fatalf("hello.radios = %v, want [ts590sg]", hello["radios"])
	}

	snap := readMsg(t, c)
	if str(snap, "type") != "state" || str(snap, "radio") != "ts590sg" {
		t.Fatalf("got %v, want a state snapshot for ts590sg", snap)
	}

	// The filtered-out radio must produce nothing at all, so the next frame has
	// to be the one from the radio we did subscribe to.
	h.devs["ic7610"].tune(21000000)
	h.devs["ts590sg"].tune(7010000)

	m := readMsg(t, c)
	if str(m, "radio") != "ts590sg" {
		t.Fatalf("got %v, want a frame for ts590sg only", m)
	}
	if obj(t, m, "changed")["frequency"] != float64(7010000) {
		t.Fatalf("changed = %v, want frequency 7010000", m["changed"])
	}
}

// TestSubscribeMessage covers the one client message that changes anything.
func TestSubscribeMessage(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{radios: []string{"ic7610", "ts590sg"}})
	c := h.dialBasic("/ws-authed?radios=ic7610")

	readMsg(t, c) // hello
	readMsg(t, c) // ic7610 snapshot

	writeClient(t, c, `{"type":"subscribe","radios":["ts590sg"]}`)

	snap := readUntil(t, c, "a snapshot for the newly subscribed radio", func(m map[string]any) bool {
		return str(m, "type") == "state" && str(m, "radio") == "ts590sg"
	})
	if snap == nil {
		t.Fatal("no snapshot")
	}

	// ic7610 was dropped by the subscribe, so only ts590sg may appear now.
	h.devs["ic7610"].tune(21000000)
	h.devs["ts590sg"].tune(7020000)

	m := readMsg(t, c)
	if str(m, "radio") != "ts590sg" {
		t.Fatalf("got %v, want ts590sg only", m)
	}
}

// TestUnknownClientMessagesIgnored is the security property of this endpoint:
// there is no message a client can send that reaches a radio.
func TestUnknownClientMessagesIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})
	c := h.dialBasic("/ws-authed")

	readMsg(t, c) // hello
	readMsg(t, c) // snapshot

	for _, junk := range []string{
		`{"type":"set_frequency","radio":"ic7610","frequency":1810000}`,
		`{"type":"ptt","on":true}`,
		`{"type":"unsubscribe"}`,
		`{"type":42}`,
		`not json at all`,
		`{"type":"ping"}`,
	} {
		writeClient(t, c, junk)
	}

	// The radio must be untouched and the connection must still work.
	if got := h.sessions["ic7610"].State().Frequency; got != 14025000 {
		t.Fatalf("frequency = %d, want 14025000: a client message reached the radio", got)
	}

	h.devs["ic7610"].tune(14025900)
	m := readUntil(t, c, "a delta after the junk", func(m map[string]any) bool {
		return str(m, "type") == "delta"
	})
	if obj(t, m, "changed")["frequency"] != float64(14025900) {
		t.Fatalf("changed = %v, want frequency 14025900", m["changed"])
	}
}

func writeClient(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
		t.Fatalf("write %s: %v", s, err)
	}
}

// TestRateLimit proves the per-radio floor is honoured: a flood arrives as at
// most one frame per min_interval, and the last one carries the newest value.
func TestRateLimit(t *testing.T) {
	t.Parallel()
	const minInterval = 200 * time.Millisecond
	h := newHarness(t, harnessOpts{ws: config.WS{MinInterval: config.Duration(minInterval)}})
	c := h.dialBasic("/ws-authed")

	readMsg(t, c) // hello
	readMsg(t, c) // snapshot

	const steps = 40
	final := uint64(14000000 + steps)
	go func() {
		for i := 1; i <= steps; i++ {
			h.devs["ic7610"].tune(uint64(14000000 + i))
			time.Sleep(15 * time.Millisecond)
		}
	}()

	var stamps []time.Time
	var last float64
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		m := readMsg(t, c)
		if str(m, "type") != "delta" {
			continue
		}
		stamps = append(stamps, time.Now())
		last = obj(t, m, "changed")["frequency"].(float64)
		if last == float64(final) {
			break
		}
	}

	if last != float64(final) {
		t.Fatalf("last delta frequency = %v, want %d", last, final)
	}
	// 40 changes over ~600 ms at a 200 ms floor: a handful of frames, not forty.
	if len(stamps) > 8 {
		t.Errorf("got %d deltas for %d changes, want the rate limit to collapse them", len(stamps), steps)
	}
	// Allow a little slack for scheduling; the point is that nothing arrives
	// back to back.
	const slack = 20 * time.Millisecond
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap < minInterval-slack {
			t.Errorf("deltas %d and %d were %s apart, want at least %s", i-1, i, gap, minInterval)
		}
	}
}

// TestCoalescingUnderStall floods a client that is not reading and proves the
// two things that matter: the rig session never waits for it, and when it comes
// back it is told the truth rather than a queue of history.
func TestCoalescingUnderStall(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{
		ws:         config.WS{MinInterval: config.Duration(50 * time.Millisecond)},
		eventQueue: 1, // guarantees the session's fan-out drops under the flood
	})
	c := h.dialBasic("/ws-authed")

	readMsg(t, c) // hello
	readMsg(t, c) // snapshot

	// Nothing reads from c for the duration of the flood.
	const updates = 2000
	flooded := uint64(14000000 + updates)

	start := time.Now()
	for i := 1; i <= updates; i++ {
		h.devs["ic7610"].tune(uint64(14000000 + i))
	}
	elapsed := time.Since(start)

	// The publisher must not have been slowed by the client that is not
	// reading. Two seconds is not a benchmark; it is "did we block".
	if elapsed > 2*time.Second {
		t.Fatalf("publishing %d updates took %s: a stalled client slowed the session", updates, elapsed)
	}
	waitFor(t, "the session to reach the flooded frequency", func() bool {
		return h.sessions["ic7610"].State().Frequency == flooded
	})

	// The session's own fan-out is bounded as well, and a drop there is only
	// reported on the NEXT event, so the tail of a flood can be lost with
	// nothing left to carry the news. Mark the end with one more change once
	// the stream has quietened; that is what the client must converge on.
	time.Sleep(200 * time.Millisecond)
	const marker = uint64(14999000)
	h.devs["ic7610"].tune(marker)
	waitFor(t, "the session to reach the marker frequency", func() bool {
		return h.sessions["ic7610"].State().Frequency == marker
	})

	// Now read. The client must be told it fell behind, and must converge on
	// the newest state without being sent one frame per update.
	frames, resyncs, converged := 0, 0, false
	deadline := time.Now().Add(5 * time.Second)
	for !converged && time.Now().Before(deadline) {
		m := readMsg(t, c)
		frames++
		switch str(m, "type") {
		case "resync":
			if str(m, "radio") == "ic7610" {
				resyncs++
			}
		case "delta":
			converged = obj(t, m, "changed")["frequency"] == float64(marker)
		case "state":
			converged = obj(t, m, "state")["frequency"] == float64(marker)
		}
	}

	if !converged {
		t.Fatalf("client never converged on the newest state after %d frames", frames)
	}
	if resyncs == 0 {
		t.Error("no resync, although the fan-out reported dropped events")
	}
	if frames > 100 {
		t.Errorf("took %d frames to convey %d coalescible updates", frames, updates)
	}
}

// TestConnEventsAreDiscrete covers the conn envelope, which must survive a
// round trip through the event lane rather than being merged into state.
func TestConnEventsAreDiscrete(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})
	c := h.dialBasic("/ws-authed")

	readMsg(t, c) // hello
	readMsg(t, c) // snapshot

	h.devs["ic7610"].drop()

	down := readUntil(t, c, "a conn frame reporting the loss", func(m map[string]any) bool {
		return str(m, "type") == "conn" && m["connected"] == false
	})
	if str(down, "radio") != "ic7610" {
		t.Errorf("conn.radio = %q, want ic7610", str(down, "radio"))
	}
	if str(down, "error") == "" {
		t.Error("conn frame carried no reason for the disconnect")
	}
	// A discrete event says which version of the radio it describes, like every
	// other frame. Without it a client would have to guess whether the conn
	// event it just read is older or newer than the state it is holding.
	if num(t, down, "seq") <= 0 {
		t.Errorf("conn.seq = %v, want the session's sequence number", down["seq"])
	}
	if str(down, "ts") == "" {
		t.Error("conn frame carried no timestamp")
	}

	up := readUntil(t, c, "a conn frame reporting the reconnect", func(m map[string]any) bool {
		return str(m, "type") == "conn" && m["connected"] == true
	})
	if str(up, "radio") != "ic7610" {
		t.Errorf("conn.radio = %q, want ic7610", str(up, "radio"))
	}
}

// TestCWEventsAreDiscrete covers the cw envelope.
func TestCWEventsAreDiscrete(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{poll: 10 * time.Millisecond})
	c := h.dialBasic("/ws-authed")

	readMsg(t, c) // hello
	readMsg(t, c) // snapshot

	h.cws["ic7610"].churn.Store(true)

	// The first cw frame may report the idle sender: the session publishes the
	// transition from "no CW status at all" to "idle at 28 wpm" too. Wait for
	// one that reports a queue.
	m := readUntil(t, c, "a cw frame reporting a queue", func(m map[string]any) bool {
		if str(m, "type") != "cw" {
			return false
		}
		q, ok := m["queued"].(float64)
		return ok && q >= 1
	})
	if str(m, "radio") != "ic7610" {
		t.Errorf("cw.radio = %q, want ic7610", str(m, "radio"))
	}
	if m["busy"] != true {
		t.Errorf("cw.busy = %v, want true", m["busy"])
	}
	if num(t, m, "queued") < 1 {
		t.Errorf("cw.queued = %v, want at least 1", m["queued"])
	}
	if num(t, m, "wpm") != 28 {
		t.Errorf("cw.wpm = %v, want 28", m["wpm"])
	}
	if num(t, m, "seq") <= 0 {
		t.Errorf("cw.seq = %v, want the session's sequence number", m["seq"])
	}
	if str(m, "ts") == "" {
		t.Error("cw frame carried no timestamp")
	}
}

// TestCWQueueChangesTravelAsDeltas is what an IC-7610 sending Morse showed:
// the queue moving on every poll while busy stayed true, and every one of those
// polls arriving as a FULL STATE SNAPSHOT because the patch behind it was
// empty. Ten seconds of CW cost sixteen snapshots of fifty fields each.
//
// The state lane is allowed to send a snapshot — it is the honest answer to a
// change no delta can name — so this asserts the absence rather than trusting
// the presence: while nothing but the queue is moving, nothing but deltas
// carrying `cw` should come out.
func TestCWQueueChangesTravelAsDeltas(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{poll: 10 * time.Millisecond})
	c := h.dialBasic("/ws-authed")

	readMsg(t, c) // hello
	readMsg(t, c) // the opening snapshot, which is owed and expected

	h.cws["ic7610"].churn.Store(true)

	const want = 5
	deltas, snapshots := 0, 0
	deadline := time.Now().Add(5 * time.Second)
	for deltas < want && time.Now().Before(deadline) {
		m := readMsg(t, c)
		switch str(m, "type") {
		case "delta":
			if _, ok := obj(t, m, "changed")["cw"]; ok {
				deltas++
			}
		case "state":
			snapshots++
		}
	}

	if deltas < want {
		t.Fatalf("saw %d cw deltas in 5 s, want %d", deltas, want)
	}
	if snapshots > 0 {
		t.Errorf("%d full state snapshots for a queue that only moved; "+
			"the delta path is not carrying the CW status", snapshots)
	}
}

// TestKeepaliveKeepsResponsiveClient proves the keepalive runs and that a
// client answering it is left alone.
func TestKeepaliveKeepsResponsiveClient(t *testing.T) {
	t.Parallel()
	const ping = 40 * time.Millisecond
	h := newHarness(t, harnessOpts{ws: config.WS{PingInterval: config.Duration(ping)}})

	var pings atomic.Int64
	c := h.dial("/ws-authed", &websocket.DialOptions{
		HTTPHeader: basicHeader(),
		OnPingReceived: func(context.Context, []byte) bool {
			pings.Add(1)
			return true // answer it
		},
	})

	readMsg(t, c) // hello
	readMsg(t, c) // snapshot

	// Keep reading so the client library processes control frames.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 8*ping)
		defer cancel()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}()
	<-done

	if pings.Load() < 2 {
		t.Fatalf("server sent %d pings in %s, want at least 2", pings.Load(), 8*ping)
	}
	if h.hub.clientCount() != 1 {
		t.Fatalf("client count = %d, want the responsive client still connected", h.hub.clientCount())
	}
}

// TestKeepaliveClosesDeadClient is a safety property, not a tidiness one: a
// client that has stopped answering still holds its radio lock, and only
// dropping it lets the lease expire and PTT fall.
func TestKeepaliveClosesDeadClient(t *testing.T) {
	t.Parallel()
	const ping = 40 * time.Millisecond
	h := newHarness(t, harnessOpts{ws: config.WS{PingInterval: config.Duration(ping)}})

	c := h.dial("/ws-authed", &websocket.DialOptions{
		HTTPHeader: basicHeader(),
		OnPingReceived: func(context.Context, []byte) bool {
			return false // swallow it: the peer has gone quiet
		},
	})

	readMsg(t, c) // hello
	readMsg(t, c) // snapshot

	// Keep reading so the pings are actually delivered and then suppressed,
	// rather than sitting in the socket buffer.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}()

	waitFor(t, "the unresponsive client to be dropped", func() bool {
		return h.hub.clientCount() == 0
	})
}

// TestUnauthenticatedUpgradeRejected covers the bare /ws mount: no context
// user and no ticket is a 401, not an open socket.
func TestUnauthenticatedUpgradeRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, h.url("/ws"), nil)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("upgrade succeeded without credentials")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}

// TestHubCloseDisconnectsClients covers shutdown.
func TestHubCloseDisconnectsClients(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})
	c := h.dialBasic("/ws-authed")
	readMsg(t, c) // hello

	h.hub.Close()

	waitFor(t, "clients to be disconnected", func() bool { return h.hub.clientCount() == 0 })

	// Anything already written is still readable; what must not happen is the
	// stream staying open.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			break
		}
	}
}

// --- White-box tests for the queue itself. -------------------------------
//
// Overflow of the discrete lane needs the writer to be genuinely blocked, which
// over a real socket means filling the kernel's buffers — a test that would be
// slow and platform dependent. The queue is exercised directly instead: it is
// the same code the writer drains.

func newQueueClient(t *testing.T, h *Hub, radios ...string) *client {
	t.Helper()
	return newClient(h, nil, testUser, radios, func() {})
}

func stateEvent(id string, seq uint64, hz uint64) rig.Event {
	st := radio.State{Frequency: hz, Seq: seq, Connected: true, UpdatedAt: time.Now()}
	return rig.Event{
		Kind: rig.EventState, RadioID: id, Seq: seq, At: st.UpdatedAt,
		State: st, Patch: radio.Patch{Frequency: &st.Frequency},
	}
}

func drainTypes(msgs []any) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		switch v := m.(type) {
		case resyncMsg:
			out = append(out, v.Type)
		case stateMsg:
			out = append(out, v.Type)
		case deltaMsg:
			out = append(out, v.Type)
		case cwMsg:
			out = append(out, v.Type)
		case connMsg:
			out = append(out, v.Type)
		}
	}
	return out
}

// TestDiscreteLaneOverflowResyncs proves the event lane admits the loss rather
// than handing the client a truncated history.
func TestDiscreteLaneOverflowResyncs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})
	h.hub.queueCap = 3
	c := newQueueClient(t, h.hub, "ic7610")
	// The snapshot the client is owed at connect is not what is under test.
	c.snapshot = map[string]struct{}{}

	for i := range 10 {
		ev := stateEvent("ic7610", uint64(i+1), 14000000)
		ev.Kind = rig.EventCW
		ev.State.CW = radio.CWStatus{Busy: true, Queued: i, WPM: 28}
		c.enqueue(ev)
	}

	msgs, _ := c.drain(time.Now())
	types := drainTypes(msgs)
	if len(types) == 0 || types[0] != typeResync {
		t.Fatalf("drained %v, want a resync first", types)
	}
	cw := 0
	for _, ty := range types {
		if ty == typeCW {
			cw++
		}
	}
	if cw > 3 {
		t.Errorf("drained %d cw frames from a queue of 3", cw)
	}
}

// TestDroppedEventResyncs covers the session-level fan-out having already lost
// events for us.
func TestDroppedEventResyncs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{radios: []string{"ic7610", "ts590sg"}})
	c := newQueueClient(t, h.hub, "ic7610", "ts590sg")
	c.snapshot = map[string]struct{}{}

	ev := stateEvent("ic7610", 7, 14020000)
	ev.Dropped = 4
	c.enqueue(ev)

	msgs, _ := c.drain(time.Now())
	var resyncs []string
	for _, m := range msgs {
		if r, ok := m.(resyncMsg); ok {
			resyncs = append(resyncs, r.Radio)
		}
	}
	// The drop counter is not per radio, so every subscribed radio is suspect.
	if len(resyncs) != 2 {
		t.Fatalf("resynced %v, want both subscribed radios", resyncs)
	}
}

// TestStateLaneCoalesces proves the state lane keeps only the newest per radio,
// however deep the flood.
func TestStateLaneCoalesces(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})
	c := newQueueClient(t, h.hub, "ic7610")
	c.snapshot = map[string]struct{}{}

	for i := 1; i <= 1000; i++ {
		c.enqueue(stateEvent("ic7610", uint64(i), uint64(14000000+i)))
	}

	msgs, _ := c.drain(time.Now())
	if len(msgs) != 1 {
		t.Fatalf("drained %d messages from 1000 coalescible updates, want 1", len(msgs))
	}
	d, ok := msgs[0].(deltaMsg)
	if !ok {
		t.Fatalf("drained %T, want a delta", msgs[0])
	}
	if d.Seq != 1000 || d.Changed["frequency"] != uint64(14001000) {
		t.Fatalf("delta = seq %d %v, want the newest update", d.Seq, d.Changed)
	}
}

// TestStaleEventsNeverGoBackwards proves seq is never reordered: an event a
// snapshot already superseded is dropped, not sent.
func TestStaleEventsNeverGoBackwards(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})
	c := newQueueClient(t, h.hub, "ic7610")
	c.snapshot = map[string]struct{}{}

	c.enqueue(stateEvent("ic7610", 10, 14010000))
	if msgs, _ := c.drain(time.Now()); len(msgs) != 1 {
		t.Fatalf("drained %d messages, want 1", len(msgs))
	}

	// An older event arriving late must not be written.
	c.enqueue(stateEvent("ic7610", 9, 13000000))
	c.nextAt = map[string]time.Time{}
	if msgs, _ := c.drain(time.Now()); len(msgs) != 0 {
		t.Fatalf("drained %v, want a stale event to be dropped", drainTypes(msgs))
	}
}

// TestChangedFieldsUsesStateNames pins the delta vocabulary to radio.State's
// JSON tags, so `changed` is always a subset of `state`.
func TestChangedFieldsUsesStateNames(t *testing.T) {
	t.Parallel()

	freq := uint64(14025300)
	mode := radio.ModeCWR
	data := true
	passband := 500
	slot := 2
	power := radio.Power{Pct: 40, Native: 102}
	ptt := true
	meter := radio.Meter{Raw: 78, Scale: 255}
	cw := radio.CWStatus{Busy: true, Queued: 12, WPM: 28}
	connected := false

	p := radio.Patch{
		Frequency: &freq, Mode: &mode, DataMode: &data, PassbandHz: &passband,
		FilterSlot: &slot, Power: &power, PTT: &ptt, SMeter: &meter,
		SWR: &meter, ALC: &meter, CW: &cw, Connected: &connected,
	}
	st := radio.State{CW: radio.CWStatus{Busy: true, Queued: 12, WPM: 28}}

	b, err := json.Marshal(changedFields(p, st))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{
		"frequency", "mode", "data_mode", "passband_hz", "filter_slot",
		"power", "ptt", "s_meter", "swr", "alc", "cw", "connected",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("changed is missing %q: %s", key, b)
		}
	}
	if got["mode"] != "CW-R" {
		t.Errorf("changed.mode = %v, want CW-R", got["mode"])
	}
	if got["frequency"] != float64(14025300) {
		t.Errorf("changed.frequency = %v", got["frequency"])
	}

	// Every name must exist in radio.State's own encoding, or a client would
	// need two vocabularies.
	//
	// The reference state has to carry the OPTIONAL fields too. They are
	// omitempty, so a zero State encodes without them and every one of them
	// would look like a name radio.State does not have.
	ratio := 1.5
	sb, err := json.Marshal(radio.State{
		SWR: &meter, ALC: &meter, PowerMeter: &meter, SWRRatio: &ratio,
		Tuner: radio.TunerOn, Standby: true,
	})
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	var stateKeys map[string]any
	if err := json.Unmarshal(sb, &stateKeys); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	for key := range got {
		if _, ok := stateKeys[key]; !ok {
			t.Errorf("delta field %q is not a radio.State field", key)
		}
	}
}

// TestResolveRadios covers the ?radios= parser, including ids this instance
// does not have.
func TestResolveRadios(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{radios: []string{"ic7610", "ts590sg"}})

	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{"ic7610", "ts590sg"}},
		{"  ", []string{"ic7610", "ts590sg"}},
		{"ts590sg", []string{"ts590sg"}},
		{"ts590sg,ic7610", []string{"ic7610", "ts590sg"}}, // configuration order
		{" ic7610 , nope ", []string{"ic7610"}},
		{"nope", nil},
	}
	for _, tc := range cases {
		got := h.hub.resolveRadios(tc.in)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("resolveRadios(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
