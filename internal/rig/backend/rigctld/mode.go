package rigctld

import (
	"fmt"

	"github.com/hessu/remoses/internal/radio"
)

// modeSpec is a remoses mode plus the data-mode flag Hamlib folds into the mode
// name. remoses keeps data mode orthogonal (radio.Mode's doc says why), so
// PKTUSB decodes as USB with DataMode set rather than as a mode of its own.
type modeSpec struct {
	mode radio.Mode
	data bool
}

// hamlibModes maps rigctld's mode tokens onto remoses modes.
//
// The tokens are the ones rig_strrmode produces, plus the aliases
// rig_parse_mode accepts, both from the mode_str table in src/misc.c. Where two
// spellings exist rig_strrmode returns whichever is listed first — "CWR" not
// "CW-R", but "AM-D" not "PKTAM" — so both are here; guessing which a given
// Hamlib version emits would be a needless way to lose a mode reading.
//
// The narrow variants (FMN, AMN, CWN) collapse onto their parent mode. That
// loses the narrowness, which remoses cannot express, but reporting FM for a
// rig in FM-narrow is closer to the truth than reporting nothing. The reverse
// direction never produces them, so a client that reads FM and sets FM does not
// silently widen the filter.
var hamlibModes = map[string]modeSpec{
	"LSB":    {radio.ModeLSB, false},
	"USB":    {radio.ModeUSB, false},
	"CW":     {radio.ModeCW, false},
	"CWN":    {radio.ModeCW, false},
	"CWR":    {radio.ModeCWR, false},
	"CW-R":   {radio.ModeCWR, false},
	"AM":     {radio.ModeAM, false},
	"AMN":    {radio.ModeAM, false},
	"FM":     {radio.ModeFM, false},
	"FMN":    {radio.ModeFM, false},
	"RTTY":   {radio.ModeFSK, false},
	"RTTYR":  {radio.ModeFSKR, false},
	"RTTY-R": {radio.ModeFSKR, false},
	"PSK":    {radio.ModePSK, false},
	"PSKR":   {radio.ModePSKR, false},

	"PKTLSB": {radio.ModeLSB, true},
	"LSB-D":  {radio.ModeLSB, true},
	"PKTUSB": {radio.ModeUSB, true},
	"USB-D":  {radio.ModeUSB, true},
	"PKTFM":  {radio.ModeFM, true},
	"FM-D":   {radio.ModeFM, true},
	"PKTFMN": {radio.ModeFM, true},
	"PKTAM":  {radio.ModeAM, true},
	"AM-D":   {radio.ModeAM, true},
}

// decodeMode reports what a mode token means. A token this table does not know
// — WFM, D-STAR, the IC-F8101's six data modes — reports false, and the caller
// leaves the mode out of the patch rather than overwriting a good reading with
// ModeUnknown.
func decodeMode(token string) (radio.Mode, bool, bool) {
	s, ok := hamlibModes[token]
	return s.mode, s.data, ok
}

// encodeTokens is decodeMode's inverse, and is deliberately a separate, smaller
// table: it names the one token to send for each mode remoses has, out of the
// several that decode to it. These are the tokens rig_strrmode itself produces,
// which is what rig_parse_mode is guaranteed to round-trip.
var encodeTokens = map[modeSpec]string{
	{radio.ModeLSB, false}:  "LSB",
	{radio.ModeUSB, false}:  "USB",
	{radio.ModeCW, false}:   "CW",
	{radio.ModeCWR, false}:  "CWR",
	{radio.ModeAM, false}:   "AM",
	{radio.ModeFM, false}:   "FM",
	{radio.ModeFSK, false}:  "RTTY",
	{radio.ModeFSKR, false}: "RTTYR",
	{radio.ModePSK, false}:  "PSK",
	{radio.ModePSKR, false}: "PSKR",

	{radio.ModeLSB, true}: "PKTLSB",
	{radio.ModeUSB, true}: "PKTUSB",
	{radio.ModeFM, true}:  "PKTFM",
	{radio.ModeAM, true}:  "PKTAM",
}

// encodeMode picks the token for a mode and data-mode pair.
//
// The combinations that are missing are missing because Hamlib has no name for
// them: there is no data variant of CW, RTTY or PSK, since those are already
// the digital modes. Substituting the plain mode would put the operator on the
// air through a different input than they asked for, so it is an error.
func encodeMode(m radio.Mode, data bool) (string, error) {
	if token, ok := encodeTokens[modeSpec{m, data}]; ok {
		return token, nil
	}
	if data {
		if _, ok := encodeTokens[modeSpec{m, false}]; ok {
			return "", fmt.Errorf("rigctld: Hamlib has no data-mode variant of %s; "+
				"it names data modes only for LSB, USB, FM and AM", m)
		}
	}
	return "", fmt.Errorf("rigctld: Hamlib has no mode token for %s", m)
}

// Hamlib mode bits from include/hamlib/rig.h, used to read the mode bitmasks
// out of \dump_state. Only the bits remoses can name are listed; the rest of
// the 64 are decoded as "some mode we have no word for" and dropped.
var modeBits = map[uint]string{
	0:  "AM",
	1:  "CW",
	2:  "USB",
	3:  "LSB",
	4:  "RTTY",
	5:  "FM",
	7:  "CWR",
	8:  "RTTYR",
	10: "PKTLSB",
	11: "PKTUSB",
	12: "PKTFM",
	22: "PKTAM",
	30: "PSK",
	31: "PSKR",
}

// tokensFromMask turns a Hamlib mode bitmask into the tokens it names, in bit
// order so the result is stable.
func tokensFromMask(mask uint64) []string {
	var out []string
	for bit := uint(0); bit < 64; bit++ {
		if mask&(1<<bit) == 0 {
			continue
		}
		if token, ok := modeBits[bit]; ok {
			out = append(out, token)
		}
	}
	return out
}
