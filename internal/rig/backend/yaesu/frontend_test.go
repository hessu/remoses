package yaesu

import (
	"context"
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

// TestAGCDoesNotRoundTrip is the interesting one in this family, and it is not a
// bug in either direction.
//
// GT ACCEPTS 0 to 4, where 4 is AUTO. It ANSWERS 0 to 6, where 4, 5 and 6 are
// auto having settled on fast, mid or slow. So a client that sets "auto" and
// reads back "auto-mid" has not failed to set anything — the radio is telling it
// what auto currently means. Flattening the three readings into "auto" would
// throw away the only report of what the AGC is actually doing.
func TestAGCDoesNotRoundTrip(t *testing.T) {
	y := newModelRig(t, "ftdx101d")

	for _, tt := range []struct {
		answer string
		want   radio.AGC
	}{
		{"GT00", radio.AGCOff},
		{"GT01", radio.AGCFast},
		{"GT02", radio.AGCMid},
		{"GT03", radio.AGCSlow},
		{"GT04", radio.AGCAutoFast},
		{"GT05", radio.AGCAutoMid},
		{"GT06", radio.AGCAutoSlow},
	} {
		u, err := y.Decode([]byte(tt.answer))
		if err != nil {
			t.Fatalf("Decode(%s): %v", tt.answer, err)
		}
		if u.Patch.AGC == nil || *u.Patch.AGC != tt.want {
			t.Errorf("%s decoded as %v, want %q", tt.answer, u.Patch.AGC, tt.want)
		}
	}

	// And the three auto readings are not settable: there is no way to tell a
	// radio "be automatic, and also be mid".
	for _, v := range []radio.AGC{radio.AGCAutoFast, radio.AGCAutoMid, radio.AGCAutoSlow} {
		if v.Settable() {
			t.Errorf("%q reports itself settable", v)
		}
		if err := y.SetAGC(context.Background(), newTestConn(t, y, answersFor(y.profile)), v); err == nil {
			t.Errorf("SetAGC(%s) was accepted", v)
		}
	}

	// Auto itself is settable, and goes out as 4 — the same byte that reads
	// back as auto-fast.
	c := newTestConn(t, y, answersFor(y.profile))
	if err := y.SetAGC(context.Background(), c, radio.AGCAuto); err != nil {
		t.Fatalf("SetAGC(auto): %v", err)
	}
	if c.sent[0] != "GT04;" {
		t.Errorf("SetAGC(auto) sent %q, want %q", c.sent[0], "GT04;")
	}
}

// TestFrontEndSetForms covers the wire forms, every one of which carries the
// fixed 0 that selects the main receiver — including on the two models with a
// real sub receiver, which remoses does not address.
func TestFrontEndSetForms(t *testing.T) {
	y := newModelRig(t, "ftdx101d")
	c := newTestConn(t, y, answersFor(y.profile))
	ctx := context.Background()

	if err := y.SetPreamp(ctx, c, 2); err != nil {
		t.Fatalf("SetPreamp: %v", err)
	}
	if err := y.SetAttenuator(ctx, c, 12); err != nil {
		t.Fatalf("SetAttenuator: %v", err)
	}
	if err := y.SetRFGain(ctx, c, 100); err != nil {
		t.Fatalf("SetRFGain: %v", err)
	}

	want := []string{"PA02;", "RA02;", "RG0255;"}
	var writes []string
	for _, s := range c.sent {
		if s != reqPA && s != reqRA && s != reqRG && s != reqGT {
			writes = append(writes, s)
		}
	}
	if len(writes) != len(want) {
		t.Fatalf("writes were %v, want %v", writes, want)
	}
	for i, w := range writes {
		if w != want[i] {
			t.Errorf("write %d was %q, want %q", i, w, want[i])
		}
	}
}

// TestAttenuatorLadderPerModel: three steps on the FTdx sets, one pad on the
// small radios, and none at all on the FTdx9000, whose command list has no RA.
func TestAttenuatorLadderPerModel(t *testing.T) {
	for _, tt := range []struct {
		model string
		steps []int
	}{
		{"ftdx101d", []int{6, 12, 18}},
		{"ft-950", []int{6, 12, 18}},
		{"ft-891", []int{12}},
		{"ft-991a", []int{12}},
		{"ftx-1", []int{12}},
		{"ftdx9000", nil},
	} {
		y := newModelRig(t, tt.model)
		got := y.Caps().AttenuatorDB
		if len(got) != len(tt.steps) {
			t.Errorf("%s attenuator %v, want %v", tt.model, got, tt.steps)
			continue
		}
		for i := range got {
			if got[i] != tt.steps[i] {
				t.Errorf("%s attenuator %v, want %v", tt.model, got, tt.steps)
				break
			}
		}
		// The FTdx9000 must refuse rather than send a command it has no row for.
		if len(tt.steps) == 0 {
			c := newTestConn(t, y, answersFor(y.profile))
			if err := y.SetAttenuator(context.Background(), c, 6); err == nil {
				t.Errorf("%s accepted an attenuator set with no RA command", tt.model)
			}
		}
	}
}

// TestPreampCountPerModel: two amplifiers past IPO on most of the family, one
// on the FT-891 ("0: IPO, 1: AMP") and one on the FTdx9000, whose PA is titled
// IPO Status and reads "0: IPO "ON" (Pre-Amp Disable), 1: IPO "OFF" (Pre-Amp
// Enable)" — the same two states named from the other end.
func TestPreampCountPerModel(t *testing.T) {
	for _, tt := range []struct {
		model string
		want  int
	}{
		{"ft-891", 1},
		{"ft-991a", 2},
		{"ftdx101d", 2},
		{"ftdx5000", 2},
		{"ftdx9000", 1},
	} {
		y := newModelRig(t, tt.model)
		if got := y.Caps().PreampLevels; got != tt.want {
			t.Errorf("%s preamp_levels = %d, want %d", tt.model, got, tt.want)
		}
	}
	// And the FT-891 refuses AMP 2, which it has not got.
	y := newModelRig(t, "ft-891")
	c := newTestConn(t, y, answersFor(y.profile))
	if err := y.SetPreamp(context.Background(), c, 2); err == nil {
		t.Error("FT-891 accepted preamp 2, which its PA table does not list")
	}
	// So does the FTdx9000, whose PA has the same two values.
	y = newModelRig(t, "ftdx9000")
	c = newTestConn(t, y, answersFor(y.profile))
	if err := y.SetPreamp(context.Background(), c, 2); err == nil {
		t.Error("FTdx9000 accepted preamp 2; its PA is a two-value IPO Status")
	}
}

// TestFTdx5000IPO2IsNotAThirdAmplifier. Its PA answers "0: IPO 1, 1: AMP 1, 2:
// AMP 2, 3: IPO 2" — four values, of which two are bypass paths. A 3 means the
// amplifier is out, so it publishes 0; discarding it, which is what a plain
// range check does, would leave the previous reading standing in the cache for
// as long as the operator sat in IPO 2.
func TestFTdx5000IPO2IsNotAThirdAmplifier(t *testing.T) {
	y := newModelRig(t, "ftdx5000")
	for _, tt := range []struct {
		answer string
		want   int
	}{
		{"PA00", 0}, // IPO 1
		{"PA01", 1}, // AMP 1
		{"PA02", 2}, // AMP 2
		{"PA03", 0}, // IPO 2 — a second bypass, not a third amplifier
	} {
		u := mustDecode(t, y, tt.answer)
		if u.Patch.Preamp == nil {
			t.Fatalf("%s published no preamplifier reading", tt.answer)
		}
		if got := *u.Patch.Preamp; got != tt.want {
			t.Errorf("%s decoded as preamp %d, want %d", tt.answer, got, tt.want)
		}
	}
	// The count Caps publishes is still two: remoses never writes a 3, because
	// the API has one "off" and no way to say which bypass a client is looking at.
	if got := y.Caps().PreampLevels; got != 2 {
		t.Errorf("ftdx5000 preamp_levels = %d, want 2", got)
	}
	c := newTestConn(t, y, answersFor(y.profile))
	if err := y.SetPreamp(context.Background(), c, 3); err == nil {
		t.Error("SetPreamp(3) was accepted; IPO 2 is not a preamplifier to select")
	}
	// And no other model decodes a 3, because no other manual has one.
	for _, name := range []string{"ftdx101d", "ft-991a", "ft-950"} {
		u := mustDecode(t, newModelRig(t, name), "PA03")
		if u.Patch.Preamp != nil {
			t.Errorf("%s decoded PA03 as preamp %d; its PA table stops at 2",
				name, *u.Patch.Preamp)
		}
	}
}

// TestRFGainScaleIsPerModel. RG is "000 - 255" on eleven of the twelve and "000
// - 030" on the FT-891, and the frames are identical — so RG0030 is 11.8% on an
// FT-991A and full gain on an FT-891, and a request for full gain sent as
// RG0255; to the FT-891 would be an out-of-range parameter answered with
// silence and a full per-command timeout.
func TestRFGainScaleIsPerModel(t *testing.T) {
	for _, tt := range []struct {
		model string
		set   string
		read  string
		pct   float64
	}{
		{"ft-891", "RG0030;", "RG0030", 100},
		{"ft-991a", "RG0255;", "RG0030", 100.0 * 30 / 255},
		{"ftdx9000", "RG0255;", "RG0255", 100},
	} {
		y := newModelRig(t, tt.model)
		c := newTestConn(t, y, answersFor(y.profile))
		if err := y.SetRFGain(context.Background(), c, 100); err != nil {
			t.Fatalf("%s: SetRFGain(100): %v", tt.model, err)
		}
		if c.sent[0] != tt.set {
			t.Errorf("%s: SetRFGain(100) sent %q, want %q", tt.model, c.sent[0], tt.set)
		}
		u := mustDecode(t, y, tt.read)
		if u.Patch.RFGain == nil {
			t.Fatalf("%s: %s published no RF gain", tt.model, tt.read)
		}
		if got := *u.Patch.RFGain; got < tt.pct-0.1 || got > tt.pct+0.1 {
			t.Errorf("%s: %s decoded as %.1f%%, want %.1f%%", tt.model, tt.read, got, tt.pct)
		}
	}
}
