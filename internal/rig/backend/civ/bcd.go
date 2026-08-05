package civ

import "fmt"

// CI-V carries numbers as packed binary-coded decimal: one byte holds two
// decimal digits, the more significant in the high nibble. Nothing here trusts
// the rig to send valid BCD — noise on the line decodes as 0x0F nibbles, and a
// silently mistranslated frequency is worse than a dropped one.

// maxFrequencyHz is the largest value the five-byte frequency field can hold.
const maxFrequencyHz = 9_999_999_999

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

// encodeFrequency packs Hz into the five-byte field of commands 03/05.
//
// The field runs least significant first, which is the opposite order to every
// other multi-byte CI-V value: byte 0 is the 1 Hz and 10 Hz digits, byte 4 the
// 100 MHz and 1 GHz digits. Within each byte the low nibble is the less
// significant digit, as everywhere else.
func encodeFrequency(hz uint64) ([5]byte, error) {
	var out [5]byte
	if hz > maxFrequencyHz {
		return out, fmt.Errorf("civ: frequency %d Hz exceeds the 10-digit CI-V field", hz)
	}
	for i := range out {
		lo := hz % 10
		hz /= 10
		hi := hz % 10
		hz /= 10
		out[i] = byte(hi<<4 | lo)
	}
	return out, nil
}

// decodeFrequency unpacks that field.
func decodeFrequency(b []byte) (uint64, bool) {
	if len(b) != 5 {
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
