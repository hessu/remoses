package rigctld

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// initAnswers is the conversation a healthy daemon has at Init.
func initAnswers() map[string]string {
	return map[string]string{
		reqDumpState: resp(append([]string{"dump_state:"}, append(lines(sampleDumpStateProto1), "RPRT 0")...)...),
		reqDumpCaps:  resp(append([]string{"dump_caps:"}, append(lines(sampleDumpCaps), "RPRT 0")...)...),
		reqGetFreq:   resp("get_freq:", "Frequency: 14074000", "RPRT 0"),
		reqGetMode:   resp("get_mode:", "Mode: USB", "Passband: 2400", "RPRT 0"),
		reqGetPTT:    resp("get_ptt:", "PTT: 0", "RPRT 0"),
	}
}

func TestNew(t *testing.T) {
	if _, err := New(&config.Radio{ID: "x"}); err == nil {
		t.Error("New accepted a radio with no rigctld block")
	}
	if _, err := New(&config.Radio{ID: "x", Rigctld: &config.Rigctld{}}); err == nil {
		t.Error("New accepted an empty address")
	}
	if _, err := New(nil); err == nil {
		t.Error("New accepted a nil radio")
	}
	if _, err := New(&config.Radio{ID: "x", Rigctld: &config.Rigctld{Address: "127.0.0.1:4532"}}); err != nil {
		t.Errorf("New: %v", err)
	}
}

func TestRegistered(t *testing.T) {
	r := &config.Radio{ID: "ft857", Backend: Name, Rigctld: &config.Rigctld{Address: "127.0.0.1:4532"}}
	g, err := backend.New(r)
	if err != nil {
		t.Fatalf("backend.New: %v", err)
	}
	if _, ok := g.(*Rig); !ok {
		t.Fatalf("registry returned %T", g)
	}
}

// TestCapsBeforeInit proves nothing is claimed before anything has been asked.
func TestCapsBeforeInit(t *testing.T) {
	c := newRig(t).Caps()
	if len(c.Modes) != 0 {
		t.Errorf("modes = %v before Init", c.Modes)
	}
	if c.CWMethod != radio.CWNone {
		t.Errorf("CWMethod = %q before Init", c.CWMethod)
	}
	if c.SupportsMode(radio.ModeUSB) {
		t.Error("a mode was reported supported before Init")
	}
}

// The placeholder above is honest but says CWNone, and the daemon's startup
// check read that as "this radio cannot send Morse" — refusing cw.method: cat
// for every rigctld radio, including the ones whose Hamlib backend has a keyer.
// Found the first time this backend met a real rigctld.
//
// CapsKnown is how a caller tells "cannot" from "not yet".
func TestCapsKnownSaysWhenCapsAreRealYet(t *testing.T) {
	g := newRig(t)
	var _ backend.CapsAtConnect = g

	if g.CapsKnown() {
		t.Fatal("CapsKnown is true before Init; Caps describes nothing yet")
	}
	if err := g.Init(context.Background(), newTestConn(t, g, initAnswers())); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !g.CapsKnown() {
		t.Error("CapsKnown is false after Init")
	}
	if got := g.Caps().CWMethod; got != radio.CWViaCAT {
		t.Errorf("CWMethod = %q after Init on a rig whose dump_caps can send Morse, want %q",
			got, radio.CWViaCAT)
	}
}

func TestInit(t *testing.T) {
	g := newRig(t)
	c := newTestConn(t, g, initAnswers())

	if err := g.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}
	c.wantSent(t, reqDumpState, reqDumpCaps, reqGetFreq, reqGetMode, reqGetPTT)

	caps := g.Caps()
	if !caps.SupportsMode(radio.ModeUSB) || !caps.SupportsMode(radio.ModeCW) {
		t.Errorf("modes after Init = %v", caps.Modes)
	}
	if caps.CWMethod != radio.CWViaCAT {
		t.Errorf("CWMethod = %q, want cat", caps.CWMethod)
	}
	if caps.CWMinWPM != 4 || caps.CWMaxWPM != 48 {
		t.Errorf("wpm range = %d..%d, want the rig's own 4..48", caps.CWMinWPM, caps.CWMaxWPM)
	}
	if g.Model() != "Icom IC-7300" {
		t.Errorf("Model = %q", g.Model())
	}
	// The mode token is remembered so SetFilterWidth has something to send.
	if tok := g.modeName.Load(); tok == nil || *tok != "USB" {
		t.Errorf("mode token = %v, want USB", tok)
	}
}

// TestInitWithoutDumpCaps proves the optional dump is optional: the rig keeps
// everything dump_state reported and loses only the CW claim.
func TestInitWithoutDumpCaps(t *testing.T) {
	g := newRig(t)
	answers := initAnswers()
	delete(answers, reqDumpCaps) // a daemon that never answers it
	c := newTestConn(t, g, answers)

	if err := g.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}
	caps := g.Caps()
	if !caps.SupportsMode(radio.ModeUSB) {
		t.Error("the mode list was lost with dump_caps")
	}
	if caps.CWMethod != radio.CWNone {
		t.Errorf("CWMethod = %q; nothing said the rig can send Morse", caps.CWMethod)
	}
	if caps.FilterWidth {
		t.Error("FilterWidth claimed without a dump saying set_mode exists")
	}
}

// TestInitFailsWithoutDumpState proves the required dump is required: it is the
// link check as well as the capability source.
func TestInitFailsWithoutDumpState(t *testing.T) {
	g := newRig(t)
	c := newTestConn(t, g, map[string]string{})
	if err := g.Init(context.Background(), c); err == nil {
		t.Fatal("Init succeeded against a daemon that answered nothing")
	}
}

// TestInitSeedsDisabledPolls proves a level dump_state did not list is never
// polled for, which saves a guaranteed rejection on every tick.
func TestInitSeedsDisabledPolls(t *testing.T) {
	// A dump with no STRENGTH and no RFPOWER in has_get_level.
	noLevels := strings.Replace(sampleDumpState, "0x40005000", "0x0", 1)
	g := newRig(t)
	answers := initAnswers()
	answers[reqDumpState] = resp(append([]string{"dump_state:"}, append(lines(noLevels), "RPRT 0")...)...)
	c := newTestConn(t, g, answers)

	if err := g.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !g.isDisabled(itemStrength) || !g.isDisabled(itemPower) {
		t.Error("levels the rig does not report are still scheduled for polling")
	}

	c.sent = nil
	if err := g.Poll(context.Background(), c, backend.PollFast); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	c.wantSent(t, reqGetFreq, reqGetMode, reqGetPTT)
}

func TestPoll(t *testing.T) {
	answers := initAnswers()
	answers[reqGetLevel(levelSTRENGTH)] = resp("get_level: STRENGTH", "-12", "RPRT 0")
	answers[reqGetLevel(levelRFPOWER)] = resp("get_level: RFPOWER", "0.75", "RPRT 0")

	g := newRig(t)
	c := newTestConn(t, g, answers)
	if err := g.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}

	c.sent = nil
	if err := g.Poll(context.Background(), c, backend.PollFast); err != nil {
		t.Fatalf("fast poll: %v", err)
	}
	c.wantSent(t, reqGetFreq, reqGetMode, reqGetPTT, reqGetLevel(levelSTRENGTH))

	c.sent = nil
	if err := g.Poll(context.Background(), c, backend.PollSlow); err != nil {
		t.Fatalf("slow poll: %v", err)
	}
	c.wantSent(t, reqGetLevel(levelRFPOWER))

	if err := g.Poll(context.Background(), c, backend.PollTier(99)); err == nil {
		t.Error("an unknown poll tier was accepted")
	}
}

// TestPollDropsUnsupportedItems is the behaviour that keeps a rig without an
// S-meter online. The poller kills the connection after a run of failures, so
// an item the rig will never do has to stop being asked for.
func TestPollDropsUnsupportedItems(t *testing.T) {
	answers := initAnswers()
	// -11 is RIG_ENAVAIL: this rig's Hamlib backend has no get_ptt.
	answers[reqGetPTT] = resp("get_ptt:", "RPRT -11")
	answers[reqGetLevel(levelSTRENGTH)] = resp("get_level: STRENGTH", "RPRT -11")

	g := newRig(t)
	c := newTestConn(t, g, answers)
	if err := g.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Init's own reads are tolerant in the same way, so get_ptt is already
	// known to be missing by the time the first poll runs.
	if !g.isDisabled(itemPTT) {
		t.Error("Init did not learn that get_ptt is unavailable")
	}

	c.sent = nil
	if err := g.Poll(context.Background(), c, backend.PollFast); err != nil {
		t.Fatalf("the first poll must survive the rejection: %v", err)
	}
	c.wantSent(t, reqGetFreq, reqGetMode, reqGetLevel(levelSTRENGTH))

	// The S-meter is not asked for again either.
	c.sent = nil
	if err := g.Poll(context.Background(), c, backend.PollFast); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	c.wantSent(t, reqGetFreq, reqGetMode)
}

// TestPollKeepsAskingForFrequency proves the one exception. Every Hamlib
// backend implements get_freq, so a rig that keeps refusing it is a link fault
// the poller must be allowed to notice.
func TestPollKeepsAskingForFrequency(t *testing.T) {
	answers := initAnswers()
	answers[reqGetFreq] = resp("get_freq:", "RPRT -11")

	g := newRig(t)
	c := newTestConn(t, g, answers)
	if err := g.Init(context.Background(), c); err == nil {
		t.Fatal("Init succeeded against a rig that refuses get_freq")
	}
	if g.isDisabled(itemFreq) {
		t.Fatal("frequency polling was disabled")
	}

	c.sent = nil
	if err := g.Poll(context.Background(), c, backend.PollFast); err == nil {
		t.Error("a refused get_freq did not fail the poll")
	}
	c.wantSent(t, reqGetFreq)
}

// TestPollPropagatesTransientErrors proves a timeout is not mistaken for a
// missing capability: the item must still be polled next time.
func TestPollPropagatesTransientErrors(t *testing.T) {
	answers := initAnswers()
	answers[reqGetLevel(levelSTRENGTH)] = resp("get_level: STRENGTH", "-12", "RPRT 0")

	g := newRig(t)
	c := newTestConn(t, g, answers)
	if err := g.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// -5 is RIG_ETIMEOUT: the rig did not answer this time, which says nothing
	// about what it can do.
	answers[reqGetMode] = resp("get_mode:", "RPRT -5")
	c.sent = nil
	if err := g.Poll(context.Background(), c, backend.PollFast); err == nil {
		t.Error("a timeout was swallowed")
	}
	if g.isDisabled(itemMode) {
		t.Error("a timeout disabled the mode poll")
	}
}

func TestSetters(t *testing.T) {
	tests := []struct {
		name    string
		answers map[string]string
		call    func(*Rig, backend.Conn) error
		want    []string
	}{
		{
			name:    "SetFrequency",
			answers: map[string]string{"+F 14074000\n": resp("set_freq: 14074000", "RPRT 0")},
			call: func(g *Rig, c backend.Conn) error {
				return g.SetFrequency(context.Background(), c, radio.VFOCurrent, 14074000)
			},
			want: []string{"+F 14074000\n"},
		},
		{
			// Mode is read back because the echo says what was asked for and
			// only get_mode says which passband the rig landed on.
			name: "SetMode",
			answers: map[string]string{
				"+M USB 0\n": resp("set_mode: USB 0", "RPRT 0"),
				reqGetMode:   resp("get_mode:", "Mode: USB", "Passband: 2400", "RPRT 0"),
			},
			call: func(g *Rig, c backend.Conn) error {
				return g.SetMode(context.Background(), c, radio.ModeUSB, false)
			},
			want: []string{"+M USB 0\n", reqGetMode},
		},
		{
			name: "SetMode in data mode sends the PKT token",
			answers: map[string]string{
				"+M PKTUSB 0\n": resp("set_mode: PKTUSB 0", "RPRT 0"),
				reqGetMode:      resp("get_mode:", "Mode: PKTUSB", "Passband: 3000", "RPRT 0"),
			},
			call: func(g *Rig, c backend.Conn) error {
				return g.SetMode(context.Background(), c, radio.ModeUSB, true)
			},
			want: []string{"+M PKTUSB 0\n", reqGetMode},
		},
		{
			name:    "SetPTT on",
			answers: map[string]string{"+T 1\n": resp("set_ptt: 1", "RPRT 0")},
			call: func(g *Rig, c backend.Conn) error {
				return g.SetPTT(context.Background(), c, true)
			},
			want: []string{"+T 1\n"},
		},
		{
			name:    "SetPTT off",
			answers: map[string]string{"+T 0\n": resp("set_ptt: 0", "RPRT 0")},
			call: func(g *Rig, c backend.Conn) error {
				return g.SetPTT(context.Background(), c, false)
			},
			want: []string{"+T 0\n"},
		},
		{
			name: "SetPower reads back what the rig took",
			answers: map[string]string{
				"+L RFPOWER 0.400\n":      resp("set_level: RFPOWER 0.400", "RPRT 0"),
				reqGetLevel(levelRFPOWER): resp("get_level: RFPOWER", "0.4", "RPRT 0"),
			},
			call: func(g *Rig, c backend.Conn) error {
				pct := 40.0
				return g.SetPower(context.Background(), c, radio.PowerSet{Pct: &pct})
			},
			want: []string{"+L RFPOWER 0.400\n", reqGetLevel(levelRFPOWER)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			c := newTestConn(t, g, tc.answers)
			if err := tc.call(g, c); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			c.wantSent(t, tc.want...)
		})
	}
}

func TestSetterRefusals(t *testing.T) {
	tests := []struct {
		name    string
		call    func(*Rig, backend.Conn) error
		wantErr string
	}{
		{
			// Targeting a named VFO would need rigctld's -o, which changes the
			// wire protocol this backend speaks.
			name: "a named VFO",
			call: func(g *Rig, c backend.Conn) error {
				return g.SetFrequency(context.Background(), c, radio.VFOB, 14074000)
			},
			wantErr: "current VFO only",
		},
		{
			name: "a mode with no Hamlib token",
			call: func(g *Rig, c backend.Conn) error {
				return g.SetMode(context.Background(), c, radio.ModeCW, true)
			},
			wantErr: "no data-mode variant",
		},
		{
			name: "power in watts",
			call: func(g *Rig, c backend.Conn) error {
				w := 40.0
				return g.SetPower(context.Background(), c, radio.PowerSet{Watts: &w})
			},
			wantErr: "no watt meaning",
		},
		{
			// The passband is only ever the second argument of set_mode, so
			// there is nothing to send before the rig has reported one.
			name: "filter width before the mode is known",
			call: func(g *Rig, c backend.Conn) error {
				return g.SetFilterWidth(context.Background(), c, 500)
			},
			wantErr: "before the rig has reported its mode",
		},
		{
			name: "a negative filter width",
			call: func(g *Rig, c backend.Conn) error {
				return g.SetFilterWidth(context.Background(), c, 0)
			},
			wantErr: "must be positive",
		},
		{
			name: "a filter slot",
			call: func(g *Rig, c backend.Conn) error {
				return g.SetFilterSlot(context.Background(), c, 1)
			},
			wantErr: "no filter-slot command",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			c := newTestConn(t, g, nil)
			err := tc.call(g, c)
			if err == nil {
				t.Fatalf("want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if len(c.sent) != 0 {
				t.Errorf("a refused command still wrote %q", c.sent)
			}
		})
	}
}

// TestSetModeRejectsAModeTheRigLacks proves the operator gets the rig's own
// mode list rather than an anonymous RPRT -1.
func TestSetModeRejectsAModeTheRigLacks(t *testing.T) {
	g := newRig(t)
	c := newTestConn(t, g, initAnswers())
	if err := g.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}

	c.sent = nil
	// The sample rig's mask has no PSK bit.
	err := g.SetMode(context.Background(), c, radio.ModePSK, false)
	if err == nil {
		t.Fatal("SetMode accepted a mode the rig does not have")
	}
	if !strings.Contains(err.Error(), "PSK") || !strings.Contains(err.Error(), "IC-7300") {
		t.Errorf("error = %v, want it to name the mode and the rig", err)
	}
	if len(c.sent) != 0 {
		t.Errorf("the command went to the wire anyway: %q", c.sent)
	}
}

func TestSetFilterWidth(t *testing.T) {
	g := newRig(t)
	c := newTestConn(t, g, map[string]string{
		reqGetMode:    resp("get_mode:", "Mode: CW", "Passband: 500", "RPRT 0"),
		"+M CW 250\n": resp("set_mode: CW 250", "RPRT 0"),
	})
	// Learn the mode the way a poll would.
	if _, err := c.Do(context.Background(), []byte(reqGetMode), keyGetMode); err != nil {
		t.Fatalf("priming get_mode: %v", err)
	}

	c.sent = nil
	if err := g.SetFilterWidth(context.Background(), c, 250); err != nil {
		t.Fatalf("SetFilterWidth: %v", err)
	}
	c.wantSent(t, "+M CW 250\n", reqGetMode)
}

// TestSetterReadBackFailures proves a failed read-back fails the whole command.
// Reporting success while the value remoses publishes is still the old one
// would be worse than reporting the failure.
func TestSetterReadBackFailures(t *testing.T) {
	tests := []struct {
		name    string
		answers map[string]string
		call    func(*Rig, backend.Conn) error
	}{
		{
			name:    "SetMode",
			answers: map[string]string{"+M USB 0\n": resp("set_mode: USB 0", "RPRT 0")},
			call: func(g *Rig, c backend.Conn) error {
				return g.SetMode(context.Background(), c, radio.ModeUSB, false)
			},
		},
		{
			name:    "SetPower",
			answers: map[string]string{"+L RFPOWER 0.400\n": resp("set_level: RFPOWER 0.400", "RPRT 0")},
			call: func(g *Rig, c backend.Conn) error {
				pct := 40.0
				return g.SetPower(context.Background(), c, radio.PowerSet{Pct: &pct})
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			c := newTestConn(t, g, tc.answers) // the read-back is unanswered
			if err := tc.call(g, c); err == nil {
				t.Error("the command reported success without confirming what the rig took")
			}
		})
	}
}

// TestSetterRejections proves a rejected set is reported rather than swallowed.
func TestSetterRejections(t *testing.T) {
	g := newRig(t)
	c := newTestConn(t, g, map[string]string{
		// -1 is RIG_EINVAL: outside this rig's range.
		"+F 999999999999\n": resp("set_freq: 999999999999", "RPRT -1"),
		"+M USB 0\n":        resp("set_mode: USB 0", "RPRT -9"),
		"+T 1\n":            resp("set_ptt: 1", "RPRT -11"),
	})

	if err := g.SetFrequency(context.Background(), c, radio.VFOCurrent, 999999999999); err == nil {
		t.Error("a rejected set_freq was reported as success")
	}
	if err := g.SetMode(context.Background(), c, radio.ModeUSB, false); err == nil {
		t.Error("a rejected set_mode was reported as success")
	}
	if err := g.SetPTT(context.Background(), c, true); err == nil {
		t.Error("a rejected set_ptt was reported as success")
	}
}

// TestSetFilterWidthReadBackFailure covers the same rule for the width, which
// re-sends set_mode and then has to learn what the rig snapped it to.
func TestSetFilterWidthReadBackFailure(t *testing.T) {
	g := newRig(t)
	c := newTestConn(t, g, map[string]string{
		reqGetMode:    resp("get_mode:", "Mode: CW", "Passband: 500", "RPRT 0"),
		"+M CW 250\n": resp("set_mode: CW 250", "RPRT 0"),
	})
	if _, err := c.Do(context.Background(), []byte(reqGetMode), keyGetMode); err != nil {
		t.Fatal(err)
	}
	delete(c.answers, reqGetMode)
	if err := g.SetFilterWidth(context.Background(), c, 250); err == nil {
		t.Error("SetFilterWidth reported success without reading the width back")
	}
}

func TestErrorMapping(t *testing.T) {
	g := newRig(t)
	c := newTestConn(t, g, map[string]string{
		reqGetFreq: resp("get_freq:", "RPRT -5"),
	})

	_, err := g.do(context.Background(), c, reqGetFreq, keyGetFreq)
	if err == nil {
		t.Fatal("a rejection produced no error")
	}
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("error = %T (%v), want a *rigctld.Error", err, err)
	}
	if he.Code != -5 {
		t.Errorf("code = %d, want -5", he.Code)
	}
	if he.Command != "f" {
		t.Errorf("command = %q, want the request as an operator would type it", he.Command)
	}
	// The message has to carry the number and its meaning: -5 is a rig that did
	// not answer, -11 is a rig that cannot.
	if !strings.Contains(err.Error(), "RPRT -5") || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("message = %q", err.Error())
	}
	if he.Unsupported() {
		t.Error("a timeout was classified as a missing capability")
	}
}

// TestErrorOnUnterminatedBlock covers the other failure shape: the size guard
// hands Decode a block with no RPRT, and the transaction has to fail with
// something an operator can read rather than a bare NAK.
func TestErrorOnUnterminatedBlock(t *testing.T) {
	g := newRig(t)
	c := newTestConn(t, g, map[string]string{
		reqGetFreq: resp("get_freq:", "Frequency: 14074000"),
	})
	_, err := g.do(context.Background(), c, reqGetFreq, keyGetFreq)
	if err == nil || !strings.Contains(err.Error(), "no RPRT line") {
		t.Fatalf("error = %v, want it to name the missing terminator", err)
	}
	var he *Error
	if errors.As(err, &he) {
		t.Error("a block with no RPRT was reported as a Hamlib return code")
	}
}

func TestModel(t *testing.T) {
	g := newRig(t)
	if got := g.Model(); got != "" {
		t.Errorf("Model = %q before Init, want empty", got)
	}
	if got := g.describe(); got != "this rig" {
		t.Errorf("describe = %q before Init", got)
	}

	// dump_state carries the numeric model, dump_caps the name. A daemon that
	// answered only the first still identifies the rig usefully.
	answers := initAnswers()
	delete(answers, reqDumpCaps)
	c := newTestConn(t, g, answers)
	if err := g.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := g.Model(); got != "Hamlib model 3073" {
		t.Errorf("Model = %q, want the numeric fallback", got)
	}
}

func TestErrorUnsupported(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{-errEINVAL, true},
		{-errENIMPL, true},
		{-errERJCTED, true},
		{-errENAVAIL, true},
		{-errENTARGET, true},
		{-errEVFO, true},
		{-errEDOM, true},

		{-errETIMEOUT, false},
		{-errEIO, false},
		{-errEPROTO, false},
		{-errBUSBUSY, false},
		// Both of these come back on their own, so the poll must keep trying.
		{-errESECURITY, false},
		{-errEPOWER, false},
		{0, false},
	}
	for _, tc := range tests {
		e := &Error{Command: "f", Code: tc.code}
		if got := e.Unsupported(); got != tc.want {
			t.Errorf("RPRT %d Unsupported = %v, want %v (%s)", tc.code, got, tc.want, hamlibError(tc.code))
		}
	}
}

func TestHamlibError(t *testing.T) {
	if s := hamlibError(0); s != "success" {
		t.Errorf("hamlibError(0) = %q", s)
	}
	if s := hamlibError(-11); !strings.Contains(s, "does not have that function") {
		t.Errorf("hamlibError(-11) = %q", s)
	}
	// Hamlib keeps adding to the enum; an unknown code is reported, not guessed.
	if s := hamlibError(-9999); !strings.Contains(s, "unrecognised") {
		t.Errorf("hamlibError(-9999) = %q", s)
	}
}

func TestLabel(t *testing.T) {
	for req, want := range map[string]string{
		reqGetFreq:                 "f",
		reqDumpState:               `\dump_state`,
		"+L RFPOWER 0.500\n":       "L RFPOWER 0.500",
		reqGetLevel(levelSTRENGTH): "l STRENGTH",
	} {
		if got := label(req); got != want {
			t.Errorf("label(%q) = %q, want %q", req, got, want)
		}
	}
}
