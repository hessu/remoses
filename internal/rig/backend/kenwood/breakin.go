package kenwood

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// CW break-in, which this family spells four different ways. See BreakInStyle
// for which model uses which, and why remoses has to control it at all rather
// than leaving it to the operator: with break-in off, KY is accepted, the rig's
// buffer drains on schedule, and nothing is transmitted.
//
// Two of the four styles have an on/off command that cannot say whether "on"
// means semi or full. There the SD delay decides — "0000 (ms): Full break-in",
// anything from 0050 to 1000 being semi — so semi and full are a pair of
// commands on those radios and a single one on the TS-990S.

// semiDelayMS is the break-in delay written when switching a binary-style radio
// from full to semi, in the units SD takes.
//
// A value has to be chosen because full break-in IS SD 0, so there is no
// previous non-zero delay to restore: the rig only remembers one number and it
// is currently zero. 300 ms is mid-scale and roughly a character at 20 wpm,
// which is the usual reason to run semi rather than full — the transmitter
// stays keyed between letters instead of chattering the relay.
const semiDelayMS = 300

// breakInFromWire turns an on/off (or off/semi/full) parameter into the
// published value, given what the delay says on the styles where it matters.
func breakInFromWire(style BreakInStyle, param int, delayMS int) (radio.BreakIn, bool) {
	switch style {
	case BreakInBI3:
		switch param {
		case 0:
			return radio.BreakInOff, true
		case 1:
			return radio.BreakInSemi, true
		case 2:
			return radio.BreakInFull, true
		}
		return radio.BreakInUnknown, false

	case BreakInVX, BreakInBI2:
		switch param {
		case 0:
			return radio.BreakInOff, true
		case 1:
			if delayMS == 0 {
				return radio.BreakInFull, true
			}
			return radio.BreakInSemi, true
		}
		return radio.BreakInUnknown, false
	}
	return radio.BreakInUnknown, false
}

// breakInCmd is the on/off command letters for a style.
func breakInCmd(style BreakInStyle) (string, bool) {
	switch style {
	case BreakInVX:
		return "VX", true
	case BreakInBI2, BreakInBI3:
		return "BI", true
	}
	return "", false
}

// SetBreakIn puts the rig into the requested break-in state.
//
// On the VX style this is only done in CW mode, and refused otherwise. The
// command is not merely useless in another mode, it is actively wrong: outside
// CW the same VX writes the VOX setting, so honouring a break-in request on an
// SSB radio would switch voice VOX on behind the operator.
func (k *Rig) SetBreakIn(ctx context.Context, c backend.Conn, v radio.BreakIn) error {
	style := k.profile.BreakIn
	cmd, ok := breakInCmd(style)
	if !ok {
		return fmt.Errorf("kenwood: %s has no CW break-in command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if style == BreakInVX && !isCW(k.lastMode()) {
		return fmt.Errorf("kenwood: on the %s, break-in is set with VX, which addresses "+
			"VOX unless the radio is in CW; it is in %s: %w",
			k.profile.Label, k.lastMode(), backend.ErrUnsupported)
	}

	// The delay first, where it is what distinguishes semi from full. Writing
	// it after the on/off command would leave a moment of the wrong kind of
	// break-in, and on a radio already transmitting that is audible.
	if style.binary() {
		switch v {
		case radio.BreakInFull:
			if err := k.setBreakInDelay(ctx, c, 0); err != nil {
				return err
			}
		case radio.BreakInSemi:
			// Only when the rig is currently on full, so that an operator's own
			// choice of delay survives being switched off and on again.
			if k.breakInDelayMS.Load() == 0 {
				if err := k.setBreakInDelay(ctx, c, semiDelayMS); err != nil {
					return err
				}
			}
		}
	}

	var param int
	switch v {
	case radio.BreakInOff:
		param = 0
	case radio.BreakInSemi:
		param = 1
	case radio.BreakInFull:
		param = 2
		if style.binary() {
			param = 1 // full is SD 0 plus "on"; see above
		}
	default:
		return fmt.Errorf("kenwood: break-in %q is not one of off, semi, full: %w",
			v, backend.ErrUnsupported)
	}

	if err := send(ctx, c, fmt.Sprintf("%s%d;", cmd, param)); err != nil {
		return err
	}
	_, err := do(ctx, c, cmd+";", breakInKey(style))
	return err
}

// setBreakInDelay writes SD, in milliseconds.
func (k *Rig) setBreakInDelay(ctx context.Context, c backend.Conn, ms int) error {
	if err := send(ctx, c, fmt.Sprintf("SD%04d;", ms)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqSD, keySD)
	return err
}

// breakInKey is the correlation key the on/off answer arrives under.
func breakInKey(style BreakInStyle) backend.Key {
	if style == BreakInVX {
		return keyVX
	}
	return keyBI
}

// breakInRead is the on/off query for this model, or "" where there is none.
func (k *Rig) breakInRead() string {
	cmd, ok := breakInCmd(k.profile.BreakIn)
	if !ok {
		return ""
	}
	// VX outside CW would return the VOX setting, which is not this and must
	// not be published as it.
	if k.profile.BreakIn == BreakInVX && !isCW(k.lastMode()) {
		return ""
	}
	return cmd + ";"
}

// BreakIn reports the last reading. Unknown until one has been made, which on
// the VX style means until the rig has been in CW.
func (k *Rig) BreakIn() radio.BreakIn {
	v, _ := k.breakIn.Load().(radio.BreakIn)
	return v
}

// storeBreakIn records a decoded on/off parameter and returns the published
// value, resolving semi against the delay where the style needs it.
func (k *Rig) storeBreakIn(param int) (radio.BreakIn, bool) {
	v, ok := breakInFromWire(k.profile.BreakIn, param, int(k.breakInDelayMS.Load()))
	if !ok {
		return radio.BreakInUnknown, false
	}
	k.breakIn.Store(v)
	return v, true
}

// parseDelay reads an SD answer's millisecond field.
func parseDelay(arg []byte) (int, bool) {
	if len(arg) != 4 {
		return 0, false
	}
	ms, err := strconv.Atoi(string(arg))
	if err != nil {
		return 0, false
	}
	return ms, true
}

// isCW reports whether the mode is one where break-in applies at all. CW-R is
// included: it is CW with the sideband inverted, and the rig treats it as CW
// for every purpose that matters here.
func isCW(m radio.Mode) bool {
	return m == radio.ModeCW || m == radio.ModeCWR
}
