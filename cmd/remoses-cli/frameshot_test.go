package main

import (
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/wire"
)

// TestFrameShot is a look at the whole display, receive and transmit, so that a
// layout change shows up as a readable diff rather than as an assertion about a
// substring. Run with -v to see it.
func TestFrameShot(t *testing.T) {
	base := wire.State{
		Connected:  true,
		Frequency:  28_030_000,
		Mode:       wire.ModeCW,
		PassbandHz: 500,
		FilterSlot: 1,
		Power:      wire.Power{Pct: 25, Watts: ptrFloat64(25)},
		SMeter:     wire.Meter{Raw: 96, Scale: 255},
		CW:         wire.CWStatus{WPM: 25},
	}

	tx := base
	tx.PTT = true
	ratio := 1.4
	tx.PowerMeter = &wire.Meter{Raw: 143, Scale: 255}
	// As the civ backend reports it: against the top of the calibrated range,
	// where 120 is SWR 3.0.
	tx.SWR = &wire.Meter{Raw: 38, Scale: 120}
	tx.SWRRatio = &ratio
	tx.ALC = &wire.Meter{Raw: 72, Scale: 120}
	tx.Tuner = ptr(wire.TunerTuning)
	base.Tuner = ptr(wire.TunerOn)

	// A radio that is reachable but switched off. The readings are whatever was
	// last true before it went off, which is why the note matters.
	standby := base
	standby.Standby = ptr(true)

	for _, tc := range []struct {
		name string
		st   wire.State
	}{{"receive", base}, {"transmit", tx}, {"standby", standby}} {
		t.Run(tc.name, func(t *testing.T) {
			v := meterView(t, tc.st)
			t.Logf("\n%s", strings.Join(frame(v, 72), "\n"))
		})
	}
}

func ptrFloat64(v float64) *float64 { return &v }
