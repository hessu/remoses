package cw

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
)

func serialCfg(wpm int, sk config.SerialKey) config.CW {
	return config.CW{
		Enabled:    true,
		Method:     "serial_key",
		DefaultWPM: wpm,
		SerialKey:  &sk,
	}
}

func newSerial(t *testing.T, fl *fakeLines, cfg config.CW) Sender {
	t.Helper()
	s, err := NewSerialKey(fl, cfg)
	if err != nil {
		t.Fatalf("NewSerialKey: %v", err)
	}
	t.Cleanup(func() { s.(io.Closer).Close() })
	return s
}

// keyIntervals turns recorded edges into the two things a Morse ear cares
// about: how long each mark was held, and how long the silence between marks
// lasted.
func keyIntervals(es []edge, name string) (marks, gaps []time.Duration) {
	var on, lastOff time.Time
	for _, e := range es {
		if e.line != name {
			continue
		}
		if e.on {
			if !lastOff.IsZero() {
				gaps = append(gaps, e.at.Sub(lastOff))
			}
			on = e.at
		} else {
			marks = append(marks, e.at.Sub(on))
			lastOff = e.at
		}
	}
	return marks, gaps
}

// assertNear allows a quarter of the nominal interval, or 20 ms, whichever is
// larger. CI runners are loaded and a preempted goroutine shows up here as a
// long mark; the ratio assertions are what pin the shape of the Morse, and
// these pin the speed.
func assertNear(t *testing.T, what string, got, want time.Duration) {
	t.Helper()
	tol := max(want/4, 20*time.Millisecond)
	if got < want-tol || got > want+tol {
		t.Errorf("%s: got %v, want %v +/- %v", what, got, want, tol)
	}
}

// assertRatio checks an interval against the dit that timed it. The bounds are
// wide enough that only a wrong number of units fails them.
func assertRatio(t *testing.T, what string, got, dit time.Duration, lo, hi float64) {
	t.Helper()
	r := float64(got) / float64(dit)
	if r < lo || r > hi {
		t.Errorf("%s: %v is %.2f dits, want between %.1f and %.1f", what, got, r, lo, hi)
	}
}

// TestSerialKeyerTiming is the heart of the serial path: the ratios that make
// keying sound like Morse rather than like a stuck relay.
//
// "AT E" exercises every interval there is: A is a dit, an inter-element gap
// and a dah; then an inter-character gap; then T, a dah; then an inter-word
// gap; then E, a dit.
func TestSerialKeyerTiming(t *testing.T) {
	const wpm = 25
	unit := 1200 * time.Millisecond / wpm // 48 ms

	fl := newFakeLines()
	s := newSerial(t, fl, serialCfg(wpm, config.SerialKey{
		KeyLine:   "dtr",
		PTTLine:   "rts",
		PTTLeadMS: 20,
		PTTTailMS: 30,
		Weight:    50,
	}))

	if _, err := s.Enqueue("AT E", Append); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Four marks is eight key edges, plus PTT up and down.
	waitFor(t, "the message to be keyed", 5*time.Second, func() bool { return fl.count() >= 10 })

	marks, gaps := keyIntervals(fl.edges(), "DTR")
	if len(marks) != 4 || len(gaps) != 3 {
		t.Fatalf("got %d marks and %d gaps, want 4 and 3: %v %v", len(marks), len(gaps), marks, gaps)
	}

	dit := marks[0]
	// Shape first: these hold even on a machine that is running slow, because
	// every interval is stretched together.
	assertRatio(t, "dah after a dit", marks[1], dit, 2.0, 4.5)
	assertRatio(t, "dah of T", marks[2], dit, 2.0, 4.5)
	assertRatio(t, "dit of E", marks[3], dit, 0.5, 2.0)
	assertRatio(t, "inter-element gap", gaps[0], dit, 0.5, 2.0)
	assertRatio(t, "inter-character gap", gaps[1], dit, 2.0, 4.5)
	assertRatio(t, "inter-word gap", gaps[2], dit, 5.0, 9.5)

	if testing.Short() {
		t.Skip("skipping absolute timing assertions under -short")
	}
	assertNear(t, "dit", marks[0], unit)
	assertNear(t, "dah", marks[1], 3*unit)
	assertNear(t, "dah", marks[2], 3*unit)
	assertNear(t, "dit", marks[3], unit)
	assertNear(t, "inter-element gap", gaps[0], unit)
	assertNear(t, "inter-character gap", gaps[1], 3*unit)
	assertNear(t, "inter-word gap", gaps[2], 7*unit)
}

func TestSerialKeyerPTTLeadAndTail(t *testing.T) {
	fl := newFakeLines()
	s := newSerial(t, fl, serialCfg(25, config.SerialKey{
		KeyLine:   "dtr",
		PTTLine:   "rts",
		PTTLeadMS: 40,
		PTTTailMS: 60,
	}))

	if _, err := s.Enqueue("E", Append); err != nil {
		t.Fatal(err)
	}
	// PTT up, key down, key up, PTT down.
	waitFor(t, "the transmission to finish", 5*time.Second, func() bool { return fl.count() >= 4 })

	es := fl.edges()
	if es[0].line != "RTS" || !es[0].on {
		t.Fatalf("first edge was %+v, want PTT up", es[0])
	}
	last := es[len(es)-1]
	if last.line != "RTS" || last.on {
		t.Fatalf("last edge was %+v, want PTT down", last)
	}
	keyMarks, _ := keyIntervals(es, "DTR")
	if len(keyMarks) != 1 {
		t.Fatalf("got %d marks, want 1", len(keyMarks))
	}

	keyDown, keyUp := es[1].at, es[2].at
	if lead := keyDown.Sub(es[0].at); lead < 30*time.Millisecond {
		t.Errorf("PTT lead-in was %v, want at least the configured 40 ms", lead)
	}
	if tail := last.at.Sub(keyUp); tail < 45*time.Millisecond {
		t.Errorf("PTT tail was %v, want at least the configured 60 ms", tail)
	}
	if testing.Short() {
		return
	}
	assertNear(t, "PTT lead-in", keyDown.Sub(es[0].at), 40*time.Millisecond)
	assertNear(t, "PTT tail", last.at.Sub(keyUp), 60*time.Millisecond)
}

// TestSerialKeyerAbortStopsInsideAnElement is the safety case: an abort must
// not wait for the current character to finish, because the operator pressing
// stop may be stopping an accidental transmission.
func TestSerialKeyerAbortStopsInsideAnElement(t *testing.T) {
	fl := newFakeLines()
	// 10 wpm: a dah is 360 ms, so there is time to abort inside one.
	s := newSerial(t, fl, serialCfg(10, config.SerialKey{
		KeyLine:   "dtr",
		PTTLine:   "rts",
		PTTTailMS: 200,
	}))

	if _, err := s.Enqueue("OOOO OOOO OOOO", Append); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "keying to start", 3*time.Second, func() bool { return fl.asserted("DTR") })

	s.Abort()

	if fl.asserted("DTR") {
		t.Error("the key line was still down when Abort returned")
	}
	if fl.asserted("RTS") {
		t.Error("PTT was still asserted when Abort returned")
	}
	if st := s.Status(); st.Busy || st.Queued != 0 {
		t.Errorf("after abort: %+v, want an empty idle queue", st)
	}

	// One element at 10 wpm is 120 ms; nothing may be keyed after that.
	before := fl.count()
	time.Sleep(300 * time.Millisecond)
	if after := fl.count(); after != before {
		t.Errorf("%d edges after the abort", after-before)
	}

	// And the sender still works afterwards.
	if _, err := s.Enqueue("E", Append); err != nil {
		t.Fatalf("Enqueue after abort: %v", err)
	}
	waitFor(t, "keying to resume", 3*time.Second, func() bool { return fl.count() > before })
}

func TestSerialKeyerRejectsCharactersItCannotKey(t *testing.T) {
	fl := newFakeLines()
	s := newSerial(t, fl, serialCfg(30, config.SerialKey{KeyLine: "dtr"}))

	var ce *CharError
	_, err := s.Enqueue("CQ; DE", Append)
	if !errors.As(err, &ce) {
		t.Fatalf("got %v, want *CharError", err)
	}
	if ce.Char != ';' || ce.Offset != 2 {
		t.Errorf("got %q at %d, want ';' at 2", ce.Char, ce.Offset)
	}
	if ce.Charset != s.Charset() {
		t.Error("CharError should carry the charset the API will quote")
	}

	// A stray caret is reported as the offending character, so the API can say
	// which one it was.
	if _, err := s.Enqueue("CQ ^ DE", Append); !errors.As(err, &ce) || ce.Char != '^' || ce.Offset != 3 {
		t.Errorf("stray caret: got %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if fl.count() != 0 {
		t.Error("rejected text was keyed")
	}
}

func TestSerialKeyerSendsAFullerCharsetThanTheRigs(t *testing.T) {
	fl := newFakeLines()
	s := newSerial(t, fl, serialCfg(40, config.SerialKey{KeyLine: "rts"}))
	// Locally generated Morse can key any run-together, not only the eight a
	// Kenwood has symbols for.
	if _, err := s.Enqueue("^SOS ^VE ^CT", Append); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := s.Enqueue("de oh2xyz: 5nn @ 14.055 (up)", Append); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	s.Abort()
}

func TestSerialKeyerStatus(t *testing.T) {
	fl := newFakeLines()
	s := newSerial(t, fl, serialCfg(20, config.SerialKey{KeyLine: "dtr"}))

	if st := s.Status(); st.Busy || st.Queued != 0 || st.EstRemainingMS != 0 {
		t.Errorf("idle status %+v", st)
	}

	// PARIS and its word gap is exactly three seconds at 20 wpm.
	if _, err := s.Enqueue("PARIS ", Append); err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if st.WPM != 20 {
		t.Errorf("wpm %d, want 20", st.WPM)
	}
	if st.Queued < 5 || st.Queued > 6 {
		t.Errorf("queued %d, want 6 (or 5 with the first character already keying)", st.Queued)
	}
	if !st.Busy {
		t.Error("status should be busy while keying")
	}
	if st.EstRemainingMS < 2600 || st.EstRemainingMS > 3100 {
		t.Errorf("est remaining %d ms, want about 3000", st.EstRemainingMS)
	}

	s.Abort()
	if st := s.Status(); st.Busy || st.Queued != 0 || st.EstRemainingMS != 0 {
		t.Errorf("after abort: %+v", st)
	}
}

func TestSerialKeyerReplace(t *testing.T) {
	fl := newFakeLines()
	s := newSerial(t, fl, serialCfg(10, config.SerialKey{KeyLine: "dtr"}))

	if _, err := s.Enqueue("OOOO OOOO OOOO OOOO", Append); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue("E", Replace); err != nil {
		t.Fatal(err)
	}
	// Only what has not started keying is dropped, so what remains is the new
	// text plus, at most, the character already on its way out.
	if got := s.Status().Queued; got > 2 {
		t.Errorf("queued %d after replace, want at most 2", got)
	}
}

func TestSerialKeyerSetSpeedIsLocal(t *testing.T) {
	fl := newFakeLines()
	s := newSerial(t, fl, serialCfg(20, config.SerialKey{KeyLine: "dtr"}))
	for _, c := range []struct{ ask, want int }{{35, 35}, {900, MaxWPM}, {2, MinWPM}} {
		if err := s.SetSpeed(c.ask); err != nil {
			t.Fatalf("SetSpeed(%d): %v", c.ask, err)
		}
		if got := s.Status().WPM; got != c.want {
			t.Errorf("SetSpeed(%d): got %d, want %d", c.ask, got, c.want)
		}
	}
}

// TestSerialKeyerWeight checks the documented property of the weighting knob:
// it changes the mark-to-space ratio without changing how long the message
// takes.
func TestSerialKeyerWeight(t *testing.T) {
	if testing.Short() {
		t.Skip("timing comparison is too tight for -short")
	}
	ratio := func(weight int) (float64, time.Duration) {
		t.Helper()
		fl := newFakeLines()
		s := newSerial(t, fl, serialCfg(20, config.SerialKey{KeyLine: "dtr", Weight: weight}))
		if _, err := s.Enqueue("EEEE", Append); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "four dits", 5*time.Second, func() bool { return fl.count() >= 8 })
		marks, gaps := keyIntervals(fl.edges(), "DTR")
		var m, g time.Duration
		for _, d := range marks {
			m += d
		}
		for _, d := range gaps {
			g += d
		}
		es := fl.edges()
		return float64(m) / float64(g), es[len(es)-1].at.Sub(es[0].at)
	}

	neutral, neutralTotal := ratio(50)
	heavy, heavyTotal := ratio(70)
	if heavy <= neutral {
		t.Errorf("weight 70 gave a mark/space ratio of %.2f, want more than the neutral %.2f", heavy, neutral)
	}
	// Thirteen units at 20 wpm is 780 ms whatever the weighting.
	assertNear(t, "neutral total", neutralTotal, 780*time.Millisecond)
	assertNear(t, "weighted total", heavyTotal, 780*time.Millisecond)
}

func TestSerialKeyerWithoutPTT(t *testing.T) {
	fl := newFakeLines()
	s := newSerial(t, fl, serialCfg(30, config.SerialKey{KeyLine: "rts"}))
	if _, err := s.Enqueue("EE", Append); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "two dits", 5*time.Second, func() bool { return fl.count() >= 4 })
	for _, e := range fl.edges() {
		if e.line != "RTS" {
			t.Fatalf("full break-in keyed %s; only the key line should move", e.line)
		}
	}
}

func TestNewSerialKeyValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.CW
	}{
		{"no serial_key block", config.CW{DefaultWPM: 20}},
		{"no key line", serialCfg(20, config.SerialKey{})},
		{"unknown key line", serialCfg(20, config.SerialKey{KeyLine: "cts"})},
		{"unknown ptt line", serialCfg(20, config.SerialKey{KeyLine: "dtr", PTTLine: "dsr"})},
		{"key and ptt on one line", serialCfg(20, config.SerialKey{KeyLine: "dtr", PTTLine: "dtr"})},
		{"negative lead", serialCfg(20, config.SerialKey{KeyLine: "dtr", PTTLeadMS: -1})},
	}
	for _, c := range cases {
		if _, err := NewSerialKey(newFakeLines(), c.cfg); err == nil {
			t.Errorf("%s: expected an error", c.name)
		}
	}
	if _, err := NewSerialKey(nil, serialCfg(20, config.SerialKey{KeyLine: "dtr"})); err == nil {
		t.Error("expected an error with no control lines")
	}
}

// TestSerialKeyerHoldsTheSchedule measures a whole message end to end rather
// than one element at a time. Edges are computed as offsets from a single
// absolute start instant, so a sixteen-edge message must not have accumulated
// sixteen sleep overshoots by the end of it.
func TestSerialKeyerHoldsTheSchedule(t *testing.T) {
	if testing.Short() {
		t.Skip("the end-to-end span needs absolute timings")
	}
	const wpm = 20
	unit := 1200 * time.Millisecond / wpm

	fl := newFakeLines()
	s := newSerial(t, fl, serialCfg(wpm, config.SerialKey{KeyLine: "dtr"}))
	// Two Hs: eight dits, and seventeen units from the first edge to the last.
	if _, err := s.Enqueue("HH", Append); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "eight dits", 5*time.Second, func() bool { return fl.count() >= 16 })

	es := fl.edges()
	span := es[15].at.Sub(es[0].at)
	want := 17 * unit
	// A tenth of the span, which a per-edge overshoot of six milliseconds would
	// already break, but which a single scheduling hiccup will not.
	if tol := want / 10; span < want-tol || span > want+tol {
		t.Errorf("span of HH: got %v, want %v +/- %v", span, want, tol)
	}
}
