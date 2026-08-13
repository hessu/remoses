package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/client"
	"github.com/hessu/remoses/internal/radio"
)

// joined renders a frame as one string for content assertions. The escape
// sequences are deliberately not part of any test here: what matters is which
// facts reach the screen, not which bytes move the cursor.
func joined(v *view, width int) string {
	return strings.Join(frame(v, width), "\n")
}

func TestFrameShowsWhatAnOperatorWatches(t *testing.T) {
	got := joined(newTestView(), 80)

	for _, want := range []string{
		"ic7610",       // the id
		"IC-7610",      // the name
		"civ",          // the backend
		"CONNECTED",    // the radio's own link
		"14.025.000",   // frequency, the thing watched most
		"MHz",          //
		"CW",           // mode
		"S5.5",         // calibrated S reading
		"78/255",       // and the raw one, because the calibration may be absent
		"500 Hz",       // passband
		"filter 2",     //
		"40 %",         // power
		"PTT   RX",     //
		"28 wpm",       // cw queue
		"seq 4471",     //
		"stream  live", // this program's own link, reported separately
	} {
		if !strings.Contains(got, want) {
			t.Errorf("frame is missing %q:\n%s", want, got)
		}
	}
}

func TestFrameFitsTheTerminal(t *testing.T) {
	v := newTestView()
	v.linkNote = "retry in 4.0 s (attempt 3): dial tcp 127.0.0.1:7342: connect: connection refused"
	v.link = linkReconnecting

	for _, width := range []int{40, 56, 72, 80, 100, 200} {
		want := clampWidth(width)
		for i, line := range frame(v, width) {
			if runeLen(line) > want {
				t.Errorf("width %d: line %d is %d columns:\n%s", want, i, runeLen(line), line)
			}
		}
	}
}

func TestFrameMarksTransmitting(t *testing.T) {
	v := newTestView()
	st := sampleState()
	st.PTT = true
	v.setState(st)

	got := joined(v, 80)
	if !strings.Contains(got, "TX") {
		t.Errorf("transmitting is not visible:\n%s", got)
	}
	if strings.Contains(got, "PTT   RX") {
		t.Errorf("still showing receive while transmitting:\n%s", got)
	}
}

// A disconnected radio with a stale snapshot is a normal, expected state. It
// must read as "this is what the radio last said", not as an error and not as
// an empty screen.
func TestFrameRendersADisconnectedRadio(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 11, 4, 0, time.UTC)
	v := newView("ic7610", func() time.Time { return now })
	v.desc = testRadio()

	st := sampleState()
	st.Connected = false
	v.setSnapshot(&client.State{State: st, AgeMS: 134000, Stale: true})
	v.link = linkLive

	got := joined(v, 80)
	for _, want := range []string{
		"DISCONNECTED",
		"radio disconnected",
		"last known state",
		"2 m 14 s old",
		"14.025.000", // the last known numbers are still shown
		"stream  live",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("frame is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(strings.ToLower(got), "error") {
		t.Errorf("a disconnected radio is not an error:\n%s", got)
	}
}

// A radio with no command behind a field must not have one drawn for it. An
// FT-857 has no CAT transmit power and no IF bandwidth in either direction, so
// the zeros its State carries are the absence of a reading rather than a small
// one — and "power  0 %" beside a radio putting out ten watts reads as a fault.
func TestFrameOmitsFieldsTheRadioCannotReport(t *testing.T) {
	v := newTestView()
	v.desc = &client.Radio{
		ID: "ft857", Name: "Yaesu FT-857D", Backend: "yaesu", Connected: true,
		Caps: radio.Caps{PTTControl: true, SMeterScale: 15},
	}
	st := sampleState()
	st.PassbandHz, st.FilterSlot = 0, 0
	st.Power = radio.Power{}
	v.setState(st)

	got := joined(v, 80)
	for _, unwanted := range []string{"power", "passband", "filter"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("frame draws %q for a radio that has no such command:\n%s", unwanted, got)
		}
	}
	// The rest of the display is unaffected.
	for _, want := range []string{"14.025.000", "CW", "PTT   RX", "cw "} {
		if !strings.Contains(got, want) {
			t.Errorf("frame is missing %q:\n%s", want, got)
		}
	}
}

// The same three fields in the piped renderer, where a log line is read later
// by somebody who cannot ask what power_pct=0 meant.
func TestPlainOmitsFieldsTheRadioCannotReport(t *testing.T) {
	buf, r, v, _ := newPlainFixture(t, time.Second)
	v.desc = &client.Radio{
		ID: "ft857", Name: "Yaesu FT-857D", Backend: "yaesu", Connected: true,
		Caps: radio.Caps{PTTControl: true, SMeterScale: 15},
	}
	r.update(v, updateState)

	line := buf.String()
	for _, unwanted := range []string{"power_pct=", "power_w=", "passband=", "filter="} {
		if strings.Contains(line, unwanted) {
			t.Errorf("line carries %q for a radio that has no such command:\n%s", unwanted, line)
		}
	}
	if !strings.Contains(line, "freq=14025000") {
		t.Errorf("the rest of the line is missing:\n%s", line)
	}
}

// The frequency line has to say when it is describing the *other* receiver.
// On an IC-9700 with the sub band selected, the frequency and mode are that
// receiver's while the VFO pair below is the main one's, and an operator
// reading "144.300" with no marker has no way to know which is which.
//
// Only for sub: "MAIN" on every line of every radio would be noise.
func TestFrameMarksTheSubReceiver(t *testing.T) {
	v := newTestView()
	st := sampleState()

	st.SelectedBand = radio.BandMain
	v.setState(st)
	if got := joined(v, 80); strings.Contains(got, "SUB") {
		t.Errorf("main is marked; only the sub case is worth interrupting for:\n%s", got)
	}

	st.SelectedBand = radio.BandSub
	v.setState(st)
	if got := joined(v, 80); !strings.Contains(got, "SUB") {
		t.Errorf("the sub receiver is not marked:\n%s", got)
	}
}

func TestFrameShowsWhyTheRadioWentAway(t *testing.T) {
	v := newTestView()
	v.applyConn(false, "port closed")

	got := joined(v, 80)
	if !strings.Contains(got, "port closed") {
		t.Errorf("the disconnect reason is missing:\n%s", got)
	}
}

func TestFrameMarksAStaleSnapshot(t *testing.T) {
	v := newTestView()
	v.stale = true
	if got := joined(v, 80); !strings.Contains(got, "stale") {
		t.Errorf("a stale snapshot is not marked:\n%s", got)
	}
}

func TestFrameShowsReconnecting(t *testing.T) {
	v := newTestView()
	v.link = linkReconnecting
	v.linkNote = "retry in 4.0 s (attempt 3): connection refused"

	got := joined(v, 80)
	if !strings.Contains(got, "stream  reconnecting") {
		t.Errorf("reconnecting is not visible:\n%s", got)
	}
	if !strings.Contains(got, "attempt 3") {
		t.Errorf("the retry schedule is not visible:\n%s", got)
	}
}

func TestFrameBeforeAnyState(t *testing.T) {
	v := newView("ic7610", time.Now)
	got := joined(v, 80)
	if !strings.Contains(got, "waiting") {
		t.Errorf("no placeholder before the first snapshot:\n%s", got)
	}
	if !strings.Contains(got, "UNKNOWN") {
		t.Errorf("connection state should be unknown, not asserted:\n%s", got)
	}
}

func TestFrameShowsWhoHoldsTheLock(t *testing.T) {
	v := newTestView()
	if got := joined(v, 80); !strings.Contains(got, "lock   free") {
		t.Errorf("free lock not shown:\n%s", got)
	}

	v.desc.Lock = client.LockState{Held: true, Holder: "oh2abc"}
	if got := joined(v, 80); !strings.Contains(got, "lock   oh2abc") {
		t.Errorf("lock holder not shown:\n%s", got)
	}
}

func TestRow(t *testing.T) {
	got := row("left", "right", 20)
	if runeLen(got) != 20 {
		t.Errorf("row = %q, %d columns", got, runeLen(got))
	}
	if !strings.HasPrefix(got, "left") || !strings.HasSuffix(got, "right") {
		t.Errorf("row = %q", got)
	}
	// When the two collide the status on the right survives: a truncated
	// status is worse than a truncated label.
	tight := row("a very long label indeed", "STATUS", 16)
	if !strings.HasSuffix(tight, "STATUS") || runeLen(tight) > 16 {
		t.Errorf("tight row = %q", tight)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("abc", 0); got != "" {
		t.Errorf("truncate = %q", got)
	}
	// Multi-byte runes are cut by rune, not by byte.
	if got := truncate("████", 2); runeLen(got) != 2 {
		t.Errorf("truncate = %q, %d runes", got, runeLen(got))
	}
}

// Redrawing an identical frame is what a websocket snapshot arriving on top of
// an identical REST fetch would otherwise cause, and it looks like a flicker.
func TestTTYRendererSkipsUnchangedFrames(t *testing.T) {
	var buf bytes.Buffer
	r := newTTYRenderer(&buf, 80)
	v := newTestView()

	r.update(v, updateState)
	first := buf.Len()
	if first == 0 {
		t.Fatal("nothing was drawn")
	}
	if !strings.Contains(buf.String(), "14.025.000") {
		t.Error("the first frame did not carry the frequency")
	}

	r.update(v, updateState)
	if buf.Len() != first {
		t.Errorf("an unchanged frame was redrawn: %d bytes became %d", first, buf.Len())
	}

	st := sampleState()
	st.Frequency = 14025300
	v.setState(st)
	r.update(v, updateState)
	if buf.Len() == first {
		t.Error("a changed frame was not redrawn")
	}
	if !strings.Contains(buf.String(), "14.025.300") {
		t.Error("the new frequency was not drawn")
	}

	r.close()
}

// A frame that shrinks must not leave the tail of the previous one behind.
func TestTTYRendererClearsAShrunkFrame(t *testing.T) {
	var buf bytes.Buffer
	r := newTTYRenderer(&buf, 80)

	v := newTestView()
	v.linkNote = "retry in 4.0 s"
	tall := len(frame(v, 80))
	r.update(v, updateState)

	v.linkNote = ""
	short := len(frame(v, 80))
	if short >= tall {
		t.Fatalf("test setup: frame did not shrink (%d -> %d)", tall, short)
	}
	r.update(v, updateState)

	if r.lines != short {
		t.Errorf("renderer tracks %d lines, frame has %d", r.lines, short)
	}
	r.close()
}

func TestMeterBarWidthMatchesTheFrame(t *testing.T) {
	v := newTestView()
	st := sampleState()
	st.SMeter = radio.Meter{Raw: 255, Scale: 255}
	v.setState(st)

	got := joined(v, 80)
	if !strings.Contains(got, strings.Repeat("█", meterCells)) {
		t.Errorf("a full-scale meter did not fill the bar:\n%s", got)
	}
}
