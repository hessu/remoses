package rigctld

import (
	"slices"
	"strconv"
	"strings"

	"github.com/hessu/remoses/internal/radio"
)

// This file reads the two capability dumps rigctld offers.
//
// \dump_state is a machine-readable, positional block whose layout is fixed by
// declare_proto_rig(dump_state) in tests/rigctl_parse.c and read back by
// Hamlib's own netrigctl client in rigs/dummy/netrigctl.c. Both were consulted;
// the field order below is theirs.
//
// \dump_caps is the human-readable dump from tests/dumpcaps.c, "Label:\tvalue"
// throughout. It is parsed only for the handful of flags that have no
// machine-readable equivalent — above all "Can send Morse", which is the only
// statement in the whole protocol about whether the rig has a CW buffer.

// --- \dump_state ------------------------------------------------------------

// freqRange is one line of the rx or tx range list.
//
// Every column is named and kept even though Caps uses only the mode mask: the
// row is positional, so all seven have to be counted off anyway, and naming
// them makes reaching for the band edges or the power range later a two-line
// change rather than a re-count.
type freqRange struct {
	startHz, endHz   float64
	modes            uint64
	lowPowerMW       int
	highPowerMW      int
	vfoMask, antMask uint64
}

// modeVal is one "mode mask, value" line: a tuning step or a filter width.
type modeVal struct {
	modes uint64
	value int64
}

// dumpState is as much of \dump_state as remoses has a use for.
type dumpState struct {
	// complete is true when the whole positional block parsed. A partial parse
	// is kept rather than discarded — a rig list that stopped halfway still
	// names real modes — but the fields after the break are zero, and zero here
	// means "not reported", which is what Caps then advertises.
	complete bool

	protocol int
	model    int

	rxRanges []freqRange
	txRanges []freqRange
	filters  []modeVal

	getLevel uint64
	setLevel uint64

	// settings holds the protocol-1 "key=value" tail. It is present only when
	// some client on this daemon has run \chk_vfo, which sets the static flag
	// that gates it (chk_vfo_executed in rigctl_parse.c). remoses does not run
	// \chk_vfo — it is the one command that answers without an RPRT line, so it
	// would not fit the framing — and treats the tail as a bonus when another
	// client has happened to unlock it.
	settings map[string]string
}

// Level bit positions from include/hamlib/rig.h. The has_get_level and
// has_set_level masks in \dump_state are bitfields over these.
const (
	bitRFPOWER      = 12
	bitKEYSPD       = 14
	bitSWR          = 28
	bitALC          = 29
	bitSTRENGTH     = 30
	bitRFPOWERMETER = 32
)

var levelBits = map[string]uint{
	levelRFPOWER:      bitRFPOWER,
	levelKEYSPD:       bitKEYSPD,
	levelSWR:          bitSWR,
	levelALC:          bitALC,
	levelSTRENGTH:     bitSTRENGTH,
	levelRFPOWERMETER: bitRFPOWERMETER,
}

func (s *dumpState) hasGetLevel(name string) bool { return hasBit(s.getLevel, levelBits[name]) }

func (s *dumpState) hasSetLevel(name string) bool { return hasBit(s.setLevel, levelBits[name]) }

func hasBit(mask uint64, bit uint) bool { return mask&(1<<bit) != 0 }

// modeMask is every mode the rig can receive or transmit.
func (s *dumpState) modeMask() uint64 {
	var m uint64
	for _, r := range s.rxRanges {
		m |= r.modes
	}
	for _, r := range s.txRanges {
		m |= r.modes
	}
	return m
}

// modeTokens names the modes the rig has, in Hamlib's own vocabulary.
func (s *dumpState) modeTokens() []string { return tokensFromMask(s.modeMask()) }

// keyerRange reads the KEYSPD entry out of level_gran.
//
// The entry is often 0,0,0: level_gran is filled in per rig backend and plenty
// of them leave it empty, so a zero maximum means "not stated" rather than "no
// keyer", and the caller keeps its defaults.
func (s *dumpState) keyerRange() (lo, hi int, ok bool) {
	gran, present := s.settings["level_gran"]
	if !present {
		return 0, 0, false
	}
	for _, entry := range strings.Split(gran, ";") {
		idx, spec, found := strings.Cut(entry, "=")
		if !found || strings.TrimSpace(idx) != strconv.Itoa(bitKEYSPD) {
			continue
		}
		parts := strings.Split(spec, ",")
		if len(parts) < 2 {
			return 0, 0, false
		}
		lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || hi <= lo || hi == 0 {
			return 0, 0, false
		}
		return lo, hi, true
	}
	return 0, 0, false
}

// lineReader walks the positional block.
type lineReader struct {
	lines []string
	i     int
}

func (r *lineReader) next() (string, bool) {
	if r.i >= len(r.lines) {
		return "", false
	}
	s := r.lines[r.i]
	r.i++
	return s, true
}

// parseDumpState reads the body of a \dump_state response.
//
// It is written to stop rather than fail. This backend exists for rigs nobody
// has tested, talking to a daemon whose version is unknown, and Hamlib has
// already extended this block once; a parser that gave up on the first
// surprise would throw away the mode list because a later field grew a
// decimal point.
func parseDumpState(body []string) dumpState {
	st := dumpState{settings: map[string]string{}}
	r := &lineReader{lines: body}

	var ok bool
	if st.protocol, ok = readInt(r); !ok {
		return st
	}
	if st.model, ok = readInt(r); !ok {
		return st
	}
	if _, ok = readInt(r); !ok { // ITU region, deprecated and always 0
		return st
	}
	if st.rxRanges, ok = readRanges(r); !ok {
		return st
	}
	if st.txRanges, ok = readRanges(r); !ok {
		return st
	}
	if _, ok = readModeVals(r); !ok { // tuning steps
		return st
	}
	if st.filters, ok = readModeVals(r); !ok {
		return st
	}
	for range 4 { // max_rit, max_xit, max_ifshift, announces
		if _, ok = readInt(r); !ok {
			return st
		}
	}
	for range 2 { // preamp and attenuator lists, either of which may be empty
		if _, ok = r.next(); !ok {
			return st
		}
	}
	for range 2 { // has_get_func, has_set_func
		if _, ok = readMask(r); !ok {
			return st
		}
	}
	if st.getLevel, ok = readMask(r); !ok {
		return st
	}
	if st.setLevel, ok = readMask(r); !ok {
		return st
	}
	for range 2 { // has_get_parm, has_set_parm
		if _, ok = readMask(r); !ok {
			return st
		}
	}
	st.complete = true

	// Whatever follows is the protocol-1 "key=value" tail, terminated by a
	// bare "done".
	for {
		line, more := r.next()
		if !more || strings.TrimSpace(line) == "done" {
			break
		}
		if k, v, found := strings.Cut(line, "="); found {
			st.settings[strings.TrimSpace(k)] = v
		}
	}
	return st
}

func readInt(r *lineReader) (int, bool) {
	line, ok := r.next()
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(firstField(line), 64)
	if err != nil {
		return 0, false
	}
	return int(v), true
}

// readMask reads one of the has_* bitfields. Hamlib prints them as "0x..." and
// its own client reads them with strtoll base 0, so base 0 is used here too.
func readMask(r *lineReader) (uint64, bool) {
	line, ok := r.next()
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseUint(firstField(line), 0, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// readRanges reads a frequency range list up to its zero sentinel.
//
// The bounds are printed with the "%lf" that FREQFMT expands to, so they arrive
// as "1800000.000000" and not as integers — while the sentinel line is written
// separately as seven plain zeros. Parsing both as floats covers the pair.
func readRanges(r *lineReader) ([]freqRange, bool) {
	var out []freqRange
	for {
		line, ok := r.next()
		if !ok {
			return out, false
		}
		f := strings.Fields(line)
		if len(f) < 7 {
			return out, false
		}
		var fr freqRange
		var err error
		if fr.startHz, err = strconv.ParseFloat(f[0], 64); err != nil {
			return out, false
		}
		if fr.endHz, err = strconv.ParseFloat(f[1], 64); err != nil {
			return out, false
		}
		if fr.startHz == 0 && fr.endHz == 0 {
			return out, true // RIG_IS_FRNG_END
		}
		fr.modes, _ = strconv.ParseUint(f[2], 0, 64)
		fr.lowPowerMW, _ = strconv.Atoi(f[3])
		fr.highPowerMW, _ = strconv.Atoi(f[4])
		fr.vfoMask, _ = strconv.ParseUint(f[5], 0, 64)
		fr.antMask, _ = strconv.ParseUint(f[6], 0, 64)
		out = append(out, fr)
	}
}

// readModeVals reads a "mode mask, value" list up to its "0 0" sentinel.
func readModeVals(r *lineReader) ([]modeVal, bool) {
	var out []modeVal
	for {
		line, ok := r.next()
		if !ok {
			return out, false
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return out, false
		}
		modes, err1 := strconv.ParseUint(f[0], 0, 64)
		value, err2 := strconv.ParseInt(f[1], 10, 64)
		if err1 != nil || err2 != nil {
			return out, false
		}
		if modes == 0 && value == 0 {
			return out, true
		}
		out = append(out, modeVal{modes: modes, value: value})
	}
}

func firstField(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// --- \dump_caps -------------------------------------------------------------

// dumpCaps is the handful of flags worth taking from the human-readable dump.
type dumpCaps struct {
	// present is false when the dump was never fetched or never parsed, which
	// is the state every field below must be read against: false then means
	// "unknown", and Caps reports unknown as absent.
	present bool

	mfgName   string
	modelName string

	sendMorse bool
	stopMorse bool
	setMode   bool
	getMode   bool
	setPTT    bool
	getPTT    bool
}

// modelLabel is the rig as a person would name it, manufacturer first.
func (d *dumpCaps) modelLabel() string {
	return strings.TrimSpace(strings.TrimSpace(d.mfgName) + " " + strings.TrimSpace(d.modelName))
}

// parseDumpCaps picks the flags out of the dump.
//
// Every line of interest is "Label:\tvalue"; the rest of the dump — frequency
// ranges, level lists, the indented per-range detail — is skipped because its
// labels are not in the switch. Values are 'Y', 'N', or 'E' for a capability
// Hamlib emulates out of other calls; 'E' counts as available, since from the
// client's side an emulated set_mode is a set_mode.
func parseDumpCaps(body []string) dumpCaps {
	d := dumpCaps{present: true}
	for _, line := range body {
		label, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		label = strings.TrimSpace(label)
		value = strings.TrimSpace(value)
		switch label {
		case "Mfg name":
			d.mfgName = value
		case "Model name":
			d.modelName = value
		case "Can send Morse":
			d.sendMorse = yes(value)
		case "Can stop Morse":
			d.stopMorse = yes(value)
		case "Can set Mode":
			d.setMode = yes(value)
		case "Can get Mode":
			d.getMode = yes(value)
		case "Can set PTT":
			d.setPTT = yes(value)
		case "Can get PTT":
			d.getPTT = yes(value)
		}
	}
	return d
}

func yes(v string) bool { return v == "Y" || v == "E" }

// --- radio.Caps -------------------------------------------------------------

// modeOrder fixes the order Caps.Modes is published in, so the same rig always
// produces the same list. Bit order would work too, but it puts AM first, and a
// client rendering a mode picker in wire-bit order is not what anyone wants.
var modeOrder = []radio.Mode{
	radio.ModeLSB, radio.ModeUSB, radio.ModeCW, radio.ModeCWR,
	radio.ModeAM, radio.ModeFM, radio.ModeFSK, radio.ModeFSKR,
	radio.ModePSK, radio.ModePSKR,
}

// buildCaps assembles radio.Caps from the two dumps.
//
// Every field here follows one rule: report a capability only where something
// said so. This backend is the one remoses ships for radios it has never seen,
// and a client that hides a control the rig had is a mild annoyance, whereas
// one that offers a control the rig will reject looks like a broken radio.
func buildCaps(st *dumpState, dc *dumpCaps, minWPM, maxWPM int) radio.Caps {
	c := radio.Caps{
		// Current-VFO only. rigctld addresses a named VFO only when it was
		// started with -o, which changes the wire protocol; see the package doc.
		VFOs: []radio.VFO{radio.VFOCurrent},

		// Both true: `T`/`t` and the RFPOWER level are part of every rigctld,
		// and this backend cannot tell which of the rigs behind it lacks the
		// underlying command. Reporting false would disable the controls for
		// every rig rather than the few that cannot use them.
		PTTControl:   true,
		PowerControl: true,
		// Unlike the two above, these are reported per rig rather than assumed:
		// dump_state's has_get_level mask says which meters the backend behind
		// this daemon implements, and most implement none of them.
		PowerMeter: st.hasGetLevel(levelRFPOWERMETER),
		SWRMeter:   st.hasGetLevel(levelSWR),
		ALCMeter:   st.hasGetLevel(levelALC),
		// RFPOWER is a 0..1 fraction. See powerFromLevel for why no watt figure
		// is derived from it, even though the tx range list carries one.
		PowerWattAccurate: false,

		// Hamlib has no filter-slot concept at all.
		FilterSlots: 0,

		// The sub receiver would need VFO mode to be addressable.
		SubReceiver: false,

		CWMethod: radio.CWNone,
	}

	seen := map[radio.Mode]bool{}
	for _, token := range st.modeTokens() {
		if m, _, ok := decodeMode(token); ok {
			seen[m] = true
		}
	}
	for _, m := range modeOrder {
		if seen[m] {
			c.Modes = append(c.Modes, m)
		}
	}
	// Caps is published by value but its slices are not, and every caller gets
	// the same backing array. Clipping the spare capacity means a caller that
	// appends to its copy allocates instead of writing into everyone else's.
	c.Modes = slices.Clip(c.Modes)

	// The passband is only ever set as the second argument of set_mode, so
	// without set_mode there is no way to change it.
	c.FilterWidth = dc.setMode

	if st.hasGetLevel(levelSTRENGTH) {
		c.SMeterScale = sMeterScale
	}

	// CW over CAT is claimed only where \dump_caps said the rig's Hamlib
	// backend implements send_morse. A rig without it answers `b` with
	// RPRT -11, and a client that had been told CW was available would have no
	// way to explain that to the operator.
	if dc.sendMorse {
		c.CWMethod = radio.CWViaCAT
		c.CWCharset = Charset
		c.CWMinWPM = minWPM
		c.CWMaxWPM = maxWPM
		if !st.hasSetLevel(levelKEYSPD) {
			// The rig will key, but not at a speed remoses can choose. Saying
			// so through a collapsed range is better than advertising a speed
			// control that does nothing.
			c.CWMinWPM, c.CWMaxWPM = 0, 0
		}
	}

	return c
}
