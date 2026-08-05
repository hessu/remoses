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
	// Modes are the operating modes this radio accepts.
	Modes []radio.Mode

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
	// MaxWPM is the top of the command 14 0C keyer range. The bottom is 6 wpm
	// everywhere.
	MaxWPM int

	// WideFrequency marks a radio whose frequency field grows beyond the usual
	// five bytes. The IC-905 sends and expects six bytes (12 digits, 100 GHz
	// down to 1 Hz) while the 10 GHz band is selected, and five below it.
	WideFrequency bool
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
		PTTSub:      0x00,
		CWBuffer:    true,
		FilterWidth: true,
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
	"generic": modern("generic", "generic Icom", 0, modesCommon()),

	"ic-7610": modern("ic-7610", "Icom IC-7610", 0x98,
		withModes(modesCommon(), radio.ModePSK, radio.ModePSKR)),

	// The MK2 reference lists no PSK in its operating mode table.
	"ic-7300mk2": modern("ic-7300mk2", "Icom IC-7300MK2", 0xB6, modesCommon()),

	"ic-7760": modern("ic-7760", "Icom IC-7760", 0xB2,
		withModes(modesCommon(), radio.ModePSK, radio.ModePSKR)),

	// VHF/UHF/1.2 GHz: D-STAR instead of PSK. DD is 1200 MHz only, which the
	// radio enforces; remoses does not second-guess the band.
	"ic-9700": modern("ic-9700", "Icom IC-9700", 0xA2,
		withModes(modesCommon(), radio.ModeDV, radio.ModeDD)),

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
