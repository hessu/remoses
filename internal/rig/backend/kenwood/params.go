package kenwood

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/hessu/remoses/internal/radio"
)

// --- Frequency (FA/FB) ------------------------------------------------------

// freqDigits is the width of the FA/FB frequency field. The rig demands exactly
// this many digits — "Blank digits must be entered as 0" — so a short or long
// parameter is a syntax error rather than a tolerated shorthand.
const freqDigits = 11

// maxFrequencyHz is what fits in freqDigits. It is far above anything the rig
// can tune; the check exists to keep a bad caller from emitting a malformed
// frame that would desynchronise the command stream.
const maxFrequencyHz uint64 = 99_999_999_999

func formatFrequency(hz uint64) (string, error) {
	if hz > maxFrequencyHz {
		return "", fmt.Errorf("kenwood: frequency %d Hz does not fit the %d-digit FA/FB field", hz, freqDigits)
	}
	return fmt.Sprintf("%0*d", freqDigits, hz), nil
}

func parseFrequency(s string) (uint64, error) {
	if len(s) != freqDigits {
		return 0, fmt.Errorf("kenwood: frequency field %q is %d digits, want %d", s, len(s), freqDigits)
	}
	hz, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("kenwood: frequency field %q is not numeric", s)
	}
	return hz, nil
}

// --- Mode (MD, and the mode digit inside IF) --------------------------------

// modeDigits maps the MD parameter to a radio.Mode.
//
// Digits 0 and 8 are deliberately absent: the reference calls them "None
// (setting failure)", not modes. Mapping them to ModeUnknown would let a failed
// set command overwrite a perfectly good cached mode, so a frame carrying one
// reports nothing about the mode at all.
var modeDigits = map[byte]radio.Mode{
	'1': radio.ModeLSB,
	'2': radio.ModeUSB,
	'3': radio.ModeCW,
	'4': radio.ModeFM,
	'5': radio.ModeAM,
	'6': radio.ModeFSK,
	'7': radio.ModeCWR,
	'9': radio.ModeFSKR,
}

// decodeMode reports the mode a digit names, and false for the two failure
// values and anything unrecognised.
func decodeMode(d byte) (radio.Mode, bool) {
	m, ok := modeDigits[d]
	return m, ok
}

// encodeMode is decodeMode's inverse. It rejects the PSK modes explicitly: the
// TS-590 decodes PSK in software through its data modes and has no MD value for
// it, so silently substituting USB-DATA would put the operator on the air in a
// mode they did not ask for.
func encodeMode(m radio.Mode) (byte, error) {
	for d, mm := range modeDigits {
		if mm == m {
			return d, nil
		}
	}
	return 0, fmt.Errorf("kenwood: the TS-590 has no MD value for mode %s", m)
}

// supportsDataMode reports whether DA may be set in this mode. The reference is
// explicit: "You can use this command in LSB, USB, FM, and AM mode. When used in
// CW, FSK, an error occurs."
//
// This is about the mode, not the radio: whether the radio has a DATA mode at
// all is Model.DataMode, and Model.checkDataMode asks both questions.
func supportsDataMode(m radio.Mode) bool {
	switch m {
	case radio.ModeLSB, radio.ModeUSB, radio.ModeFM, radio.ModeAM:
		return true
	}
	return false
}

// --- Mode (OM, on the TS-890S and TS-990S) ----------------------------------

// omMode is one row of the OM P2 table: a mode and whether the code is its DATA
// variant.
type omMode struct {
	mode radio.Mode
	data bool
}

// omCodes maps the OM P2 parameter to a mode and its DATA flag.
//
// The reason this table exists rather than a translation to MD is the last four
// rows: OM has no separate DATA command, so C, D, E and F *are* LSB-D, USB-D,
// FM-D and AM-D. Encoding USB with DATA has to produce D, and decoding C has to
// report LSB with the DATA flag set, or a radio in LSB-D would be published as
// plain LSB and the operator's data path would be invisible.
//
// Codes 0 and 8 are "Unused" in the reference, exactly like MD's setting-failure
// values, so they decode to nothing and are never encoded.
var omCodes = map[byte]omMode{
	'1': {radio.ModeLSB, false},
	'2': {radio.ModeUSB, false},
	'3': {radio.ModeCW, false},
	'4': {radio.ModeFM, false},
	'5': {radio.ModeAM, false},
	'6': {radio.ModeFSK, false},
	'7': {radio.ModeCWR, false},
	'9': {radio.ModeFSKR, false},
	'A': {radio.ModePSK, false},
	'B': {radio.ModePSKR, false},
	'C': {radio.ModeLSB, true},
	'D': {radio.ModeUSB, true},
	'E': {radio.ModeFM, true},
	'F': {radio.ModeAM, true},
}

// decodeOMMode reports the mode and DATA flag a P2 code names, and false for the
// two unused values and anything unrecognised. Lower case is accepted because
// the whole protocol is case-insensitive.
func decodeOMMode(c byte) (radio.Mode, bool, bool) {
	if c >= 'a' && c <= 'z' {
		c -= 32
	}
	v, ok := omCodes[c]
	return v.mode, v.data, ok
}

// encodeOMMode is decodeOMMode's inverse.
func encodeOMMode(m radio.Mode, data bool) (byte, error) {
	for c, v := range omCodes {
		if v.mode == m && v.data == data {
			return c, nil
		}
	}
	if data {
		return 0, fmt.Errorf("kenwood: OM has no DATA code for mode %s", m)
	}
	return 0, fmt.Errorf("kenwood: OM has no value for mode %s", m)
}

// --- Power (PC) -------------------------------------------------------------

// minPowerW is the PC floor in every mode.
const minPowerW = 5

// maxPowerW is the PC ceiling for a mode: on a 100 W radio "005 ~ 100:
// SSB/CW/FM/FSK, 005 ~ 025: AM". AM is carrier power, so the ceiling really is a
// quarter of the nominal rating and the percentage scale has to follow it —
// reporting 50 W as 50% while the rig is in AM would be a lie by a factor of
// two. The quarter holds on the 200 W TS-990S too, where AM stops at 50 W.
//
// An unknown mode is treated as the full-power case, which is the conservative
// direction for a *reported* percentage but not for a *requested* one; callers
// that set power should have polled the mode first, which Init and PollSlow both
// do.
func (mdl Model) maxPowerW(m radio.Mode) int {
	if m == radio.ModeAM {
		return mdl.MaxPowerW / 4
	}
	return mdl.MaxPowerW
}

// powerFromWatts turns a decoded PC value into the three-way radio.Power. These
// rigs are among the few whose power control is in real watts, so Watts is
// always populated and Native carries the same number the wire did.
func (mdl Model) powerFromWatts(w int, m radio.Mode) radio.Power {
	watts := float64(w)
	return radio.Power{
		Watts:  &watts,
		Pct:    watts / float64(mdl.maxPowerW(m)) * 100,
		Native: w,
	}
}

// wattsFromSet resolves a PowerSet to the integer watt value to put on the wire.
//
// It does not pre-round to the rig's 5 W grid on purpose: the step size is 5 W
// only while the rig's Power Fine setting is off, and there is no command to ask
// which it is. Sending the exact request lets a Power Fine rig honour it to the
// watt, and a coarse rig round it down itself — which is why every SetPower
// reads PC; back afterwards rather than assuming what was accepted.
func (mdl Model) wattsFromSet(p radio.PowerSet, m radio.Mode) (int, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	ceiling := mdl.maxPowerW(m)

	var want float64
	if p.Watts != nil {
		want = *p.Watts
	} else {
		if *p.Pct < 0 || *p.Pct > 100 {
			return 0, fmt.Errorf("kenwood: power %.1f%% is outside 0..100", *p.Pct)
		}
		want = *p.Pct / 100 * float64(ceiling)
	}
	if math.IsNaN(want) {
		return 0, fmt.Errorf("kenwood: power request is not a number")
	}

	// Clamping mirrors what the rig does with an out-of-range parameter
	// ("entering a value lower than the minimum value results in the minimum
	// value being entered") and keeps the value inside PC's 3-digit field.
	w := int(math.Round(want))
	return min(ceiling, max(minPowerW, w)), nil
}

// --- Filter width (FW) ------------------------------------------------------

// cwFilterWidths is the FW ladder in CW and CW-R.
var cwFilterWidths = []int{50, 80, 100, 150, 200, 250, 300, 400, 500, 600, 1000, 1500, 2000, 2500}

// fskFilterWidths is the FW ladder in FSK and FSK-R.
var fskFilterWidths = []int{250, 500, 1000, 1500}

// filterWidths returns the FW ladder for a mode, or an error explaining why FW
// is the wrong command there.
//
// The reference is blunt — "The FW command cannot be used in SSB or AM mode" —
// and FM is excluded here for a different reason: in FM, FW is not a bandwidth
// at all but a modulation-degree switch (0000 normal, 0001 narrow). Publishing
// that as a 0 Hz or 1 Hz passband would be worse than publishing nothing, so the
// backend neither sets nor polls FW in FM.
func filterWidths(m radio.Mode) ([]int, error) {
	switch m {
	case radio.ModeCW, radio.ModeCWR:
		return cwFilterWidths, nil
	case radio.ModeFSK, radio.ModeFSKR:
		return fskFilterWidths, nil
	case radio.ModeLSB, radio.ModeUSB, radio.ModeAM:
		return nil, fmt.Errorf("kenwood: FW cannot be used in %s; the TS-590 shapes the SSB and AM passband with SH/SL instead", m)
	case radio.ModeFM:
		return nil, fmt.Errorf("kenwood: FW in FM selects the modulation degree, not a bandwidth; the TS-590 shapes the FM passband with SH/SL")
	default:
		return nil, fmt.Errorf("kenwood: cannot decide whether FW is legal without a known mode (have %s)", m)
	}
}

// filterWidthLegal reports whether FW carries a bandwidth in this mode, so the
// slow poll can skip it rather than provoke an error tone at the rig.
func filterWidthLegal(m radio.Mode) bool {
	_, err := filterWidths(m)
	return err == nil
}

// snapFilterWidth rounds hz onto the mode's ladder exactly the way the rig does:
// below the ladder gives the minimum, above it gives the maximum, and anything
// in between falls to the closest lower rung ("1400 will revert to 1000").
//
// Doing it here rather than leaving it to the rig makes the outcome testable and
// lets SetFilterWidth report the effective width without a second guess.
func snapFilterWidth(hz int, m radio.Mode) (int, error) {
	steps, err := filterWidths(m)
	if err != nil {
		return 0, err
	}
	if hz <= steps[0] {
		return steps[0], nil
	}
	out := steps[0]
	for _, s := range steps {
		if s > hz {
			break
		}
		out = s
	}
	return out, nil
}

// --- Model identification (ID) ----------------------------------------------

// normaliseModel folds the configured model string into the form used in
// messages, so "TS-590SG", "ts590sg" and "ts-590sg" all read the same.
//
// A name the registry does not know is passed through rather than rejected:
// this is only ever a display string, and New has already refused to build a
// backend for an unknown model.
func normaliseModel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if m, ok := models[modelKey(s)]; ok {
		return m.Label
	}
	return s
}
