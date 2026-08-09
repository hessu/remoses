package rigctld

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The long command names rigctld echoes back in extended-response mode. They
// are the names from the test_table in tests/rigctl_parse.c, not the single
// letters, because that is what the echo line carries.
const (
	cmdGetFreq   = "get_freq"
	cmdSetFreq   = "set_freq"
	cmdGetMode   = "get_mode"
	cmdSetMode   = "set_mode"
	cmdGetPTT    = "get_ptt"
	cmdSetPTT    = "set_ptt"
	cmdGetLevel  = "get_level"
	cmdSetLevel  = "set_level"
	cmdSendMorse = "send_morse"
	cmdStopMorse = "stop_morse"
	cmdDumpState = "dump_state"
	cmdDumpCaps  = "dump_caps"
)

// commands is the set of echoes this backend recognises. A line is treated as
// an echo only if its name is in here, which is what keeps a labelled value
// line ("Frequency: 14074000") from being mistaken for one.
var commands = map[string]bool{
	cmdGetFreq: true, cmdSetFreq: true,
	cmdGetMode: true, cmdSetMode: true,
	cmdGetPTT: true, cmdSetPTT: true,
	cmdGetLevel: true, cmdSetLevel: true,
	cmdSendMorse: true, cmdStopMorse: true,
	cmdDumpState: true, cmdDumpCaps: true,
}

// Correlation keys. The echo line names the command that produced the block, so
// the long command name is the natural key space.
const (
	keyGetFreq   backend.Key = cmdGetFreq
	keySetFreq   backend.Key = cmdSetFreq
	keyGetMode   backend.Key = cmdGetMode
	keySetMode   backend.Key = cmdSetMode
	keyGetPTT    backend.Key = cmdGetPTT
	keySetPTT    backend.Key = cmdSetPTT
	keySendMorse backend.Key = cmdSendMorse
	keyStopMorse backend.Key = cmdStopMorse
	keyDumpState backend.Key = cmdDumpState
	keyDumpCaps  backend.Key = cmdDumpCaps
)

// levelKey extends the key space for get_level and set_level, whose command
// name alone does not say which value the block carries. It is not merely
// tidier: get_level's answer is an unlabelled bare number, so the level name
// from the echo is the only thing that says whether it is a power setting or an
// S-meter reading.
func levelKey(cmd, level string) backend.Key { return backend.Key(cmd + " " + level) }

// The Hamlib level names this backend uses. Spelled as rig_parse_level expects
// them, in upper case.
const (
	levelRFPOWER  = "RFPOWER"
	levelSTRENGTH = "STRENGTH"
	levelKEYSPD   = "KEYSPD"
	// The transmit meters. RFPOWER_METER is a fraction of maximum power and
	// ALC an uncalibrated float, both treated as 0..1; SWR is the odd one, a
	// real standing-wave ratio rather than a deflection, which no other backend
	// here can offer.
	levelRFPOWERMETER = "RFPOWER_METER"
	levelSWR          = "SWR"
	levelALC          = "ALC"
)

func reqGetLevel(name string) string { return "+l " + name + "\n" }

// Value labels from the get_freq, get_mode and get_ptt implementations. They
// come from the arg1/arg2 columns of rigctld's own command table, so they are
// as stable as the command names themselves.
const (
	labelFrequency = "Frequency"
	labelMode      = "Mode"
	labelPassband  = "Passband"
	labelPTT       = "PTT"
)

// passbandNormal is RIG_PASSBAND_NORMAL: ask the rig's backend for the mode's
// default width. See SetMode for why the alternative is worse.
const passbandNormal = 0

// respTerm begins the line that ends every extended-response block.
const respTerm = "RPRT"

// maxBlock bounds one response block.
//
// The session's scanner refuses a token above 64 kB and drops the connection
// when it hits one, so this backend surrenders first. Nothing rigctld sends is
// remotely this large — \dump_caps, the biggest by far, runs to a handful of
// kilobytes — so reaching the limit means the stream has gone wrong, and
// resynchronising beats taking the radio offline.
const maxBlock = 56 << 10

// splitBlocks cuts the stream into whole response blocks: everything from the
// echo line through the RPRT line that closes it.
//
// Resynchronisation is free. Any garbage that precedes an RPRT simply becomes
// part of that block, and Decode reports a block whose first line is not a
// recognised echo as unsolicited, so noise from a daemon starting up or a
// connection joined mid-response costs one ignored frame.
//
// The RPRT test is anchored at the start of a line, which matters: a Morse
// request carries arbitrary text and rigctld echoes it back, so `b RPRT 0`
// would otherwise terminate its own block early.
func splitBlocks(data []byte, atEOF bool) (int, []byte, error) {
	for off := 0; off < len(data); {
		i := bytes.IndexByte(data[off:], '\n')
		if i < 0 {
			break
		}
		end := off + i
		if isTerminator(data[off:end]) {
			// The '\r' of a CRLF terminator is dropped here rather than left
			// for Decode, so that the frame kept in Update.Raw for logging is
			// the block and nothing else.
			return end + 1, bytes.TrimSuffix(data[:end], []byte("\r")), nil
		}
		off = end + 1
	}

	if atEOF {
		// A tail with no RPRT can only be a response cut short by the daemon
		// going away. Drop it rather than report it as a frame.
		return len(data), nil, nil
	}
	if len(data) >= maxBlock {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// isTerminator reports whether a line is the RPRT that closes a block.
func isTerminator(line []byte) bool {
	line = bytes.TrimRight(line, "\r")
	if !bytes.HasPrefix(line, []byte(respTerm)) {
		return false
	}
	rest := line[len(respTerm):]
	return len(rest) == 0 || rest[0] == ' ' || rest[0] == '\t'
}

// block is one parsed response.
type block struct {
	// cmd is the long command name from the echo line, empty when the block
	// carried no recognisable echo.
	cmd string
	// arg is everything the echo repeated back after the colon.
	arg string
	// body is every line between the echo and the RPRT, verbatim and including
	// the empty ones: \dump_state uses an empty line to mean "this rig has no
	// preamps", and dropping it would shift every field after it.
	body []string
	// code is the RPRT value, valid only when haveCode is set.
	code     int
	haveCode bool
}

// parseBlock splits a frame into echo, body and return code.
func parseBlock(frame []byte) block {
	var b block
	lines := splitLines(frame)

	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i < len(lines) {
		if name, arg, ok := parseEcho(lines[i]); ok {
			b.cmd, b.arg = name, arg
			i++
		}
	}
	for ; i < len(lines); i++ {
		if code, ok := parseRPRT(lines[i]); ok {
			b.code, b.haveCode = code, true
			break
		}
		b.body = append(b.body, lines[i])
	}
	return b
}

func splitLines(frame []byte) []string {
	out := strings.Split(string(frame), "\n")
	for i := range out {
		out[i] = strings.TrimRight(out[i], "\r")
	}
	return out
}

// parseEcho recognises the "long_command_name: arguments" line rigctld writes
// before it runs a command.
func parseEcho(line string) (name, arg string, ok bool) {
	name, arg, ok = strings.Cut(line, ":")
	if !ok || !commands[name] {
		return "", "", false
	}
	return name, strings.TrimSpace(arg), true
}

func parseRPRT(line string) (int, bool) {
	if !isTerminator([]byte(line)) {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), respTerm)))
	if err != nil {
		// "RPRT" with no number still closes the block; the code is unknown,
		// which is reported as a failure rather than guessed as success.
		return 0, false
	}
	return code, true
}

// blockCode extracts the RPRT value from a raw frame, for the error path.
func blockCode(frame []byte) (int, bool) {
	b := parseBlock(frame)
	return b.code, b.haveCode
}

// bodyOf returns a frame's body lines, for the capability parsers.
func bodyOf(frame []byte) []string { return parseBlock(frame).body }

// Decode turns one response block into an Update.
//
// It never errors. A block from a command this backend does not send, a value
// that will not parse, a block truncated by the size guard — all come back as
// something the session can apply harmlessly. The one thing it does insist on
// is the RPRT: a block without one, or with a non-zero one, is reported as
// OK=false so that the waiting transaction fails immediately instead of sitting
// out the session's timeout after the daemon has already said no.
//
// Set commands are decoded from their own echo. rigctld repeats the arguments
// it received, so `set_ptt: 1` followed by `RPRT 0` is a complete statement
// that the rig is now transmitting, and State learns it without a second round
// trip. The RPRT is what makes this sound: the echo is printed before the
// command runs, so it is applied only when the command then succeeded.
func (g *Rig) Decode(frame []byte) (backend.Update, error) {
	// The session's scanner reuses its read buffer, so Raw has to own its bytes
	// or a logger holding onto an Update would print the next frame.
	u := backend.Update{Key: backend.KeyUnsolicited, Raw: bytes.Clone(frame)}
	b := parseBlock(u.Raw)
	u.OK = b.haveCode && b.code == 0

	switch b.cmd {
	case cmdGetFreq:
		u.Key = keyGetFreq
		if v, ok := fieldValue(b.body, labelFrequency); ok {
			applyFrequency(&u.Patch, v)
		}

	case cmdSetFreq:
		u.Key = keySetFreq
		if u.OK {
			applyFrequency(&u.Patch, b.arg)
		}

	case cmdGetMode:
		u.Key = keyGetMode
		mode, _ := fieldValue(b.body, labelMode)
		width, _ := fieldValue(b.body, labelPassband)
		g.applyMode(&u.Patch, mode, width)

	case cmdSetMode:
		u.Key = keySetMode
		if u.OK {
			mode, width, _ := strings.Cut(b.arg, " ")
			g.applyMode(&u.Patch, mode, width)
		}

	case cmdGetPTT:
		u.Key = keyGetPTT
		if v, ok := fieldValue(b.body, labelPTT); ok {
			g.applyPTT(&u.Patch, v)
		}

	case cmdSetPTT:
		u.Key = keySetPTT
		if u.OK {
			g.applyPTT(&u.Patch, b.arg)
		}

	case cmdGetLevel:
		name, _, _ := strings.Cut(b.arg, " ")
		u.Key = levelKey(cmdGetLevel, name)
		if v, ok := firstValue(b.body); ok {
			applyLevel(&u.Patch, name, v)
		}

	case cmdSetLevel:
		name, value, _ := strings.Cut(b.arg, " ")
		u.Key = levelKey(cmdSetLevel, name)
		if u.OK {
			applyLevel(&u.Patch, name, value)
		}

	case cmdSendMorse:
		u.Key = keySendMorse
	case cmdStopMorse:
		u.Key = keyStopMorse
	case cmdDumpState:
		u.Key = keyDumpState
	case cmdDumpCaps:
		u.Key = keyDumpCaps
	}

	return u, nil
}

// fieldValue finds a labelled value line such as "Frequency: 14074000".
func fieldValue(body []string, label string) (string, bool) {
	for _, line := range body {
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(name) == label {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// firstValue returns the first non-empty body line, which is how get_level
// answers: a bare number with no label. See the package doc.
func firstValue(body []string) (string, bool) {
	for _, line := range body {
		if s := strings.TrimSpace(line); s != "" {
			return s, true
		}
	}
	return "", false
}

func applyFrequency(p *radio.Patch, s string) {
	// rigctld prints the frequency with %lld, but a set command echoes back
	// whatever the client wrote, and Hamlib's own parser accepts a float there.
	hz, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || hz < 0 {
		return
	}
	v := uint64(hz)
	p.Frequency = &v
}

// applyMode fills in mode, data mode and passband, and remembers the mode token
// for SetFilterWidth.
func (g *Rig) applyMode(p *radio.Patch, token, width string) {
	token = strings.TrimSpace(token)
	if token != "" {
		// Remembered even when remoses cannot name the mode: SetFilterWidth has
		// to be able to hand the token straight back.
		saved := token
		g.modeName.Store(&saved)

		if m, data, ok := decodeMode(token); ok {
			p.Mode = &m
			p.DataMode = &data
		}
	}

	// A width of 0 is RIG_PASSBAND_NORMAL, which is a request, not a
	// measurement. Publishing it would put a 0 Hz passband in State.
	if hz, err := strconv.Atoi(strings.TrimSpace(width)); err == nil && hz > 0 {
		p.PassbandHz = &hz
	}
}

// applyPTT decodes the ptt_t enum: 0 is RIG_PTT_OFF and everything else is a
// flavour of transmitting (1 plain, 2 microphone input, 3 data input).
func (g *Rig) applyPTT(p *radio.Patch, s string) {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return
	}
	on := v != 0
	p.PTT = &on
	// Remembered so the fast poll knows whether to ask for the transmit
	// meters, which mean nothing in receive.
	g.transmitting.Store(on)
}

// applyLevel maps a level value onto State. Only the two levels remoses models
// land here; KEYSPD is deliberately absent, because the keying speed belongs to
// the CW layer and radio.Patch has no home for it.
func applyLevel(p *radio.Patch, name, value string) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case levelRFPOWER:
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return
		}
		pw := powerFromLevel(v)
		p.Power = &pw
	case levelSTRENGTH:
		db, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return
		}
		m := meterFromStrength(db)
		p.SMeter = &m
	case levelRFPOWERMETER:
		// "percentage of maximum power" as a 0..1 fraction, so it becomes a
		// percentage meter rather than a raw deflection: this is the one
		// backend where forward power arrives already scaled.
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return
		}
		m := meterFromFraction(v)
		p.PowerMeter = &m
	case levelALC:
		// Documented only as "arg float", with no range. Every backend that
		// implements it returns a 0..1 fraction, and it is clamped to that, so
		// a rig that does something else pins the bar rather than producing a
		// meter reading larger than its own scale.
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return
		}
		m := meterFromFraction(v)
		p.ALC = &m
	case levelSWR:
		// A ratio — "[0.0 ... infinite]" — not a deflection. So this is the
		// only backend that fills SWRRatio without a calibration table of its
		// own: the rig's Hamlib backend has already done that conversion.
		//
		// A reading below 1.0 is not physical and means the rig reported
		// nothing useful, so it is dropped rather than published as a
		// suspiciously good match.
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || v < 1 {
			return
		}
		p.SWRRatio = &v
		m := meterFromSWR(v)
		p.SWR = &m
	}
}
