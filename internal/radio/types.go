// Package radio defines the value types shared by every remoses component:
// transceiver state, sparse state patches, and capability descriptions.
//
// This is a leaf package. It must not import any other remoses package, so that
// backends, the session layer, and the API can all depend on it without cycles.
package radio

import (
	"fmt"
	"strings"
	"time"
)

// Mode is an operating mode. Data mode is deliberately NOT folded in here:
// Kenwood models it as an orthogonal setting (MD + DA), so State carries it
// separately.
type Mode uint8

const (
	ModeUnknown Mode = iota
	ModeLSB
	ModeUSB
	ModeCW
	ModeCWR
	ModeAM
	ModeFM
	ModeFSK  // RTTY
	ModeFSKR // RTTY reverse
	ModePSK
	ModePSKR

	// Digital and image modes carried by Icom's VHF/UHF and microwave sets.
	// They are ordinary operating modes on the wire, selected by the same
	// commands as SSB or CW, so they belong here rather than in a side channel.
	ModeDV  // D-STAR digital voice; IC-9700, IC-905
	ModeDD  // D-STAR digital data; IC-9700 (1200 MHz), IC-905
	ModeATV // analogue television; IC-905 only

	// Yaesu's C4FM. Three values rather than one because two radios put the
	// same distinction in two different places. The FTX-1 has DN and VW as
	// genuine mode codes. The FT-991A has one C4FM mode and chooses the
	// sub-mode with a persistent menu item (EX 090, AMS TX MODE) that is
	// orthogonal to the operating mode, much as Kenwood's DA is orthogonal to
	// MD — and which remoses does not read, because writing EX would alter the
	// operator's saved configuration and reading it alone buys nothing. So
	// ModeC4FM means "C4FM, sub-mode not expressed as a mode". Folding it into
	// DN would report a menu setting remoses has never seen, and encoding it
	// back would write a mode the radio does not have.
	ModeC4FM   // FT-991A
	ModeC4FMDN // FTX-1, digital narrow
	ModeC4FMVW // FTX-1, voice wide

	// The FT-857/FT-897 generation, whose mode table has two entries the rest
	// of this list cannot express.
	//
	// ModeWFM is wide FM, the 76-108 MHz broadcast band. It is receive-only and
	// cannot be selected over CAT — those radios' mode-set table has no code
	// for it — so it never appears in Caps.Modes. It exists because their
	// status read does report it, and calling it FM would misreport a 200 kHz
	// passband as a 15 kHz one.
	//
	// ModeDIG is Yaesu's DIG, and it is one mode here for the same reason
	// ModeC4FM is one mode on an FT-991A: which digital mode it actually is —
	// RTTY-L, RTTY-U, PSK31-L, PSK31-U, USER-L or USER-U — is menu item 038, a
	// persistent setting orthogonal to the mode, and one no CAT command reads.
	// So ModeDIG means "DIG, sub-mode not expressed as a mode". Mapping it to
	// FSK because RTTY-L is the factory default would report a radio sitting in
	// PSK31 as RTTY: a wrong answer rather than a missing one.
	ModeWFM
	ModeDIG
)

var modeNames = map[Mode]string{
	ModeUnknown: "UNKNOWN",
	ModeLSB:     "LSB",
	ModeUSB:     "USB",
	ModeCW:      "CW",
	ModeCWR:     "CW-R",
	ModeAM:      "AM",
	ModeFM:      "FM",
	ModeFSK:     "FSK",
	ModeFSKR:    "FSK-R",
	ModePSK:     "PSK",
	ModePSKR:    "PSK-R",
	ModeDV:      "DV",
	ModeDD:      "DD",
	ModeATV:     "ATV",
	ModeC4FM:    "C4FM",
	ModeC4FMDN:  "C4FM-DN",
	ModeC4FMVW:  "C4FM-VW",
	ModeWFM:     "WFM",
	ModeDIG:     "DIG",
}

func (m Mode) String() string {
	if s, ok := modeNames[m]; ok {
		return s
	}
	return fmt.Sprintf("Mode(%d)", uint8(m))
}

// ParseMode accepts the canonical API spelling, case-insensitively, and
// tolerates the common "CWR"/"RTTY" aliases.
//
// UNKNOWN parses, and must: String emits it for a radio that has not reported a
// mode yet, so refusing it here made Mode a type that could not decode its own
// output — which broke every client reading the state of a rig that had just
// connected, remoses-cli included.
//
// That it parses does not make it settable. The API rejects it on the way in
// because caps.modes never lists it, and every backend refuses it explicitly
// besides; those are the checks that enforce "never accepted as input", and
// they belong there rather than in a text decoder.
func ParseMode(s string) (Mode, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "UNKNOWN":
		return ModeUnknown, nil
	case "LSB":
		return ModeLSB, nil
	case "USB":
		return ModeUSB, nil
	case "CW":
		return ModeCW, nil
	case "CW-R", "CWR":
		return ModeCWR, nil
	case "AM":
		return ModeAM, nil
	case "FM":
		return ModeFM, nil
	case "FSK", "RTTY":
		return ModeFSK, nil
	case "FSK-R", "FSKR", "RTTY-R", "RTTYR":
		return ModeFSKR, nil
	case "PSK":
		return ModePSK, nil
	case "PSK-R", "PSKR":
		return ModePSKR, nil
	case "DV":
		return ModeDV, nil
	case "DD":
		return ModeDD, nil
	case "ATV":
		return ModeATV, nil
	case "C4FM":
		return ModeC4FM, nil
	case "C4FM-DN", "C4FMDN":
		return ModeC4FMDN, nil
	case "C4FM-VW", "C4FMVW":
		return ModeC4FMVW, nil
	case "WFM":
		return ModeWFM, nil
	case "DIG":
		return ModeDIG, nil
	}
	return ModeUnknown, fmt.Errorf("radio: unknown mode %q", s)
}

func (m Mode) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

func (m *Mode) UnmarshalText(b []byte) error {
	v, err := ParseMode(string(b))
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// IsCW reports whether the mode is one of the CW variants.
func (m Mode) IsCW() bool { return m == ModeCW || m == ModeCWR }

// VFO selects which VFO or receiver a command addresses.
type VFO uint8

const (
	VFOCurrent VFO = iota // whatever the rig is on now
	VFOA
	VFOB
	VFOMain // IC-7610 main receiver
	VFOSub  // IC-7610 sub receiver
)

var vfoNames = map[VFO]string{
	VFOCurrent: "current",
	VFOA:       "A",
	VFOB:       "B",
	VFOMain:    "main",
	VFOSub:     "sub",
}

func (v VFO) String() string {
	if s, ok := vfoNames[v]; ok {
		return s
	}
	return fmt.Sprintf("VFO(%d)", uint8(v))
}

func ParseVFO(s string) (VFO, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "current":
		return VFOCurrent, nil
	case "a":
		return VFOA, nil
	case "b":
		return VFOB, nil
	case "main":
		return VFOMain, nil
	case "sub":
		return VFOSub, nil
	}
	return VFOCurrent, fmt.Errorf("radio: unknown VFO %q", s)
}

func (v VFO) MarshalText() ([]byte, error) { return []byte(v.String()), nil }

func (v *VFO) UnmarshalText(b []byte) error {
	x, err := ParseVFO(string(b))
	if err != nil {
		return err
	}
	*v = x
	return nil
}

// Power reports transmit power in every form the backend can supply.
//
// The two target rigs disagree about units and normalising them away would lose
// real information: Kenwood PC is watts (005..100, 005..025 in AM), while Icom
// 14 0A is a relative 0000..0255 with no watt meaning. So all three are carried.
type Power struct {
	// Watts is the absolute power, nil when the rig has no watt-accurate scale.
	Watts *float64 `json:"watts"`
	// Pct is normalised 0..100 against the rig's maximum for the current mode.
	Pct float64 `json:"pct"`
	// Native is the value as it appears on the wire.
	Native int `json:"native"`
}

// PowerSet is a power change request. Exactly one field must be non-nil; the
// API rejects a request carrying both.
type PowerSet struct {
	Watts *float64 `json:"watts,omitempty"`
	Pct   *float64 `json:"pct,omitempty"`
}

// Validate reports whether exactly one of the two fields is set.
func (p PowerSet) Validate() error {
	switch {
	case p.Watts == nil && p.Pct == nil:
		return fmt.Errorf("radio: power requires either watts or pct")
	case p.Watts != nil && p.Pct != nil:
		return fmt.Errorf("radio: power accepts watts or pct, not both")
	}
	return nil
}

// Meter is an uncalibrated meter reading.
//
// Neither target rig reports dBm. Kenwood SM returns 0..30 meter "dots" (and
// reads the RF power meter while transmitting); Icom 15 02 returns 0..255. S is
// populated only where a per-model calibration table exists.
type Meter struct {
	Raw   int      `json:"raw"`
	Scale int      `json:"scale"`
	S     *float64 `json:"s,omitempty"`
}

// Fraction returns the reading normalised to 0..1, for drawing a bar.
func (m Meter) Fraction() float64 {
	if m.Scale <= 0 {
		return 0
	}
	f := float64(m.Raw) / float64(m.Scale)
	return min(max(f, 0), 1)
}

// BreakIn is the rig's CW break-in setting, which decides whether keying the
// transmitter is automatic.
//
// It matters far beyond being one more knob: on an Icom a CW message sent over
// CAT transmits only "if the [TRANSMIT] or an external TX switch is ON, or the
// Break-in function is ON" — the reference says so in as many words. With
// break-in off and nothing keying manually, command 17 is accepted, the queue
// drains, and nothing goes on the air. That is a success that means nothing,
// which is the failure this project works hardest to avoid, so remoses reads
// this, publishes it, and refuses to send Morse into it.
type BreakIn string

const (
	// BreakInUnknown is a radio that has not reported one, or one remoses
	// cannot ask. It is never treated as "off": refusing to send CW on a radio
	// whose break-in state is simply unknown would break every rig whose
	// reference this backend has not read.
	BreakInUnknown BreakIn = ""
	BreakInOff     BreakIn = "off"
	// BreakInSemi keys the transmitter and holds it for the delay; BreakInFull
	// is QSK, receiving between elements. Both transmit, which is all the CW
	// path needs to know.
	BreakInSemi BreakIn = "semi"
	BreakInFull BreakIn = "full"
)

// Transmits reports whether CW keyed from the computer will actually go out
// without somebody holding a switch down.
func (b BreakIn) Transmits() bool { return b == BreakInSemi || b == BreakInFull }

// CWStatus describes the CW sending queue.
type CWStatus struct {
	Busy           bool `json:"busy"`
	Queued         int  `json:"queued"`
	WPM            int  `json:"wpm"`
	EstRemainingMS int  `json:"est_remaining_ms"`
}

// VFOState is everything one VFO carries on a radio whose VFOs are
// independent.
//
// On an IC-7610 they genuinely are: each VFO has its own mode, data-mode flag,
// IF filter slot and passband, not just a frequency. Its command 26 sets mode,
// data mode and filter for one VFO in a single frame, which is the clearest
// statement the reference makes that these belong together.
//
// The zero value means "this radio does not expose that VFO separately", which
// is every radio but the IC-7610 today. Caps.VFOs is what a client should read
// to know; see Caps.PerVFOMode for the mode/filter half of it.
type VFOState struct {
	Frequency  uint64 `json:"frequency"`
	Mode       Mode   `json:"mode"`
	DataMode   bool   `json:"data_mode"`
	PassbandHz int    `json:"passband_hz"`
	FilterSlot int    `json:"filter_slot"`
}

// State is the full snapshot of a radio. It is copied by value and published
// through an atomic pointer, so it must stay free of reference types that a
// reader could mutate.
type State struct {
	// The operating VFO: the one the radio is receiving on, and — unless Split
	// is set — transmitting on. These fields mean the same thing on every
	// radio, including the single-VFO ones, so a client that does not care
	// about a second VFO can ignore everything below them.
	Frequency  uint64   `json:"frequency"`
	Mode       Mode     `json:"mode"`
	DataMode   bool     `json:"data_mode"`
	PassbandHz int      `json:"passband_hz"`
	FilterSlot int      `json:"filter_slot"`
	Power      Power    `json:"power"`
	PTT        bool     `json:"ptt"`
	SMeter     Meter    `json:"s_meter"`
	SWR        *Meter   `json:"swr,omitempty"`
	ALC        *Meter   `json:"alc,omitempty"`
	CW         CWStatus `json:"cw"`

	// VFO names which of the two the fields above describe, so a client can
	// tell whether it is looking at A or B without inferring it.
	//
	// It is not always something the operator can change, and the two radio
	// designs behind that are worth knowing apart. The classic one has a single
	// receiver and an A/B switch: exactly one VFO is live, the display shows
	// which, and split transmits on the other. The IC-7610 has no such switch —
	// its two VFOs are both real receivers, A is always the one it receives and
	// transmits on, and B joins in when DualWatch is set or takes the transmit
	// when Split is. So this reads A permanently there, and remoses offers no
	// VFO-select operation for it, because the radio has nothing to select.
	VFO VFO `json:"vfo"`
	// VFOA and VFOB are the per-VFO detail on a radio that exposes both. One of
	// them duplicates the operating fields above; that redundancy is deliberate,
	// because the common client wants "the frequency" and the split-aware one
	// wants "both frequencies", and making the first derive from the second
	// would tax every caller for a feature one radio has.
	VFOA VFOState `json:"vfo_a"`
	VFOB VFOState `json:"vfo_b"`

	// Split transmits on the other VFO. It is the one flag here that changes
	// where RF comes out, so it is carried for every radio that can report it
	// rather than hidden behind the dual-VFO block.
	Split bool `json:"split"`
	// DualWatch receives on both VFOs at once. Only then does SubSMeter mean
	// anything: with it off the second receiver is not running, and a meter
	// reading from it would be a stale number that looks live.
	DualWatch bool  `json:"dual_watch"`
	SubSMeter Meter `json:"sub_s_meter"`

	// BreakIn decides whether CW sent from here reaches the air at all. Empty
	// on a radio remoses cannot ask.
	BreakIn BreakIn `json:"break_in,omitempty"`

	Connected bool      `json:"connected"`
	UpdatedAt time.Time `json:"updated_at"`
	Seq       uint64    `json:"seq"`
}

// TXVFO reports which VFO the radio will transmit on: the other one when split
// is set, the operating one otherwise.
//
// It exists so that no caller has to re-derive the rule, because getting it
// wrong means telling an operator they are transmitting somewhere they are not.
//
// One rule covers both radio designs. Where the operator switches A and B it
// follows the switch; on an IC-7610, where VFO is always A, it simply answers B
// whenever split is on — which is the same statement, since B is where that
// radio's transmit goes.
func (s State) TXVFO() VFO {
	if !s.Split {
		return s.VFO
	}
	if s.VFO == VFOB {
		return VFOA
	}
	return VFOB
}

// Patch is a sparse state update. Backends decode a wire frame into a Patch;
// the session applies it and the WebSocket layer turns it into a delta message.
// A nil field means "this frame said nothing about that".
type Patch struct {
	Frequency  *uint64
	Mode       *Mode
	DataMode   *bool
	PassbandHz *int
	FilterSlot *int
	Power      *Power
	PTT        *bool
	SMeter     *Meter
	SWR        *Meter
	ALC        *Meter
	CWBusy     *bool
	Connected  *bool

	// The dual-VFO fields. VFOA and VFOB are whole-VFO replacements rather than
	// per-field pointers: the IC-7610's command 26 answers mode, data mode and
	// filter together and command 25 answers a frequency, so a decoder always
	// has a whole VFO's worth or none of it, and finer granularity would only
	// invite half-applied updates.
	VFO       *VFO
	VFOA      *VFOState
	VFOB      *VFOState
	Split     *bool
	DualWatch *bool
	SubSMeter *Meter
	BreakIn   *BreakIn
}

// Empty reports whether the patch carries no fields at all.
func (p Patch) Empty() bool {
	return p.Frequency == nil && p.Mode == nil && p.DataMode == nil &&
		p.PassbandHz == nil && p.FilterSlot == nil && p.Power == nil &&
		p.PTT == nil && p.SMeter == nil && p.SWR == nil && p.ALC == nil &&
		p.CWBusy == nil && p.Connected == nil &&
		p.VFO == nil && p.VFOA == nil && p.VFOB == nil &&
		p.Split == nil && p.DualWatch == nil && p.SubSMeter == nil &&
		p.BreakIn == nil
}

// Apply returns s updated with every field the patch sets. It does not touch
// UpdatedAt or Seq; the session owns those.
func (s State) Apply(p Patch) State {
	if p.Frequency != nil {
		s.Frequency = *p.Frequency
	}
	if p.Mode != nil {
		s.Mode = *p.Mode
	}
	if p.DataMode != nil {
		s.DataMode = *p.DataMode
	}
	if p.PassbandHz != nil {
		s.PassbandHz = *p.PassbandHz
	}
	if p.FilterSlot != nil {
		s.FilterSlot = *p.FilterSlot
	}
	if p.Power != nil {
		s.Power = *p.Power
	}
	if p.PTT != nil {
		s.PTT = *p.PTT
	}
	if p.SMeter != nil {
		s.SMeter = *p.SMeter
	}
	if p.SWR != nil {
		s.SWR = p.SWR
	}
	if p.ALC != nil {
		s.ALC = p.ALC
	}
	if p.CWBusy != nil {
		s.CW.Busy = *p.CWBusy
	}
	if p.Connected != nil {
		s.Connected = *p.Connected
	}

	if p.VFO != nil {
		s.VFO = *p.VFO
	}
	if p.VFOA != nil {
		s.VFOA = *p.VFOA
	}
	if p.VFOB != nil {
		s.VFOB = *p.VFOB
	}
	if p.Split != nil {
		s.Split = *p.Split
	}
	if p.DualWatch != nil {
		s.DualWatch = *p.DualWatch
	}
	if p.SubSMeter != nil {
		s.SubSMeter = *p.SubSMeter
	}
	if p.BreakIn != nil {
		s.BreakIn = *p.BreakIn
	}
	return s
}

// Diff returns a patch describing every field in which next differs from s.
// The WebSocket layer uses it to emit deltas instead of full snapshots.
func (s State) Diff(next State) Patch {
	var p Patch
	if s.Frequency != next.Frequency {
		p.Frequency = &next.Frequency
	}
	if s.Mode != next.Mode {
		p.Mode = &next.Mode
	}
	if s.DataMode != next.DataMode {
		p.DataMode = &next.DataMode
	}
	if s.PassbandHz != next.PassbandHz {
		p.PassbandHz = &next.PassbandHz
	}
	if s.FilterSlot != next.FilterSlot {
		p.FilterSlot = &next.FilterSlot
	}
	if s.Power != next.Power {
		p.Power = &next.Power
	}
	if s.PTT != next.PTT {
		p.PTT = &next.PTT
	}
	if s.SMeter != next.SMeter {
		p.SMeter = &next.SMeter
	}
	if s.Connected != next.Connected {
		p.Connected = &next.Connected
	}
	if s.CW.Busy != next.CW.Busy {
		p.CWBusy = &next.CW.Busy
	}

	if s.VFO != next.VFO {
		p.VFO = &next.VFO
	}
	if s.VFOA != next.VFOA {
		p.VFOA = &next.VFOA
	}
	if s.VFOB != next.VFOB {
		p.VFOB = &next.VFOB
	}
	if s.Split != next.Split {
		p.Split = &next.Split
	}
	if s.DualWatch != next.DualWatch {
		p.DualWatch = &next.DualWatch
	}
	// The sub meter moves constantly while dual watch is on, exactly as the
	// main one does, so it is diffed like a meter and coalesced by the
	// WebSocket layer's min_interval rather than suppressed here.
	if s.SubSMeter != next.SubSMeter {
		p.SubSMeter = &next.SubSMeter
	}
	if s.BreakIn != next.BreakIn {
		p.BreakIn = &next.BreakIn
	}
	return p
}

// CWMethod is how a radio sends Morse.
type CWMethod string

const (
	CWNone      CWMethod = "none"
	CWViaCAT    CWMethod = "cat"        // rig-side buffer: Icom 17, Kenwood KY
	CWViaSerial CWMethod = "serial_key" // locally generated, DTR/RTS keyed
)

// Caps describes what a particular radio can do. The API publishes this so
// clients can adapt rather than guessing from the model name.
type Caps struct {
	Modes []Mode `json:"modes"`
	VFOs  []VFO  `json:"vfos"`

	// PowerWattAccurate is true when the rig's power scale is real watts
	// (Kenwood PC) rather than a relative index (Icom 14 0A).
	PowerWattAccurate bool    `json:"power_watt_accurate"`
	MaxPowerW         float64 `json:"max_power_w,omitempty"`

	FilterWidth bool `json:"filter_width"`
	FilterSlots int  `json:"filter_slots"`
	SMeterScale int  `json:"s_meter_scale"`

	// SubReceiver is a second receiver that can be listened to at the same
	// time as the first — the IC-7610's dual watch. It is not "the radio has
	// two VFOs", which nearly every radio here does: it is "both can be
	// received at once", which is what makes State.SubSMeter mean anything.
	SubReceiver bool `json:"sub_receiver"`
	// Split reports that the radio can transmit on the VFO it is not receiving
	// on, and that remoses can read and set that. It is the capability that
	// changes where RF comes out, so a client should check it before offering
	// the control rather than inferring it from the VFO list.
	Split bool `json:"split"`
	// DualWatch is the receive-on-both control. Implies SubReceiver; kept
	// separate because a radio could in principle have the second receiver
	// wired to something remoses cannot switch.
	DualWatch bool `json:"dual_watch"`
	// PerVFOMode reports that each VFO carries its own mode, data-mode flag and
	// filter, rather than only its own frequency. True on the IC-7610, whose
	// command 26 addresses all three per VFO. Where it is false a client should
	// treat mode and filter as properties of the radio, and State.VFOA/VFOB
	// carry frequencies only.
	PerVFOMode bool `json:"per_vfo_mode"`

	// VFOAddressing says what State.VFOA and State.VFOB actually name, because
	// the two radios that have them disagree and a client that assumed would
	// mislabel one of them.
	//
	//	"named"     A and B are stable labels for two fixed tuning slots. The
	//	            IC-7610's main and sub bands: A is always the same one.
	//	"relative"  A is whichever VFO the operator has selected and B is the
	//	            other. The IC-9700's commands 25 and 26 take exactly that
	//	            selector, and nothing in its protocol reports which letter
	//	            is selected — so remoses does not claim one.
	//	""          The radio exposes a single VFO; VFOA and VFOB are unused.
	//
	// The split rule is the same either way: B is where transmit goes.
	VFOAddressing string `json:"vfo_addressing,omitempty"`

	// SubReceiverReadable distinguishes a second receiver remoses can report
	// from one that merely exists.
	//
	// The IC-9700 has a sub band that receives independently, and no command
	// that addresses it: reaching it means sending "select the sub band", which
	// moves the operator's own focus and fights whoever is holding the dial. So
	// SubReceiver is true there and this is false, and State.SubSMeter stays
	// empty. remoses does not switch bands behind an operator's back to fill in
	// a meter.
	SubReceiverReadable bool `json:"sub_receiver_readable"`

	// BreakInControl reports that remoses can read and set the CW break-in
	// setting. Worth publishing on its own because it gates whether CW sent
	// over CAT reaches the air at all: a client offering a CW box on a radio
	// with this true should show the break-in state next to it.
	BreakInControl bool `json:"break_in_control"`

	CWMethod  CWMethod `json:"cw_method"`
	CWCharset string   `json:"cw_charset,omitempty"`
	CWMinWPM  int      `json:"cw_min_wpm,omitempty"`
	CWMaxWPM  int      `json:"cw_max_wpm,omitempty"`
}

// SupportsMode reports whether m is in the capability list.
func (c Caps) SupportsMode(m Mode) bool {
	for _, x := range c.Modes {
		if x == m {
			return true
		}
	}
	return false
}
