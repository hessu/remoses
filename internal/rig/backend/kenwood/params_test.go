package kenwood

import (
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

func TestFrequencyRoundTrip(t *testing.T) {
	tests := []struct {
		hz   uint64
		wire string
	}{
		{0, "00000000000"},
		{14_025_000, "00014025000"},
		{7_050_123, "00007050123"},
		{1_810_000, "00001810000"},
		{54_000_000, "00054000000"},
		{maxFrequencyHz, "99999999999"},
	}
	for _, tt := range tests {
		t.Run(tt.wire, func(t *testing.T) {
			got, err := formatFrequency(tt.hz)
			if err != nil {
				t.Fatalf("formatFrequency(%d): %v", tt.hz, err)
			}
			if got != tt.wire {
				t.Fatalf("formatFrequency(%d) = %q, want %q", tt.hz, got, tt.wire)
			}
			if len(got) != freqDigits {
				t.Fatalf("formatFrequency(%d) is %d digits, want %d", tt.hz, len(got), freqDigits)
			}
			back, err := parseFrequency(got)
			if err != nil {
				t.Fatalf("parseFrequency(%q): %v", got, err)
			}
			if back != tt.hz {
				t.Fatalf("round trip = %d, want %d", back, tt.hz)
			}
		})
	}
}

func TestFrequencyErrors(t *testing.T) {
	if _, err := formatFrequency(maxFrequencyHz + 1); err == nil {
		t.Error("formatFrequency accepted a value too wide for the 11-digit field")
	}
	for _, bad := range []string{"", "0001402500", "000140250000", "0001402500x"} {
		if _, err := parseFrequency(bad); err == nil {
			t.Errorf("parseFrequency(%q) accepted a malformed field", bad)
		}
	}
}

func TestModeMapping(t *testing.T) {
	tests := []struct {
		digit byte
		mode  radio.Mode
	}{
		{'1', radio.ModeLSB},
		{'2', radio.ModeUSB},
		{'3', radio.ModeCW},
		{'4', radio.ModeFM},
		{'5', radio.ModeAM},
		{'6', radio.ModeFSK},
		{'7', radio.ModeCWR},
		{'9', radio.ModeFSKR},
	}
	for _, tt := range tests {
		t.Run(tt.mode.String(), func(t *testing.T) {
			got, ok := decodeMode(tt.digit)
			if !ok || got != tt.mode {
				t.Fatalf("decodeMode(%q) = (%s, %v), want (%s, true)", tt.digit, got, ok, tt.mode)
			}
			back, err := encodeMode(tt.mode)
			if err != nil {
				t.Fatalf("encodeMode(%s): %v", tt.mode, err)
			}
			if back != tt.digit {
				t.Fatalf("encodeMode(%s) = %q, want %q", tt.mode, back, tt.digit)
			}
		})
	}
}

// TestModeSettingFailureValues pins the reason 0 and 8 are not in the table: the
// rig uses them to report a *failed* set, and folding them into ModeUnknown
// would let that overwrite the cached mode.
func TestModeSettingFailureValues(t *testing.T) {
	for _, d := range []byte{'0', '8'} {
		if m, ok := decodeMode(d); ok {
			t.Errorf("decodeMode(%q) = %s, want no mode: the reference calls it a setting failure", d, m)
		}
	}
}

func TestModeUnmappable(t *testing.T) {
	for _, m := range []radio.Mode{radio.ModePSK, radio.ModePSKR, radio.ModeUnknown} {
		if d, err := encodeMode(m); err == nil {
			t.Errorf("encodeMode(%s) = %q, want an error: the TS-590 has no MD value for it", m, d)
		}
	}
	for _, d := range []byte{'x', ' ', 'A'} {
		if _, ok := decodeMode(d); ok {
			t.Errorf("decodeMode(%q) accepted a value the reference does not define", d)
		}
	}
}

func TestSupportsDataMode(t *testing.T) {
	// "You can use this command in LSB, USB, FM, and AM mode. When used in CW,
	// FSK, an error occurs."
	yes := []radio.Mode{radio.ModeLSB, radio.ModeUSB, radio.ModeFM, radio.ModeAM}
	no := []radio.Mode{radio.ModeCW, radio.ModeCWR, radio.ModeFSK, radio.ModeFSKR, radio.ModeUnknown}
	for _, m := range yes {
		if !supportsDataMode(m) {
			t.Errorf("supportsDataMode(%s) = false, want true", m)
		}
	}
	for _, m := range no {
		if supportsDataMode(m) {
			t.Errorf("supportsDataMode(%s) = true, want false", m)
		}
	}
}

func TestPowerFromWatts(t *testing.T) {
	tests := []struct {
		name    string
		watts   int
		mode    radio.Mode
		wantPct float64
	}{
		{"SSB minimum", 5, radio.ModeUSB, 5},
		{"SSB half", 50, radio.ModeUSB, 50},
		{"SSB full", 100, radio.ModeUSB, 100},
		{"CW full", 100, radio.ModeCW, 100},
		{"FSK quarter", 25, radio.ModeFSK, 25},
		// AM tops out at 25 W, so the same watts are four times the percentage.
		{"AM minimum", 5, radio.ModeAM, 20},
		{"AM ten watts", 10, radio.ModeAM, 40},
		{"AM full", 25, radio.ModeAM, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := powerFromWatts(tt.watts, tt.mode)
			if p.Watts == nil {
				t.Fatal("Watts is nil; the TS-590 scale is watt-accurate")
			}
			if *p.Watts != float64(tt.watts) {
				t.Errorf("Watts = %v, want %d", *p.Watts, tt.watts)
			}
			if p.Native != tt.watts {
				t.Errorf("Native = %d, want %d", p.Native, tt.watts)
			}
			if p.Pct != tt.wantPct {
				t.Errorf("Pct = %v, want %v", p.Pct, tt.wantPct)
			}
		})
	}
}

func TestWattsFromSet(t *testing.T) {
	w := func(v float64) radio.PowerSet { return radio.PowerSet{Watts: &v} }
	p := func(v float64) radio.PowerSet { return radio.PowerSet{Pct: &v} }

	tests := []struct {
		name string
		set  radio.PowerSet
		mode radio.Mode
		want int
	}{
		{"watts pass through", w(50), radio.ModeUSB, 50},
		{"watts clamped up to the minimum", w(1), radio.ModeUSB, minPowerW},
		{"watts clamped down to the SSB maximum", w(250), radio.ModeUSB, 100},
		{"watts clamped down to the AM maximum", w(100), radio.ModeAM, 25},
		{"off-grid watts are left for the rig to round", w(93), radio.ModeUSB, 93},
		{"percent of the SSB scale", p(50), radio.ModeUSB, 50},
		{"full percent in SSB", p(100), radio.ModeUSB, 100},
		{"percent of the AM scale", p(50), radio.ModeAM, 13}, // 12.5 W rounds to 13
		{"full percent in AM", p(100), radio.ModeAM, 25},
		{"zero percent clamps to the floor", p(0), radio.ModeUSB, minPowerW},
		{"unknown mode uses the 100 W scale", p(50), radio.ModeUnknown, 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := wattsFromSet(tt.set, tt.mode)
			if err != nil {
				t.Fatalf("wattsFromSet: %v", err)
			}
			if got != tt.want {
				t.Fatalf("= %d W, want %d W", got, tt.want)
			}
		})
	}
}

func TestWattsFromSetErrors(t *testing.T) {
	over, under := 101.0, -1.0
	tests := []struct {
		name string
		set  radio.PowerSet
	}{
		{"neither field", radio.PowerSet{}},
		{"both fields", radio.PowerSet{Watts: &over, Pct: &over}},
		{"percent above 100", radio.PowerSet{Pct: &over}},
		{"negative percent", radio.PowerSet{Pct: &under}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := wattsFromSet(tt.set, radio.ModeUSB); err == nil {
				t.Fatal("accepted an invalid PowerSet")
			}
		})
	}
}

// TestPowerRoundTrip checks that what SetPower would send comes back out of
// Decode as the same watts.
func TestPowerRoundTrip(t *testing.T) {
	for _, mode := range []radio.Mode{radio.ModeUSB, radio.ModeCW, radio.ModeAM} {
		for _, pct := range []float64{20, 40, 60, 80, 100} {
			w, err := wattsFromSet(radio.PowerSet{Pct: &pct}, mode)
			if err != nil {
				t.Fatalf("wattsFromSet: %v", err)
			}
			got := powerFromWatts(w, mode)
			if diff := got.Pct - pct; diff > 2 || diff < -2 {
				t.Errorf("%s %.0f%% -> %d W -> %.2f%%, drifted by more than rounding explains",
					mode, pct, w, got.Pct)
			}
		}
	}
}

func TestSnapFilterWidth(t *testing.T) {
	tests := []struct {
		name string
		mode radio.Mode
		hz   int
		want int
	}{
		// CW ladder: 50 80 100 150 200 250 300 400 500 600 1000 1500 2000 2500
		{"CW exact rung", radio.ModeCW, 500, 500},
		{"CW off-step snaps down", radio.ModeCW, 1400, 1000},
		{"CW just under a rung", radio.ModeCW, 599, 500},
		{"CW just over a rung", radio.ModeCW, 601, 600},
		{"CW below the ladder", radio.ModeCW, 49, 50},
		{"CW at the floor", radio.ModeCW, 50, 50},
		{"CW zero", radio.ModeCW, 0, 50},
		{"CW negative", radio.ModeCW, -100, 50},
		{"CW above the ladder", radio.ModeCW, 2501, 2500},
		{"CW far above", radio.ModeCW, 9999, 2500},
		{"CW-R uses the CW ladder", radio.ModeCWR, 633, 600},

		// FSK ladder: 250 500 1000 1500
		{"FSK exact rung", radio.ModeFSK, 500, 500},
		{"FSK off-step snaps down", radio.ModeFSK, 1400, 1000},
		{"FSK below the ladder", radio.ModeFSK, 249, 250},
		{"FSK above the ladder", radio.ModeFSK, 1501, 1500},
		{"FSK-R uses the FSK ladder", radio.ModeFSKR, 900, 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := snapFilterWidth(tt.hz, tt.mode)
			if err != nil {
				t.Fatalf("snapFilterWidth(%d, %s): %v", tt.hz, tt.mode, err)
			}
			if got != tt.want {
				t.Fatalf("snapFilterWidth(%d, %s) = %d, want %d", tt.hz, tt.mode, got, tt.want)
			}
		})
	}
}

// TestFilterWidthIllegalModes covers the modes where FW is the wrong command.
// The error has to say so plainly: an operator who set a CW filter and then
// switched to SSB needs to know why the width stopped moving.
func TestFilterWidthIllegalModes(t *testing.T) {
	tests := []struct {
		mode     radio.Mode
		wantWord string
	}{
		{radio.ModeLSB, "SH/SL"},
		{radio.ModeUSB, "SH/SL"},
		{radio.ModeAM, "SH/SL"},
		{radio.ModeFM, "modulation degree"},
		{radio.ModeUnknown, "known mode"},
	}
	for _, tt := range tests {
		t.Run(tt.mode.String(), func(t *testing.T) {
			if filterWidthLegal(tt.mode) {
				t.Fatalf("filterWidthLegal(%s) = true, want false", tt.mode)
			}
			_, err := snapFilterWidth(500, tt.mode)
			if err == nil {
				t.Fatalf("snapFilterWidth accepted %s", tt.mode)
			}
			if !strings.Contains(err.Error(), tt.wantWord) {
				t.Errorf("error %q does not explain itself (want %q in it)", err, tt.wantWord)
			}
		})
	}

	for _, m := range []radio.Mode{radio.ModeCW, radio.ModeCWR, radio.ModeFSK, radio.ModeFSKR} {
		if !filterWidthLegal(m) {
			t.Errorf("filterWidthLegal(%s) = false, want true", m)
		}
	}
}

func TestNormaliseModel(t *testing.T) {
	tests := map[string]string{
		"ts590s":    "TS-590S",
		"TS-590S":   "TS-590S",
		"ts-590sg":  "TS-590SG",
		"TS590SG":   "TS-590SG",
		"  ts590s ": "TS-590S",
		"":          "",
		"k3":        "k3",
	}
	for in, want := range tests {
		if got := normaliseModel(in); got != want {
			t.Errorf("normaliseModel(%q) = %q, want %q", in, got, want)
		}
	}
}
