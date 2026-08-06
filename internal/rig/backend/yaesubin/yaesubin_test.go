package yaesubin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

func TestNewRequiresAKnownModel(t *testing.T) {
	if _, err := New(radioCfg("ftdx10")); err == nil {
		t.Error("New accepted an ASCII-dialect radio")
	}
	if _, err := New(nil); err == nil {
		t.Error("New accepted a nil configuration; this package has no default model")
	}
	y := testRig(t, "ft-897d")
	if got := y.Model(); got != "Yaesu FT-897D" {
		t.Errorf("Model() = %q", got)
	}
}

// TestCapsRecordsWhatIsMissing is most of what this backend has to say about
// these radios, and each false is a documented absence. Publishing them wrongly
// would produce a client offering controls that can only ever draw an error.
func TestCapsRecordsWhatIsMissing(t *testing.T) {
	c := testRig(t, "ft-857d").Caps()

	if c.PowerWattAccurate || c.MaxPowerW != 0 {
		t.Errorf("Caps claims a power scale: watt_accurate=%v max=%v", c.PowerWattAccurate, c.MaxPowerW)
	}
	if c.FilterWidth || c.FilterSlots != 0 {
		t.Errorf("Caps claims filter control: width=%v slots=%d", c.FilterWidth, c.FilterSlots)
	}
	if c.CWMethod != radio.CWNone {
		t.Errorf("cw_method = %q, want none: there is no keyer buffer command", c.CWMethod)
	}
	if c.SubReceiver {
		t.Error("Caps claims a sub receiver")
	}
	if c.SMeterScale != 15 {
		t.Errorf("s_meter_scale = %d, want 15: the meter is four bits", c.SMeterScale)
	}
	if len(c.VFOs) != 1 || c.VFOs[0] != radio.VFOCurrent {
		t.Errorf("VFOs = %v, want only the current one: nothing reports which of A and B is selected", c.VFOs)
	}
}

// TestCapsModesIsAFreshSlice guards against a caps list published through the
// API sharing a backing array with the profile.
func TestCapsModesIsAFreshSlice(t *testing.T) {
	y := testRig(t, "ft-857d")
	c := y.Caps()
	c.Modes[0] = radio.ModeUnknown
	if y.Caps().Modes[0] == radio.ModeUnknown {
		t.Error("Caps().Modes aliases the model profile")
	}
}

// TestMorseSenderNotImplemented is a type assertion the daemon effectively
// makes. Implementing it partially would be worse than not at all: a successful
// assertion produces failures that look to the operator like a message was
// sent.
func TestMorseSenderNotImplemented(t *testing.T) {
	var r backend.Rig = testRig(t, "ft-857d")
	if _, ok := r.(backend.MorseSender); ok {
		t.Error("the backend implements MorseSender; no command in this protocol keys arbitrary text")
	}
}

func TestReplyFramerImplemented(t *testing.T) {
	var r backend.Rig = testRig(t, "ft-857d")
	if _, ok := r.(backend.ReplyFramer); !ok {
		t.Fatal("the backend does not implement ReplyFramer; nothing else can size an answer on this protocol")
	}
}

// TestInitReadsState covers the connect path. There is nothing to enable first
// — no AI, no transceive — so the first read is both the state fetch and the
// only proof anything is listening.
func TestInitReadsState(t *testing.T) {
	y := testRig(t, "ft-857d")
	c := newTestConn(t, y, answersFor())

	if err := y.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}
	c.wantSent(t,
		hex(read(opReadFreqMode)),
		hex(read(opReadTXStatus)),
		hex(read(opReadRXStatus)),
	)
}

// TestInitFailsOnASilentRig is what a radio switched off behind a live USB
// adapter looks like: the port opens, nothing answers.
func TestInitFailsOnASilentRig(t *testing.T) {
	y := testRig(t, "ft-857d")
	c := newTestConn(t, y, nil)

	err := y.Init(context.Background(), c)
	if err == nil {
		t.Fatal("Init succeeded against a rig that answered nothing")
	}
	if !strings.Contains(err.Error(), "read frequency & mode") {
		t.Errorf("Init error does not name the command that failed: %v", err)
	}
}

// TestPollSkipsTheReceiveMeterWhileKeyed is the shape of the fast poll. A
// transmitting radio is not measuring a received signal, and these radios are
// reported to answer FF to the receive-status read, which decodes to a
// plausible full-scale reading rather than to anything recognisable as "no
// signal".
func TestPollSkipsTheReceiveMeterWhileKeyed(t *testing.T) {
	y := testRig(t, "ft-857d")
	answers := answersFor()
	answers[hex(read(opReadTXStatus))] = []byte{0x0C} // keyed, power 12

	c := newTestConn(t, y, answers)
	if err := y.Poll(context.Background(), c, backend.PollFast); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	c.wantSent(t,
		hex(read(opReadFreqMode)),
		hex(read(opReadTXStatus)),
	)
}

// TestPollSlowAsksNothing states the absence plainly: power, filter width and
// filter slot are the slow tier everywhere else, and no command here reads any
// of them.
func TestPollSlowAsksNothing(t *testing.T) {
	y := testRig(t, "ft-857d")
	c := newTestConn(t, y, answersFor())

	if err := y.Poll(context.Background(), c, backend.PollSlow); err != nil {
		t.Fatalf("Poll(slow): %v", err)
	}
	c.wantSent(t)
}

func TestSetFrequency(t *testing.T) {
	y := testRig(t, "ft-857d")
	set := block(0x01, 0x42, 0x50, 0x00, opSetFrequency)
	c := newTestConn(t, y, withAcks(answersFor(), set))

	if err := y.SetFrequency(context.Background(), c, radio.VFOCurrent, 14_250_000); err != nil {
		t.Fatalf("SetFrequency: %v", err)
	}
	// The set, its acknowledgement consumed, then the read-back.
	c.wantSent(t, hex(set), hex(read(opReadFreqMode)))
}

// TestSetFrequencyRefusesANamedVFO is the honest refusal. Opcode 81 toggles A
// and B blindly and nothing reports which is selected, so accepting "A" would
// mean tuning whichever VFO the operator was on and calling it A.
func TestSetFrequencyRefusesANamedVFO(t *testing.T) {
	y := testRig(t, "ft-857d")
	c := newTestConn(t, y, answersFor())

	for _, vfo := range []radio.VFO{radio.VFOA, radio.VFOB, radio.VFOMain, radio.VFOSub} {
		err := y.SetFrequency(context.Background(), c, vfo, 14_250_000)
		if err == nil {
			t.Fatalf("SetFrequency(%v) succeeded", vfo)
		}
		if !strings.Contains(err.Error(), "toggles A and B blindly") {
			t.Errorf("SetFrequency(%v) error does not explain itself: %v", vfo, err)
		}
	}
	c.wantSent(t) // nothing reached the wire
}

func TestSetFrequencyRangeChecked(t *testing.T) {
	y := testRig(t, "ft-857d")
	c := newTestConn(t, y, answersFor())

	if err := y.SetFrequency(context.Background(), c, radio.VFOCurrent, 900_000_000); err == nil {
		t.Fatal("SetFrequency accepted 900 MHz")
	}
	// Nothing may go out: this protocol has no rejection, so an out-of-range
	// frequency would be answered exactly as a good one is.
	c.wantSent(t)
}

func TestSetMode(t *testing.T) {
	y := testRig(t, "ft-857d")
	set := block(0x0C, 0, 0, 0, opSetMode) // PKT
	c := newTestConn(t, y, withAcks(answersFor(), set))

	if err := y.SetMode(context.Background(), c, radio.ModeFM, true); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	c.wantSent(t, hex(set), hex(read(opReadFreqMode)))
}

func TestSetPTT(t *testing.T) {
	y := testRig(t, "ft-857d")
	on, off := read(opPTTOn), read(opPTTOff)
	c := newTestConn(t, y, withAcks(answersFor(), on, off))

	if err := y.SetPTT(context.Background(), c, true); err != nil {
		t.Fatalf("SetPTT(true): %v", err)
	}
	if err := y.SetPTT(context.Background(), c, false); err != nil {
		t.Fatalf("SetPTT(false): %v", err)
	}
	c.wantSent(t, hex(on), hex(off))
}

// TestSetPTTToleratesTheAlreadyInThatStateAnswer is the safety case. ForceRX
// sends an unkey whether or not the radio is transmitting; F0 is reported to be
// what a redundant one draws, and failing on it would report the dead-man timer
// as broken every time it did its job.
func TestSetPTTToleratesTheAlreadyInThatStateAnswer(t *testing.T) {
	y := testRig(t, "ft-857d")
	c := newTestConn(t, y, map[string][]byte{
		hex(read(opPTTOff)): {0xF0},
	})
	if err := y.SetPTT(context.Background(), c, false); err != nil {
		t.Fatalf("SetPTT(false) failed on an F0 acknowledgement: %v", err)
	}
}

// TestUnsupportedControlsExplainThemselves covers the three things these radios
// simply do not have. Each refusal has to say what the operator should do
// instead, because each is a control a client may well be showing.
func TestUnsupportedControlsExplainThemselves(t *testing.T) {
	y := testRig(t, "ft-857d")
	c := newTestConn(t, y, answersFor())
	ctx := context.Background()
	pct := 50.0

	cases := []struct {
		what string
		err  error
		want string
	}{
		{"power", y.SetPower(ctx, c, radio.PowerSet{Pct: &pct}), "RF POWER SET"},
		{"filter width", y.SetFilterWidth(ctx, c, 500), "front-panel keys"},
		{"filter slot", y.SetFilterSlot(ctx, c, 1), "front-panel keys"},
	}
	for _, tc := range cases {
		if tc.err == nil {
			t.Errorf("Set%s succeeded; no such command exists", tc.what)
			continue
		}
		if !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("Set%s error does not point anywhere useful: %v", tc.what, tc.err)
		}
	}
	c.wantSent(t)
}

// TestRefusalsAreUnsupportedNotFailures decides what a client is told. These
// radios refuse four controls a client may reasonably offer, and each refusal
// has to reach the API as 422 with its own explanation — "your radio has no
// such control" — rather than as the 500 catch-all, which reads as a daemon
// bug and invites a retry that can never work.
func TestRefusalsAreUnsupportedNotFailures(t *testing.T) {
	y := testRig(t, "ft-857d")
	c := newTestConn(t, y, answersFor())
	ctx := context.Background()
	pct := 50.0

	errs := map[string]error{
		"power":           y.SetPower(ctx, c, radio.PowerSet{Pct: &pct}),
		"filter width":    y.SetFilterWidth(ctx, c, 500),
		"filter slot":     y.SetFilterSlot(ctx, c, 1),
		"named VFO":       y.SetFrequency(ctx, c, radio.VFOB, 14_250_000),
		"frequency range": y.SetFrequency(ctx, c, radio.VFOCurrent, 900_000_000),
		"mode WFM":        y.SetMode(ctx, c, radio.ModeWFM, false),
		"USB with data":   y.SetMode(ctx, c, radio.ModeUSB, true),
	}
	for what, err := range errs {
		if err == nil {
			t.Errorf("%s: no error", what)
			continue
		}
		if !errors.Is(err, backend.ErrUnsupported) {
			t.Errorf("%s refused with %v, which the API reports as a 500; "+
				"wrap it in backend.ErrUnsupported so the client is told it is a 422", what, err)
		}
	}
}

// TestEveryCommandConsumesItsAnswer is the invariant the whole package rests
// on. A set fired with Send would leave its acknowledgement in the stream, and
// on a protocol with no delimiters that byte becomes the first byte of the next
// answer for ever. testConn fails the test if Send is called at all; this
// drives every write path past it.
func TestEveryCommandConsumesItsAnswer(t *testing.T) {
	y := testRig(t, "ft-857d")
	ctx := context.Background()

	setFreq := block(0x01, 0x42, 0x50, 0x00, opSetFrequency)
	setMode := block(0x01, 0, 0, 0, opSetMode)
	c := newTestConn(t, y, withAcks(answersFor(), setFreq, setMode, read(opPTTOn), read(opPTTOff)))

	if err := y.Init(ctx, c); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := y.SetFrequency(ctx, c, radio.VFOCurrent, 14_250_000); err != nil {
		t.Fatalf("SetFrequency: %v", err)
	}
	if err := y.SetMode(ctx, c, radio.ModeUSB, false); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if err := y.SetPTT(ctx, c, true); err != nil {
		t.Fatalf("SetPTT: %v", err)
	}
	if err := y.SetPTT(ctx, c, false); err != nil {
		t.Fatalf("SetPTT: %v", err)
	}
	if err := y.Poll(ctx, c, backend.PollFast); err != nil {
		t.Fatalf("Poll: %v", err)
	}
}

// TestPollFailsOnACorruptedStream drives the recovery path end to end: a rig
// answering rubbish produces an error from the poll, which the session counts
// and, five in a row, turns into a reconnect.
func TestPollFailsOnACorruptedStream(t *testing.T) {
	y := testRig(t, "ft-857d")
	answers := answersFor()
	answers[hex(read(opReadFreqMode))] = []byte{0xAB, 0xCD, 0xEF, 0x00, 0x01}

	c := newTestConn(t, y, answers)
	if err := y.Poll(context.Background(), c, backend.PollFast); err == nil {
		t.Fatal("a poll against a corrupted stream succeeded; nothing would ever reconnect")
	}
}
