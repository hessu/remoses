package yaesubin

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Model is what remoses knows about one radio of this generation.
//
// It is a much thinner type than the ASCII backend's, and that is the finding
// rather than an omission: the four manuals describe the same seventeen
// opcodes, the same five-byte block, the same mode table and the same tuning
// range. Nothing in the wire format varies between an FT-857 and an FT-897D.
//
// So a profile carries the operator's own statement of which radio is on the
// desk — for the label in logs, errors and the API — plus the two things that
// could in principle differ and are worth stating explicitly rather than
// hard-coding: the mode table and the frequency bounds.
//
// There is no ID command anywhere in this generation, so unlike the ASCII
// backend there is nothing to cross-check the configuration against. What the
// operator wrote is all remoses will ever know.
type Model struct {
	// Name is the configuration value, as printed in YaesuBinModels.
	Name string
	// Label is for logs, errors and the API's model string.
	Label string

	// Modes are the modes this radio can be *put into* over CAT, in display
	// order. Deliberately not the same set as Codes: WFM is in the status
	// answer and in no set command, so it decodes and never encodes.
	Modes []radio.Mode
	// Codes is the whole mode table, keyed by the byte the status read
	// answers with. The DATA flag rides along in the value because PKT is a
	// mode code rather than a flag, exactly as DATA-FM is on the newer radios.
	Codes map[byte]modeCode

	// MinHz and MaxHz bound the set-frequency command. The check is remoses's
	// own: these radios document no error response at all, so a frequency they
	// will not take is not refused visibly.
	MinHz, MaxHz uint64
}

// modeCode is one row of the mode table.
type modeCode struct {
	mode radio.Mode
	data bool
}

// codesCommon is the mode table, and it is the same one on all four radios.
//
// The keys are the values the *status read* (opcode 03) answers with, which is
// the larger of the two tables the manuals print: the set command (opcode 07)
// has no 06 and the status answer has no separate DATA rows. Encoding picks
// from the same map, which is safe because every code the set table lacks
// decodes to a mode that also has a settable code — see encodeMode.
//
// Three codes are narrow variants of a mode that already has a code: 82 is
// CW-N, 88 is FM-N. They decode to the same radio.Mode as their wide siblings,
// because the narrowing is a filter rather than a mode, and encoding never
// picks them — see encodeMode for why the scan is ordered.
//
// The two entries worth stating outright:
//
//   - 0C is PKT, which is packet on FM: 1200 bps AFSK or 9600 bps direct FSK,
//     chosen by menu 073. It maps to FM with the DATA flag, which is what the
//     ASCII backend does with its own DATA-FM code, so SetMode(FM, data) tunes
//     it naturally and no special case is needed.
//   - 0A is DIG, which is its own mode here and not a DATA variant of an SSB
//     one. Its DATA flag is false, and that is a decision rather than an
//     oversight: radio.DataMode exists to tell USB from USB-DATA, and there is
//     no plain DIG for a flag to distinguish this from. Leaving it false also
//     keeps the ordinary move — SSB to DIG with nothing said about data mode —
//     working, because the API carries the current flag forward when a request
//     names only a mode. See radio.ModeDIG for what DIG actually is.
//
// The FT-897 and FT-897D manuals print 82 and 88 in their *set* table and omit
// them from their status table, where both FT-857 manuals list them in each.
// remoses decodes them on all four. Nothing else in the four CAT chapters
// differs by so much as a value, both radios have the narrow CW and FM filters
// the codes name, and the omission is on the side that only costs a reading:
// following it would leave an FT-897 in FM-N reporting the mode it was in
// before, which is worse than following an FT-857 manual that says what the
// byte means.
func codesCommon() map[byte]modeCode {
	return map[byte]modeCode{
		0x00: {radio.ModeLSB, false},
		0x01: {radio.ModeUSB, false},
		0x02: {radio.ModeCW, false},
		0x03: {radio.ModeCWR, false},
		0x04: {radio.ModeAM, false},
		0x06: {radio.ModeWFM, false}, // read-only: no set code exists
		0x08: {radio.ModeFM, false},
		0x0A: {radio.ModeDIG, false},
		0x0C: {radio.ModeFM, true},  // PKT
		0x82: {radio.ModeCW, false}, // CW-N
		0x88: {radio.ModeFM, false}, // FM-N
	}
}

// modesCommon is what these radios can be put into over CAT, which is the
// opcode 07 table minus the narrow variants that are the same radio.Mode.
//
// WFM is absent on purpose. It is a real mode of these radios — 76-108 MHz
// broadcast — and the status read reports it, but no set command has a code for
// it, so promising it in Caps would produce a client offering a mode remoses
// would have to refuse.
func modesCommon() []radio.Mode {
	return []radio.Mode{
		radio.ModeLSB, radio.ModeUSB, radio.ModeCW, radio.ModeCWR,
		radio.ModeAM, radio.ModeFM, radio.ModeDIG,
	}
}

// family builds a profile for one of these radios. Every field is the same on
// all four; only the two names differ.
//
// The frequency bounds are the receive range every one of the four
// specification pages prints: 0.1-56 MHz, 76-108, 118-164 and 420-470. Only the
// outer bounds are enforced. The gaps are not, because the manuals say nothing
// about what the radio does with a frequency inside one, and refusing a
// frequency the rig would have tuned is worse than letting it ignore one it
// will not — the read-back after every write reports what it actually did
// either way.
func family(name, label string) Model {
	return Model{
		Name:  name,
		Label: label,
		Modes: modesCommon(),
		Codes: codesCommon(),
		MinHz: 100_000,
		MaxHz: 470_000_000,
	}
}

// models is the registry, keyed by configuration name.
//
// The four radios are two pairs, and the D of each pair adds 60 m transmit in
// the USA rather than anything a CAT command can see. They are separate entries
// because an operator should be able to write what is printed on the front
// panel and see it come back in the API, not because remoses treats them
// differently — nothing here does, and a test asserts as much.
//
// There is deliberately no `generic` here, and no default. `yaesu.model` is a
// single namespace shared with the ASCII backend, which owns that name and
// must keep it: an unnamed Yaesu has to go on meaning what it has always meant.
// Nothing is lost, because a generic entry would be a fifth copy of one
// identical profile — this package is reached only by naming a radio, which is
// what makes the dispatch unambiguous.
//
// The FT-817 and FT-817ND belong to the same family and speak the same
// five-byte CAT. They are not profiled: remoses has no manual for either, and
// every value in this file was transcribed from one. An owner who wants to try
// can name an FT-857, whose CAT chapter is the one this package was written
// from — accepting that the label remoses reports will then say FT-857.
var models = map[string]Model{
	"ft-857":  family("ft-857", "Yaesu FT-857"),
	"ft-857d": family("ft-857d", "Yaesu FT-857D"),
	"ft-897":  family("ft-897", "Yaesu FT-897"),
	"ft-897d": family("ft-897d", "Yaesu FT-897D"),
}

// Handles reports whether name is one of the radios in this package, and so
// whether `backend: yaesu` should be built from here rather than from the ASCII
// backend.
//
// This is the whole of the dispatch. It is a lookup against the registry rather
// than a pattern on the name, because a pattern would have to guess: "FT-891"
// and "FT-897" differ by one character and by an entire protocol.
func Handles(name string) bool {
	_, err := LookupModel(name)
	return err == nil
}

// LookupModel resolves a configuration model name.
//
// Case and hyphens are ignored, so "FT-857D", "ft857d" and "FT857D" all work,
// for the same reason the ASCII backend does it: an operator copying what is
// printed on the radio should not have to discover which spelling this file
// happened to choose.
//
// An empty name is an error rather than a default. Reaching this package at all
// means a model was named and matched; see models.
func LookupModel(name string) (Model, error) {
	key := modelKey(name)
	if key == "" {
		return Model{}, fmt.Errorf("yaesubin: no model named, want one of %s",
			strings.Join(ModelNames(), ", "))
	}
	for n, m := range models {
		if modelKey(n) == key {
			return m, nil
		}
	}
	return Model{}, fmt.Errorf("yaesubin: unknown model %q, want one of %s",
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

// supportsMode reports whether this radio can be put into m over CAT.
func (m Model) supportsMode(x radio.Mode) bool {
	for _, v := range m.Modes {
		if v == x {
			return true
		}
	}
	return false
}

// decodeMode reports the mode and DATA flag a status byte names.
//
// A byte the table does not list reports nothing at all rather than
// ModeUnknown: letting an unrecognised code overwrite a good cached mode would
// be worse than ignoring it. That is not the desynchronisation check — the BCD
// frequency in the same answer is, see decodeFreqMode — so an unknown mode code
// here is treated as a gap in remoses's table, not as evidence of a bad frame.
func (m Model) decodeMode(c byte) (radio.Mode, bool, bool) {
	v, ok := m.Codes[c]
	return v.mode, v.data, ok
}

// encodeMode is decodeMode's inverse.
//
// The scan is over sorted codes, and that is load-bearing rather than tidiness.
// Two modes have two codes each, and in both cases the lower one is the variant
// to send: 02 CW before 82 CW-N, 08 FM before 88 FM-N. Narrow is a filter
// selection on these radios — the front panel's [B]/[C] keys pick the optional
// YF-122 filters — not something remoses should assert as part of a mode
// change, so emitting the wide code is the only defensible rule. An unordered
// map scan would pick either at random.
//
// It also cannot emit 06: WFM is not in Modes, so modeSet refuses before
// getting here.
func (m Model) encodeMode(mode radio.Mode, data bool) (byte, error) {
	if mode == radio.ModeUnknown {
		return 0, fmt.Errorf("yaesubin: cannot set an unknown mode on the %s", m.Label)
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
		return 0, fmt.Errorf("yaesubin: the %s has no DATA mode code for %s; "+
			"its data modes are DIG and PKT, and PKT is FM with data mode on: %w",
			m.Label, mode, backend.ErrUnsupported)
	}
	return 0, fmt.Errorf("yaesubin: the %s has no mode code for %s: %w",
		m.Label, mode, backend.ErrUnsupported)
}

// modeSet reports the opcode 07 parameter that selects mode.
func (m Model) modeSet(mode radio.Mode, data bool) (byte, error) {
	if !m.supportsMode(mode) {
		if mode == radio.ModeWFM {
			return 0, fmt.Errorf("yaesubin: the %s reports WFM but cannot be put into it over CAT: "+
				"its mode-set table has no code for wide FM: %w", m.Label, backend.ErrUnsupported)
		}
		return 0, fmt.Errorf("yaesubin: the %s does not have mode %s: %w",
			m.Label, mode, backend.ErrUnsupported)
	}
	return m.encodeMode(mode, data)
}

// checkFrequency reports why hz cannot go out to this radio, or nil.
//
// The range check exists because nothing else will catch the mistake. These
// radios document no rejection of any kind, so an impossible frequency is
// answered the same way a good one is — see the package doc on the
// acknowledgement — and the error would surface only as a read-back that did
// not move.
func (m Model) checkFrequency(hz uint64) error {
	if hz < m.MinHz || hz > m.MaxHz {
		return fmt.Errorf("yaesubin: %.6f MHz is outside the %s's %.3f-%.3f MHz range: %w",
			float64(hz)/1e6, m.Label, float64(m.MinHz)/1e6, float64(m.MaxHz)/1e6,
			backend.ErrUnsupported)
	}
	return nil
}

// normaliseModel folds the configured model string into the form used in
// messages. A name the registry does not know is passed through: this is only
// ever a display string, and New has already refused to build a backend for an
// unknown model.
func normaliseModel(s string) string {
	if m, err := LookupModel(s); err == nil {
		return m.Label
	}
	return strings.TrimSpace(s)
}
