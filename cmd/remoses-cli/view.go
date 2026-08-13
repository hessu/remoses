package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/oapi-codegen/nullable"

	"github.com/hessu/remoses/internal/client"
	"github.com/hessu/remoses/internal/wire"
)

// linkState is what this monitor's own connection to the daemon is doing. It is
// deliberately separate from the state's own `connected`: "the radio is
// unplugged" and "I cannot reach the daemon" look nothing alike to an operator,
// and collapsing them into one indicator would send someone to the wrong end of
// the link.
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
//
// The two it holds — wire.Radio and wire.State — are generated from
// api/openapi.yaml, so this display can only show fields the published contract
// declares. That is the arrangement working rather than a constraint to work
// around: a field remoses-cli draws is one a client written against the spec
// can draw too.
type view struct {
	radioID string
	desc    *wire.Radio

	st        wire.State
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

// setSnapshot records a REST fetch. age_ms and stale come with a REST read and
// with nothing else, which is why they are lifted out here rather than read off
// v.st wherever they are drawn.
func (v *view) setSnapshot(s *wire.State) {
	v.st = *s
	v.haveState = true
	v.stale = client.IsStale(s)
	v.ageBase, v.ageAt = client.Age(s), v.now()
}

// setState records a websocket snapshot. It arrives having just been read from
// the session's cache, so its age restarts at zero.
func (v *view) setState(st wire.State) {
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
func (v *view) applyCW(cw wire.CWStatus) {
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

// value dereferences an optional field, or answers its zero.
//
// Optional is how the spec says "this radio cannot report that", and the
// generated types spell it as a pointer. Flattening one to its zero is safe
// wherever the zero is not itself a reading: an absent tuner and an absent AGC
// setting are the empty string, an absent standby flag is false. It is NOT safe
// for the numbers — a preamp that reads 0 is switched off, which is a different
// statement from a radio with no preamplifier — so those keep their pointers
// and their own presence flags below.
func value[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// reading unwraps a field the spec declares nullable — the transmit meters, the
// calibrated S figure, the watt reading.
//
// Those carry three states on the wire and the generated type keeps all three:
// absent, an explicit null, and a value. The distinction is what the stream
// needs — a delta naming a meter as null is the radio saying the reading has
// gone away, where the same field absent means it did not change — and a
// display needs neither. There is nothing to draw either way, so both collapse
// to "no reading" here, once, rather than at every call site.
func reading[T any](n nullable.Nullable[T]) (T, bool) {
	v, err := n.Get()
	return v, err == nil
}

// fraction returns a meter reading normalised to 0..1, for drawing a bar.
func fraction(m wire.Meter) float64 {
	if m.Scale <= 0 {
		return 0
	}
	f := float64(m.Raw) / float64(m.Scale)
	return min(max(f, 0), 1)
}

// significant is the part of the state whose change is worth an output line.
//
// It exists so that both renderers make the same decision from the same value,
// and it is comparable on purpose: the pointers in wire.State would otherwise
// compare by address, which is not what "did anything change" means. The
// meters are excluded — they move continuously and are handled separately.
type significant struct {
	connected  bool
	standby    bool
	stale      bool
	connErr    string
	frequency  int64
	mode       wire.Mode
	dataMode   bool
	passbandHz int
	filterSlot int
	powerPct   float64
	powerW     float64
	havePowerW bool
	ptt        bool
	tuner      wire.Tuner
	cw         wire.CWStatus

	// The receive front end. Flattened out of the pointers State holds them in,
	// because this type is compared with == and pointers would compare by
	// address — and each carries its own presence flag, since "this radio has
	// no preamplifier" and "the preamplifier is off" are both worth drawing and
	// are not the same thing.
	havePreamp   bool
	preamp       int
	haveAtt      bool
	attDB        int
	haveRFGain   bool
	rfGain       float64
	agc          wire.AGC
	haveIPPlus   bool
	ipPlus       bool
	haveDigiSel  bool
	digiSel      bool
	haveDSShift  bool
	digiSelShift float64

	// The noise processing and the notches, flattened for the same reason.
	haveNB      bool
	nb          int
	haveNBLevel bool
	nbLevel     float64
	haveNR      bool
	nr          int
	haveNRLevel bool
	nrLevel     float64
	haveNotch   bool
	notch       bool
	haveNotchF  bool
	notchFreq   float64
	notchWidth  wire.NotchWidth
	haveAuto    bool
	autoNotch   bool
	haveAnt     bool
	antenna     int
	haveRXAnt   bool
	rxAntenna   bool
}

func (v *view) significant() significant {
	s := significant{
		connected:  v.st.Connected,
		standby:    value(v.st.Standby),
		stale:      v.stale,
		connErr:    v.connErr,
		frequency:  v.st.Frequency,
		mode:       v.st.Mode,
		dataMode:   v.st.DataMode,
		passbandHz: v.st.PassbandHz,
		filterSlot: v.st.FilterSlot,
		powerPct:   v.st.Power.Pct,
		ptt:        v.st.PTT,
		tuner:      value(v.st.Tuner),
		cw:         v.st.CW,
	}
	if w, ok := reading(v.st.Power.Watts); ok {
		s.powerW, s.havePowerW = w, true
	}
	s.agc = value(v.st.AGC)
	if p := v.st.Preamp; p != nil {
		s.preamp, s.havePreamp = *p, true
	}
	if a := v.st.AttenuatorDB; a != nil {
		s.attDB, s.haveAtt = *a, true
	}
	if g := v.st.RFGain; g != nil {
		s.rfGain, s.haveRFGain = *g, true
	}
	if p := v.st.IPPlus; p != nil {
		s.ipPlus, s.haveIPPlus = *p, true
	}
	if d := v.st.DigiSel; d != nil {
		s.digiSel, s.haveDigiSel = *d, true
	}
	if d := v.st.DigiSelShift; d != nil {
		s.digiSelShift, s.haveDSShift = *d, true
	}
	s.notchWidth = value(v.st.NotchWidth)
	if n := v.st.NoiseBlanker; n != nil {
		s.nb, s.haveNB = *n, true
	}
	if l := v.st.NBLevel; l != nil {
		s.nbLevel, s.haveNBLevel = *l, true
	}
	if n := v.st.NoiseReduction; n != nil {
		s.nr, s.haveNR = *n, true
	}
	if l := v.st.NRLevel; l != nil {
		s.nrLevel, s.haveNRLevel = *l, true
	}
	if n := v.st.Notch; n != nil {
		s.notch, s.haveNotch = *n, true
	}
	if f := v.st.NotchFreq; f != nil {
		s.notchFreq, s.haveNotchF = *f, true
	}
	if a := v.st.AutoNotch; a != nil {
		s.autoNotch, s.haveAuto = *a, true
	}
	if a := v.st.Antenna; a != nil {
		s.antenna, s.haveAnt = *a, true
	}
	if a := v.st.RXAntenna; a != nil {
		s.rxAntenna, s.haveRXAnt = *a, true
	}
	return s
}

// meters is the continuously-moving part: what a bar is drawn from.
//
// The transmit meters are here too, flattened rather than kept as the pointers
// State holds them in, so that this stays comparable — the point of the type is
// that == answers "did a meter move".
type meters struct {
	sRaw   int
	sScale int

	// Each transmit meter's presence is carried separately from its value: the
	// end of a transmission is a change worth a line, and a meter that has gone
	// away is not the same as one reading zero.
	havePower bool
	powerRaw  int
	haveSWR   bool
	swrRaw    int
	swrRatio  float64
	haveALC   bool
	alcRaw    int
}

func (v *view) meters() meters {
	m := meters{sRaw: v.st.SMeter.Raw, sScale: v.st.SMeter.Scale}
	if p, ok := reading(v.st.PowerMeter); ok {
		m.havePower, m.powerRaw = true, p.Raw
	}
	if s, ok := reading(v.st.SWR); ok {
		m.haveSWR, m.swrRaw = true, s.Raw
	}
	if r, ok := reading(v.st.SWRRatio); ok {
		m.swrRatio = r
	}
	if a, ok := reading(v.st.ALC); ok {
		m.haveALC, m.alcRaw = true, a.Raw
	}
	return m
}

// transmitting reports whether there is a transmit meter to show. It is the
// meters rather than PTT that decide, because a radio can be keyed and report
// none of them — the IC-706 family cannot even report the PTT.
func (v *view) haveTXMeters() bool {
	_, pwr := reading(v.st.PowerMeter)
	_, swr := reading(v.st.SWR)
	_, alc := reading(v.st.ALC)
	return pwr || swr || alc
}

// formatFreq renders hertz the way a radio's display does: megahertz, then
// kilohertz, then hertz, in groups of three. 14025300 becomes 14.025.300, which
// is readable at a glance in a way that a bare nine-digit integer is not — and
// glancing at the frequency is most of what this tool is for.
func formatFreq(hz int64) string {
	return fmt.Sprintf("%d.%03d.%03d", hz/1_000_000, (hz/1000)%1000, hz%1000)
}

// formatMode spells the mode and the orthogonal data flag together, because
// that is how an operator reads them, while keeping them separate values
// underneath as the rigs do.
func formatMode(st wire.State) string {
	m := string(st.Mode)
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
func formatSUnit(m wire.Meter) string {
	s, ok := reading(m.S)
	if !ok {
		return ""
	}
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
// hasPowerReading, hasFilterWidth and hasFilterSlots report whether the radio
// has the command behind a field, so the display can leave out what it cannot
// know rather than drawing a zero.
//
// A zero in these three is not a small reading, it is the absence of one. An
// FT-857 has no CAT command for transmit power or for IF bandwidth in either
// direction — its optional YF-122 filters are chosen with the front-panel keys
// — so "power  0 %" beside a radio putting out ten watts reads as a fault, and
// "passband  0 Hz" as a filter closed to nothing. This is the rule the transmit
// meters already follow, applied to the three fields that predate it.
//
// Unknown counts as present. The descriptor is fetched before the first state
// and refreshed alongside it, so the only window where it is missing is before
// anything is on screen at all; blanking on a missing descriptor would make
// real readings flicker on a reconnect.
func (v *view) hasPowerReading() bool {
	return v.desc == nil || v.desc.Caps.PowerControl
}

func (v *view) hasFilterWidth() bool {
	return v.desc == nil || v.desc.Caps.FilterWidth
}

func (v *view) hasFilterSlots() bool {
	return v.desc == nil || v.desc.Caps.FilterSlots > 0
}

func formatPower(p wire.Power) string {
	s := fmt.Sprintf("%.0f %%", p.Pct)
	if w, ok := reading(p.Watts); ok {
		s += fmt.Sprintf("  %g W", w)
	}
	return s
}

// formatMeterValue renders the number beside a transmit meter bar: the raw
// reading against its own scale, plus the percentage that the bar is drawn
// from.
//
// Both, because neither is enough on its own. The percentage is what an
// operator reads at a glance, and the raw pair is what makes a bug reportable —
// the scales differ per meter and per model, and "143/255" beside "56%" is what
// tells somebody the scale is the one they expected.
func formatMeterValue(m wire.Meter) string {
	return fmt.Sprintf("%d/%d  %.0f %%", m.Raw, m.Scale, fraction(m)*100)
}

// formatSWR renders the SWR reading, preferring the ratio where the radio's
// documentation gave one.
//
// The single-bit case is spelled out rather than drawn as a number: an FT-857
// reports a high-SWR alarm and nothing else, and "1/1" would look like a
// reading rather than the warning light it is.
func formatSWR(m wire.Meter, ratio nullable.Nullable[float64]) string {
	if m.Scale == 1 {
		if m.Raw > 0 {
			return "HIGH"
		}
		return "ok"
	}
	if r, ok := reading(ratio); ok {
		return fmt.Sprintf("%.1f:1", r)
	}
	return formatMeterValue(m)
}

// formatTuner renders the antenna tuner, spelling out a running cycle rather
// than leaving "tuning" to be read as a settled state.
//
// A cycle transmits, so it is worth saying so on a display whose whole purpose
// is to tell an operator what a radio they cannot see is doing.
func formatTuner(t wire.Tuner) string {
	if t == wire.TunerTuning {
		return ">> TUNING <<"
	}
	return string(t)
}

// frontEndLine renders the receive front end as one row, or "" on a radio that
// reports none of it.
//
// One row rather than six, because these are read together and take one glance:
// what the signal meets on its way in, left to right in that order. Each part
// appears only where the radio reports it, so an FT-891 shows a preamp and a
// pad and no IP+, and an IC-706 shows two of the six.
//
// The preamp is spelled "off" rather than "0" and the attenuator "0 dB" rather
// than "off", because that is how the front panels label them: a preamplifier
// is in or out, an attenuator has a depth.
func frontEndLine(st wire.State) string {
	var parts []string
	if p := st.Preamp; p != nil {
		if *p == 0 {
			parts = append(parts, "pre off")
		} else {
			parts = append(parts, fmt.Sprintf("pre %d", *p))
		}
	}
	if a := st.AttenuatorDB; a != nil {
		parts = append(parts, fmt.Sprintf("att %d dB", *a))
	}
	if g := st.RFGain; g != nil {
		parts = append(parts, fmt.Sprintf("rf %.0f%%", *g))
	}
	if a := st.AGC; a != nil {
		parts = append(parts, "agc "+string(*a))
	}
	// The two Icom extras, named only when they are on: a client showing
	// "ip+ off" on every radio that has one would spend a column on a control
	// nobody has touched.
	if p := st.IPPlus; p != nil && *p {
		parts = append(parts, "ip+")
	}
	if d := st.DigiSel; d != nil && *d {
		s := "digi-sel"
		if v := st.DigiSelShift; v != nil {
			s += fmt.Sprintf(" %.0f%%", *v)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "   ")
}

// noiseLine renders the noise processing and the notches as one row, or "" on
// a radio that reports none of it.
//
// Each part appears only where the radio reports it, and the levels ride with
// their switch rather than getting a column of their own: "nb 1 40%" is one
// control an operator thinks of as one thing.
func noiseLine(st wire.State) string {
	var parts []string
	if n := st.NoiseBlanker; n != nil {
		parts = append(parts, withLevel("nb", *n, st.NBLevel))
	}
	if n := st.NoiseReduction; n != nil {
		parts = append(parts, withLevel("nr", *n, st.NRLevel))
	}
	if n := st.Notch; n != nil && *n {
		s := "notch"
		if f := st.NotchFreq; f != nil {
			s += fmt.Sprintf(" %.0f%%", *f)
		}
		if w := st.NotchWidth; w != nil {
			s += " " + string(*w)
		}
		parts = append(parts, s)
	}
	if a := st.AutoNotch; a != nil && *a {
		parts = append(parts, "auto-notch")
	}
	if a := st.Antenna; a != nil {
		s := fmt.Sprintf("ant %d", *a)
		if rx := st.RXAntenna; rx != nil && *rx {
			s += "+rx"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "   ")
}

// withLevel renders a noise circuit and its level: "nb off" when it is out,
// "nb 1 40%" when it is in and the radio reports a level.
func withLevel(name string, sel int, level *float64) string {
	if sel == 0 {
		return name + " off"
	}
	s := fmt.Sprintf("%s %d", name, sel)
	if level != nil {
		s += fmt.Sprintf(" %.0f%%", *level)
	}
	return s
}

// formatCW renders the Morse queue.
func formatCW(cw wire.CWStatus) string {
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
