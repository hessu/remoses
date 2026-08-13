package radio

import "testing"

// TestModeTextRoundTrip is the check that was missing when a client could not
// read a radio that had just connected.
//
// Mode.String emits UNKNOWN for a rig that has not reported one yet, and
// ParseMode refused it — so the type could not decode its own output, and
// remoses-cli failed with `unknown mode "UNKNOWN"` against a freshly connected
// IC-9700. Every value has to survive the round trip, whatever the API then
// decides to accept as input.
func TestModeTextRoundTrip(t *testing.T) {
	for m := ModeUnknown; m <= ModeDIG; m++ {
		name := m.String()
		if name == "" || name[0] == 'M' && len(name) > 5 && name[:5] == "Mode(" {
			t.Errorf("mode %d has no name; add it to modeNames", uint8(m))
			continue
		}
		got, err := ParseMode(name)
		if err != nil {
			t.Errorf("ParseMode(%q) — the string Mode(%d) marshals to — failed: %v",
				name, uint8(m), err)
			continue
		}
		if got != m {
			t.Errorf("%q round-tripped to %v, want %v", name, got, m)
		}
	}
}

// TestModeUnmarshalRoundTrip covers the same ground through the JSON path a
// client actually uses.
func TestModeUnmarshalRoundTrip(t *testing.T) {
	for m := ModeUnknown; m <= ModeDIG; m++ {
		b, err := m.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%v): %v", m, err)
		}
		var got Mode
		if err := got.UnmarshalText(b); err != nil {
			t.Errorf("UnmarshalText(%q): %v", b, err)
			continue
		}
		if got != m {
			t.Errorf("%q unmarshalled to %v, want %v", b, got, m)
		}
	}
}

// TestTXVFO pins the rule that decides where RF comes out. Getting it wrong
// tells an operator they are transmitting on a frequency they are not, which is
// the worst kind of wrong answer this project can give.
func TestTXVFO(t *testing.T) {
	cases := []struct {
		name  string
		vfo   VFO
		split bool
		want  VFO
	}{
		{"no split transmits on the operating VFO", VFOA, false, VFOA},
		{"no split on B transmits on B", VFOB, false, VFOB},
		{"split from A transmits on B", VFOA, true, VFOB},
		{"split from B transmits on A", VFOB, true, VFOA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := State{VFO: tc.vfo, Split: tc.split}
			if got := s.TXVFO(); got != tc.want {
				t.Errorf("TXVFO() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDualVFOPatchRoundTrip covers Apply and Diff together for the new fields:
// anything Diff reports must be enough for Apply to reproduce the change, or a
// WebSocket client working from deltas drifts out of step with the cache.
func TestDualVFOPatchRoundTrip(t *testing.T) {
	before := State{
		Frequency: 28030000, Mode: ModeCW, VFO: VFOA,
		VFOA: VFOState{Frequency: 28030000, Mode: ModeCW, PassbandHz: 500, FilterSlot: 1},
		VFOB: VFOState{Frequency: 28035000, Mode: ModeUSB, PassbandHz: 2400, FilterSlot: 2},
	}
	after := before
	after.Split = true
	after.DualWatch = true
	after.VFO = VFOB
	after.VFOB.Frequency = 28040000
	after.VFOB.DataMode = true
	after.SubSMeter = Meter{Raw: 7, Scale: 255}

	delta := before.Diff(after)
	if delta.Empty() {
		t.Fatal("Diff reported no change")
	}
	for name, got := range map[string]bool{
		"split": delta.Split != nil, "dual_watch": delta.DualWatch != nil,
		"vfo": delta.VFO != nil, "vfo_b": delta.VFOB != nil, "sub_s_meter": delta.SubSMeter != nil,
	} {
		if !got {
			t.Errorf("Diff omitted %s", name)
		}
	}
	if delta.VFOA != nil {
		t.Error("Diff reported a change to VFO A, which did not move")
	}

	if got := before.Apply(delta); got != after {
		t.Errorf("Apply(Diff) did not reproduce the state:\n got %+v\nwant %+v", got, after)
	}
}

// TestDiffComparesReadingsRatherThanAddresses is the bug a live radio found.
//
// Meter carries an optional S and Power an optional Watts, and a backend fills
// those in fresh on every poll: rigctld allocates an S for the calibrated
// S-unit figure, Kenwood and Yaesu allocate a Watts. Comparing the structs with
// == compares those POINTERS, so two identical readings taken half a second
// apart answered "changed" — and a radio sitting on a dead quiet band published
// an s_meter delta to every connected client twice a second, for ever.
//
// Nothing in the fakes caught it because a fake that returns a constant returns
// the same value from the same allocation, and on the air a moving meter hides
// it: the deltas are real then. It takes a real backend and a still radio.
func TestDiffComparesReadingsRatherThanAddresses(t *testing.T) {
	sUnits, watts := 5.5, 40.0
	st := State{
		Frequency: 28030000,
		SMeter:    Meter{Raw: 78, Scale: 255, S: &sUnits},
		SubSMeter: Meter{Raw: 12, Scale: 255, S: &sUnits},
		Power:     Power{Pct: 40, Native: 102, Watts: &watts},
	}

	// The same readings, as the next poll would build them: equal values in
	// freshly allocated pointers.
	sAgain, wattsAgain := 5.5, 40.0
	next := st
	next.SMeter = Meter{Raw: 78, Scale: 255, S: &sAgain}
	next.SubSMeter = Meter{Raw: 12, Scale: 255, S: &sAgain}
	next.Power = Power{Pct: 40, Native: 102, Watts: &wattsAgain}

	if d := st.Diff(next); !d.Empty() {
		t.Errorf("Diff reported a change between two identical readings: %+v", d)
	}

	// And a reading that really moved is still a change, including one where
	// only the calibrated figure differs.
	moved := next
	movedS := 7.0
	moved.SMeter = Meter{Raw: 78, Scale: 255, S: &movedS}
	if d := st.Diff(moved); d.SMeter == nil {
		t.Error("Diff missed a change to the calibrated S reading")
	}

	louder := next
	louder.SMeter = Meter{Raw: 120, Scale: 255, S: &sAgain}
	if d := st.Diff(louder); d.SMeter == nil {
		t.Error("Diff missed a change to the raw meter reading")
	}

	turnedDown := next
	half := 20.0
	turnedDown.Power = Power{Pct: 20, Native: 51, Watts: &half}
	if d := st.Diff(turnedDown); d.Power == nil {
		t.Error("Diff missed a power change")
	}

	// The optional halves, either way round: a meter that gains a calibration
	// and one that loses it have both changed.
	gained, lost := next, next
	gained.SMeter = Meter{Raw: 78, Scale: 255, S: &sAgain}
	lost.SMeter = Meter{Raw: 78, Scale: 255}
	if d := lost.Diff(gained); d.SMeter == nil {
		t.Error("Diff missed a meter that gained a calibrated reading")
	}
	if d := gained.Diff(lost); d.SMeter == nil {
		t.Error("Diff missed a meter that lost its calibrated reading")
	}
}

// TestDiffReportsAQueueThatMovedWithoutStopping is the other half of what a
// transmitting radio showed.
//
// A message draining reports busy=true for its whole length while the queue
// depth and the estimate move on every poll. When the patch could say only
// "busy changed", those polls produced an EMPTY patch — and the WebSocket layer
// answers an empty patch with a full state snapshot, because there is nothing
// else honest it can send. Sending Morse therefore published the entire state
// twice a second for as long as the message lasted.
func TestDiffReportsAQueueThatMovedWithoutStopping(t *testing.T) {
	sending := State{CW: CWStatus{Busy: true, Queued: 14, WPM: 20, EstRemainingMS: 6235}}

	drained := sending
	drained.CW.Queued, drained.CW.EstRemainingMS = 9, 4110

	d := sending.Diff(drained)
	if d.Empty() {
		t.Fatal("a queue that moved reported no change at all")
	}
	if d.CW == nil {
		t.Fatal("Diff did not report the CW status")
	}
	if d.CW.Queued != 9 || d.CW.EstRemainingMS != 4110 || !d.CW.Busy {
		t.Errorf("cw = %+v, want the new depth and estimate with busy still set", *d.CW)
	}

	// The speed is part of it too: a client showing wpm should see it change.
	faster := sending
	faster.CW.WPM = 28
	if d := sending.Diff(faster); d.CW == nil {
		t.Error("Diff missed a keyer speed change")
	}

	// And an unchanged queue is still not a change.
	if d := sending.Diff(sending); !d.Empty() {
		t.Errorf("an unchanged queue reported a change: %+v", *d.CW)
	}
}

// The transmit meters are pointers to the same type, and they are compared by
// their own helper. It has the same trap in it.
func TestTransmitMeterDiffComparesReadings(t *testing.T) {
	a, b := 1.5, 1.5
	st := State{PTT: true, SWR: &Meter{Raw: 30, Scale: 100, S: &a}}
	next := State{PTT: true, SWR: &Meter{Raw: 30, Scale: 100, S: &b}}

	if d := st.Diff(next); d.SWR != nil {
		t.Errorf("Diff reported a change between two identical SWR readings: %+v", *d.SWR)
	}
}

// TestEmptyKnowsTheDualVFOFields guards the trap in Patch.Empty: a field added
// to the struct and forgotten here makes the session drop the update silently,
// because applyUpdate returns early on an empty patch.
func TestEmptyKnowsTheDualVFOFields(t *testing.T) {
	vfo, on, st, m := VFOB, true, VFOState{Frequency: 1}, Meter{Raw: 1, Scale: 255}
	for name, p := range map[string]Patch{
		"vfo":         {VFO: &vfo},
		"vfo_a":       {VFOA: &st},
		"vfo_b":       {VFOB: &st},
		"split":       {Split: &on},
		"dual_watch":  {DualWatch: &on},
		"sub_s_meter": {SubSMeter: &m},
	} {
		if p.Empty() {
			t.Errorf("a patch carrying only %s reports Empty; the session would drop it", name)
		}
	}
}
