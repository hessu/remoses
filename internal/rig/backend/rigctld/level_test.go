package rigctld

import (
	"math"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

func TestPowerFromLevel(t *testing.T) {
	tests := []struct {
		name   string
		v      float64
		pct    float64
		native int
	}{
		{name: "off", v: 0, pct: 0, native: 0},
		{name: "half", v: 0.5, pct: 50, native: 50},
		{name: "full", v: 1, pct: 100, native: 100},
		{name: "a rig's own quantisation", v: 0.235, pct: 23.5, native: 24},
		// A rig backend outside its documented 0..1 range is a Hamlib bug;
		// clamping keeps it out of a percentage field documented as 0..100.
		{name: "above range is clamped", v: 1.5, pct: 100, native: 100},
		{name: "below range is clamped", v: -0.2, pct: 0, native: 0},
		{name: "NaN is not propagated", v: math.NaN(), pct: 0, native: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := powerFromLevel(tc.v)
			if p.Pct != tc.pct || p.Native != tc.native {
				t.Errorf("powerFromLevel(%v) = pct %v native %d, want pct %v native %d",
					tc.v, p.Pct, p.Native, tc.pct, tc.native)
			}
			// The one invariant that matters: RFPOWER is not watts, and this
			// backend never claims it is.
			if p.Watts != nil {
				t.Errorf("powerFromLevel(%v) invented %v watts", tc.v, *p.Watts)
			}
		})
	}
}

func TestLevelFromPowerSet(t *testing.T) {
	pct := func(v float64) *float64 { return &v }

	tests := []struct {
		name    string
		set     radio.PowerSet
		want    float64
		wantErr string
	}{
		{name: "zero per cent", set: radio.PowerSet{Pct: pct(0)}, want: 0},
		{name: "half", set: radio.PowerSet{Pct: pct(50)}, want: 0.5},
		{name: "full", set: radio.PowerSet{Pct: pct(100)}, want: 1},
		{name: "fractional", set: radio.PowerSet{Pct: pct(37.5)}, want: 0.375},

		{
			name:    "watts are refused, not converted",
			set:     radio.PowerSet{Watts: pct(40)},
			wantErr: "no watt meaning",
		},
		{name: "above range", set: radio.PowerSet{Pct: pct(101)}, wantErr: "outside 0..100"},
		{name: "below range", set: radio.PowerSet{Pct: pct(-1)}, wantErr: "outside 0..100"},
		{name: "NaN", set: radio.PowerSet{Pct: pct(math.NaN())}, wantErr: "not a number"},
		{name: "neither field", set: radio.PowerSet{}, wantErr: "requires either"},
		{
			name:    "both fields",
			set:     radio.PowerSet{Pct: pct(50), Watts: pct(40)},
			wantErr: "not both",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := levelFromPowerSet(tc.set)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("got %v, want an error mentioning %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("levelFromPowerSet: %v", err)
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPowerRoundTrip proves a percentage survives the trip out to the wire and
// back, to the precision the request format allows.
func TestPowerRoundTrip(t *testing.T) {
	for _, want := range []float64{0, 12.5, 25, 50, 75, 100} {
		v, err := levelFromPowerSet(radio.PowerSet{Pct: &want})
		if err != nil {
			t.Fatalf("%v%%: %v", want, err)
		}
		got := powerFromLevel(v).Pct
		if math.Abs(got-want) > 0.1 {
			t.Errorf("%v%% round-tripped to %v%%", want, got)
		}
	}
}

func TestMeterFromStrength(t *testing.T) {
	tests := []struct {
		name  string
		db    int
		s     float64
		raw   int
		scale int
	}{
		// The reference point: STRENGTH is dB relative to S9.
		{name: "S9", db: 0, s: 9, raw: 54, scale: sMeterScale},
		// One S-unit is 6 dB below S9.
		{name: "S8", db: -6, s: 8, raw: 48, scale: sMeterScale},
		{name: "S5", db: -24, s: 5, raw: 30, scale: sMeterScale},
		{name: "S0", db: -54, s: 0, raw: 0, scale: sMeterScale},
		// Above S9 the scale continues linearly so no reading is lost:
		// (S-9)*6 recovers the dB exactly.
		{name: "S9+20", db: 20, s: 9 + 20.0/6, raw: 74, scale: sMeterScale},
		{name: "S9+60", db: 60, s: 19, raw: 114, scale: sMeterScale},
		// Clamped at the ends so Fraction stays inside a bar.
		{name: "below S0", db: -80, s: 0, raw: 0, scale: sMeterScale},
		{name: "above the marked scale", db: 90, s: 24, raw: 114, scale: sMeterScale},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := meterFromStrength(tc.db)
			if m.Raw != tc.raw || m.Scale != tc.scale {
				t.Errorf("raw/scale = %d/%d, want %d/%d", m.Raw, m.Scale, tc.raw, tc.scale)
			}
			if m.S == nil {
				t.Fatal("S is nil; STRENGTH is calibrated and must populate it")
			}
			if math.Abs(*m.S-tc.s) > 1e-9 {
				t.Errorf("S = %v, want %v", *m.S, tc.s)
			}
			if f := m.Fraction(); f < 0 || f > 1 {
				t.Errorf("Fraction = %v, outside 0..1", f)
			}
		})
	}
}
