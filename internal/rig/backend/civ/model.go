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

	// PTTSub is the sub-command of 1C that carries transmitter status. It is
	// 0x00 on every radio here except the IC-718, whose manual puts it on 0x01.
	// Wrong here means keying the transmitter with the wrong command, so it is
	// explicit per model rather than assumed.
	PTTSub byte
	// CWBuffer is true when the radio implements command 17, the CAT CW message
	// buffer. Where it is false remoses cannot send Morse over CAT at all and
	// the station must key a serial control line instead.
	CWBuffer bool
	// FilterWidth is true when the radio implements command 1A 03.
	FilterWidth bool
	// DataMode is true when sub-command 06 of 1A is the data-mode setting.
	// It is not universal and not merely absent elsewhere: on the IC-910H the
	// same sub-command is RIT on/off, so sending it to a radio without a data
	// mode would change something unrelated rather than draw a rejection.
	DataMode bool
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
	// DualWatch marks 07 C0/C1/C2, receiving on both VFOs at once. This is what
	// makes the second VFO a second *receiver* rather than a stored frequency,
	// and so what makes a sub S-meter reading mean anything.
	DualWatch bool
}

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
		Name:        name,
		Label:       label,
		Address:     addr,
		Modes:       modes,
		FilterSlots: 3,
		PTTSub:      0x00,
		CWBuffer:    true,
		FilterWidth: true,
		DataMode:    true,
		MaxWPM:      48,
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
		return m
	}(),

	// The MK2 reference lists no PSK in its operating mode table.
	"ic-7300mk2": modern("ic-7300mk2", "Icom IC-7300MK2", 0xB6, modesCommon()),

	"ic-7760": modern("ic-7760", "Icom IC-7760", 0xB2,
		withModes(modesCommon(), radio.ModePSK, radio.ModePSKR)),

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
		return m
	}(),

	// DD and ATV need 1200 MHz or higher, again enforced by the radio.
	"ic-905": func() Model {
		m := modern("ic-905", "Icom IC-905", 0xAC,
			withModes(modesCommon(), radio.ModeDV, radio.ModeDD, radio.ModeATV))
		m.WideFrequency = true
		return m
	}(),

	// Like the MK2: no PSK in its mode table.
	"ic-7300": modern("ic-7300", "Icom IC-7300", 0x94, modesCommon()),

	"ic-7600": modern("ic-7600", "Icom IC-7600", 0x7A,
		withModes(modesCommon(), radio.ModePSK, radio.ModePSKR)),

	"ic-7700": modern("ic-7700", "Icom IC-7700", 0x74,
		withModes(modesCommon(), radio.ModePSK, radio.ModePSKR)),

	// The IC-7850 and IC-7851 are the same radio to CI-V and share an address.
	"ic-7850": modern("ic-7850", "Icom IC-7850/7851", 0x8E,
		withModes(modesCommon(), radio.ModePSK, radio.ModePSKR)),

	// HF/VHF/UHF with D-STAR: DV instead of PSK, and no DD.
	"ic-9100": modern("ic-9100", "Icom IC-9100", 0x7C,
		withModes(modesCommon(), radio.ModeDV)),

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
	//
	// PTT is the ordinary 1C 00 here, unlike the IC-718.
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
		PTTSub:      0x00,
		CWBuffer:    false,
		FilterWidth: false,
		MaxWPM:      60,
	},

	// The IC-718 is the outlier, and every difference below is from its own
	// command table (Advanced Manual section 5):
	//
	//   - PTT is 1C 01, not 1C 00. Its table has no 1C 00 row at all.
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
		PTTSub:      0x01,
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
