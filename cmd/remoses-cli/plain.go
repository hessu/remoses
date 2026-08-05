package main

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// tsLayout keeps the offset, so a log collected at a radio site and read
// somewhere else still says when things happened.
const tsLayout = "2006-01-02T15:04:05.000Z07:00"

// plainRenderer writes one timestamped line per change instead of redrawing.
//
// This is what `remoses-cli ic7610 | tee log` gets. Cursor movement and erase
// sequences in a pipe produce a file nobody can read, and a file that repeats
// the same frame twenty times a second is not much better — so the rules here
// are about which changes deserve a line, not about how to draw one.
type plainRenderer struct {
	w   io.Writer
	now func() time.Time
	// meterInterval throttles lines whose only change is a meter. The S meter
	// moves on every poll, and at the daemon's default coalescing floor that is
	// twenty lines a second of nothing else happening.
	meterInterval time.Duration

	started       bool
	lastSig       significant
	lastMeters    meters
	lastMeterLine time.Time
	lastLink      linkState
	lastLinkNote  string
}

func newPlainRenderer(w io.Writer, now func() time.Time, meterInterval time.Duration) *plainRenderer {
	if now == nil {
		now = time.Now
	}
	return &plainRenderer{w: w, now: now, meterInterval: meterInterval}
}

func (r *plainRenderer) update(v *view, kind updateKind) {
	switch kind {
	case updateLink:
		if v.link == r.lastLink && v.linkNote == r.lastLinkNote {
			return
		}
		r.lastLink, r.lastLinkNote = v.link, v.linkNote
		r.line(v, "stream", fmt.Sprintf("state=%s%s", v.link, optQuoted(" note=", v.linkNote)))
		return

	case updateResync:
		r.line(v, "resync", `note="server reported dropped events; refetching state"`)
		return

	case updateTick:
		// Nothing changed; the clock moved. A redrawing display cares, a log
		// does not.
		return
	}

	if !v.haveState {
		return
	}

	sig, m, now := v.significant(), v.meters(), r.now()
	switch {
	case !r.started, sig != r.lastSig:
	case m != r.lastMeters && now.Sub(r.lastMeterLine) >= r.meterInterval:
	default:
		return
	}

	r.started = true
	r.lastSig, r.lastMeters, r.lastMeterLine = sig, m, now
	r.line(v, "status", statusFields(v))
}

func (r *plainRenderer) close() {}

func (r *plainRenderer) line(v *view, kind, fields string) {
	fmt.Fprintf(r.w, "%s %s %s %s\n", r.now().Format(tsLayout), v.radioID, kind, fields)
}

// statusFields renders the whole state as key=value pairs.
//
// The whole state rather than the delta, because each line has to stand on its
// own: a log is read by tailing it or by grepping one line out of the middle,
// and a line saying only what moved forces the reader to reconstruct the rest.
func statusFields(v *view) string {
	st := v.st
	var b strings.Builder

	fmt.Fprintf(&b, "connected=%t freq=%d mode=%s data=%t",
		st.Connected, st.Frequency, st.Mode, st.DataMode)
	fmt.Fprintf(&b, " passband=%d filter=%d", st.PassbandHz, st.FilterSlot)
	fmt.Fprintf(&b, " power_pct=%g", st.Power.Pct)
	if st.Power.Watts != nil {
		fmt.Fprintf(&b, " power_w=%g", *st.Power.Watts)
	}
	fmt.Fprintf(&b, " ptt=%t", st.PTT)
	fmt.Fprintf(&b, " s_raw=%d s_scale=%d", st.SMeter.Raw, st.SMeter.Scale)
	if st.SMeter.S != nil {
		fmt.Fprintf(&b, " s_units=%g", *st.SMeter.S)
	}
	fmt.Fprintf(&b, " cw_busy=%t cw_queued=%d wpm=%d", st.CW.Busy, st.CW.Queued, st.CW.WPM)
	fmt.Fprintf(&b, " seq=%d age_ms=%d stale=%t", st.Seq, v.age().Milliseconds(), v.stale)
	b.WriteString(optQuoted(" conn_error=", v.connErr))
	return b.String()
}

// optQuoted renders an optional quoted field, or nothing at all when it is
// empty, so that absent means absent rather than an empty string a reader has
// to interpret.
func optQuoted(key, value string) string {
	if value == "" {
		return ""
	}
	return key + fmt.Sprintf("%q", value)
}
