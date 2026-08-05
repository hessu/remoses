package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/hessu/remoses/internal/client"
)

const (
	daemonUser = "oh2xyz"
	daemonPass = "hunter2"
	daemonBase = "/api/v1"
)

const descriptorJSON = `{"id":"ic7610","name":"IC-7610","backend":"civ","connected":true,
	"caps":{"s_meter_scale":255,"cw_method":"cat"},
	"limits":{},"lock":{"held":false}}`

func stateBody(freqHz uint64, seq uint64) string {
	return fmt.Sprintf(`{"frequency":%d,"mode":"CW","data_mode":false,
		"passband_hz":500,"filter_slot":2,
		"power":{"pct":40,"watts":null,"native":102},"ptt":false,
		"s_meter":{"raw":78,"scale":255,"s":5.5},
		"cw":{"busy":false,"queued":0,"wpm":28},
		"connected":true,"seq":%d,"updated_at":"2026-08-04T20:11:04Z",
		"age_ms":120,"stale":false}`, freqHz, seq)
}

// daemon is a stand-in for remoses: the same routes, the same auth, and a real
// websocket server rather than a fake, so the client is exercised against the
// protocol instead of against a mock of it.
type daemon struct {
	srv *httptest.Server

	mu           sync.Mutex
	stateBodies  []string // consumed in order; the last one repeats
	stateFetches int
	descFetches  int
	dials        int
	// wsStatus, when non-zero, makes the upgrade fail with that status instead
	// of being accepted.
	wsStatus int

	// script drives one accepted connection. n is the dial number, so a test
	// can behave differently on a reconnect.
	script func(conn *websocket.Conn, n int, done <-chan struct{})
}

func newDaemon(t *testing.T, d *daemon) *daemon {
	t.Helper()

	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+daemonBase+"/radios/{radioId}", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		d.descFetches++
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(descriptorJSON))
	})
	mux.HandleFunc("GET "+daemonBase+"/radios/{radioId}/state", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		body := d.stateBodies[min(d.stateFetches, len(d.stateBodies)-1)]
		d.stateFetches++
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("GET "+daemonBase+"/ws", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.dials++
		n := d.dials
		script := d.script
		status := d.wsStatus
		d.mu.Unlock()

		if status != 0 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"title":%q,"status":%d,"detail":"refused"}`,
				http.StatusText(status), status)
			return
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if script != nil {
			script(conn, n, done)
		}
	})

	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != daemonUser || pass != daemonPass {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,` +
				`"detail":"missing or invalid credentials"}`))
			return
		}
		mux.ServeHTTP(w, r)
	}))

	t.Cleanup(func() {
		stop()
		d.srv.Close()
	})
	return d
}

func (d *daemon) counts() (state, desc, dials int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stateFetches, d.descFetches, d.dials
}

func (d *daemon) client(t *testing.T, pass string) *client.Client {
	t.Helper()
	c, err := client.New(d.srv.URL+daemonBase, daemonUser, pass)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

// syncBuffer is written by the monitor's goroutine and read by the test's.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls until the output contains want. Polling beats a channel here
// because the thing under test is a whole pipeline, and what a test wants to
// assert is that the fact reached the screen.
func waitFor(t *testing.T, b *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(b.String(), want) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q; output was:\n%s", want, b.String())
}

func send(conn *websocket.Conn, msg string) error {
	return conn.Write(context.Background(), websocket.MessageText, []byte(msg))
}

func newTestMonitor(cl *client.Client, out renderer) *monitor {
	m := newMonitor(cl, "ic7610", out, newView("ic7610", time.Now))
	return m
}

// The REST fetch puts something on screen at once; the stream keeps it current.
func TestMonitorFetchesInitialStateThenFollowsTheStream(t *testing.T) {
	d := newDaemon(t, &daemon{
		stateBodies: []string{stateBody(14025000, 4471)},
		script: func(conn *websocket.Conn, _ int, done <-chan struct{}) {
			_ = send(conn, `{"type":"hello","version":"0.1.0","radios":["ic7610"]}`)
			// The stream sends a snapshot per radio on connect, identical to
			// what REST just answered.
			_ = send(conn, `{"type":"state","radio":"ic7610","seq":4471,`+
				`"ts":"2026-08-04T20:11:04Z","state":`+stateBody(14025000, 4471)+`}`)
			_ = send(conn, `{"type":"delta","radio":"ic7610","seq":4472,`+
				`"ts":"2026-08-04T20:11:05Z","changed":{"frequency":14025300}}`)
			<-done
		},
	})

	var buf syncBuffer
	m := newTestMonitor(d.client(t, daemonPass), newPlainRenderer(&buf, time.Now, time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- m.run(ctx) }()

	waitFor(t, &buf, "freq=14025300")
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("run: %v", err)
	}

	got := lines(&buf.buf)
	if len(got) < 2 {
		t.Fatalf("want at least two lines:\n%s", buf.String())
	}
	if !strings.Contains(got[0], "freq=14025000") {
		t.Errorf("the first line did not come from the REST fetch: %s", got[0])
	}
	// The websocket snapshot repeating what REST already said must not produce
	// a second, identical line.
	for _, l := range got[1:] {
		if strings.Contains(l, "freq=14025000") && strings.Contains(l, "status") {
			t.Errorf("the initial state was rendered twice:\n%s", buf.String())
		}
	}
}

// resync means the server dropped events for this client. What was missed is
// unknowable from here, so the state is refetched rather than guessed at.
func TestMonitorResyncTriggersARefetch(t *testing.T) {
	d := newDaemon(t, &daemon{
		stateBodies: []string{stateBody(14025000, 4471), stateBody(21050000, 5000)},
		script: func(conn *websocket.Conn, _ int, done <-chan struct{}) {
			_ = send(conn, `{"type":"hello","version":"0.1.0","radios":["ic7610"]}`)
			_ = send(conn, `{"type":"resync","radio":"ic7610"}`)
			<-done
		},
	})

	var buf syncBuffer
	m := newTestMonitor(d.client(t, daemonPass), newPlainRenderer(&buf, time.Now, time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- m.run(ctx) }()

	waitFor(t, &buf, "freq=21050000")
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("run: %v", err)
	}

	if state, _, _ := d.counts(); state != 2 {
		t.Errorf("state fetched %d times, want 2 (initial plus the resync refetch)", state)
	}
	if !strings.Contains(buf.String(), "resync") {
		t.Errorf("the resync was not reported:\n%s", buf.String())
	}
}

// A dropped stream must come back without spinning, and must say that it is
// trying.
func TestMonitorReconnectsWithBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := newDaemon(t, &daemon{
		stateBodies: []string{stateBody(14025000, 4471)},
		script: func(conn *websocket.Conn, n int, _ <-chan struct{}) {
			_ = send(conn, `{"type":"hello","version":"0.1.0","radios":["ic7610"]}`)
			if n >= 3 {
				cancel()
			}
			conn.Close(websocket.StatusGoingAway, "bye")
		},
	})

	var buf syncBuffer
	m := newTestMonitor(d.client(t, daemonPass), newPlainRenderer(&buf, time.Now, time.Hour))

	// The schedule is asserted, not waited for.
	var waits []time.Duration
	m.bo.rnd = func() float64 { return 0 }
	m.sleep = func(dur time.Duration) <-chan time.Time {
		waits = append(waits, dur)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	if err := m.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	if _, _, dials := d.counts(); dials < 3 {
		t.Errorf("dialled %d times, want at least 3", dials)
	}
	if len(waits) < 2 {
		t.Fatalf("waited %d times, want at least 2: %v", len(waits), waits)
	}
	// A stream that opened resets the schedule, so every wait here is the
	// floor: the point is that there is a wait at all, and that it is not zero.
	for i, w := range waits {
		if w <= 0 || w > backoffMax {
			t.Errorf("wait %d = %v, outside (0, %v]", i, w, backoffMax)
		}
	}
	if !strings.Contains(buf.String(), "stream state=reconnecting") {
		t.Errorf("reconnecting was not reported:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "attempt 1") {
		t.Errorf("the attempt count is missing:\n%s", buf.String())
	}
}

// A stream that never opens must back off too, and the delay must grow: this
// is the daemon-restarting case, where spinning would be worst.
func TestMonitorBacksOffWhenTheStreamNeverOpens(t *testing.T) {
	d := newDaemon(t, &daemon{
		stateBodies: []string{stateBody(14025000, 4471)},
		wsStatus:    http.StatusServiceUnavailable,
	})

	var buf syncBuffer
	m := newTestMonitor(d.client(t, daemonPass), newPlainRenderer(&buf, time.Now, time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var waits []time.Duration
	m.bo.rnd = func() float64 { return 0 }
	m.sleep = func(dur time.Duration) <-chan time.Time {
		waits = append(waits, dur)
		if len(waits) >= 4 {
			cancel()
		}
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	if err := m.run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(waits) < 4 {
		t.Fatalf("waited %d times: %v", len(waits), waits)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}
	for i, w := range want {
		if waits[i] != w {
			t.Errorf("wait %d = %v, want %v", i, waits[i], w)
		}
	}
	// The REST fetch already put the radio on screen, so a stream that will not
	// open leaves a useful display rather than a blank one.
	if !strings.Contains(buf.String(), "freq=14025000") {
		t.Errorf("no state reached the display:\n%s", buf.String())
	}
}

// A rejected password must be reported, not retried forever on a backoff loop.
func TestMonitorReportsAuthFailureRatherThanRetrying(t *testing.T) {
	d := newDaemon(t, &daemon{stateBodies: []string{stateBody(14025000, 4471)}})

	var buf syncBuffer
	m := newTestMonitor(d.client(t, "wrong"), newPlainRenderer(&buf, time.Now, time.Hour))

	done := make(chan error, 1)
	go func() { done <- m.run(context.Background()) }()

	select {
	case err := <-done:
		if !client.IsUnauthorized(err) {
			t.Fatalf("err = %v, want a 401", err)
		}
		msg := describe(err, "http://example/api/v1", daemonUser, "ic7610").Error()
		if !strings.Contains(msg, "authentication failed") {
			t.Errorf("unhelpful message: %s", msg)
		}
		if !strings.Contains(msg, "bcrypt") {
			t.Errorf("the message does not explain why the config is no help: %s", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a rejected password hung instead of being reported")
	}

	if _, _, dials := d.counts(); dials != 0 {
		t.Errorf("dialled the stream %d times with bad credentials", dials)
	}
}

// The same has to hold when only the upgrade is refused: a 401 on the stream is
// still a 401, and the reconnect loop must not swallow it.
func TestMonitorDoesNotRetryARefusedUpgrade(t *testing.T) {
	d := newDaemon(t, &daemon{
		stateBodies: []string{stateBody(14025000, 4471)},
		wsStatus:    http.StatusUnauthorized,
	})

	var buf syncBuffer
	m := newTestMonitor(d.client(t, daemonPass), newPlainRenderer(&buf, time.Now, time.Hour))
	m.sleep = func(time.Duration) <-chan time.Time {
		t.Error("a refused upgrade must not be retried")
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	err := m.run(context.Background())
	if !client.IsUnauthorized(err) {
		t.Fatalf("err = %v, want a 401", err)
	}
	if _, _, dials := d.counts(); dials != 1 {
		t.Errorf("dialled %d times, want exactly 1", dials)
	}
}

// -once is for scripting: fetch, print, exit, and never open a stream.
func TestMonitorOnce(t *testing.T) {
	d := newDaemon(t, &daemon{stateBodies: []string{stateBody(14025000, 4471)}})

	var buf syncBuffer
	m := newTestMonitor(d.client(t, daemonPass), newPlainRenderer(&buf, time.Now, time.Hour))
	m.once = true

	if err := m.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := len(lines(&buf.buf)); got != 1 {
		t.Errorf("-once printed %d lines:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "freq=14025000") {
		t.Errorf("-once printed no state:\n%s", buf.String())
	}
	if _, _, dials := d.counts(); dials != 0 {
		t.Errorf("-once opened %d streams", dials)
	}
}

// A radio that is unplugged is a state to display, not an error to raise.
func TestMonitorRendersADisconnectedRadio(t *testing.T) {
	d := newDaemon(t, &daemon{
		stateBodies: []string{`{"frequency":14025000,"mode":"CW",
			"power":{"pct":0,"native":0,"watts":null},
			"s_meter":{"raw":0,"scale":255},"cw":{"busy":false,"queued":0,"wpm":25},
			"connected":false,"seq":9,"updated_at":"2026-08-04T20:11:04Z",
			"age_ms":134000,"stale":true}`},
	})

	var buf syncBuffer
	m := newTestMonitor(d.client(t, daemonPass), newPlainRenderer(&buf, time.Now, time.Hour))
	m.once = true

	if err := m.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"connected=false", "stale=true", "freq=14025000"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}

	// And the terminal rendering of the same state says so in words.
	if got := strings.Join(frame(m.view, 80), "\n"); !strings.Contains(got, "radio disconnected") {
		t.Errorf("the frame does not explain the state:\n%s", got)
	}
}

// The stream carries every radio the connection subscribed to. Anything else is
// not this radio's business.
func TestMonitorIgnoresOtherRadios(t *testing.T) {
	d := newDaemon(t, &daemon{
		stateBodies: []string{stateBody(14025000, 4471)},
		script: func(conn *websocket.Conn, _ int, done <-chan struct{}) {
			_ = send(conn, `{"type":"hello","version":"0.1.0","radios":["ic7610"]}`)
			_ = send(conn, `{"type":"delta","radio":"ts590sg","seq":99,`+
				`"changed":{"frequency":7005000}}`)
			_ = send(conn, `{"type":"delta","radio":"ic7610","seq":4472,`+
				`"changed":{"frequency":14025300}}`)
			<-done
		},
	})

	var buf syncBuffer
	m := newTestMonitor(d.client(t, daemonPass), newPlainRenderer(&buf, time.Now, time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- m.run(ctx) }()

	waitFor(t, &buf, "freq=14025300")
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(buf.String(), "7005000") {
		t.Errorf("another radio's frequency reached the display:\n%s", buf.String())
	}
}
