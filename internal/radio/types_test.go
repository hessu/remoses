package radio

import "testing"

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
