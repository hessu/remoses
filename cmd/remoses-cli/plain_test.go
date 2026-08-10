package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// clock is a hand-wound clock, so the throttling rules can be tested without
// waiting for them.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newPlainFixture(t *testing.T, meterInterval time.Duration) (*bytes.Buffer, *plainRenderer, *view, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 8, 4, 20, 11, 4, 0, time.UTC)}
	var buf bytes.Buffer
	r := newPlainRenderer(&buf, c.now, meterInterval)

	v := newView("ic7610", c.now)
	v.desc = testRadio()
	v.setState(sampleState())
	v.link = linkLive
	return &buf, r, v, c
}

func lines(buf *bytes.Buffer) []string {
	s := strings.TrimRight(buf.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestPlainRendererFirstLineCarriesEverything(t *testing.T) {
	buf, r, v, _ := newPlainFixture(t, time.Second)
	r.update(v, updateState)

	got := lines(buf)
	if len(got) != 1 {
		t.Fatalf("want one line, got %d: %q", len(got), buf.String())
	}
	line := got[0]

	for _, want := range []string{
		"ic7610", "status",
		"connected=true", "freq=14025000", "mode=CW", "data=false",
		"passband=500", "filter=2", "power_pct=40", "ptt=false",
		"s_raw=78", "s_scale=255", "s_units=5.5",
		"cw_busy=false", "cw_queued=0", "wpm=28",
		"seq=4471", "age_ms=0", "stale=false",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("line is missing %q:\n%s", want, line)
		}
	}
	if !strings.HasPrefix(line, "2026-08-04T20:11:04.000") {
		t.Errorf("line is not timestamped: %s", line)
	}
}

// Escape sequences in a pipe produce a file nobody can read. This is the whole
// reason the non-TTY path exists.
func TestPlainRendererEmitsNoEscapes(t *testing.T) {
	buf, r, v, c := newPlainFixture(t, 0)
	r.update(v, updateState)
	v.link = linkReconnecting
	v.linkNote = "retry in 1.0 s (attempt 1): connection refused"
	r.update(v, updateLink)
	r.update(v, updateResync)
	c.add(time.Second)
	r.close()

	if strings.ContainsRune(buf.String(), '\x1b') {
		t.Errorf("line output contains an escape sequence:\n%q", buf.String())
	}
}

func TestPlainRendererSkipsUnchangedState(t *testing.T) {
	buf, r, v, _ := newPlainFixture(t, time.Second)
	r.update(v, updateState)
	// The websocket sends a snapshot per radio on connect, on top of the REST
	// fetch that is already on screen. Repeating it must not repeat the line.
	r.update(v, updateState)
	r.update(v, updateState)

	if got := len(lines(buf)); got != 1 {
		t.Errorf("want one line, got %d:\n%s", got, buf.String())
	}
}

func TestPlainRendererPrintsSignificantChangesImmediately(t *testing.T) {
	buf, r, v, _ := newPlainFixture(t, time.Hour)
	r.update(v, updateState)

	st := sampleState()
	st.Frequency = 14025300
	v.setState(st)
	r.update(v, updateState)

	got := lines(buf)
	if len(got) != 2 {
		t.Fatalf("want two lines, got %d:\n%s", len(got), buf.String())
	}
	if !strings.Contains(got[1], "freq=14025300") {
		t.Errorf("second line is missing the new frequency: %s", got[1])
	}
}

// The S meter moves on every poll. At the daemon's coalescing floor that is
// twenty lines a second of nothing else happening, which is not a log.
func TestPlainRendererThrottlesMeterOnlyChanges(t *testing.T) {
	buf, r, v, c := newPlainFixture(t, time.Second)
	r.update(v, updateState)

	bump := func(raw int) {
		st := sampleState()
		st.SMeter.Raw = raw
		v.setState(st)
		r.update(v, updateState)
	}

	c.add(100 * time.Millisecond)
	bump(90)
	c.add(100 * time.Millisecond)
	bump(100)
	if got := len(lines(buf)); got != 1 {
		t.Fatalf("meter changes inside the interval produced %d lines:\n%s", got, buf.String())
	}

	c.add(time.Second)
	bump(120)
	got := lines(buf)
	if len(got) != 2 {
		t.Fatalf("want two lines after the interval, got %d:\n%s", len(got), buf.String())
	}
	if !strings.Contains(got[1], "s_raw=120") {
		t.Errorf("second line is missing the meter reading: %s", got[1])
	}
}

func TestPlainRendererReportsStreamState(t *testing.T) {
	buf, r, v, _ := newPlainFixture(t, time.Second)
	r.update(v, updateState)

	v.link = linkReconnecting
	v.linkNote = `retry in 1.0 s (attempt 1): connection refused`
	r.update(v, updateLink)
	// Repeating the same link state is not news.
	r.update(v, updateLink)

	got := lines(buf)
	if len(got) != 2 {
		t.Fatalf("want two lines, got %d:\n%s", len(got), buf.String())
	}
	if !strings.Contains(got[1], "stream state=reconnecting") {
		t.Errorf("stream line = %s", got[1])
	}
	if !strings.Contains(got[1], "attempt 1") {
		t.Errorf("stream line lost the retry detail: %s", got[1])
	}
}

func TestPlainRendererLogsResync(t *testing.T) {
	buf, r, v, _ := newPlainFixture(t, time.Second)
	r.update(v, updateResync)

	got := lines(buf)
	if len(got) != 1 || !strings.Contains(got[0], "resync") {
		t.Fatalf("resync was not logged:\n%s", buf.String())
	}
	if !strings.Contains(got[0], "refetching state") {
		t.Errorf("resync line does not say what happens next: %s", got[0])
	}
}

func TestPlainRendererIgnoresTicks(t *testing.T) {
	buf, r, v, c := newPlainFixture(t, time.Second)
	r.update(v, updateState)
	c.add(10 * time.Second)
	r.update(v, updateTick)

	if got := len(lines(buf)); got != 1 {
		t.Errorf("a clock tick produced a line:\n%s", buf.String())
	}
}

func TestPlainRendererRendersADisconnectedRadio(t *testing.T) {
	buf, r, v, _ := newPlainFixture(t, time.Second)
	r.update(v, updateState)

	v.applyConn(false, "port closed")
	r.update(v, updateState)

	got := lines(buf)
	if len(got) != 2 {
		t.Fatalf("want two lines, got %d:\n%s", len(got), buf.String())
	}
	if !strings.Contains(got[1], "connected=false") {
		t.Errorf("disconnect not reported: %s", got[1])
	}
	if !strings.Contains(got[1], `conn_error="port closed"`) {
		t.Errorf("the reason is missing: %s", got[1])
	}
	// The last known numbers are still there: a disconnected radio is a state
	// to report, not a blank.
	if !strings.Contains(got[1], "freq=14025000") {
		t.Errorf("the stale snapshot lost its frequency: %s", got[1])
	}
}

func TestPlainRendererSkipsBeforeAnyState(t *testing.T) {
	c := &clock{t: time.Now()}
	var buf bytes.Buffer
	r := newPlainRenderer(&buf, c.now, time.Second)
	r.update(newView("ic7610", c.now), updateState)

	if buf.Len() != 0 {
		t.Errorf("printed a status line with no state:\n%s", buf.String())
	}
}

func TestStatusFieldsOmitsAbsentOptionalValues(t *testing.T) {
	_, _, v, _ := newPlainFixture(t, time.Second)
	st := sampleState()
	st.SMeter.S = nil
	st.Power.Watts = nil
	v.setState(st)

	got := statusFields(v)
	if strings.Contains(got, "s_units=") {
		t.Errorf("an uncalibrated meter reported S units: %s", got)
	}
	if strings.Contains(got, "power_w=") {
		t.Errorf("a relative power scale reported watts: %s", got)
	}
	if strings.Contains(got, "conn_error=") {
		t.Errorf("a connected radio reported a disconnect reason: %s", got)
	}
}
