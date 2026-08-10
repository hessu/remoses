package yaesu

import (
	"context"
	"testing"
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

// TestAntennaIsOnlyOnTheFTdx101: the one command list read for this backend
// with an AN row. Claiming it elsewhere would send a command the radio has
// never heard of.
func TestAntennaIsOnlyOnTheFTdx101(t *testing.T) {
	for _, tt := range []struct {
		model string
		want  int
	}{
		{"ftdx101d", 3},
		{"ftdx101mp", 3},
		{"ft-891", 0},
		{"ft-991a", 0},
		{"ftdx9000", 0},
		{"ft-950", 0},
	} {
		y := newModelRig(t, tt.model)
		if got := y.Caps().Antennas; got != tt.want {
			t.Errorf("%s antennas = %d, want %d", tt.model, got, tt.want)
		}
	}

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
	// And the receive-only input, which no command list here has.
	if err := y.SetRXAntenna(context.Background(), c, true); err == nil {
		t.Error("SetRXAntenna was accepted; no Yaesu read here has such a command")
	}
}
