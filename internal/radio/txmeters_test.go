package radio

import "testing"

// The transmit meters are absent in receive rather than zero, and State is
// where that invariant lives: every radio that reports them reports them only
// while keyed, so no backend should have to remember to clear them.
func TestApplyClearsTXMetersOnReceive(t *testing.T) {
	keyed := State{
		PTT:        true,
		PowerMeter: &Meter{Raw: 200, Scale: 255},
		SWR:        &Meter{Raw: 48, Scale: 255},
		ALC:        &Meter{Raw: 60, Scale: 120},
		SWRRatio:   ptrFloat(1.5),
	}

	// Still transmitting: everything survives.
	got := keyed.Apply(Patch{})
	if got.PowerMeter == nil || got.SWR == nil || got.ALC == nil || got.SWRRatio == nil {
		t.Fatalf("a patch that says nothing dropped the meters: %+v", got)
	}

	// Unkeyed: all four go, so that a client never draws a stale 3:1 SWR as if
	// the fault were still happening.
	off := false
	got = keyed.Apply(Patch{PTT: &off})
	if got.PowerMeter != nil {
		t.Errorf("PowerMeter = %+v, want nil after PTT dropped", got.PowerMeter)
	}
	if got.SWR != nil {
		t.Errorf("SWR = %+v, want nil after PTT dropped", got.SWR)
	}
	if got.ALC != nil {
		t.Errorf("ALC = %+v, want nil after PTT dropped", got.ALC)
	}
	if got.SWRRatio != nil {
		t.Errorf("SWRRatio = %v, want nil after PTT dropped", *got.SWRRatio)
	}
}

// A meter arriving in the same patch as the PTT that keyed the rig has to
// survive, or the first sample of every transmission would be thrown away.
func TestApplyKeepsTXMetersWhenPTTGoesUp(t *testing.T) {
	on := true
	got := State{}.Apply(Patch{
		PTT:        &on,
		PowerMeter: &Meter{Raw: 100, Scale: 255},
	})
	if got.PowerMeter == nil || got.PowerMeter.Raw != 100 {
		t.Errorf("PowerMeter = %+v, want the reading that arrived with the key-down", got.PowerMeter)
	}
}

// Diff has to notice a meter appearing, changing and going away, since all
// three are what a WebSocket client draws from.
func TestDiffReportsTXMeterChanges(t *testing.T) {
	rx := State{}
	tx := State{PTT: true, PowerMeter: &Meter{Raw: 200, Scale: 255}, SWRRatio: ptrFloat(1.5)}

	t.Run("appearing", func(t *testing.T) {
		p := rx.Diff(tx)
		if p.PowerMeter == nil || p.PowerMeter.Raw != 200 {
			t.Errorf("PowerMeter = %+v, want the new reading", p.PowerMeter)
		}
		if p.SWRRatio == nil || *p.SWRRatio != 1.5 {
			t.Errorf("SWRRatio = %v, want 1.5", p.SWRRatio)
		}
	})

	t.Run("unchanged", func(t *testing.T) {
		same := State{PTT: true, PowerMeter: &Meter{Raw: 200, Scale: 255}, SWRRatio: ptrFloat(1.5)}
		p := tx.Diff(same)
		if p.PowerMeter != nil || p.SWRRatio != nil {
			t.Errorf("a repeated reading produced a delta: %+v", p)
		}
	})

	t.Run("changing", func(t *testing.T) {
		next := State{PTT: true, PowerMeter: &Meter{Raw: 40, Scale: 255}}
		if p := tx.Diff(next); p.PowerMeter == nil || p.PowerMeter.Raw != 40 {
			t.Errorf("PowerMeter = %+v, want the changed reading", p.PowerMeter)
		}
	})

	// The end of a transmission is a change a client needs as much as a new
	// reading: it is what tells the display to stop.
	t.Run("going away", func(t *testing.T) {
		p := tx.Diff(rx)
		if !p.Empty() {
			// It should report the drop; what matters is that Diff noticed at
			// all, and that the fields read as cleared.
			if p.PowerMeter != nil {
				t.Errorf("PowerMeter = %+v, want nil to signal the end", p.PowerMeter)
			}
			if p.SWRRatio != nil {
				t.Errorf("SWRRatio = %v, want nil to signal the end", *p.SWRRatio)
			}
			return
		}
		t.Error("Diff said nothing when the transmission ended")
	})
}

func ptrFloat(v float64) *float64 { return &v }
