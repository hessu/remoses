package morse

import "time"

// Weight limits. Weighting is a percentage with 50 neutral; the bounds keep the
// inter-element gap comfortably positive, since a gap of zero would run the
// elements of a character into one continuous mark.
const (
	MinWeight     = 20
	MaxWeight     = 80
	NeutralWeight = 50
)

// UnitDuration returns the dot unit for a keying speed, by the PARIS standard:
// the word "PARIS " is exactly 50 dot units long, so one unit is 1200 ms / wpm.
//
// It returns 0 for a non-positive speed rather than panicking, because it sits
// on the path of an API-supplied number; callers clamp speed before keying.
func UnitDuration(wpm int) time.Duration {
	if wpm <= 0 {
		return 0
	}
	return 1200 * time.Millisecond / time.Duration(wpm)
}

// Timing is the set of durations one speed and weighting imply. CharGap and
// WordGap are the FULL gap between characters and between words, not an extra
// delay added to an inter-element gap, so a caller adds exactly one of the
// three gap values between any two marks.
type Timing struct {
	// Unit is the dot unit, 1200 ms / wpm.
	Unit time.Duration
	// Dit is a one-unit mark, lengthened or shortened by the weighting.
	Dit time.Duration
	// Dah is a three-unit mark, shifted by the same absolute amount as Dit.
	Dah time.Duration
	// ElementGap separates marks inside one character: one unit, less the
	// weighting shift.
	ElementGap time.Duration
	// CharGap separates characters: three units, less the weighting shift.
	CharGap time.Duration
	// WordGap separates words: seven units, less the weighting shift.
	WordGap time.Duration
}

// NewTiming derives the element durations for a speed and weighting.
//
// # How weight is applied
//
// Weighting moves time from the gaps into the marks. With shift =
// (weight-50)/50 of a dot unit, EVERY mark is lengthened by shift and EVERY gap
// that follows a mark is shortened by the same amount:
//
//	Dit        = 1*Unit + shift      ElementGap = 1*Unit - shift
//	Dah        = 3*Unit + shift      CharGap    = 3*Unit - shift
//	                                 WordGap    = 7*Unit - shift
//
// Every mark in a message is followed by exactly one gap, apart from the very
// last one, so the total transmission time is independent of weighting to
// within a single shift. That matters here: the same arithmetic drives the
// Icom open-loop pacing estimate and the API's est_duration_ms, and a weighting
// knob must not quietly make both of them wrong.
//
// A weight of 0 means "not configured" and is taken as neutral; anything else
// is clamped to [MinWeight, MaxWeight]. A non-positive wpm yields a zero
// Timing.
func NewTiming(wpm, weight int) Timing {
	unit := UnitDuration(wpm)
	if unit == 0 {
		return Timing{}
	}
	if weight == 0 {
		weight = NeutralWeight
	}
	weight = min(max(weight, MinWeight), MaxWeight)

	shift := time.Duration(int64(unit) * int64(weight-NeutralWeight) / 50)
	return Timing{
		Unit:       unit,
		Dit:        unit + shift,
		Dah:        3*unit + shift,
		ElementGap: unit - shift,
		CharGap:    3*unit - shift,
		WordGap:    7*unit - shift,
	}
}

// Mark returns the keyed length of one element.
func (t Timing) Mark(e Element) time.Duration {
	if e == Dash {
		return t.Dah
	}
	return t.Dit
}

// Symbol returns the time from the start of a symbol's first mark to the end of
// its last, including the gaps inside it. A Space symbol keys nothing and
// measures zero; its gap is supplied by Symbols.
func (t Timing) Symbol(s Symbol) time.Duration {
	var d time.Duration
	for i, e := range s.Elements {
		if i > 0 {
			d += t.ElementGap
		}
		d += t.Mark(e)
	}
	return d
}

// Symbols returns the total time to key a sequence, including the gaps between
// characters and words.
//
// Leading Space symbols key nothing and cost nothing: a word gap before any
// mark is silence the operator cannot hear. A trailing Space does count, which
// is what makes "PARIS " exactly 50 units.
func (t Timing) Symbols(syms []Symbol) time.Duration {
	var (
		total    time.Duration
		keyed    bool // something has been sent, so a gap is now audible
		prevChar bool
	)
	for _, s := range syms {
		if s.Kind == KindSpace {
			if keyed {
				total += t.WordGap
			}
			prevChar = false
			continue
		}
		if prevChar {
			total += t.CharGap
		}
		total += t.Symbol(s)
		keyed, prevChar = true, true
	}
	return total
}

// Duration is the total time to key canonical text, counting elements rather
// than characters: "E" and "0" are one character each and differ by a factor of
// nineteen. It drives both the Icom open-loop pacing estimate and the API's
// est_duration_ms, so a character count would not do.
func Duration(text string, wpm, weight int) (time.Duration, error) {
	syms, err := Parse(text)
	if err != nil {
		return 0, err
	}
	return NewTiming(wpm, weight).Symbols(syms), nil
}

// Estimate is Duration for callers that cannot fail.
//
// A pacing loop works on text that has already been translated into a rig's own
// dialect, where a prosign may have become some symbol this package has never
// heard of. Refusing to time it would stall the queue, so an unrecognised
// character is timed as a nominal nine-unit character instead.
func Estimate(text string, wpm, weight int) time.Duration {
	syms, err := parse(text, true)
	if err != nil {
		return 0
	}
	return NewTiming(wpm, weight).Symbols(syms)
}
