package yaesu

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hessu/remoses/internal/radio"
)

// Model is what remoses knows about one Yaesu radio.
//
// Yaesu's CAT dialect is a family protocol in its framing and in most of its
// command letters, but it is emphatically not one in its parameters, and the
// differences land in the places where being wrong is worst: the mode code, the
// bulk status layout and the width of the frequency field.
//
//   - MD code E is PSK on the FT-710, FTdx10, FTdx101 and FTX-1 and C4FM on the
//     FT-991A. Decoding an FT-991A with the family table reports a radio
//     sitting in C4FM as PSK — a wrong answer rather than a missing one, the
//     same failure the IC-910H's 0x04 produces (DESIGN.md §5.4). Every model
//     therefore carries its own table, never a family default.
//   - FA/FB are nine digits on the FTdx101 generation and eight on the FT-950
//     generation, and the IF answer is one byte shorter to match, because the
//     frequency field is where the byte goes. Every field after it shifts. See
//     decodeIF.
//   - The FTX-1's IF is longer again, because its memory-channel field is five
//     characters rather than three.
//   - The FTX-1's PC carries a head selector, so the plain three-digit form is
//     malformed there — and on the FTdx5000 and FTdx9000 the three digits are
//     an uncalibrated 000-255 index rather than watts.
//   - The FTdx9000 has no ID and no NA command at all, so two reads the rest of
//     the family makes unconditionally would cost it a full per-command timeout
//     each.
//
// What is deliberately NOT per-model:
//
//   - PTT. TX; reads, TX1; keys and TX0; unkeys on all twelve.
//   - The S-meter. SM0; answers three digits, 000-255, everywhere.
//   - CW. No model has a CAT command that streams arbitrary Morse, so there is
//     nothing to vary; see the package doc.
type Model struct {
	// Name is the configuration value, as printed in YaesuModels.
	Name string
	// Label is for logs, errors and the API's model string.
	Label string
	// IDs are the numbers ID; answers with, e.g. 670 for an FT-991A. Unlike an
	// Icom's bus address these are fixed in firmware and genuinely identify the
	// radio, so they are worth cross-checking at Init. Usually one; the
	// FTdx1200 has two, because the number it reports says whether the optional
	// FFT-1 unit is fitted. Empty on generic, which stands for a Yaesu remoses
	// has no profile for, and on the FTdx9000, which has no ID command — see
	// HasID, which is the flag that decides whether the command is sent.
	IDs []int

	// HasID and HasNarrow are the two commands the FTdx9000 does not have. They
	// are recorded rather than probed because a Yaesu answers a command it does
	// not implement with silence, so asking costs the session's full
	// per-command timeout and returns nothing.
	//
	// Missing ID means there is no identity cross-check to make on that radio.
	// Missing NA means nothing here, since the same radio has no bandwidth
	// table for NA to choose a column of.
	//
	// HasAI is true on every profiled model, the FTdx9000 included, so no radio
	// here is poll-only. It stays a per-model field rather than becoming a
	// constant because push updates are the one capability an operator feels
	// directly, and a model that lacked AI would have to be able to say so.
	HasID     bool
	HasAI     bool
	HasNarrow bool

	// The receive front end: PA the preamplifier (which Yaesu calls IPO when it
	// is switched out), RA the attenuator, RG the RF gain, GT the AGC.
	//
	// Preamp is how many amplifiers PA offers past IPO: two nearly everywhere,
	// one on the FT-891, whose parameter is "0: IPO, 1: AMP" with no second
	// stage.
	Preamp int
	// Attenuator lists the RA steps in dB. Three on the FT-950 generation and
	// the FTdx sets, which print "1: 6 dB, 2: 12 dB, 3: 18 dB"; one on the
	// FT-891, FT-991A and FTX-1, whose RA is "0: OFF, 1: ON" with the depth
	// unstated — see the registry entries for where that dB figure comes from.
	// Empty on the FTdx9000, which has no RA command at all.
	Attenuator []int
	// AGC is the GT map. The same five settings across the family, and worth
	// stating per model anyway because the FTdx9000 is the one radio here whose
	// reference this backend has not read for it.
	AGC map[radio.AGC]byte
	// RFGain is true when RG reads and sets the receiver RF gain, 000-255.
	RFGain bool

	// The noise processing, the notches and the antenna. NoiseBlanker covers
	// NB and its NL level together, NoiseReduction covers NR and RL, Notch is
	// BP (which carries both the switch and the position) and AutoNotch is BC.
	//
	// Antennas is AN's socket count, and it is NOT family-wide: the FTdx101
	// generation has "AN ANTENNA NUMBER" with three sockets, and the FT-891's
	// command list has no AN row at all.
	NoiseBlanker   bool
	NoiseReduction bool
	Notch          bool
	AutoNotch      bool
	Antennas       int

	// Modes are the operating modes this radio accepts, in display order.
	Modes []radio.Mode
	// Codes is this model's whole MD table, keyed by the mode character. The
	// DATA flag rides along in the value because Yaesu folds data mode into the
	// mode code — there is no DA command to carry it separately, so 8 is
	// LSB-DATA and C is USB-DATA. Codes the manual lists as unused are absent,
	// so a frame carrying one reports nothing rather than overwriting a good
	// mode.
	Codes map[byte]modeCode

	// FreqDigits is the width of the FA/FB parameter: nine on the FTdx101
	// generation, eight on the FT-950 generation. It governs encoding only.
	// Decoding reads whatever arrived, because the two widths are the same
	// zero-padded decimal Hz and a misconfigured station should still be read
	// correctly — the rule DESIGN.md §5.4 settled on for the IC-905.
	FreqDigits int
	// TunerTuneParam is the AC P3 value that starts a tuning cycle, which is
	// not the same on both generations: the FT-950's table reads "0: Tuner OFF,
	// 1: Tuner ON, 2: Tuning Start" and the FT-710's "0: Tuner OFF (Tuning
	// Stop), 1: Tuner ON, 2: -, 3: Tuning Start".
	//
	// A wrong value here fails safe, which is worth knowing: 2 on the newer
	// radios is the documented no-op, and 3 on the older ones is out of range.
	// Neither keys the transmitter, so the failure mode is a tune that does not
	// happen rather than one that happens unasked.
	TunerTuneParam byte

	// MaxPowerW is the top of the PC range in watts: 100 W on most, 200 on the
	// FTdx101MP and per-head on the FTX-1. See Rig.maxPowerW. Zero where PC is
	// not in watts at all — see PowerRaw.
	MaxPowerW int
	// PowerHead marks the FTX-1, whose PC takes a head selector in front of the
	// three digits: 1 is the field head (005-010 W), 2 the SPA-1 amplifier
	// (005-100 W). Sending the common three-digit form there is malformed.
	PowerHead bool
	// PowerRaw marks the FTdx5000 and FTdx9000, whose PC parameter their
	// manuals give as 000-255 where every other model here gives a watt range.
	// Both are 200 W radios, but nothing in either manual calibrates the index
	// against an output, so MaxPowerW is left at zero rather than filled in
	// with the nameplate figure: publishing 200 there would invite a client to
	// read the index as watts. Caps reports PowerWattAccurate false, watt
	// requests are refused rather than converted, and the percentage is a
	// straight fraction of 255 — the same treatment the civ backend gives
	// Icom's 14 0A.
	PowerRaw bool

	// Filter is how SH is spelled on this radio; Widths is what its parameter
	// means. SH takes a table index, not a width in Hz, and the index's meaning
	// depends on the mode. Widths is empty on the FTdx9000, where the parameter
	// is not an index into anything.
	Filter FilterShape
	Widths widths

	// MinHz and MaxHz bound FA/FB. The check is remoses's own: a Yaesu
	// documents no error response, so an out-of-range frequency would not be
	// refused visibly — it would cost a full session timeout.
	MinHz, MaxHz uint64
}

// modeCode is one row of a model's MD table.
type modeCode struct {
	mode radio.Mode
	data bool
}

// FilterShape is how a model spells SH.
//
// All three spellings are read with SH0; and answer with the index in the last
// two characters. They differ in what sits between the command and the index,
// and the FT-991A/FT-891 pair differ from each other despite sharing a
// byte-identical bandwidth table — which is why this is a table rather than a
// family constant.
type FilterShape int

const (
	// FilterShort is SH0<nn>;: six bytes, no middle parameter. The FT-991A and
	// the whole FT-950 generation take it. What the leading 0 means varies —
	// "0: Fixed" on the FT-950, FTdx1200 and FTdx3000, the MAIN/SUB receiver
	// selector on the FTdx5000 and FTdx9000 — but remoses addresses MAIN, so
	// all five are written the same way.
	FilterShort FilterShape = iota
	// FilterNarrowFlag is the FT-891's SH0<n><nn>;, where the middle parameter
	// is the narrow flag rather than a fixed 0. It is the only model that
	// carries the flag inside SH; everywhere else it lives only in NA.
	FilterNarrowFlag
	// FilterFixed is SH<s>0<nn>;: a fixed 0 in the first parameter on the
	// FT-710 and FTdx10, the MAIN/SUB selector on the FTdx101 and FTX-1.
	// remoses addresses MAIN, so both are written SH00<nn>;.
	FilterFixed
)

// modesCommon is the set every Yaesu here has. FSK is what the rigs call RTTY.
func modesCommon() []radio.Mode {
	return []radio.Mode{
		radio.ModeLSB, radio.ModeUSB, radio.ModeCW, radio.ModeCWR,
		radio.ModeAM, radio.ModeFM, radio.ModeFSK, radio.ModeFSKR,
	}
}

func withModes(base []radio.Mode, extra ...radio.Mode) []radio.Mode {
	return append(base, extra...)
}

// codesCommon is the twelve MD codes every model in this file agrees on. The
// three contested ones — A, E and F — are added per model.
//
// Note what B, D and F are: FM-N, AM-N and DATA-FM-N are distinct mode codes,
// while narrow in SSB, CW and RTTY is the separate NA command. They decode to
// the same radio.Mode as their wide siblings, and encoding never picks them —
// see encodeMode.
func codesCommon() map[byte]modeCode {
	return map[byte]modeCode{
		'1': {radio.ModeLSB, false},
		'2': {radio.ModeUSB, false},
		'3': {radio.ModeCW, false}, // CW-U is normal CW
		'4': {radio.ModeFM, false},
		'5': {radio.ModeAM, false},
		'6': {radio.ModeFSK, false}, // RTTY-LSB is normal RTTY
		'7': {radio.ModeCWR, false},
		'8': {radio.ModeLSB, true},
		'9': {radio.ModeFSKR, false},
		'B': {radio.ModeFM, false}, // FM-N
		'C': {radio.ModeUSB, true},
		'D': {radio.ModeAM, false}, // AM-N
	}
}

// codesModern is the table shared by the FT-710, FTdx10, FTdx101D/MP and FTX-1.
// It is not a family default: the FT-991A and FT-891 each have their own, and
// nothing in this package may fall back to this one.
func codesModern() map[byte]modeCode {
	c := codesCommon()
	c['A'] = modeCode{radio.ModeFM, true} // DATA-FM
	c['E'] = modeCode{radio.ModePSK, false}
	c['F'] = modeCode{radio.ModeFM, true} // DATA-FM-N
	return c
}

// codesOlder is the FT-950 and FTdx3000 table: the common twelve plus A.
//
// The older manuals spell the data modes PKT-L, PKT-U and PKT-FM where the
// newer ones say DATA-L, DATA-U and DATA-FM. Same codes, same meaning, so they
// map to the same radio.Mode with the DATA flag — the naming changed, the wire
// did not.
//
// None of the five older radios has an E, so there is no PSK and no C4FM in
// this generation; nor an F, so no DATA-FM-N. What varies among them is which
// of A and D each one has, and that is spelled out per model in the registry.
func codesOlder() map[byte]modeCode {
	c := codesCommon()
	c['A'] = modeCode{radio.ModeFM, true} // PKT-FM
	return c
}

// modern describes an FTdx101-generation radio, which is the shape the rest of
// this backend assumes: the code table above, nine-digit FA/FB, a 100 W PC, the
// seven-byte SH and the FTdx101 bandwidth tables. Models that differ override
// after building.
func modern(name, label string, id int) Model {
	return Model{
		Name:       name,
		Label:      label,
		IDs:        []int{id},
		HasID:      true,
		HasAI:      true,
		HasNarrow:  true,
		Modes:      withModes(modesCommon(), radio.ModePSK),
		Codes:      codesModern(),
		FreqDigits: 9,
		// This generation's AC has "2: -" where the older one has Tuning Start,
		// and puts the start on 3.
		TunerTuneParam: '3',
		MaxPowerW:      100,
		Filter:         FilterFixed,
		Widths:         widthsFTdx101(),
		// The family front end: IPO plus two amplifiers on PA, the 6/12/18 dB
		// ladder on RA, RG for the gain and the five GT settings. The radios
		// with a single pad or a single amplifier override it below.
		Preamp:     2,
		Attenuator: []int{6, 12, 18},
		RFGain:     true,
		AGC:        agcYaesu(),
		// The noise and notch group, which every profiled model has: NB with
		// NL, NR with RL, BP for the manual notch and BC for the automatic one.
		// The antenna is per model and set in the registry below.
		NoiseBlanker:   true,
		NoiseReduction: true,
		Notch:          true,
		AutoNotch:      true,
		MinHz:          30_000,
		MaxHz:          75_000_000,
	}
}

// agcYaesu is the GT map, which is the same across every profiled model.
//
// Note what is NOT here: 5 and 6. The radio ACCEPTS 0 to 4 and ANSWERS 0 to 6,
// because 4 means "choose for me" and the answer says which of fast, mid and
// slow auto currently chose. So the settable values and the readable ones are
// different sets, and radio.AGC carries both — see AGCAutoFast and its
// neighbours, and Model.agcReading below, which decodes them.
func agcYaesu() map[radio.AGC]byte {
	return map[radio.AGC]byte{
		radio.AGCOff: '0', radio.AGCFast: '1', radio.AGCMid: '2',
		radio.AGCSlow: '3', radio.AGCAuto: '4',
	}
}

// agcReading decodes a GT answer, which has three values no set may carry.
func agcReading(b byte) (radio.AGC, bool) {
	switch b {
	case '0':
		return radio.AGCOff, true
	case '1':
		return radio.AGCFast, true
	case '2':
		return radio.AGCMid, true
	case '3':
		return radio.AGCSlow, true
	case '4':
		return radio.AGCAutoFast, true
	case '5':
		return radio.AGCAutoMid, true
	case '6':
		return radio.AGCAutoSlow, true
	}
	return radio.AGCUnknown, false
}

// older describes an FT-950-generation radio: eight-digit FA/FB and with it the
// IF answer its manuals count as 27 characters, the codesOlder table, a 100 W
// PC in watts, and the six-byte SH. Every one of the five overrides something.
//
// ids is variadic because the FTdx1200 answers with two different numbers
// depending on whether the optional FFT-1 unit is fitted, and empty because the
// FTdx9000 has no ID command at all — see Model.HasID, which is set here and
// cleared there.
func older(name, label string, ids ...int) Model {
	return Model{
		Name:       name,
		Label:      label,
		IDs:        ids,
		HasID:      true,
		HasAI:      true,
		HasNarrow:  true,
		Modes:      modesCommon(),
		Codes:      codesOlder(),
		FreqDigits: 8,
		// The FT-950 generation starts a tuning cycle with 2, not 3.
		TunerTuneParam: '2',
		MaxPowerW:      100,
		Filter:         FilterShort,
		// The family front end: IPO plus two amplifiers on PA, the 6/12/18 dB
		// ladder on RA, RG for the gain and the five GT settings. The radios
		// with a single pad or a single amplifier override it below.
		Preamp:     2,
		Attenuator: []int{6, 12, 18},
		RFGain:     true,
		AGC:        agcYaesu(),
		// The noise and notch group, which every profiled model has: NB with
		// NL, NR with RL, BP for the manual notch and BC for the automatic one.
		// The antenna is per model and set in the registry below.
		NoiseBlanker:   true,
		NoiseReduction: true,
		Notch:          true,
		AutoNotch:      true,
		MinHz:          30_000,
		MaxHz:          56_000_000,
	}
}

// models is the registry, keyed by configuration name.
//
// Every field is transcribed from that radio's own CAT Operation Reference
// Manual. Where a model lacks a command the manual simply has no row for it,
// which is why capabilities are recorded here rather than probed: a command a
// Yaesu does not implement is not refused, it is answered with silence, and
// silence costs a full timeout. The ?; busy answer is no help there — it comes
// back from commands the rig knows and will not run just now, not from ones it
// has never heard of.
var models = map[string]Model{
	// generic is the escape hatch for a Yaesu remoses has no profile for. It
	// claims the FTdx101 shape, which four of the profiled models share, and no
	// ID number, so identification never claims a model — though it still asks,
	// because an unprofiled Yaesu answers ID; and the number is worth logging.
	// Its frequency range is the widest any of them has, because refusing a
	// frequency the radio can tune would be worse than letting the rig ignore
	// one it cannot.
	"generic": func() Model {
		m := modern("generic", "generic Yaesu", 0)
		m.IDs = nil
		m.MaxHz = 470_000_000
		return m
	}(),

	// The FT-991A is the outlier that made the code table per model: E is C4FM
	// there and PSK on every newer radio, so it has no PSK at all. Its C4FM is
	// one mode, not the FTX-1's two: DN versus VW is EX menu item 090, a
	// persistent setting orthogonal to MD, and remoses neither reads nor writes
	// EX. So on this radio remoses can see that the operator is in C4FM but not
	// which sub-mode will go out.
	//
	// Its SH is also one parameter shorter than the FT-891's despite an
	// identical bandwidth table — verified on the rendered pages of both
	// manuals precisely because it is so unlikely.
	"ft-991a": func() Model {
		m := modern("ft-991a", "Yaesu FT-991A", 670)
		m.Modes = withModes(modesCommon(), radio.ModeC4FM)
		m.Codes = codesCommon()
		m.Codes['A'] = modeCode{radio.ModeFM, true}
		m.Codes['E'] = modeCode{radio.ModeC4FM, false}
		m.Filter = FilterShort
		m.Widths = widthsFT991A()
		m.MaxHz = 470_000_000 // HF through 70 cm
		// One pad rather than three: its RA is "0: OFF, 1: ON". The depth is
		// not in the CAT reference; 12 dB is this radio's published receiver
		// specification, and it is a label — the byte on the wire is 1 either
		// way. See the same note on the Kenwood single-pad models.
		m.Attenuator = []int{12}
		return m
	}(),

	// The FT-891 is the smallest table here: no A (DATA-FM), no E and no F, so
	// no PSK and no C4FM. It is also the only model whose SH carries the narrow
	// flag itself, and the only one whose manual declines to say which of codes
	// 1 and 2 is LSB — it defers to a BFO menu item. The pairing structure
	// (3/7 CW, 6/9 RTTY, 8/C DATA) makes 1 = LSB the only consistent reading,
	// but it is an inference and wants confirming on hardware.
	"ft-891": func() Model {
		m := modern("ft-891", "Yaesu FT-891", 650)
		m.Modes = modesCommon()
		m.Codes = codesCommon()
		m.Filter = FilterNarrowFlag
		m.Widths = widthsFT991A() // byte-identical to the FT-991A's
		m.MaxHz = 56_000_000
		// And the smallest front end too: its PA is "0: IPO, 1: AMP", one
		// amplifier where the rest of the family has two, and its RA is a
		// single pad. Same note on the 12 dB as the FT-991A's.
		m.Preamp = 1
		m.Attenuator = []int{12}
		return m
	}(),

	// The FT-710 shares the FTdx101's code table and adds the wider bandwidth
	// ladder the FTX-1 also uses. Its RI0; answers a bulk status carrying
	// Hi-SWR and TX-INHIBIT, which is a better PTT source than TX; and worth
	// picking up later.
	"ft-710": func() Model {
		m := modern("ft-710", "Yaesu FT-710", 800)
		m.Widths = widthsFT710()
		return m
	}(),

	// The FTdx10 has a sub-receiver selector on MD but no sub receiver, while
	// its SM and SH take a fixed 0 — so the selector is a property of the
	// command, not of the radio, and a profile cannot derive one from the
	// other. Its AI also works only over the USB CAT port, so a rig reached
	// through the RS-232C jack or a serial-to-TCP bridge is poll-only.
	"ftdx10": modern("ftdx10", "Yaesu FTdx10", 761),

	// The FTdx101D and MP differ only in output power, and unlike two Icoms
	// sharing a bus address they are distinguishable at runtime: 0681 and 0682.
	// Both have a real sub receiver, reached with the 1 selector on MD, SM, SH
	// and NA; remoses addresses MAIN only and reports SubReceiver false.
	// They are also the only Yaesus here with an antenna selector on the bus:
	// "AN ANTENNA NUMBER", three sockets. No other command list read for this
	// backend has an AN row, the FTdx9000's included.
	"ftdx101d": func() Model {
		m := modern("ftdx101d", "Yaesu FTdx101D", 681)
		m.Antennas = 3
		return m
	}(),

	"ftdx101mp": func() Model {
		m := modern("ftdx101mp", "Yaesu FTdx101MP", 682)
		m.MaxPowerW = 200
		m.Antennas = 3
		return m
	}(),

	// The FTX-1 changes more of the wire than any other model here: a 30-byte
	// IF whose fields all shift by two, a PC with a head selector, and mode
	// codes running past F into C4FM-DN and C4FM-VW.
	"ftx-1": func() Model {
		m := modern("ftx-1", "Yaesu FTX-1", 840)
		m.Modes = withModes(modesCommon(), radio.ModePSK, radio.ModeC4FMDN, radio.ModeC4FMVW)
		m.Codes = codesModern()
		m.Codes['H'] = modeCode{radio.ModeC4FMDN, false}
		m.Codes['I'] = modeCode{radio.ModeC4FMVW, false}
		m.PowerHead = true
		// The field head alone is a 10 W radio; the SPA-1 amplifier takes it to
		// 100. Which one is fitted is reported by PC;, so the ceiling is
		// refined at run time — see Rig.maxPowerW.
		m.MaxPowerW = ftx1HeadMaxW
		m.Widths = widthsFT710()
		m.MaxHz = 470_000_000
		// Its RA is a single on/off like the FT-891's, and its PA parameters
		// are band-dependent — "0: IPO (HF/50), 1: AMP1 (HF/50), 2: AMP2
		// (HF/50)" — so the count is the family's two and which of them a given
		// band offers is the radio's business.
		m.Attenuator = []int{12}
		return m
	}(),

	// --- The FT-950 generation: eight-digit FA/FB and a 27-byte IF ----------

	// The FT-950 is the oldest radio here and the one that established the
	// generation: FA14250000; is the worked example in its own manual, eight
	// digits where every FTdx101-generation radio takes nine.
	//
	// Its mode table is the common twelve plus A, which is the largest of the
	// five — the newer FTdx1200 has fewer codes, not more.
	"ft-950": func() Model {
		m := older("ft-950", "Yaesu FT-950", 310)
		m.Widths = widthsFT950()
		return m
	}(),

	// The FTdx1200 is the only radio in the registry with two ID numbers. They
	// are not two variants of the radio: 0582 and 0583 are the same FTdx1200
	// reporting whether the optional FFT-1 unit is fitted, so both have to be
	// accepted or half the population would draw a spurious mismatch warning at
	// every connect.
	//
	// Its mode table is the smallest of the five. A is printed "----" —
	// explicitly unused — so there is no DATA-FM code, and there is no D
	// either, so no AM-N. Its manual is also the one that renames the data
	// modes DATA-LSB and DATA-USB while the FT-950 and FTdx3000 still call the
	// same codes PKT-L and PKT-U.
	"ftdx1200": func() Model {
		m := older("ftdx1200", "Yaesu FTdx1200", 582, 583)
		delete(m.Codes, 'A') // printed "----": explicitly unused
		delete(m.Codes, 'D') // no AM-N
		m.Widths = widthsFTdx1200()
		return m
	}(),

	// The FTdx3000 is the FT-950's table on the FTdx1200's bandwidth ladder,
	// and reaches 60 MHz where the two of them stop at 56.
	"ftdx3000": func() Model {
		m := older("ftdx3000", "Yaesu FTdx3000", 462)
		m.Widths = widthsFTdx1200()
		m.MaxHz = 60_000_000
		return m
	}(),

	// The FTdx5000 is the first radio here with a real second receiver, so its
	// MD, SM, SH, NA and IS all take a MAIN/SUB selector where the FT-950's
	// take a fixed 0. The wire form is identical because remoses addresses MAIN
	// and writes 0 either way; what it changes is the meaning of a frame
	// arriving with a 1, which decodeMD and the SM decoder already discard.
	//
	// Its PC is the first departure from watts in this backend: 000-255, with
	// no calibration anywhere in the manual. See Model.PowerRaw.
	"ftdx5000": func() Model {
		m := older("ftdx5000", "Yaesu FTdx5000", 362)
		delete(m.Codes, 'D') // no AM-N
		m.PowerRaw = true
		// Not 200, which is what the radio makes: PC gives no watt scale, so
		// there is no ceiling to publish. See Model.PowerRaw.
		m.MaxPowerW = 0
		m.Widths = widthsFTdx5000()
		m.MaxHz = 60_000_000
		return m
	}(),

	// The FTdx9000 is the only radio in the registry that cannot be identified.
	// It has no ID command — its manual's command list has no row for one — so
	// there is nothing to cross-check the configuration against, and Init does
	// not send it: a Yaesu answers a command it does not implement with
	// silence, so ID; would cost a full per-command timeout and then fail the
	// connect. FA; is its link check. It does have AI, so it pushes changes
	// like the rest of the family.
	//
	// It has no NA either, and its SH is not a bandwidth at all: the parameter
	// is the WIDTH knob's position, 00 fully anticlockwise to 31 fully
	// clockwise with 16 centred, which no table in the manual converts to Hz.
	// So Widths is empty, Caps reports FilterWidth false, and SetFilterWidth
	// refuses rather than sending a number that would move the knob to an
	// arbitrary place.
	//
	// Its TX answer has a fourth value the others lack — 3, meaning keyed at
	// the rig and by CAT at once — which decodes as transmitting like 1 and 2.
	"ftdx9000": func() Model {
		m := older("ftdx9000", "Yaesu FTdx9000")
		m.HasID = false
		m.HasNarrow = false
		delete(m.Codes, 'D') // no AM-N
		m.PowerRaw = true
		// This one is sold as a 200 W radio and as a 400 W one, and with no ID
		// command there is nothing on the wire to say which is on the desk — so
		// a watt figure would be wrong for one set of owners either way.
		m.MaxPowerW = 0
		m.Widths = widths{}
		m.MaxHz = 60_000_000
		// And no RA either: its command list has no attenuator row, where every
		// other radio here has one. PA, RG and GT are all present.
		m.Attenuator = nil
		return m
	}(),
}

// DefaultModel is used when the configuration names none.
//
// generic rather than a specific radio: this backend has no installed base to
// preserve, and guessing a model would mean guessing a mode table — the one
// thing that must never be guessed here.
const DefaultModel = "generic"

// LookupModel resolves a configuration model name.
//
// Case and hyphens are ignored, so "FT-991A", "ft991a" and "FTDX-10" all work.
// Yaesu is not consistent about hyphens in its own product names — FT-991A but
// FTDX10 — so an operator copying what is printed on the front panel should not
// have to discover which spelling this file happened to choose.
func LookupModel(name string) (Model, error) {
	if strings.TrimSpace(name) == "" {
		name = DefaultModel
	}
	key := modelKey(name)
	for n, m := range models {
		if modelKey(n) == key {
			return m, nil
		}
	}
	return Model{}, fmt.Errorf("yaesu: unknown model %q, want one of %s",
		name, strings.Join(ModelNames(), ", "))
}

// modelKey folds a name for comparison.
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
// profile. Yaesu's ID really does name a model, but a radio remoses has no
// profile for answers with something too, and the operator's configuration is
// the statement of intent this backend acts on.
//
// The scan is over the sorted names rather than the map, because two entries
// could in principle claim one number and an unordered scan would pick either.
func ModelForID(id int) (Model, bool) {
	if id == 0 {
		return Model{}, false
	}
	for _, n := range ModelNames() {
		if m := models[n]; m.knowsID(id) {
			return m, true
		}
	}
	return Model{}, false
}

// knowsID reports whether id is one of the numbers this radio answers with.
func (m Model) knowsID(id int) bool {
	for _, v := range m.IDs {
		if v == id {
			return true
		}
	}
	return false
}

// idList renders this model's ID numbers the way the manuals print them, for
// the mismatch warning. Empty where there are none to name.
func (m Model) idList() string {
	out := make([]string, 0, len(m.IDs))
	for _, v := range m.IDs {
		out = append(out, fmt.Sprintf("%04d", v))
	}
	return strings.Join(out, " or ")
}

// hasFilterWidth reports whether SH means a bandwidth in Hz on this radio.
//
// False only on the FTdx9000, whose SH parameter is the position of the WIDTH
// knob rather than an index into a table of passbands.
func (m Model) hasFilterWidth() bool { return m.Widths.any() }

// supportsMode reports whether this radio accepts m.
func (m Model) supportsMode(x radio.Mode) bool {
	for _, v := range m.Modes {
		if v == x {
			return true
		}
	}
	return false
}

// decodeMode reports the mode and DATA flag an MD character names.
//
// Lower case is accepted because the whole protocol is case-insensitive. A
// character the model's table does not list reports nothing at all rather than
// ModeUnknown: the manuals mark several codes unused, and letting one overwrite
// a good cached mode would be worse than ignoring the frame.
func (m Model) decodeMode(c byte) (radio.Mode, bool, bool) {
	if c >= 'a' && c <= 'z' {
		c -= 32
	}
	v, ok := m.Codes[c]
	return v.mode, v.data, ok
}

// encodeMode is decodeMode's inverse.
//
// The scan is over sorted codes, and that is load-bearing rather than tidiness:
// several modes have two codes, and on every model the lower one is the wide
// variant — 4 FM before B FM-N, 5 AM before D AM-N, A DATA-FM before F
// DATA-FM-N. Yaesu splits narrow off into the separate NA command for SSB, CW
// and RTTY, so emitting the wide code and leaving narrow to NA is the only
// consistent rule. An unordered map scan would pick either at random.
func (m Model) encodeMode(mode radio.Mode, data bool) (byte, error) {
	if mode == radio.ModeUnknown {
		return 0, fmt.Errorf("yaesu: cannot set an unknown mode on the %s", m.Label)
	}
	codes := make([]int, 0, len(m.Codes))
	for c := range m.Codes {
		codes = append(codes, int(c))
	}
	sort.Ints(codes)
	for _, c := range codes {
		v := m.Codes[byte(c)]
		if v.mode == mode && v.data == data {
			return byte(c), nil
		}
	}
	if data {
		return 0, fmt.Errorf("yaesu: the %s has no DATA mode code for %s", m.Label, mode)
	}
	return 0, fmt.Errorf("yaesu: the %s has no mode code for %s", m.Label, mode)
}

// modeSet builds the MD command that selects mode.
//
// P1 is the receiver selector, and it is mandatory even on the single-receiver
// models, where the manual documents it as "0: Fixed". remoses addresses MAIN,
// so it is always 0.
func (m Model) modeSet(mode radio.Mode, data bool) (string, error) {
	if !m.supportsMode(mode) {
		return "", fmt.Errorf("yaesu: the %s does not have mode %s", m.Label, mode)
	}
	code, err := m.encodeMode(mode, data)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("MD0%c;", code), nil
}

// checkFrequency reports why hz cannot go out to this radio, or nil.
//
// The range check exists because nothing else will catch the mistake: no Yaesu
// documents a rejection, so a frequency the radio cannot tune produces silence,
// and silence costs the session's full per-command timeout.
func (m Model) checkFrequency(hz uint64) error {
	if hz < m.MinHz || hz > m.MaxHz {
		return fmt.Errorf("yaesu: %.6f MHz is outside the %s's %.3f-%.3f MHz range",
			float64(hz)/1e6, m.Label, float64(m.MinHz)/1e6, float64(m.MaxHz)/1e6)
	}
	return nil
}

// filterSet builds the SH command that selects a bandwidth index.
//
// The four spellings differ only in the middle, and only the FT-891 puts
// anything meaningful there: its P2 is the narrow flag, where the FT-710 and
// FTdx10 have a fixed 0 and the FTdx101 and FTX-1 have a MAIN/SUB selector that
// remoses always sets to MAIN.
func (m Model) filterSet(index int, narrow bool) string {
	switch m.Filter {
	case FilterShort:
		return fmt.Sprintf("SH0%02d;", index)
	case FilterNarrowFlag:
		n := 0
		if narrow {
			n = 1
		}
		return fmt.Sprintf("SH0%d%02d;", n, index)
	default:
		return fmt.Sprintf("SH00%02d;", index)
	}
}

// filterIndex pulls the bandwidth index out of an SH answer's argument.
//
// The index is the last two characters in every spelling; what varies is how
// much comes before it, so the argument length is checked rather than assumed —
// reading two digits at the wrong offset would report a completely different
// passband. The FT-891's answer also reports the narrow flag, which is the
// other half of choosing a column in the bandwidth table.
func (m Model) filterIndex(arg []byte) (index int, narrow bool, ok bool) {
	want := 4
	if m.Filter == FilterShort {
		want = 3
	}
	if len(arg) != want || arg[0] != '0' {
		return 0, false, false
	}
	if m.Filter == FilterNarrowFlag {
		switch arg[1] {
		case '0':
		case '1':
			narrow = true
		default:
			return 0, false, false
		}
	}
	d := arg[len(arg)-2:]
	if d[0] < '0' || d[0] > '9' || d[1] < '0' || d[1] > '9' {
		return 0, false, false
	}
	return int(d[0]-'0')*10 + int(d[1]-'0'), narrow, true
}
