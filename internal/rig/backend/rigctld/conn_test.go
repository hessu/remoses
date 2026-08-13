package rigctld

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// errNAK stands in for the session's own ErrNAK. It cannot be imported —
// internal/rig imports backend, and a backend importing it back would be a
// cycle — so the shape is reproduced instead.
var errNAK = errors.New("rejected")

// testConn stands in for the session's transaction layer. Requests are scripted
// to whole response blocks, blocks go through the real Decode, and every
// request is recorded so a test can assert the exact wire conversation.
//
// It mirrors conn.Do in the one respect this backend depends on: a matched
// rejection comes back as the decoded Update AND an error, because that is how
// the session reports it, and it is what lets do() recover the Hamlib code from
// Update.Raw.
type testConn struct {
	t *testing.T
	g *Rig

	// answers maps a request, newline included, to the response block rigctld
	// would write back, without its final newline. A request that is absent
	// stands for a daemon that says nothing at all.
	answers map[string]string

	// sent records every request written, in order.
	sent []string
}

func newTestConn(t *testing.T, g *Rig, answers map[string]string) *testConn {
	t.Helper()
	if answers == nil {
		answers = map[string]string{}
	}
	return &testConn{t: t, g: g, answers: answers}
}

func (c *testConn) Do(ctx context.Context, req []byte, want ...backend.Key) (backend.Update, error) {
	s := string(req)
	c.sent = append(c.sent, s)
	if len(want) == 0 {
		return backend.Update{}, fmt.Errorf("testConn: Do(%q) with no wanted keys", s)
	}

	answer, ok := c.answers[s]
	if !ok {
		// What a silent daemon looks like from inside a backend: the session's
		// per-command timeout, not a protocol error.
		return backend.Update{}, fmt.Errorf("testConn: timeout waiting for an answer to %q", s)
	}

	u, err := c.g.Decode([]byte(answer))
	if err != nil {
		return u, err
	}
	matched := false
	for _, w := range want {
		if u.Key == w {
			matched = true
			break
		}
	}
	if !matched {
		return backend.Update{}, fmt.Errorf("testConn: answer %q keyed %q, none of %v", answer, u.Key, want)
	}
	if !u.OK {
		return u, fmt.Errorf("testConn: %q: %w", s, errNAK)
	}
	return u, nil
}

func (c *testConn) Send(ctx context.Context, req []byte) error {
	c.sent = append(c.sent, string(req))
	return nil
}

// wantSent asserts the exact request sequence.
func (c *testConn) wantSent(t *testing.T, want ...string) {
	t.Helper()
	if len(c.sent) != len(want) {
		t.Fatalf("sent %q, want %q", c.sent, want)
	}
	for i := range want {
		if c.sent[i] != want[i] {
			t.Fatalf("request %d = %q, want %q (full: %q)", i, c.sent[i], want[i], c.sent)
		}
	}
}

// newRig builds a backend without going through config, so a test can state its
// intent in one line.
func newRig(t *testing.T) *Rig {
	t.Helper()
	g, err := New(&config.Radio{ID: "test", Rigctld: &config.Rigctld{Address: "127.0.0.1:4532"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

// resp joins lines into a response block the way rigctld writes one, so a test
// reads as the conversation it is describing. Split has already removed the
// final newline by the time a frame reaches Decode, so there is none here.
func resp(lines ...string) string { return strings.Join(lines, "\n") }

// patchDiff renders the difference between two patches, comparing what the
// pointers point AT rather than the pointers themselves. It returns "" when
// they agree.
func patchDiff(got, want radio.Patch) string {
	var diffs []string
	cmp := func(name string, g, w any) {
		gs, ws := showPtr(g), showPtr(w)
		if gs != ws {
			diffs = append(diffs, fmt.Sprintf("%s: got %s, want %s", name, gs, ws))
		}
	}
	cmp("Frequency", got.Frequency, want.Frequency)
	cmp("Mode", got.Mode, want.Mode)
	cmp("DataMode", got.DataMode, want.DataMode)
	cmp("PassbandHz", got.PassbandHz, want.PassbandHz)
	cmp("FilterSlot", got.FilterSlot, want.FilterSlot)
	cmp("Power", got.Power, want.Power)
	cmp("PTT", got.PTT, want.PTT)
	cmp("SMeter", got.SMeter, want.SMeter)
	cmp("PowerMeter", got.PowerMeter, want.PowerMeter)
	cmp("SWR", got.SWR, want.SWR)
	cmp("ALC", got.ALC, want.ALC)
	cmp("SWRRatio", got.SWRRatio, want.SWRRatio)
	cmp("CW", got.CW, want.CW)
	cmp("Connected", got.Connected, want.Connected)
	return strings.Join(diffs, "; ")
}

// showPtr renders a possibly-nil pointer field, dereferencing one more level for
// the struct fields (Power, Meter) so that a differing S-meter reports its
// contents rather than two addresses.
func showPtr(v any) string {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.IsNil() {
		return "nil"
	}
	elem := rv.Elem().Interface()
	switch e := elem.(type) {
	case radio.Power:
		w := "nil"
		if e.Watts != nil {
			w = fmt.Sprintf("%g", *e.Watts)
		}
		return fmt.Sprintf("{watts:%s pct:%g native:%d}", w, e.Pct, e.Native)
	case radio.Meter:
		s := "nil"
		if e.S != nil {
			s = fmt.Sprintf("%g", *e.S)
		}
		return fmt.Sprintf("{raw:%d scale:%d s:%s}", e.Raw, e.Scale, s)
	default:
		return fmt.Sprintf("%v", elem)
	}
}
