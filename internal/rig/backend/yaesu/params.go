package yaesu

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/hessu/remoses/internal/radio"
)

// --- Frequency (FA/FB) ------------------------------------------------------

// The two widths of the FA/FB frequency field, neither of them Kenwood's eleven.
//
// Nine is exact rather than generous on the two radios that reach 470 MHz.
// Eight is the whole FT-950 generation, whose own manuals print FA14250000; as
// the worked example and whose IF answer is a byte shorter to match — see
// decodeIF. Which one a model takes is Model.FreqDigits.
//
// The rig demands the full width zero-padded, so a short parameter is a
// malformed frame rather than a tolerated shorthand.
const (
	freqDigitsOld    = 8
	freqDigitsModern = 9
)

// maxFrequencyHz is what fits in a field of n digits. Per-model tuning ranges
// are much narrower and are checked separately; this guard only keeps a bad
// caller from emitting a frame that would desynchronise the command stream.
func maxFrequencyHz(digits int) uint64 {
	max := uint64(1)
	for range digits {
		max *= 10
	}
	return max - 1
}

func formatFrequency(hz uint64, digits int) (string, error) {
	if hz > maxFrequencyHz(digits) {
		return "", fmt.Errorf("yaesu: frequency %d Hz does not fit the %d-digit FA/FB field", hz, digits)
	}
	return fmt.Sprintf("%0*d", digits, hz), nil
}

// parseFrequency reads an FA/FB argument, accepting either width.
//
// Both are the same value — zero-padded decimal Hz — so there is nothing to
// choose between them once the field boundaries are known, and taking whatever
// arrived means a station whose configuration names the wrong generation is
// still read correctly. It is the rule DESIGN.md §5.4 settled on for the
// IC-905's variable frequency field: decode from the wire, encode from the
// model.
//
// The width is still checked, because a field of some other length is a frame
// from a different protocol rather than a variant of this one. Kenwood's eleven
// digits is the case that matters.
func parseFrequency(s string) (uint64, error) {
	if len(s) != freqDigitsOld && len(s) != freqDigitsModern {
		return 0, fmt.Errorf("yaesu: frequency field %q is %d digits, want %d or %d",
			s, len(s), freqDigitsOld, freqDigitsModern)
	}
	return parseFrequencyWidth(s, len(s))
}

// parseFrequencyWidth reads a frequency field of exactly the stated width. The
// IF decoder uses it, where the layout that matched has already fixed the width
// and a field of any other length would mean the layout table is wrong.
func parseFrequencyWidth(s string, digits int) (uint64, error) {
	if len(s) != digits {
		return 0, fmt.Errorf("yaesu: frequency field %q is %d digits, want %d", s, len(s), digits)
	}
	hz, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("yaesu: frequency field %q is not numeric", s)
	}
	return hz, nil
}

// --- Power (PC) -------------------------------------------------------------

// The PC range. The floor is 5 W on every model whose PC is in watts; the
// ceiling is per model, and on the FTX-1 per head.
const (
	minPowerW = 5
	// ftx1HeadMaxW and ftx1AmpMaxW are the FTX-1's two ceilings: the field head
	// alone, and the head driving the SPA-1 amplifier. PC; names which one is
	// answering, so the ceiling is refined once the rig has been read.
	ftx1HeadMaxW = 10
	ftx1AmpMaxW  = 100
	// pcHeadRadio and pcHeadAmp are the FTX-1's P1 values.
	pcHeadRadio = 1
	pcHeadAmp   = 2
	// powerRawMax is the top of PC on the FTdx5000 and FTdx9000, whose manuals
	// give the parameter as 000-255 where every other model here gives a watt
	// range. There is no floor: unlike the watt form, which starts at 005, the
	// range as printed starts at 000.
	powerRawMax = 255
)

// powerFromWatts turns a decoded PC value into radio.Power.
//
// Unlike Kenwood there is no documented per-mode ceiling — no Yaesu manual here
// derates AM the way a TS-590 does — so the percentage is a straight fraction
// of the model's maximum and does not depend on the mode.
//
// The watt unit is an inference, but a solid one: the 200 W FTdx101MP's range
// stops at 200 where the 100 W FTdx101D's stops at 100, and the FTX-1's table
// annotates both of its ranges "(W)".
func powerFromWatts(w, ceiling int) radio.Power {
	watts := float64(w)
	pct := 0.0
	if ceiling > 0 {
		pct = watts / float64(ceiling) * 100
	}
	return radio.Power{Watts: &watts, Pct: pct, Native: w}
}

// wattsFromSet resolves a PowerSet to the integer watt value to put on the wire,
// clamped into the rig's range.
//
// Clamping rather than refusing matches what the rig does with an out-of-range
// parameter, and matters more here than on a Kenwood: no Yaesu documents a
// rejection, so an out-of-range PC would not come back as an error but as
// silence and a full session timeout.
func wattsFromSet(p radio.PowerSet, ceiling int) (int, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	var want float64
	if p.Watts != nil {
		want = *p.Watts
	} else {
		if *p.Pct < 0 || *p.Pct > 100 {
			return 0, fmt.Errorf("yaesu: power %.1f%% is outside 0..100", *p.Pct)
		}
		want = *p.Pct / 100 * float64(ceiling)
	}
	if math.IsNaN(want) {
		return 0, fmt.Errorf("yaesu: power request is not a number")
	}
	w := int(math.Round(want))
	return min(ceiling, max(minPowerW, w)), nil
}

// powerFromRaw turns a decoded PC value into radio.Power on the two models
// whose PC is an uncalibrated index.
//
// Watts stays nil, exactly as it does for Icom's 14 0A: the FTdx5000 and
// FTdx9000 manuals give PC as 000-255 and calibrate it against nothing, and the
// FTdx9000 is sold as both a 200 W and a 400 W radio with no CAT command that
// says which is on the desk. A watt figure here would be a number remoses
// invented about a transmitter's output.
func powerFromRaw(n int) radio.Power {
	return radio.Power{Pct: float64(n) / powerRawMax * 100, Native: n}
}

// rawFromSet resolves a PowerSet to the 000-255 index those two models take.
//
// Watts are refused rather than approximated, for the reason powerFromRaw gives:
// there is no documented conversion, and inventing one would misreport what the
// transmitter is doing.
func rawFromSet(p radio.PowerSet) (int, error) {
	if err := p.Validate(); err != nil {
		return 0, err
	}
	if p.Watts != nil {
		return 0, fmt.Errorf("yaesu: this radio's PC is an uncalibrated 000-%d index, "+
			"not watts; set power as a percentage", powerRawMax)
	}
	if *p.Pct < 0 || *p.Pct > 100 {
		return 0, fmt.Errorf("yaesu: power %.1f%% is outside 0..100", *p.Pct)
	}
	n := int(math.Round(*p.Pct / 100 * powerRawMax))
	return min(powerRawMax, max(0, n)), nil
}

// --- Filter width (SH) ------------------------------------------------------

// widths is a model's SH bandwidth table: index to Hz, one ladder per mode
// group.
//
// Two things make this awkward enough to be worth a type. SH does not carry a
// width in Hz at all — its parameter is an index into a table whose meaning
// depends on the current mode — and on the FT-991A and FT-891 each group has a
// narrow and a wide column, chosen by the separate NA setting. The newer radios
// have one column per group and point both fields at the same ladder.
//
// A ladder is indexed by the SH parameter itself. Element 0 is the rig's
// default for that column, which is 0 where the manual says the default "varies
// depending on the selected roofing filter" or "on the selected mode" — an
// honest unknown rather than a guessed number. A 0 anywhere else marks an index
// the manual leaves blank in that column.
type widths struct {
	ssbNarrow, ssbWide   []int
	cwNarrow, cwWide     []int
	digiNarrow, digiWide []int
}

// any reports whether this model has a bandwidth table at all.
//
// The FTdx9000 is the one that does not. Its SH parameter is the position of
// the WIDTH knob — 00 fully anticlockwise to 31 fully clockwise, 16 centred —
// and no table in its manual converts that to Hz, so there is nothing to look
// up in either direction. Caps reports FilterWidth false there rather than
// promising a passband remoses cannot read.
func (w widths) any() bool {
	return len(w.ssbNarrow) > 0 || len(w.ssbWide) > 0 ||
		len(w.cwNarrow) > 0 || len(w.cwWide) > 0 ||
		len(w.digiNarrow) > 0 || len(w.digiWide) > 0
}

// ladder returns the table column for the current mode, and false where SH
// carries no width worth publishing.
//
// AM and FM are the false case on every model here. The FT-991A, FT-891 and
// FTdx101 tables have no AM or FM column at all, and on the FT-710 and FTX-1
// those columns hold a single fixed value per mode code (6, 9 or 16 kHz) rather
// than a ladder — and which of the three applies depends on distinctions
// radio.Mode does not carry, since AM-N and FM-N are separate mode codes.
// Reporting nothing beats reporting the wrong one of three.
//
// DATA is grouped with CW, RTTY and PSK rather than with SSB. That is stated
// outright in the FT-710 and FTX-1 tables ("CW / DATA / RTTY / PSK") and
// inferred for the older three, whose tables name no DATA column at all.
func (w widths) ladder(m radio.Mode, data, narrow bool) ([]int, bool) {
	var n, wide []int
	switch m {
	case radio.ModeLSB, radio.ModeUSB:
		if data {
			n, wide = w.digiNarrow, w.digiWide
		} else {
			n, wide = w.ssbNarrow, w.ssbWide
		}
	case radio.ModeCW, radio.ModeCWR:
		n, wide = w.cwNarrow, w.cwWide
	case radio.ModeFSK, radio.ModeFSKR, radio.ModePSK:
		n, wide = w.digiNarrow, w.digiWide
	default:
		return nil, false
	}
	if narrow {
		return n, len(n) > 0
	}
	return wide, len(wide) > 0
}

// widthAt reports the bandwidth an SH index names in this column, and false for
// an index the column leaves blank or a default the manual declines to state.
func widthAt(ladder []int, index int) (int, bool) {
	if index < 0 || index >= len(ladder) || ladder[index] == 0 {
		return 0, false
	}
	return ladder[index], true
}

// snapWidth picks the SH index for a requested width in Hz.
//
// The rule is the one the Kenwood backend already uses for FW: the closest rung
// at or below the request, the lowest rung when the request is below the ladder
// and the highest when it is above. No manual states what a Yaesu does with an
// off-table value — SH takes an index, so the question does not arise on the
// wire — which is exactly why remoses does the rounding itself: choosing here
// makes the outcome testable, and never widening the passband beyond what was
// asked for is the safe direction.
//
// Index 0 is skipped. It is the column's default, a duplicate of a rung that
// also appears explicitly on the FT-991A and an unknown on the newer radios, so
// selecting it would be either redundant or unreadable.
func snapWidth(ladder []int, hz int) (index int, width int, ok bool) {
	best, bestIdx := 0, 0
	lowest, lowestIdx := 0, 0
	for i := 1; i < len(ladder); i++ {
		v := ladder[i]
		if v == 0 {
			continue
		}
		if lowest == 0 || v < lowest {
			lowest, lowestIdx = v, i
		}
		if v <= hz && v > best {
			best, bestIdx = v, i
		}
	}
	switch {
	case best != 0:
		return bestIdx, best, true
	case lowest != 0:
		return lowestIdx, lowest, true
	}
	return 0, 0, false
}

// widthsFT991A is the FT-991A and FT-891 bandwidth table.
//
// The two radios share it byte for byte; only the command that indexes it
// differs, which is why Model.Filter and Model.Widths are separate fields. Each
// group has a narrow and a wide column, selected by NA rather than by the mode.
//
// The CW and RTTY/PSK columns are identical from index 1 onwards and differ
// only in their defaults — 500 against 300 narrow, 2400 against 500 wide — so
// they are written out separately rather than shared.
func widthsFT991A() widths {
	return widths{
		ssbNarrow: []int{1500, 200, 400, 600, 850, 1100, 1350, 1500, 1650, 1800},
		ssbWide: []int{2400,
			0, 0, 0, 0, 0, 0, 0, 0,
			1800, 1950, 2100, 2200, 2300, 2400, 2500, 2600, 2700, 2800, 2900, 3000, 3200},
		cwNarrow: []int{500, 50, 100, 150, 200, 250, 300, 350, 400, 450, 500},
		cwWide: []int{2400,
			0, 0, 0, 0, 0, 0, 0, 0, 0,
			500, 800, 1200, 1400, 1700, 2000, 2400, 3000},
		digiNarrow: []int{300, 50, 100, 150, 200, 250, 300, 350, 400, 450, 500},
		digiWide: []int{500,
			0, 0, 0, 0, 0, 0, 0, 0, 0,
			500, 800, 1200, 1400, 1700, 2000, 2400, 3000},
	}
}

// widthsFTdx101 is the FTdx101D/MP and FTdx10 table, which the generic profile
// also claims.
//
// One column per group: NA still exists, but the bandwidth table does not split
// on it here. Index 0 is the rig's default, and the manual says it "varies
// depending on the selected roofing filter" — so it is left unknown rather than
// guessed at.
func widthsFTdx101() widths {
	ssb := []int{0,
		300, 400, 600, 850, 1100, 1200, 1500, 1650, 1800, 1950,
		2100, 2200, 2300, 2400, 2500, 2600, 2700, 2800, 2900, 3000, 3200}
	digi := []int{0,
		50, 100, 150, 200, 250, 300, 350, 400, 450, 500,
		600, 800, 1200, 1400, 1700, 2000, 2400, 3000}
	return widths{
		ssbNarrow: ssb, ssbWide: ssb,
		cwNarrow: digi, cwWide: digi,
		digiNarrow: digi, digiWide: digi,
	}
}

// widthsFT710 is the FT-710 and FTX-1 table: the FTdx101's ladders extended to
// index 23, with a wider top end in SSB and three more rungs in the digital
// modes. Its default is again unstated, "varies depending on the selected mode".
func widthsFT710() widths {
	ssb := []int{0,
		300, 400, 600, 850, 1100, 1200, 1500, 1650, 1800, 1950,
		2100, 2250, 2400, 2450, 2500, 2600, 2700, 2800,
		2900, 3000, 3200, 3500, 4000}
	digi := []int{0,
		50, 100, 150, 200, 250, 300, 350, 400, 450, 500,
		600, 800, 1200, 1400, 1700, 2000, 2400, 3000,
		3200, 3500, 4000}
	return widths{
		ssbNarrow: ssb, ssbWide: ssb,
		cwNarrow: digi, cwWide: digi,
		digiNarrow: digi, digiWide: digi,
	}
}

// widthsFT950 is the FT-950's bandwidth table, and it is nobody else's.
//
// It looks like the FT-991A's until the middle of the wide SSB column, where it
// steps 2250 / 2400 / 2450 against that radio's 2200 / 2300 / 2400, and it
// stops at index 20 rather than 21. The CW and RTTY/PKT columns are coarser
// than every other radio here: the narrow column starts at 100 Hz where the
// FTdx1200 generation starts at 50, and there are seven rungs where there are
// ten.
//
// Its defaults are stated outright — index 0 is a real width on this radio, not
// the newer manuals' "varies depending on the roofing filter" — so element 0
// carries them. CW and RTTY/PKT differ only in those defaults, which is why
// they are written out separately rather than shared.
func widthsFT950() widths {
	return widths{
		ssbNarrow: []int{1800, 200, 400, 600, 850, 1100, 1350, 1500, 1650, 1800},
		ssbWide: []int{2400,
			0, 0, 0, 0, 0, 0, 0, 0,
			1800, 1950, 2100, 2250, 2400, 2450, 2500, 2600, 2700, 2800, 2900, 3000},
		cwNarrow: []int{500, 0, 0, 100, 200, 300, 400, 500},
		cwWide: []int{2400,
			0, 0, 0, 0, 0, 0,
			500, 800, 1200, 1400, 1700, 2000, 2400},
		digiNarrow: []int{300, 0, 0, 100, 200, 300, 400, 500},
		digiWide: []int{500,
			0, 0, 0, 0, 0, 0,
			500, 800, 1200, 1400, 1700, 2000, 2400},
	}
}

// widthsFTdx1200 is the FTdx1200 and FTdx3000 table, which the two share to the
// digit.
//
// It is the FT-991A's ladder extended: SSB runs on past 3200 to 4000 in 200 Hz
// steps, and the CW and RTTY/PSK columns are identical to each other in their
// defaults as well as their rungs — 500 narrow and 2400 wide in both — so
// unlike the FT-991A and FT-950 there is nothing to distinguish them.
func widthsFTdx1200() widths {
	ssbNarrow := []int{1500, 200, 400, 600, 850, 1100, 1350, 1500, 1650, 1800}
	ssbWide := []int{2400,
		0, 0, 0, 0, 0, 0, 0, 0,
		1800, 1950, 2100, 2200, 2300, 2400, 2500, 2600, 2700, 2800,
		2900, 3000, 3200, 3400, 3600, 3800, 4000}
	narrow := []int{500, 50, 100, 150, 200, 250, 300, 350, 400, 450, 500}
	wide := []int{2400,
		0, 0, 0, 0, 0, 0, 0, 0, 0,
		500, 800, 1200, 1400, 1700, 2000, 2400}
	return widths{
		ssbNarrow: ssbNarrow, ssbWide: ssbWide,
		cwNarrow: narrow, cwWide: wide,
		digiNarrow: narrow, digiWide: wide,
	}
}

// widthsFTdx5000 is the FTdx5000's table, printed as running text rather than a
// grid and different from the FTdx1200's in three places.
//
// Its narrow SSB column stops at index 7 (1500 Hz) where the FTdx1200's runs to
// 9 (1800), and the wide column starts at 7 rather than 9 and steps 2250 / 2400
// through 12 and 13. Index 14 is printed "- - - -", an index the manual defines
// for no width at all, so it is a hole here rather than a rung.
//
// The RTTY and PSK columns are identical to each other and differ from CW only
// in their defaults — 300 against 500 narrow, 500 against 2400 wide — the same
// split the FT-991A and FT-950 have.
func widthsFTdx5000() widths {
	narrow := []int{500, 50, 100, 150, 200, 250, 300, 350, 400, 450, 500}
	wide := []int{2400,
		0, 0, 0, 0, 0, 0, 0, 0, 0,
		500, 800, 1200, 1400, 1700, 2000, 2400}
	return widths{
		ssbNarrow: []int{1500, 200, 400, 600, 850, 1100, 1350, 1500},
		ssbWide: []int{2400,
			0, 0, 0, 0, 0, 0,
			1500, 1650, 1800, 1950, 2100, 2250, 2400,
			0, // index 14 is printed "- - - -"
			2500, 2600, 2700, 2800, 2900, 3000, 3200, 3400, 3600, 3800, 4000},
		cwNarrow: narrow, cwWide: wide,
		digiNarrow: []int{300, 50, 100, 150, 200, 250, 300, 350, 400, 450, 500},
		digiWide: []int{500,
			0, 0, 0, 0, 0, 0, 0, 0, 0,
			500, 800, 1200, 1400, 1700, 2000, 2400},
	}
}

// --- Model identification (ID) ----------------------------------------------

// normaliseModel folds the configured model string into the form used in
// messages. A name the registry does not know is passed through: this is only
// ever a display string, and New has already refused to build a backend for an
// unknown model.
func normaliseModel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if m, err := LookupModel(s); err == nil {
		return m.Label
	}
	return s
}
