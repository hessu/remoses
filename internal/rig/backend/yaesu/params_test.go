package yaesu

import (
	"math"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

func TestFormatFrequency(t *testing.T) {
	tests := []struct {
		hz     uint64
		digits int
		want   string
		fail   bool
	}{
		// Nine digits on the FTdx101 generation, eight on the FT-950
		// generation. Kenwood's eleven would be a syntax error on either.
		{hz: 14_025_000, digits: 9, want: "014025000"},
		{hz: 30_000, digits: 9, want: "000030000"},
		{hz: 470_000_000, digits: 9, want: "470000000"},
		{hz: 999_999_999, digits: 9, want: "999999999"},
		{hz: 1_000_000_000, digits: 9, fail: true},
		{hz: 14_025_000, digits: 8, want: "14025000"},
		{hz: 30_000, digits: 8, want: "00030000"},
		{hz: 60_000_000, digits: 8, want: "60000000"},
		{hz: 99_999_999, digits: 8, want: "99999999"},
		// A frequency an eight-digit rig cannot express. The per-model range
		// check catches this first in practice; this is the guard that keeps a
		// bad caller from desynchronising the command stream.
		{hz: 100_000_000, digits: 8, fail: true},
	}
	for _, tt := range tests {
		got, err := formatFrequency(tt.hz, tt.digits)
		if tt.fail {
			if err == nil {
				t.Errorf("formatFrequency(%d, %d) = %q, want an error", tt.hz, tt.digits, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("formatFrequency(%d, %d): %v", tt.hz, tt.digits, err)
		}
		if got != tt.want {
			t.Errorf("formatFrequency(%d, %d) = %q, want %q", tt.hz, tt.digits, got, tt.want)
		}
		if len(got) != tt.digits {
			t.Errorf("formatFrequency(%d, %d) is %d digits", tt.hz, tt.digits, len(got))
		}

		// Both widths read back, which is what lets a station whose
		// configuration names the wrong generation still be decoded.
		back, err := parseFrequency(got)
		if err != nil || back != tt.hz {
			t.Errorf("parseFrequency(%q) = (%d, %v), want %d", got, back, err, tt.hz)
		}
		if back, err := parseFrequencyWidth(got, tt.digits); err != nil || back != tt.hz {
			t.Errorf("parseFrequencyWidth(%q, %d) = (%d, %v), want %d", got, tt.digits, back, err, tt.hz)
		}
	}
}

func TestParseFrequencyRejectsOtherWidths(t *testing.T) {
	// Eleven digits is a Kenwood frame; seven and ten are nothing at all.
	// Reading one at a fixed offset would report a frequency a hundred times
	// out, so the width is checked.
	for _, s := range []string{"00014025000", "1402500", "0140250000", "", "01402500x"} {
		if hz, err := parseFrequency(s); err == nil {
			t.Errorf("parseFrequency(%q) = %d, want an error", s, hz)
		}
	}
	// parseFrequencyWidth is stricter still: the IF decoder has already fixed
	// the width from the layout that matched, so the other one is an error too.
	if hz, err := parseFrequencyWidth("14025000", freqDigitsModern); err == nil {
		t.Errorf("parseFrequencyWidth(8 digits, want 9) = %d, want an error", hz)
	}
	if hz, err := parseFrequencyWidth("014025000", freqDigitsOld); err == nil {
		t.Errorf("parseFrequencyWidth(9 digits, want 8) = %d, want an error", hz)
	}
}

func TestMaxFrequencyHz(t *testing.T) {
	for _, tt := range []struct {
		digits int
		want   uint64
	}{
		{8, 99_999_999},
		{9, 999_999_999},
	} {
		if got := maxFrequencyHz(tt.digits); got != tt.want {
			t.Errorf("maxFrequencyHz(%d) = %d, want %d", tt.digits, got, tt.want)
		}
	}
}

func TestWattsFromSet(t *testing.T) {
	watts := func(v float64) radio.PowerSet { return radio.PowerSet{Watts: &v} }
	pct := func(v float64) radio.PowerSet { return radio.PowerSet{Pct: &v} }

	tests := []struct {
		name    string
		set     radio.PowerSet
		ceiling int
		want    int
		fail    bool
	}{
		{name: "watts pass through", set: watts(50), ceiling: 100, want: 50},
		{name: "clamped to the ceiling", set: watts(150), ceiling: 100, want: 100},
		{name: "clamped to the floor", set: watts(1), ceiling: 100, want: minPowerW},
		{name: "full scale", set: pct(100), ceiling: 100, want: 100},
		{name: "half scale on a 200 W radio", set: pct(50), ceiling: 200, want: 100},
		{name: "full scale on a 200 W radio", set: pct(100), ceiling: 200, want: 200},
		// The FTX-1's bare field head.
		{name: "10 W head", set: pct(100), ceiling: ftx1HeadMaxW, want: 10},
		{name: "10 W head clamps watts", set: watts(100), ceiling: ftx1HeadMaxW, want: 10},
		{name: "no field set", set: radio.PowerSet{}, ceiling: 100, fail: true},
		{name: "both fields set", set: radio.PowerSet{Watts: ptr(50.0), Pct: ptr(50.0)}, ceiling: 100, fail: true},
		{name: "percentage out of range", set: pct(120), ceiling: 100, fail: true},
		{name: "negative percentage", set: pct(-1), ceiling: 100, fail: true},
		{name: "not a number", set: watts(math.NaN()), ceiling: 100, fail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := wattsFromSet(tt.set, tt.ceiling)
			if tt.fail {
				if err == nil {
					t.Fatalf("wattsFromSet = %d, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("wattsFromSet: %v", err)
			}
			if got != tt.want {
				t.Errorf("wattsFromSet = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestRawFromSet covers the two models whose PC is an uncalibrated 000-255
// index rather than watts. Their manuals calibrate it against nothing, so a
// request in watts is refused rather than converted with a figure remoses
// invented about a transmitter's output.
func TestRawFromSet(t *testing.T) {
	pct := func(v float64) radio.PowerSet { return radio.PowerSet{Pct: &v} }

	tests := []struct {
		name string
		set  radio.PowerSet
		want int
		fail string
	}{
		{name: "full scale", set: pct(100), want: powerRawMax},
		{name: "half rounds up", set: pct(50), want: 128}, // 127.5
		{name: "zero", set: pct(0), want: 0},
		{name: "watts are refused", set: radio.PowerSet{Watts: ptr(50.0)}, fail: "percentage"},
		{name: "no field set", set: radio.PowerSet{}, fail: "either watts or pct"},
		{name: "both fields set", set: radio.PowerSet{Watts: ptr(50.0), Pct: ptr(50.0)},
			fail: "not both"},
		{name: "percentage out of range", set: pct(120), fail: "outside 0..100"},
		{name: "negative percentage", set: pct(-1), fail: "outside 0..100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rawFromSet(tt.set)
			if tt.fail != "" {
				if err == nil {
					t.Fatalf("rawFromSet = %d, want an error", got)
				}
				if !strings.Contains(err.Error(), tt.fail) {
					t.Errorf("error %q does not explain itself (want %q in it)", err, tt.fail)
				}
				return
			}
			if err != nil {
				t.Fatalf("rawFromSet: %v", err)
			}
			if got != tt.want {
				t.Errorf("rawFromSet = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestPowerFromRaw covers the read side of the same two. Watts must stay nil:
// the index has no documented watt meaning, and the FTdx9000 is sold as both a
// 200 W and a 400 W radio with no CAT command that says which is on the desk.
func TestPowerFromRaw(t *testing.T) {
	for _, tt := range []struct {
		n       int
		wantPct float64
	}{
		{0, 0},
		{51, 20},
		{255, 100},
	} {
		p := powerFromRaw(tt.n)
		if p.Pct != tt.wantPct {
			t.Errorf("powerFromRaw(%d).Pct = %v, want %v", tt.n, p.Pct, tt.wantPct)
		}
		if p.Native != tt.n {
			t.Errorf("powerFromRaw(%d).Native = %d", tt.n, p.Native)
		}
		if p.Watts != nil {
			t.Errorf("powerFromRaw(%d).Watts = %v, want none", tt.n, *p.Watts)
		}
	}
}

// TestPowerFromWatts covers the read side. There is no per-mode ceiling on a
// Yaesu — no manual here derates AM the way a TS-590 does — so the percentage
// is a straight fraction of the model's maximum.
func TestPowerFromWatts(t *testing.T) {
	for _, tt := range []struct {
		w, ceiling int
		wantPct    float64
	}{
		{50, 100, 50},
		{100, 100, 100},
		{100, 200, 50},
		{5, 10, 50},
	} {
		p := powerFromWatts(tt.w, tt.ceiling)
		if p.Pct != tt.wantPct {
			t.Errorf("powerFromWatts(%d, %d).Pct = %v, want %v", tt.w, tt.ceiling, p.Pct, tt.wantPct)
		}
		if p.Native != tt.w || p.Watts == nil || *p.Watts != float64(tt.w) {
			t.Errorf("powerFromWatts(%d, %d) = %+v, want %d watts", tt.w, tt.ceiling, p, tt.w)
		}
	}
}

// TestWidthTables pins the rungs the manuals print, at the boundaries where a
// transcription slip would otherwise be invisible.
func TestWidthTables(t *testing.T) {
	tests := []struct {
		name    string
		ladder  []int
		index   int
		want    int
		unknown bool
	}{
		// FT-991A and FT-891, whose table is byte-identical.
		{name: "991A SSB narrow default", ladder: widthsFT991A().ssbNarrow, index: 0, want: 1500},
		{name: "991A SSB narrow bottom", ladder: widthsFT991A().ssbNarrow, index: 1, want: 200},
		{name: "991A SSB narrow top", ladder: widthsFT991A().ssbNarrow, index: 9, want: 1800},
		{name: "991A SSB narrow past the end", ladder: widthsFT991A().ssbNarrow, index: 10, unknown: true},
		{name: "991A SSB wide default", ladder: widthsFT991A().ssbWide, index: 0, want: 2400},
		{name: "991A SSB wide has a hole", ladder: widthsFT991A().ssbWide, index: 5, unknown: true},
		{name: "991A SSB wide bottom", ladder: widthsFT991A().ssbWide, index: 9, want: 1800},
		{name: "991A SSB wide top", ladder: widthsFT991A().ssbWide, index: 21, want: 3200},
		{name: "991A CW narrow bottom", ladder: widthsFT991A().cwNarrow, index: 1, want: 50},
		{name: "991A CW narrow top", ladder: widthsFT991A().cwNarrow, index: 10, want: 500},
		{name: "991A CW wide top", ladder: widthsFT991A().cwWide, index: 17, want: 3000},
		// The CW and digital columns differ only in their defaults, which is
		// the whole reason they are separate ladders.
		{name: "991A CW narrow default", ladder: widthsFT991A().cwNarrow, index: 0, want: 500},
		{name: "991A digi narrow default", ladder: widthsFT991A().digiNarrow, index: 0, want: 300},
		{name: "991A CW wide default", ladder: widthsFT991A().cwWide, index: 0, want: 2400},
		{name: "991A digi wide default", ladder: widthsFT991A().digiWide, index: 0, want: 500},

		// FTdx101: one column per group, default unknown.
		{name: "dx101 SSB default varies", ladder: widthsFTdx101().ssbWide, index: 0, unknown: true},
		{name: "dx101 SSB bottom", ladder: widthsFTdx101().ssbWide, index: 1, want: 300},
		{name: "dx101 SSB top", ladder: widthsFTdx101().ssbWide, index: 21, want: 3200},
		{name: "dx101 SSB past the end", ladder: widthsFTdx101().ssbWide, index: 22, unknown: true},
		{name: "dx101 digi bottom", ladder: widthsFTdx101().digiWide, index: 1, want: 50},
		{name: "dx101 digi top", ladder: widthsFTdx101().digiWide, index: 18, want: 3000},
		{name: "dx101 digi past the end", ladder: widthsFTdx101().digiWide, index: 19, unknown: true},

		// FT-710 and FTX-1: the same ladders extended, with different rungs in
		// the middle of SSB (2250 and 2450 where the FTdx101 has 2200 and 2300).
		{name: "710 SSB 12", ladder: widthsFT710().ssbWide, index: 12, want: 2250},
		{name: "710 SSB 14", ladder: widthsFT710().ssbWide, index: 14, want: 2450},
		{name: "710 SSB top", ladder: widthsFT710().ssbWide, index: 23, want: 4000},
		{name: "710 digi top", ladder: widthsFT710().digiWide, index: 21, want: 4000},
		{name: "710 digi past the end", ladder: widthsFT710().digiWide, index: 22, unknown: true},

		// FT-950: like the FT-991A's until the middle of wide SSB, where it
		// steps 2250/2400/2450 against that radio's 2200/2300/2400, and it
		// stops at index 20. Its CW and RTTY columns are the coarse ones: seven
		// rungs from 100 Hz where the FTdx1200 has ten from 50.
		{name: "950 SSB narrow default", ladder: widthsFT950().ssbNarrow, index: 0, want: 1800},
		{name: "950 SSB narrow bottom", ladder: widthsFT950().ssbNarrow, index: 1, want: 200},
		{name: "950 SSB narrow top", ladder: widthsFT950().ssbNarrow, index: 9, want: 1800},
		{name: "950 SSB wide default", ladder: widthsFT950().ssbWide, index: 0, want: 2400},
		{name: "950 SSB wide bottom", ladder: widthsFT950().ssbWide, index: 9, want: 1800},
		{name: "950 SSB wide 12", ladder: widthsFT950().ssbWide, index: 12, want: 2250},
		{name: "950 SSB wide 14", ladder: widthsFT950().ssbWide, index: 14, want: 2450},
		{name: "950 SSB wide top", ladder: widthsFT950().ssbWide, index: 20, want: 3000},
		{name: "950 SSB wide past the end", ladder: widthsFT950().ssbWide, index: 21, unknown: true},
		{name: "950 CW narrow has holes at 1 and 2", ladder: widthsFT950().cwNarrow, index: 1, unknown: true},
		{name: "950 CW narrow bottom", ladder: widthsFT950().cwNarrow, index: 3, want: 100},
		{name: "950 CW narrow top", ladder: widthsFT950().cwNarrow, index: 7, want: 500},
		{name: "950 CW wide bottom", ladder: widthsFT950().cwWide, index: 7, want: 500},
		{name: "950 CW wide top", ladder: widthsFT950().cwWide, index: 13, want: 2400},
		// CW and RTTY/PKT differ only in their defaults, which is why they are
		// separate ladders.
		{name: "950 CW narrow default", ladder: widthsFT950().cwNarrow, index: 0, want: 500},
		{name: "950 digi narrow default", ladder: widthsFT950().digiNarrow, index: 0, want: 300},
		{name: "950 CW wide default", ladder: widthsFT950().cwWide, index: 0, want: 2400},
		{name: "950 digi wide default", ladder: widthsFT950().digiWide, index: 0, want: 500},

		// FTdx1200 and FTdx3000, which share the table to the digit. SSB runs
		// past 3200 to 4000, and CW and RTTY/PSK agree even in their defaults.
		{name: "dx1200 SSB narrow default", ladder: widthsFTdx1200().ssbNarrow, index: 0, want: 1500},
		{name: "dx1200 SSB narrow top", ladder: widthsFTdx1200().ssbNarrow, index: 9, want: 1800},
		{name: "dx1200 SSB wide 12", ladder: widthsFTdx1200().ssbWide, index: 12, want: 2200},
		{name: "dx1200 SSB wide 14", ladder: widthsFTdx1200().ssbWide, index: 14, want: 2400},
		{name: "dx1200 SSB wide top", ladder: widthsFTdx1200().ssbWide, index: 25, want: 4000},
		{name: "dx1200 SSB wide past the end", ladder: widthsFTdx1200().ssbWide, index: 26, unknown: true},
		{name: "dx1200 CW narrow bottom", ladder: widthsFTdx1200().cwNarrow, index: 1, want: 50},
		{name: "dx1200 CW narrow top", ladder: widthsFTdx1200().cwNarrow, index: 10, want: 500},
		{name: "dx1200 CW wide top", ladder: widthsFTdx1200().cwWide, index: 16, want: 2400},
		{name: "dx1200 CW wide past the end", ladder: widthsFTdx1200().cwWide, index: 17, unknown: true},
		{name: "dx1200 digi matches CW", ladder: widthsFTdx1200().digiWide, index: 0, want: 2400},

		// FTdx5000: narrow SSB stops at 7, wide SSB starts at 7, and index 14
		// is printed "- - - -" — an index the manual defines for no width.
		{name: "5000 SSB narrow top", ladder: widthsFTdx5000().ssbNarrow, index: 7, want: 1500},
		{name: "5000 SSB narrow past the end", ladder: widthsFTdx5000().ssbNarrow, index: 8, unknown: true},
		{name: "5000 SSB wide bottom", ladder: widthsFTdx5000().ssbWide, index: 7, want: 1500},
		{name: "5000 SSB wide 12", ladder: widthsFTdx5000().ssbWide, index: 12, want: 2250},
		{name: "5000 SSB wide 13", ladder: widthsFTdx5000().ssbWide, index: 13, want: 2400},
		{name: "5000 SSB wide 14 is undefined", ladder: widthsFTdx5000().ssbWide, index: 14, unknown: true},
		{name: "5000 SSB wide 15", ladder: widthsFTdx5000().ssbWide, index: 15, want: 2500},
		{name: "5000 SSB wide top", ladder: widthsFTdx5000().ssbWide, index: 25, want: 4000},
		{name: "5000 CW narrow default", ladder: widthsFTdx5000().cwNarrow, index: 0, want: 500},
		{name: "5000 digi narrow default", ladder: widthsFTdx5000().digiNarrow, index: 0, want: 300},
		{name: "5000 CW wide default", ladder: widthsFTdx5000().cwWide, index: 0, want: 2400},
		{name: "5000 digi wide default", ladder: widthsFTdx5000().digiWide, index: 0, want: 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := widthAt(tt.ladder, tt.index)
			if tt.unknown {
				if ok {
					t.Fatalf("widthAt(%d) = %d, want nothing", tt.index, got)
				}
				return
			}
			if !ok || got != tt.want {
				t.Errorf("widthAt(%d) = (%d, %v), want %d", tt.index, got, ok, tt.want)
			}
		})
	}

	// Negative indices cannot come off the wire, but widthAt is the only guard.
	if _, ok := widthAt(widthsFT710().ssbWide, -1); ok {
		t.Error("widthAt accepted a negative index")
	}
}

// TestWidthsAny separates "this model has no bandwidth table" from "this mode
// has no column", which are different answers with different consequences: the
// first makes Caps.FilterWidth false and SetFilterWidth refuse outright.
func TestWidthsAny(t *testing.T) {
	for _, w := range []widths{
		widthsFT991A(), widthsFTdx101(), widthsFT710(),
		widthsFT950(), widthsFTdx1200(), widthsFTdx5000(),
	} {
		if !w.any() {
			t.Error("a populated table reported no columns")
		}
	}
	// The FTdx9000's, whose SH is the WIDTH knob's position rather than an
	// index into anything.
	if (widths{}).any() {
		t.Error("an empty table reported a column")
	}
}

// TestLadder covers the column choice: which group a mode belongs to, and where
// SH carries no bandwidth at all.
func TestLadder(t *testing.T) {
	dx := widthsFTdx101()
	f991 := widthsFT991A()

	tests := []struct {
		name       string
		w          widths
		mode       radio.Mode
		data       bool
		narrow     bool
		wantAt1    int
		wantAbsent bool
	}{
		{name: "SSB", w: dx, mode: radio.ModeUSB, wantAt1: 300},
		{name: "LSB", w: dx, mode: radio.ModeLSB, wantAt1: 300},
		// DATA is grouped with CW, RTTY and PSK rather than with SSB, which the
		// FT-710 and FTX-1 tables say outright.
		{name: "USB-DATA", w: dx, mode: radio.ModeUSB, data: true, wantAt1: 50},
		{name: "LSB-DATA", w: dx, mode: radio.ModeLSB, data: true, wantAt1: 50},
		{name: "CW", w: dx, mode: radio.ModeCW, wantAt1: 50},
		{name: "CW-R", w: dx, mode: radio.ModeCWR, wantAt1: 50},
		{name: "FSK", w: dx, mode: radio.ModeFSK, wantAt1: 50},
		{name: "PSK", w: dx, mode: radio.ModePSK, wantAt1: 50},
		// AM and FM have no ladder anywhere: the older tables have no column
		// for them and the newer ones hold a single fixed value per mode code,
		// chosen by distinctions radio.Mode does not carry.
		{name: "AM", w: dx, mode: radio.ModeAM, wantAbsent: true},
		{name: "FM", w: dx, mode: radio.ModeFM, wantAbsent: true},
		{name: "DATA-FM", w: dx, mode: radio.ModeFM, data: true, wantAbsent: true},
		{name: "C4FM", w: dx, mode: radio.ModeC4FM, wantAbsent: true},
		{name: "unknown mode", w: dx, mode: radio.ModeUnknown, wantAbsent: true},
		// Narrow moves the whole column on the FT-991A and FT-891 and nothing
		// at all on the newer radios.
		{name: "991A SSB wide", w: f991, mode: radio.ModeUSB, wantAt1: 0},
		{name: "991A SSB narrow", w: f991, mode: radio.ModeUSB, narrow: true, wantAt1: 200},
		{name: "dx101 narrow is the same column", w: dx, mode: radio.ModeUSB, narrow: true, wantAt1: 300},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, ok := tt.w.ladder(tt.mode, tt.data, tt.narrow)
			if tt.wantAbsent {
				if ok {
					t.Fatalf("ladder(%s) = %v, want none", tt.mode, l)
				}
				return
			}
			if !ok {
				t.Fatalf("ladder(%s) reported none", tt.mode)
			}
			got, _ := widthAt(l, 1)
			if got != tt.wantAt1 {
				t.Errorf("ladder(%s)[1] = %d, want %d", tt.mode, got, tt.wantAt1)
			}
		})
	}
}

// TestSnapWidth covers the rule SetFilterWidth turns a request in Hz into an
// index with: the closest rung at or below, clamped to the ends. Index 0 is
// never chosen, because it is the column's default rather than a rung.
func TestSnapWidth(t *testing.T) {
	dx := widthsFTdx101()
	f991 := widthsFT991A()

	tests := []struct {
		name      string
		ladder    []int
		hz        int
		wantIndex int
		wantWidth int
	}{
		{name: "exact rung", ladder: dx.digiWide, hz: 500, wantIndex: 10, wantWidth: 500},
		{name: "rounds down", ladder: dx.digiWide, hz: 520, wantIndex: 10, wantWidth: 500},
		{name: "just below the next rung", ladder: dx.digiWide, hz: 599, wantIndex: 10, wantWidth: 500},
		{name: "below the ladder clamps up", ladder: dx.digiWide, hz: 10, wantIndex: 1, wantWidth: 50},
		{name: "above the ladder clamps down", ladder: dx.digiWide, hz: 9000, wantIndex: 18, wantWidth: 3000},
		{name: "SSB", ladder: dx.ssbWide, hz: 2400, wantIndex: 14, wantWidth: 2400},
		{name: "SSB rounds down", ladder: dx.ssbWide, hz: 2450, wantIndex: 14, wantWidth: 2400},
		// The FT-991A's wide SSB column starts at index 9, so a narrow request
		// clamps up to 1800 rather than reaching the narrow column's 200.
		{name: "991A SSB wide floor", ladder: f991.ssbWide, hz: 300, wantIndex: 9, wantWidth: 1800},
		{name: "991A SSB narrow floor", ladder: f991.ssbNarrow, hz: 100, wantIndex: 1, wantWidth: 200},
		{name: "991A SSB narrow rounds down", ladder: f991.ssbNarrow, hz: 1000, wantIndex: 4, wantWidth: 850},
		{name: "991A SSB narrow exact", ladder: f991.ssbNarrow, hz: 1100, wantIndex: 5, wantWidth: 1100},
		// Index 0 duplicates index 7 in this column; the explicit rung wins.
		{name: "991A prefers the explicit rung", ladder: f991.ssbNarrow, hz: 1500, wantIndex: 7, wantWidth: 1500},
		{name: "991A CW wide floor", ladder: f991.cwWide, hz: 50, wantIndex: 10, wantWidth: 500},
		{name: "710 top", ladder: widthsFT710().ssbWide, hz: 5000, wantIndex: 23, wantWidth: 4000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			index, width, ok := snapWidth(tt.ladder, tt.hz)
			if !ok {
				t.Fatal("snapWidth found no rung")
			}
			if index != tt.wantIndex || width != tt.wantWidth {
				t.Errorf("snapWidth(%d) = (index %d, %d Hz), want (index %d, %d Hz)",
					tt.hz, index, width, tt.wantIndex, tt.wantWidth)
			}
			// Whatever it picked has to read back as the width it claimed.
			if back, ok := widthAt(tt.ladder, index); !ok || back != width {
				t.Errorf("widthAt(%d) = (%d, %v), want %d", index, back, ok, width)
			}
		})
	}

	// A column with nothing but a default has no rung to snap to.
	if index, w, ok := snapWidth([]int{2400}, 2400); ok {
		t.Errorf("snapWidth on a default-only column = (%d, %d), want none", index, w)
	}
}

// TestNormaliseModel covers the display string, which is only ever cosmetic —
// New has already refused an unknown model by the time it is read.
func TestNormaliseModel(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"ft-991a", "Yaesu FT-991A"},
		{"FTDX-10", "Yaesu FTdx10"},
		{"", ""},
		{"  ", ""},
		{"ft-857", "ft-857"},
	} {
		if got := normaliseModel(tt.in); got != tt.want {
			t.Errorf("normaliseModel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if !strings.HasPrefix(normaliseModel("generic"), "generic") {
		t.Errorf("normaliseModel(generic) = %q", normaliseModel("generic"))
	}
}
