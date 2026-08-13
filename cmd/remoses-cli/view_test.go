package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hessu/remoses/internal/wire"
)

func f64(v float64) *float64 { return &v }

// ptr is for the optional fields of a generated type, where "the radio cannot
// report this" is spelled as a nil pointer.
func ptr[T any](v T) *T { return &v }

// sampleState is the DESIGN.md §8 example, so the display is exercised against
// numbers the contract actually documents.
func sampleState() wire.State {
	return wire.State{
		Frequency:  14025000,
		Mode:       wire.ModeCW,
		PassbandHz: 500,
		FilterSlot: 2,
		Power:      wire.Power{Pct: 40, Native: 102},
		SMeter:     wire.Meter{Raw: 78, Scale: 255, S: f64(5.5)},
		CW:         wire.CWStatus{WPM: 28},
		Connected:  true,
		Seq:        4471,
		UpdatedAt:  time.Date(2026, 8, 4, 20, 11, 4, 0, time.UTC),
	}
}

// testRadio is the descriptor the fixtures share. Its capabilities are not
// decoration: they decide which fields the renderers draw at all, so an
// IC-7610 has to say it has the power and filter commands an IC-7610 has.
func testRadio() *wire.Radio {
	return &wire.Radio{
		ID: "ic7610", Name: "IC-7610", Backend: wire.BackendCiv, Connected: true,
		Caps: wire.Caps{PowerControl: true, FilterWidth: true, FilterSlots: 3},
	}
}

func newTestView() *view {
	now := time.Date(2026, 8, 4, 20, 11, 4, 0, time.UTC)
	v := newView("ic7610", func() time.Time { return now })
	v.desc = testRadio()
	v.setState(sampleState())
	v.link = linkLive
	return v
}

func TestFormatFreq(t *testing.T) {
	tests := map[int64]string{
		14025300:   "14.025.300",
		14025000:   "14.025.000",
		1840000:    "1.840.000",
		475000:     "0.475.000",
		144300000:  "144.300.000",
		1296100000: "1296.100.000",
	}
	for hz, want := range tests {
		if got := formatFreq(hz); got != want {
			t.Errorf("formatFreq(%d) = %q, want %q", hz, got, want)
		}
	}
}

func TestFormatSUnit(t *testing.T) {
	if got := formatSUnit(wire.Meter{Raw: 10, Scale: 255}); got != "" {
		t.Errorf("uncalibrated meter = %q, want empty", got)
	}
	tests := map[float64]string{
		5.5:    "S5.5",
		9:      "S9",
		3:      "S3",
		9.5:    "S9+3 dB",
		12.333: "S9+20 dB",
	}
	for s, want := range tests {
		got := formatSUnit(wire.Meter{Raw: 1, Scale: 255, S: f64(s)})
		if got != want {
			t.Errorf("formatSUnit(%v) = %q, want %q", s, got, want)
		}
	}
}

func TestMeterBar(t *testing.T) {
	empty := meterBar(0, 20)
	if utf8.RuneCountInString(empty) != 20 || strings.ContainsRune(empty, '█') {
		t.Errorf("empty bar = %q", empty)
	}
	full := meterBar(1, 20)
	if utf8.RuneCountInString(full) != 20 || strings.ContainsRune(full, '░') {
		t.Errorf("full bar = %q", full)
	}
	half := meterBar(0.5, 20)
	if utf8.RuneCountInString(half) != 20 {
		t.Errorf("half bar has %d cells: %q", utf8.RuneCountInString(half), half)
	}
	if got := strings.Count(half, "█"); got != 10 {
		t.Errorf("half bar has %d full cells, want 10: %q", got, half)
	}
	// Out-of-range input must not produce a ragged bar: the meter's scale is
	// the rig's, and a rig that reports past its own maximum is not unheard of.
	for _, frac := range []float64{-1, 2} {
		if n := utf8.RuneCountInString(meterBar(frac, 20)); n != 20 {
			t.Errorf("meterBar(%v) has %d cells", frac, n)
		}
	}
	if meterBar(0.5, 0) != "" {
		t.Error("a zero-width bar should be empty")
	}
}

// The bar has to move for a change the radio can actually report, or it reads
// as a stalled meter.
func TestMeterBarResolvesSingleUnitSteps(t *testing.T) {
	a := meterBar(78.0/255, meterCells)
	b := meterBar(79.0/255, meterCells)
	if a == b {
		t.Errorf("one raw unit of a 255-point scale did not move the bar: %q", a)
	}
}

func TestFormatPower(t *testing.T) {
	if got := formatPower(wire.Power{Pct: 40, Native: 102}); got != "40 %" {
		t.Errorf("relative power = %q", got)
	}
	if got := formatPower(wire.Power{Pct: 40, Native: 40, Watts: f64(40)}); got != "40 %  40 W" {
		t.Errorf("watt-accurate power = %q", got)
	}
}

func TestFormatMode(t *testing.T) {
	if got := formatMode(wire.State{Mode: wire.ModeCW}); got != "CW" {
		t.Errorf("mode = %q", got)
	}
	// Data mode is orthogonal to the mode, as the rigs model it; the display
	// joins them without the state pretending they are one value.
	if got := formatMode(wire.State{Mode: wire.ModeUSB, DataMode: true}); got != "USB/D" {
		t.Errorf("data mode = %q", got)
	}
}

func TestFormatCW(t *testing.T) {
	idle := formatCW(wire.CWStatus{WPM: 28})
	if !strings.HasPrefix(idle, "idle") || !strings.Contains(idle, "28 wpm") {
		t.Errorf("idle = %q", idle)
	}
	busy := formatCW(wire.CWStatus{Busy: true, Queued: 12, WPM: 28, EstRemainingMS: 4300})
	for _, want := range []string{"sending", "queued 12", "28 wpm", "~4.3 s"} {
		if !strings.Contains(busy, want) {
			t.Errorf("busy = %q, missing %q", busy, want)
		}
	}
}

func TestFormatAge(t *testing.T) {
	tests := map[time.Duration]string{
		0:                       "0.0 s",
		1500 * time.Millisecond: "1.5 s",
		30 * time.Second:        "30 s",
		90 * time.Second:        "1 m 30 s",
		3660 * time.Second:      "1 h 01 m",
	}
	for d, want := range tests {
		if got := formatAge(d); got != want {
			t.Errorf("formatAge(%v) = %q, want %q", d, got, want)
		}
	}
}

// Age is measured from the age the server reported, plus local elapsed time.
// Subtracting UpdatedAt from a local clock would report clock skew between two
// machines as staleness.
func TestAgeIsServerReportedPlusLocalElapsed(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 11, 4, 0, time.UTC)
	v := newView("ic7610", func() time.Time { return now })

	st := sampleState()
	// A snapshot whose UpdatedAt is an hour ahead of this machine's clock.
	st.UpdatedAt = now.Add(time.Hour)
	st.AgeMS = ptr(120)
	v.setSnapshot(&st)

	if got := v.age(); got != 120*time.Millisecond {
		t.Fatalf("age = %v, want 120ms", got)
	}
	now = now.Add(2 * time.Second)
	if got := v.age(); got != 2120*time.Millisecond {
		t.Errorf("age after 2s = %v, want 2.12s", got)
	}
}

// A websocket snapshot has just been read from the session cache, so its age
// starts again from zero.
func TestStreamUpdateResetsAge(t *testing.T) {
	now := time.Now()
	v := newView("ic7610", func() time.Time { return now })
	snap := sampleState()
	snap.AgeMS, snap.Stale = ptr(5000), ptr(true)
	v.setSnapshot(&snap)
	if !v.stale {
		t.Fatal("stale was not taken from the snapshot")
	}

	v.setState(sampleState())
	if v.age() != 0 {
		t.Errorf("age after a live update = %v, want 0", v.age())
	}
	if v.stale {
		t.Error("a live update proves the poller is delivering; stale should clear")
	}
}

func TestSignificantIgnoresMeters(t *testing.T) {
	v := newTestView()
	before := v.significant()

	st := sampleState()
	st.SMeter = wire.Meter{Raw: 200, Scale: 255, S: f64(9)}
	v.setState(st)

	if v.significant() != before {
		t.Error("a meter change must not count as a significant change")
	}
	if v.meters() == (meters{sRaw: 78, sScale: 255}) {
		t.Error("the meter change was not recorded")
	}

	st.Frequency = 14025300
	v.setState(st)
	if v.significant() == before {
		t.Error("a frequency change is significant")
	}
}

// Power carries a pointer, so the naive comparison of two states would compare
// addresses. significant flattens it for exactly that reason.
func TestSignificantComparesPowerByValue(t *testing.T) {
	v := newTestView()
	st := sampleState()
	st.Power = wire.Power{Pct: 40, Native: 40, Watts: f64(40)}
	v.setState(st)
	a := v.significant()

	st.Power = wire.Power{Pct: 40, Native: 40, Watts: f64(40)}
	v.setState(st)
	if v.significant() != a {
		t.Error("two equal powers compared unequal")
	}

	st.Power = wire.Power{Pct: 50, Native: 50, Watts: f64(50)}
	v.setState(st)
	if v.significant() == a {
		t.Error("a power change went unnoticed")
	}
}

func TestApplyConnClearsTheErrorOnReconnect(t *testing.T) {
	v := newTestView()
	v.applyConn(false, "port closed")
	if v.st.Connected || v.connErr != "port closed" {
		t.Fatalf("after disconnect: connected=%t err=%q", v.st.Connected, v.connErr)
	}
	v.applyConn(true, "")
	if !v.st.Connected || v.connErr != "" {
		t.Errorf("after reconnect: connected=%t err=%q", v.st.Connected, v.connErr)
	}
}

func TestRadioNameFallsBackToTheID(t *testing.T) {
	v := newView("ic7610", time.Now)
	if got := v.radioName(); got != "ic7610" {
		t.Errorf("name before the descriptor arrives = %q", got)
	}
	v.desc = &wire.Radio{Name: "IC-7610"}
	if got := v.radioName(); got != "IC-7610" {
		t.Errorf("name = %q", got)
	}
}

func TestLinkStateStrings(t *testing.T) {
	for l, want := range map[linkState]string{
		linkConnecting: "connecting", linkLive: "live",
		linkReconnecting: "reconnecting", linkClosed: "closed",
	} {
		if got := l.String(); got != want {
			t.Errorf("linkState(%d) = %q, want %q", l, got, want)
		}
	}
}
