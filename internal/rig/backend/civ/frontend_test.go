package civ

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// modelRig builds a backend for one named model against a simulator.
func modelRig(t *testing.T, model string) (*Rig, *simRig) {
	t.Helper()
	s := newSim(t)
	r, err := New(&config.Radio{
		ID:      "rig",
		Backend: "civ",
		CIV: &config.CIV{Model: model, RigAddress: int(s.backend.rigAddr),
			ControllerAddress: int(s.backend.ctrlAddr)},
	})
	if err != nil {
		t.Fatalf("New(%s): %v", model, err)
	}
	s.backend = r
	return r, s
}

// TestAttenuatorEncoding is the one byte in this whole feature that two
// references disagree about.
//
// Command 11 carries the attenuation as BCD — the IC-703's table says "00=OFF,
// 20=ON (20 dB)" in as many words — except on the IC-718, whose own table says
// "00=OFF (0dB), 01=20dB". Same opcode, same depth, different byte, and nothing
// in the frame to tell them apart. Getting it backwards would put 0x20 into a
// radio that wanted 0x01.
func TestAttenuatorEncoding(t *testing.T) {
	tests := []struct {
		model string
		db    int
		want  byte
	}{
		{"ic-703", 20, 0x20},  // BCD: the depth itself
		{"ic-718", 20, 0x01},  // an index into a one-entry ladder
		{"ic-7610", 12, 0x12}, // BCD again, and 12 dB is 0x12 not 12
		{"ic-7610", 45, 0x45},
		{"ic-7610", 0, 0x00},
	}
	for _, tt := range tests {
		r, s := modelRig(t, tt.model)
		if err := r.SetAttenuator(context.Background(), s, tt.db); err != nil {
			t.Fatalf("%s SetAttenuator(%d): %v", tt.model, tt.db, err)
		}
		if s.att != tt.want {
			t.Errorf("%s SetAttenuator(%d) put %#02x on the wire, want %#02x",
				tt.model, tt.db, s.att, tt.want)
		}
		// And it has to survive the round trip, or the poll would publish a
		// different depth from the one just set.
		if db, ok := r.attenuatorDB(s.att); !ok || db != tt.db {
			t.Errorf("%s decoded %#02x back as %d (ok %v), want %d",
				tt.model, s.att, db, ok, tt.db)
		}
	}
}

// TestAttenuatorRefusesAStepTheRadioLacks: the ladders really are different, and
// a step from one radio must not be sent to another.
func TestAttenuatorRefusesAStepTheRadioLacks(t *testing.T) {
	r, s := modelRig(t, "ic-7600") // 6, 12 and 18 only
	for _, db := range []int{3, 20, 45} {
		if err := r.SetAttenuator(context.Background(), s, db); err == nil {
			t.Errorf("SetAttenuator(%d) was accepted on an IC-7600, whose ladder is %v",
				db, r.model.Attenuator)
		}
	}
}

// TestAGCEncodingPerModel covers command 16 12, whose five known spellings share
// nothing but the opcode. One byte out and FAST becomes MID — a wrong setting
// that looks exactly like a right one.
func TestAGCEncodingPerModel(t *testing.T) {
	tests := []struct {
		model string
		v     radio.AGC
		want  byte
	}{
		{"ic-7610", radio.AGCFast, 0x01}, // 01 FAST, 02 MID, 03 SLOW
		{"ic-7610", radio.AGCSlow, 0x03},
		{"ic-7600", radio.AGCFast, 0x00}, // the same three, counted from 00
		{"ic-7600", radio.AGCSlow, 0x02},
		{"ic-7700", radio.AGCOff, 0x00}, // four values, 00 being OFF
		{"ic-7700", radio.AGCFast, 0x01},
		{"ic-703", radio.AGCFast, 0x01}, // two values: 1 fast, 2 slow
		{"ic-703", radio.AGCSlow, 0x02},
		{"ic-910h", radio.AGCSlow, 0x00}, // two values the other way round
		{"ic-910h", radio.AGCFast, 0x01},
	}
	for _, tt := range tests {
		r, s := modelRig(t, tt.model)
		if err := r.SetAGC(context.Background(), s, tt.v); err != nil {
			t.Fatalf("%s SetAGC(%s): %v", tt.model, tt.v, err)
		}
		if s.agc != tt.want {
			t.Errorf("%s SetAGC(%s) sent %#02x, want %#02x", tt.model, tt.v, s.agc, tt.want)
		}
		if got, ok := agcValue(r.model.AGC, s.agc); !ok || got != tt.v {
			t.Errorf("%s decoded %#02x as %q, want %q", tt.model, s.agc, got, tt.v)
		}
	}
}

// TestAGCRefusesASpeedTheRadioLacks: an IC-703 has no middle speed and no off.
func TestAGCRefusesASpeedTheRadioLacks(t *testing.T) {
	r, s := modelRig(t, "ic-703")
	for _, v := range []radio.AGC{radio.AGCMid, radio.AGCOff, radio.AGCAuto} {
		if err := r.SetAGC(context.Background(), s, v); err == nil {
			t.Errorf("SetAGC(%s) was accepted on an IC-703, which has fast and slow", v)
		}
	}
}

// TestPreampCountsAmplifiersNotValues is the IC-9700's 16 02, which runs 00 to
// 03 without being a gain ladder: 02 is "internal off, external on". Publishing
// it as a third level would tell a client that 03 is more gain than 02.
func TestPreampCountsAmplifiersNotValues(t *testing.T) {
	r, s := modelRig(t, "ic-9700")
	if got := r.Caps().PreampLevels; got != 1 {
		t.Errorf("IC-9700 preamp_levels = %d, want 1: its 02 and 03 are the external "+
			"preamp in combination, not further stages", got)
	}
	if err := r.SetPreamp(context.Background(), s, 2); err == nil {
		t.Error("SetPreamp(2) was accepted on an IC-9700")
	}
	// And a reading of 02 publishes nothing rather than a level that does not
	// mean what a client would take it to mean.
	u, err := r.Decode(fromRig(cmdFunc, subPreamp, 0x02))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Key != KeyPreamp {
		t.Errorf("a 16 02 answer keyed %q, want %q — an unmatched read fails the poll",
			u.Key, KeyPreamp)
	}
	if u.Patch.Preamp != nil {
		t.Errorf("preamp published as %d from a value that is the external preamp",
			*u.Patch.Preamp)
	}
}

// TestUndecodableFrontEndReadStillCompletes is the rule that keeps a poll alive.
//
// An answer this backend cannot make sense of must still resolve the request it
// belongs to. Otherwise the read fails, the failures accumulate on the fast
// tier's budget, and the session tears down a link to a radio that is answering
// perfectly well — which is how a wrong value becomes a dropped connection.
func TestUndecodableFrontEndReadStillCompletes(t *testing.T) {
	r, _ := modelRig(t, "ic-910h") // one 20 dB pad
	// 10 dB: in the IC-910H's own table, and not a step remoses offers.
	u, err := r.Decode(fromRig(cmdAttenuator, 0x10))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Key != KeyAttenuator {
		t.Fatalf("an out-of-ladder attenuator answer keyed %q, want %q", u.Key, KeyAttenuator)
	}
	if u.Patch.AttenuatorDB != nil {
		t.Errorf("published %d dB from a reading outside the documented ladder",
			*u.Patch.AttenuatorDB)
	}
}

// TestFrontEndLevelsRoundTrip covers the two command 14 levels, where the
// rounding matters: 0-255 over 0-100% is 2.55 counts a point, so truncating
// would leave 100% unreachable at 254.
func TestFrontEndLevelsRoundTrip(t *testing.T) {
	r, s := modelRig(t, "ic-7610")
	for _, pct := range []float64{0, 50, 100} {
		if err := r.SetRFGain(context.Background(), s, pct); err != nil {
			t.Fatalf("SetRFGain(%g): %v", pct, err)
		}
		u, err := r.Decode(fromRig(cmdLevel, subRFGain, s.rfGain[0], s.rfGain[1]))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if u.Patch.RFGain == nil {
			t.Fatalf("no RF gain decoded from % X", s.rfGain)
		}
		if got := *u.Patch.RFGain; got < pct-0.5 || got > pct+0.5 {
			t.Errorf("RF gain %g%% came back as %g%%", pct, got)
		}
	}
	if err := r.SetRFGain(context.Background(), s, 101); err == nil {
		t.Error("SetRFGain(101) was accepted")
	}
}

// TestAGCRefusalInFMSaysWhy covers the second interlock this feature found on
// hardware. An IC-9700 sets all three speeds in USB and answers NG to every one
// of them in FM — while still ANSWERING a read with "fast", so the state looks
// perfectly healthy and only the refusal says anything is different.
func TestAGCRefusalInFMSaysWhy(t *testing.T) {
	r, s := modelRig(t, "ic-9700")
	r.mode.Store(uint32(radio.ModeFM))
	s.nak = true

	err := r.SetAGC(context.Background(), s, radio.AGCMid)
	if err == nil {
		t.Fatal("SetAGC succeeded against a rig answering NG")
	}
	if !strings.Contains(err.Error(), "FM") {
		t.Errorf("refusal was %q, which does not mention the mode it was refused in", err)
	}

	// In any other mode the same refusal must not blame FM, or the message
	// would send somebody looking at the wrong thing.
	r.mode.Store(uint32(radio.ModeUSB))
	err = r.SetAGC(context.Background(), s, radio.AGCMid)
	if err == nil {
		t.Fatal("SetAGC succeeded against a rig answering NG")
	}
	if strings.Contains(err.Error(), "fixed in FM") {
		t.Errorf("a refusal in USB blamed FM: %q", err)
	}
}

// TestPreselectorIsPerModel: IP+ belongs to the direct-sampling sets and
// DIGI-SEL to the big superhets and the IC-7610, and neither is family-wide.
func TestPreselectorIsPerModel(t *testing.T) {
	tests := []struct {
		model              string
		ipPlus, ds, dsShif bool
	}{
		{"ic-7610", true, true, true},
		{"ic-7760", true, true, true},
		{"ic-7300", true, false, false},
		{"ic-7700", false, true, false}, // DIGI-SEL, and no command to move it
		{"ic-7850", false, true, false},
		{"ic-7600", false, false, false},
		{"ic-718", false, false, false},
	}
	for _, tt := range tests {
		r, _ := modelRig(t, tt.model)
		c := r.Caps()
		if c.IPPlusControl != tt.ipPlus || c.DigiSelControl != tt.ds ||
			c.DigiSelShiftControl != tt.dsShif {
			t.Errorf("%s: ip+=%v digi_sel=%v shift=%v, want %v/%v/%v",
				tt.model, c.IPPlusControl, c.DigiSelControl, c.DigiSelShiftControl,
				tt.ipPlus, tt.ds, tt.dsShif)
		}
	}
}

// TestPreampRefusalNamesDigiSel covers the interlock this feature found on the
// air: an IC-7610 with DIGI-SEL engaged answers NG to 16 02 01, and says nothing
// about why. The bare rejection is true and useless; the operator needs to know
// which other control is in the way.
func TestPreampRefusalNamesDigiSel(t *testing.T) {
	r, s := modelRig(t, "ic-7610")
	r.digiSel.Store(true)
	s.nak = true

	err := r.SetPreamp(context.Background(), s, 1)
	if err == nil {
		t.Fatal("SetPreamp succeeded against a rig answering NG")
	}
	if !strings.Contains(err.Error(), "DIGI-SEL") {
		t.Errorf("refusal was %q, which does not mention the control in the way", err)
	}

	// Switching one OFF is accepted by the radio even then, so that refusal —
	// if it happens — must not blame the preselector.
	r.digiSel.Store(true)
	err = r.SetPreamp(context.Background(), s, 0)
	if err != nil && strings.Contains(err.Error(), "DIGI-SEL") {
		t.Errorf("switching the preamp off blamed DIGI-SEL: %q", err)
	}
}

// TestFrontEndCapsMatchTheModelTable guards the other direction: a radio must
// not advertise a control its model table does not describe.
func TestFrontEndCapsMatchTheModelTable(t *testing.T) {
	for _, name := range ModelNames() {
		r, _ := modelRig(t, name)
		m, c := r.Model(), r.Caps()
		if c.PreampLevels != m.Preamp {
			t.Errorf("%s: caps preamp %d, model %d", name, c.PreampLevels, m.Preamp)
		}
		if len(c.AttenuatorDB) != len(m.Attenuator) {
			t.Errorf("%s: caps attenuator %v, model %v", name, c.AttenuatorDB, m.Attenuator)
		}
		if c.RFGainControl != m.RFGain {
			t.Errorf("%s: caps rf_gain %v, model %v", name, c.RFGainControl, m.RFGain)
		}
		if got, want := len(c.AGCSettings), len(m.AGC); got != want {
			t.Errorf("%s: caps agc %v, model has %d", name, c.AGCSettings, want)
		}
		// A radio with no ladder must refuse every depth, including its own 0:
		// there is nothing to switch out.
		if !c.AttenuatorControl() {
			if err := r.SetAttenuator(context.Background(), newSim(t), 0); err == nil {
				t.Errorf("%s: SetAttenuator succeeded with no attenuator in the table", name)
			}
		}
	}
}

// TestFrontEndSettersRefuseWhereUnsupported: every setter has to answer
// ErrUnsupported rather than putting a command on the bus that the radio has
// never heard of.
func TestFrontEndSettersRefuseWhereUnsupported(t *testing.T) {
	r, s := modelRig(t, "ic-910h") // no 14 group at all, no preselector
	ctx := context.Background()
	for _, tc := range []struct {
		what string
		err  error
	}{
		{"rf gain", r.SetRFGain(ctx, s, 50)},
		{"ip+", r.SetIPPlus(ctx, s, true)},
		{"digi-sel", r.SetDigiSel(ctx, s, true)},
		{"digi-sel shift", r.SetDigiSelShift(ctx, s, 50)},
	} {
		if tc.err == nil {
			t.Errorf("%s was accepted on an IC-910H", tc.what)
			continue
		}
		if !errors.Is(tc.err, backend.ErrUnsupported) {
			t.Errorf("%s refused with %v, which is not ErrUnsupported and would be a 500",
				tc.what, tc.err)
		}
	}
}
