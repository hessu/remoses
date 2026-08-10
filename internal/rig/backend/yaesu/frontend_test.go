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

// TestPreampCountPerModel: the FT-891 has one amplifier past IPO where the rest
// of the family has two.
func TestPreampCountPerModel(t *testing.T) {
	for _, tt := range []struct {
		model string
		want  int
	}{
		{"ft-891", 1},
		{"ft-991a", 2},
		{"ftdx101d", 2},
		{"ftdx9000", 2},
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
}
