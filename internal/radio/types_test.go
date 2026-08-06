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
