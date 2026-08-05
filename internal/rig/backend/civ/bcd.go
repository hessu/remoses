package civ

import "fmt"

// CI-V carries numbers as packed binary-coded decimal: one byte holds two
// decimal digits, the more significant in the high nibble. Nothing here trusts
// the rig to send valid BCD — noise on the line decodes as 0x0F nibbles, and a
// silently mistranslated frequency is worse than a dropped one.

// The frequency field is normally five bytes (10 digits, 1 GHz down to 1 Hz),
// but it is not fixed across the family. The IC-905 uses six bytes (12 digits,
// 100 GHz down to 1 Hz) while its 10 GHz band is selected, and five below it.
//
// Decoding is therefore driven by the length that actually arrived rather than
// by the model, which keeps the decoder a pure function and means a radio that
// surprises us is still read correctly. Only encoding needs to choose a width.
const (
	freqBytes     = 5
	freqBytesWide = 6

	// maxFrequencyHz is the largest value the five-byte field can hold, and
	// wideMaxFrequencyHz the six-byte one.
	maxFrequencyHz     = 9_999_999_999
	wideMaxFrequencyHz = 999_999_999_999

	// wideThresholdHz is where the IC-905 switches to the longer field: its
	// 10 GHz band. Below it, the reference specifies the ordinary five bytes.
	wideThresholdHz = 10_000_000_000
)

// bcdByte packs 0..99 into one byte.
func bcdByte(v int) byte { return byte(v/10<<4 | v%10) }

// unbcdByte unpacks one byte, reporting false if either nibble is not a digit.
func unbcdByte(b byte) (int, bool) {
	hi, lo := int(b>>4), int(b&0x0F)
	if hi > 9 || lo > 9 {
		return 0, false
	}
	return hi*10 + lo, true
}

// encodeBCD2 packs 0..9999 into the two-byte, most-significant-first field used
// by the level and meter commands (14 xx, 15 02).
func encodeBCD2(v int) [2]byte {
	return [2]byte{bcdByte(v / 100), bcdByte(v % 100)}
}

// decodeBCD2 unpacks that field.
func decodeBCD2(b []byte) (int, bool) {
	if len(b) != 2 {
		return 0, false
	}
	hi, ok := unbcdByte(b[0])
	if !ok {
		return 0, false
	}
	lo, ok := unbcdByte(b[1])
	if !ok {
		return 0, false
	}
	return hi*100 + lo, true
}

// encodeFrequency packs Hz into the frequency field of commands 03/05, using
// the width the radio expects for that frequency.
//
// The field runs least significant first, which is the opposite order to every
// other multi-byte CI-V value: byte 0 is the 1 Hz and 10 Hz digits, byte 4 the
// 100 MHz and 1 GHz digits. Within each byte the low nibble is the less
// significant digit, as everywhere else.
//
// wide selects the six-byte encoding, which only the IC-905 uses and only on
// its 10 GHz band. Sending six bytes to a radio expecting five would be
// rejected, so the width is chosen from the target frequency rather than from
// the model alone.
func encodeFrequency(hz uint64, wide bool) ([]byte, error) {
	n := freqBytes
	limit := uint64(maxFrequencyHz)
	if wide {
		n, limit = freqBytesWide, wideMaxFrequencyHz
	}
	if hz > limit {
		return nil, fmt.Errorf("civ: frequency %d Hz exceeds the %d-digit CI-V field", hz, n*2)
	}
	out := make([]byte, n)
	for i := range out {
		lo := hz % 10
		hz /= 10
		hi := hz % 10
		hz /= 10
		out[i] = byte(hi<<4 | lo)
	}
	return out, nil
}

// decodeFrequency unpacks the field, accepting either width.
//
// Length-driven rather than model-driven on purpose: the rig tells us how many
// digits it is sending, and believing it costs nothing while letting an
// unprofiled radio still be read correctly.
func decodeFrequency(b []byte) (uint64, bool) {
	if len(b) != freqBytes && len(b) != freqBytesWide {
		return 0, false
	}
	var hz uint64
	for i := len(b) - 1; i >= 0; i-- {
		v, ok := unbcdByte(b[i])
		if !ok {
			return 0, false
		}
		hz = hz*100 + uint64(v)
	}
	return hz, true
}
