package kenwood

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Model is what remoses knows about one Kenwood radio.
//
// The ASCII CAT dialect is a family protocol: the framing, the command letters
// and their fixed-width parameters are shared, so one backend serves all of
// them. What differs is which commands exist and what their parameters mean —
// and on this family the differences are not cosmetic. The TS-890S and TS-990S
// replaced MD with OM, dropped IF; entirely, and read a 70-dot S-meter where the
// TS-480 reads a 20-dot one. Naming the model in the configuration is what lets
// the backend publish honest capabilities and avoid sending a command that would
// either be refused or, worse, quietly mean something else.
//
// The worst kind of difference is the second one, and this family has a clear
// example: FW. On a TS-590 it is the IF filter width in Hz. On a TS-890S it is
// the FM modulation-degree switch. A backend that assumed FW would move the
// operator's FM deviation while reporting a passband, so FilterWidth is a
// per-model capability rather than something to try and see.
//
// What is deliberately NOT per-model:
//
//   - The MD and OM code tables. Where a model has MD, 3 is CW; where it has OM,
//     C is LSB-D. Models differ only in which command they implement, not in
//     what the codes mean, so there is one table per command (params.go).
//   - Frequency, power and keyer encodings. FA/FB are 11 digits in Hz, PC is
//     three digits in watts and KS is 004-060 wpm on every model here.
//   - KY. The CW buffer is a fixed 24-character field family-wide.
type Model struct {
	// Name is the configuration value, lower case and without hyphens.
	Name string
	// Label is for logs, errors and the API's model string.
	Label string
	// ID is the number the ID; command answers with. Unlike an Icom's bus
	// address this genuinely identifies the radio — it is fixed in firmware and
	// there is no menu item for it — so it is trustworthy for display and for
	// warning about a configuration pointed at the wrong rig. Zero on generic,
	// which stands for a radio remoses has no profile for.
	ID int
	// Modes are the operating modes this radio accepts, in display order.
	Modes []radio.Mode

	// ModeCmd is which command reads and sets the mode. It splits the family in
	// two and drags DataMode along with it: see ModeCommand.
	ModeCmd ModeCommand
	// DataMode is how DATA is expressed, which is three genuinely different
	// answers rather than a flag. See DataModeStyle.
	DataMode DataModeStyle

	// BulkPoll is true when the radio implements IF;, the 38-character status
	// answer that collapses the fast poll into one transaction. The TS-890S and
	// TS-990S do not have it at all, and losing it costs more than a round trip:
	// IF; is the only query on this protocol that carries the TX/RX flag, so
	// without it PTT cannot be polled. See Poll.
	BulkPoll bool

	// SMeterRequest is the exact S-meter query. It is reqSM ("SM0;") on every
	// model but the TS-890S, whose SM takes no meter selector at all — and the
	// answer follows the request, so this also decides how the reply is parsed.
	// See smeterArgLen.
	SMeterRequest string
	// SMeterScale is the full-scale SM reading. The parameter is a count of
	// meter dots, not a signal level, and the count is not the same on any two
	// generations: 20, 30 and 70 dots respectively. Publishing the wrong one
	// would misdraw every S-meter a client renders.
	SMeterScale int

	// FilterWidth is true when FW carries an IF passband in Hz. Where it is
	// false FW exists but means something else entirely; see the type comment.
	FilterWidth bool
	// FilterSelect is how an IF filter is chosen, if at all.
	FilterSelect FilterStyle

	// MaxPowerW is the top of the PC range. The per-mode ceiling can be lower;
	// see maxPowerW.
	MaxPowerW int

	// BreakIn is which command switches CW break-in, which is four different
	// answers across the family. See BreakInStyle.
	BreakIn BreakInStyle

	// VFOPair is what this model's FA and FB actually name. See VFOStyle: on
	// one radio here they are not two VFOs at all.
	VFOPair VFOStyle

	// The receive front end: PA the preamplifier, RA the attenuator, RG the RF
	// gain, and the AGC under whichever command this generation puts it.
	//
	// Preamp is how many preamplifiers PA offers. One on the TS-480, TS-590 and
	// TS-990, whose parameter is "0: Pre-amp OFF, 1: Pre-amp ON"; two on the
	// TS-890S, which names them PRE 1 and PRE 2.
	Preamp int
	// Attenuator lists the RA steps in dB, ascending and without the 0. One
	// entry on the TS-480 and TS-590, whose RA is "00: ATT OFF, 01: ATT ON";
	// three on the TS-890S and TS-990S, which step 6, 12 and 18 dB.
	Attenuator []int
	// AttenuatorWidth is how many digits RA takes, and the two generations
	// disagree in a way that would otherwise put a syntax error on the wire:
	// the TS-480 and TS-590 spell it RA00/RA01, the TS-890S RA0, the TS-990S
	// RA<band><value>. See AttenuatorStyle for the last of those.
	AttenuatorWidth int
	// AttenuatorBanded marks the TS-990S, whose RA and PA and GC and RG all
	// carry a leading main/sub band selector that the others have no room for.
	Banded bool
	// RFGainMax is the top of the RG range, and it is NOT the same across the
	// family: 100 on a TS-480 and 255 on everything since. Publishing a
	// percentage against the wrong ceiling would misreport the same knob by a
	// factor of two and a half.
	RFGainMax int
	// AGC maps each speed onto its parameter, and AGCCmd says which command
	// carries it: GC on everything current, GT on the TS-480, whose GT is a
	// three-digit AGC constant where its successors use GT for a time constant
	// and GC for the speed.
	AGC    map[radio.AGC]string
	AGCCmd string
	// The noise processing, the notches and the antenna.
	//
	// NoiseBlanker and NoiseReduction are counts of CIRCUITS: two on this
	// family, NB1/NB2 and NR1/NR2, which are different algorithms rather than
	// two strengths of one. NBLevel is NL and NRLevel is RL, both refused while
	// their circuit is off.
	//
	// Notch is NT, one selector carrying off, auto and manual — which is why
	// Caps.NotchExclusive is true here and false on the other two families —
	// and NotchFreq is BP, the manual notch's position.
	//
	// Antennas is AN's socket count and RXAntenna its receive-only input.
	// Claimed only where the parameter layout has been transcribed: the
	// TS-590's AN takes three parameters, the TS-890S's takes four, and the
	// TS-990S has no AN row at all. Sending the wrong width would be a syntax
	// error rather than a wrong antenna, but it would still be sending
	// something nobody read.
	NoiseBlanker   int
	NBLevel        bool
	NoiseReduction int
	NRLevel        bool
	Notch          bool
	NotchFreq      bool
	Antennas       int
	RXAntenna      bool

	// The transmit audio chain: MG the gain into the modulator, the speech
	// processor's switch, and PL its level.
	//
	// MicGainMax and ProcLevelMax are the top of a range that begins at 000,
	// and the family does not agree on where it ends. MG is "000 ~ 100" on the
	// TS-480, the TS-590S/SG and the TS-890S, and "000 ~ 255 (in steps of 1)"
	// on the TS-990S; PL's two fields move with it, "000 (minimum) ~ 100
	// (maximum)" on the first three and 000 ~ 255 on the TS-990S. That is the
	// RG trap a second time — the same control reported on scales a factor of
	// two and a half apart — which is why the API publishes a percentage and
	// each model states its own ceiling here. Zero where the reference has no
	// row for the command.
	//
	// ProcCmd is the switch's command, and the two generations do not spell it
	// the same way. It is PR on the TS-480 and TS-590S/SG; on the TS-890S and
	// TS-990S it is PR0, and those two also have a PR1 that sets the
	// processor's effect type, "0: Soft, 1: Hard". So "PR1;" — the frame that
	// switches a TS-590's processor ON — is on a TS-890S the read form of an
	// unrelated setting, and nothing about that goes wrong loudly: the frame is
	// well formed, the radio answers, and the processor stays off. Empty where
	// there is no such command.
	MicGainMax   int
	ProcCmd      string
	ProcLevelMax int

	// AGCOnCode is the parameter that turns the AGC back ON, and without it
	// switching the AGC off is a ONE-WAY TRIP.
	//
	// The reference documents the parameter — "3: AGC Off → On (AGC returns to
	// its Slow/Fast status before turning Off)", "used only for turning AGC
	// On" — and it reads as one option among four. What no reference says is
	// that the other two are REFUSED while the AGC is off: on a TS-590S, GC1
	// and GC2 both draw an error and the radio stays off. That half came from
	// the radio, and it is what makes this the only way back rather than a
	// convenience.
	//
	// The TS-890S and TS-990S values are transcribed rather than tested;
	// neither has been on the bench. If they take a speed directly from off,
	// sending this first is harmless.
	//
	// Empty on the TS-480, whose AGC is a different command with no such value
	// documented.
	AGCOnCode string
}

// VFOStyle is what a model's FA and FB commands address.
//
// The two values are the same opcodes pointing at different axes, which is the
// trap this backend has already met once on the Icom side: the IC-7610's 25/26
// name main and sub bands where the IC-9700's name the selected and unselected
// VFO of one band. Reading FB as "VFO B" on a radio where it means "sub band"
// would set the wrong receiver's frequency and report it as the wrong thing.
type VFOStyle int

const (
	// VFOPairAB is two VFOs of one receiver: FA is VFO A, FB is VFO B, and
	// FR/FT select which is received and which is transmitted. The TS-480,
	// TS-590S/SG and TS-890S, and what generic assumes.
	VFOPairAB VFOStyle = iota
	// VFOPairMainSub is the TS-990S, where "FA Main Band Frequency" and "FB Sub
	// Band Frequency" address its two receivers rather than two VFOs of one.
	// Each band has its own VFO A and B underneath, reached by commands this
	// backend does not implement.
	//
	// So remoses offers no VFO addressing at all on that radio: calling its Sub
	// band "VFO B" would be a different radio's model wearing the same letters.
	VFOPairMainSub
)

// twoVFOs reports whether FA and FB name two VFOs that remoses can address as A
// and B.
func (v VFOStyle) twoVFOs() bool { return v == VFOPairAB }

// BreakInStyle is how a model reads and sets CW break-in.
//
// This matters more than a settings switch usually would. With break-in off, a
// KY message is accepted, the rig's buffer drains on schedule and nothing is
// transmitted — so remoses has to be able to read and set this, or CAT Morse
// silently goes nowhere. It was found exactly that way, twice: on an IC-9700
// and then on a TS-590S, both times by the operator reporting that they heard
// none of what remoses reported as sent.
type BreakInStyle int

const (
	// BreakInNone is a radio whose break-in remoses will not touch. Only
	// generic, and by choice rather than for want of a command: see the
	// registry entry for why an unidentified radio is the one case where
	// guessing at VX is not worth it.
	BreakInNone BreakInStyle = iota
	// BreakInVX is the TS-590S/SG arrangement, and the surprising one: there is
	// no break-in command at all. VX sets VOX, except that "when transmitting
	// the VX command in CW mode, the Break-in function is set and read, rather
	// than the VOX function". One command, two meanings, chosen by the mode the
	// rig happens to be in — so remoses only touches it in CW.
	//
	// The TS-480 is given this style too, on inference rather than on its own
	// reference; the registry entry sets out the argument.
	BreakInVX
	// BreakInBI2 is BI with two values, off and on: the TS-890S. Semi and full
	// are not distinguished by BI; the SD delay decides, 0 ms being full.
	BreakInBI2
	// BreakInBI3 is BI with three: off, semi and full directly. Only the
	// TS-990S states the three values, and there SD does not decide the mode.
	BreakInBI3
)

// binary reports whether this style's on/off command cannot itself distinguish
// semi from full, so that the SD delay has to be read and written to tell them
// apart.
func (b BreakInStyle) binary() bool {
	return b == BreakInVX || b == BreakInBI2
}

// ModeCommand is the command a model uses to read and set the operating mode.
//
// This is the family's fault line. The two commands are not spellings of the
// same thing: OM addresses a display area with its first parameter and folds
// DATA into the mode code with its second, so a backend cannot simply translate
// one into the other.
type ModeCommand int

const (
	// ModeMD is MD: one digit, DATA on a separate DA command.
	ModeMD ModeCommand = iota
	// ModeOM is OM: P1 selects the display area, P2 is a mode code that already
	// carries DATA.
	ModeOM
)

// DataModeStyle is how a model expresses DATA mode.
type DataModeStyle int

const (
	// DataModeNone is a radio with no DATA mode to set: the TS-480. Sending DA
	// to it is not a no-op to be tolerated, it is a request remoses must refuse,
	// because reporting a DATA flag the rig does not have would be a lie.
	DataModeNone DataModeStyle = iota
	// DataModeCommand is DA, orthogonal to MD: USB-DATA is MD2 plus DA1. This is
	// why radio.State carries DataMode separately from Mode.
	DataModeCommand
	// DataModeInMode is OM's arrangement, where DATA lives in the mode code
	// itself: USB-D is P2 = D. There is nothing to send afterwards, and encoding
	// LSB with DATA has to produce C rather than 1.
	DataModeInMode
)

// FilterStyle is how a model selects among its IF filters.
type FilterStyle int

const (
	// FilterSelectNone is a radio with no filter selection over CAT.
	FilterSelectNone FilterStyle = iota
	// FilterSelectAB is FL1 / FL2 — IF Filter A and B on the TS-590.
	FilterSelectAB
	// FilterSelectABC is the TS-890S arrangement: ONE command, FL0, whose
	// parameter picks receive filter A, B or C.
	//
	// FL0 is not "filter zero". FL0, FL1, FL2 and FL3 are four unrelated
	// commands — Select the Receive Filter, Roofing Filter, IF Filter Shape and
	// AF Filter Type — so treating them as four filter slots would set the
	// roofing filter when the operator asked for slot 2.
	FilterSelectABC
	// FilterSelectBandABC is the TS-990S arrangement: the same FL0 command, but
	// its first parameter selects the main or sub band and the second picks
	// A, B or C.
	FilterSelectBandABC
)

// slots is how many selections this style offers, which is what Caps publishes
// and what the session range-checks a request against.
//
// Three for the A/B/C styles. Whether C is actually available depends on a menu
// setting the rig does not expose, so a request for it may be rejected; that is
// a clear error from the radio rather than something remoses can predict.
func (f FilterStyle) slots() int {
	switch f {
	case FilterSelectAB:
		return 2
	case FilterSelectABC, FilterSelectBandABC:
		return 3
	}
	return 0
}

// param maps a 1-based API slot onto the FL parameter character.
//
// The session's slot numbering starts at 1 (Caps.FilterSlots is a count), so on
// the A/B/C styles, whose parameter starts at 0, the two are offset by one.
func (f FilterStyle) param(slot int) (byte, bool) {
	switch f {
	case FilterSelectAB:
		if slot == 1 || slot == 2 {
			return byte('0' + slot), true
		}
	case FilterSelectABC, FilterSelectBandABC:
		if slot >= 1 && slot <= 3 {
			return byte('0' + slot - 1), true
		}
	}
	return 0, false
}

// decode is param's inverse, turning an FL answer's selection character back
// into a slot.
func (f FilterStyle) decode(c byte) (int, bool) {
	switch f {
	case FilterSelectAB:
		if c == '1' || c == '2' {
			return int(c - '0'), true
		}
	case FilterSelectABC, FilterSelectBandABC:
		if c >= '0' && c <= '2' {
			return int(c-'0') + 1, true
		}
	}
	return 0, false
}

// modesCommon is the set every radio here has. FSK is what the rigs call RTTY.
func modesCommon() []radio.Mode {
	return []radio.Mode{
		radio.ModeLSB, radio.ModeUSB, radio.ModeCW, radio.ModeCWR,
		radio.ModeAM, radio.ModeFM, radio.ModeFSK, radio.ModeFSKR,
	}
}

func withModes(base []radio.Mode, extra ...radio.Mode) []radio.Mode {
	return append(base, extra...)
}

// md describes a radio of the TS-590 generation, which is what the rest of this
// backend was originally written against: MD for the mode with DATA on its own
// DA command, IF; for the bulk poll, a 30-dot S-meter read with SM0;, FW for the
// filter width in Hz, FL for IF Filter A/B, and 100 W. Models that differ
// override the fields after building.
func md(name, label string, id int) Model {
	return Model{
		Name:          name,
		Label:         label,
		ID:            id,
		Modes:         modesCommon(),
		ModeCmd:       ModeMD,
		DataMode:      DataModeCommand,
		BulkPoll:      true,
		SMeterRequest: reqSM,
		SMeterScale:   30,
		FilterWidth:   true,
		FilterSelect:  FilterSelectAB,
		MaxPowerW:     100,
		BreakIn:       BreakInVX,
		VFOPair:       VFOPairAB,
		// The TS-590 front end. PA is one digit and answers two; RA is two
		// digits and answers four; RG is three throughout. GC carries the AGC,
		// in a form with no middle speed — "0: AGC Off, 1: AGC Slow, 2: AGC
		// Fast" — and its 3 is a set-only "off then on again" that remoses does
		// not offer, since it means "restore whatever you had", which is not a
		// state a client can ask for by name.
		Preamp: 1,
		// One pad, and the reference does not say how deep: RA is "00: ATT OFF,
		// 01: ATT ON" and nothing more. 12 dB is this series' published receiver
		// specification, and it is a LABEL only — the byte on the wire is 01
		// either way, so a wrong figure here mislabels a control rather than
		// mis-setting one. Worth confirming against the radio's own display.
		Attenuator:      []int{12},
		AttenuatorWidth: 2,
		RFGainMax:       255,
		AGCCmd:          "GC",
		AGC: map[radio.AGC]string{
			radio.AGCOff: "0", radio.AGCSlow: "1", radio.AGCFast: "2",
		},
		AGCOnCode: "3",
		// Two blankers and two reducers, each with a level, plus the one notch
		// selector and its position. The antenna is claimed for this shape
		// only: AN there is "1: ANT1, 2: ANT2" with a receive input on P2.
		NoiseBlanker:   2,
		NBLevel:        true,
		NoiseReduction: 2,
		NRLevel:        true,
		Notch:          true,
		NotchFreq:      true,
		Antennas:       2,
		RXAntenna:      true,
		// The transmit audio chain in its older spelling: MG three digits over
		// 000-100, PR one digit for the processor's switch, and PL carrying an
		// input level and an output level in one frame, each three digits over
		// 000-100. Identical rows in the TS-480 and TS-590S/SG references, so
		// this is the shape both inherit.
		MicGainMax:   100,
		ProcCmd:      "PR",
		ProcLevelMax: 100,
	}
}

// om describes a radio of the TS-890S/TS-990S generation. Everything the two
// have in common is here; only the S-meter request and the power ceiling differ
// between them.
//
// They gain PSK and PSK-R, which OM can select directly — the older radios
// decode PSK in software through their DATA modes and have no code for it — and
// lose three things: IF;, the FW filter width, and with IF; the ability to poll
// PTT at all.
func om(name, label string, id int) Model {
	m := md(name, label, id)
	m.Modes = withModes(modesCommon(), radio.ModePSK, radio.ModePSKR)
	m.ModeCmd = ModeOM
	m.DataMode = DataModeInMode
	m.BulkPoll = false
	m.SMeterScale = 70
	m.FilterWidth = false
	// The band-selected form is the TS-990S's; the TS-890S overrides it below.
	m.FilterSelect = FilterSelectBandABC
	// Both of this generation have a real BI command instead of VX's double
	// meaning. How many values it takes differs, so the TS-890S overrides this.
	m.BreakIn = BreakInBI3
	// And both gain a stepped attenuator where the older pair have one pad, in
	// dB their own references print: "1: 6 dB, 2: 12 dB, 3: 18 dB". Its width
	// drops to one digit, and the AGC gains a middle speed.
	m.Attenuator = []int{6, 12, 18}
	m.AttenuatorWidth = 1
	m.AGC = map[radio.AGC]string{
		radio.AGCOff: "0", radio.AGCSlow: "1", radio.AGCMid: "2", radio.AGCFast: "3",
	}
	// Their off-then-on value moves up with the extra speed: 4 here, 3 on the
	// TS-590. Both references describe it the same way, and the TS-890S's adds
	// "will turn the AGC On and will set the previous AGC state".
	m.AGCOnCode = "4"
	// Their AN is a different shape — four parameters on the TS-890S, and the
	// TS-990S's command list has no AN row at all — so neither inherits the
	// TS-590's antenna support. The noise and notch commands are shared.
	m.Antennas = 0
	m.RXAntenna = false
	// And the speech processor's switch moves from PR to PR0, because both of
	// these radios needed the second digit for PR1, the processor's effect
	// type. MG and PL keep their letters and their shapes; only the TS-990S
	// changes what they count in, which it does below.
	m.ProcCmd = "PR0"
	return m
}

// models is the registry, keyed by configuration name.
//
// Every field is transcribed from that radio's own PC Control Command Reference
// Guide. Where a model is missing a command the reference simply has no row for
// it, which is why the capability is recorded here rather than discovered by
// sending it and reading the rejection: a rejection is indistinguishable from
// "refused in the rig's current state", the other thing ?; means.
var models = map[string]Model{
	// generic is the escape hatch for a Kenwood-compatible radio remoses has no
	// profile for, and there are many: the dialect is spoken by Elecraft and by
	// modern Yaesu too. It claims the TS-590 shape, which is the most widely
	// copied one, and no ID, so identification never claims a model.
	"generic": func() Model {
		m := md("generic", "generic Kenwood", 0)
		// No break-in, unlike the TS-590 shape this otherwise copies.
		//
		// The inference that makes VX safe to write on a TS-480 — no BI
		// command, an SD delay, and a family where those two facts move
		// together — is an inference about Kenwood. It says nothing about the
		// Elecrafts and modern Yaesus that also speak this dialect, and this is
		// the one command in the profile that WRITES on the strength of the
		// guess rather than merely reading.
		//
		// Being wrong leaves VOX switched on. remoses only writes VX in CW, so
		// nothing happens there — it surfaces later, when the operator moves to
		// SSB and finds the radio keying on room noise. A fault that appears
		// somewhere other than where it was caused is a bad one to introduce
		// into an unidentified radio.
		//
		// The cost of abstaining is that CW on a generic radio can still be
		// accepted and not transmitted, which is exactly where every
		// unidentified radio already stood. Name the model to get the check.
		m.BreakIn = BreakInNone
		// The transmit audio chain IS kept, unlike break-in, and the difference
		// is worth stating because the argument just above looks as though it
		// should carry over.
		//
		// It does not, and the reason is where the fault would appear. VX is
		// written blind and its consequence surfaces somewhere else entirely:
		// later, in another mode, as a radio keying on room noise. MG, PR and
		// PL are each written and then read straight back inside the one
		// operation, so a dialect that spells them differently shows up as a
		// rejection, or as a value that does not come back — at the moment of
		// asking, to the operator who asked. A wrong answer that arrives with
		// the question is a different kind of thing from one that arrives an
		// hour later in another mode.
		return m
	}(),

	// The TS-480 predates DATA mode as a CAT concept: it has no DA command, so a
	// DATA request has to be refused rather than approximated. Its S-meter is
	// also the odd one out at 20 dots, and it offers no IF filter selection over
	// CAT. PC runs 005-100.
	"ts480": func() Model {
		m := md("ts480", "TS-480", 20)
		m.DataMode = DataModeNone
		m.SMeterScale = 20
		m.FilterSelect = FilterSelectNone
		// Break-in on VX here is INFERRED, not transcribed. Its reference
		// documents VX as "the VOX function status" and says nothing about CW,
		// unlike the TS-590's, which spells the overload out.
		//
		// Three things say it is nonetheless the same radio underneath. It has
		// SD, "the CW Break-in time delay", where 0000 is full break-in — so
		// break-in exists and is switchable somehow. It has no BI. And across
		// this family the two facts move together: the TS-890S and TS-990S,
		// which do have BI, both fence VX off with "cannot be set in modes
		// other than SSB/AM/FM", while the TS-590, which does not, overloads
		// it. The TS-480 has no BI and no such fence.
		//
		// So the silence reads as an omission rather than a denial. If it turns
		// out to be wrong the cost is bounded: remoses writes VX only in CW,
		// where the effect would be VOX switched on rather than break-in.
		m.BreakIn = BreakInVX
		// Its RF gain counts to 100, not to 255. The same knob, reported on a
		// different scale from every later radio in the family — which is
		// exactly why RFGainMax is per model and the API publishes a
		// percentage.
		m.RFGainMax = 100
		// And its AGC is on GT rather than GC, in three digits: "000: OFF,
		// 001: Fast, 002: Slow". Later radios use GT for the time constant and
		// put the speed on GC, so the same two letters mean different things
		// two generations apart.
		m.AGCCmd = "GT"
		m.AGC = map[radio.AGC]string{
			radio.AGCOff: "000", radio.AGCFast: "001", radio.AGCSlow: "002",
		}
		// And no off-then-on value: its GT table is those three and stops. Which
		// may mean this radio takes a speed directly from off, or may mean it has
		// the TS-590's trap with no documented way out; nothing here can tell,
		// and inventing a fourth parameter to send blind is not the way to find
		// out. Whoever puts one on the air will know within a minute.
		m.AGCOnCode = ""
		return m
	}(),

	"ts590s":  md("ts590s", "TS-590S", 21),
	"ts590sg": md("ts590sg", "TS-590SG", 23),

	// The TS-890S is the only model whose SM takes no meter selector: it answers
	// SMnnnn where every other radio here answers SM0nnnn.
	"ts890s": func() Model {
		m := om("ts890s", "TS-890S", 24)
		m.SMeterRequest = reqSMNoSelector
		// Single receiver, so FL0 takes the selection directly with no band
		// parameter in front of it — unlike the TS-990S.
		m.FilterSelect = FilterSelectABC
		// Its BI is off/on only, where the TS-990S's takes semi and full
		// directly; here the SD delay is what separates them.
		m.BreakIn = BreakInBI2
		// Two preamplifiers, which its reference names PRE 1 and PRE 2 where
		// the rest of the family has a single on/off.
		m.Preamp = 2
		return m
	}(),

	// The TS-990S is the only 200 W radio in the family, which the percentage
	// scale has to follow: 100 W is half power there and full power everywhere
	// else.
	"ts990s": func() Model {
		m := om("ts990s", "TS-990S", 22)
		m.MaxPowerW = 200
		// And the only one whose FA and FB are not VFO A and VFO B. Its
		// reference names them "Main Band Frequency" and "Sub Band Frequency":
		// two receivers, each with its own pair of VFOs underneath. See
		// VFOPairMainSub for why that means no VFO addressing rather than
		// addressing under different names.
		m.VFOPair = VFOPairMainSub
		// Two receivers, and every front-end command carries which one it is
		// about: PA0/PA1, RA<band><value>, GC<band><value>, RG<band><nnn>.
		// remoses works the main band, as it does everywhere else on this
		// radio — see VFOPairMainSub.
		m.Banded = true
		// And the only one whose transmit-audio levels count past 100. Its MG
		// is "000 ~ 255 (in steps of 1)" and both of PL's fields are "000
		// (minimum) ~ 255 (maximum)", where the TS-480, TS-590 and TS-890S all
		// stop at 100. A percentage written against the wrong one of those two
		// ceilings sets roughly two fifths of the audio the operator asked for
		// and reports it as the figure they typed.
		//
		// Banded does NOT reach them, and that is the reference's arrangement
		// rather than an omission here: PA, RA, GC and RG each carry a main/sub
		// selector because there are two receivers, but there is one
		// transmitter, so MG, PR0 and PL take their value with nothing in front
		// of it.
		m.MicGainMax = 255
		m.ProcLevelMax = 255
		return m
	}(),
}

// DefaultModel is used when the configuration names none. It preserves the
// behaviour remoses had before models existed, when this backend was written
// against the TS-590S/SG reference alone.
const DefaultModel = "ts590sg"

// LookupModel resolves a configuration model name.
//
// Case and hyphens are ignored, so "TS-590SG", "ts-590sg" and "ts590sg" are the
// same radio. The registry keys are the hyphen-free form because that is what
// the configuration documents, but an operator writing the name the way it is
// printed on the front panel should not have to discover the difference.
func LookupModel(name string) (Model, error) {
	if strings.TrimSpace(name) == "" {
		name = DefaultModel
	}
	m, ok := models[modelKey(name)]
	if !ok {
		return Model{}, fmt.Errorf("kenwood: unknown model %q, want one of %s",
			name, strings.Join(ModelNames(), ", "))
	}
	return m, nil
}

// modelKey folds a name into a registry key.
func modelKey(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "")
}

// ModelNames lists the configurable model names, sorted.
func ModelNames() []string {
	out := make([]string, 0, len(models))
	for n := range models {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ModelForID reports the model whose ID; answer is id.
//
// Used for display and for the mismatch warning at Init, never to choose a
// profile. The ID does identify the radio, but a model remoses has no profile
// for still answers with something, and the operator's configuration is the
// statement of intent this backend acts on.
func ModelForID(id int) (Model, bool) {
	if id == 0 {
		return Model{}, false
	}
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// supportsMode reports whether this radio accepts m.
func (m Model) supportsMode(x radio.Mode) bool {
	for _, v := range m.Modes {
		if v == x {
			return true
		}
	}
	return false
}

// modeReq is the command that reads the mode.
//
// OM's P1 selects which display area to read, and 0 is the left-hand one — the
// main receiver, the only one this backend publishes.
func (m Model) modeReq() string {
	if m.ModeCmd == ModeOM {
		return reqOM
	}
	return reqMD
}

// modeKey is the correlation key the mode answer arrives under.
func (m Model) modeKey() backend.Key {
	if m.ModeCmd == ModeOM {
		return keyOM
	}
	return keyMD
}

// modeSet builds the command that sets the mode.
//
// On OM the first parameter is ignored by the set form — the reference says to
// enter any value — so it is always 0, and the DATA flag is folded into the code
// rather than sent afterwards.
func (m Model) modeSet(mode radio.Mode, data bool) (string, error) {
	if !m.supportsMode(mode) {
		return "", fmt.Errorf("kenwood: the %s does not have mode %s", m.Label, mode)
	}
	if m.ModeCmd == ModeOM {
		code, err := encodeOMMode(mode, data)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("OM0%c;", code), nil
	}
	digit, err := encodeMode(mode)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("MD%c;", digit), nil
}

// checkDataMode reports why this radio cannot put mode into DATA, or nil if it
// can. Both refusals are real: one radio has no DATA mode at all, and on the
// others the rig answers an error when DATA is asked for in CW or FSK.
func (m Model) checkDataMode(mode radio.Mode) error {
	if m.DataMode == DataModeNone {
		return fmt.Errorf("kenwood: the %s has no DATA mode", m.Label)
	}
	if !supportsDataMode(mode) {
		return fmt.Errorf("kenwood: %s has no DATA mode on the %s; DATA is available only in LSB, USB, FM and AM",
			mode, m.Label)
	}
	return nil
}

// filterSlotSet builds the FL command that selects slot, or explains why it
// cannot.
func (m Model) filterSlotSet(slot int) (string, error) {
	p, ok := m.FilterSelect.param(slot)
	switch m.FilterSelect {
	case FilterSelectNone:
		return "", fmt.Errorf("kenwood: the %s has no IF filter selection over CAT", m.Label)
	case FilterSelectAB:
		if !ok {
			return "", fmt.Errorf("kenwood: filter slot must be 1 (IF Filter A) or 2 (IF Filter B), have %d", slot)
		}
		return fmt.Sprintf("FL%c;", p), nil
	case FilterSelectABC:
		if !ok {
			return "", fmt.Errorf("kenwood: filter slot must be 1, 2 or 3 (receive filter A, B or C on the %s), have %d",
				m.Label, slot)
		}
		return fmt.Sprintf("FL0%c;", p), nil
	case FilterSelectBandABC:
		if !ok {
			return "", fmt.Errorf("kenwood: filter slot must be 1, 2 or 3 (receive filter A, B or C on the %s), have %d",
				m.Label, slot)
		}
		// The first parameter is the band. remoses addresses the main band
		// only; Caps.SubReceiver is false and one filter selection is published.
		return fmt.Sprintf("FL00%c;", p), nil
	}
	return "", fmt.Errorf("kenwood: unknown filter style on the %s", m.Label)
}

// filterSlotRead is the request that reads the current filter selection back,
// or "" when there is no form that is safe to send.
//
// The TS-890S is the awkward case. Its manual prints the read form of FL0 as
// "FL0 P1 ;" — with the selection parameter — which is indistinguishable from
// the set form, so there is no way to ask without also telling. Rather than risk
// changing a filter while trying to read one, remoses does not read it back
// there: the set is echoed by the AI push that Init enables, and the demux
// applies that exactly like a solicited answer.
func (m Model) filterSlotRead() string {
	switch m.FilterSelect {
	case FilterSelectAB:
		return "FL;"
	case FilterSelectBandABC:
		return "FL00;" // P1 = main band; unambiguous, the selection is P2
	}
	return ""
}

// filterSelectionChar picks the character carrying the filter selection out of
// an FL answer's argument.
//
// The frame splitter keys on the two letters "FL", so everything after them is
// the argument — including the command's own digit on the models whose command
// is FL0. The offsets are therefore not the same across the family:
//
//	TS-590   FL1;      arg "1"    -> selection at 0 (the digit IS the command)
//	TS-890S  FL021;    arg "021"  -> 0 is FL0's digit, 2 the selection, 1 the
//	                                 270 Hz option flag
//	TS-990S  FL0 0 2;  arg "002"  -> 0 is FL0's digit, 0 the band, 2 the
//	                                 selection
//
// Taking the first character everywhere would report FL0's own digit as a filter
// slot on two of the three, and the band on the third.
func (m Model) filterSelectionChar(arg []byte) (byte, bool) {
	switch m.FilterSelect {
	case FilterSelectAB:
		if len(arg) >= 1 {
			return arg[0], true
		}
	case FilterSelectABC:
		if len(arg) >= 2 && arg[0] == '0' {
			return arg[1], true
		}
	case FilterSelectBandABC:
		// Main band only. remoses publishes one filter selection and reports
		// Caps.SubReceiver false, so letting a sub-band answer through would
		// overwrite the main band's slot with an unrelated value — the same
		// reason an OM frame for the right-hand display is decoded to nothing.
		if len(arg) >= 3 && arg[0] == '0' && arg[1] == '0' {
			return arg[2], true
		}
	}
	return 0, false
}

// procReq is the request that reads the speech processor's switch, and procSet
// the frame that writes it. Both are built from ProcCmd because the command is
// PR on one half of the family and PR0 on the other; see the field's comment
// for what PR1 means on the half that spells it PR0.
func (m Model) procReq() string { return m.ProcCmd + ";" }

func (m Model) procSet(on bool) string {
	if on {
		return m.ProcCmd + "1;"
	}
	return m.ProcCmd + "0;"
}

// procSwitchChar picks the character carrying the processor's on/off state out
// of a PR answer's argument.
//
// The frame splitter keys on the run of letters, so everything after "PR" is
// the argument — including the command's own digit on the models whose command
// is PR0, exactly as it does for FL0:
//
//	TS-590   PR1;    arg "1"    -> the switch is at 0
//	TS-890S  PR01;   arg "01"   -> 0 is PR0's own digit, 1 the switch
//
// That digit is CHECKED rather than skipped. On the TS-890S and TS-990S, PR1 is
// the processor's effect type, so an unsolicited PR11; announcing "Hard" would
// otherwise be read as the processor having been switched on.
func (m Model) procSwitchChar(arg []byte) (byte, bool) {
	if m.ProcCmd == "" {
		return 0, false
	}
	digits := m.ProcCmd[2:] // "" on PR, "0" on PR0
	if len(arg) < len(digits)+1 || string(arg[:len(digits)]) != digits {
		return 0, false
	}
	return arg[len(digits)], true
}

// smeterArgLen is how many characters the SM answer's parameter has.
//
// The answer mirrors the request: a radio asked with SM0; answers SM0nnnn, and
// the TS-890S — whose SM has no meter selector — answers SMnnnn. Reading four
// digits at the wrong offset would report a tenth of the true signal, so the
// length is checked rather than assumed.
func (m Model) smeterArgLen() int {
	if m.SMeterRequest == reqSMNoSelector {
		return 4
	}
	return 5
}
