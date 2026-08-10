package civ

import (
	"context"
	"fmt"

	"github.com/hessu/remoses/internal/rig/backend"
)

// Switching the radio itself off and on: command 18 00 and 18 01.
//
// Powering off is an ordinary set. Powering on is not, and the reference is
// unusually explicit about why:
//
//	When sending the power ON command (18 01), you need to repeatedly send
//	"FE" before the standard format.
//
// A radio that is off is not listening the way a radio that is on listens. Its
// CI-V circuit wakes on seeing traffic, and the preamble is what gives it time
// to do so before the frame that matters arrives. The count is per baud rate,
// because what the radio needs is a duration and the reference expresses it in
// bytes:
//
//	115200: 150    57600: 75    38400: 50
//	 19200:  25     9600: 13     4800:  7
//
// Sent short, the radio misses the frame and simply stays off — which looks
// exactly like a radio that is not there, so it is worth getting right rather
// than discovering by field report.

// wakePreamble is the number of 0xFE bytes to send before 18 01 at a given
// baud rate, from the reference's own table.
//
// Rates between the tabulated ones round UP to the next listed count: the
// preamble is a duration, and too many FEs costs milliseconds where too few
// costs a radio that does not wake.
func wakePreamble(baud int) int {
	switch {
	case baud >= 115200:
		return 150
	case baud >= 57600:
		return 75
	case baud >= 38400:
		return 50
	case baud >= 19200:
		return 25
	case baud >= 9600:
		return 13
	default:
		return 7
	}
}

// requirePowerSwitch guards both directions.
func (r *Rig) requirePowerSwitch() error {
	if r.model.PowerSwitch {
		return nil
	}
	return fmt.Errorf("civ: the %s has no power on/off command (18): %w",
		r.model.Label, backend.ErrUnsupported)
}

// PowerOff switches the radio off with 18 00.
//
// deep is accepted and ignored: this family documents one off. Refusing it
// would fail a request the radio can honour perfectly well — 18 00 is its only
// off, so it is both the shallow and the deep one.
//
// The acknowledgement is not waited for. A radio going off may answer FB and it
// may simply stop, depending on how fast it drops its CI-V circuit, and a
// timeout waiting for a reply from a radio that just did what it was told would
// be reported as a failure of a command that succeeded.
func (r *Rig) PowerOff(ctx context.Context, c backend.Conn, deep bool) error {
	if err := r.requirePowerSwitch(); err != nil {
		return err
	}
	return c.Send(ctx, r.frame(cmdPower, subPowerOff))
}

// PowerOn wakes the radio with a preamble and 18 01.
//
// There is no escalation to try here: the preamble IS this family's ritual, and
// the reference documents no second way in. Sending it is cheap, so it goes out
// whether the radio was asleep or not — 18 01 to a radio already on is a
// no-op, and guessing wrong in the other direction leaves a station dark.
//
// Also unacknowledged, and for a better reason than PowerOff's: the radio is
// asleep when this is sent. Whether it woke is answered by whether it starts
// talking, which is what the connection attempt that follows is for.
func (r *Rig) PowerOn(ctx context.Context, c backend.Conn) error {
	if err := r.requirePowerSwitch(); err != nil {
		return err
	}
	n := wakePreamble(r.baud)
	frame := make([]byte, 0, n+6)
	for range n {
		frame = append(frame, preamble)
	}
	frame = append(frame, r.frame(cmdPower, subPowerOn)...)
	return c.Send(ctx, frame)
}
