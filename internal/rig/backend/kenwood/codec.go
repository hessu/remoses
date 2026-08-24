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
	keyFR backend.Key = "FR"
	keyFT backend.Key = "FT"
	keyMD backend.Key = "MD"
	keyOM backend.Key = "OM"
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
	keyBI backend.Key = "BI"
	keyVX backend.Key = "VX"
	keySD backend.Key = "SD"
	keyRM backend.Key = "RM"
	keyAC backend.Key = "AC"
	keyPS backend.Key = "PS"
	// The receive front end. GC and GT are both here because the AGC speed
	// moved between them: GT carries it on a TS-480 and the time constant on
	// everything since, where GC carries the speed. See Model.AGCCmd.
	keyPA backend.Key = "PA"
	keyRA backend.Key = "RA"
	keyRG backend.Key = "RG"
	keyGC backend.Key = "GC"
	keyGT backend.Key = "GT"
	// The noise processing, the notches and the antenna.
	keyNB backend.Key = "NB"
	keyNL backend.Key = "NL"
	keyNR backend.Key = "NR"
	keyRL backend.Key = "RL"
	keyNT backend.Key = "NT"
	keyBP backend.Key = "BP"
	keyAN backend.Key = "AN"
	// The transmit audio chain. One key covers PR and PR0 both: splitCommand
	// takes the run of LETTERS as the command, so a TS-890S's PR01 answer
	// arrives here under the same key as a TS-590's PR1, with the extra digit
	// left in the argument. Model.procSwitchChar is what tells the two apart.
	keyMG backend.Key = "MG"
	keyPR backend.Key = "PR"
	keyPL backend.Key = "PL"
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
	case keyFA, keyFB:
		// Each names its own VFO, so both are published into that VFO's slot.
		// State.Frequency follows only when this is the VFO being received:
		// otherwise a poll of the parked VFO would overwrite the operating
		// frequency with one the operator is not listening to.
		//
		// Until the radio has said which VFO it is on, FA is taken as the
		// operating one. That is this backend's long-standing assumption and it
		// is right far more often than not — but it is now an assumption with a
		// short life, because FR; settles it at connect.
		u.Key = backend.Key(cmd)
		hz, err := parseFrequency(string(arg))
		if err != nil {
			break
		}
		vfo := radio.VFOA
		if u.Key == keyFB {
			vfo = radio.VFOB
		}
		k.storeVFOFrequency(&u, vfo, hz)

	case keyFR, keyFT:
		// Which VFO is received and which transmitted. Split is the
		// relationship between them rather than a field either one carries, so
		// both are stored and the flag recomputed from the pair.
		u.Key = backend.Key(cmd)
		if len(arg) < 1 {
			break
		}
		if u.Key == keyFR {
			k.decodeReceiveVFO(&u, arg[0])
		} else if v, ok := vfoFromParam(arg[0]); ok {
			k.transmitVFO.Store(v)
		}
		if split, ok := k.storeSplit(); ok {
			u.Patch.Split = &split
		}

	case keyMD:
		u.Key = keyMD
		if len(arg) >= 1 {
			if m, ok := decodeMode(arg[0]); ok {
				u.Patch.Mode = &m
				k.mode.Store(uint32(m))
			}
		}

	case keyOM:
		// Both mode commands are decoded on every model. Only one of them can
		// arrive from a given radio, and keeping the decoder independent of the
		// configured model means a misconfigured station still reads correctly
		// instead of silently ignoring its rig's mode.
		u.Key = keyOM
		k.decodeOM(&u, arg)

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
			p := k.profile.powerFromWatts(w, k.lastMode())
			u.Patch.Power = &p
		}

	case keySM:
		u.Key = keySM
		// The meter selector, where the model has one, plus four digits. Both
		// the field width and the full-scale count are per model — 20, 30 or 70
		// dots — so neither may be assumed here.
		//
		// The digits count meter dots, and read the RF POWER METER while
		// transmitting rather than the S-meter: one command, two meters, chosen
		// by whether the rig is keyed. It used to land in SMeter either way,
		// which meant a transmission drove the receive signal bar to full scale
		// and left it there. Now it goes where it belongs, which is also what
		// makes power_meter work on this family — there is no separate command
		// for it to come from.
		if n := k.profile.smeterArgLen(); len(arg) == n {
			if v, err := strconv.Atoi(string(arg[n-4:])); err == nil {
				m := radio.Meter{Raw: v, Scale: k.profile.SMeterScale}
				if k.transmitting.Load() {
					u.Patch.PowerMeter = &m
				} else {
					u.Patch.SMeter = &m
				}
			}
		}

	case keySD:
		// The break-in delay. It is decoded before BI/VX in the poll order so
		// that "on" can be resolved into semi or full, and it is stored rather
		// than published: State carries the break-in mode, not the delay.
		u.Key = keySD
		if ms, ok := parseDelay(arg); ok {
			k.breakInDelayMS.Store(int32(ms))
		}

	case keyBI, keyVX:
		// Both are decoded on every model for the same reason as the two mode
		// commands: only one can arrive from a given radio, and a decoder that
		// depends on the configured model reads a misconfigured station wrong
		// rather than merely incompletely.
		//
		// A VX frame is taken as break-in only in CW. In any other mode the
		// same frame is the VOX setting, and storing that as break-in would
		// make EnsureCWWillTransmit trust a number about something else.
		u.Key = backend.Key(cmd)
		if u.Key == keyVX && !isCW(k.lastMode()) {
			break
		}
		if len(arg) >= 1 && arg[0] >= '0' && arg[0] <= '2' {
			if v, ok := k.storeBreakIn(int(arg[0] - '0')); ok {
				u.Patch.BreakIn = &v
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
		// Which character carries the selection is per model. On the TS-590 the
		// command is FL1/FL2 and the argument starts with the selection; on the
		// TS-890S it is FL0 followed by A/B/C; on the TS-990S an FL0 answer puts
		// the band first and the selection second. Reading the wrong character
		// would report the band as a filter slot.
		if sel, ok := k.profile.filterSelectionChar(arg); ok {
			if slot, ok := k.profile.FilterSelect.decode(sel); ok {
				u.Patch.FilterSlot = &slot
			}
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
		k.transmitting.Store(on)

	case keyPS:
		// The power switch. Nothing is published — a radio that answers at all
		// is on, which State already says through Connected — but the frame
		// still has to complete the transaction that the wake probe opens.
		u.Key = keyPS

	case keyAC:
		// The antenna tuner. Its three parameters collapse into one published
		// state: tuning while P3 says a cycle is running, otherwise in or out
		// of line as P2 says.
		u.Key = keyAC
		if v, ok := tunerFromAC(arg); ok {
			u.Patch.Tuner = &v
			k.tuner.Store(v)
		}

	case keyRM:
		// The meter function. One RM; read draws three answers — the reference
		// says so outright, "there are always three types of responses: SWR,
		// COMP, and ALC" — so each is decoded on its own and the transaction is
		// completed by whichever arrives first.
		u.Key = keyRM
		k.decodeRM(&u, arg)

	// The receive front end. Each sets its key before looking at the argument,
	// so that a value this backend cannot make sense of completes the read and
	// publishes nothing, rather than leaving the request unmatched and failing
	// the whole poll. A TS-480 answering three spaces to GT; in FM is the case
	// that makes this necessary rather than merely tidy.
	case keyPA:
		u.Key = keyPA
		if k.profile.Preamp > 0 {
			k.decodePA(&u, arg)
		}
	case keyRA:
		u.Key = keyRA
		if len(k.profile.Attenuator) > 0 {
			k.decodeRA(&u, arg)
		}
	case keyRG:
		u.Key = keyRG
		k.decodeRG(&u, arg)
	// The noise processing, the notches and the antenna. Each sets its key
	// before parsing, for the reason the front end does: an answer this
	// backend cannot read must still complete the request it belongs to.
	case keyNB:
		u.Key = keyNB
		if k.profile.NoiseBlanker > 0 {
			k.decodeNB(&u, arg)
		}
	case keyNL:
		u.Key = keyNL
		if k.profile.NBLevel {
			k.decodeNL(&u, arg)
		}
	case keyNR:
		u.Key = keyNR
		if k.profile.NoiseReduction > 0 {
			k.decodeNR(&u, arg)
		}
	case keyRL:
		u.Key = keyRL
		if k.profile.NRLevel {
			k.decodeRL(&u, arg)
		}
	case keyNT:
		u.Key = keyNT
		if k.profile.Notch {
			k.decodeNT(&u, arg)
		}
	case keyBP:
		u.Key = keyBP
		if k.profile.NotchFreq {
			k.decodeBP(&u, arg)
		}
	case keyAN:
		u.Key = keyAN
		if k.profile.Antennas > 0 {
			k.decodeAN(&u, arg)
		}

	// The transmit audio chain, gated per model for the same reason the front
	// end and the noise commands are, and keyed before parsing for the same
	// reason too. The gate matters more here than most: a TS-890S answering
	// PR11 — its processor effect type, not its switch — must complete the
	// transaction and publish nothing rather than report the processor on.
	case keyMG:
		u.Key = keyMG
		if k.profile.MicGainMax > 0 {
			k.decodeMG(&u, arg)
		}
	case keyPR:
		u.Key = keyPR
		if k.profile.ProcCmd != "" {
			k.decodePR(&u, arg)
		}
	case keyPL:
		u.Key = keyPL
		if k.profile.ProcLevelMax > 0 {
			k.decodePL(&u, arg)
		}

	case keyGC, keyGT:
		// Only the command this model actually keeps its AGC on. A TS-480's
		// GT answer is the AGC; a TS-590's is a time constant, and reading that
		// as a speed would publish a wrong value rather than none.
		u.Key = backend.Key(cmd)
		if len(k.profile.AGC) > 0 && u.Key == k.agcKey() {
			k.decodeAGC(&u, arg)
		}
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

// decodeOM parses the TS-890S/TS-990S mode answer, OM P1 P2.
//
// P1 is the display area: 0 is the left one, which is the main receiver, and 1
// is the right one. Frames for the right area complete their transaction but
// change nothing, because radio.State publishes a single mode and folding the
// second receiver into it would let a change over there overwrite the mode the
// operator is actually working. remoses does not model a sub receiver
// (Caps.SubReceiver is false), so the alternative is not "publish both".
//
// P2 carries the DATA flag inside the mode code, so a well-formed answer always
// settles both — unlike the MD models, where DATA needs its own DA.
func (k *Rig) decodeOM(u *backend.Update, arg []byte) {
	if len(arg) < 2 || arg[0] != '0' {
		return
	}
	m, data, ok := decodeOMMode(arg[1])
	if !ok {
		return
	}
	u.Patch.Mode = &m
	k.mode.Store(uint32(m))
	u.Patch.DataMode = &data
	k.dataMode.Store(data)
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
		k.transmitting.Store(false)
	case '1':
		on := true
		u.Patch.PTT = &on
		k.transmitting.Store(true)
	}

	if m, ok := decodeMode(f[ifMode]); ok {
		u.Patch.Mode = &m
		k.mode.Store(uint32(m))
	}

	// P10 and P12: which VFO is selected, and simplex or split. Both were
	// already in the field table and neither was read, which is why split was
	// never published on a radio that reports it on every fast poll.
	//
	// Only where FA and FB are two VFOs. On the TS-990S the same field means
	// something this backend does not model, so it is left alone.
	if k.profile.VFOPair.twoVFOs() {
		k.decodeReceiveVFO(u, f[ifFunc])
		switch f[ifSplit] {
		case '0', '1':
			split := f[ifSplit] == '1'
			// IF names the receive VFO, not the transmit one, so the pair FR/FT
			// tracks cannot be completed from here. The flag is authoritative
			// on its own, and the transmit VFO is recorded to match so that a
			// later FR or FT answer does not contradict it.
			k.transmitVFO.Store(transmitVFOFor(k.rxVFO(), split))
			u.Patch.Split = &split
		}
	}

	// A well-formed IF answer is proof that the rig is answering IF; again.
	k.ifBlocked.Store(false)
}
