package morse

import (
	"testing"
	"time"
)

func TestUnitDuration(t *testing.T) {
	cases := map[int]time.Duration{
		5:  240 * time.Millisecond,
		20: 60 * time.Millisecond,
		25: 48 * time.Millisecond,
		40: 30 * time.Millisecond,
	}
	for wpm, want := range cases {
		if got := UnitDuration(wpm); got != want {
			t.Errorf("UnitDuration(%d): got %v, want %v", wpm, got, want)
		}
	}
	if got := UnitDuration(0); got != 0 {
		t.Errorf("UnitDuration(0): got %v, want 0", got)
	}
	if got := UnitDuration(-3); got != 0 {
		t.Errorf("UnitDuration(-3): got %v, want 0", got)
	}
}

// TestParisAnchor is the anchor for all timing arithmetic: PARIS plus its word
// gap is 50 dot units by definition, so at 20 wpm it is exactly three seconds.
func TestParisAnchor(t *testing.T) {
	got, err := Duration("PARIS ", 20, NeutralWeight)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3*time.Second {
		t.Fatalf("PARIS at 20 wpm: got %v, want 3s", got)
	}
	// The same word at 40 wpm takes half as long, and at 10 wpm twice as long.
	for wpm, want := range map[int]time.Duration{10: 6 * time.Second, 40: 1500 * time.Millisecond} {
		got, err := Duration("PARIS ", wpm, NeutralWeight)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("PARIS at %d wpm: got %v, want %v", wpm, got, want)
		}
	}
}

// TestWeightDoesNotChangeTotal pins the property that justifies the weighting
// formula: what the marks gain the gaps give back, so an estimate stays honest.
func TestWeightDoesNotChangeTotal(t *testing.T) {
	for _, w := range []int{0, 20, 35, 50, 65, 80} {
		got, err := Duration("PARIS ", 20, w)
		if err != nil {
			t.Fatal(err)
		}
		if got != 3*time.Second {
			t.Errorf("weight %d: got %v, want 3s", w, got)
		}
	}
	// It does change the sound: a heavier weight lengthens the mark.
	light, heavy := NewTiming(20, 30), NewTiming(20, 70)
	if !(light.Dit < heavy.Dit && light.ElementGap > heavy.ElementGap) {
		t.Errorf("weighting had no effect: light=%+v heavy=%+v", light, heavy)
	}
	if heavy.Dah-heavy.Dit != light.Dah-light.Dit {
		t.Error("the dash must shift by the same absolute amount as the dot")
	}
	// Out-of-range weights are clamped rather than producing a negative gap.
	if NewTiming(20, 500).ElementGap <= 0 {
		t.Error("extreme weight produced a non-positive inter-element gap")
	}
}

func TestNeutralTiming(t *testing.T) {
	tm := NewTiming(20, NeutralWeight)
	u := 60 * time.Millisecond
	want := Timing{Unit: u, Dit: u, Dah: 3 * u, ElementGap: u, CharGap: 3 * u, WordGap: 7 * u}
	if tm != want {
		t.Errorf("got %+v, want %+v", tm, want)
	}
	if got := NewTiming(0, 50); got != (Timing{}) {
		t.Errorf("zero wpm: got %+v, want zero Timing", got)
	}
}

// TestDurationCountsElements checks the arithmetic against durations worked out
// by hand, in units, so a regression shows up as a wrong count rather than as
// vaguely wrong Morse.
func TestDurationCountsElements(t *testing.T) {
	cases := []struct {
		text  string
		units int
		why   string
	}{
		{"E", 1, "one dot"},
		{"T", 3, "one dash"},
		{"EE", 1 + 3 + 1, "dot, inter-character gap, dot"},
		{"E E", 1 + 7 + 1, "dot, word gap, dot"},
		{"A", 1 + 1 + 3, "dot, inter-element gap, dash"},
		{"0", 5*3 + 4, "five dashes and four inter-element gaps"},
		{" E", 1, "a leading word gap is silence nobody hears"},
		{"E ", 1 + 7, "a trailing word gap counts; PARIS depends on it"},
		{"^AR", 1 + 1 + 3 + 1 + 1 + 1 + 3 + 1 + 1, "A and R run together: .-.-."},
		{"AR", 1 + 1 + 3 + 3 + 1 + 1 + 3 + 1 + 1, "the same letters with a gap between them"},
		{"PARIS ", 50, "the definition of the standard word"},
	}
	for _, c := range cases {
		got, err := Duration(c.text, 20, NeutralWeight)
		if err != nil {
			t.Fatalf("%q: %v", c.text, err)
		}
		want := time.Duration(c.units) * 60 * time.Millisecond
		if got != want {
			t.Errorf("%q (%s): got %v (%d units), want %v (%d units)",
				c.text, c.why, got, got/(60*time.Millisecond), want, c.units)
		}
	}
}

func TestDurationRejectsBadText(t *testing.T) {
	if _, err := Duration("A;B", 20, 50); err == nil {
		t.Error("expected an error for an unsendable character")
	}
	if _, err := Duration("A^ B", 20, 50); err == nil {
		t.Error("expected an error for a stray caret")
	}
	if d, err := Duration("PARIS ", 0, 50); err != nil || d != 0 {
		t.Errorf("zero wpm: got %v, %v; want 0, nil", d, err)
	}
}

// TestEstimateTolerates covers the pacing loop's input: text already translated
// into a rig dialect, where a prosign has become a symbol we do not know.
func TestEstimateTolerates(t *testing.T) {
	// '_' is the Kenwood spelling of ^AR. We cannot key it, but we must still
	// be able to pace it.
	got := Estimate("CQ_", 20, 50)
	if got <= 0 {
		t.Fatalf("got %v, want a positive estimate", got)
	}
	known := Estimate("CQK", 20, 50)
	if got != known {
		t.Errorf("unknown character should cost a nominal K: got %v, want %v", got, known)
	}
	if Estimate("^", 20, 50) <= 0 {
		t.Error("a stray caret must still be timed rather than failing")
	}
	if Estimate("CQ", 0, 50) != 0 {
		t.Error("zero wpm should estimate zero rather than dividing by zero")
	}
}

func TestSymbolsSkipsLeadingSpacesOnly(t *testing.T) {
	tm := NewTiming(20, 50)
	syms, err := Parse("  E  E")
	if err != nil {
		t.Fatal(err)
	}
	// Two leading gaps cost nothing; the two between the dots cost a word gap
	// each, because a deliberate double space is a deliberate pause.
	want := tm.Dit + 2*tm.WordGap + tm.Dit
	if got := tm.Symbols(syms); got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}
