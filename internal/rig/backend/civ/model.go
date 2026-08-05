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
// and their encodings are shared, but which commands and which operating modes
// a given radio implements is model-specific, and so is its factory bus
// address. Naming the model in the configuration lets the backend publish
// honest capabilities and default the address correctly, and gives later work
// somewhere to hang the differences that do need separate code paths.
//
// What is deliberately NOT per-model:
//
//   - Mode byte values. 0x03 is CW on every Icom; models differ only in which
//     of the shared codes they accept. So there is one code table (mode.go) and
//     each model lists the subset it supports.
//   - Command opcodes. All five models below implement 03/05 (frequency),
//     04/06 (mode), 14 0A (RF power), 15 02 (S-meter), 1A 03 (filter width),
//     1C 00 (PTT), 17 (send CW, 30 characters) and 19 00 (read ID) identically.
//     Verified against each model's CI-V reference guide.
type Model struct {
	// Name is the configuration value, lower case and hyphenated.
	Name string
	// Label is for logs and error messages.
	Label string
	// Address is the factory default CI-V bus address. It is only a default:
	// the address is menu-configurable on every one of these radios, which is
	// why config.CIV.RigAddress overrides it and why address is not a reliable
	// way to identify a model. See Rig.Init.
	Address byte
	// Modes are the operating modes this radio accepts.
	Modes []radio.Mode
	// WideFrequency marks a radio whose frequency field grows beyond the usual
	// five bytes. The IC-905 sends and expects six bytes (12 digits, 100 GHz
	// down to 1 Hz) while the 10 GHz band is selected, and five below it.
	WideFrequency bool
}

// hfModes is the mode set of the HF radios: everything classic, no digital
// voice.
func hfModes(withPSK bool) []radio.Mode {
	m := []radio.Mode{
		radio.ModeLSB, radio.ModeUSB, radio.ModeAM, radio.ModeCW,
		radio.ModeFSK, radio.ModeFM, radio.ModeCWR, radio.ModeFSKR,
	}
	if withPSK {
		m = append(m, radio.ModePSK, radio.ModePSKR)
	}
	return m
}

// models is the registry, keyed by configuration name.
//
// Addresses and mode sets are transcribed from each radio's CI-V reference
// guide. Every one of these is a factory default that the operator can change
// in Set mode.
var models = map[string]Model{
	// generic is the escape hatch for an Icom remoses has no profile for. It
	// claims the modes common to essentially every Icom and nothing more, and
	// has no default address, so the configuration must give one.
	"generic": {
		Name:    "generic",
		Label:   "generic Icom",
		Address: 0,
		Modes:   hfModes(false),
	},
	"ic-7610": {
		Name:    "ic-7610",
		Label:   "Icom IC-7610",
		Address: 0x98,
		Modes:   hfModes(true),
	},
	"ic-7300mk2": {
		Name:  "ic-7300mk2",
		Label: "Icom IC-7300MK2",
		// The MK2 reference lists no PSK or PSK-R in its operating mode table.
		Address: 0xB6,
		Modes:   hfModes(false),
	},
	"ic-7760": {
		Name:    "ic-7760",
		Label:   "Icom IC-7760",
		Address: 0xB2,
		Modes:   hfModes(true),
	},
	"ic-9700": {
		Name:    "ic-9700",
		Label:   "Icom IC-9700",
		Address: 0xA2,
		// VHF/UHF/1.2 GHz: D-STAR instead of PSK. DD is 1200 MHz only, which
		// the radio enforces; remoses does not second-guess the band.
		Modes: append(hfModes(false), radio.ModeDV, radio.ModeDD),
	},
	"ic-905": {
		Name:    "ic-905",
		Label:   "Icom IC-905",
		Address: 0xAC,
		// DD and ATV need 1200 MHz or higher, again enforced by the radio.
		Modes:         append(hfModes(false), radio.ModeDV, radio.ModeDD, radio.ModeATV),
		WideFrequency: true,
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
