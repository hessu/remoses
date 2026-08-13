package ws

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/wire"
)

// The frames this package writes are described in api/openapi.yaml as a oneOf
// discriminated by `type`, and internal/wire is the Go that document generates.
// These tests push real frames — from a real hub, over a real socket — through
// those types with unknown members refused.
//
// It is the check the REST side gets from internal/api's conformance tests, and
// the stream needs it more rather than less: a WebSocket has no schema anybody
// can validate against at runtime, so if the document and the writer drift
// apart, nothing but this notices.

// requiredMembers are the members each frame type must carry.
//
// hello is the one frame with no radio and no seq, because it describes the
// connection rather than a radio. Every other frame has both, which is what
// lets a client place a message in the stream without correlating it against
// something else it received.
var requiredMembers = map[string][]string{
	"hello":  {"type", "version", "radios", "server_time"},
	"state":  {"type", "radio", "seq", "ts", "state"},
	"delta":  {"type", "radio", "seq", "ts", "changed"},
	"cw":     {"type", "radio", "seq", "ts", "busy", "queued", "wpm"},
	"conn":   {"type", "radio", "seq", "ts", "connected"},
	"resync": {"type", "radio", "seq", "ts"},
}

// checkFrame validates one frame and reports which kind it was.
func checkFrame(t *testing.T, raw []byte) string {
	t.Helper()

	var msg wire.WSMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("frame is not a WSMessage: %v (%s)", err, raw)
	}
	kind, err := msg.Discriminator()
	if err != nil {
		t.Fatalf("frame carries no type: %v (%s)", err, raw)
	}

	// Decoding through the union's own accessor, so a frame that the document
	// declares and this package spells differently fails on the way in.
	var target any
	switch kind {
	case "hello":
		target = &wire.WSHello{}
	case "state":
		target = &wire.WSState{}
	case "delta":
		target = &wire.WSDelta{}
	case "cw":
		target = &wire.WSCW{}
	case "conn":
		target = &wire.WSConn{}
	case "resync":
		target = &wire.WSResync{}
	default:
		t.Fatalf("frame type %q is not one of WSMessage's: %s", kind, raw)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		t.Errorf("decoding a %s frame into %T: %v (%s)", kind, target, err, raw)
	}

	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("frame is not an object: %v", err)
	}
	for _, name := range requiredMembers[kind] {
		if _, ok := members[name]; !ok {
			t.Errorf("%s frame is missing the required member %q: %s", kind, name, raw)
		}
	}
	return kind
}

// TestEveryFrameMatchesTheContract provokes each kind of frame out of a live
// hub and validates all of them, on one connection, because the frames a client
// has to cope with are the ones that arrive interleaved rather than one per
// test.
func TestEveryFrameMatchesTheContract(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{poll: 10 * time.Millisecond})
	c := h.dialBasic("/ws-authed")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := readFrames(ctx, c)
	seen := map[string]bool{}

	// hello and the opening snapshot arrive by themselves.
	await(t, frames, seen, "hello", nil)
	await(t, frames, seen, "state", nil)

	// Turning the knob is provoked repeatedly rather than once, because the
	// state lane is last-value-wins: a CW status change arriving in the same
	// window supersedes a frequency change and the client is sent the whole
	// snapshot instead. Another turn of the knob produces another delta.
	await(t, frames, seen, "delta", func(i int) {
		h.devs["ic7610"].tune(uint64(14_030_000 + i*100))
	})

	// A queue that fills becomes a discrete cw frame.
	h.cws["ic7610"].churn.Store(true)
	await(t, frames, seen, "cw", nil)
	h.cws["ic7610"].churn.Store(false)

	// And a port that goes away becomes a conn frame.
	await(t, frames, seen, "conn", func(int) { h.devs["ic7610"].drop() })
}

// readFrames turns the blocking reader into a channel.
//
// A test cannot simply read with a short deadline instead: cancelling a read
// closes the connection under coder/websocket, so a poll that timed out once
// would take the stream with it.
func readFrames(ctx context.Context, c *websocket.Conn) <-chan []byte {
	out := make(chan []byte, 64)
	go func() {
		defer close(out)
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			select {
			case out <- data:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// await validates frames as they arrive until one of the wanted kind has been
// seen, calling provoke every so often to cause it.
func await(t *testing.T, frames <-chan []byte, seen map[string]bool, want string, provoke func(int)) {
	t.Helper()
	if seen[want] {
		return
	}
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for i := 0; ; i++ {
		select {
		case raw, ok := <-frames:
			if !ok {
				t.Fatalf("stream ended while waiting for a %s frame; saw %v", want, seen)
			}
			if checkFrame(t, raw) == want {
				seen[want] = true
				return
			}
		case <-tick.C:
			if provoke != nil {
				provoke(i)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s frame; saw %v", want, seen)
		}
	}
}

// resync needs a client that has fallen behind, which needs its own hub: the
// event queue has to be small enough to overflow.
func TestResyncFrameMatchesTheContract(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{
		ws:         config.WS{MinInterval: config.Duration(50 * time.Millisecond)},
		eventQueue: 1, // guarantees the session's fan-out drops under the flood
	})
	c := h.dialBasic("/ws-authed")

	readRaw(t, c) // hello
	readRaw(t, c) // snapshot

	// Flood the session while this client is not reading, so the fan-out drops
	// events and the server has to admit the hole rather than wedge or hang up.
	for i := 1; i <= 2000; i++ {
		h.devs["ic7610"].tune(uint64(14_000_000 + i))
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if checkFrame(t, readRaw(t, c)) == "resync" {
			return
		}
	}
	t.Fatal("no resync frame arrived")
}

// readRaw reads one frame as it went over the wire. The bytes matter here:
// decoding to a map first would throw away the difference between a member that
// is absent and one that is null, which is exactly what the contract uses to
// say a transmit meter has gone away.
func readRaw(t *testing.T, c *websocket.Conn) []byte {
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
	return data
}
