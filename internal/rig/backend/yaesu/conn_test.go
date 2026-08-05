package yaesu

import (
	"context"
	"fmt"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/rig/backend"
)

// testConn stands in for the session's transaction layer. It is deliberately
// thin: requests are scripted to answers, answers go through the real Decode,
// and every request is recorded so a test can assert the exact wire
// conversation.
//
// A request with no scripted answer produces the error a silent rig produces —
// a timeout — which is the only failure mode this protocol has, since no Yaesu
// documents a rejection response.
type testConn struct {
	t *testing.T
	y *Rig

	// answers maps a request, terminator included, to the answer frame the rig
	// would send back, without its terminator.
	answers map[string]string

	// sent records every request written, Do and Send alike, in order.
	sent []string
}

func newTestConn(t *testing.T, y *Rig, answers map[string]string) *testConn {
	t.Helper()
	if answers == nil {
		answers = map[string]string{}
	}
	return &testConn{t: t, y: y, answers: answers}
}

func (c *testConn) Do(ctx context.Context, req []byte, want ...backend.Key) (backend.Update, error) {
	s := string(req)
	c.sent = append(c.sent, s)
	if len(want) == 0 {
		return backend.Update{}, fmt.Errorf("testConn: Do(%q) with no wanted keys", s)
	}

	answer, ok := c.answers[s]
	if !ok {
		return backend.Update{}, fmt.Errorf("testConn: timeout waiting for an answer to %q", s)
	}

	u, err := c.y.Decode([]byte(answer))
	if err != nil {
		return u, err
	}
	for _, w := range want {
		if u.Key == w {
			return u, nil
		}
	}
	return backend.Update{}, fmt.Errorf("testConn: answer %q keyed %q, none of %v", answer, u.Key, want)
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

// modelNamed resolves a profile for a test, where an unknown name is a typo in
// the test rather than a condition to handle.
func modelNamed(t *testing.T, name string) Model {
	t.Helper()
	m, err := LookupModel(name)
	if err != nil {
		t.Fatalf("LookupModel(%q): %v", name, err)
	}
	return m
}

// newModelRig builds a backend for a named model with the defaults config would
// have supplied.
func newModelRig(t *testing.T, name string) *Rig {
	t.Helper()
	y, err := New(&config.Radio{
		ID:      "rig",
		Backend: Name,
		Yaesu:   &config.Yaesu{Model: name, AutoInformation: true},
	})
	if err != nil {
		t.Fatalf("New(%q): %v", name, err)
	}
	return y
}

// allModels is every profile in the registry, so a table-driven test can assert
// something of all of them without restating the list.
func allModels(t *testing.T) []Model {
	t.Helper()
	out := make([]Model, 0, len(models))
	for _, n := range ModelNames() {
		out = append(out, modelNamed(t, n))
	}
	return out
}

// answersFor is a rig of any model sitting on 14.025 MHz in CW, half power,
// receiving, with a 500 Hz filter and narrow off.
//
// It is built from the profile rather than written out because the models
// genuinely disagree about what a well-formed answer looks like: the frequency
// field is eight or nine digits, the IF answer three lengths, PC watts or a
// 0-255 index, and the FTdx9000 answers neither ID nor NA at all.
func answersFor(m Model) map[string]string {
	a := map[string]string{
		reqFA: "FA" + mustFreq(m, 14_025_000),
		reqMD: "MD03",
		reqTX: "TX0",
		reqSM: "SM0123",
		reqIF: sampleIFFor(m),
		reqSH: shAnswer(m, 10, false),
	}
	if m.HasID {
		// generic asks but claims no number of its own, so it gets one no
		// profile claims — which is exactly what an unprofiled Yaesu sends.
		id := 999
		if len(m.IDs) > 0 {
			id = m.IDs[0]
		}
		a[reqID] = fmt.Sprintf("ID%04d", id)
	}
	if m.HasNarrow {
		a[reqNA] = "NA00"
	}
	switch {
	// The FTX-1 is the only model with a head selector in PC.
	case m.PowerHead:
		a[reqPC] = "PC2050"
	// The FTdx5000 and FTdx9000 answer an index, not watts; 128 is about half
	// of 255.
	case m.PowerRaw:
		a[reqPC] = "PC128"
	default:
		a[reqPC] = "PC050"
	}
	return a
}

// sampleIFFor is the IF answer this model's generation sends, which is the one
// place three different layouts have to be produced rather than just parsed.
func sampleIFFor(m Model) string {
	switch {
	case m.PowerHead: // the FTX-1, whose memory channel is five characters
		return sampleIFLong
	case m.FreqDigits == freqDigitsOld:
		return sampleIFOlder
	}
	return sampleIF
}

// mustFreq renders a frequency the way this model's FA/FB field takes it.
func mustFreq(m Model, hz uint64) string {
	s, err := formatFrequency(hz, m.FreqDigits)
	if err != nil {
		panic(err)
	}
	return s
}

// shAnswer builds the SH answer this model would send, which is one character
// shorter on the FT-991A and the whole FT-950 generation than on the newer
// radios.
func shAnswer(m Model, index int, narrow bool) string {
	if m.Filter == FilterShort {
		return fmt.Sprintf("SH0%02d", index)
	}
	n := 0
	if narrow && m.Filter == FilterNarrowFlag {
		n = 1
	}
	return fmt.Sprintf("SH0%d%02d", n, index)
}
