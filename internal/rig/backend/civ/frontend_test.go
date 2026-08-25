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
		{"ic-7850", radio.AGCOff, 0x00}, // the same four, printed p. 18-4
		{"ic-7850", radio.AGCSlow, 0x03},
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
//
// The shift used to be denied to the IC-7700 and the IC-7850 here, and both
// their manuals print it — see Model.DigiSelShift for the rows. What is left
// per model is the pair against radios that have neither: the IC-7600's 14
// group runs 01 to 19 with no 13, and its 16 group 02 to 58 with no 4E.
func TestPreselectorIsPerModel(t *testing.T) {
	tests := []struct {
		model              string
		ipPlus, ds, dsShif bool
	}{
		{"ic-7610", true, true, true},
		{"ic-7760", true, true, true},
		{"ic-7300", true, false, false},
		{"ic-7700", false, true, true},   // 16 4E and 14 13, printed p. 14-4
		{"ic-7850", false, true, true},   // the same pair, printed p. 18-3 and 18-4
		{"ic-7600", false, false, false}, // neither row is in its table
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

// TestDigiSelShiftReachesTheWire is the half of that correction that belongs on
// the bus rather than in the capability set.
//
// 14 13 WRITES to a radio, and the model table denied it to the IC-7700 and the
// IC-7850: a client asking to move the preselector on either got ErrUnsupported
// and nothing went out. Both tables print "13 0000 to 0255 Send/read [DIGI-SEL]
// position (0000=max. CCW to 0255=max. CW)", which is the IC-7610's row word for
// word, so one encoder serves all four and the top of the range has to arrive as
// the two BCD bytes 02 55 rather than 0xFF.
func TestDigiSelShiftReachesTheWire(t *testing.T) {
	for _, model := range []string{"ic-7610", "ic-7760", "ic-7700", "ic-7850"} {
		r, s := modelRig(t, model)
		if err := r.SetDigiSelShift(context.Background(), s, 100); err != nil {
			t.Fatalf("%s SetDigiSelShift: %v", model, err)
		}
		if s.digiSelShift != [2]byte{0x02, 0x55} {
			t.Errorf("%s SetDigiSelShift(100%%) sent % X, want 02 55", model, s.digiSelShift)
		}
		// And the slow poll has to ask for it, or the published position would
		// only ever be what remoses last wrote — the control is a knob on the
		// front panel too.
		//
		// A rejection does not fail this: the simulator is an IC-7610 and the
		// four-socket radios have antenna rows it does not answer, which stops
		// nothing in the tier. The frame going out is the part being checked.
		s.log = nil
		if err := r.Poll(context.Background(), s, backend.PollSlow); err != nil &&
			!errors.Is(err, ErrRejected) {
			t.Fatalf("%s Poll: %v", model, err)
		}
		var asked bool
		for _, f := range s.log {
			if len(f) >= 6 && f[4] == cmdLevel && f[5] == subDigiSelShift {
				asked = true
			}
		}
		if !asked {
			t.Errorf("%s: the slow poll never read 14 13", model)
		}
	}

	// And the radio whose table has neither row still refuses both. The IC-7600
	// is a superhet of the same size and generation, which is exactly why this
	// is read per model: its 14 group runs 01 to 19 with no 13 in it.
	r, s := modelRig(t, "ic-7600")
	for _, tc := range []struct {
		what string
		err  error
	}{
		{"digi-sel", r.SetDigiSel(context.Background(), s, true)},
		{"digi-sel shift", r.SetDigiSelShift(context.Background(), s, 50)},
	} {
		if !errors.Is(tc.err, backend.ErrUnsupported) {
			t.Errorf("IC-7600 %s refused with %v, which is not ErrUnsupported", tc.what, tc.err)
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
//
// The IC-706MKIIG rather than the IC-910H, which is where this test used to
// look. That entry claimed the IC-910H had no command 14 at all, so it stood in
// for a radio with no RF gain — and its table plainly prints "14 02 [RF GAIN]
// level setting (0=max. CCW; 255=max. CW)" (printed p. 79). The MKIIG genuinely
// has no 14 at any sub-command, and a 16 group of eight rows with no
// preselector in it.
func TestFrontEndSettersRefuseWhereUnsupported(t *testing.T) {
	r, s := modelRig(t, "ic-706mkiig")
	ctx := context.Background()
	for _, tc := range []struct {
		what string
		err  error
	}{
		{"rf gain", r.SetRFGain(ctx, s, 50)},
		{"ip+", r.SetIPPlus(ctx, s, true)},
		{"digi-sel", r.SetDigiSel(ctx, s, true)},
		{"digi-sel shift", r.SetDigiSelShift(ctx, s, 50)},
		// And the AGC, which its table does list as "16 12 AGC setting" with no
		// data column: the byte values are the one thing in this group no two
		// references spell the same way, so the model claims nothing.
		{"agc", r.SetAGC(ctx, s, radio.AGCFast)},
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

// TestIC910HHasAnRFGain is the other half of that correction, and it is the
// whole of what the model entry got wrong.
//
// Its command table (§13, printed p. 79) carries a 14 group of 01, 02, 03, 04,
// 06, 09, 0A, 0B, 0C, 0E and 0F — which is also why that same entry could set
// Power from 14 0A and MaxWPM from 14 0C while claiming the group did not exist.
// 14 02 is the receiver's RF gain in the family's ordinary 0-255 field, so it
// needs no special case: setLevel writes it and the slow poll reads it back.
func TestIC910HHasAnRFGain(t *testing.T) {
	r, s := modelRig(t, "ic-910h")
	if !r.Caps().RFGainControl {
		t.Fatal("caps.rf_gain_control is false on a radio whose table prints 14 02")
	}
	if err := r.SetRFGain(context.Background(), s, 50); err != nil {
		t.Fatalf("SetRFGain: %v", err)
	}
	// 50% of 255 is 127.5, rounded up: the same encoding as every other level
	// in the group, because this radio spells 14 02 the same way the rest do.
	if s.rfGain != [2]byte{0x01, 0x28} {
		t.Errorf("SetRFGain(50%%) sent % X, want 01 28", s.rfGain)
	}
	u, err := r.Decode(fromRig(cmdLevel, subRFGain, s.rfGain[0], s.rfGain[1]))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Key != KeyRFGain {
		t.Errorf("a 14 02 answer keyed %q, want %q", u.Key, KeyRFGain)
	}
	if u.Patch.RFGain == nil || int(*u.Patch.RFGain+0.5) != 50 {
		t.Errorf("RF gain decoded as %v, want 50%%", u.Patch.RFGain)
	}
}
