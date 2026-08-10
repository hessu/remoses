package main

import (
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

// TestFrameShot is a look at the whole display, receive and transmit, so that a
// layout change shows up as a readable diff rather than as an assertion about a
// substring. Run with -v to see it.
func TestFrameShot(t *testing.T) {
	base := radio.State{
		Connected:  true,
		Frequency:  28_030_000,
		Mode:       radio.ModeCW,
		PassbandHz: 500,
		FilterSlot: 1,
		Power:      radio.Power{Pct: 25, Watts: ptrFloat64(25)},
		SMeter:     radio.Meter{Raw: 96, Scale: 255},
		CW:         radio.CWStatus{WPM: 25},
	}

	tx := base
	tx.PTT = true
	ratio := 1.4
	tx.PowerMeter = &radio.Meter{Raw: 143, Scale: 255}
	// As the civ backend reports it: against the top of the calibrated range,
	// where 120 is SWR 3.0.
	tx.SWR = &radio.Meter{Raw: 38, Scale: 120}
	tx.SWRRatio = &ratio
	tx.ALC = &radio.Meter{Raw: 72, Scale: 120}

	for _, tc := range []struct {
		name string
		st   radio.State
	}{{"receive", base}, {"transmit", tx}} {
		t.Run(tc.name, func(t *testing.T) {
			v := meterView(t, tc.st)
			t.Logf("\n%s", strings.Join(frame(v, 72), "\n"))
		})
	}
}

func ptrFloat64(v float64) *float64 { return &v }
