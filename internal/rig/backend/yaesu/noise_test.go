package yaesu

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/rig/backend"
)

// TestBPCarriesTwoDifferentThings is the shape that makes this family's notch
// unlike the other two.
//
// BP is one command with a selector inside it: "P2 0: Manual NOTCH ON/OFF" with
// P3 000 or 001, and "P2 1: Manual NOTCH LEVEL" with P3 001-320, the notch
// frequency in tens of Hz. Both halves answer with the same two letters, so a
// decoder has to read P2 before it can know what P3 means — read one as the
// other and a 3200 Hz notch position decodes as "on", or a switch decodes as a
// position at the bottom of the range.
func TestBPCarriesTwoDifferentThings(t *testing.T) {
	y := newModelRig(t, "ftdx101d")

	// The switch half.
	for _, tt := range []struct {
		answer string
		want   bool
	}{
		{"BP00000", false},
		{"BP00001", true},
	} {
		u, err := y.Decode([]byte(tt.answer))
		if err != nil {
			t.Fatalf("Decode(%s): %v", tt.answer, err)
		}
		if u.Patch.Notch == nil || *u.Patch.Notch != tt.want {
			t.Errorf("%s decoded notch as %v, want %v", tt.answer, u.Patch.Notch, tt.want)
		}
		if u.Patch.NotchFreq != nil {
			t.Errorf("%s also published a notch position of %g; that half was not asked "+
				"about", tt.answer, *u.Patch.NotchFreq)
		}
	}

	// The position half. 001 is the bottom of the range and 320 the top, and
	// neither must be mistaken for the switch's 000/001.
	for _, tt := range []struct {
		answer string
		want   float64
	}{
		{"BP01001", 0},
		{"BP01320", 100},
	} {
		u, err := y.Decode([]byte(tt.answer))
		if err != nil {
			t.Fatalf("Decode(%s): %v", tt.answer, err)
		}
		if u.Patch.NotchFreq == nil {
			t.Fatalf("%s published no notch position", tt.answer)
		}
		if got := *u.Patch.NotchFreq; got < tt.want-0.5 || got > tt.want+0.5 {
			t.Errorf("%s decoded position as %g%%, want %g%%", tt.answer, got, tt.want)
		}
		if u.Patch.Notch != nil {
			t.Errorf("%s also published the switch as %v", tt.answer, *u.Patch.Notch)
		}
	}
}

// TestNotchWritesNameTheirHalf: the two setters have to carry the right P2, or
// each would write the other's field.
func TestNotchWritesNameTheirHalf(t *testing.T) {
	y := newModelRig(t, "ftdx101d")

	c := newTestConn(t, y, answersFor(y.profile))
	if err := y.SetNotch(context.Background(), c, true); err != nil {
		t.Fatalf("SetNotch: %v", err)
	}
	if c.sent[0] != "BP00001;" {
		t.Errorf("SetNotch(true) sent %q, want BP00001;", c.sent[0])
	}

	c = newTestConn(t, y, answersFor(y.profile))
	if err := y.SetNotchFreq(context.Background(), c, 100); err != nil {
		t.Fatalf("SetNotchFreq: %v", err)
	}
	if c.sent[0] != "BP01320;" {
		t.Errorf("SetNotchFreq(100) sent %q, want BP01320; — 320 tens of Hz is "+
			"3200 Hz, the top of the range", c.sent[0])
	}
}

// TestNoiseLevelRangesAreTheirOwn. Neither matches another family's, and
// neither matches the other: the blanker counts 000-010 and the reducer 01-15.
func TestNoiseLevelRangesAreTheirOwn(t *testing.T) {
	y := newModelRig(t, "ftdx101d")

	for _, tt := range []struct {
		pct  float64
		want string
	}{
		{0, "NL0000;"},
		{100, "NL0010;"},
		{50, "NL0005;"},
	} {
		c := newTestConn(t, y, answersFor(y.profile))
		if err := y.SetNBLevel(context.Background(), c, tt.pct); err != nil {
			t.Fatalf("SetNBLevel(%g): %v", tt.pct, err)
		}
		if c.sent[0] != tt.want {
			t.Errorf("SetNBLevel(%g) sent %q, want %q", tt.pct, c.sent[0], tt.want)
		}
	}

	for _, tt := range []struct {
		pct  float64
		want string
	}{
		{0, "RL001;"},
		{100, "RL015;"},
	} {
		c := newTestConn(t, y, answersFor(y.profile))
		if err := y.SetNRLevel(context.Background(), c, tt.pct); err != nil {
			t.Fatalf("SetNRLevel(%g): %v", tt.pct, err)
		}
		if c.sent[0] != tt.want {
			t.Errorf("SetNRLevel(%g) sent %q, want %q", tt.pct, c.sent[0], tt.want)
		}
	}
}

// TestNBLevelScaleIsPerModel is the wrong-ceiling test, and the ceiling is
// three different numbers across the twelve on a command whose frame never
// changes: NL is "000 - 010" on the FTdx101 generation, "000 - 100" on the
// FTdx1200 and FTdx3000, and "000 - 255" on the FT-950, FTdx5000 and FTdx9000.
//
// Against a family constant of 10, remoses could reach only the bottom 4% of an
// FT-950's blanker threshold and would publish the top of it as 4% — a
// confidently wrong number, which is the failure this backend is shaped around.
func TestNBLevelScaleIsPerModel(t *testing.T) {
	for _, tt := range []struct {
		model string
		full  string // what 100% goes out as
		read  string // an answer at the top of THIS radio's range
	}{
		{"ftdx101d", "NL0010;", "NL0010"},
		{"ft-991a", "NL0010;", "NL0010"},
		{"ft-891", "NL0010;", "NL0010"},
		{"ft-710", "NL0010;", "NL0010"},
		{"ftdx10", "NL0010;", "NL0010"},
		{"ftx-1", "NL0010;", "NL0010"},
		{"ftdx1200", "NL0100;", "NL0100"},
		{"ftdx3000", "NL0100;", "NL0100"},
		{"ft-950", "NL0255;", "NL0255"},
		{"ftdx5000", "NL0255;", "NL0255"},
		{"ftdx9000", "NL0255;", "NL0255"},
	} {
		y := newModelRig(t, tt.model)
		c := newTestConn(t, y, answersFor(y.profile))
		if err := y.SetNBLevel(context.Background(), c, 100); err != nil {
			t.Fatalf("%s: SetNBLevel(100): %v", tt.model, err)
		}
		if c.sent[0] != tt.full {
			t.Errorf("%s: SetNBLevel(100) sent %q, want %q", tt.model, c.sent[0], tt.full)
		}
		u := mustDecode(t, y, tt.read)
		if u.Patch.NBLevel == nil {
			t.Fatalf("%s: %s published no blanker level", tt.model, tt.read)
		}
		if got := *u.Patch.NBLevel; got < 99.5 {
			t.Errorf("%s: %s decoded as %.1f%%, want the top of the range", tt.model, tt.read, got)
		}
	}

	// And the crossing case, which is the whole point: 010 is full scale on an
	// FTdx101 and a twenty-fifth of an FT-950's blanker.
	u := mustDecode(t, newModelRig(t, "ft-950"), "NL0010")
	if u.Patch.NBLevel == nil || *u.Patch.NBLevel > 5 {
		t.Errorf("NL0010 on an FT-950 decoded as %v, want about 4%% of its 000-255 range",
			u.Patch.NBLevel)
	}
}

// TestNBWideIsASecondCircuit. The whole FT-950 generation prints a third NB
// value, "2: Noise Blanker (Wide) "ON"", where the FTdx101 generation stops at
// 1. Rejecting it on the way out and ignoring it on the way in left a rig
// sitting in wide-blanker mode reading back as having no blanker at all,
// because the previous value stayed in the cache.
func TestNBWideIsASecondCircuit(t *testing.T) {
	wide := map[string]bool{
		"ft-950": true, "ftdx1200": true, "ftdx3000": true,
		"ftdx5000": true, "ftdx9000": true,
	}
	for _, name := range ModelNames() {
		y := newModelRig(t, name)
		want := 1
		if wide[name] {
			want = 2
		}
		if y.profile.NBCircuits == 0 {
			want = 0 // the FTX-1, which has no NB row at all
		}
		if got := y.Caps().NoiseBlankerLevels; got != want {
			t.Errorf("%s noise_blanker_levels = %d, want %d", name, got, want)
		}

		c := newTestConn(t, y, answersFor(y.profile))
		err := y.SetNoiseBlanker(context.Background(), c, 2)
		if wide[name] {
			if err != nil {
				t.Errorf("%s refused the wide blanker its manual prints: %v", name, err)
			} else if c.sent[0] != "NB02;" {
				t.Errorf("%s: SetNoiseBlanker(2) sent %q, want NB02;", name, c.sent[0])
			}
		} else if err == nil {
			t.Errorf("%s accepted noise blanker 2; its NB table stops at 1", name)
		}

		// And the reading, which is where the stale value came from.
		u := mustDecode(t, y, "NB02")
		if wide[name] {
			if u.Patch.NoiseBlanker == nil || *u.Patch.NoiseBlanker != 2 {
				t.Errorf("%s: NB02 decoded as %v, want 2", name, u.Patch.NoiseBlanker)
			}
		} else if u.Patch.NoiseBlanker != nil {
			t.Errorf("%s: NB02 decoded as %d; its NB table has no such value",
				name, *u.Patch.NoiseBlanker)
		}
	}
}

// TestFTX1HasNoBlankerOrReducerSwitch is the running-cost half of that radio's
// profile. Its CAT Control Command List has no NB and no NR row — only NL and
// RL, each with an off value of its own — so a family-wide flag was sending it
// NB0; and NR0; on every slow tick, each answered with silence and each costing
// the session's full per-command timeout.
func TestFTX1HasNoBlankerOrReducerSwitch(t *testing.T) {
	y := newModelRig(t, "ftx-1")
	c := newTestConn(t, y, answersFor(y.profile))
	if err := y.Poll(context.Background(), c, backend.PollSlow); err != nil {
		t.Fatalf("PollSlow: %v", err)
	}
	for _, req := range c.sent {
		if strings.HasPrefix(req, "NB") || strings.HasPrefix(req, "NR") {
			t.Errorf("polled %q; the FTX-1's command list has no such row, so it answers "+
				"with silence and costs a full per-command timeout", req)
		}
	}
	// The levels ARE polled, because they are the only noise commands it has.
	var sawNL, sawRL bool
	for _, req := range c.sent {
		sawNL = sawNL || req == reqNL
		sawRL = sawRL || req == reqRL
	}
	if !sawNL || !sawRL {
		t.Errorf("polled %q; NL and RL are in the FTX-1's command list and are the only "+
			"noise commands it has", c.sent)
	}

	// Caps says so, and the switches refuse rather than write.
	caps := y.Caps()
	if caps.NoiseBlankerLevels != 0 || caps.NoiseReductionLevels != 0 {
		t.Errorf("ftx-1 claims %d blankers and %d reducers; it has no NB and no NR command",
			caps.NoiseBlankerLevels, caps.NoiseReductionLevels)
	}
	if !caps.NBLevelControl || !caps.NRLevelControl {
		t.Error("ftx-1 claims no noise levels; NL and RL are what it does have")
	}
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"SetNoiseBlanker", y.SetNoiseBlanker(context.Background(), c, 1)},
		{"SetNoiseReduction", y.SetNoiseReduction(context.Background(), c, 1)},
	} {
		if tt.err == nil {
			t.Errorf("%s was accepted on an FTX-1", tt.name)
			continue
		}
		if !strings.Contains(tt.err.Error(), "FTX-1") {
			t.Errorf("%s refusal %q does not name the radio", tt.name, tt.err)
		}
	}

	// And because each level carries its own documented "OFF", the reading
	// settles the switch as well: NL000 and RL000 are the blanker and the
	// reducer out of circuit, which no other command on that radio can report.
	for _, tt := range []struct {
		answer string
		on     bool
	}{
		{"NL0000", false},
		{"NL0005", true},
	} {
		u := mustDecode(t, y, tt.answer)
		if u.Patch.NoiseBlanker == nil {
			t.Fatalf("%s published no blanker switch; NL's 000 is its documented OFF", tt.answer)
		}
		if got := *u.Patch.NoiseBlanker != 0; got != tt.on {
			t.Errorf("%s decoded the blanker as on=%v, want %v", tt.answer, got, tt.on)
		}
	}
	for _, tt := range []struct {
		answer string
		on     bool
	}{
		{"RL000", false},
		{"RL005", true},
	} {
		u := mustDecode(t, y, tt.answer)
		if u.Patch.NoiseReduction == nil {
			t.Fatalf("%s published no reducer switch; RL's 00 is its documented OFF", tt.answer)
		}
		if got := *u.Patch.NoiseReduction != 0; got != tt.on {
			t.Errorf("%s decoded the reducer as on=%v, want %v", tt.answer, got, tt.on)
		}
	}
	// Its reducer ladder is ten steps, not the family's fifteen: "00: "OFF",
	// 01 -10" (FTX-1 CAT Operation Reference Manual, page 23).
	c = newTestConn(t, y, answersFor(y.profile))
	if err := y.SetNRLevel(context.Background(), c, 100); err != nil {
		t.Fatalf("SetNRLevel(100): %v", err)
	}
	if c.sent[0] != "RL010;" {
		t.Errorf("SetNRLevel(100) sent %q, want RL010; — its ladder stops at 10", c.sent[0])
	}
	// Only the FTX-1 lets the level speak for the switch, because only its
	// manual prints one. Everywhere else the switch is NB's and NR's alone.
	for _, name := range ModelNames() {
		if name == "ftx-1" {
			continue
		}
		other := newModelRig(t, name)
		if u := mustDecode(t, other, "NL0000"); u.Patch.NoiseBlanker != nil {
			t.Errorf("%s decoded a blanker switch out of NL; its 000 is the bottom of the "+
				"threshold range, not an off value", name)
		}
	}
}

// TestNotchFreqScaleIsPerModel. BP's position range is 001-300 on the FT-950
// and FTdx9000, 001-320 on the FTdx101 generation and the FT-991A and FT-891,
// and 001-400 on the FTdx1200, FTdx3000 and FTdx5000 — three ceilings that do
// not follow the generation boundary. 320 sent to an FT-950 is an out-of-range
// parameter answered with silence, and a 400 read against 320 would publish
// more than full scale.
func TestNotchFreqScaleIsPerModel(t *testing.T) {
	for _, tt := range []struct {
		model string
		top   int
	}{
		{"ft-950", 300},
		{"ftdx9000", 300},
		{"ftdx101d", 320},
		{"ftdx10", 320},
		{"ft-710", 320},
		{"ftx-1", 320},
		{"ft-991a", 320},
		{"ft-891", 320},
		{"ftdx1200", 400},
		{"ftdx3000", 400},
		{"ftdx5000", 400},
	} {
		y := newModelRig(t, tt.model)
		if got := y.profile.NotchFreqMax; got != tt.top {
			t.Errorf("%s NotchFreqMax = %d, want %d", tt.model, got, tt.top)
		}
		c := newTestConn(t, y, answersFor(y.profile))
		if err := y.SetNotchFreq(context.Background(), c, 100); err != nil {
			t.Fatalf("%s: SetNotchFreq(100): %v", tt.model, err)
		}
		want := fmt.Sprintf("BP01%03d;", tt.top)
		if y.profile.NotchShape == NotchCombined {
			want = fmt.Sprintf("BP%03d;", tt.top)
		}
		if c.sent[0] != want {
			t.Errorf("%s: SetNotchFreq(100) sent %q, want %q", tt.model, c.sent[0], want)
		}
	}
}

// TestFTdx9000NotchHasNoSelector is the shape half of the same command.
//
// Its BP is "B P P1 P1 P1 ;" — three digits, no receiver selector and no
// sub-command selector — read with a bare BP; and answered BP<nnn>, with "000:
// Manual NOTCH "OFF" / 001 - 300: NOTCH Frequency (x10 Hz)". So the two reads
// and the two writes every other model makes are all malformed there, and one
// answer settles both fields.
func TestFTdx9000NotchHasNoSelector(t *testing.T) {
	y := newModelRig(t, "ftdx9000")
	ctx := context.Background()

	// One read on the slow poll, not two, and it is the bare form.
	c := newTestConn(t, y, answersFor(y.profile))
	if err := y.Poll(ctx, c, backend.PollSlow); err != nil {
		t.Fatalf("PollSlow: %v", err)
	}
	var bp []string
	for _, req := range c.sent {
		if strings.HasPrefix(req, "BP") {
			bp = append(bp, req)
		}
	}
	if len(bp) != 1 || bp[0] != "BP;" {
		t.Errorf("polled %q for the notch, want exactly [\"BP;\"] — BP00; and BP01; carry a "+
			"sub-command selector this radio's BP does not have", bp)
	}

	// One answer settles the switch AND the position.
	u := mustDecode(t, y, "BP150")
	if u.Patch.Notch == nil || !*u.Patch.Notch {
		t.Errorf("BP150 decoded the notch switch as %v, want on — 001-300 is a position "+
			"and a position is an on", u.Patch.Notch)
	}
	if u.Patch.NotchFreq == nil {
		t.Fatal("BP150 published no notch position")
	}
	if got := *u.Patch.NotchFreq; got < 49 || got > 51 {
		t.Errorf("BP150 decoded as %.1f%%, want about 50%% of 001-300", got)
	}
	// And 000 is the switch out, with no position in it to publish.
	u = mustDecode(t, y, "BP000")
	if u.Patch.Notch == nil || *u.Patch.Notch {
		t.Errorf("BP000 decoded the notch switch as %v, want off", u.Patch.Notch)
	}
	if u.Patch.NotchFreq != nil {
		t.Errorf("BP000 published a position of %g%%; that answer carries none, and the last "+
			"known position is the one a client redrawing the control wants",
			*u.Patch.NotchFreq)
	}
	// A five-character argument is one of the other eleven radios' answers and
	// must not be read here.
	if u = mustDecode(t, y, "BP01150"); u.Patch.Notch != nil || u.Patch.NotchFreq != nil {
		t.Error("BP01150 was decoded on an FTdx9000; that is the two-parameter answer shape")
	}

	// Switching the notch out is the documented BP000;.
	c = newTestConn(t, y, answersFor(y.profile))
	if err := y.SetNotch(ctx, c, false); err != nil {
		t.Fatalf("SetNotch(false): %v", err)
	}
	if c.sent[0] != "BP000;" {
		t.Errorf("SetNotch(false) sent %q, want BP000;", c.sent[0])
	}

	// Switching it IN has to name a position, because the radio has no value
	// meaning "on, where it was". With one known it goes back there; with none
	// it refuses rather than inventing a frequency to park a filter on.
	fresh := newModelRig(t, "ftdx9000")
	c = newTestConn(t, fresh, answersFor(fresh.profile))
	err := fresh.SetNotch(ctx, c, true)
	if err == nil {
		t.Fatal("SetNotch(true) was accepted before any position had been read; there is no " +
			"parameter that means on without naming a frequency")
	}
	if !errors.Is(err, backend.ErrUnsupported) || !strings.Contains(err.Error(), "FTdx9000") {
		t.Errorf("refusal %q should wrap ErrUnsupported and name the radio", err)
	}
	if len(c.sent) != 0 {
		t.Errorf("wrote %q despite refusing", c.sent)
	}

	mustDecode(t, fresh, "BP150")
	c = newTestConn(t, fresh, answersFor(fresh.profile))
	if err := fresh.SetNotch(ctx, c, true); err != nil {
		t.Fatalf("SetNotch(true) after a reading: %v", err)
	}
	if c.sent[0] != "BP150;" {
		t.Errorf("SetNotch(true) sent %q, want BP150; — the position the rig itself last "+
			"reported", c.sent[0])
	}
}

// TestNotchesAreIndependentOnYaesu: BP and BC are separate commands with no
// documented interaction, and no Yaesu has been on the bench to say otherwise.
// So remoses does not claim the exclusivity the two Icoms and the Kenwood have.
func TestNotchesAreIndependentOnYaesu(t *testing.T) {
	y := newModelRig(t, "ftdx101d")
	if y.Caps().NotchExclusive {
		t.Error("notch_exclusive is claimed on a family where it has not been observed; " +
			"that would refuse a combination the radio may well accept")
	}
}

// TestAntennaSocketsPerModel: AN is not the FTdx101-only command this backend
// used to claim. Six of the twelve command lists have a row for it and they
// disagree about how many sockets it reaches, so the count is per model —
// claiming one the radio has not got sends a parameter it answers with silence,
// and claiming none loses a control it really has.
func TestAntennaSocketsPerModel(t *testing.T) {
	for _, tt := range []struct {
		model string
		want  int
	}{
		{"ftdx101d", 3},
		{"ftdx101mp", 3},
		{"ft-950", 2},
		{"ftdx1200", 2},
		{"ftdx3000", 3},
		{"ftdx5000", 4},
		{"ftdx9000", 4},
		// The five whose indexes genuinely have no AN row, checked in each.
		{"ft-891", 0},
		{"ft-991a", 0},
		{"ft-710", 0},
		{"ftdx10", 0},
		{"ftx-1", 0},
	} {
		y := newModelRig(t, tt.model)
		if got := y.Caps().Antennas; got != tt.want {
			t.Errorf("%s antennas = %d, want %d", tt.model, got, tt.want)
		}
	}

	// The set is the same six characters on all six radios that have one.
	y := newModelRig(t, "ftdx101d")
	c := newTestConn(t, y, answersFor(y.profile))
	if err := y.SetAntenna(context.Background(), c, 3); err != nil {
		t.Fatalf("SetAntenna(3): %v", err)
	}
	if c.sent[0] != "AN03;" {
		t.Errorf("SetAntenna(3) sent %q, want AN03;", c.sent[0])
	}
	if err := y.SetAntenna(context.Background(), c, 4); err == nil {
		t.Error("SetAntenna(4) was accepted on a radio with three sockets")
	}

	// A radio with no AN row must refuse rather than send one.
	y = newModelRig(t, "ftx-1")
	c = newTestConn(t, y, answersFor(y.profile))
	if err := y.SetAntenna(context.Background(), c, 1); err == nil {
		t.Error("SetAntenna was accepted on an FTX-1, whose command list has no AN row")
	}
	if len(c.sent) != 0 {
		t.Errorf("wrote %q anyway", c.sent)
	}
}

// TestAntennaAnswerShapes covers the three layouts, and in particular the two
// radios that report a receive antenna two incompatible ways.
//
// The FTdx5000 treats ANT RX as an overlay — its answer names a transmit socket
// AND says separately whether the receive input is in circuit. The FTdx9000
// treats it as the fifth position of one selector, so a 5 there means the
// answer is not naming a transmit socket at all and Antenna must be left alone
// rather than filled in with a number the radio never sent.
func TestAntennaAnswerShapes(t *testing.T) {
	type want struct {
		antenna int // 0 for "nothing published"
		rx      int // -1 nothing, 0 false, 1 true
	}
	for _, tt := range []struct {
		model  string
		answer string
		want   want
	}{
		// The plain shape: socket, then a documented fixed 0.
		{"ftdx101d", "AN020", want{2, -1}},
		{"ft-950", "AN020", want{2, -1}},
		{"ft-950", "AN030", want{0, -1}}, // two sockets: a 3 is not one of them
		// The FTdx5000's P4 is a real ANT RX flag.
		{"ftdx5000", "AN040", want{4, 0}},
		{"ftdx5000", "AN041", want{4, 1}},
		// The FTdx9000's fifth position IS the receive antenna.
		{"ftdx9000", "AN02", want{2, 0}},
		{"ftdx9000", "AN05", want{0, 1}},
		{"ftdx9000", "AN06", want{0, -1}},
	} {
		u := mustDecode(t, newModelRig(t, tt.model), tt.answer)
		got := 0
		if u.Patch.Antenna != nil {
			got = *u.Patch.Antenna
		}
		if got != tt.want.antenna {
			t.Errorf("%s: %s decoded antenna %d, want %d", tt.model, tt.answer, got, tt.want.antenna)
		}
		gotRX := -1
		if u.Patch.RXAntenna != nil {
			gotRX = 0
			if *u.Patch.RXAntenna {
				gotRX = 1
			}
		}
		if gotRX != tt.want.rx {
			t.Errorf("%s: %s decoded rx_antenna %d, want %d (-1 means nothing published)",
				tt.model, tt.answer, gotRX, tt.want.rx)
		}
	}
}

// TestRXAntennaIsReadButNotWritten. Two radios report a receive antenna and
// neither manual gives a value that switches one out: the FTdx5000's AN has one
// P2 value that reaches ANT RX and no companion that clears it, and the
// FTdx9000's fifth position is cleared only by naming a transmit antenna the
// answer does not report while ANT RX is selected. So the reading is published
// and the switch is not, on every model — which is a result rather than a gap,
// and the refusal has to name the radio so an operator can tell the two cases
// apart.
func TestRXAntennaIsReadButNotWritten(t *testing.T) {
	for _, name := range ModelNames() {
		y := newModelRig(t, name)
		if y.Caps().RXAntennaControl {
			t.Errorf("%s claims rx_antenna_control; no manual read here has a value that "+
				"switches a receive antenna out", name)
		}
		c := newTestConn(t, y, answersFor(y.profile))
		err := y.SetRXAntenna(context.Background(), c, true)
		if err == nil {
			t.Errorf("%s accepted SetRXAntenna", name)
			continue
		}
		if !strings.Contains(err.Error(), y.profile.Label) {
			t.Errorf("%s: refusal %q does not name the radio", name, err)
		}
		if len(c.sent) != 0 {
			t.Errorf("%s wrote %q despite refusing", name, c.sent)
		}
	}
	// The two that do report one say so in the refusal, because "this radio has
	// no receive antenna" and "this radio has one remoses will not write" are
	// different facts for an operator staring at a front panel that has the
	// button.
	for _, name := range []string{"ftdx5000", "ftdx9000"} {
		y := newModelRig(t, name)
		err := y.SetRXAntenna(context.Background(), newTestConn(t, y, nil), true)
		if err == nil || !strings.Contains(err.Error(), "reports its receive antenna") {
			t.Errorf("%s: refusal %q does not say the radio has one", name, err)
		}
	}
}
