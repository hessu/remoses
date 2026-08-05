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
	"generic": md("generic", "generic Kenwood", 0),

	// The TS-480 predates DATA mode as a CAT concept: it has no DA command, so a
	// DATA request has to be refused rather than approximated. Its S-meter is
	// also the odd one out at 20 dots, and it offers no IF filter selection over
	// CAT. PC runs 005-100.
	"ts480": func() Model {
		m := md("ts480", "TS-480", 20)
		m.DataMode = DataModeNone
		m.SMeterScale = 20
		m.FilterSelect = FilterSelectNone
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
		return m
	}(),

	// The TS-990S is the only 200 W radio in the family, which the percentage
	// scale has to follow: 100 W is half power there and full power everywhere
	// else.
	"ts990s": func() Model {
		m := om("ts990s", "TS-990S", 22)
		m.MaxPowerW = 200
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
