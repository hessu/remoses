package civ

import (
	"context"
	"fmt"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// CW break-in, command 16 47, from the IC-7610 and IC-9700 CI-V references.
//
//	16 47        read
//	16 47 00     BK-IN off
//	16 47 01     semi break-in on
//	16 47 02     full break-in (QSK) on
//
// This is not one more switch. Both references print the same footnote against
// command 17, the CW message buffer: a message sent from the PC is transmitted
// "if the [TRANSMIT] or an external TX switch is ON, or the Break-in function
// is ON". With break-in off and nothing keying by hand, command 17 is accepted,
// the rig's buffer drains on schedule, PTT never rises and nothing reaches the
// air.
//
// That is a command succeeding and meaning nothing, which is the failure mode
// this backend keeps being bitten by — and it was found the same way as the
// others, by sending real Morse at a real radio and having the operator report
// that they heard none of it. So remoses reads break-in, publishes it, lets a
// client set it, and refuses to queue CW that would go nowhere.

// breakInValue maps the wire byte onto the published value, for the style this
// radio speaks.
//
// The two-value form reports "on" rather than choosing between semi and full.
// Its table offers no way to tell them apart, and they are audibly different —
// full is QSK, switching between transmit and receive inside the element gaps —
// so naming one would be remoses deciding something the radio did not say.
func breakInValue(style BreakInStyle, b byte) (radio.BreakIn, bool) {
	if b == 0x00 {
		return radio.BreakInOff, true
	}
	switch style {
	case BreakInOnOff:
		if b == 0x01 {
			return radio.BreakInOn, true
		}
	case BreakInSemiFull:
		switch b {
		case 0x01:
			return radio.BreakInSemi, true
		case 0x02:
			return radio.BreakInFull, true
		}
	}
	return radio.BreakInUnknown, false
}

// breakInByte is the inverse, for a set.
//
// On the two-value form every request that transmits becomes its single "on":
// semi and full are the same setting there, so honouring both is accurate
// rather than approximate. Sending 02 to that radio would be an out-of-range
// parameter for a distinction it does not have.
//
// On the three-value form a bare "on" becomes semi. The radio does distinguish
// them and the caller did not, so the quieter one is chosen — the same choice
// cw.break_in's default makes, and for the same reason: full clocks the relays
// between elements and is not something to turn on for somebody.
func breakInByte(style BreakInStyle, v radio.BreakIn) (byte, error) {
	if v == radio.BreakInOff {
		return 0x00, nil
	}
	if style == BreakInOnOff {
		if v.Transmits() {
			return 0x01, nil
		}
		return 0, fmt.Errorf("civ: break-in %q is not one of off, on: %w", v, backend.ErrUnsupported)
	}
	switch v {
	case radio.BreakInSemi, radio.BreakInOn:
		return 0x01, nil
	case radio.BreakInFull:
		return 0x02, nil
	}
	return 0, fmt.Errorf("civ: break-in %q is not one of off, semi, full: %w", v, backend.ErrUnsupported)
}

// SetBreakIn writes command 16 47.
//
// A radio without it refuses rather than silently doing nothing, because the
// whole point of this control is that silence is the failure being prevented.
func (r *Rig) SetBreakIn(ctx context.Context, c backend.Conn, v radio.BreakIn) error {
	if r.model.BreakIn == BreakInNone {
		return fmt.Errorf("civ: %s has no CW break-in command (16 47): %w",
			r.model.Label, backend.ErrUnsupported)
	}
	b, err := breakInByte(r.model.BreakIn, v)
	if err != nil {
		return err
	}
	return r.set(ctx, c, "break-in", r.frame(cmdFunc, subBreakIn, b))
}

// BreakIn reports the last reading, for the CW path to check before it queues
// anything. Unknown on a radio that has not been asked or cannot be.
func (r *Rig) BreakIn() radio.BreakIn {
	v, _ := r.breakIn.Load().(radio.BreakIn)
	return v
}
