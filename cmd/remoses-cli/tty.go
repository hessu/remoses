package main

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/hessu/remoses/internal/radio"
)

// Frame width bounds. Below the minimum the two-column layout stops lining up;
// above the maximum the right-hand column drifts so far from the left that the
// pair stops reading as one row.
const (
	minWidth = 56
	maxWidth = 100
)

// meterCells is how wide the S-meter bar is drawn. Twenty cells at eighth-block
// resolution give 160 steps, which is finer than any of the meter scales the
// rigs report, so the bar never quantises away a change the radio saw.
const meterCells = 20

// ttyRenderer redraws the same block of lines in place.
//
// It writes only where a terminal is on the other end. The escape sequences
// that make this work — cursor up, erase line — are noise in a pipe, which is
// why the plain renderer exists rather than this one growing a flag.
type ttyRenderer struct {
	w     io.Writer
	width int
	// prev is the last frame written. Comparing frames rather than tracking
	// what changed is what keeps a websocket snapshot arriving on top of an
	// identical REST fetch from flickering: an unchanged frame is not drawn.
	prev []string
	// lines is how far below the start of the block the cursor is.
	lines int
}

func newTTYRenderer(w io.Writer, width int) *ttyRenderer {
	// Hide the cursor: it would otherwise sit blinking in the middle of the
	// block being rewritten. close puts it back.
	fmt.Fprint(w, "\x1b[?25l")
	return &ttyRenderer{w: w, width: clampWidth(width)}
}

func clampWidth(w int) int {
	return min(max(w, minWidth), maxWidth)
}

func (r *ttyRenderer) update(v *view, _ updateKind) {
	lines := frame(v, r.width)
	if equalLines(lines, r.prev) {
		return
	}
	r.draw(lines)
	r.prev = lines
}

// draw rewrites the block: back to the top, then one erased line at a time.
//
// Erasing per line rather than clearing the screen keeps whatever was in the
// terminal before this program started, which is the difference between a tool
// that cooperates with a shell session and one that takes it over.
func (r *ttyRenderer) draw(lines []string) {
	var b strings.Builder
	if r.lines > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", r.lines)
	}
	for _, l := range lines {
		b.WriteString("\r\x1b[2K")
		b.WriteString(l)
		b.WriteString("\n")
	}
	// A shorter frame than last time leaves the tail of the old one on screen;
	// erase it, then come back so the cursor is exactly one frame below the top.
	if extra := r.lines - len(lines); extra > 0 {
		for range extra {
			b.WriteString("\r\x1b[2K\n")
		}
		fmt.Fprintf(&b, "\x1b[%dA", extra)
	}
	r.lines = len(lines)
	io.WriteString(r.w, b.String())
}

func (r *ttyRenderer) close() {
	fmt.Fprint(r.w, "\x1b[?25h")
}

// frame renders the whole display as plain lines, none wider than the terminal.
//
// It is separate from the escape-sequence handling above so that the layout can
// be tested, and so that -once can print one frame without any of the cursor
// machinery. Fitting the width is decided here rather than in the drawing code:
// a line that wrapped would leave the cursor somewhere other than where the
// redraw arithmetic expects it, and the display would walk down the screen.
func frame(v *view, width int) []string {
	width = clampWidth(width)
	lines := layout(v, width)
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return lines
}

// layout places the content. frame is what callers use; this half exists only
// so the width fitting has one home.
func layout(v *view, width int) []string {
	head := "remoses  " + v.radioID
	if name := v.radioName(); name != v.radioID {
		head += " - " + name
	}
	if v.desc != nil && v.desc.Backend != "" {
		head += " - " + v.desc.Backend
	}

	out := []string{
		row(head, radioBadge(v), width),
		strings.Repeat("-", width),
		"",
	}

	if !v.haveState {
		out = append(out, "  waiting for the first state snapshot", "")
		return append(out, footer(v, width)...)
	}

	st := v.st

	ptt := "PTT   RX"
	if st.PTT {
		// No colour, because this has to be as visible over ssh in a plain
		// xterm as it is in a modern terminal.
		ptt = "PTT   >> TX <<"
	}
	out = append(out,
		row(fmt.Sprintf("  %s MHz    %s", formatFreq(st.Frequency), formatMode(st)), ptt, width),
		"")

	out = append(out, meterLines(v)...)
	out = append(out, "")

	filter := fmt.Sprintf("  passband  %d Hz", st.PassbandHz)
	if st.FilterSlot > 0 {
		filter += fmt.Sprintf("   filter %d", st.FilterSlot)
	}
	out = append(out,
		row(filter, "power  "+formatPower(st.Power), width),
		row("  cw        "+formatCW(st.CW), lockNote(v), width),
	)
	if st.Tuner != radio.TunerUnknown {
		out = append(out, "  tuner     "+formatTuner(st.Tuner))
	}

	if note := stateNote(v); note != "" {
		out = append(out, "", "  "+note)
	}
	out = append(out, "")
	return append(out, footer(v, width)...)
}

// meterLines draws the meter block: the S meter in receive, and forward power,
// SWR and ALC while transmitting.
//
// They swap rather than stack, which is what the radio's own meter does and
// what the numbers justify. During a transmission the S reading is not merely
// uninteresting, it is wrong: on a Kenwood the command that reports it is
// reporting the power meter instead, so what is left in State is whatever the
// last receive poll saw, sitting there looking current.
//
// Only the meters the radio actually reports get a line — an FT-857 has power
// and a high-SWR bit and no ALC — so the block is as tall as the radio has
// something to say, and the renderer redraws a changed line count in place.
func meterLines(v *view) []string {
	st := v.st
	if !v.haveTXMeters() {
		sUnit := formatSUnit(st.SMeter)
		if sUnit != "" {
			sUnit += "  "
		}
		return []string{fmt.Sprintf("  S   %s  %s%d/%d",
			meterBar(st.SMeter.Fraction(), meterCells), sUnit, st.SMeter.Raw, st.SMeter.Scale)}
	}

	var out []string
	if m := st.PowerMeter; m != nil {
		out = append(out, fmt.Sprintf("  PWR %s  %s",
			meterBar(m.Fraction(), meterCells), formatMeterValue(*m)))
	}
	if m := st.SWR; m != nil {
		out = append(out, fmt.Sprintf("  SWR %s  %s",
			meterBar(m.Fraction(), meterCells), formatSWR(*m, st.SWRRatio)))
	}
	if m := st.ALC; m != nil {
		out = append(out, fmt.Sprintf("  ALC %s  %s",
			meterBar(m.Fraction(), meterCells), formatMeterValue(*m)))
	}
	return out
}

// radioBadge is the radio's own link to its rig, not this program's link to the
// daemon. The two are reported in different places on the frame for that
// reason.
func radioBadge(v *view) string {
	if !v.haveState && v.desc == nil {
		return "UNKNOWN"
	}
	// Standby before connected, because it is the more specific of the two and
	// they are both true. A radio that is switched off is reachable — the link
	// is fine — so showing CONNECTED would leave an operator wondering why
	// nothing works, and DISCONNECTED would send them to check a cable.
	if v.st.Standby {
		return "STANDBY"
	}
	if v.st.Connected {
		return "CONNECTED"
	}
	return "DISCONNECTED"
}

// stateNote explains a state that would otherwise look like a bug: a snapshot
// whose numbers are real but no longer current.
func stateNote(v *view) string {
	switch {
	// The values on screen are whatever was last true before the radio was
	// switched off, so saying so matters more here than for a stale snapshot:
	// nothing about them looks wrong.
	case v.st.Standby:
		// Short enough to survive the narrowest frame: the badge already says
		// STANDBY rather than DISCONNECTED, so this only has to explain why the
		// numbers below it look plausible and are not current.
		return "radio is switched off - readings are from before"
	case !v.st.Connected && v.connErr != "":
		return fmt.Sprintf("radio disconnected: %s - last known state, %s old",
			v.connErr, formatAge(v.age()))
	case !v.st.Connected:
		return fmt.Sprintf("radio disconnected - last known state, %s old", formatAge(v.age()))
	case v.stale:
		return "snapshot is stale - the poller has not refreshed it"
	}
	return ""
}

// lockNote reports who holds exclusive control. This program never takes the
// lock; it shows it so an operator can see that somebody else is on the radio.
func lockNote(v *view) string {
	if v.desc == nil || !v.desc.Lock.Held {
		return "lock   free"
	}
	who := v.desc.Lock.Holder
	if who == "" {
		who = "another user"
	}
	if v.desc.Lock.IsMine {
		who += " (this login)"
	}
	return "lock   " + who
}

func footer(v *view, width int) []string {
	left := fmt.Sprintf("  seq %d   updated %s ago", v.st.Seq, formatAge(v.age()))
	if !v.haveState {
		left = "  no state yet"
	}
	lines := []string{row(left, "stream  "+v.link.String(), width)}
	if v.linkNote != "" {
		lines = append(lines, "  "+v.linkNote)
	}
	return lines
}

// row lays a line out as a left part and a right part flush to width. When the
// two would collide the right part wins: it carries the status, and a truncated
// status is worse than a truncated label.
func row(left, right string, width int) string {
	l, r := runeLen(left), runeLen(right)
	if l+1+r > width {
		left = truncate(left, max(width-r-1, 0))
		l = runeLen(left)
	}
	pad := width - l - r
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// truncate cuts s to w columns. Every rune this program draws is single-width,
// so counting runes is counting columns.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if runeLen(s) <= w {
		return s
	}
	rs := []rune(s)
	return string(rs[:w])
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
