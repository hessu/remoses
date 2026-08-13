package main

import (
	"strings"
	"testing"
	"time"

	"github.com/oapi-codegen/nullable"

	"github.com/hessu/remoses/internal/wire"
)

func meterView(t *testing.T, st wire.State) *view {
	t.Helper()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	v := newView("ic7610", func() time.Time { return now })
	v.setState(st)
	return v
}

// In receive the block is the S meter, as it always was.
func TestMeterLinesInReceive(t *testing.T) {
	v := meterView(t, wire.State{SMeter: wire.Meter{Raw: 60, Scale: 255}})
	lines := meterLines(v)
	if len(lines) != 1 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "S ") {
		t.Fatalf("meterLines() = %q, want one S meter line", lines)
	}
}

// While transmitting they swap. Stacking them would leave the S reading on
// screen, and during a transmission that number is not merely uninteresting —
// on a Kenwood the command behind it is reporting the power meter instead, so
// what is left in State is whatever the last receive poll saw.
func TestMeterLinesWhileTransmitting(t *testing.T) {
	v := meterView(t, wire.State{
		PTT:        true,
		SMeter:     wire.Meter{Raw: 60, Scale: 255},
		PowerMeter: txMeter(143, 255),
		SWR:        txMeter(48, 255),
		SWRRatio:   f64(1.5),
		ALC:        txMeter(60, 120),
	})
	lines := meterLines(v)
	if len(lines) != 3 {
		t.Fatalf("meterLines() = %q, want power, SWR and ALC", lines)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"PWR", "143/255", "56 %", "SWR", "1.5:1", "ALC", "60/120", "50 %"} {
		if !strings.Contains(joined, want) {
			t.Errorf("meter block is missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "  S ") {
		t.Errorf("the S meter is still shown while transmitting:\n%s", joined)
	}
}

// Only the meters the radio reports get a line: an FT-857 has forward power and
// a high-SWR bit and no ALC at all.
func TestMeterLinesOnlyWhatTheRadioReports(t *testing.T) {
	v := meterView(t, wire.State{
		PTT:        true,
		PowerMeter: txMeter(10, 15),
		SWR:        txMeter(1, 1),
	})
	lines := meterLines(v)
	if len(lines) != 2 {
		t.Fatalf("meterLines() = %q, want power and SWR only", lines)
	}
	// A one-bit SWR is a warning light, not a reading: "1/1" would look like a
	// measurement.
	if !strings.Contains(lines[1], "HIGH") {
		t.Errorf("SWR line = %q, want the alarm spelled out", lines[1])
	}
}

func TestFormatSWR(t *testing.T) {
	var absent, cleared nullable.Nullable[float64]
	cleared.SetNull()

	tests := []struct {
		name  string
		meter wire.Meter
		ratio nullable.Nullable[float64]
		want  string
	}{
		{"a calibrated ratio wins", wire.Meter{Raw: 80, Scale: 255}, f64(2.0), "2.0:1"},
		{"without one, the deflection", wire.Meter{Raw: 80, Scale: 255}, absent, "80/255  31 %"},
		// A ratio the radio has explicitly cleared reads the same as one it
		// never had: there is nothing to draw either way, and the display is
		// where the three wire states collapse to two.
		{"a cleared one, the deflection", wire.Meter{Raw: 80, Scale: 255}, cleared, "80/255  31 %"},
		{"a single bit set", wire.Meter{Raw: 1, Scale: 1}, absent, "HIGH"},
		{"a single bit clear", wire.Meter{Raw: 0, Scale: 1}, absent, "ok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatSWR(tt.meter, tt.ratio); got != tt.want {
				t.Errorf("formatSWR() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The log renderer omits the transmit meters in receive, where they do not
// exist. A line carrying pwr_raw=0 could not be told from a real reading into a
// dead load, and a log is read by somebody who cannot ask.
func TestStatusFieldsTXMeters(t *testing.T) {
	rx := statusFields(meterView(t, wire.State{SMeter: wire.Meter{Raw: 3, Scale: 255}}))
	for _, absent := range []string{"pwr_raw", "swr_raw", "swr=", "alc_raw"} {
		if strings.Contains(rx, absent) {
			t.Errorf("receive log line carries %q: %s", absent, rx)
		}
	}

	tx := statusFields(meterView(t, wire.State{
		PTT:        true,
		PowerMeter: txMeter(143, 255),
		SWR:        txMeter(48, 255),
		SWRRatio:   f64(1.5),
		ALC:        txMeter(60, 120),
	}))
	for _, want := range []string{
		"pwr_raw=143 pwr_scale=255",
		"swr_raw=48 swr_scale=255",
		"swr=1.50",
		"alc_raw=60 alc_scale=120",
	} {
		if !strings.Contains(tx, want) {
			t.Errorf("transmit log line is missing %q: %s", want, tx)
		}
	}
}

// A meter moving is a change the log should throttle on, and a transmission
// ending is one it should not miss.
func TestMetersComparableAcrossTransmit(t *testing.T) {
	rx := meterView(t, wire.State{SMeter: wire.Meter{Raw: 3, Scale: 255}}).meters()
	tx := meterView(t, wire.State{
		PTT:        true,
		SMeter:     wire.Meter{Raw: 3, Scale: 255},
		PowerMeter: txMeter(143, 255),
	}).meters()

	if rx == tx {
		t.Error("a transmission starting did not register as a meter change")
	}
	if tx == rx {
		t.Error("a transmission ending did not register as a meter change")
	}

	same := meterView(t, wire.State{
		PTT:        true,
		SMeter:     wire.Meter{Raw: 3, Scale: 255},
		PowerMeter: txMeter(143, 255),
	}).meters()
	if tx != same {
		t.Error("two identical readings compared unequal; the pointers are not flattened")
	}
}
