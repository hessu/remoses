package yaesubin

import (
	"bytes"
	"fmt"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// blockLen is the command block: five bytes, always, whatever the command.
//
// "Irrespective of the number of parameters present, every Command Block sent
// must consist of five bytes." Unused parameter positions are padding — the
// manuals say the dummy bytes may contain any value — and remoses writes zeros
// there so a wire trace reads as the command it is rather than as whatever
// happened to be lying around.
const blockLen = 5

// The opcodes remoses sends. All seventeen are transcribed in the package doc;
// these six are the ones that map onto anything in radio.State.
const (
	opReadFreqMode = 0x03 // read frequency and mode: answers five bytes
	opSetFrequency = 0x01
	opSetMode      = 0x07
	opPTTOn        = 0x08
	opPTTOff       = 0x88
	opReadRXStatus = 0xE7 // answers one byte
	opReadTXStatus = 0xF7 // answers one byte
)

// Correlation keys.
//
// Unlike every other backend here these are not read off the answer — a
// five-byte binary answer names nothing, not even itself. They are derived from
// the request, by the same function that sizes the answer, so that the key a
// transaction waits for and the length the reader frames can never disagree.
const (
	keyFreqMode backend.Key = "03"
	keyRXStatus backend.Key = "E7"
	keyTXStatus backend.Key = "F7"
	// keyAck is every command that is not one of the three reads. They all
	// answer the same way and there is nothing in the answer to tell them
	// apart, so one key covers the lot; the session only ever has one
	// outstanding anyway.
	keyAck backend.Key = "ack"
)

// pending is what the command in flight will be answered with. It is the whole
// of this backend's framing: see the package doc, and backend.ReplyFramer.
type pending uint32

const (
	// pendNone is the state before the first command of a connection. Anything
	// arriving then can only be noise from a rig powering up, or the tail of an
	// answer to a command that timed out, and is discarded.
	pendNone pending = iota
	pendAck
	pendRXStatus
	pendTXStatus
	pendFreqMode
)

// statusLen is the length of the frequency-and-mode answer: four BCD bytes and
// a mode byte. Every other answer in the protocol is one byte.
const statusLen = 5

// replyLen is how many bytes this answer occupies.
func (p pending) replyLen() int {
	switch p {
	case pendAck, pendRXStatus, pendTXStatus:
		return 1
	case pendFreqMode:
		return statusLen
	}
	return 0
}

// key is the correlation key an answer of this kind arrives under.
func (p pending) key() backend.Key {
	switch p {
	case pendRXStatus:
		return keyRXStatus
	case pendTXStatus:
		return keyTXStatus
	case pendFreqMode:
		return keyFreqMode
	case pendAck:
		return keyAck
	}
	return backend.KeyUnsolicited
}

// replyTo reports what a request will be answered with.
//
// Deriving it from the request bytes rather than tracking it separately is what
// keeps the two halves honest: the key do() waits for and the length Split
// frames come out of this one function, so there is no second place to forget
// to update when a command is added.
func replyTo(req []byte) pending {
	if len(req) != blockLen {
		return pendNone
	}
	switch req[blockLen-1] {
	case opReadFreqMode:
		return pendFreqMode
	case opReadRXStatus:
		return pendRXStatus
	case opReadTXStatus:
		return pendTXStatus
	}
	// Everything else is a set, and every set is acknowledged. See the package
	// doc: that is field behaviour rather than anything the manuals promise,
	// and it is what keeps the stream aligned.
	return pendAck
}

// Expect implements backend.ReplyFramer. The session calls it under the write
// lock, immediately before req goes out.
//
// It is one atomic store and nothing else, deliberately: the reader goroutine
// is concurrently inside Split reading the same word.
func (y *Rig) Expect(req []byte) {
	y.pending.Store(uint32(replyTo(req)))
}

// Split is a bufio.SplitFunc over the inbound stream.
//
// There is nothing in the bytes to split on. An answer has no terminator, no
// length, no opcode and no checksum, and the two lengths it can have — one byte
// and five — are not distinguishable from each other by content: an
// acknowledgement of 0x00 and the leading 100/10 MHz digit pair of a status
// answer on the 1.8 MHz band are the same byte. So the length comes from what
// was asked, which is what Expect recorded.
//
// Nothing is ever cleared. Leaving the last command's expectation standing is
// better than reverting to pendNone when a transaction ends, because an answer
// that arrives after its own request timed out is then still framed correctly
// and still folds its frequency into the cache on the way past — it simply
// finds no waiter. Reverting would throw those bytes away and lose the reading.
func (y *Rig) Split(data []byte, atEOF bool) (int, []byte, error) {
	n := pending(y.pending.Load()).replyLen()
	if n == 0 {
		// Nothing has been asked for yet on this connection. A rig powering up
		// emits noise; discard it rather than let it become the first frame.
		return len(data), nil, nil
	}
	if len(data) < n {
		if atEOF {
			// A short tail can only be a truncated answer from a port that went
			// away. Drop it rather than report it.
			return len(data), nil, nil
		}
		return 0, nil, nil
	}
	return n, data[:n], nil
}

// Decode turns one frame from Split into an Update.
//
// What the frame means comes from the same pending value that sized it, for the
// same reason: there is nothing in a binary answer that identifies it. The
// value cannot change between the two, because it is written only from the
// command goroutine before a write and this runs on the reader goroutine while
// that write's transaction is still outstanding.
//
// It never errors. The one thing it does do that no other backend here does is
// report OK false for a frame that cannot be what was asked for — see
// decodeFreqMode, which is this protocol's only means of noticing that the
// stream has slipped.
func (y *Rig) Decode(frame []byte) (backend.Update, error) {
	// The session's scanner reuses its read buffer, so Raw has to own its bytes
	// or a logger holding onto an Update would print the next frame.
	u := backend.Update{Key: backend.KeyUnsolicited, OK: true, Raw: bytes.Clone(frame)}

	p := pending(y.pending.Load())
	u.Key = p.key()
	switch p {
	case pendFreqMode:
		y.decodeFreqMode(&u, u.Raw)
	case pendRXStatus:
		y.decodeRXStatus(&u, u.Raw)
	case pendTXStatus:
		y.decodeTXStatus(&u, u.Raw)
	case pendAck:
		// The acknowledgement carries no state. Its value is left in Raw for
		// the wire trace and is deliberately not turned into a rejection; see
		// the package doc.
	}
	return u, nil
}

// decodeFreqMode parses the answer to opcode 03: four packed-BCD bytes of
// frequency and one mode byte.
//
// It is also the only desynchronisation check this backend has, and the reason
// it is worth having is that on a protocol with no framing a single lost or
// late byte offsets everything after it for ever. Four BCD bytes are eight
// nibbles that must all be 0-9, so an offset frame fails here about
// forty-nine times in fifty. Reporting OK false makes the session's Do return
// rig.ErrNAK, which counts as a poll failure; five in a row tear the connection
// down and the reconnect starts a clean stream. That is the recovery, and there
// is no other: nothing in the protocol can resynchronise in place.
//
// An unknown *mode* byte is not treated the same way. It publishes no mode and
// leaves the frame OK, because the frequency in the same answer already proved
// the framing is right, and the manuals do print codes remoses's table would
// have to be extended for rather than a bad frame.
func (y *Rig) decodeFreqMode(u *backend.Update, f []byte) {
	if len(f) != statusLen {
		u.OK = false
		return
	}
	hz, ok := decodeFrequency(f[:4])
	if !ok {
		u.OK = false
		return
	}
	u.Patch.Frequency = &hz

	if m, data, ok := y.profile.decodeMode(f[4]); ok {
		u.Patch.Mode = &m
		u.Patch.DataMode = &data
	}
}

// Bit positions in the two status bytes. Both put a four-bit meter in the low
// nibble, a dummy bit above it, and three flags in the top three bits.
const (
	meterMask = 0x0F
	// In the RX status byte.
	rxSquelch = 0x80 // 1: squelch closed, no signal
	rxCTCSS   = 0x40 // 1: code un-matched
	rxDisc    = 0x20 // 1: discriminator off-centre
	// In the TX status byte. Note the polarity of the first two: zero means the
	// thing is happening.
	txPTT   = 0x80 // 0: transmitting
	txHiSWR = 0x40 // 1: high SWR
	txSplit = 0x20 // 0: split on
)

// meterScale is full scale for both meters: four bits, 0-15.
//
// The manuals label the field "S Meter Data" and "PO Meter Data" and calibrate
// neither against an S number or a watt figure, so radio.Meter carries the raw
// reading and this scale. Nothing here invents an S value.
const meterScale = 15

// decodeRXStatus parses the answer to opcode E7.
//
// Only the S-meter has a home in radio.State. The other three bits — squelch
// open, CTCSS/DCS code matched, discriminator centred — are real information
// this generation gives and newer Yaesus do not, and they are dropped because
// v1 models none of them, not because they are unavailable.
//
// The command is only ever sent while receiving. Its reading during transmit is
// not documented and cannot be trusted; see pollFast.
func (y *Rig) decodeRXStatus(u *backend.Update, f []byte) {
	if len(f) != 1 {
		u.OK = false
		return
	}
	m := radio.Meter{Raw: int(f[0] & meterMask), Scale: meterScale}
	u.Patch.SMeter = &m
}

// decodeTXStatus parses the answer to opcode F7, which is where PTT lives.
//
// PTT is inverted — bit 7 clear means transmitting — as is the split bit below
// it, which remoses reads and drops because radio.State has no split field.
//
// The other two fields are published **only while the radio is actually
// keyed**, and that restriction is the whole of the care this function needs.
// This is a transmitter status byte; nothing in it is documented as meaningful
// in receive, and these radios are reported to answer 0xFF there, which decodes
// to a perfectly plausible-looking full-scale power reading and a high SWR
// alarm on a radio sitting quietly in receive. Gating on the PTT bit in the
// same byte costs nothing and is self-consistent: if the radio says it is not
// transmitting, its transmit meters say nothing.
//
// The power reading goes into PowerMeter. It used to go into SMeter, for want
// of a forward-power field to put it in — a compromise that made a transmission
// drive the receive signal bar to full scale. State carries a transmit power
// meter now and this is what it is for.
//
// HI SWR is a threshold flag, not a ratio, and is published as a 0-or-1 reading
// on a scale of 1 so that its granularity is visible rather than implied. It is
// the only transmit fault these radios report over CAT, and a remote operator
// who cannot see the front panel is exactly who needs it. No SWRRatio is
// derived from it for the obvious reason: one bit cannot carry one.
func (y *Rig) decodeTXStatus(u *backend.Update, f []byte) {
	if len(f) != 1 {
		u.OK = false
		return
	}
	b := f[0]

	on := b&txPTT == 0
	u.Patch.PTT = &on
	y.transmitting.Store(on)
	if !on {
		return
	}

	po := radio.Meter{Raw: int(b & meterMask), Scale: meterScale}
	u.Patch.PowerMeter = &po

	swr := radio.Meter{Scale: 1}
	if b&txHiSWR != 0 {
		swr.Raw = 1
	}
	u.Patch.SWR = &swr
}

// --- Frequency -------------------------------------------------------------

// The frequency field is four bytes of packed BCD counting in units of ten
// hertz, which both worked examples in the manuals confirm: 43 97 00 00 is
// 439.700 MHz, and 01 42 34 56 is 14.23456 MHz.
//
// Ten hertz is also the radios' own minimum synthesizer step in CW and SSB, so
// the field is not a coarser view of a finer setting — it is exactly what the
// radio can tune.
const (
	freqStepHz = 10
	// maxFreqUnits is what fits in eight BCD digits, far above the 470 MHz these
	// radios reach. The guard is against emitting a malformed block, not against
	// an out-of-range frequency: Model.checkFrequency does that, with the real
	// bounds and a message naming them.
	maxFreqUnits = 99_999_999
)

// decodeFrequency reads the four-byte field.
//
// A nibble above 9 is not a frequency, and saying so is what lets the caller
// notice a stream that has slipped a byte. See decodeFreqMode.
func decodeFrequency(b []byte) (uint64, bool) {
	if len(b) != 4 {
		return 0, false
	}
	var units uint64
	for _, c := range b {
		hi, lo := c>>4, c&0x0F
		if hi > 9 || lo > 9 {
			return 0, false
		}
		units = units*100 + uint64(hi)*10 + uint64(lo)
	}
	return units * freqStepHz, true
}

// encodeFrequency builds the four-byte field.
//
// A request that is not a multiple of ten hertz is rounded to the nearest step
// rather than refused. The radio cannot express anything finer, refusing would
// turn a 3 Hz slip in a client's arithmetic into a failed tune, and the caller
// is not left guessing: every set is followed by a read-back, so what comes
// back is the frequency the radio is actually on.
func encodeFrequency(hz uint64) ([4]byte, error) {
	var out [4]byte
	units := (hz + freqStepHz/2) / freqStepHz
	if units > maxFreqUnits {
		return out, fmt.Errorf("yaesubin: frequency %d Hz does not fit the four-byte BCD field", hz)
	}
	for i := 3; i >= 0; i-- {
		out[i] = byte(units%10) | byte((units/10)%10)<<4
		units /= 100
	}
	return out, nil
}

// --- Command blocks ---------------------------------------------------------

// block builds one five-byte command: four parameter bytes, opcode last.
//
// A fresh slice each time. These are handed to the transport, and a shared
// backing array would be one retained reference away from a command being
// rewritten under the radio.
func block(p1, p2, p3, p4, opcode byte) []byte {
	return []byte{p1, p2, p3, p4, opcode}
}

// read builds one of the three read commands, whose four parameter bytes are
// all padding.
func read(opcode byte) []byte { return block(0, 0, 0, 0, opcode) }

// describe renders a command for an error message. A five-byte binary block is
// unreadable in a log line, and the opcode is the only part of it a reader
// needs to identify which command failed.
func describe(req []byte) string {
	if len(req) != blockLen {
		return fmt.Sprintf("malformed %d-byte command", len(req))
	}
	op := req[blockLen-1]
	name := map[byte]string{
		opReadFreqMode: "read frequency & mode",
		opSetFrequency: "set frequency",
		opSetMode:      "set mode",
		opPTTOn:        "PTT on",
		opPTTOff:       "PTT off",
		opReadRXStatus: "read RX status",
		opReadTXStatus: "read TX status",
	}[op]
	if name == "" {
		return fmt.Sprintf("opcode %02X", op)
	}
	return fmt.Sprintf("%s (%02X)", name, op)
}
