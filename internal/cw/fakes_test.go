package cw

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hessu/remoses/internal/rig/backend"
)

// The fakes below stand in for the civ, kenwood and serial packages so that
// this package's tests exercise the pacing loops and the keyer without any
// backend, any port, or any radio.

const (
	// icomCharset and kenwoodCharset are what the two target rigs accept; both
	// exclude ';' , which is why a charset rejection has something to reject.
	icomCharset    = `0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ '()/=?+."-@,: `
	kenwoodCharset = `ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 '"()*+,-./:=?@`
)

// kenwoodProsigns is the single-symbol substitution from §5.3.
var kenwoodProsigns = map[string]rune{
	"AR": '_', "BT": '[', "SK": '>', "KN": ']',
	"AS": '<', "BK": '\\', "HH": '#', "SN": '%',
}

func encodeKenwood(text string) (string, error) {
	var b strings.Builder
	rs := []rune(text)
	for i := 0; i < len(rs); {
		if rs[i] != '^' {
			b.WriteRune(rs[i])
			i++
			continue
		}
		j := i + 1
		for j < len(rs) && isLetter(rs[j]) {
			j++
		}
		name := strings.ToUpper(string(rs[i+1 : j]))
		sym, ok := kenwoodProsigns[name]
		if !ok {
			return "", fmt.Errorf("fake: no Kenwood symbol for prosign %q", name)
		}
		b.WriteRune(sym)
		i = j
	}
	return b.String(), nil
}

// encodeIcom is the Icom dialect: '^' is the rig's own run-together marker, so
// canonical text passes straight through.
func encodeIcom(text string) (string, error) { return text, nil }

type sent struct {
	text string
	at   time.Time
}

// fakeMorse is a backend.MorseSender whose buffer the test drives by hand.
type fakeMorse struct {
	maxChunk int
	charset  string
	encode   func(string) (string, error)
	// freeOK false models a rig that cannot be asked how full it is, which
	// forces the open loop.
	freeOK bool

	mu      sync.Mutex
	free    int
	sent    []sent
	polls   int
	aborts  int
	speed   int
	sendErr error
	// overrun records the failure the closed loop exists to prevent.
	overrun string
}

func (f *fakeMorse) MaxChunk() int   { return f.maxChunk }
func (f *fakeMorse) Charset() string { return f.charset }

func (f *fakeMorse) EncodeProsigns(text string) (string, error) {
	if f.encode == nil {
		return text, nil
	}
	return f.encode(text)
}

func (f *fakeMorse) BufferFree(ctx context.Context, c backend.Conn) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if !f.freeOK {
		return 0, false, nil
	}
	return f.free, true, nil
}

func (f *fakeMorse) SendChunk(ctx context.Context, c backend.Conn, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	if f.freeOK && len(text) > f.free {
		f.overrun = fmt.Sprintf("wrote %d characters to a buffer with room for %d", len(text), f.free)
	}
	if len(text) > f.maxChunk {
		f.overrun = fmt.Sprintf("chunk of %d exceeds MaxChunk %d", len(text), f.maxChunk)
	}
	f.free -= len(text)
	if f.free < 0 {
		f.free = 0
	}
	f.sent = append(f.sent, sent{text: text, at: time.Now()})
	return nil
}

func (f *fakeMorse) Abort(ctx context.Context, c backend.Conn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborts++
	f.free = f.maxChunk
	return nil
}

func (f *fakeMorse) SetSpeed(ctx context.Context, c backend.Conn, wpm int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.speed = wpm
	return nil
}

func (f *fakeMorse) setFree(n int) {
	f.mu.Lock()
	f.free = n
	f.mu.Unlock()
}

func (f *fakeMorse) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeMorse) sentChunks() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.sent...)
}

func (f *fakeMorse) sentText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, s := range f.sent {
		b.WriteString(s.text)
	}
	return b.String()
}

func (f *fakeMorse) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

func (f *fakeMorse) abortCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aborts
}

// fakeConn satisfies backend.Conn. The pacing loop only ever passes it through
// to the MorseSender, so it needs no behaviour.
type fakeConn struct{}

func (fakeConn) Do(ctx context.Context, req []byte, want ...backend.Key) (backend.Update, error) {
	return backend.Update{OK: true}, nil
}

func (fakeConn) Send(ctx context.Context, req []byte) error { return nil }

// edge is one control line transition, with the instant it happened. The keyer
// tests are entirely about the intervals between these.
type edge struct {
	line string
	on   bool
	at   time.Time
}

// fakeLines is a transport.ControlLines that records edges.
type fakeLines struct {
	mu    sync.Mutex
	state map[string]bool
	all   []edge
	err   error
}

func newFakeLines() *fakeLines {
	return &fakeLines{state: map[string]bool{"DTR": false, "RTS": false}}
}

func (f *fakeLines) SetDTR(on bool) error { return f.set("DTR", on) }
func (f *fakeLines) SetRTS(on bool) error { return f.set("RTS", on) }

func (f *fakeLines) set(name string, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	// Only transitions are recorded: the keyer drops a line it believes is
	// already low whenever it tidies up, and those are not edges.
	if f.state[name] == on {
		return nil
	}
	f.state[name] = on
	f.all = append(f.all, edge{line: name, on: on, at: time.Now()})
	return nil
}

func (f *fakeLines) edges() []edge {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]edge(nil), f.all...)
}

func (f *fakeLines) edgesOn(name string) []edge {
	var out []edge
	for _, e := range f.edges() {
		if e.line == name {
			out = append(out, e)
		}
	}
	return out
}

func (f *fakeLines) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.all)
}

func (f *fakeLines) asserted(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[name]
}
