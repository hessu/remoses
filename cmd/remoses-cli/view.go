package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/hessu/remoses/internal/client"
	"github.com/hessu/remoses/internal/radio"
)

// linkState is what this monitor's own connection to the daemon is doing. It is
// deliberately separate from radio.State.Connected: "the radio is unplugged"
// and "I cannot reach the daemon" look nothing alike to an operator, and
// collapsing them into one indicator would send someone to the wrong end of the
// link.
type linkState int

const (
	linkConnecting linkState = iota
	linkLive
	linkReconnecting
	linkClosed
)

func (l linkState) String() string {
	switch l {
	case linkConnecting:
		return "connecting"
	case linkLive:
		return "live"
	case linkReconnecting:
		return "reconnecting"
	}
	return "closed"
}

// view is everything on screen: the radio's state, what is known about the
// radio itself, and the health of the connection carrying it.
type view struct {
	radioID string
	desc    *client.Radio

	st        radio.State
	haveState bool
	stale     bool
	// connErr is the reason the radio's port went away, from a conn event. It
	// is the one thing an operator at the far end of the link cannot see for
	// themselves, so it is kept and displayed rather than logged and dropped.
	connErr string

	// ageBase and ageAt render the snapshot's age without trusting a clock this
	// process does not own. The server reports how old its snapshot was when it
	// answered; everything after that is measured locally, so skew between the
	// two machines never turns into apparent staleness.
	ageBase time.Duration
	ageAt   time.Time

	link     linkState
	linkNote string

	now func() time.Time
}

func newView(radioID string, now func() time.Time) *view {
	if now == nil {
		now = time.Now
	}
	return &view{radioID: radioID, link: linkConnecting, now: now}
}

// setSnapshot records a REST fetch.
func (v *view) setSnapshot(s *client.State) {
	v.st = s.State
	v.haveState = true
	v.stale = s.Stale
	v.ageBase, v.ageAt = s.Age(), v.now()
}

// setState records a websocket snapshot. It arrives having just been read from
// the session's cache, so its age restarts at zero.
func (v *view) setState(st radio.State) {
	v.st = st
	v.haveState = true
	v.stale = false
	v.ageBase, v.ageAt = 0, v.now()
}

// applyDelta folds a delta message into the current snapshot.
func (v *view) applyDelta(ev client.Event) error {
	next, err := ev.ApplyDelta(v.st)
	if err != nil {
		return err
	}
	v.setState(next)
	return nil
}

// applyCW records a discrete cw event. Those carry the queue but nothing else,
// so only the queue is touched.
func (v *view) applyCW(cw radio.CWStatus) {
	v.st.CW = cw
	v.haveState = true
	v.ageBase, v.ageAt = 0, v.now()
}

// applyConn records a discrete conn event.
func (v *view) applyConn(connected bool, errText string) {
	v.st.Connected = connected
	v.connErr = errText
	if connected {
		v.connErr = ""
	}
	v.haveState = true
	v.ageBase, v.ageAt = 0, v.now()
}

// age is how old the displayed snapshot is.
func (v *view) age() time.Duration {
	if !v.haveState {
		return 0
	}
	d := v.ageBase + v.now().Sub(v.ageAt)
	if d < 0 {
		return 0
	}
	return d
}

// radioName is the human-readable name, falling back to the id before the
// descriptor has been fetched.
func (v *view) radioName() string {
	if v.desc != nil && v.desc.Name != "" {
		return v.desc.Name
	}
	return v.radioID
}

// significant is the part of the state whose change is worth an output line.
//
// It exists so that both renderers make the same decision from the same value,
// and it is comparable on purpose: the pointers in radio.State would otherwise
// compare by address, which is not what "did anything change" means. The
// meters are excluded — they move continuously and are handled separately.
type significant struct {
	connected  bool
	stale      bool
	connErr    string
	frequency  uint64
	mode       radio.Mode
	dataMode   bool
	passbandHz int
	filterSlot int
	powerPct   float64
	powerW     float64
	havePowerW bool
	ptt        bool
	cw         radio.CWStatus
}

func (v *view) significant() significant {
	s := significant{
		connected:  v.st.Connected,
		stale:      v.stale,
		connErr:    v.connErr,
		frequency:  v.st.Frequency,
		mode:       v.st.Mode,
		dataMode:   v.st.DataMode,
		passbandHz: v.st.PassbandHz,
		filterSlot: v.st.FilterSlot,
		powerPct:   v.st.Power.Pct,
		ptt:        v.st.PTT,
		cw:         v.st.CW,
	}
	if w := v.st.Power.Watts; w != nil {
		s.powerW, s.havePowerW = *w, true
	}
	return s
}

// meters is the continuously-moving part: what a bar is drawn from.
type meters struct {
	sRaw   int
	sScale int
}

func (v *view) meters() meters {
	return meters{sRaw: v.st.SMeter.Raw, sScale: v.st.SMeter.Scale}
}

// formatFreq renders hertz the way a radio's display does: megahertz, then
// kilohertz, then hertz, in groups of three. 14025300 becomes 14.025.300, which
// is readable at a glance in a way that a bare nine-digit integer is not — and
// glancing at the frequency is most of what this tool is for.
func formatFreq(hz uint64) string {
	return fmt.Sprintf("%d.%03d.%03d", hz/1_000_000, (hz/1000)%1000, hz%1000)
}

// formatMode spells the mode and the orthogonal data flag together, because
// that is how an operator reads them, while keeping them separate values
// underneath as the rigs do.
func formatMode(st radio.State) string {
	m := st.Mode.String()
	if st.DataMode {
		m += "/D"
	}
	return m
}

// formatSUnit renders a calibrated S reading.
//
// Above S9 the convention on every radio's meter is decibels over S9 at 6 dB
// per S unit, so that is what is shown rather than a fractional S number nobody
// speaks. An uncalibrated rig reports no s at all and gets an empty string; the
// raw reading is displayed alongside regardless.
func formatSUnit(m radio.Meter) string {
	if m.S == nil {
		return ""
	}
	s := *m.S
	if s > 9 {
		return fmt.Sprintf("S9+%.0f dB", math.Round((s-9)*6))
	}
	if s == math.Trunc(s) {
		return fmt.Sprintf("S%.0f", s)
	}
	return fmt.Sprintf("S%.1f", s)
}

// barRunes are the eighth-width blocks, so the S meter moves smoothly rather
// than in whole-cell jumps. A meter that only advances a character at a time
// reads as a stalled meter.
var barRunes = []rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉'}

// meterBar draws frac (0..1) across cells columns.
func meterBar(frac float64, cells int) string {
	if cells <= 0 {
		return ""
	}
	frac = min(max(frac, 0), 1)
	eighths := int(math.Round(frac * float64(cells) * 8))
	full := eighths / 8
	rem := eighths % 8
	if full > cells {
		full, rem = cells, 0
	}

	var b strings.Builder
	b.Grow(cells * 3)
	for range full {
		b.WriteRune('█')
	}
	used := full
	if rem > 0 && used < cells {
		b.WriteRune(barRunes[rem])
		used++
	}
	for ; used < cells; used++ {
		b.WriteRune('░')
	}
	return b.String()
}

// formatPower shows both scales when the rig has both. Icom's 14 0A is a
// relative index with no watt meaning and Kenwood's PC is watts; normalising
// that away would invent a number, so whichever the rig actually supplies is
// what appears.
func formatPower(p radio.Power) string {
	s := fmt.Sprintf("%.0f %%", p.Pct)
	if p.Watts != nil {
		s += fmt.Sprintf("  %g W", *p.Watts)
	}
	return s
}

// formatCW renders the Morse queue.
func formatCW(cw radio.CWStatus) string {
	what := "idle"
	if cw.Busy {
		what = "sending"
	}
	s := fmt.Sprintf("%s  queued %d  %d wpm", what, cw.Queued, cw.WPM)
	if cw.EstRemainingMS > 0 {
		s += fmt.Sprintf("  ~%.1f s", float64(cw.EstRemainingMS)/1000)
	}
	return s
}

// formatAge renders a duration at a precision that suits its size: tenths while
// a snapshot is fresh, because that is where the interesting range is, and
// whole units once it is old enough that the tenths are noise.
func formatAge(d time.Duration) string {
	switch {
	case d < 0:
		return "0.0 s"
	case d < 10*time.Second:
		return fmt.Sprintf("%.1f s", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%.0f s", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%d m %02d s", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%d h %02d m", int(d.Hours()), int(d.Minutes())%60)
	}
}
