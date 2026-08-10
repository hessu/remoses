package civ

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hessu/remoses/internal/radio"
)

// Model is what remoses knows about one Icom radio.
//
// CI-V is a family protocol rather than one-command-set-fits-all: the opcodes
// and their encodings are shared, but which commands a given radio implements
// is model-specific, and so is its factory bus address. Naming the model in the
// configuration lets the backend publish honest capabilities, default the
// address correctly, and avoid sending commands the radio will only reject.
//
// Most of the family is uniform enough that a model is a table entry. The
// IC-718 is the reminder that it is not guaranteed: it puts PTT on a different
// sub-command from everything else, has no CW buffer and no IF filter width
// command, and its keyer runs to 60 wpm rather than 48.
//
// What is deliberately NOT per-model:
//
//   - Mode byte values. 0x03 is CW on every Icom; models differ only in which
//     of the shared codes they accept. So there is one code table (mode.go) and
//     each model lists the subset it supports.
//   - Frequency and level encodings. Packed BCD throughout, with the one
//     documented exception of the IC-905's wide frequency field.
type Model struct {
	// Name is the configuration value, lower case and hyphenated.
	Name string
	// Label is for logs and error messages.
	Label string
	// Address is the factory default CI-V bus address. It is only a default:
	// the address is menu-configurable on every one of these radios, which is
	// why config.CIV.RigAddress overrides it and why the address is not a
	// reliable way to identify a model. See Rig.checkIdentity.
	Address byte
	// Modes are the operating modes this radio accepts, in display order.
	Modes []radio.Mode
	// Codes overrides the family mode-byte table. Almost every Icom shares it,
	// but the IC-910H puts FM on 0x04 where the rest have RTTY, so the mapping
	// cannot be assumed. nil means the family table.
	Codes map[byte]radio.Mode
	// FilterSlots is how many IF filter selections the mode command carries
	// (FIL1..FIL3). Zero on radios whose mode command has no filter byte at
	// all, where a trailing byte must not be read as a slot.
	FilterSlots int
	// FilterZeroBased says the mode command's filter byte counts from 0 rather
	// than 1.
	//
	// The modern family numbers its filters FIL1, FIL2, FIL3 and puts 01, 02,
	// 03 on the wire. The IC-706 family instead encodes wide, normal and narrow
	// as 00, 01, 02 — the same three slots offset by one. Getting it wrong
	// selects the neighbouring filter on every mode change, which is a quiet
	// wrongness rather than an error: the rig accepts it and the operator hears
	// a bandwidth they did not ask for.
	FilterZeroBased bool

	// PTT is true when the radio has command 1C at all.
	//
	// It is not a formality. The IC-706 family has no transmitter command in
	// its command set whatsoever — PTT there is the microphone, a footswitch or
	// a control line, and nothing on the bus can key it or report that it is
	// keyed. Claiming otherwise would offer a client a button that does nothing
	// and a state field frozen at false, which is worse than saying so.
	PTT bool
	// PTTSub is the sub-command of 1C that carries transmitter status. It is
	// 0x00 on every radio here except the IC-718, whose manual puts it on 0x01.
	// Wrong here means keying the transmitter with the wrong command, so it is
	// explicit per model rather than assumed.
	PTTSub byte
	// Power is true when the radio has command 14 0A, the RF output level.
	// False on the IC-706 family, whose output is a front-panel control with no
	// bus equivalent.
	Power bool
	// Tuner is true when 1C 01 is the antenna tuner: 00 off, 01 on, 02 start a
	// tuning cycle, and 02 read back while one is running.
	//
	// It is per model for two reasons, and the first would be a transmit
	// accident. On the IC-718, 1C 01 is not the tuner at all — it is PTT, that
	// radio's table having no 1C 00 row — so a "start tuning" sent there would
	// key the transmitter and hold it keyed. Nothing about the frame
	// distinguishes the two; only the model does.
	//
	// The second is that plenty of these radios have no tuner. The IC-9700's
	// table goes 1C 00, 1C 02, 1C 03 and simply omits 01, because there is
	// nothing to address — it is a VHF/UHF set. This started as a default of
	// true for every modern radio and was caught on the air: the IC-9700
	// advertised tuner_control and tuner_tune, answered NG to the poll every
	// slow tick, and would have shown an operator a Tune button that could only
	// ever fail. So it is claimed per model, like everything else here.
	Tuner bool
	// TXMeters is true when the radio has the transmit meters, 15 11 (PO),
	// 15 12 (SWR) and 15 13 (ALC).
	//
	// The three arrived together on the modern radios and are absent together
	// on the older ones: the IC-706MKIIG's 15 stops at 02 and the IC-703's at
	// 13 but with all three present. Recorded as one flag because no reference
	// read so far has any one of them without the others.
	TXMeters bool
	// SWRCal is true when this radio's reference prints the calibration points
	// for 15 12 — "0000=SWR1.0, 0048=SWR1.5, 0080=SWR2.0, 0120=SWR3.0" — so a
	// ratio can be derived from the deflection.
	//
	// Both current references carry the same four points, but the IC-703's
	// manual names the command and stops there. Publishing a ratio for it on
	// the strength of the others would be remoses inventing a figure about
	// somebody's antenna; the bar is still published.
	SWRCal bool
	// POScale is the raw 15 11 reading that means 100% output.
	//
	// It is not the same everywhere and the difference is not cosmetic: the
	// IC-7610's table reads "0000=0% ~ 0143=50% ~ 0255=100%" and the IC-9700's
	// "0000=0% to 0143=50% to 0213=100%". Publishing 213 against a scale of 255
	// would show 84% for a radio at full power.
	POScale int
	// SMeter is true when the radio has command 15 02.
	//
	// The IC-706 and IC-706MKII have no readable meter of any kind; the MKIIG
	// gained one. A meter that is never read publishes a permanent zero, which
	// in a client's signal bar is indistinguishable from a dead band.
	SMeter bool
	// CWBuffer is true when the radio implements command 17, the CAT CW message
	// buffer. Where it is false remoses cannot send Morse over CAT at all and
	// the station must key a serial control line instead.
	CWBuffer bool
	// FilterWidth is true when the radio implements command 1A 03.
	FilterWidth bool
	// DataMode is true when the radio has a data-mode setting under command 1A.
	// It is not universal and not merely absent elsewhere: on the IC-910H the
	// sub-command that carries it elsewhere is RIT on/off, so sending it to a
	// radio without a data mode would change something unrelated rather than
	// draw a rejection.
	DataMode bool
	// DataModeSub is which sub-command of 1A that is. 06 across the modern
	// family, but 04 on the IC-703 — where 06 is not in the table at all and 03
	// is a Set-mode item index rather than the filter width. Same command, a
	// different menu underneath, which is why this cannot be a constant.
	DataModeSub byte
	// DataModeFilter is true where that sub-command carries an IF filter byte
	// after the on/off flag, so that turning data on has to preserve the
	// filter. False on a radio with no filter selection to preserve: the
	// IC-703's 1A 04 is the flag alone, and a second byte would be a parameter
	// its parser is not expecting.
	DataModeFilter bool
	// MaxWPM is the top of the command 14 0C keyer range. The bottom is 6 wpm
	// everywhere.
	MaxWPM int

	// WideFrequency marks a radio whose frequency field grows beyond the usual
	// five bytes. The IC-905 sends and expects six bytes (12 digits, 100 GHz
	// down to 1 Hz) while the 10 GHz band is selected, and five below it.
	WideFrequency bool

	// DualVFO marks a radio with commands 25 and 26, which address one VFO by
	// name instead of operating on whichever is selected: 25 <band> for the
	// frequency, 26 <band> for the mode, data mode and filter together. With
	// them a second VFO can be read and set without disturbing the first;
	// without them the only way to reach it is to select it, which changes what
	// the operator is using.
	//
	// It also implies the 29 prefix, which addresses the inactive band for the
	// commands its table marks — the S-meter and the filter width among them.
	//
	// True only on the IC-7610, because that is the only reference remoses has
	// read that lists them. Several other Icoms very likely have 25 and 26; a
	// capability this backend has not transcribed is one it does not claim, and
	// the cost of guessing wrong here is a command sent to a radio that has
	// never heard of it.
	DualVFO bool
	// DualVFOBandSelector says what the first byte of 25 and 26 means, and
	// getting it wrong addresses a different part of the radio.
	//
	// True on the IC-7610: 00 is the main band and 01 the sub band, two fixed
	// receivers, and the reference titles them so. False on the IC-9700: 00 is
	// the *selected* VFO and 01 the unselected one, both within the main band,
	// and its own note says outright "You cannot set the SUB band frequency".
	//
	// One opcode, two axes — the same shape as the IC-718's 1C 01, and the
	// reason this is a per-model field rather than a constant.
	DualVFOBandSelector bool
	// Split marks command 0F, which reads and sets transmit-on-the-other-VFO.
	// Recorded separately from DualVFO because it is a different command with a
	// different history, and a radio could have one without the other.
	Split bool
	// SubReceiver marks a radio with a second receiver that can listen at the
	// same time as the first. Whether remoses can *read* it is a different
	// question — see DualWatch and the IC-9700's entry.
	SubReceiver bool

	// VFOModeSelect marks command 07 with no sub-command, "select the VFO
	// mode", which is how a radio leaves memory mode.
	//
	// remoses does not model memory mode: it has no channel list, and command
	// 25 answers NG there, so a radio left in MR reports stale readings with no
	// way out. This is the way out, and the only part of memory mode worth
	// implementing — an operator whose rig is on a memory channel wants it back
	// on a VFO, not a memory API.
	//
	// True on the two radios whose references have been read for it. Command 07
	// is very likely universal across Icom; it stays off elsewhere under the
	// same rule as the rest of this table.
	VFOModeSelect bool
	// VFOSelect marks 07 00 and 07 01, which select VFO A and VFO B. Separate
	// from VFOModeSelect because the IC-7610 has the latter and not the former:
	// its two VFOs are fixed receivers with no A/B switch between them.
	VFOSelect bool

	// BreakIn is how command 16 47, the CW break-in setting, is spelled on this
	// radio. See BreakInStyle.
	//
	// It gates whether CW sent over CAT is audible at all. The references carry
	// the same footnote against command 17: a message from the PC is
	// transmitted "if the [TRANSMIT] or an external TX switch is ON, or the
	// Break-in function is ON". With break-in off and nothing keying manually,
	// 17 is accepted and nothing goes out — which is exactly what happened on
	// an IC-9700 the first time this was tried.
	BreakIn BreakInStyle
	// DualWatch marks 07 C0/C1/C2, receiving on both VFOs at once. This is what
	// makes the second VFO a second *receiver* rather than a stored frequency,
	// and so what makes a sub S-meter reading mean anything.
	DualWatch bool
}

// BreakInStyle is how many values command 16 47 takes on a radio.
//
// One command, two vocabularies. Most of the family reads "00=OFF, 01=semi
// break-in, 02=full break-in"; the IC-910H's table says only "Set break-in
// (0=OFF; 1=ON)". Sending 02 to that radio would be an out-of-range parameter
// for a distinction it does not make, and reading its 01 as "semi" would invent
// one — full and semi differ audibly, full being QSK.
type BreakInStyle int

const (
	// BreakInNone is a radio with no 16 47 at all.
	BreakInNone BreakInStyle = iota
	// BreakInSemiFull is the three-value form: off, semi, full.
	BreakInSemiFull
	// BreakInOnOff is the two-value form, which is the IC-910H's. It publishes
	// radio.BreakInOn rather than picking one of semi and full, and accepts a
	// request for either by sending its single "on".
	BreakInOnOff
)

// Mode sets. The common set is what essentially every Icom has; the variants
// differ at the edges.
func modesCommon() []radio.Mode {
	return []radio.Mode{
		radio.ModeLSB, radio.ModeUSB, radio.ModeAM, radio.ModeCW,
		radio.ModeFSK, radio.ModeFM, radio.ModeCWR, radio.ModeFSKR,
	}
}

func withModes(base []radio.Mode, extra ...radio.Mode) []radio.Mode {
	return append(base, extra...)
}

// modern describes a radio that behaves the way the rest of this backend
// assumes: PTT on 1C 00, a CW buffer, an IF filter width command, and a 6-48
// wpm keyer. Models that differ override the fields after building.
func modern(name, label string, addr byte, modes []radio.Mode) Model {
	return Model{
		Name:           name,
		Label:          label,
		Address:        addr,
		Modes:          modes,
		FilterSlots:    3,
		PTT:            true,
		PTTSub:         0x00,
		Power:          true,
		SMeter:         true,
		TXMeters:       true,
		SWRCal:         true,
		POScale:        255,
		CWBuffer:       true,
		FilterWidth:    true,
		DataMode:       true,
		DataModeSub:    subDataMode,
		DataModeFilter: true,
		MaxWPM:         48,
	}
}

// filterByte encodes a 1-based API filter slot as the byte this model's mode
// command expects.
//
// The API counts from 1 throughout, because Caps.FilterSlots is a count. Most
// radios put the same number on the wire — FIL1 is 01 — but the IC-706 family
// counts wide, normal and narrow from zero, so the two differ by one.
func (m Model) filterByte(slot int) byte {
	if m.FilterZeroBased {
		return byte(slot - 1)
	}
	return byte(slot)
}

// filterSlot is the inverse: a wire byte back to a 1-based slot, and false when
// the byte is outside what this model offers.
//
// Rejecting an out-of-range byte matters more than it looks. A radio with no
// filter selection reports FilterSlots 0 and never gets here, but a trailing
// byte in some other reply must not be published as a slot the operator did not
// select.
func (m Model) filterSlot(b byte) (int, bool) {
	slot := int(b)
	if m.FilterZeroBased {
		slot++
	}
	if m.FilterSlots <= 0 || slot < 1 || slot > m.FilterSlots {
		return 0, false
	}
	return slot, true
}

// withTuner marks a model whose 1C 01 is the internal antenna tuner: 00 off,
// 01 on, 02 start a cycle.
//
// It is applied per model rather than defaulted in modern(), and that is worth
// the repetition. A default of true is what this backend shipped for a day, and
// it made the IC-9700 advertise a tuner it has no hardware for — its 1C table
// runs 00, 02, 03 and omits 01 — so every slow poll drew a rejection and a
// client would have been shown a Tune button that could only fail. Written out
// here, each call is a record that somebody read that radio's table.
//
// Which radios: every HF set whose reference has been read, and none of the
// VHF/UHF ones. The IC-905, IC-910H and IC-9700 have no 1C 01 row at all.
func withTuner(m Model) Model {
	m.Tuner = true
	return m
}

// mkiiFamily describes an IC-706 of any generation: what the three share, which
// is a command set with no transmitter control, no power level and no CW
// buffer. See the registry entries for what each adds.
func mkiiFamily(name, label string, addr byte) Model {
	return Model{
		Name:    name,
		Label:   label,
		Address: addr,
		// WFM is the addition, and CW-R and RTTY-R the omission: command 06
		// runs 00 to 06 on these radios and stops, where the rest of the family
		// carries on to 07 and 08. Both differences need the whole table
		// overriding, since a byte that means WFM here means nothing elsewhere.
		Modes: []radio.Mode{
			radio.ModeLSB, radio.ModeUSB, radio.ModeAM, radio.ModeCW,
			radio.ModeFSK, radio.ModeFM, radio.ModeWFM,
		},
		Codes: map[byte]radio.Mode{
			0x00: radio.ModeLSB,
			0x01: radio.ModeUSB,
			0x02: radio.ModeAM,
			0x03: radio.ModeCW,
			0x04: radio.ModeFSK, // the rig calls this RTTY
			0x05: radio.ModeFM,
			0x06: radio.ModeWFM,
		},
		// Three filter selections, counted from zero: 00 wide, 01 normal,
		// 02 narrow. Which of the three a given mode actually offers varies —
		// the manual spells out that some modes have only two — but the rig
		// rejects what it cannot do, and guessing per mode would be inventing a
		// table nothing documents.
		FilterSlots:     3,
		FilterZeroBased: true,
		// No 1C at any sub-command, no 14, no 17. See the registry comment.
		PTT:      false,
		Power:    false,
		SMeter:   false,
		CWBuffer: false,
		// No 1A at all, so neither a filter width nor a data mode.
		FilterWidth: false,
		DataMode:    false,
		// 07 selects VFO mode, and 07 00 / 07 01 select VFO A and B.
		VFOModeSelect: true,
		VFOSelect:     true,
		// 0F 00 and 0F 01 turn split off and on, with no read form printed —
		// the same reading as the IC-703's, and the same decision.
		Split: false,
		// There is no keyer speed command either (no 14), so this is the range
		// the family's own keyer covers rather than anything remoses can set.
		MaxWPM: 0,
	}
}

// models is the registry, keyed by configuration name.
//
// Addresses, mode sets and command availability are transcribed from each
// radio's own CI-V documentation — the standalone CI-V reference guides for the
// current models, and the control-command section of the instruction manual for
// the IC-718, IC-7300, IC-7600 and IC-7700. Every address is a factory default
// that the operator can change in Set mode.
var models = map[string]Model{
	// generic is the escape hatch for an Icom remoses has no profile for. It
	// claims the modes common to essentially every Icom and nothing more, and
	// has no default address, so the configuration must give one.
	//
	// Data mode is deliberately off: on an unidentified radio, 1A 06 might be
	// something else entirely — it is RIT on the IC-910H — and quietly changing
	// an unrelated setting is worse than not offering the feature.
	"generic": func() Model {
		m := modern("generic", "generic Icom", 0, modesCommon())
		m.DataMode = false
		return m
	}(),

	// The IC-7610 is the only radio here with a second receiver remoses can
	// read, and the only one whose reference this backend has read for the
	// dual-VFO commands. Its two VFOs are not the classic A/B pair with a
	// switch between them: both are real receivers, A is always what it
	// receives and transmits on, B joins in under dual watch and takes the
	// transmit under split. So there is no VFO-select operation to offer, only
	// the two flags.
	"ic-7610": func() Model {
		m := modern("ic-7610", "Icom IC-7610", 0x98,
			withModes(modesCommon(), radio.ModePSK, radio.ModePSKR))
		m.DualVFO = true
		m.DualVFOBandSelector = true // 25/26 take 00=MAIN, 01=SUB
		m.Split = true
		m.SubReceiver = true
		m.DualWatch = true
		m.VFOModeSelect = true
		m.BreakIn = BreakInSemiFull
		// "1C 01 Antenna tuner (00=OFF, 01=ON, 02=Tune)". Exercised on the air:
		// switched in and out, and four tuning cycles run on 80 m.
		m.Tuner = true
		// No VFOSelect: its table has no 07 00 / 07 01. The two VFOs are fixed
		// receivers and there is no A/B switch to throw.
		return m
	}(),

	// The MK2 reference lists no PSK in its operating mode table.
	"ic-7300mk2": withTuner(modern("ic-7300mk2", "Icom IC-7300MK2", 0xB6, modesCommon())),

	"ic-7760": withTuner(modern("ic-7760", "Icom IC-7760", 0xB2,
		withModes(modesCommon(), radio.ModePSK, radio.ModePSKR))),

	// VHF/UHF/1.2 GHz: D-STAR instead of PSK. DD is 1200 MHz only, which the
	// radio enforces; remoses does not second-guess the band.
	// The IC-9700 has 25 and 26 like the IC-7610 and means something different
	// by them. There, 00 and 01 select the main and sub *bands*; here they
	// select the *selected and unselected VFO*, and only within the main band —
	// its own reference says "You cannot set the SUB band frequency" and titles
	// both commands "(Only MAIN band)".
	//
	// So remoses addresses the main band's two VFOs and nothing else. The sub
	// band is real, receives independently, and is deliberately left alone: the
	// only way to reach it is `07 D1`, select the sub band, which moves the
	// operator's own focus and fights whoever is holding the dial. A meter
	// reading is not worth grabbing somebody's radio for, so SubReceiver is
	// true, the backend never reads it, and Caps says so.
	//
	// Nor is which VFO is "selected" knowable: 07 00 and 07 01 *set* VFO A and
	// B and nothing reports the current one. Hence relative addressing — see
	// radio.Caps.VFOAddressing. Calling the selected one "A" would be a guess,
	// and wrong half the time.
	"ic-9700": func() Model {
		m := modern("ic-9700", "Icom IC-9700", 0xA2,
			withModes(modesCommon(), radio.ModeDV, radio.ModeDD))
		m.DualVFO = true
		m.DualVFOBandSelector = false // 25/26 take 00=selected, 01=unselected
		m.Split = true
		m.SubReceiver = true
		// No DualWatch: 07 C0/C1/C2 is not in this radio's command table, and
		// its sub band is not the IC-7610's dual watch in any case.
		m.VFOModeSelect = true
		m.VFOSelect = true // 07 00 and 07 01 select VFO A and VFO B
		m.BreakIn = BreakInSemiFull
		// Its PO meter reaches 100% at 213, not the 255 of the IC-7610.
		m.POScale = 213
		// And no antenna tuner: its 1C table is 00, 02, 03, with no 01 row.
		// A VHF/UHF set has nothing for one to match. Confirmed on the radio,
		// which answers NG to 1C 01 in every form.
		m.Tuner = false
		return m
	}(),

	// DD and ATV need 1200 MHz or higher, again enforced by the radio.
	//
	// No withTuner: its 1C table is 00, 02, 03 with no 01 row. A 144 MHz to
	// 10 GHz set has nothing for a matching network to do.
	"ic-905": func() Model {
		m := modern("ic-905", "Icom IC-905", 0xAC,
			withModes(modesCommon(), radio.ModeDV, radio.ModeDD, radio.ModeATV))
		m.WideFrequency = true
		return m
	}(),

	// Like the MK2: no PSK in its mode table.
	"ic-7300": withTuner(modern("ic-7300", "Icom IC-7300", 0x94, modesCommon())),

	"ic-7600": withTuner(modern("ic-7600", "Icom IC-7600", 0x7A,
		withModes(modesCommon(), radio.ModePSK, radio.ModePSKR))),

	"ic-7700": withTuner(modern("ic-7700", "Icom IC-7700", 0x74,
		withModes(modesCommon(), radio.ModePSK, radio.ModePSKR))),

	// The IC-7850 and IC-7851 are the same radio to CI-V and share an address.
	"ic-7850": withTuner(modern("ic-7850", "Icom IC-7850/7851", 0x8E,
		withModes(modesCommon(), radio.ModePSK, radio.ModePSKR))),

	// HF/VHF/UHF with D-STAR: DV instead of PSK, and no DD.
	//
	// It has a tuner despite the VHF and UHF bands, and its table words the
	// tune trigger differently — "Manual tuning selection" rather than
	// "Tuning" — for the same 1C 01 02.
	//
	// The tuner covers HF and 50 MHz only: on 144 MHz and up the radio rejects
	// the command. remoses does not pre-empt that with a frequency test, both
	// because the boundary is the rig's to enforce and because a hard-coded one
	// would be a number invented here. The refusal arrives as an ordinary 422
	// carrying the radio's own NG, which is the truthful answer — but it is
	// worth knowing that a tune failing on 2 m is the radio saying no, not
	// remoses malfunctioning.
	"ic-9100": withTuner(modern("ic-9100", "Icom IC-9100", 0x7C,
		withModes(modesCommon(), radio.ModeDV))),

	// The IC-910H is the second outlier, and the only radio here that does not
	// use the family mode-byte table. From its command table (section 13):
	//
	//   - Command 06 has just four modes, and FM is 04 — where every other
	//     Icom here has RTTY. Decoding that with the family table would report
	//     RTTY for a radio sitting in FM.
	//   - The mode command carries no filter byte, so there are no FIL slots.
	//   - No command 17 (the table jumps 16 to 19), so no CW over CAT.
	//   - No 1A 03: its 1A sub-commands stop at 09.
	//   - Command 14 0C runs 6-60 wpm, like the IC-718.
	//   - No 1C 01, so no antenna tuner: its table ends at 1C 00.
	//
	// PTT is the ordinary 1C 00 here, unlike the IC-718.
	//
	// Its 16 47 is the two-value form: "Set break-in (0=OFF; 1=ON)", where the
	// rest of the family has off, semi and full. So it publishes radio.BreakInOn
	// rather than picking one of the two it cannot tell apart, and a request
	// for either semi or full sends its single "on" — accurate rather than
	// approximate, since they are the same setting on this radio.
	"ic-910h": {
		Name:    "ic-910h",
		Label:   "Icom IC-910H",
		Address: 0x60,
		Modes:   []radio.Mode{radio.ModeLSB, radio.ModeUSB, radio.ModeCW, radio.ModeFM},
		Codes: map[byte]radio.Mode{
			0x00: radio.ModeLSB,
			0x01: radio.ModeUSB,
			0x03: radio.ModeCW,
			0x04: radio.ModeFM,
		},
		FilterSlots: 0,
		PTT:         true,
		PTTSub:      0x00,
		Power:       true,
		SMeter:      true,
		CWBuffer:    false,
		FilterWidth: false,
		BreakIn:     BreakInOnOff,
		MaxWPM:      60,
	},

	// The IC-706 family: three mobiles from the mid-1990s, and the oldest
	// radios in this table by a decade. They share a command set that predates
	// most of what this backend assumes, so mkiiFamily below builds the common
	// part and each entry states what it adds.
	//
	// Their manuals are the thinnest documentation here — the original IC-706's
	// command table is one narrow column — and are incomplete in one direction
	// and wrong in another, so two facts below are stated on the strength of
	// how these radios actually behave rather than what their tables print:
	//
	//   - The IC-706 and IC-706MKII tables list only control commands, stopping
	//     at 10, with no 03 or 04. Both nonetheless answer "read frequency" and
	//     "read mode", which is just as well: remoses fills its state cache by
	//     polling, so a radio it could command but never read would never come
	//     up at all. This is the one place a profile here rests on something the
	//     manual does not say, and it is the difference between a working entry
	//     and no entry.
	//   - The IC-706MKII's data-format diagram prints the transceiver address as
	//     48, the same as the IC-706's, which looks like reused artwork: its
	//     factory address is 4E. The address is menu-configurable in any case,
	//     so `civ.rig_address` settles an argument with a particular radio.
	//
	// What all three genuinely lack is more interesting than what they have.
	// There is NO transmitter command — no 1C at any sub-command — so remoses
	// can neither key these radios nor tell whether they are keyed; PTT is the
	// microphone, a footswitch or a control line. There is no 14 either, so no
	// RF power. And no 17, so no CW over CAT.
	"ic-706": mkiiFamily("ic-706", "Icom IC-706", 0x48),

	// Same command set and the same silences; a different factory address, and
	// the narrow-filter encoding is spelled out as wide/normal/narrow where the
	// IC-706's table says only "add 02 to select narrow IF filters".
	"ic-706mkii": mkiiFamily("ic-706mkii", "Icom IC-706MKII", 0x4E),

	// The MKIIG is the one with a documented command table of the modern shape,
	// and it is the only one of the three that gained anything remoses can use:
	//
	//   15 02  a readable S-meter, where the earlier two have no meter at all
	//   16 47  CW break-in, which matters because these radios have no CAT CW
	//          buffer: an operator keying a control line still needs the rig in
	//          break-in for the transmitter to follow the key
	//   16 02/12/22/42/43/44/46  preamp, AGC, noise blanker, tone, compressor
	//          and VOX, none of which remoses models
	//   19 00  the identity read, so a wrong bus address can be diagnosed
	//
	// It still has no 1C and no 14, so PTT and power remain absent.
	"ic-706mkiig": func() Model {
		m := mkiiFamily("ic-706mkiig", "Icom IC-706MKIIG", 0x58)
		m.SMeter = true
		// Its table lists "16 47 BK-IN setting" and no data values at all —
		// that table has no Data column. Taken as the three-value form, which
		// is the family norm and which fails LOUDLY if wrong: a request for
		// full would send 02 and draw an NG, where guessing the two-value form
		// on a three-value radio would quietly deliver semi to somebody who
		// asked for QSK.
		m.BreakIn = BreakInSemiFull
		return m
	}(),

	// The IC-703 is a 10 W portable of the IC-718's generation, and its command
	// table is the older shape throughout. Transcribed from section 11 of its
	// instruction manual, which is the only CI-V documentation it has.
	//
	// What it does NOT have matters more than what it does, because three of
	// the absent commands are ones this backend would otherwise send blind:
	//
	//   17     no CAT CW buffer at all, so Morse has to be keyed on a control
	//          line — cw.method: serial_key. Same as the IC-718.
	//   1A 03  present, but it is NOT the filter width. Here 1A 03 takes a
	//          two-byte Set-mode item index — 1A 0301 is the confirmation beep,
	//          1A 0305 the CW carrier point — so reading it as a passband would
	//          ask the radio about its beeper, and writing one would change it.
	//   25/26  absent, so no VFO addressing; 07 00 and 07 01 select A and B.
	//
	// Its mode command carries no filter byte either, so FilterSlots is 0 and a
	// trailing byte must never be read as a slot.
	//
	// Data mode is on 1A 04, not the 1A 06 the rest of the family uses, and is
	// the on/off flag alone with no filter byte after it. It is real — the
	// radio has an SSB-D mode with its own Quick set menu — so it is worth
	// having rather than switching off for being spelled differently.
	//
	// Split is deliberately absent. The table lists 0F 00 and 0F 01 to turn it
	// off and on and shows NO read form, while the same table writes "Set/read"
	// against 1C 00 and "Select/read" against 11. Claiming it would give a
	// setting remoses writes and can never read back, which is the failure this
	// backend keeps being bitten by; 0F may well answer a bare read, but that
	// belongs to whoever puts one on the air, not to a guess made here.
	"ic-703": {
		Name:    "ic-703",
		Label:   "Icom IC-703",
		Address: 0x68,
		// The family byte table exactly: 00 LSB, 01 USB, 02 AM, 03 CW,
		// 04 RTTY, 05 FM, 07 CW-R, 08 RTTY-R. No PSK, no D-STAR.
		Modes:       modesCommon(),
		FilterSlots: 0,
		PTT:         true,
		PTTSub:      0x00, // 1C 00, "set/read the transceiver's condition"
		Power:       true, // 14 0A
		SMeter:      true, // 15 02
		// Its table has all three transmit meters — 15 11 RF power, 15 12 SWR,
		// 15 13 ALC — but, unlike the newer references, no calibration for any
		// of them. So the deflections are published and no SWR ratio is, and
		// the PO scale is taken as the family's 255 rather than measured.
		TXMeters: true,
		SWRCal:   false,
		POScale:  255,
		// 1C 01, "set/read antenna tuner condition (0=OFF, 1=ON, 2=Start
		// tuning or while tuning)" — the clearest statement of the command in
		// any of these references, and the one that says a read of 02 means a
		// cycle is running rather than about to start.
		Tuner:    true,
		CWBuffer: false,
		FilterWidth: false,
		DataMode:    true,
		DataModeSub: 0x04,
		// 1A 04 is "(0=OFF, 1=ON)" and nothing else.
		DataModeFilter: false,
		// 16 47, "Break-in (0=OFF; 1=semi break-in; 2=full break-in)". It has
		// no CAT CW buffer for this to gate, but an operator keying a control
		// line still needs the rig in break-in to hear themselves through it.
		BreakIn: BreakInSemiFull,
		// 07 with no sub-command selects VFO mode; 07 00 and 07 01 select VFO A
		// and VFO B. Both are in the table.
		VFOModeSelect: true,
		VFOSelect:     true,
		// 14 0C is the keyer speed and the Quick set menu gives its range as
		// 6 to 60 wpm.
		MaxWPM: 60,
	},

	// The IC-718 is the outlier, and every difference below is from its own
	// command table (Advanced Manual section 5):
	//
	//   - PTT is 1C 01, not 1C 00. Its table has no 1C 00 row at all. That is
	//     also why Tuner stays false here and must: on every other radio 1C 01
	//     is the antenna tuner, so a "start tuning" sent to this one would key
	//     the transmitter and leave it keyed.
	//   - No command 17, so no CW over CAT. Keying needs a serial control line.
	//   - No 1A 03, so no IF filter width.
	//   - Command 14 0C runs 6-60 wpm, not 6-48.
	//   - No FM and no PSK: the mode table stops at 08 (RTTY-R).
	//
	// It also has a "CI-V 731 mode" (1A 05 27) that shortens the frequency
	// field to four bytes. remoses does not enable it, and decoding is length
	// driven, so a radio left in that mode is still read correctly.
	"ic-718": {
		Name:        "ic-718",
		Label:       "Icom IC-718",
		Address:     0x5E,
		Modes:       []radio.Mode{radio.ModeLSB, radio.ModeUSB, radio.ModeAM, radio.ModeCW, radio.ModeFSK, radio.ModeCWR, radio.ModeFSKR},
		FilterSlots: 0, // its mode command carries no filter byte
		PTT:         true,
		PTTSub:      0x01,
		Power:       true,
		SMeter:      true,
		CWBuffer:    false,
		FilterWidth: false,
		MaxWPM:      60,
	},
}

// DefaultModel is used when the configuration names none. It preserves the
// behaviour remoses had before models existed, including the 0x98 address.
const DefaultModel = "ic-7610"

// LookupModel resolves a configuration model name.
func LookupModel(name string) (Model, error) {
	if name == "" {
		name = DefaultModel
	}
	m, ok := models[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return Model{}, fmt.Errorf("civ: unknown model %q, want one of %s",
			name, strings.Join(ModelNames(), ", "))
	}
	return m, nil
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

// ModelForAddress reports the model whose factory address is addr.
//
// Used only to make a mismatch warning more useful — "the rig answers to 0xA2,
// which is the IC-9700's default" — never to choose a model. An address is not
// a model: it is menu-configurable, and two different radios can be set to the
// same value.
func ModelForAddress(addr byte) (Model, bool) {
	for _, m := range models {
		if m.Address != 0 && m.Address == addr {
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
