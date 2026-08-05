package civ

import (
	"fmt"

	"github.com/hessu/remoses/internal/radio"
)

// Scales shared by the level (14 xx) and meter (15 xx) commands.
const (
	// levelMax is the top of every 0000-0255 relative scale. It is not a byte
	// range: the value is sent as four BCD digits in two bytes, so 255 is 02 55.
	levelMax = 255

	// sMeterScale is the raw range of command 15 02. The reference gives three
	// calibration points (0000 = S0, 0120 = S9, 0241 = S9+60 dB), which is not
	// enough to interpolate an S value honestly, so radio.Meter.S is left nil
	// and clients get the raw reading and its scale.
	sMeterScale = 255

	filterSlots = 3
)

// Keyer speed range for command 14 0C. The endpoints are from the reference
// ("0000 = 6 wpm ~ 0255 = 48 wpm"); the linear interpolation between them is
// an assumption, since the guide tabulates only the two extremes.
const (
	minWPM = 6
	maxWPM = 48
)

// Mode bytes of commands 01/04/06. These are literal byte values, so PSK and
// PSK-R are 0x12 and 0x13 rather than decimal 12 and 13, and 0x06 is unused.
var modeBytes = map[byte]radio.Mode{
	0x00: radio.ModeLSB,
	0x01: radio.ModeUSB,
	0x02: radio.ModeAM,
	0x03: radio.ModeCW,
	0x04: radio.ModeFSK, // the rig calls this RTTY
	0x05: radio.ModeFM,
	0x07: radio.ModeCWR,
	0x08: radio.ModeFSKR, // RTTY-R
	0x12: radio.ModePSK,
	0x13: radio.ModePSKR,
	// D-STAR and image modes on the VHF/UHF and microwave sets. Again literal
	// bytes: DV is 0x17, not decimal 17. Which radios accept them is a model
	// property, but the codes themselves are shared across the family.
	0x17: radio.ModeDV,
	0x22: radio.ModeDD,
	0x23: radio.ModeATV,
}

// modeFromByte maps a wire mode byte to a radio.Mode.
func modeFromByte(b byte) (radio.Mode, bool) {
	m, ok := modeBytes[b]
	return m, ok
}

// modeByte maps a radio.Mode to its wire byte.
func modeByte(m radio.Mode) (byte, bool) {
	for b, x := range modeBytes {
		if x == m {
			return b, true
		}
	}
	return 0, false
}

// supportsDataMode reports whether command 1A 06 means anything in this mode.
//
// The CI-V guide does not say which modes carry a data setting; SSB, AM and FM
// is taken from the operating manual, where DATA appears only on those. The
// consequence of being wrong is an NG on 1A 06, not a mis-set radio.
func supportsDataMode(m radio.Mode) bool {
	switch m {
	case radio.ModeLSB, radio.ModeUSB, radio.ModeAM, radio.ModeFM:
		return true
	}
	return false
}

// wpmFromNative converts a command 14 0C value to words per minute.
func wpmFromNative(n int) int {
	n = min(max(n, 0), levelMax)
	return minWPM + (n*(maxWPM-minWPM)+levelMax/2)/levelMax
}

// nativeFromWPM converts words per minute to a command 14 0C value, clamping to
// the rig's range rather than refusing: a request for 60 wpm is better served
// as 48 than as an error, and the honest range is published in radio.Caps.
func nativeFromWPM(wpm int) int {
	wpm = min(max(wpm, minWPM), maxWPM)
	span := maxWPM - minWPM
	return ((wpm-minWPM)*levelMax*2 + span) / (2 * span)
}

// filterWidthIndex maps a width in Hz to the mode-dependent index of command
// 1A 03, transcribed from the reference's "IF filter width settings" table:
//
//	SSB/CW/RTTY/PSK   0-9    50 Hz - 500 Hz     in 50 Hz steps
//	SSB/CW/PSK        10-40  600 Hz - 3.6 kHz   in 100 Hz steps
//	RTTY              10-31  600 Hz - 2.7 kHz   in 100 Hz steps
//	AM                0-49   200 Hz - 10.0 kHz  in 200 Hz steps
//
// A request between steps snaps down to the next available width, and one
// outside the table's range clamps to its ends.
func filterWidthIndex(m radio.Mode, hz int) (int, error) {
	if hz <= 0 {
		return 0, fmt.Errorf("civ: filter width %d Hz is not a width", hz)
	}
	switch m {
	case radio.ModeAM:
		return clampIndex(hz/200-1, 0, 49), nil
	case radio.ModeFM:
		// FM appears in no row of the table: its filters are fixed.
		return 0, fmt.Errorf("civ: filter width is not adjustable in FM")
	case radio.ModeFSK, radio.ModeFSKR:
		return ssbFamilyIndex(hz, 31), nil
	case radio.ModeLSB, radio.ModeUSB, radio.ModeCW, radio.ModeCWR, radio.ModePSK, radio.ModePSKR:
		return ssbFamilyIndex(hz, 40), nil
	case radio.ModeDV, radio.ModeDD, radio.ModeATV:
		// The digital and image modes have fixed channel bandwidths; no row of
		// the IF filter table covers them.
		return 0, fmt.Errorf("civ: filter width is not adjustable in %s", m)
	}
	return 0, fmt.Errorf("civ: filter width cannot be set in mode %s", m)
}

// ssbFamilyIndex implements the two-part table shared by SSB, CW, RTTY and PSK,
// whose upper end differs only in where it stops.
func ssbFamilyIndex(hz, maxIdx int) int {
	if hz < 600 {
		// 50 Hz steps, 50..500 Hz at indices 0..9.
		return clampIndex(hz/50-1, 0, 9)
	}
	// 100 Hz steps, 600 Hz upwards from index 10.
	return clampIndex(10+(hz-600)/100, 10, maxIdx)
}

func clampIndex(v, lo, hi int) int { return min(max(v, lo), hi) }
