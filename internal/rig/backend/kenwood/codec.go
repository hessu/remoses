package kenwood

import (
	"bytes"
	"strconv"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Correlation keys. A Kenwood answer names itself — the reply to FA; is an FA
// frame — so the command letters are the natural key space.
const (
	keyFA backend.Key = "FA"
	keyFB backend.Key = "FB"
	keyMD backend.Key = "MD"
	keyDA backend.Key = "DA"
	keyPC backend.Key = "PC"
	keySM backend.Key = "SM"
	keyFW backend.Key = "FW"
	keyFL backend.Key = "FL"
	keyIF backend.Key = "IF"
	keyKY backend.Key = "KY"
	keyKS backend.Key = "KS"
	keyID backend.Key = "ID"
	keyAI backend.Key = "AI"
	keyTX backend.Key = "TX"
	keyRX backend.Key = "RX"
)

// The three error answers, which carry no clue as to which command provoked
// them. They are given keys of their own so that every transaction can wait on
// "my answer OR a rejection" and fail fast, instead of sitting out the session's
// timeout after the rig has already said no.
//
// The blind spot is inherent to the protocol: a late ?; belonging to an
// abandoned command can knock out the next one. That costs one retry, whereas
// ignoring rejections costs a full timeout every time.
const (
	keyErrSyntax backend.Key = "?" // bad syntax, or refused in the current state
	keyErrComm   backend.Key = "E" // serial overrun or framing error
	keyErrBusy   backend.Key = "O" // data received, processing not completed
)

// errorKeys is appended to every read transaction's want list.
var errorKeys = []backend.Key{keyErrSyntax, keyErrComm, keyErrBusy}

// smeterScale is the full-scale SM reading: the parameter is a count of meter
// dots, 0000 to 0030, not a signal level.
const smeterScale = 30

// Split is a bufio.SplitFunc over the inbound stream.
//
// Kenwood framing is as simple as it gets — everything ends in ';' — so the only
// real work is resynchronisation. A rig that has just been powered on, or a port
// opened mid-frame, emits leading rubbish; since every command begins with an
// ASCII letter, anything before the first letter of a frame can be discarded
// without ambiguity. Empty frames (a stray ';', or ";;") are swallowed here so
// Decode never has to reason about them.
func (k *Rig) Split(data []byte, atEOF bool) (int, []byte, error) {
	return splitFrames(data, atEOF)
}

func splitFrames(data []byte, atEOF bool) (int, []byte, error) {
	adv := 0
	for {
		rest := data[adv:]
		i := bytes.IndexByte(rest, ';')
		if i < 0 {
			if atEOF && len(rest) > 0 {
				// A tail with no terminator can only be a truncated frame from
				// a port that went away. Drop it rather than report it.
				return len(data), nil, nil
			}
			// adv may be non-zero: empty frames already consumed above.
			return adv, nil, nil
		}
		frame := trimFrame(rest[:i])
		adv += i + 1
		if len(frame) == 0 {
			continue
		}
		return adv, frame, nil
	}
}

// trimFrame discards leading bytes that cannot begin a command and trailing
// whitespace. Interior bytes are left alone: IF; legitimately carries spaces,
// and KY text may carry punctuation.
func trimFrame(b []byte) []byte {
	for len(b) > 0 && !isCommandByte(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == '\r' || b[len(b)-1] == '\n' || b[len(b)-1] == 0) {
		b = b[:len(b)-1]
	}
	return b
}

// isCommandByte reports whether c can start a frame: a command letter, or one of
// the three single-character error answers.
func isCommandByte(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '?'
}

// Decode turns one frame from Split into an Update.
//
// It never errors. An unrecognised command, a frame too short for its own
// parameters, a mode digit the rig uses to signal a failed set — all of them
// come back as an Update the session can apply harmlessly. Refusing to decode
// would be worse than useless here, because this same path carries the AI push
// traffic, which is by definition whatever the operator happened to touch.
//
// A frame that is recognised but malformed keeps its key and returns an empty
// patch. Completing the pending transaction with nothing to say beats stalling
// it until the timeout: the value is unchanged in the cache and the next poll
// will fetch it again.
func (k *Rig) Decode(frame []byte) (backend.Update, error) {
	// The session's scanner reuses its read buffer, so Raw has to own its bytes
	// or a logger holding onto an Update would print the next frame.
	u := backend.Update{Key: backend.KeyUnsolicited, OK: true, Raw: bytes.Clone(frame)}
	f := u.Raw

	if len(f) == 0 {
		return u, nil
	}

	// The error answers are one character and no parameters.
	if len(f) == 1 {
		switch f[0] {
		case '?':
			u.Key, u.OK = keyErrSyntax, false
			return u, nil
		case 'E':
			u.Key, u.OK = keyErrComm, false
			return u, nil
		case 'O':
			u.Key, u.OK = keyErrBusy, false
			return u, nil
		}
		return u, nil
	}

	cmd, arg, ok := splitCommand(f)
	if !ok {
		return u, nil
	}

	switch backend.Key(cmd) {
	case keyFA:
		u.Key = keyFA
		if hz, err := parseFrequency(string(arg)); err == nil {
			u.Patch.Frequency = &hz
		}

	case keyFB:
		// VFO B is decoded only far enough to complete a transaction. State
		// carries a single frequency, and this backend treats VFO A as the
		// operating VFO throughout (Poll reads FA;, IF; reports the displayed
		// frequency), so publishing VFO B here would fight with that.
		u.Key = keyFB

	case keyMD:
		u.Key = keyMD
		if len(arg) >= 1 {
			if m, ok := decodeMode(arg[0]); ok {
				u.Patch.Mode = &m
				k.mode.Store(uint32(m))
			}
		}

	case keyDA:
		u.Key = keyDA
		if len(arg) >= 1 && (arg[0] == '0' || arg[0] == '1') {
			on := arg[0] == '1'
			u.Patch.DataMode = &on
			k.dataMode.Store(on)
			if !on {
				// Leaving Data mode makes IF; usable again. Clearing the
				// blocked flag here is what gives a failed bulk poll a natural
				// retry cadence: one slow poll.
				k.ifBlocked.Store(false)
			}
		}

	case keyPC:
		u.Key = keyPC
		if w, err := strconv.Atoi(string(arg)); err == nil && len(arg) == 3 {
			p := powerFromWatts(w, k.lastMode())
			u.Patch.Power = &p
		}

	case keySM:
		u.Key = keySM
		// SM0 + 4 digits. P2 counts meter dots, and reads the RF power meter
		// while transmitting rather than the S-meter, so this single field
		// means two different things depending on PTT. State keeps it in SMeter
		// either way; the scale is the same 30 dots.
		if len(arg) == 5 {
			if n, err := strconv.Atoi(string(arg[1:])); err == nil {
				m := radio.Meter{Raw: n, Scale: smeterScale}
				u.Patch.SMeter = &m
			}
		}

	case keyFW:
		u.Key = keyFW
		if len(arg) == 4 {
			if hz, err := strconv.Atoi(string(arg)); err == nil {
				u.Patch.PassbandHz = &hz
			}
		}

	case keyFL:
		u.Key = keyFL
		if len(arg) >= 1 && (arg[0] == '1' || arg[0] == '2') {
			slot := int(arg[0] - '0')
			u.Patch.FilterSlot = &slot
		}

	case keyIF:
		u.Key = keyIF
		k.decodeIF(&u, f)

	case keyKY:
		// The answer is a buffer-space flag, which says nothing about state:
		// KY0 (space available) does not mean the rig has stopped sending, so
		// deriving CWBusy from it would flap. BufferFree reads Raw instead.
		u.Key = keyKY

	case keyKS:
		// Keyer speed has no home in radio.Patch; the CW layer owns it.
		u.Key = keyKS

	case keyID:
		u.Key = keyID
		if n, err := strconv.Atoi(string(arg)); err == nil {
			k.id.Store(uint32(n))
		}

	case keyAI:
		u.Key = keyAI

	case keyTX, keyRX:
		// With AI on these arrive unsolicited whenever the operator keys up,
		// which is the only way to observe PTT while the rig is in Data mode:
		// IF; is refused there and TX/RX have no read form.
		u.Key = backend.Key(cmd)
		on := cmd == string(keyTX)
		u.Patch.PTT = &on
	}

	return u, nil
}

// splitCommand peels the leading command letters off a frame. Every parameter in
// the commands this backend speaks is digits, spaces or punctuation, so the run
// of letters is unambiguous. A run that is not 2 or 3 long is line noise, and
// the frame is reported unsolicited.
func splitCommand(f []byte) (cmd string, arg []byte, ok bool) {
	n := 0
	for n < len(f) && isLetter(f[n]) {
		n++
	}
	if n < 2 || n > 3 {
		return "", nil, false
	}
	return upperASCII(string(f[:n])), f[n:], true
}

func isLetter(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// upperASCII is strings.ToUpper without the Unicode machinery, which cannot
// apply: commands are ASCII by definition.
func upperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

// Field offsets within an IF answer, zero-based and with the ';' already
// stripped by Split. The reference numbers these 1..38 including the
// terminator; ifLen is therefore 37 here.
const (
	ifFreqStart = 2 // P1, 11 digits
	ifFreqEnd   = 13
	ifRITStart  = 18 // P3, sign plus 4 digits (P2 is five spaces)
	ifRITEnd    = 23
	ifRITOn     = 23 // P4
	ifXITOn     = 24 // P5
	ifMemHundos = 25 // P6
	ifMemChan   = 26 // P7, two digits
	ifTX        = 28 // P8, 0 RX / 1 TX
	ifMode      = 29 // P9, MD digit
	ifFunc      = 30 // P10, FR/FT
	ifScan      = 31 // P11
	ifSplit     = 32 // P12
	ifTone      = 33 // P13
	ifToneFreq  = 34 // P14, two digits
	ifAlwaysNul = 36 // P15
	ifLen       = 37
)

// decodeIF parses the one command worth exploiting on this rig: a single 38-byte
// answer carrying frequency, RX/TX and mode, so the fast poll costs one
// transaction instead of three.
//
// Only the fields State has a home for are extracted. RIT/XIT, memory channel,
// scan, split and tone are read off the wire by the offsets above but discarded,
// since v1 does not model them; the offsets are named so adding one later is a
// two-line change rather than a re-count.
func (k *Rig) decodeIF(u *backend.Update, f []byte) {
	if len(f) != ifLen {
		// Short or long means line corruption. Leave the patch empty; the key
		// still completes the transaction.
		return
	}

	if hz, err := parseFrequency(string(f[ifFreqStart:ifFreqEnd])); err == nil {
		u.Patch.Frequency = &hz
	}

	switch f[ifTX] {
	case '0':
		on := false
		u.Patch.PTT = &on
	case '1':
		on := true
		u.Patch.PTT = &on
	}

	if m, ok := decodeMode(f[ifMode]); ok {
		u.Patch.Mode = &m
		k.mode.Store(uint32(m))
	}

	// A well-formed IF answer is proof that the rig is answering IF; again.
	k.ifBlocked.Store(false)
}
