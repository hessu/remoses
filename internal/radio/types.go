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
	// BreakInOn is break-in enabled on a radio that does not say which kind.
	//
	// Not every rig distinguishes them over CAT: an IC-910H's command is
	// "0=OFF, 1=ON" where the rest of its family has three values. Reporting
	// that as semi or full would be inventing a distinction the radio declined
	// to make, and the difference is audible — full is QSK and clocks the
	// relays between elements. So it gets its own value, and a client that
	// wants to show "semi" or "full" can tell when it may not.
	//
	// Setting semi or full on such a radio is not an error: both mean "on"
	// there, both are sent as on, and the state reads back "on".
	BreakInOn BreakIn = "on"
)

// Transmits reports whether CW keyed from the computer will actually go out
// without somebody holding a switch down.
//
// Which is the whole reason BreakInOn can exist without disturbing anything:
// the CW path only ever asks this question, and "on" answers it.
func (b BreakIn) Transmits() bool {
	return b == BreakInSemi || b == BreakInFull || b == BreakInOn
}

// Band names one of a two-receiver radio's two receivers.
//
// "Band" rather than "receiver" because that is what the radios call them —
// Icom's MAIN and SUB bands — and an operator reading a client will recognise
// the word off the front panel.
type Band string

const (
	BandMain Band = "main"
	BandSub  Band = "sub"
)

// Tuner is the state of a radio's internal antenna tuner.
type Tuner string

const (
	// TunerUnknown is a radio that has not reported one, or one remoses cannot
	// ask. Absent from the published state rather than shown as "off", which
	// would be a claim about hardware that may not exist.
	TunerUnknown Tuner = ""
	// TunerOff is the tuner bypassed — "THRU" in Kenwood's vocabulary.
	TunerOff Tuner = "off"
	// TunerOn is the tuner in line, using whatever match it last found.
	TunerOn Tuner = "on"
	// TunerTuning is a tuning cycle running right now. The radio is keying its
	// own transmitter to find a match, which is why this is a state a client
	// can observe rather than something that happens invisibly inside a set.
	TunerTuning Tuner = "tuning"
)

// Valid reports whether t is a state a client may ask for.
//
// "tuning" is deliberately excluded: a tuning cycle is started with the
// separate TunerTune action, so that echoing back a state just read can never
// key a transmitter. See Patch.TunerTune.
func (t Tuner) Valid() bool { return t == TunerOff || t == TunerOn }

// AGC is the receiver's automatic gain control setting.
//
// The named speeds are the ones every manufacturer agrees on, and the radios
// disagree about which of them exist: a TS-590 has slow and fast and no middle,
// a TS-890 and a TS-990 have all three, a TS-480 has off, fast and slow under a
// different command entirely. Caps.AGCSettings lists what a given radio will
// accept, and a client should offer those rather than assume the full set.
type AGC string

const (
	// AGCUnknown is a radio that has not reported one, or one remoses cannot
	// ask.
	AGCUnknown AGC = ""
	// AGCOff is the AGC switched out. Not every radio can: Icom's command has
	// no off at all, because on those sets AGC OFF is a per-mode menu item
	// rather than one of the three speeds.
	AGCOff  AGC = "off"
	AGCFast AGC = "fast"
	AGCMid  AGC = "mid"
	AGCSlow AGC = "slow"
	// AGCAuto lets the radio pick a speed from the operating mode. Yaesu only.
	//
	// It is settable but never read back, and that is not an inconsistency to
	// paper over: the radio answers with the speed auto CHOSE, as one of the
	// three values below. So a client that sets "auto" and reads "auto-mid" has
	// not failed to set anything — it is being told what auto currently means.
	AGCAuto AGC = "auto"
	// The auto-resolved speeds. Read-only: sending one is refused, because the
	// radio has no way to be told "be automatic, and also be fast".
	AGCAutoFast AGC = "auto-fast"
	AGCAutoMid  AGC = "auto-mid"
	AGCAutoSlow AGC = "auto-slow"
)

// Settable reports whether a is a value a client may ask for.
//
// The auto-resolved readings are excluded, so echoing back a state just read
// cannot fail on a radio that reported one.
func (a AGC) Settable() bool {
	switch a {
	case AGCOff, AGCFast, AGCMid, AGCSlow, AGCAuto:
		return true
	}
	return false
}

// Auto reports whether the radio is choosing the speed itself, whichever speed
// it has currently chosen.
func (a AGC) Auto() bool {
	return a == AGCAuto || a == AGCAutoFast || a == AGCAutoMid || a == AGCAutoSlow
}

// NotchWidth is how wide the manual notch bites.
type NotchWidth string

const (
	NotchWidthUnknown NotchWidth = ""
	NotchWidthWide    NotchWidth = "wide"
	NotchWidthMid     NotchWidth = "mid"
	NotchWidthNarrow  NotchWidth = "narrow"
)

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
	CW         CWStatus `json:"cw"`

	// The transmit meters. All three are absent in receive rather than zero,
	// because a transmitter that is not keyed has no forward power, no SWR and
	// no ALC action — and a client drawing 0 on a bar cannot tell "not
	// transmitting" from "transmitting into a dead load". They are read only
	// while PTT is up and cleared when it drops, so their presence is itself
	// the statement that the radio is transmitting and these are live.
	//
	// Each is a raw deflection with the full-scale value that goes with it, in
	// whatever units the radio's own meter uses: Icom counts 0-255, Kenwood
	// counts meter dots. Compare Raw against Scale rather than assuming either.
	PowerMeter *Meter `json:"power_meter,omitempty"`
	SWR        *Meter `json:"swr,omitempty"`
	ALC        *Meter `json:"alc,omitempty"`
	// Standby is a radio that is reachable but switched off.
	//
	// It is a third thing, and neither of the other two describes it. The link
	// is fine — the port is open and the radio is answering — so `connected`
	// false would send somebody to check a cable that is perfectly seated. But
	// nothing else in this struct is live: an IC-7610 in standby answers NG to
	// every command, frequency and mode included, so the values here are
	// whatever was last true before it was switched off.
	//
	// A client should show it as its own condition and offer a power-on, not a
	// reconnect.
	Standby bool `json:"standby,omitempty"`

	// The receive front end: what the signal meets between the antenna socket
	// and the first mixer, plus the gain controls behind it.
	//
	// All of them are pointers because zero is a real setting for every one —
	// preamp off, 0 dB of attenuation, RF gain backed all the way off — and a
	// client cannot be left to guess whether a 0 means "off" or "this radio
	// never said". Absent means remoses has no reading, either because the
	// radio has no such control or because its CAT set cannot report one.
	//
	// Preamp is 0 for off and 1..Caps.PreampLevels for the preamplifiers, in
	// the order the radio numbers them. On a set with two, 1 is the quieter and
	// 2 the higher gain; Yaesu calls off "IPO" and 1 and 2 "AMP 1" and "AMP 2",
	// which is the same control under another name.
	Preamp *int `json:"preamp,omitempty"`
	// AttenuatorDB is the attenuation actually in line, in dB, 0 for none.
	//
	// It is a dB figure rather than a step index because the steps are not the
	// same set twice: an IC-7610 has 3 dB steps to 45, an IC-7600 has 6, 12 and
	// 18, an FT-891 has one switch and a TS-590 has another. Caps.AttenuatorDB
	// lists what this radio will accept.
	AttenuatorDB *int `json:"attenuator_db,omitempty"`
	// RFGain is the receiver's RF gain control, 0-100%.
	//
	// Percent rather than the radio's own count because the counts differ — a
	// TS-480 reads 0-100 and a TS-590 reads 0-255 for the same knob — and a
	// number that means a different thing on two radios of the same make is a
	// number a client cannot draw.
	RFGain *float64 `json:"rf_gain,omitempty"`
	// AGC is the automatic gain control speed.
	AGC AGC `json:"agc,omitempty"`
	// IPPlus is Icom's IP+ : an intermodulation-rejection mode that trades a
	// little sensitivity for a better third-order intercept, by backing the
	// direct-sampling ADC off its clipping point.
	IPPlus *bool `json:"ip_plus,omitempty"`
	// DigiSel is Icom's DIGI-SEL preselector, a narrow tracking filter ahead of
	// the ADC, and DigiSelShift trims where it sits, 0-100%.
	//
	// The shift is meaningless while the preselector is switched out, but it is
	// still reported: the radio remembers it, and a client redrawing the
	// control wants the position it will return to.
	DigiSel      *bool    `json:"digi_sel,omitempty"`
	DigiSelShift *float64 `json:"digi_sel_shift,omitempty"`

	// The noise processing, and both of these are COUNTS rather than switches
	// because the radios really do have more than one circuit. A TS-590's NB
	// takes NB1 and NB2, its NR takes NR1 and NR2, and they are different
	// algorithms rather than two strengths of one: NR1 is a noise reducer and
	// NR2 is SPAC, whose level parameter is a following speed in milliseconds.
	// 0 is off, 1..Caps.NoiseBlankerLevels / NoiseReductionLevels select one.
	//
	// The levels are percentages for the same reason RFGain is: the counts
	// underneath are 0-255 on Icom, 1-10 on a Kenwood blanker, 1-15 on a Yaesu
	// noise reducer, and no client can draw a number that means three things.
	NoiseBlanker   *int     `json:"noise_blanker,omitempty"`
	NBLevel        *float64 `json:"nb_level,omitempty"`
	NoiseReduction *int     `json:"noise_reduction,omitempty"`
	NRLevel        *float64 `json:"nr_level,omitempty"`

	// The notch filters. Two of them, and they are separate fields because on
	// two of the three families they are separate controls that can both be on.
	//
	// Notch is the manual one, which the operator parks on a carrier;
	// NotchFreq is where it sits, 0-100% of the radio's own range, because the
	// underlying counts are 0-255 on Icom, 0-127 on Kenwood and an actual
	// frequency on Yaesu. NotchWidth is how wide it bites, where the radio can
	// say. AutoNotch is the automatic one, which hunts for tones by itself.
	//
	// A Kenwood cannot have both: its NT is one selector with off, auto and
	// manual, so asking for both is refused there and accepted elsewhere.
	Notch      *bool      `json:"notch,omitempty"`
	NotchFreq  *float64   `json:"notch_freq,omitempty"`
	NotchWidth NotchWidth `json:"notch_width,omitempty"`
	AutoNotch  *bool      `json:"auto_notch,omitempty"`

	// Antenna is which socket is selected, counting from 1, and RXAntenna is
	// the separate receive-only input where a radio has one.
	//
	// Absent on every Icom here, and that is not an omission: those radios have
	// no live antenna selector on the bus. The antenna is a per-band MEMORY —
	// an IC-7610 keeps one entry per band range in its Set menu — so switching
	// it would mean writing a stored setting rather than throwing a switch, and
	// remoses does not write band memories.
	Antenna   *int  `json:"antenna,omitempty"`
	RXAntenna *bool `json:"rx_antenna,omitempty"`

	// Tuner is the internal antenna tuner: off, on, or a tuning cycle in
	// progress. Absent on a radio that has none or that remoses cannot ask.
	Tuner Tuner `json:"tuner,omitempty"`
	// SWRRatio is the standing-wave ratio as a number — 1.5, 2.0, 3.0 — where
	// the radio's own documentation calibrates its meter well enough to say.
	//
	// It is absent rather than computed where it is not documented, which is
	// most radios: a raw deflection means nothing without the manufacturer's
	// scale, and a ratio invented from a linear guess would be a number remoses
	// made up about somebody's antenna. Icom prints four calibration points and
	// gets a figure; Kenwood and Yaesu print none and get only the bar.
	SWRRatio *float64 `json:"swr_ratio,omitempty"`

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

	// SelectedBand says which of a two-receiver radio's receivers the operator
	// has selected, and it exists because the answer changes what the rest of
	// this struct is describing.
	//
	// On an IC-9700 the operating fields — Frequency, Mode — follow the
	// selection, while VFOA and VFOB are the *main* receiver's pair whichever
	// is selected, because that radio's per-VFO commands are documented "(Only
	// MAIN band)". So with "sub" here, the two halves describe two different
	// receivers, and nothing else in the state says so. Touching the sub
	// frequency field on the radio's own screen is enough to get there.
	//
	// The sub receiver has its own VFO A and B, usable and exchangeable at the
	// radio; remoses cannot reach them, which is the same limitation
	// Caps.SubReceiverReadable reports. Exchanging the two receivers swaps
	// their VFO pairs along with everything else.
	//
	// Empty where the radio has one receiver, or has two and cannot be asked.
	SelectedBand Band `json:"selected_band,omitempty"`

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
	PowerMeter *Meter
	SWR        *Meter
	ALC        *Meter
	SWRRatio   *float64
	Tuner      *Tuner
	Standby    *bool
	CWBusy     *bool
	Connected  *bool

	// The receive front end. AGC is a string enum whose zero value already
	// means "unknown", so it needs no pointer; the rest carry a real zero.
	Preamp       *int
	AttenuatorDB *int
	RFGain       *float64
	AGC          *AGC
	IPPlus       *bool
	DigiSel      *bool
	DigiSelShift *float64

	// The noise processing and the notches.
	NoiseBlanker   *int
	NBLevel        *float64
	NoiseReduction *int
	NRLevel        *float64
	Notch          *bool
	NotchFreq      *float64
	NotchWidth     *NotchWidth
	AutoNotch      *bool
	Antenna        *int
	RXAntenna      *bool

	// The dual-VFO fields. VFOA and VFOB are whole-VFO replacements rather than
	// per-field pointers: the IC-7610's command 26 answers mode, data mode and
	// filter together and command 25 answers a frequency, so a decoder always
	// has a whole VFO's worth or none of it, and finer granularity would only
	// invite half-applied updates.
	VFO          *VFO
	VFOA         *VFOState
	VFOB         *VFOState
	Split        *bool
	DualWatch    *bool
	SubSMeter    *Meter
	SelectedBand *Band
	BreakIn      *BreakIn
}

// Empty reports whether the patch carries no fields at all.
func (p Patch) Empty() bool {
	return p.Frequency == nil && p.Mode == nil && p.DataMode == nil &&
		p.PassbandHz == nil && p.FilterSlot == nil && p.Power == nil &&
		p.PTT == nil && p.SMeter == nil && p.PowerMeter == nil &&
		p.SWR == nil && p.ALC == nil && p.SWRRatio == nil &&
		p.Tuner == nil && p.Standby == nil &&
		p.CWBusy == nil && p.Connected == nil &&
		p.Preamp == nil && p.AttenuatorDB == nil && p.RFGain == nil &&
		p.AGC == nil && p.IPPlus == nil && p.DigiSel == nil &&
		p.DigiSelShift == nil &&
		p.NoiseBlanker == nil && p.NBLevel == nil &&
		p.NoiseReduction == nil && p.NRLevel == nil &&
		p.Notch == nil && p.NotchFreq == nil && p.NotchWidth == nil &&
		p.AutoNotch == nil && p.Antenna == nil && p.RXAntenna == nil &&
		p.VFO == nil && p.VFOA == nil && p.VFOB == nil &&
		p.Split == nil && p.DualWatch == nil && p.SubSMeter == nil &&
		p.SelectedBand == nil && p.BreakIn == nil
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
	if p.PowerMeter != nil {
		s.PowerMeter = p.PowerMeter
	}
	if p.SWRRatio != nil {
		s.SWRRatio = p.SWRRatio
	}
	if p.SWR != nil {
		s.SWR = p.SWR
	}
	if p.ALC != nil {
		s.ALC = p.ALC
	}
	if p.Tuner != nil {
		s.Tuner = *p.Tuner
	}
	if p.Standby != nil {
		s.Standby = *p.Standby
	}
	// The front end. Each is copied by value into a fresh pointer rather than
	// aliased, so that a state handed to two readers cannot be changed under
	// one of them by whoever still holds the patch.
	if p.Preamp != nil {
		v := *p.Preamp
		s.Preamp = &v
	}
	if p.AttenuatorDB != nil {
		v := *p.AttenuatorDB
		s.AttenuatorDB = &v
	}
	if p.RFGain != nil {
		v := *p.RFGain
		s.RFGain = &v
	}
	if p.AGC != nil {
		s.AGC = *p.AGC
	}
	if p.IPPlus != nil {
		v := *p.IPPlus
		s.IPPlus = &v
	}
	if p.DigiSel != nil {
		v := *p.DigiSel
		s.DigiSel = &v
	}
	if p.DigiSelShift != nil {
		v := *p.DigiSelShift
		s.DigiSelShift = &v
	}
	// The noise processing and the notches, copied by value for the same
	// reason the front end is.
	if p.NoiseBlanker != nil {
		v := *p.NoiseBlanker
		s.NoiseBlanker = &v
	}
	if p.NBLevel != nil {
		v := *p.NBLevel
		s.NBLevel = &v
	}
	if p.NoiseReduction != nil {
		v := *p.NoiseReduction
		s.NoiseReduction = &v
	}
	if p.NRLevel != nil {
		v := *p.NRLevel
		s.NRLevel = &v
	}
	if p.Notch != nil {
		v := *p.Notch
		s.Notch = &v
	}
	if p.NotchFreq != nil {
		v := *p.NotchFreq
		s.NotchFreq = &v
	}
	if p.NotchWidth != nil {
		s.NotchWidth = *p.NotchWidth
	}
	if p.AutoNotch != nil {
		v := *p.AutoNotch
		s.AutoNotch = &v
	}
	if p.Antenna != nil {
		v := *p.Antenna
		s.Antenna = &v
	}
	if p.RXAntenna != nil {
		v := *p.RXAntenna
		s.RXAntenna = &v
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
	if p.SelectedBand != nil {
		s.SelectedBand = *p.SelectedBand
	}
	if p.BreakIn != nil {
		s.BreakIn = *p.BreakIn
	}

	// Dropping out of transmit clears the transmit meters, so that what a
	// client sees is either a live reading or nothing at all. Leaving the last
	// values behind would be worse than useless: a 1:3 SWR frozen on the
	// display after the operator stopped transmitting reads as a fault that is
	// still happening, and the last power reading of a transmission is usually
	// mid-decay rather than representative.
	//
	// It is done here rather than in each backend because it is a property of
	// the state, not of any protocol: every radio that reports these reports
	// them only while keyed.
	//
	// A tuning cycle is deliberately NOT special-cased here, though it does key
	// the transmitter. The two radios tested disagree about what they report
	// during one, and following the rig's own PTT is what gets both right:
	//
	//   - A TS-590S reports PTT true through the cycle and returns real
	//     readings, the SWR visibly falling as the tuner finds its match.
	//   - An IC-7610 reports PTT false throughout and answers zero to all three
	//     meters, its bursts being shorter than a poll interval in any case.
	//
	// Treating "tuning" as transmitting was tried and reverted: on the IC-7610
	// it published a zero SWR as a confident 1.0:1 — the best possible match —
	// at the exact moment the tuner was failing to find one.
	if !s.PTT {
		s.PowerMeter, s.SWR, s.ALC, s.SWRRatio = nil, nil, nil, nil
	}
	return s
}

// Equal compares two meter readings by value.
//
// It exists because == does not, and the difference is not academic: Meter
// carries an optional S, and a backend fills that in from a fresh allocation on
// every poll — rigctld computes an S-unit figure per read. Comparing the
// structs therefore compares the POINTER, so two identical readings half a
// second apart answer "different", and Diff publishes a delta for a meter that
// has not moved. A radio on a quiet band did exactly that, twice a second, to
// every connected client.
func (m Meter) Equal(other Meter) bool {
	return m.Raw == other.Raw && m.Scale == other.Scale && sameRatio(m.S, other.S)
}

// Equal compares two power readings by value, for the same reason: Watts is a
// pointer, and Kenwood and Yaesu allocate one per read.
func (p Power) Equal(other Power) bool {
	return p.Pct == other.Pct && p.Native == other.Native &&
		sameRatio(p.Watts, other.Watts)
}

// sameMeter compares two optional meter readings, nil included.
func sameMeter(a, b *Meter) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// sameRatio compares two optional floats by value, nil included. Used for the
// derived SWR figure and for the optional halves of Meter and Power.
func sameRatio(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// samePtr is sameMeter for any comparable value: the front-end controls are all
// optional, and for every one of them "went away" is as much a change as a new
// reading.
func samePtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
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
	if !s.Power.Equal(next.Power) {
		p.Power = &next.Power
	}
	if s.PTT != next.PTT {
		p.PTT = &next.PTT
	}
	if !s.SMeter.Equal(next.SMeter) {
		p.SMeter = &next.SMeter
	}
	// The transmit meters are pointers, so a delta has to compare what they
	// point at, and "became nil" — the transmission ended — is a change a
	// client needs as much as a new reading.
	if !sameMeter(s.PowerMeter, next.PowerMeter) {
		p.PowerMeter = next.PowerMeter
	}
	if !sameMeter(s.SWR, next.SWR) {
		p.SWR = next.SWR
	}
	if !sameMeter(s.ALC, next.ALC) {
		p.ALC = next.ALC
	}
	if !sameRatio(s.SWRRatio, next.SWRRatio) {
		p.SWRRatio = next.SWRRatio
	}
	if s.Tuner != next.Tuner {
		p.Tuner = &next.Tuner
	}
	if s.Standby != next.Standby {
		p.Standby = &next.Standby
	}
	if !samePtr(s.Preamp, next.Preamp) {
		p.Preamp = next.Preamp
	}
	if !samePtr(s.AttenuatorDB, next.AttenuatorDB) {
		p.AttenuatorDB = next.AttenuatorDB
	}
	if !samePtr(s.RFGain, next.RFGain) {
		p.RFGain = next.RFGain
	}
	if s.AGC != next.AGC {
		p.AGC = &next.AGC
	}
	if !samePtr(s.IPPlus, next.IPPlus) {
		p.IPPlus = next.IPPlus
	}
	if !samePtr(s.DigiSel, next.DigiSel) {
		p.DigiSel = next.DigiSel
	}
	if !samePtr(s.DigiSelShift, next.DigiSelShift) {
		p.DigiSelShift = next.DigiSelShift
	}
	if !samePtr(s.NoiseBlanker, next.NoiseBlanker) {
		p.NoiseBlanker = next.NoiseBlanker
	}
	if !samePtr(s.NBLevel, next.NBLevel) {
		p.NBLevel = next.NBLevel
	}
	if !samePtr(s.NoiseReduction, next.NoiseReduction) {
		p.NoiseReduction = next.NoiseReduction
	}
	if !samePtr(s.NRLevel, next.NRLevel) {
		p.NRLevel = next.NRLevel
	}
	if !samePtr(s.Notch, next.Notch) {
		p.Notch = next.Notch
	}
	if !samePtr(s.NotchFreq, next.NotchFreq) {
		p.NotchFreq = next.NotchFreq
	}
	if s.NotchWidth != next.NotchWidth {
		p.NotchWidth = &next.NotchWidth
	}
	if !samePtr(s.AutoNotch, next.AutoNotch) {
		p.AutoNotch = next.AutoNotch
	}
	if !samePtr(s.Antenna, next.Antenna) {
		p.Antenna = next.Antenna
	}
	if !samePtr(s.RXAntenna, next.RXAntenna) {
		p.RXAntenna = next.RXAntenna
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
	if !s.SubSMeter.Equal(next.SubSMeter) {
		p.SubSMeter = &next.SubSMeter
	}
	// Worth a delta of its own: it changes what Frequency and Mode are talking
	// about, so a client that missed it would misattribute every field above.
	if s.SelectedBand != next.SelectedBand {
		p.SelectedBand = &next.SelectedBand
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

	// PTTControl is true when remoses can key and unkey the transmitter over
	// the control link.
	//
	// Not every radio offers it, and the ones that do not are not exotic: the
	// IC-706 family has no CI-V command for the transmitter at all, so PTT there
	// is a control line or a footswitch and nothing else. Where this is false,
	// `ptt` in a state patch is refused and `state.ptt` never becomes true from
	// polling, because nothing reports it either.
	PTTControl bool `json:"ptt_control"`
	// PowerControl is true when remoses can read and set RF output power.
	// False on a radio whose command set has no power level — again the IC-706
	// family, where output is a front-panel control.
	PowerControl bool `json:"power_control"`
	// PowerWattAccurate is true when the rig's power scale is real watts
	// (Kenwood PC) rather than a relative index (Icom 14 0A).
	PowerWattAccurate bool    `json:"power_watt_accurate"`
	MaxPowerW         float64 `json:"max_power_w,omitempty"`

	FilterWidth bool `json:"filter_width"`
	FilterSlots int  `json:"filter_slots"`
	// SMeterScale is the full-scale meter reading, or 0 on a radio with no
	// readable signal meter at all.
	SMeterScale int `json:"s_meter_scale"`

	// Which transmit meters this radio can report. They are separate flags
	// because radios really do have some and not others: a Kenwood TS-590 reads
	// SWR and ALC on one command and its power meter on another, an FT-857
	// reports high-SWR as a yes/no flag and no ALC at all, and the older Icoms
	// have none of the three.
	//
	// A client should read these before drawing a transmit meter panel: the
	// fields themselves are absent in receive, so their absence alone does not
	// distinguish "not transmitting" from "this radio cannot say".
	PowerMeter bool `json:"power_meter"`
	SWRMeter   bool `json:"swr_meter"`
	ALCMeter   bool `json:"alc_meter"`

	// PowerSwitch is true when remoses can switch the radio itself off, and on
	// again afterwards.
	//
	// The second half is the part that needs checking at the station rather
	// than in a manual. A radio switched off over CAT stops answering, and
	// whether it can be woken depends on how it is wired: an external CI-V
	// interface stays powered and listening, while a radio whose CAT arrives
	// over its own USB may take the USB device down with it and leave nothing
	// to send the wake-up to. remoses reports the command, not the wiring.
	PowerSwitch bool `json:"power_switch"`
	// TunerControl is true when remoses can switch the internal antenna tuner
	// in and out of line and read which it is.
	TunerControl bool `json:"tuner_control"`
	// TunerTune is true when remoses can start a tuning cycle.
	//
	// Separate from TunerControl because it is a different kind of thing: it
	// KEYS THE TRANSMITTER for a second or two while the radio hunts for a
	// match. A client should treat it as a transmit control, not a settings
	// toggle — and it is refused without the lock for exactly that reason.
	TunerTune bool `json:"tuner_tune"`

	// The receive front end. Each says only that remoses can read and set the
	// control over CAT — a radio may well have a preamplifier whose switch is
	// on the panel and nowhere in its command set.
	//
	// PreampLevels is how many preamplifiers there are, so 0 for none, 1 for a
	// single on/off, 2 for a set with two stages. A client offers 0..n.
	PreampLevels int `json:"preamp_levels"`
	// AttenuatorDB lists the attenuation steps the radio accepts, in dB and
	// ascending, NOT including 0 — off is always available where the list is
	// not empty. One entry is an on/off attenuator whose depth is fixed, which
	// is most radios; an IC-7610 lists fifteen.
	AttenuatorDB []int `json:"attenuator_db,omitempty"`
	// RFGainControl is the receiver's RF gain, reported as a percentage.
	RFGainControl bool `json:"rf_gain_control"`
	// AGCSettings lists the AGC speeds this radio accepts, empty where remoses
	// cannot work its AGC at all. The set really does differ between models of
	// one make, so a client should offer these rather than a fixed three.
	AGCSettings []AGC `json:"agc_settings,omitempty"`
	// IPPlusControl is Icom's IP+ intermodulation-rejection mode.
	IPPlusControl bool `json:"ip_plus_control"`
	// DigiSelControl is Icom's DIGI-SEL tracking preselector, and
	// DigiSelShiftControl its tuning trim. Separate because the big sets have
	// both and it is not safe to assume a radio with the preselector will also
	// let remoses move it.
	DigiSelControl      bool `json:"digi_sel_control"`
	DigiSelShiftControl bool `json:"digi_sel_shift_control"`

	// The noise processing. Both are counts of CIRCUITS, not switches: 0 where
	// remoses cannot work it, 1 for a plain on/off, 2 on a Kenwood, whose NB1
	// and NB2 (and NR1 and NR2) are different algorithms rather than two
	// strengths of one. A client offers 0..n.
	NoiseBlankerLevels   int  `json:"noise_blanker_levels"`
	NBLevelControl       bool `json:"nb_level_control"`
	NoiseReductionLevels int  `json:"noise_reduction_levels"`
	NRLevelControl       bool `json:"nr_level_control"`

	// The notches. NotchControl is the manual one and AutoNotchControl the
	// automatic; NotchFreqControl says the manual one can be moved, and
	// NotchWidths lists the widths where the radio has any.
	//
	// NotchExclusive is the one that changes what a client may ask for: on a
	// Kenwood the two notches are a single selector — off, auto or manual — so
	// they cannot both be on, and a request for both is refused. False on Icom
	// and Yaesu, where they are independent commands.
	NotchControl     bool         `json:"notch_control"`
	NotchFreqControl bool         `json:"notch_freq_control"`
	NotchWidths      []NotchWidth `json:"notch_widths,omitempty"`
	AutoNotchControl bool         `json:"auto_notch_control"`
	NotchExclusive   bool         `json:"notch_exclusive"`

	// Antennas is how many antenna sockets remoses can select between, 0 where
	// it cannot. RXAntennaControl is the separate receive-only input.
	//
	// Zero on every Icom here, and deliberately: those radios keep the antenna
	// as a per-band memory rather than a live selector. See State.Antenna.
	Antennas         int  `json:"antennas"`
	RXAntennaControl bool `json:"rx_antenna_control"`

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
	// BandExchange reports that the two receivers' contents can be swapped, so
	// that what the sub receiver was holding becomes the one remoses operates.
	//
	// It exists for a problem an IC-9700 has and its owner cannot work around
	// remotely: that radio refuses to put its main receiver on a band its sub
	// receiver is using, and nothing in the protocol addresses the sub. An
	// operator who left 2 m on the sub band therefore cannot reach 2 m at all
	// from the network — the frequency set is simply rejected — until the two
	// are exchanged. See State.ExchangeBands for why it is an action.
	BandExchange bool `json:"band_exchange"`
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

// SupportsVFO reports whether v is a VFO this radio can be told to address.
//
// VFOCurrent is always allowed and is deliberately not in the list: it means
// "whichever VFO the radio is on", which every radio has, and it is the zero
// value a request that names no VFO arrives with.
//
// Everything else has to be advertised. An IC-9700 has a second receiver and no
// way to address it — its Caps.VFOs is current, A and B — and asking it for
// "sub" used to reach the wire as a write to VFO B of the *main* band, which is
// a different receiver from the one that was asked for.
func (c Caps) SupportsVFO(v VFO) bool {
	if v == VFOCurrent {
		return true
	}
	for _, x := range c.VFOs {
		if x == v {
			return true
		}
	}
	return false
}

// AttenuatorControl reports whether this radio has an attenuator remoses can
// work at all.
func (c Caps) AttenuatorControl() bool { return len(c.AttenuatorDB) > 0 }

// SupportsAttenuation reports whether db is a setting this radio accepts.
//
// 0 — switched out — is accepted wherever there is an attenuator, and is not in
// the list for the same reason "not transmitting" is not a transmit meter.
func (c Caps) SupportsAttenuation(db int) bool {
	if !c.AttenuatorControl() {
		return false
	}
	if db == 0 {
		return true
	}
	for _, x := range c.AttenuatorDB {
		if x == db {
			return true
		}
	}
	return false
}

// AGCControl reports whether remoses can work this radio's AGC.
func (c Caps) AGCControl() bool { return len(c.AGCSettings) > 0 }

// SupportsNotchWidth reports whether w is a width this radio accepts.
func (c Caps) SupportsNotchWidth(w NotchWidth) bool {
	for _, x := range c.NotchWidths {
		if x == w {
			return true
		}
	}
	return false
}

// SupportsAGC reports whether v is a speed this radio accepts being set to.
func (c Caps) SupportsAGC(v AGC) bool {
	for _, x := range c.AGCSettings {
		if x == v {
			return true
		}
	}
	return false
}
