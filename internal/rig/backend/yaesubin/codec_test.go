package yaesubin

import (
	"bytes"
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// --- Frequency --------------------------------------------------------------

// TestFrequencyWorkedExamples is the pair of examples the manuals themselves
// print, in both directions. They are the only statement anywhere of what the
// four bytes mean, so they are the test that matters most in this file.
func TestFrequencyWorkedExamples(t *testing.T) {
	cases := []struct {
		name  string
		bytes [4]byte
		hz    uint64
	}{
		// "43 97 00 00 = 439.700 MHz", from the Read Frequency & Mode Status
		// note and from the set-frequency worked example.
		{"439.700 MHz", [4]byte{0x43, 0x97, 0x00, 0x00}, 439_700_000},
		// "01, 42, 34, 56, [01] = 14.23456 MHz", from the opcode chart.
		{"14.23456 MHz", [4]byte{0x01, 0x42, 0x34, 0x56}, 14_234_560},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hz, ok := decodeFrequency(c.bytes[:])
			if !ok || hz != c.hz {
				t.Errorf("decodeFrequency(% 02X) = %d, %v; want %d, true", c.bytes, hz, ok, c.hz)
			}
			got, err := encodeFrequency(c.hz)
			if err != nil {
				t.Fatalf("encodeFrequency(%d): %v", c.hz, err)
			}
			if got != c.bytes {
				t.Errorf("encodeFrequency(%d) = % 02X, want % 02X", c.hz, got, c.bytes)
			}
		})
	}
}

func TestFrequencyRoundTrip(t *testing.T) {
	for _, hz := range []uint64{100_000, 1_810_000, 7_074_000, 50_313_000, 144_300_000, 432_100_000, 470_000_000} {
		b, err := encodeFrequency(hz)
		if err != nil {
			t.Fatalf("encodeFrequency(%d): %v", hz, err)
		}
		got, ok := decodeFrequency(b[:])
		if !ok || got != hz {
			t.Errorf("round trip of %d Hz gave %d, %v", hz, got, ok)
		}
	}
}

// TestFrequencyRoundsToStep pins the rounding rule. The field counts in tens of
// hertz, which is also the radios' finest tuning step, so a finer request has
// to go somewhere; nearest is what encodeFrequency documents.
func TestFrequencyRoundsToStep(t *testing.T) {
	cases := []struct{ in, want uint64 }{
		{14_074_000, 14_074_000},
		{14_074_004, 14_074_000},
		{14_074_005, 14_074_010},
		{14_074_009, 14_074_010},
	}
	for _, c := range cases {
		b, err := encodeFrequency(c.in)
		if err != nil {
			t.Fatalf("encodeFrequency(%d): %v", c.in, err)
		}
		got, _ := decodeFrequency(b[:])
		if got != c.want {
			t.Errorf("encodeFrequency(%d) encodes %d Hz, want %d", c.in, got, c.want)
		}
	}
}

// TestDecodeFrequencyRejectsNonBCD is the desynchronisation check at its lowest
// level. A nibble above 9 cannot be a frequency, and saying so is the only way
// this protocol ever notices that the stream has slipped.
func TestDecodeFrequencyRejectsNonBCD(t *testing.T) {
	for _, b := range [][]byte{
		{0x0A, 0x00, 0x00, 0x00},
		{0x00, 0xF0, 0x00, 0x00},
		{0x00, 0x00, 0x00, 0xBB},
		{0x00, 0x00, 0x00}, // wrong length
	} {
		if _, ok := decodeFrequency(b); ok {
			t.Errorf("decodeFrequency(% 02X) accepted a non-BCD field", b)
		}
	}
}

func TestEncodeFrequencyOverflows(t *testing.T) {
	// Eight BCD digits of ten hertz stop just short of 1 GHz.
	if _, err := encodeFrequency(1_000_000_000); err == nil {
		t.Error("encodeFrequency accepted a value too wide for the four-byte field")
	}
}

// --- Framing ----------------------------------------------------------------

// TestSplitSizesFromTheRequest is the core of this backend. The same two bytes
// are one frame or part of another depending only on what was asked for, which
// is exactly what a protocol with no delimiters means.
func TestSplitSizesFromTheRequest(t *testing.T) {
	y := testRig(t, "ft-857d")
	stream := []byte{0x01, 0x42, 0x50, 0x00, 0x01}

	// Asked for a status: all five bytes are one frame.
	y.Expect(read(opReadFreqMode))
	frames, err := scan(y, stream)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(frames) != 1 || !bytes.Equal(frames[0], stream) {
		t.Fatalf("status framing gave %v, want one 5-byte frame", frames)
	}

	// Asked for an acknowledgement: the same bytes are five separate frames,
	// because each answer is one byte.
	y.Expect(block(0, 0, 0, 0, opSetMode))
	frames, err = scan(y, stream)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(frames) != 5 {
		t.Fatalf("ack framing gave %d frames, want 5", len(frames))
	}
}

// TestSplitWaitsForAWholeFrame covers the byte-at-a-time arrival a slow serial
// port produces: nothing may be emitted until the whole answer is in.
func TestSplitWaitsForAWholeFrame(t *testing.T) {
	y := testRig(t, "ft-857d")
	y.Expect(read(opReadFreqMode))

	for n := range 5 {
		adv, tok, err := y.Split(make([]byte, n), false)
		if err != nil || adv != 0 || tok != nil {
			t.Errorf("Split with %d of 5 bytes = %d, %v, %v; want 0, nil, nil", n, adv, tok, err)
		}
	}
	adv, tok, err := y.Split(make([]byte, 5), false)
	if err != nil || adv != 5 || len(tok) != 5 {
		t.Errorf("Split with a whole frame = %d, %v, %v", adv, tok, err)
	}
}

// TestSplitDiscardsBeforeAnythingIsAsked covers a rig powering up into an open
// port. Until the first command there is no expectation to frame against, so
// there is nothing those bytes could be but noise.
func TestSplitDiscardsBeforeAnythingIsAsked(t *testing.T) {
	y := testRig(t, "ft-857d")
	junk := []byte{0xFF, 0x00, 0x55}
	adv, tok, err := y.Split(junk, false)
	if err != nil || tok != nil || adv != len(junk) {
		t.Errorf("Split before any command = %d, %v, %v; want %d, nil, nil", adv, tok, err, len(junk))
	}
}

// TestSplitDropsATruncatedTail covers a port that went away mid-answer.
func TestSplitDropsATruncatedTail(t *testing.T) {
	y := testRig(t, "ft-857d")
	y.Expect(read(opReadFreqMode))
	adv, tok, err := y.Split([]byte{0x01, 0x42}, true)
	if err != nil || tok != nil || adv != 2 {
		t.Errorf("Split at EOF with a partial frame = %d, %v, %v; want 2, nil, nil", adv, tok, err)
	}
}

// TestExpectIgnoresAMalformedRequest guards the one input Expect cannot
// trust — it is called with whatever the session was handed.
func TestExpectIgnoresAMalformedRequest(t *testing.T) {
	if got := replyTo([]byte{0x03}); got != pendNone {
		t.Errorf("replyTo of a short block = %v, want pendNone", got)
	}
	if got := replyTo(nil); got != pendNone {
		t.Errorf("replyTo(nil) = %v, want pendNone", got)
	}
}

// TestReplyToCoversEveryCommand asserts the property the design rests on: the
// key a transaction waits for and the length Split frames come from one
// function, so they cannot disagree.
func TestReplyToCoversEveryCommand(t *testing.T) {
	cases := []struct {
		req  []byte
		want pending
		len  int
		key  backend.Key
	}{
		{read(opReadFreqMode), pendFreqMode, 5, keyFreqMode},
		{read(opReadRXStatus), pendRXStatus, 1, keyRXStatus},
		{read(opReadTXStatus), pendTXStatus, 1, keyTXStatus},
		{read(opPTTOn), pendAck, 1, keyAck},
		{read(opPTTOff), pendAck, 1, keyAck},
		{block(0x01, 0x42, 0x50, 0x00, opSetFrequency), pendAck, 1, keyAck},
		{block(0x01, 0, 0, 0, opSetMode), pendAck, 1, keyAck},
	}
	for _, c := range cases {
		got := replyTo(c.req)
		if got != c.want {
			t.Errorf("replyTo(%s) = %v, want %v", describe(c.req), got, c.want)
		}
		if got.replyLen() != c.len {
			t.Errorf("%s: reply length %d, want %d", describe(c.req), got.replyLen(), c.len)
		}
		if got.key() != c.key {
			t.Errorf("%s: key %q, want %q", describe(c.req), got.key(), c.key)
		}
	}
}

// --- Decode -----------------------------------------------------------------

func TestDecodeFreqMode(t *testing.T) {
	y := testRig(t, "ft-857d")
	y.Expect(read(opReadFreqMode))

	u, err := y.Decode([]byte{0x01, 0x42, 0x50, 0x00, 0x01})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Key != keyFreqMode || !u.OK {
		t.Fatalf("Decode gave key %q ok %v", u.Key, u.OK)
	}
	if u.Patch.Frequency == nil || *u.Patch.Frequency != 14_250_000 {
		t.Errorf("frequency = %v, want 14250000", u.Patch.Frequency)
	}
	if u.Patch.Mode == nil || *u.Patch.Mode != radio.ModeUSB {
		t.Errorf("mode = %v, want USB", u.Patch.Mode)
	}
	if u.Patch.DataMode == nil || *u.Patch.DataMode {
		t.Errorf("data mode = %v, want false", u.Patch.DataMode)
	}
}

// TestDecodeFreqModeRejectsCorruption is the recovery path in miniature: a
// frame that cannot be a frequency is reported not-OK, which the session turns
// into a poll failure, and five of those reconnect onto a clean stream.
func TestDecodeFreqModeRejectsCorruption(t *testing.T) {
	y := testRig(t, "ft-857d")
	y.Expect(read(opReadFreqMode))

	u, err := y.Decode([]byte{0xFF, 0x42, 0x50, 0x00, 0x01})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.OK {
		t.Error("a non-BCD frequency field decoded as OK; the stream has no other way to notice a slip")
	}
	if !u.Patch.Empty() {
		t.Error("a rejected frame published state")
	}
}

// TestDecodeUnknownModeKeepsTheFrame is the other half of the rule above. An
// unrecognised mode byte is a gap in remoses's table, not evidence of a bad
// frame — the frequency in the same answer already proved the framing — so the
// frequency is published and the mode is left alone.
func TestDecodeUnknownModeKeepsTheFrame(t *testing.T) {
	y := testRig(t, "ft-857d")
	y.Expect(read(opReadFreqMode))

	u, err := y.Decode([]byte{0x01, 0x42, 0x50, 0x00, 0x77})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !u.OK {
		t.Error("an unknown mode byte rejected the whole frame")
	}
	if u.Patch.Frequency == nil {
		t.Error("frequency was dropped along with the unknown mode")
	}
	if u.Patch.Mode != nil {
		t.Errorf("an unknown mode byte published mode %v", *u.Patch.Mode)
	}
}

func TestDecodeRXStatus(t *testing.T) {
	y := testRig(t, "ft-857d")
	y.Expect(read(opReadRXStatus))

	// Squelch closed, code un-matched, discriminator off-centre, meter 9. Only
	// the meter has anywhere to go.
	u, err := y.Decode([]byte{rxSquelch | rxCTCSS | rxDisc | 0x09})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Key != keyRXStatus {
		t.Fatalf("key = %q, want %q", u.Key, keyRXStatus)
	}
	if u.Patch.SMeter == nil || u.Patch.SMeter.Raw != 9 || u.Patch.SMeter.Scale != meterScale {
		t.Errorf("S-meter = %+v, want raw 9 of %d", u.Patch.SMeter, meterScale)
	}
	if u.Patch.SMeter.S != nil {
		t.Error("an S number was invented; the manuals calibrate this field against nothing")
	}
}

// TestDecodeTXStatusReceiving pins the inverted PTT bit and the rule that a
// transmit meter says nothing while the radio is receiving. FF is what these
// radios are reported to answer in receive, and it is exactly the frame that
// would otherwise publish a full-scale power reading and a high-SWR alarm on a
// radio that is not transmitting.
func TestDecodeTXStatusReceiving(t *testing.T) {
	y := testRig(t, "ft-857d")
	y.Expect(read(opReadTXStatus))

	u, err := y.Decode([]byte{0xFF})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Patch.PTT == nil || *u.Patch.PTT {
		t.Fatalf("PTT = %v, want false: bit 7 set means PTT OFF", u.Patch.PTT)
	}
	if u.Patch.SMeter != nil {
		t.Errorf("a transmit power reading of %+v was published while receiving", u.Patch.SMeter)
	}
	if u.Patch.SWR != nil {
		t.Errorf("a high-SWR alarm was published while receiving")
	}
	if y.transmitting.Load() {
		t.Error("transmitting hint set while receiving")
	}
}

func TestDecodeTXStatusTransmitting(t *testing.T) {
	y := testRig(t, "ft-857d")
	y.Expect(read(opReadTXStatus))

	// PTT on (bit 7 clear), SWR fine, split off, power 12 of 15.
	u, err := y.Decode([]byte{txSplit | 0x0C})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Patch.PTT == nil || !*u.Patch.PTT {
		t.Fatalf("PTT = %v, want true", u.Patch.PTT)
	}
	if !y.transmitting.Load() {
		t.Error("transmitting hint not set")
	}
	// Into the transmit power meter, not the S-meter: this is forward power,
	// and putting it in the receive signal bar drove that to full scale on
	// every transmission.
	if u.Patch.PowerMeter == nil || u.Patch.PowerMeter.Raw != 12 {
		t.Errorf("power meter = %+v, want raw 12", u.Patch.PowerMeter)
	}
	if u.Patch.SMeter != nil {
		t.Errorf("S-meter = %+v, want it left alone while transmitting", u.Patch.SMeter)
	}
	if u.Patch.SWR == nil || u.Patch.SWR.Raw != 0 || u.Patch.SWR.Scale != 1 {
		t.Errorf("SWR = %+v, want raw 0 of 1", u.Patch.SWR)
	}

	// The same reading with the high-SWR bit set.
	u, err = y.Decode([]byte{txSplit | txHiSWR | 0x0C})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Patch.SWR == nil || u.Patch.SWR.Raw != 1 {
		t.Errorf("SWR with the alarm bit set = %+v, want raw 1", u.Patch.SWR)
	}
}

// TestDecodeAckIsNeverARejection is the safety rule. F0 is reported to mean
// "already in that state", which is precisely what a redundant unkey is, and
// the dead-man path sends those by design.
func TestDecodeAckIsNeverARejection(t *testing.T) {
	y := testRig(t, "ft-857d")
	y.Expect(read(opPTTOff))

	for _, b := range []byte{0x00, 0xF0, 0xFF} {
		u, err := y.Decode([]byte{b})
		if err != nil {
			t.Fatalf("Decode(%02X): %v", b, err)
		}
		if u.Key != keyAck {
			t.Errorf("Decode(%02X) key = %q, want %q", b, u.Key, keyAck)
		}
		if !u.OK {
			t.Errorf("acknowledgement %02X was reported as a rejection", b)
		}
		if !u.Patch.Empty() {
			t.Errorf("acknowledgement %02X published state", b)
		}
	}
}

// TestDecodeRawOwnsItsBytes matters because the session's scanner reuses its
// read buffer: an Update that aliased it would print the next frame from a log.
func TestDecodeRawOwnsItsBytes(t *testing.T) {
	y := testRig(t, "ft-857d")
	y.Expect(read(opReadFreqMode))

	buf := []byte{0x01, 0x42, 0x50, 0x00, 0x01}
	u, err := y.Decode(buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	buf[0] = 0x99
	if u.Raw[0] == 0x99 {
		t.Error("Update.Raw aliases the caller's buffer")
	}
}

func TestDescribeNamesTheOpcode(t *testing.T) {
	if got := describe(read(opReadFreqMode)); got != "read frequency & mode (03)" {
		t.Errorf("describe = %q", got)
	}
	if got := describe(block(0, 0, 0, 0, 0x81)); got != "opcode 81" {
		t.Errorf("describe of an unimplemented opcode = %q", got)
	}
}
