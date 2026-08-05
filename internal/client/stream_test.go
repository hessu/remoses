package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/hessu/remoses/internal/radio"
)

// newWSServer runs a real websocket server, not a fake, so that the handshake,
// the framing and the close path are the ones the daemon uses.
//
// script writes whatever the test needs and then blocks on done, which is
// closed before the server is shut down: a hijacked connection is not something
// httptest will tear down for us.
func newWSServer(t *testing.T, script func(conn *websocket.Conn, done <-chan struct{})) (*httptest.Server, *[]url.Values) {
	t.Helper()

	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	var (
		mu      sync.Mutex
		queries []url.Values
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != testUser || pass != testPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="remoses"`)
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,` +
				`"detail":"HTTP Basic credentials or a ticket from /ws-ticket are required"}`))
			return
		}
		if r.URL.Path != testBase+"/ws" {
			http.NotFound(w, r)
			return
		}

		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		script(conn, done)
	}))

	t.Cleanup(func() {
		stop()
		srv.Close()
	})
	return srv, &queries
}

func send(conn *websocket.Conn, msg string) error {
	return conn.Write(context.Background(), websocket.MessageText, []byte(msg))
}

func TestStreamDeliversTheDocumentedEnvelopes(t *testing.T) {
	srv, queries := newWSServer(t, func(conn *websocket.Conn, done <-chan struct{}) {
		msgs := []string{
			`{"type":"hello","version":"0.1.0","radios":["ic7610"],"server_time":"2026-08-04T20:11:04Z"}`,
			`{"type":"state","radio":"ic7610","seq":4471,"ts":"2026-08-04T20:11:04Z","state":` + stateJSON + `}`,
			`{"type":"delta","radio":"ic7610","seq":4472,"ts":"2026-08-04T20:11:05Z",` +
				`"changed":{"frequency":14025300,"s_meter":{"raw":120,"scale":255,"s":7.0}}}`,
			// A type this client does not know must be ignored rather than
			// ending the stream; the contract says so explicitly.
			`{"type":"something-new","radio":"ic7610"}`,
			`{"type":"cw","radio":"ic7610","busy":true,"queued":12,"wpm":28}`,
			`{"type":"conn","radio":"ic7610","connected":false,"error":"port closed"}`,
			`{"type":"resync","radio":"ic7610"}`,
		}
		for _, m := range msgs {
			if err := send(conn, m); err != nil {
				return
			}
		}
		<-done
	})

	c := newClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := c.Stream(ctx, "ic7610")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()

	next := func() Event {
		t.Helper()
		ev, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		return ev
	}

	hello := next()
	if hello.Kind != EventHello || hello.Version != "0.1.0" {
		t.Fatalf("hello = %+v", hello)
	}
	if len(hello.Radios) != 1 || hello.Radios[0] != "ic7610" {
		t.Errorf("hello radios = %v", hello.Radios)
	}

	snap := next()
	if snap.Kind != EventState || snap.Seq != 4471 {
		t.Fatalf("state = %+v", snap)
	}
	if snap.State.Frequency != 14025000 || snap.State.Mode != radio.ModeCW {
		t.Errorf("snapshot = %+v", snap.State)
	}

	delta := next()
	if delta.Kind != EventDelta || delta.Seq != 4472 {
		t.Fatalf("delta = %+v", delta)
	}
	if got, want := strings.Join(delta.Fields, ","), "frequency,s_meter"; got != want {
		t.Errorf("delta fields = %q, want %q", got, want)
	}

	cw := next()
	if cw.Kind != EventCW || !cw.CW.Busy || cw.CW.Queued != 12 || cw.CW.WPM != 28 {
		t.Fatalf("cw = %+v", cw)
	}

	conn := next()
	if conn.Kind != EventConn || conn.Connected || conn.Err != "port closed" {
		t.Fatalf("conn = %+v", conn)
	}

	resync := next()
	if resync.Kind != EventResync || resync.Radio != "ic7610" {
		t.Fatalf("resync = %+v", resync)
	}

	if len(*queries) != 1 || (*queries)[0].Get("radios") != "ic7610" {
		t.Errorf("subscription query = %v, want radios=ic7610", *queries)
	}
	// The ticket flow exists for browsers, which cannot set headers on a
	// handshake. A programmatic client has no such problem and must not spend a
	// round trip working around it.
	if (*queries)[0].Get("ticket") != "" {
		t.Error("client used the browser ticket flow")
	}
}

func TestStreamAuthFailureIsReportedNotHung(t *testing.T) {
	srv, _ := newWSServer(t, func(conn *websocket.Conn, done <-chan struct{}) { <-done })

	c, err := New(srv.URL+testBase, testUser, "wrong")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	type result struct {
		s   *Stream
		err error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := c.Stream(context.Background(), "ic7610")
		ch <- result{s, err}
	}()

	select {
	case r := <-ch:
		if r.err == nil {
			r.s.Close()
			t.Fatal("expected the upgrade to be refused")
		}
		if !IsUnauthorized(r.err) {
			t.Fatalf("IsUnauthorized = false for %v", r.err)
		}
		if !Fatal(r.err) {
			t.Error("a rejected password must not be retried on a backoff loop")
		}
		if !strings.Contains(r.err.Error(), "Basic credentials") {
			t.Errorf("error does not explain itself: %v", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a refused upgrade hung instead of returning an error")
	}
}

func TestStreamEndsWhenTheServerHangsUp(t *testing.T) {
	srv, _ := newWSServer(t, func(conn *websocket.Conn, _ <-chan struct{}) {
		_ = send(conn, `{"type":"hello","version":"0.1.0","radios":["ic7610"]}`)
		conn.Close(websocket.StatusGoingAway, "server shutting down")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := newClient(t, srv).Stream(ctx, "ic7610")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()

	if _, err := s.Next(ctx); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if _, err := s.Next(ctx); err == nil {
		t.Fatal("expected an error once the server hung up")
	}
}

func TestApplyDelta(t *testing.T) {
	base := radio.State{
		Frequency:  14025000,
		Mode:       radio.ModeCW,
		PassbandHz: 500,
		SMeter:     radio.Meter{Raw: 10, Scale: 255},
		CW:         radio.CWStatus{WPM: 28},
		Connected:  true,
		Seq:        10,
	}

	ev, ok, err := decodeEvent([]byte(`{"type":"delta","radio":"ic7610","seq":11,` +
		`"ts":"2026-08-04T20:11:05Z","changed":{"frequency":14025300,"mode":"USB",` +
		`"s_meter":{"raw":120,"scale":255},"cw":{"busy":true,"queued":4,"wpm":30}}}`))
	if err != nil || !ok {
		t.Fatalf("decodeEvent: ok=%t err=%v", ok, err)
	}

	next, err := ev.ApplyDelta(base)
	if err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if next.Frequency != 14025300 || next.Mode != radio.ModeUSB {
		t.Errorf("changed fields not applied: %+v", next)
	}
	if next.SMeter.Raw != 120 || !next.CW.Busy || next.CW.Queued != 4 {
		t.Errorf("nested fields not applied: %+v", next)
	}
	// Untouched fields survive: a delta names only what moved, so anything it
	// does not mention has to be carried over from the snapshot.
	if next.PassbandHz != 500 || !next.Connected {
		t.Errorf("untouched fields lost: %+v", next)
	}
	if next.Seq != 11 {
		t.Errorf("seq = %d, want 11", next.Seq)
	}
	if next.UpdatedAt.IsZero() {
		t.Error("updated_at was not taken from the delta's ts")
	}
}

// The previous snapshot is what a renderer diffs against, so applying a delta
// must not reach back through the pointers it shares with it.
func TestApplyDeltaDoesNotMutateThePreviousSnapshot(t *testing.T) {
	swr := radio.Meter{Raw: 3, Scale: 100}
	base := radio.State{Frequency: 14025000, SWR: &swr}

	ev, _, err := decodeEvent([]byte(`{"type":"delta","radio":"ic7610","seq":2,` +
		`"changed":{"swr":{"raw":30,"scale":100}}}`))
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}

	next, err := ev.ApplyDelta(base)
	if err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if next.SWR.Raw != 30 {
		t.Errorf("delta not applied: %+v", next.SWR)
	}
	if base.SWR.Raw != 3 {
		t.Errorf("previous snapshot was mutated: %+v", base.SWR)
	}
}

func TestDecodeEventIgnoresUnknownTypes(t *testing.T) {
	_, ok, err := decodeEvent([]byte(`{"type":"lock","radio":"ic7610","held":true}`))
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	if ok {
		t.Error("an unrecognised type should be skipped, not surfaced")
	}
}

func TestEventKindStrings(t *testing.T) {
	for kind, want := range map[EventKind]string{
		EventHello: "hello", EventState: "state", EventDelta: "delta",
		EventCW: "cw", EventConn: "conn", EventResync: "resync",
	} {
		if got := kind.String(); got != want {
			t.Errorf("EventKind(%d) = %q, want %q", kind, got, want)
		}
	}
}
