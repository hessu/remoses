package civ

import (
	"context"
	"errors"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// singleVFORig is a model without the 25/26 family — every Icom here but the
// IC-7610, since that is the only reference remoses has read for them.
func singleVFORig(t *testing.T) *Rig {
	t.Helper()
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-7300"}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestReadVFOsReadsBothWithoutSelecting is the property the whole feature rests
// on. Commands 03 and 04 read "the operating VFO", so reaching the other one
// with them would mean selecting it — changing what the operator is using and
// racing the front panel. 25 and 26 name the VFO in the frame instead, and this
// asserts that no selection command goes out.
func TestReadVFOsReadsBothWithoutSelecting(t *testing.T) {
	s := newSim(t)
	if err := s.backend.ReadVFOs(context.Background(), s); err != nil {
		t.Fatalf("ReadVFOs: %v", err)
	}
	for _, req := range s.log {
		if cmd, body := req[4], req[5:len(req)-1]; cmd == cmdVFO && len(body) == 1 {
			if body[0] == subSelectMain || body[0] == subSelectSub {
				t.Fatalf("ReadVFOs sent a band selection (07 %02X); reading must not "+
					"change which VFO the operator is on", body[0])
			}
		}
	}
}

func TestReadVFOsDecodesBoth(t *testing.T) {
	s := newSim(t)
	// VFO B is on 28.350 USB/FIL1 in the simulator, nowhere near VFO A, so a
	// decoder that mixed the bands up gives an obviously wrong answer.
	if err := s.backend.ReadVFOs(context.Background(), s); err != nil {
		t.Fatalf("ReadVFOs: %v", err)
	}
	a := s.backend.vfoSnapshot(radio.VFOA)
	b := s.backend.vfoSnapshot(radio.VFOB)
	if a.Frequency != 14_025_000 || a.Mode != radio.ModeCW || a.FilterSlot != 2 {
		t.Errorf("VFO A = %+v, want 14.025 MHz CW FIL2", a)
	}
	if b.Frequency != 28_350_000 || b.Mode != radio.ModeUSB || b.FilterSlot != 1 {
		t.Errorf("VFO B = %+v, want 28.350 MHz USB FIL1", b)
	}
}

// TestVFOFieldsMergeAcrossCommands covers the reason the backend keeps a
// snapshot per VFO: 25 answers a frequency and 26 answers a mode, but a Patch
// carries a whole VFOState, so each decode has to merge rather than blank what
// the other one said.
func TestVFOFieldsMergeAcrossCommands(t *testing.T) {
	r := testRig(t)

	// A frequency arrives first, with nothing known about the mode.
	u, err := r.Decode(fromRig(cmdBandFreq, bandSub, 0x00, 0x00, 0x35, 0x28, 0x00))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Patch.VFOB == nil || u.Patch.VFOB.Frequency != 28_350_000 {
		t.Fatalf("frequency decode gave %+v", u.Patch.VFOB)
	}

	// Then the mode. The frequency must survive it.
	u, err = r.Decode(fromRig(cmdBandMode, bandSub, 0x01, 0x01, 0x02)) // USB, DATA1, FIL2
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := u.Patch.VFOB
	if got == nil {
		t.Fatal("mode decode published no VFO B")
	}
	if got.Frequency != 28_350_000 {
		t.Errorf("frequency = %d after a mode frame; the merge dropped it", got.Frequency)
	}
	if got.Mode != radio.ModeUSB || !got.DataMode || got.FilterSlot != 2 {
		t.Errorf("VFO B = %+v, want USB with data on FIL2", *got)
	}
	// And VFO A is untouched, which is the point of naming the band.
	if u.Patch.VFOA != nil {
		t.Error("a VFO B frame published VFO A")
	}
}

func TestSetVFOFrequency(t *testing.T) {
	s := newSim(t)
	if err := s.backend.SetVFOFrequency(context.Background(), s, radio.VFOB, 21_074_000); err != nil {
		t.Fatalf("SetVFOFrequency: %v", err)
	}
	if got := s.bandFreq[bandSub]; got != [5]byte{0x00, 0x40, 0x07, 0x21, 0x00} {
		t.Errorf("VFO B frequency = % X, want 21.074 MHz", got)
	}
	if s.bandFreq[bandMain] != [5]byte{0x00, 0x50, 0x02, 0x14, 0x00} {
		t.Error("setting VFO B moved VFO A")
	}
}

// TestSetVFOModeIsAtomic is why command 26 is worth using even for the
// operating VFO. Mode, data mode and filter travel in one frame, so none of
// them can disturb the others — which is exactly the collision that made
// SetFilterSlot clear data mode and SetMode move the filter on the single-VFO
// path.
func TestSetVFOModeIsAtomic(t *testing.T) {
	s := newSim(t)
	ctx := context.Background()

	if err := s.backend.SetVFOMode(ctx, s, radio.VFOA, radio.ModeUSB, true, 3); err != nil {
		t.Fatalf("SetVFOMode: %v", err)
	}
	if s.bandMode[bandMain] != 0x01 || s.bandData[bandMain] != 0x01 || s.bandFilter[bandMain] != 0x03 {
		t.Errorf("VFO A = mode %02X data %02X filter %02X, want 01/01/03",
			s.bandMode[bandMain], s.bandData[bandMain], s.bandFilter[bandMain])
	}
	// One frame, so there is no window in which the radio has the new mode and
	// the old filter.
	if got := s.requests(); len(got) != 1 || got[0] != "26" {
		t.Errorf("sent %v, want a single 26", got)
	}
}

// TestSetVFOModeKeepsTheFilterWhenNotAsked covers slot 0. Command 26 has no
// "leave the filter alone" encoding — the reference is explicit that omitting
// the data and filter bytes selects DATA OFF and the mode's default filter — so
// the current filter has to be read and sent back.
func TestSetVFOModeKeepsTheFilterWhenNotAsked(t *testing.T) {
	s := newSim(t)
	s.bandFilter[bandSub] = 0x03

	if err := s.backend.SetVFOMode(context.Background(), s, radio.VFOB, radio.ModeCW, false, 0); err != nil {
		t.Fatalf("SetVFOMode: %v", err)
	}
	if s.bandFilter[bandSub] != 0x03 {
		t.Errorf("filter = %02X, want FIL3 kept; a short frame would have reset it to FIL1",
			s.bandFilter[bandSub])
	}
	if got := s.requests(); len(got) != 2 || got[0] != "26" || got[1] != "26" {
		t.Errorf("sent %v, want a read then a set", got)
	}
}

func TestSplitRoundTrip(t *testing.T) {
	s := newSim(t)
	ctx := context.Background()

	if err := s.backend.SetSplit(ctx, s, true); err != nil {
		t.Fatalf("SetSplit: %v", err)
	}
	if s.split != 0x01 {
		t.Fatalf("rig split = %02X, want 01", s.split)
	}
	u, err := s.backend.Decode(fromRig(cmdSplit, 0x01))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Key != KeySplit || u.Patch.Split == nil || !*u.Patch.Split {
		t.Errorf("split decode gave key %q patch %+v", u.Key, u.Patch.Split)
	}
}

// TestDualWatchUsesSeparateSetAndRead covers the asymmetry in command 07: C0
// and C1 are the set forms and C2 is the read, so this cannot be written the
// way most settings in this protocol are.
func TestDualWatchUsesSeparateSetAndRead(t *testing.T) {
	s := newSim(t)
	ctx := context.Background()

	if err := s.backend.SetDualWatch(ctx, s, true); err != nil {
		t.Fatalf("SetDualWatch: %v", err)
	}
	if s.dualWatch != 0x01 {
		t.Fatalf("rig dual watch = %02X, want 01", s.dualWatch)
	}
	sent := s.log[len(s.log)-1]
	if body := sent[5 : len(sent)-1]; len(body) != 1 || body[0] != subDualWatchOn {
		t.Errorf("set frame body = % X, want C1", body)
	}

	if err := s.backend.SetDualWatch(ctx, s, false); err != nil {
		t.Fatalf("SetDualWatch(false): %v", err)
	}
	if s.dualWatch != 0x00 {
		t.Errorf("rig dual watch = %02X, want 00", s.dualWatch)
	}
}

// TestSubSMeterOnlyWhileDualWatch is the same rule the yaesubin backend applies
// to a transmit meter in receive: a reading from a receiver that is not running
// would sit in the cache looking live.
func TestSubSMeterOnlyWhileDualWatch(t *testing.T) {
	s := newSim(t)
	ctx := context.Background()

	// Dual watch off: the fast poll must not ask.
	if err := s.backend.Poll(ctx, s, backend.PollFast); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	for _, req := range s.requests() {
		if req == "29" {
			t.Fatal("the sub S-meter was polled with dual watch off")
		}
	}

	// Turn it on and the reading joins the fast tier.
	s.dualWatch = 0x01
	if err := s.backend.ReadVFOs(ctx, s); err != nil { // settles the flag
		t.Fatalf("ReadVFOs: %v", err)
	}
	s.log = nil
	if err := s.backend.Poll(ctx, s, backend.PollFast); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	found := false
	for _, req := range s.requests() {
		if req == "29" {
			found = true
		}
	}
	if !found {
		t.Error("the sub S-meter was not polled with dual watch on")
	}
}

// TestSubSMeterDecodesBehindThePrefix covers the 29 wrapper, and that the two
// receivers' meters do not collide: the sub reading must not satisfy a pending
// read of the main one, or a client's main meter would show the other receiver.
func TestSubSMeterDecodesBehindThePrefix(t *testing.T) {
	r := testRig(t)

	u, err := r.Decode(fromRig(cmdBand, bandSub, cmdMeter, subSMeter, 0x00, 0x21))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Key != KeySubSMeter {
		t.Fatalf("key = %q, want %q", u.Key, KeySubSMeter)
	}
	if u.Patch.SubSMeter == nil || u.Patch.SubSMeter.Raw != 21 {
		t.Errorf("sub meter = %+v, want raw 21", u.Patch.SubSMeter)
	}
	if u.Patch.SMeter != nil {
		t.Error("a sub-band reading was published as the main S-meter")
	}
}

// TestDualVFORefusedWithoutTheCommands is the honest refusal. A radio whose
// reference remoses has not read for 25/26 must not have them sent to it, and
// must say so as a 422 rather than a 500.
func TestDualVFORefusedWithoutTheCommands(t *testing.T) {
	r := singleVFORig(t)
	ctx := context.Background()
	var c captureConn

	for name, err := range map[string]error{
		"ReadVFOs":        r.ReadVFOs(ctx, &c),
		"SetVFOFrequency": r.SetVFOFrequency(ctx, &c, radio.VFOB, 14_074_000),
		"SetVFOMode":      r.SetVFOMode(ctx, &c, radio.VFOB, radio.ModeUSB, false, 1),
		"SetSplit":        r.SetSplit(ctx, &c, true),
		"SetDualWatch":    r.SetDualWatch(ctx, &c, true),
	} {
		if err == nil {
			t.Errorf("%s succeeded on a radio without the commands", name)
			continue
		}
		if !errors.Is(err, backend.ErrUnsupported) {
			t.Errorf("%s refused with %v, which the API reports as a 500", name, err)
		}
	}
	if len(c.sent) != 0 {
		t.Errorf("%d frames reached a radio that has none of these commands", len(c.sent))
	}
}

// TestCapsFollowTheModel keeps the capability list honest in both directions:
// the IC-7610 advertises the dual-VFO controls and an IC-7300 advertises none
// of them, so a client can tell before it asks.
func TestCapsFollowTheModel(t *testing.T) {
	dual := testRig(t).Caps()
	if !dual.SubReceiver || !dual.Split || !dual.DualWatch || !dual.PerVFOMode {
		t.Errorf("IC-7610 caps = %+v, want the dual-VFO controls advertised", dual)
	}
	if len(dual.VFOs) != 3 {
		t.Errorf("IC-7610 VFOs = %v, want current, A and B", dual.VFOs)
	}

	single := singleVFORig(t).Caps()
	if single.SubReceiver || single.Split || single.DualWatch || single.PerVFOMode {
		t.Errorf("IC-7300 caps = %+v, want none of the dual-VFO controls", single)
	}
	if len(single.VFOs) != 1 || single.VFOs[0] != radio.VFOCurrent {
		t.Errorf("IC-7300 VFOs = %v, want only the operating one", single.VFOs)
	}
}

// ic9700 builds the other dual-VFO profile: same commands, different meaning.
//
// Addressed at the simulator's bus address rather than its own factory 0xA2, so
// that a test can drive it through simRig. Which address it uses is settled
// separately, by TestConfigDefaultsDoNotOverrideTheModelAddress.
func ic9700(t *testing.T) *Rig {
	t.Helper()
	r, err := New(&config.Radio{CIV: &config.CIV{
		Model:      "ic-9700",
		RigAddress: int(DefaultRigAddress),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestTheTwoDualVFORadiosDisagreeAboutTheSelector is the whole reason the
// selector is a per-model field.
//
// 25 and 26 exist on both radios and mean different things: the IC-7610's byte
// picks the main or sub *band*, two fixed receivers, while the IC-9700's picks
// the *selected or unselected VFO* of the main band and cannot reach its sub
// band at all. One opcode, two axes — the IC-718's 1C 01 shape. A client is
// told which it is looking at through caps rather than left to infer it.
func TestTheTwoDualVFORadiosDisagreeAboutTheSelector(t *testing.T) {
	c7610 := testRig(t).Caps()
	if c7610.VFOAddressing != "named" {
		t.Errorf("IC-7610 vfo_addressing = %q, want named: its A and B are fixed bands",
			c7610.VFOAddressing)
	}
	c9700 := ic9700(t).Caps()
	if c9700.VFOAddressing != "relative" {
		t.Errorf("IC-9700 vfo_addressing = %q, want relative: nothing reports which "+
			"VFO is selected, so A and B cannot be stable labels", c9700.VFOAddressing)
	}
}

// TestIC9700SubReceiverIsNotRead is the instruction this profile encodes: the
// radio has a sub band, remoses cannot address it, and the one route that
// exists — 07 D1, select the sub band — moves the operator's own focus.
//
// So the capability says "there is one and I cannot read it", and nothing in
// the poll goes looking.
func TestIC9700SubReceiverIsNotRead(t *testing.T) {
	r := ic9700(t)
	caps := r.Caps()
	if !caps.SubReceiver {
		t.Error("caps deny the sub receiver; the radio has one")
	}
	if caps.SubReceiverReadable {
		t.Error("caps claim the sub receiver is readable; no command addresses it")
	}
	if caps.DualWatch {
		t.Error("caps claim dual watch; 07 C0/C1/C2 is not in this radio's table")
	}

	s := newSim(t)
	s.backend = r
	ctx := context.Background()
	for _, tier := range []backend.PollTier{backend.PollFast, backend.PollSlow} {
		if err := r.Poll(ctx, s, tier); err != nil {
			t.Fatalf("Poll: %v", err)
		}
	}
	if err := r.ReadVFOs(ctx, s); err != nil {
		t.Fatalf("ReadVFOs: %v", err)
	}
	for _, req := range s.log {
		cmd, body := req[4], req[5:len(req)-1]
		if cmd == cmdVFO && len(body) == 1 && (body[0] == subSelectMain || body[0] == subSelectSub) {
			t.Fatalf("sent 07 %02X: remoses must not switch bands to read one", body[0])
		}
		if cmd == cmdBand {
			t.Fatal("sent a 29 prefix; the IC-9700 has no such command")
		}
	}
}

// TestBandByteRejectsCurrent guards a subtle one: resolving "current" to A
// would make a request about one VFO act on another the moment a radio appeared
// whose operating VFO is not A.
func TestBandByteRejectsCurrent(t *testing.T) {
	if _, err := bandByte(radio.VFOCurrent); err == nil {
		t.Error("bandByte accepted VFOCurrent; these commands must name a VFO")
	}
	for _, vfo := range []radio.VFO{radio.VFOA, radio.VFOMain} {
		if b, err := bandByte(vfo); err != nil || b != bandMain {
			t.Errorf("bandByte(%v) = %02X, %v; want main", vfo, b, err)
		}
	}
	for _, vfo := range []radio.VFO{radio.VFOB, radio.VFOSub} {
		if b, err := bandByte(vfo); err != nil || b != bandSub {
			t.Errorf("bandByte(%v) = %02X, %v; want sub", vfo, b, err)
		}
	}
}

// TestVFOSnapshotRoundTrip covers the per-VFO hint cache, including the zero
// case: a snapshot asked for before anything has been read must not be a nil
// dereference.
func TestVFOSnapshotRoundTrip(t *testing.T) {
	r := testRig(t)
	if got := r.vfoSnapshot(radio.VFOB); got != (radio.VFOState{}) {
		t.Errorf("an unread VFO gave %+v, want the zero value", got)
	}
	for _, s := range []radio.VFOState{
		{Frequency: 28_350_000, Mode: radio.ModeUSB, DataMode: true, FilterSlot: 3, PassbandHz: 2400},
		{Frequency: 30_000_000_000, Mode: radio.ModeATV, FilterSlot: 1}, // an IC-905 on 30 GHz
	} {
		r.storeVFO(radio.VFOB, s)
		if got := r.vfoSnapshot(radio.VFOB); got != s {
			t.Errorf("round trip of %+v gave %+v", s, got)
		}
		if got := r.vfoSnapshot(radio.VFOA); got == s {
			t.Error("storing VFO B changed VFO A")
		}
	}
}
