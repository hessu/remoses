package yaesu

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/rig/backend"
)

// testCmdTimeout stands in for the session's per-command timeout, which is a
// second in production. It is what a transaction costs when nothing on its wait
// list ever arrives, and avoiding that cost is the entire point of the busy
// key — so the fake charges it, shortened so a test that fails to fast-fail
// still finishes.
const testCmdTimeout = 300 * time.Millisecond

// errFakeNAK is what the session makes of an Update whose OK is false: it
// reports rig.ErrNAK, a permanent rejection that the API maps to 422. The fake
// reproduces that rather than importing internal/rig, which a backend may not
// do, so a test can prove that a busy answer does NOT reach the caller as one.
var errFakeNAK = errors.New("testConn: rejected (what the session reports as rig.ErrNAK)")

// testConn stands in for the session's transaction layer. It is deliberately
// thin: requests are scripted to answers, answers go through the real Decode,
// and every request is recorded so a test can assert the exact wire
// conversation.
//
// A request with no scripted answer produces the error a silent rig produces —
// a timeout — which is what an unimplemented or malformed command costs on this
// protocol, since no Yaesu manual documents a rejection response.
type testConn struct {
	t *testing.T
	y *Rig

	// answers maps a request, terminator included, to the answer frame the rig
	// would send back, without its terminator.
	answers map[string]string

	// busy makes the rig answer '?' to everything, the way a real one does when
	// it will not run a command just now. See newBusyConn.
	busy bool

	// sent records every request written, Do and Send alike, in order.
	sent []string
}

func newTestConn(t *testing.T, y *Rig, answers map[string]string) *testConn {
	t.Helper()
	if answers == nil {
		answers = map[string]string{}
	}
	// The receive front end answers by default, so a test about something else
	// need not know that the slow poll reads four more commands. Every one of
	// them echoes the fixed 0 that selects the main receiver, which is what the
	// decoders index past.
	//
	// GT04 is worth noticing: 4 is AUTO-FAST as a READING, and there is no
	// reading that means plain "auto" — see agcReading.
	for req, answer := range map[string]string{
		reqPA: "PA01", reqRA: "RA01", reqRG: "RG0020", reqGT: "GT04",
	} {
		if _, ok := answers[req]; !ok {
			answers[req] = answer
		}
	}
	// The noise processing, the notches and the antenna cannot be written out
	// flat: the FTX-1 has no NB or NR command at all, the FTdx9000's BP is one
	// parameter rather than three, NL counts to 10, 100 or 255 depending on the
	// radio, and the antenna answer has three shapes. See noiseAnswers.
	for req, answer := range noiseAnswers(y.profile) {
		if _, ok := answers[req]; !ok {
			answers[req] = answer
		}
	}
	// The transmit audio chain answers by default too, and it cannot be written
	// out flat the way the front end is: MG and PL are counted against a full
	// scale that differs by model, and PR answers with one parameter on three
	// radios and two on the rest. See txAudioAnswers.
	for req, answer := range txAudioAnswers(y.profile) {
		if _, ok := answers[req]; !ok {
			answers[req] = answer
		}
	}
	return &testConn{t: t, y: y, answers: answers}
}

// txAudioAnswers is a rig with its transmit gain and processor level at about
// half of whatever this model's own count happens to be, and its speech
// processor switched on.
//
// Half is written as MicGainMax/2 rather than a literal so that the answer is
// well formed on both scales: MG050 on the eight radios that count to 100 and
// MG127 on the three that count to 255. A test that wants a specific reading
// overrides it; what this provides is a rig that does not time out.
func txAudioAnswers(m Model) map[string]string {
	a := map[string]string{}
	if m.MicGain {
		a[reqMG] = fmt.Sprintf("MG%03d", m.MicGainMax/2)
	}
	if m.ProcLevel {
		a[reqPL] = fmt.Sprintf("PL%03d", m.ProcLevelMax/2)
	}
	switch m.Proc {
	case ProcSingle:
		a[reqPRSingle] = fmt.Sprintf("PR%c", m.ProcOn)
	case ProcSelect:
		// The answer echoes the parameter that names the compressor, exactly as
		// the set carries it.
		a[reqPRSelect] = fmt.Sprintf("PR%c%c", procSpeech, m.ProcOn)
	}
	return a
}

// noiseAnswers is a rig with its blanker and reducer switched on at about half
// strength, its manual notch parked in the middle of whatever range this radio
// has, its automatic notch off and antenna 1 selected.
//
// Every value is derived from the profile rather than written out, because the
// well-formed answer is genuinely different per model: NL is three digits
// counting to 010, 100 or 255; the FTX-1 has no NB and no NR row to answer at
// all; the FTdx9000's BP answers a bare BP; with three digits where everyone
// else answers BP00; and BP01; with five; and the antenna answer is AN0<n><rx>
// on one radio, AN0<n>0 on four and AN0<n> on the FTdx9000.
func noiseAnswers(m Model) map[string]string {
	a := map[string]string{}
	if m.NBCircuits > 0 {
		a[reqNB] = "NB01"
	}
	if m.NBLevelMax > 0 {
		a[reqNL] = fmt.Sprintf("NL0%03d", m.NBLevelMax/2)
	}
	if m.NRCircuits > 0 {
		a[reqNR] = "NR01"
	}
	if m.NRLevelMax > 0 {
		a[reqRL] = fmt.Sprintf("RL0%02d", m.NRLevelMax/2)
	}
	if m.Notch {
		if m.NotchShape == NotchCombined {
			a[reqBPCombined] = fmt.Sprintf("BP%03d", m.NotchFreqMax/2)
		} else {
			a[reqBPSwitch] = "BP00001"
			a[reqBPFreq] = fmt.Sprintf("BP01%03d", m.NotchFreqMax/2)
		}
	}
	if m.AutoNotch {
		a[reqBC] = "BC00"
	}
	if m.Antennas > 0 {
		switch m.AntennaShape {
		case AntennaRXSlot:
			a[reqAN] = "AN01" // no trailing parameter on the FTdx9000
		case AntennaRXFlag:
			a[reqAN] = "AN010" // socket 1, ANT RX out
		default:
			a[reqAN] = "AN010" // socket 1, and a documented fixed 0 behind it
		}
	}
	return a
}

// newBusyConn is a rig that answers '?' to every command.
func newBusyConn(t *testing.T, y *Rig) *testConn {
	t.Helper()
	c := newTestConn(t, y, answersFor(y.profile))
	c.busy = true
	return c
}

func (c *testConn) Do(ctx context.Context, req []byte, want ...backend.Key) (backend.Update, error) {
	s := string(req)
	c.sent = append(c.sent, s)
	if len(want) == 0 {
		return backend.Update{}, fmt.Errorf("testConn: Do(%q) with no wanted keys", s)
	}

	answer, ok := c.answers[s]
	if c.busy {
		// The busy answer is delivered only to a caller that asked for it. A
		// transaction that did not put the key on its wait list gets what the
		// session gives it: nothing, until the timeout runs out.
		answer, ok = "?", true
	}
	if !ok {
		return backend.Update{}, fmt.Errorf("testConn: timeout waiting for an answer to %q", s)
	}

	u, err := c.y.Decode([]byte(answer))
	if err != nil {
		return u, err
	}
	for _, w := range want {
		if u.Key != w {
			continue
		}
		if !u.OK {
			// The session hands back the update AND an error, which is what
			// lets a backend classify the answer for itself.
			return u, errFakeNAK
		}
		return u, nil
	}
	if c.busy {
		// Nobody was waiting for it, so the frame is dropped and the
		// transaction sits out the whole command timeout. This is the cost the
		// busy key exists to avoid, and charging it here is what makes the
		// fast-fail tests mean something.
		time.Sleep(testCmdTimeout)
		return backend.Update{}, fmt.Errorf("testConn: timeout waiting for an answer to %q", s)
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
		reqAC: "AC001", // internal tuner in line, not tuning
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
