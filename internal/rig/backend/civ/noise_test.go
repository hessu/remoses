package civ

import (
	"context"
	"errors"
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// TestPollSurvivesARejectedRead is the IC-9700's bug, and it was never about
// the noise commands.
//
// That radio rejects 16 57 — the notch width — in FM, which is correct: FM has
// no use for a DSP notch. readAll stopped the whole tier at the first failure,
// so every request queued BEHIND it was skipped on every slow tick. The
// automatic notch sat two places back and was therefore never read at all, on a
// radio that reports it perfectly well in every other mode.
//
// A rejection means the radio is alive and said no. The reads after it are
// still worth making.
func TestPollSurvivesARejectedRead(t *testing.T) {
	r, s := modelRig(t, "ic-9700")
	// Refuse exactly the command the radio refuses, and nothing else.
	s.reject = map[string]bool{"16/57": true}

	err := r.Poll(context.Background(), s, backend.PollSlow)
	if err == nil {
		t.Fatal("Poll hid the rejection entirely; the session should still see one")
	}
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("Poll returned %v, want an ErrRejected the session treats as non-fatal", err)
	}

	// The point of the fix: the reads after the refused one still happened.
	// 16 41 is the automatic notch, which comes after the width.
	var sawAutoNotch bool
	for _, f := range s.log {
		if len(f) >= 6 && f[4] == cmdFunc && f[5] == subAutoNotch {
			sawAutoNotch = true
		}
	}
	if !sawAutoNotch {
		t.Error("the auto notch read never went out: one refused command starved " +
			"everything behind it in the tier")
	}
}

// TestPollStopsOnATransportFailure is the other half. A dead link is not a
// refusal, and carrying on would spend one full command timeout per remaining
// read before anybody noticed.
func TestPollStopsOnATransportFailure(t *testing.T) {
	r, s := modelRig(t, "ic-9700")
	s.transportErr = true

	if err := r.Poll(context.Background(), s, backend.PollSlow); err == nil {
		t.Fatal("Poll ignored a transport failure")
	}
	if len(s.log) != 1 {
		t.Errorf("%d requests went out after the link failed; want 1", len(s.log))
	}
}

// TestNotchWidthEncoding covers 16 57, whose bytes are an index rather than a
// bandwidth: "00=WIDE, 01=MID, 02=NAR".
func TestNotchWidthEncoding(t *testing.T) {
	r, s := modelRig(t, "ic-7610")
	for _, tt := range []struct {
		w    radio.NotchWidth
		want byte
	}{
		{radio.NotchWidthWide, 0x00},
		{radio.NotchWidthMid, 0x01},
		{radio.NotchWidthNarrow, 0x02},
	} {
		if err := r.SetNotchWidth(context.Background(), s, tt.w); err != nil {
			t.Fatalf("SetNotchWidth(%s): %v", tt.w, err)
		}
		if s.notchWidth != tt.want {
			t.Errorf("SetNotchWidth(%s) sent %#02x, want %#02x", tt.w, s.notchWidth, tt.want)
		}
		u, err := r.Decode(fromRig(cmdFunc, subNotchWide, s.notchWidth))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if u.Patch.NotchWidth == nil || *u.Patch.NotchWidth != tt.w {
			t.Errorf("%#02x decoded as %v, want %s", s.notchWidth, u.Patch.NotchWidth, tt.w)
		}
	}
	if err := r.SetNotchWidth(context.Background(), s, radio.NotchWidth("huge")); err == nil {
		t.Error("SetNotchWidth accepted a width that does not exist")
	}
}

// TestNotchesAreExclusiveOnIcom records what the radios do rather than what
// their references say.
//
// 16 41 and 16 48 are separate commands and no reference here mentions them
// interacting. An IC-7610 nonetheless switches one off whenever the other goes
// on — verified in both directions — so a client asking for both would be given
// one and told nothing about the other.
func TestNotchesAreExclusiveOnIcom(t *testing.T) {
	for _, model := range []string{"ic-7610", "ic-9700", "ic-7300"} {
		r, _ := modelRig(t, model)
		if !r.Caps().NotchExclusive {
			t.Errorf("%s reports notch_exclusive false; asking for both notches would "+
				"then be accepted and silently give one", model)
		}
	}
	// And a radio with only the automatic notch is not "exclusive": there is no
	// pair to be exclusive between, and claiming so would refuse a request that
	// is perfectly sensible.
	r, _ := modelRig(t, "ic-703")
	if r.Caps().NotchExclusive {
		t.Error("the IC-703 has no manual notch, so exclusivity is not a thing it has")
	}
}

// TestNoiseGroupIsPerModel: the older radios draw the line in three different
// places, which is why the model table has a flag per command rather than one
// for the group.
func TestNoiseGroupIsPerModel(t *testing.T) {
	for _, tt := range []struct {
		model                string
		nb, nbl, nr, nrl     bool
		notch, notchF, autoN bool
	}{
		// The modern shape: everything.
		{"ic-7610", true, true, true, true, true, true, true},
		// A blanker, a reducer and an AUTOMATIC notch with no manual one, and
		// no level for anything.
		{"ic-703", true, false, true, false, false, false, true},
		// The same, plus a reducer level: its 14 group carries 06.
		{"ic-718", true, false, true, true, false, false, true},
		// A blanker alone. Its 16 40 carries switch and level together, which
		// is not the family's form, so the reducer is not claimed.
		{"ic-910h", true, false, false, false, false, false, false},
		// And the IC-706 family: the blanker and nothing else.
		{"ic-706mkiig", true, false, false, false, false, false, false},
	} {
		r, _ := modelRig(t, tt.model)
		c := r.Caps()
		got := []bool{
			c.NoiseBlankerLevels > 0, c.NBLevelControl,
			c.NoiseReductionLevels > 0, c.NRLevelControl,
			c.NotchControl, c.NotchFreqControl, c.AutoNotchControl,
		}
		want := []bool{tt.nb, tt.nbl, tt.nr, tt.nrl, tt.notch, tt.notchF, tt.autoN}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: caps %v, want %v", tt.model, got, want)
				break
			}
		}
	}
}

// TestNoiseSettersRefuseWhereUnsupported: each has to answer ErrUnsupported
// rather than putting a command on the bus the radio has never heard of.
func TestNoiseSettersRefuseWhereUnsupported(t *testing.T) {
	r, s := modelRig(t, "ic-706mkiig") // the blanker and nothing else
	ctx := context.Background()
	for _, tc := range []struct {
		what string
		err  error
	}{
		{"nb level", r.SetNBLevel(ctx, s, 50)},
		{"noise reduction", r.SetNoiseReduction(ctx, s, 1)},
		{"nr level", r.SetNRLevel(ctx, s, 50)},
		{"notch", r.SetNotch(ctx, s, true)},
		{"notch freq", r.SetNotchFreq(ctx, s, 50)},
		{"notch width", r.SetNotchWidth(ctx, s, radio.NotchWidthWide)},
		{"auto notch", r.SetAutoNotch(ctx, s, true)},
	} {
		if tc.err == nil {
			t.Errorf("%s was accepted on an IC-706MKIIG", tc.what)
			continue
		}
		if !errors.Is(tc.err, backend.ErrUnsupported) {
			t.Errorf("%s refused with %v, which is not ErrUnsupported and would be a 500",
				tc.what, tc.err)
		}
	}
}
