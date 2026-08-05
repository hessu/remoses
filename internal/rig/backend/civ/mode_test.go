package civ

import (
	"fmt"
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

func TestModeMapping(t *testing.T) {
	tests := []struct {
		b byte
		m radio.Mode
	}{
		{0x00, radio.ModeLSB},
		{0x01, radio.ModeUSB},
		{0x02, radio.ModeAM},
		{0x03, radio.ModeCW},
		{0x04, radio.ModeFSK}, // the rig's RTTY
		{0x05, radio.ModeFM},
		{0x07, radio.ModeCWR},
		{0x08, radio.ModeFSKR}, // RTTY-R
		{0x12, radio.ModePSK},  // a literal byte, not decimal 12
		{0x13, radio.ModePSKR},
	}
	for _, tc := range tests {
		t.Run(tc.m.String(), func(t *testing.T) {
			got, ok := modeFromByte(tc.b)
			if !ok || got != tc.m {
				t.Errorf("modeFromByte(%#02X) = %s, %v; want %s", tc.b, got, ok, tc.m)
			}
			b, ok := modeByte(tc.m)
			if !ok || b != tc.b {
				t.Errorf("modeByte(%s) = %#02X, %v; want %#02X", tc.m, b, ok, tc.b)
			}
		})
	}
}

func TestModeMappingRejects(t *testing.T) {
	// 06 and 09..11 are unassigned on this radio, as is anything above 13.
	for _, b := range []byte{0x06, 0x09, 0x0A, 0x11, 0x14, 0xFF} {
		if m, ok := modeFromByte(b); ok {
			t.Errorf("modeFromByte(%#02X) = %s; want not ok", b, m)
		}
	}
	if b, ok := modeByte(radio.ModeUnknown); ok {
		t.Errorf("modeByte(unknown) = %#02X; want not ok", b)
	}
}

func TestSupportsDataMode(t *testing.T) {
	on := []radio.Mode{radio.ModeLSB, radio.ModeUSB, radio.ModeAM, radio.ModeFM}
	off := []radio.Mode{radio.ModeCW, radio.ModeCWR, radio.ModeFSK, radio.ModeFSKR, radio.ModePSK, radio.ModePSKR}
	for _, m := range on {
		if !supportsDataMode(m) {
			t.Errorf("supportsDataMode(%s) = false", m)
		}
	}
	for _, m := range off {
		if supportsDataMode(m) {
			t.Errorf("supportsDataMode(%s) = true", m)
		}
	}
}

func TestKeyerSpeedMapping(t *testing.T) {
	// The top of the range is per model: 48 wpm on most, 60 on the IC-718.
	for _, maxWPM := range []int{defaultMaxWPM, 60} {
		t.Run(fmt.Sprintf("max%dwpm", maxWPM), func(t *testing.T) {
			// The endpoints are what each reference states; everything between
			// them is the documented linear assumption.
			if got := wpmFromNative(0, maxWPM); got != minWPM {
				t.Errorf("wpmFromNative(0) = %d, want %d", got, minWPM)
			}
			if got := wpmFromNative(levelMax, maxWPM); got != maxWPM {
				t.Errorf("wpmFromNative(255) = %d, want %d", got, maxWPM)
			}
			if got := nativeFromWPM(minWPM, maxWPM); got != 0 {
				t.Errorf("nativeFromWPM(%d) = %d, want 0", minWPM, got)
			}
			if got := nativeFromWPM(maxWPM, maxWPM); got != levelMax {
				t.Errorf("nativeFromWPM(%d) = %d, want %d", maxWPM, got, levelMax)
			}
			// Every speed the rig can be asked for must survive the round trip,
			// or the value read back would fight the value set.
			for wpm := minWPM; wpm <= maxWPM; wpm++ {
				n := nativeFromWPM(wpm, maxWPM)
				if n < 0 || n > levelMax {
					t.Fatalf("nativeFromWPM(%d) = %d, out of range", wpm, n)
				}
				if got := wpmFromNative(n, maxWPM); got != wpm {
					t.Errorf("round trip of %d wpm gave %d (native %d)", wpm, got, n)
				}
			}
			// Out of range requests clamp rather than failing.
			if got := nativeFromWPM(1, maxWPM); got != 0 {
				t.Errorf("nativeFromWPM(1) = %d, want 0", got)
			}
			if got := nativeFromWPM(1000, maxWPM); got != levelMax {
				t.Errorf("nativeFromWPM(1000) = %d, want %d", got, levelMax)
			}
			if got := wpmFromNative(1000, maxWPM); got != maxWPM {
				t.Errorf("wpmFromNative(1000) = %d, want %d", got, maxWPM)
			}
		})
	}
}

func TestFilterWidthIndex(t *testing.T) {
	tests := []struct {
		name string
		mode radio.Mode
		hz   int
		want int
	}{
		{"cw 50 Hz", radio.ModeCW, 50, 0},
		{"cw 250 Hz", radio.ModeCW, 250, 4},
		{"cw 500 Hz", radio.ModeCW, 500, 9},
		{"cw snaps down between steps", radio.ModeCW, 279, 4},
		{"cw below the first step clamps", radio.ModeCW, 10, 0},
		{"cw in the gap below 600 Hz", radio.ModeCW, 550, 9},
		{"ssb 600 Hz", radio.ModeUSB, 600, 10},
		{"ssb 2400 Hz", radio.ModeUSB, 2400, 28},
		{"ssb 3600 Hz", radio.ModeUSB, 3600, 40},
		{"ssb above the top clamps", radio.ModeLSB, 5000, 40},
		{"rtty stops at 2.7 kHz", radio.ModeFSK, 3600, 31},
		{"rtty 2700 Hz", radio.ModeFSKR, 2700, 31},
		{"psk uses the ssb table", radio.ModePSK, 3600, 40},
		{"am 200 Hz", radio.ModeAM, 200, 0},
		{"am 6 kHz", radio.ModeAM, 6000, 29},
		{"am 10 kHz", radio.ModeAM, 10000, 49},
		{"am above the top clamps", radio.ModeAM, 20000, 49},
		{"am below the first step clamps", radio.ModeAM, 100, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := filterWidthIndex(tc.mode, tc.hz)
			if err != nil {
				t.Fatalf("filterWidthIndex(%s, %d): %v", tc.mode, tc.hz, err)
			}
			if got != tc.want {
				t.Errorf("filterWidthIndex(%s, %d) = %d, want %d", tc.mode, tc.hz, got, tc.want)
			}
		})
	}
}

func TestFilterWidthIndexRejects(t *testing.T) {
	if _, err := filterWidthIndex(radio.ModeFM, 12000); err == nil {
		t.Error("filterWidthIndex accepted FM, whose filters are fixed")
	}
	if _, err := filterWidthIndex(radio.ModeCW, 0); err == nil {
		t.Error("filterWidthIndex accepted a zero width")
	}
	if _, err := filterWidthIndex(radio.ModeUnknown, 500); err == nil {
		t.Error("filterWidthIndex accepted an unknown mode")
	}
}
