package kenwood

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// initAnswers is a rig sitting on 14.025 MHz in CW, 50 W, filter A, receiving.
func initAnswers() map[string]string {
	return map[string]string{
		reqID: "ID023",
		reqFA: "FA00014025000",
		reqMD: "MD3",
		reqDA: "DA0",
		reqPC: "PC050",
		reqFL: "FL1",
		reqIF: sampleIF,
		reqSM: "SM00000",
		reqFW: "FW0500",
	}
}

func TestNewDefaults(t *testing.T) {
	// A missing kenwood block must land on the same defaults config would have
	// filled in, since a backend can be built directly.
	k, err := New(&config.Radio{ID: "ts590", Backend: Name})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if k.ai != 2 {
		t.Errorf("auto-information = %d, want 2 (AI2 self-clears at rig power-off)", k.ai)
	}
	if !k.bulkPoll {
		t.Error("bulk poll off by default; IF; is the whole point of this backend")
	}
}

func TestNewFromConfig(t *testing.T) {
	k, err := New(&config.Radio{
		ID:      "ts590",
		Backend: Name,
		Kenwood: &config.Kenwood{Model: "ts590sg", AutoInformation: 4, BulkPoll: false},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if k.ai != 4 || k.bulkPoll {
		t.Errorf("config not honoured: ai=%d bulk=%v", k.ai, k.bulkPoll)
	}
	if k.Model() != "TS-590SG" {
		t.Errorf("Model = %q, want TS-590SG", k.Model())
	}
}

func TestNewRejectsBadAutoInformation(t *testing.T) {
	for _, ai := range []int{1, 3, 5, -1} {
		_, err := New(&config.Radio{Kenwood: &config.Kenwood{AutoInformation: ai}})
		if err == nil {
			t.Errorf("New accepted auto_information %d; only 0, 2 and 4 are defined", ai)
		}
	}
}

func TestRegistered(t *testing.T) {
	found := false
	for _, n := range backend.Registered() {
		if n == Name {
			found = true
		}
	}
	if !found {
		t.Fatalf("backend %q not registered (have %v)", Name, backend.Registered())
	}

	r, err := backend.New(&config.Radio{ID: "ts590", Backend: Name, Kenwood: &config.Kenwood{}})
	if err != nil {
		t.Fatalf("backend.New: %v", err)
	}
	if _, ok := r.(backend.MorseSender); !ok {
		t.Error("the registered backend does not implement MorseSender; CAT CW would be unavailable")
	}
}

func TestCaps(t *testing.T) {
	c := newRig(t, 2, true).Caps()

	if !c.PowerWattAccurate || c.MaxPowerW != 100 {
		t.Errorf("power caps = {accurate %v, max %v}, want {true, 100}", c.PowerWattAccurate, c.MaxPowerW)
	}
	if c.SMeterScale != 30 {
		t.Errorf("SMeterScale = %d, want 30 (SM counts meter dots, 0000..0030)", c.SMeterScale)
	}
	if c.FilterSlots != 2 || !c.FilterWidth {
		t.Errorf("filter caps = {slots %d, width %v}, want {2, true}", c.FilterSlots, c.FilterWidth)
	}
	if c.CWMethod != radio.CWViaCAT || c.CWCharset != Charset {
		t.Errorf("CW caps = {%s, %q}", c.CWMethod, c.CWCharset)
	}
	if c.CWMinWPM != 4 || c.CWMaxWPM != 60 {
		t.Errorf("wpm range = %d..%d, want 4..60", c.CWMinWPM, c.CWMaxWPM)
	}
	if c.SubReceiver {
		t.Error("SubReceiver true; the TS-590 has one receiver")
	}

	for _, m := range []radio.Mode{
		radio.ModeLSB, radio.ModeUSB, radio.ModeCW, radio.ModeCWR,
		radio.ModeAM, radio.ModeFM, radio.ModeFSK, radio.ModeFSKR,
	} {
		if !c.SupportsMode(m) {
			t.Errorf("caps omit %s", m)
		}
	}
	// PSK is decoded in software through the data modes; MD has no value for
	// it, so advertising it would be a promise the backend cannot keep.
	for _, m := range []radio.Mode{radio.ModePSK, radio.ModePSKR} {
		if c.SupportsMode(m) {
			t.Errorf("caps advertise %s, which MD cannot select", m)
		}
	}
}

func TestInit(t *testing.T) {
	k := newRig(t, 2, true)
	c := newTestConn(t, k, initAnswers())

	if err := k.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// AI first (fire and forget: the rig answers a set only when AI is already
	// on), then ID; as the link check, then the state reads. DA; precedes IF;
	// because it decides whether IF; will answer at all, and MD; precedes PC;
	// because the watt ceiling depends on the mode.
	c.wantSent(t, "AI2;", "ID;", "FA;", "MD;", "DA;", "PC;", "FL;", "IF;")

	if k.Model() != "TS-590SG" {
		t.Errorf("Model = %q after ID023, want TS-590SG", k.Model())
	}
	if k.lastMode() != radio.ModeCW {
		t.Errorf("mode = %s, want CW", k.lastMode())
	}
}

func TestInitAutoInformationValues(t *testing.T) {
	for _, tt := range []struct {
		ai   int
		want string
	}{
		{0, "AI0;"},
		{2, "AI2;"},
		{4, "AI4;"},
	} {
		k := newRig(t, tt.ai, true)
		c := newTestConn(t, k, initAnswers())
		if err := k.Init(context.Background(), c); err != nil {
			t.Fatalf("Init: %v", err)
		}
		if c.sent[0] != tt.want {
			t.Errorf("first command = %q, want %q", c.sent[0], tt.want)
		}
	}
}

// TestInitSkipsIFInDataMode is the quirk in miniature: DA; comes back 1, so IF;
// is never asked, because the rig would not answer and the transaction would
// burn a full timeout.
func TestInitSkipsIFInDataMode(t *testing.T) {
	k := newRig(t, 2, true)
	answers := initAnswers()
	answers[reqDA] = "DA1"
	answers[reqMD] = "MD2" // USB-DATA is MD2 + DA1
	c := newTestConn(t, k, answers)

	if err := k.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}
	c.wantSent(t, "AI2;", "ID;", "FA;", "MD;", "DA;", "PC;", "FL;")

	if !k.dataMode.Load() {
		t.Error("data mode not recorded from DA1")
	}
	if k.useBulkPoll() {
		t.Error("bulk poll still enabled after DA1")
	}
}

// TestInitToleratesSilentIF covers a rig that ignores IF; for some reason other
// than Data mode. Init must not fail: everything IF; would have supplied except
// PTT has already been read individually.
func TestInitToleratesSilentIF(t *testing.T) {
	k := newRig(t, 2, true)
	answers := initAnswers()
	delete(answers, reqIF)
	c := newTestConn(t, k, answers)

	if err := k.Init(context.Background(), c); err != nil {
		t.Fatalf("Init failed on a silent IF;: %v", err)
	}
	if !k.ifBlocked.Load() {
		t.Error("a silent IF; did not mark the bulk poll unavailable")
	}
}

func TestInitFailsOnASilentRig(t *testing.T) {
	k := newRig(t, 2, true)
	c := newTestConn(t, k, map[string]string{}) // nothing answers
	if err := k.Init(context.Background(), c); err == nil {
		t.Fatal("Init succeeded against a rig that answered nothing")
	}
}

func TestPollFast(t *testing.T) {
	tests := []struct {
		name string
		// setup puts the backend into the state under test.
		setup func(*Rig)
		bulk  bool
		want  []string
	}{
		{
			// One 38-byte answer covers frequency, RX/TX and mode, so the fast
			// poll is two transactions instead of three.
			name: "bulk poll", bulk: true,
			want: []string{"IF;", "SM0;"},
		},
		{
			// The single most important quirk in this backend: IF; is refused
			// in Data mode, and refused by silence.
			name: "data mode falls back to discrete reads", bulk: true,
			setup: func(k *Rig) { k.dataMode.Store(true) },
			want:  []string{"FA;", "MD;", "SM0;"},
		},
		{
			name: "bulk poll disabled in config", bulk: false,
			want: []string{"FA;", "MD;", "SM0;"},
		},
		{
			name: "a previously refused IF stays on the discrete path", bulk: true,
			setup: func(k *Rig) { k.ifBlocked.Store(true) },
			want:  []string{"FA;", "MD;", "SM0;"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, tt.bulk)
			if tt.setup != nil {
				tt.setup(k)
			}
			c := newTestConn(t, k, initAnswers())
			if err := k.Poll(context.Background(), c, backend.PollFast); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			c.wantSent(t, tt.want...)
		})
	}
}

// TestPollFastFallsBackAfterASilentIF covers the transition an operator actually
// makes: switch the rig into Data mode, and the next poll stalls once before the
// backend gives up on IF;.
func TestPollFastFallsBackAfterASilentIF(t *testing.T) {
	k := newRig(t, 2, true)
	answers := initAnswers()
	delete(answers, reqIF) // the rig has entered Data mode and stopped answering
	c := newTestConn(t, k, answers)
	ctx := context.Background()

	// One poll is lost. Retrying inside the same call would stack three
	// timeouts on a link that may simply be dead.
	if err := k.Poll(ctx, c, backend.PollFast); err == nil {
		t.Fatal("Poll succeeded despite a silent IF;")
	}
	c.wantSent(t, "IF;")

	c.sent = nil
	if err := k.Poll(ctx, c, backend.PollFast); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	c.wantSent(t, "FA;", "MD;", "SM0;")

	// A DA; reporting Data mode off is what lets the bulk poll be tried again,
	// giving it a retry cadence of one slow poll rather than never.
	c.sent = nil
	if err := k.Poll(ctx, c, backend.PollSlow); err != nil {
		t.Fatalf("slow poll: %v", err)
	}
	c.sent = nil
	c.answers[reqIF] = sampleIF
	if err := k.Poll(ctx, c, backend.PollFast); err != nil {
		t.Fatalf("third poll: %v", err)
	}
	c.wantSent(t, "IF;", "SM0;")
}

func TestPollSlow(t *testing.T) {
	tests := []struct {
		name string
		mode radio.Mode
		want []string
	}{
		// FW carries a bandwidth only in CW and FSK.
		{"CW includes the filter width", radio.ModeCW, []string{"PC;", "FL;", "DA;", "FW;"}},
		{"CW-R includes the filter width", radio.ModeCWR, []string{"PC;", "FL;", "DA;", "FW;"}},
		{"FSK includes the filter width", radio.ModeFSK, []string{"PC;", "FL;", "DA;", "FW;"}},
		// In SSB and AM the rig refuses FW outright; in FM it answers with a
		// modulation-degree switch that would land in State as a 0 Hz passband.
		{"USB skips it", radio.ModeUSB, []string{"PC;", "FL;", "DA;"}},
		{"LSB skips it", radio.ModeLSB, []string{"PC;", "FL;", "DA;"}},
		{"AM skips it", radio.ModeAM, []string{"PC;", "FL;", "DA;"}},
		{"FM skips it", radio.ModeFM, []string{"PC;", "FL;", "DA;"}},
		{"an unknown mode skips it", radio.ModeUnknown, []string{"PC;", "FL;", "DA;"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			k.mode.Store(uint32(tt.mode))
			c := newTestConn(t, k, initAnswers())
			if err := k.Poll(context.Background(), c, backend.PollSlow); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			c.wantSent(t, tt.want...)
		})
	}
}

func TestPollUnknownTier(t *testing.T) {
	k := newRig(t, 2, true)
	c := newTestConn(t, k, initAnswers())
	if err := k.Poll(context.Background(), c, backend.PollTier(99)); err == nil {
		t.Fatal("Poll accepted an unknown tier")
	}
}

func TestSetFrequency(t *testing.T) {
	tests := []struct {
		name string
		vfo  radio.VFO
		hz   uint64
		want []string
	}{
		{"VFO A", radio.VFOA, 14_025_000, []string{"FA00014025000;", "FA;"}},
		// Nothing in this backend tracks FR;/FT;, and every read path is
		// anchored on VFO A, so "current" means A.
		{"current means A", radio.VFOCurrent, 7_050_000, []string{"FA00007050000;", "FA;"}},
		{"VFO B", radio.VFOB, 14_200_000, []string{"FB00014200000;", "FB;"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			c := newTestConn(t, k, map[string]string{
				reqFA: "FA00014025000",
				reqFB: "FB00014200000",
			})
			if err := k.SetFrequency(context.Background(), c, tt.vfo, tt.hz); err != nil {
				t.Fatalf("SetFrequency: %v", err)
			}
			c.wantSent(t, tt.want...)
		})
	}
}

func TestSetFrequencyErrors(t *testing.T) {
	k := newRig(t, 2, true)
	c := newTestConn(t, k, initAnswers())
	ctx := context.Background()

	if err := k.SetFrequency(ctx, c, radio.VFOSub, 14_025_000); err == nil {
		t.Error("accepted a sub-receiver VFO on a single-receiver rig")
	}
	if err := k.SetFrequency(ctx, c, radio.VFOMain, 14_025_000); err == nil {
		t.Error("accepted a main-receiver VFO")
	}
	if err := k.SetFrequency(ctx, c, radio.VFOA, maxFrequencyHz+1); err == nil {
		t.Error("accepted a frequency too wide for the 11-digit field")
	}
	if len(c.sent) != 0 {
		t.Errorf("wrote %q despite rejecting the request", c.sent)
	}
}

func TestSetMode(t *testing.T) {
	tests := []struct {
		name string
		mode radio.Mode
		data bool
		want []string
	}{
		{
			// DA is rejected in CW, so it is not sent. Safe: the rig reports
			// Data mode off by itself in every non-DATA mode.
			name: "CW sends MD only", mode: radio.ModeCW,
			want: []string{"MD3;", "MD;"},
		},
		{
			name: "USB clears data mode", mode: radio.ModeUSB,
			want: []string{"MD2;", "MD;", "DA0;", "DA;"},
		},
		{
			// USB-DATA is MD2 + DA1: the two settings are orthogonal, which is
			// why radio.State carries DataMode separately from Mode.
			name: "USB-DATA sets both", mode: radio.ModeUSB, data: true,
			want: []string{"MD2;", "MD;", "DA1;", "DA;"},
		},
		{
			name: "AM-DATA", mode: radio.ModeAM, data: true,
			want: []string{"MD5;", "MD;", "DA1;", "DA;"},
		},
		{
			name: "FSK sends MD only", mode: radio.ModeFSK,
			want: []string{"MD6;", "MD;"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			digit, _ := encodeMode(tt.mode)
			answers := map[string]string{
				reqMD: "MD" + string(digit),
				reqDA: "DA0",
			}
			if tt.data {
				answers[reqDA] = "DA1"
			}
			c := newTestConn(t, k, answers)
			if err := k.SetMode(context.Background(), c, tt.mode, tt.data); err != nil {
				t.Fatalf("SetMode: %v", err)
			}
			c.wantSent(t, tt.want...)
		})
	}
}

func TestSetModeErrors(t *testing.T) {
	k := newRig(t, 2, true)
	c := newTestConn(t, k, initAnswers())
	ctx := context.Background()

	if err := k.SetMode(ctx, c, radio.ModePSK, false); err == nil {
		t.Error("accepted PSK, which MD cannot select")
	}
	// "When used in CW, FSK, an error occurs."
	err := k.SetMode(ctx, c, radio.ModeCW, true)
	if err == nil {
		t.Fatal("accepted CW-DATA, which the rig rejects")
	}
	if !strings.Contains(err.Error(), "LSB, USB, FM and AM") {
		t.Errorf("error %q does not say where DATA mode is available", err)
	}
	if len(c.sent) != 0 {
		t.Errorf("wrote %q despite rejecting the request", c.sent)
	}
}

func TestSetPower(t *testing.T) {
	watts := func(v float64) radio.PowerSet { return radio.PowerSet{Watts: &v} }
	pct := func(v float64) radio.PowerSet { return radio.PowerSet{Pct: &v} }

	tests := []struct {
		name string
		mode radio.Mode
		set  radio.PowerSet
		want string
	}{
		{"watts in SSB", radio.ModeUSB, watts(50), "PC050;"},
		{"minimum", radio.ModeUSB, watts(5), "PC005;"},
		{"maximum", radio.ModeUSB, watts(100), "PC100;"},
		// Off-grid values are sent as asked: the 5 W step only applies while
		// the rig's Power Fine setting is off, and there is no way to ask.
		{"off-grid watts", radio.ModeUSB, watts(93), "PC093;"},
		{"percent in SSB", radio.ModeUSB, pct(75), "PC075;"},
		// The AM ceiling is 25 W, so 100% is a quarter of the SSB value.
		{"percent in AM", radio.ModeAM, pct(100), "PC025;"},
		{"watts clamped to the AM ceiling", radio.ModeAM, watts(100), "PC025;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			k.mode.Store(uint32(tt.mode))
			c := newTestConn(t, k, map[string]string{reqPC: "PC050"})
			if err := k.SetPower(context.Background(), c, tt.set); err != nil {
				t.Fatalf("SetPower: %v", err)
			}
			// The read-back is not ceremony: the rig may round the request
			// down onto its 5 W grid.
			c.wantSent(t, tt.want, "PC;")
		})
	}
}

// TestSetPTTIsFireAndForget pins the one rule that would otherwise cost a
// timeout on the most latency-sensitive command in the API: TX; and RX; produce
// no answer unless AI happens to be on.
//
// The test conn has no scripted answers at all, so a Do here would fail.
func TestSetPTTIsFireAndForget(t *testing.T) {
	for _, tt := range []struct {
		on   bool
		want string
	}{
		{true, "TX;"},
		{false, "RX;"},
	} {
		k := newRig(t, 2, true)
		c := newTestConn(t, k, nil)
		if err := k.SetPTT(context.Background(), c, tt.on); err != nil {
			t.Fatalf("SetPTT(%v): %v — did it wait for an answer?", tt.on, err)
		}
		c.wantSent(t, tt.want)
	}
}

func TestSetFilterWidth(t *testing.T) {
	tests := []struct {
		name string
		mode radio.Mode
		hz   int
		want string
	}{
		{"CW on a rung", radio.ModeCW, 500, "FW0500;"},
		{"CW snapped down", radio.ModeCW, 1400, "FW1000;"},
		{"CW below the ladder", radio.ModeCW, 10, "FW0050;"},
		{"CW above the ladder", radio.ModeCW, 5000, "FW2500;"},
		{"FSK snapped down", radio.ModeFSK, 1400, "FW1000;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			k.mode.Store(uint32(tt.mode))
			c := newTestConn(t, k, map[string]string{reqFW: "FW0500"})
			if err := k.SetFilterWidth(context.Background(), c, tt.hz); err != nil {
				t.Fatalf("SetFilterWidth: %v", err)
			}
			c.wantSent(t, tt.want, "FW;")
		})
	}
}

func TestSetFilterWidthIllegalModes(t *testing.T) {
	for _, mode := range []radio.Mode{radio.ModeUSB, radio.ModeLSB, radio.ModeAM, radio.ModeFM} {
		t.Run(mode.String(), func(t *testing.T) {
			k := newRig(t, 2, true)
			k.mode.Store(uint32(mode))
			c := newTestConn(t, k, initAnswers())
			err := k.SetFilterWidth(context.Background(), c, 2400)
			if err == nil {
				t.Fatalf("SetFilterWidth accepted %s; FW does not set a bandwidth there", mode)
			}
			if len(c.sent) != 0 {
				t.Errorf("wrote %q anyway", c.sent)
			}
		})
	}
}

func TestSetFilterSlot(t *testing.T) {
	for _, tt := range []struct {
		slot int
		want string
	}{
		{1, "FL1;"},
		{2, "FL2;"},
	} {
		k := newRig(t, 2, true)
		c := newTestConn(t, k, map[string]string{reqFL: "FL1"})
		if err := k.SetFilterSlot(context.Background(), c, tt.slot); err != nil {
			t.Fatalf("SetFilterSlot(%d): %v", tt.slot, err)
		}
		c.wantSent(t, tt.want, "FL;")
	}

	k := newRig(t, 2, true)
	c := newTestConn(t, k, initAnswers())
	for _, bad := range []int{0, 3, -1} {
		if err := k.SetFilterSlot(context.Background(), c, bad); err == nil {
			t.Errorf("accepted filter slot %d; the TS-590 has A and B", bad)
		}
	}
}

// TestRejectionsFailFast covers the three anonymous error answers. Waiting them
// out instead would cost a full timeout on a command the rig has already
// refused.
func TestRejectionsFailFast(t *testing.T) {
	tests := []struct {
		answer   string
		wantWord string
	}{
		{"?", "bad syntax"},
		{"E", "serial error"},
		{"O", "could not finish"},
	}
	for _, tt := range tests {
		t.Run(tt.answer, func(t *testing.T) {
			k := newRig(t, 2, true)
			c := newTestConn(t, k, map[string]string{reqFA: tt.answer})
			_, err := do(context.Background(), c, reqFA, keyFA)
			if err == nil {
				t.Fatalf("answer %q; was not treated as a rejection", tt.answer)
			}
			if !strings.Contains(err.Error(), tt.wantWord) {
				t.Errorf("error %q does not explain the rejection (want %q in it)", err, tt.wantWord)
			}
			if !strings.Contains(err.Error(), reqFA) {
				t.Errorf("error %q does not name the command that failed", err)
			}
		})
	}
}

// deafConn fails every write, standing in for a serial port that has gone away
// mid-command.
type deafConn struct{ err error }

func (c deafConn) Do(context.Context, []byte, ...backend.Key) (backend.Update, error) {
	return backend.Update{}, c.err
}
func (c deafConn) Send(context.Context, []byte) error { return c.err }

// TestWriteFailuresPropagate checks that a dead port surfaces as an error from
// every command rather than being swallowed somewhere in the set/read-back pair.
func TestWriteFailuresPropagate(t *testing.T) {
	ctx := context.Background()
	c := deafConn{err: errPortGone}

	tests := []struct {
		name string
		call func(*Rig) error
	}{
		{"Init", func(k *Rig) error { return k.Init(ctx, c) }},
		{"PollFast", func(k *Rig) error { return k.Poll(ctx, c, backend.PollFast) }},
		{"PollSlow", func(k *Rig) error { return k.Poll(ctx, c, backend.PollSlow) }},
		{"SetFrequency", func(k *Rig) error { return k.SetFrequency(ctx, c, radio.VFOA, 14_025_000) }},
		{"SetMode", func(k *Rig) error { return k.SetMode(ctx, c, radio.ModeUSB, true) }},
		{"SetPower", func(k *Rig) error { return k.SetPower(ctx, c, radio.PowerSet{Pct: ptrTo(50.0)}) }},
		{"SetPTT", func(k *Rig) error { return k.SetPTT(ctx, c, true) }},
		{"SetFilterSlot", func(k *Rig) error { return k.SetFilterSlot(ctx, c, 1) }},
		{"SetSpeed", func(k *Rig) error { return k.SetSpeed(ctx, c, 25) }},
		{"SendChunk", func(k *Rig) error { return k.SendChunk(ctx, c, "CQ") }},
		{"Abort", func(k *Rig) error { return k.Abort(ctx, c) }},
		{"BufferFree", func(k *Rig) error { _, _, err := k.BufferFree(ctx, c); return err }},
		{"SetFilterWidth", func(k *Rig) error {
			k.mode.Store(uint32(radio.ModeCW))
			return k.SetFilterWidth(ctx, c, 500)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			err := tt.call(k)
			if err == nil {
				t.Fatal("succeeded against a dead port")
			}
			if !strings.Contains(err.Error(), "kenwood") {
				t.Errorf("error %q is not attributed to this backend", err)
			}
		})
	}
}

// TestSetPowerReadBackFailure covers the second half of a set: the write lands
// but the confirming read does not.
func TestSetPowerReadBackFailure(t *testing.T) {
	k := newRig(t, 2, true)
	k.mode.Store(uint32(radio.ModeUSB))
	c := newTestConn(t, k, nil) // the set is accepted, PC; is never answered
	if err := k.SetPower(context.Background(), c, radio.PowerSet{Watts: ptrTo(50.0)}); err == nil {
		t.Fatal("SetPower succeeded without confirming what the rig took")
	}
	c.wantSent(t, "PC050;", "PC;")
}

// TestModelFallbacks covers a rig that answers ID; with something neither model
// uses, which a Kenwood-compatible clone might.
func TestModelFallbacks(t *testing.T) {
	k := newRig(t, 2, true)
	if k.Model() != "" {
		t.Errorf("Model = %q before Init with no configured model, want empty", k.Model())
	}

	k.model = "TS-590S"
	if k.Model() != "TS-590S" {
		t.Errorf("Model = %q, want the configured name before Init", k.Model())
	}

	mustDecode(t, k, "ID021")
	if k.Model() != "TS-590S" {
		t.Errorf("Model = %q after ID021, want TS-590S", k.Model())
	}

	mustDecode(t, k, "ID099")
	if got := k.Model(); got != "Kenwood ID 099" {
		t.Errorf("Model = %q for an unknown ID, want a legible fallback", got)
	}
}

func ptrTo[T any](v T) *T { return &v }

var errPortGone = errors.New("serial port closed")
