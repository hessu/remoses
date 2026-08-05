package yaesu

import (
	"bytes"
	"strconv"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Correlation keys. A Yaesu answer names itself — the reply to FA; is an FA
// frame — so the command letters are the natural key space.
const (
	keyFA backend.Key = "FA"
	keyFB backend.Key = "FB"
	keyMD backend.Key = "MD"
	keyPC backend.Key = "PC"
	keySM backend.Key = "SM"
	keySH backend.Key = "SH"
	keyNA backend.Key = "NA"
	keyIF backend.Key = "IF"
	keyTX backend.Key = "TX"
	keyID backend.Key = "ID"
	keyAI backend.Key = "AI"
)

// There are deliberately no error keys here.
//
// None of the six CAT manuals documents any error, NAK or busy response — there
// is no Yaesu equivalent of Kenwood's ?; / E; / O;. Inventing one and waiting
// for it would be worse than not having it: the wait list is what completes a
// transaction, so a key the rig never sends buys nothing, and a key it sends
// with a different meaning would complete the wrong transaction. The cost is
// real and is a property of the protocol rather than of this code: a command the
// rig refuses produces silence, so it burns the session's full per-command
// timeout where a Kenwood fails in one round trip. Poll and Init are therefore
// conservative about sending anything that might be refused, and every value
// with a range is range-checked here before it goes out.

// smeterScale is the full-scale SM reading, and it is family-wide: SM0; answers
// three digits, 000-255, on every model. The parameter is an uncalibrated meter
// reading, not a signal level, so radio.Meter carries the raw value and this
// scale rather than an invented dBm or S number.
const smeterScale = 255

// Split is a bufio.SplitFunc over the inbound stream.
//
// Yaesu framing is Kenwood's: everything ends in ';', so the only real work is
// resynchronisation. A rig that has just been powered on, or a port opened
// mid-frame, emits leading rubbish; since every command begins with an ASCII
// letter, anything before the first letter of a frame can be discarded without
// ambiguity. Empty frames (a stray ';', or ";;") are swallowed here so Decode
// never has to reason about them.
func (y *Rig) Split(data []byte, atEOF bool) (int, []byte, error) {
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
// whitespace. Interior bytes are left alone: IF; legitimately carries spaces
// and signs.
func trimFrame(b []byte) []byte {
	for len(b) > 0 && !isLetter(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == '\r' || b[len(b)-1] == '\n' || b[len(b)-1] == 0) {
		b = b[:len(b)-1]
	}
	return b
}

func isLetter(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// Decode turns one frame from Split into an Update.
//
// It never errors. An unrecognised command, a frame too short for its own
// parameters, a mode code the model's table does not list — all of them come
// back as an Update the session can apply harmlessly. Refusing to decode would
// be worse than useless here, because this same path carries the AI push
// traffic, which is by definition whatever the operator happened to touch.
//
// A frame that is recognised but malformed keeps its key and returns an empty
// patch. Completing the pending transaction with nothing to say beats stalling
// it until the timeout: the value is unchanged in the cache and the next poll
// will fetch it again.
func (y *Rig) Decode(frame []byte) (backend.Update, error) {
	// The session's scanner reuses its read buffer, so Raw has to own its bytes
	// or a logger holding onto an Update would print the next frame.
	u := backend.Update{Key: backend.KeyUnsolicited, OK: true, Raw: bytes.Clone(frame)}
	f := u.Raw

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
		// carries a single frequency and this backend treats VFO A — the MAIN
		// band on the dual-receiver models — as the operating VFO throughout,
		// so publishing VFO B here would fight with that.
		u.Key = keyFB

	case keyMD:
		u.Key = keyMD
		y.decodeMD(&u, arg)

	case keyPC:
		u.Key = keyPC
		y.decodePC(&u, arg)

	case keySM:
		u.Key = keySM
		// The selector plus three digits. Only the MAIN meter is published:
		// State has one S-meter, so letting a SUB reading through would
		// overwrite the meter the operator is actually watching. Whether SM
		// reads power during transmit is documented for Kenwood and for no
		// Yaesu, so nothing is assumed about it here.
		if len(arg) == 4 && arg[0] == '0' {
			if v, err := strconv.Atoi(string(arg[1:])); err == nil {
				m := radio.Meter{Raw: v, Scale: smeterScale}
				u.Patch.SMeter = &m
			}
		}

	case keySH:
		u.Key = keySH
		y.decodeSH(&u, arg)

	case keyNA:
		// Narrow has no home in radio.Patch, but it selects which column of the
		// SH bandwidth table applies on the FT-991A and FT-891, so it is kept.
		u.Key = keyNA
		if len(arg) == 2 && arg[0] == '0' && (arg[1] == '0' || arg[1] == '1') {
			y.narrow.Store(arg[1] == '1')
		}

	case keyIF:
		u.Key = keyIF
		y.decodeIF(&u, f)

	case keyTX:
		// TX is the one command in this protocol whose read and set forms are
		// spelled differently, and getting it backwards keys a transmitter: a
		// bare TX; is the QUERY and this is its answer. See Rig.SetPTT.
		//
		// The answer distinguishes CAT keying from rig-side keying — a foot
		// switch, MOX, or a paddle in break-in — which no other backend here
		// can report. Both are transmitting, so both are PTT true.
		//
		// 3 is the FTdx9000's alone: its table has a fourth row for keyed at
		// the rig AND by CAT at once, where every other manual stops at 2. It
		// is decoded on every model rather than gated on the profile, because
		// this is a read of what the transmitter is doing and there is no
		// reading of a 3 that is not transmitting.
		u.Key = keyTX
		if len(arg) == 1 {
			switch arg[0] {
			case '0':
				on := false
				u.Patch.PTT = &on
			case '1', '2', '3':
				on := true
				u.Patch.PTT = &on
			}
		}

	case keyID:
		u.Key = keyID
		if n, err := strconv.Atoi(string(arg)); err == nil {
			y.id.Store(uint32(n))
		}

	case keyAI:
		u.Key = keyAI
	}

	return u, nil
}

// splitCommand peels the leading command letters off a frame. Every parameter
// this backend reads is digits, spaces or punctuation, so the run of letters is
// unambiguous. A run that is not exactly 2 long is line noise, and the frame is
// reported unsolicited.
func splitCommand(f []byte) (cmd string, arg []byte, ok bool) {
	n := 0
	for n < len(f) && isLetter(f[n]) {
		n++
	}
	if n != 2 {
		return "", nil, false
	}
	return upperASCII(string(f[:n])), f[n:], true
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

// decodeMD parses the mode answer, MD P1 P2.
//
// P1 is the receiver: 0 is MAIN, 1 the sub receiver on the FTdx101 and FTX-1 —
// and on the FTdx10, which carries the selector on MD without having a second
// receiver at all. Frames for the sub receiver complete their transaction but
// change nothing, because radio.State publishes a single mode and folding the
// second receiver into it would let a change over there overwrite the mode the
// operator is actually working.
//
// P2 carries the DATA flag inside the mode code, so a well-formed answer always
// settles both. Which is which is per model: see Model.Codes.
func (y *Rig) decodeMD(u *backend.Update, arg []byte) {
	if len(arg) != 2 || arg[0] != '0' {
		return
	}
	m, data, ok := y.profile.decodeMode(arg[1])
	if !ok {
		return
	}
	u.Patch.Mode = &m
	u.Patch.DataMode = &data
	y.mode.Store(uint32(m))
	y.dataMode.Store(data)
}

// decodePC parses the power answer.
//
// The FTX-1 puts a head selector in front of its three digits, so the parameter
// is four characters there and three everywhere else. Which arrived decides how
// it is read, rather than the configured model: the two forms are unambiguous
// by length, and a station whose configuration names the wrong radio is still
// read correctly instead of silently reporting no power at all.
//
// What the three digits mean is the one thing here that must come from the
// model. They are watts on nine of the twelve radios and an uncalibrated 000-255
// index on the FTdx5000 and FTdx9000, and nothing in the frame distinguishes
// them — PC050 is 50 W on an FT-950 and a fifth of full output on an FTdx5000.
// Model.PowerRaw is the only thing that can tell them apart.
func (y *Rig) decodePC(u *backend.Update, arg []byte) {
	digits := arg
	if len(arg) == 4 {
		head := arg[0] - '0'
		if head != pcHeadRadio && head != pcHeadAmp {
			return
		}
		y.pcHead.Store(uint32(head))
		digits = arg[1:]
	}
	if len(digits) != 3 {
		return
	}
	n, err := strconv.Atoi(string(digits))
	if err != nil {
		return
	}
	p := powerFromRaw(n)
	if !y.profile.PowerRaw {
		p = powerFromWatts(n, y.maxPowerW())
	}
	u.Patch.Power = &p
}

// decodeSH turns an SH answer into a passband in Hz.
//
// SH reports a table index, not a width, so the reverse lookup needs the mode
// the rig is in and — on the FT-991A and FT-891 — the narrow setting. Both come
// from earlier frames, which is why the poll reads MD and NA before SH. Where
// the mode has no bandwidth table, or the index is one the manual leaves blank,
// nothing is published: a passband invented from the wrong column would look
// exactly like a real one.
func (y *Rig) decodeSH(u *backend.Update, arg []byte) {
	index, narrow, ok := y.profile.filterIndex(arg)
	if !ok {
		return
	}
	if y.profile.Filter == FilterNarrowFlag {
		// The FT-891 is the one model whose SH answer carries the flag, so it
		// is more current than the last NA.
		y.narrow.Store(narrow)
	}
	ladder, ok := y.profile.Widths.ladder(y.lastMode(), y.dataMode.Load(), y.narrow.Load())
	if !ok {
		return
	}
	if hz, ok := widthAt(ladder, index); ok {
		u.Patch.PassbandHz = &hz
	}
}

// ifLayout is where the fields remoses reads sit inside an IF answer,
// zero-based and with the ';' already stripped by Split. The manuals count both
// differently: they number character positions from 1, so an offset here is one
// less than the position they print, and they include the terminator, so length
// here is one less than the answer length they quote. A 28-character IF in a
// manual is 27 here.
type ifLayout struct {
	length    int
	freqStart int
	freqEnd   int
	mode      int
}

// The three layouts, and the two independent reasons they differ.
//
// The FT-950 generation's frequency field is eight digits where the FTdx101
// generation's is nine, so its whole answer is one byte shorter and every field
// after the frequency sits one earlier. The FTX-1's memory-channel field is five
// characters (00000-00999, P-01L-P-50U, 50001-50020, EMGCH) where everyone
// else's is three, so its fields sit two later. Nothing else in the answer
// changes: the FT-950's fixed three-character memory channel is the FTdx9000's
// three spaces, and the parameter numbering the manuals use differs (the
// FTdx9000 splits the clarifier into two parameters, so its mode is P7 rather
// than P6) without any byte moving.
//
// Note what is in none of them: there is no TX/RX flag anywhere in a Yaesu IF.
// Kenwood's is at P8; Yaesu's P8 is CTCSS/DCS. So the fast poll cannot be one
// transaction the way it is on a TS-590, and PTT needs its own TX;. There is no
// split flag either.
var (
	ifOlder = ifLayout{length: 26, freqStart: 5, freqEnd: 13, mode: 20}
	ifShort = ifLayout{length: 27, freqStart: 5, freqEnd: 14, mode: 21}
	ifLong  = ifLayout{length: 29, freqStart: 7, freqEnd: 16, mode: 23}
)

// ifQuirkLength is a short-layout answer with thirteen bytes of rubbish on the
// end: 27 + 13. Some FT-991 firmware is reported to emit it at random. No
// manual mentions it, and remoses has no such radio to confirm it against, so
// this is tolerance for a field report rather than a transcribed fact.
const ifQuirkLength = 40

// decodeIF parses the bulk status answer: frequency and mode in one
// transaction, which is two of the three things the fast poll needs.
//
// The layout is chosen by the length that arrived rather than by the configured
// model. The three forms are 26, 27 and 29 characters and nothing else is any
// of them, so the dispatch is unambiguous — and it means an FTX-1 configured as
// an FT-710, an FT-950 configured as an FTdx101, or an unprofiled radio on
// generic, is still read correctly rather than reporting a frequency taken from
// bytes inside the clarifier field. It is the same reasoning that drives the
// IC-905's variable frequency field (DESIGN.md §5.4): decode from what the wire
// says, encode from the model.
//
// Only the fields State has a home for are extracted. Clarifier, memory
// channel, CTCSS and repeater shift are at known offsets but discarded, since
// v1 does not model them.
//
// Unlike Kenwood's, this command answers in every mode — data mode is just
// another mode code on a Yaesu — so there is no fallback path to keep.
func (y *Rig) decodeIF(u *backend.Update, f []byte) {
	var l ifLayout
	switch len(f) {
	case ifOlder.length:
		l = ifOlder
	case ifShort.length:
		l = ifShort
	case ifLong.length:
		l = ifLong
	case ifQuirkLength:
		// Some FT-991 firmware is reported to append thirteen bytes of rubbish
		// at random. The surplus is trailing, so every field remoses reads is
		// still at its usual offset and the short layout applies unchanged.
		// Tolerating it costs nothing; refusing it would drop the entire bulk
		// poll on an affected radio, intermittently, for no gain.
		l = ifShort
	default:
		// Short or long means line corruption. Leave the patch empty; the key
		// still completes the transaction.
		return
	}

	if hz, err := parseFrequencyWidth(string(f[l.freqStart:l.freqEnd]), l.freqEnd-l.freqStart); err == nil {
		u.Patch.Frequency = &hz
	}
	if m, data, ok := y.profile.decodeMode(f[l.mode]); ok {
		u.Patch.Mode = &m
		u.Patch.DataMode = &data
		y.mode.Store(uint32(m))
		y.dataMode.Store(data)
	}
}
