package kenwood

import (
	"context"
	"time"

	"github.com/hessu/remoses/internal/rig/backend"
)

// Switching the radio itself off and on: command PS.
//
//	PS0 ;   power off
//	PS1 ;   power on
//	PS9 ;   power off, low current mode
//
// Two kinds of off, and the reference is unusually forthcoming about the
// difference:
//
//	When turning the power Off by setting the P1 parameter to 0, more current
//	is consumed than if you turn the power Off by operating the transceiver
//	panel power switch. However, you can switch the power back On without any
//	special procedures, using the PS command.
//
//	When turning the power Off by setting the P1 parameter to 9, the same
//	amount of standby current is consumed as if you turned the power Off by
//	operating the transceiver panel power switch. In this case, to turn the
//	power back On using the PS command, you must perform the following
//	procedure: 1) When using hardware flow control, turn the flow control Off.
//	2) Send dummy data (;). 3) Wait for more than 200 ms. 4) Send "PS1;" within
//	2 seconds of sending the dummy data.
//
// So the shallow off trades standby current for a wake that is one command, and
// the deep one trades a fiddly wake for the current draw of the front-panel
// switch. remoses defaults to the shallow one: a remote station that cannot be
// woken is a station somebody has to drive to.
const (
	reqPowerOn      = "PS1;"
	reqPowerOff     = "PS0;"
	reqPowerOffDeep = "PS9;"
	reqPS           = "PS;"
	// wakeDummy is step 2 of the deep-off wake: a bare terminator, which is not
	// a command and cannot do anything if the radio turns out to be awake.
	wakeDummy = ";"
)

// wakeSettle is step 3, "more than 200 ms". A little over, because the step
// after it has a two-second deadline and being early is the failure that costs
// a wake.
const wakeSettle = 300 * time.Millisecond

// PowerOn wakes the radio, escalating only if it needs to.
//
// The plain PS1; comes first, because it is what a radio put into the shallow
// off wants and it costs one command. If nothing answers, the radio is either
// in the low-current standby or was switched off at the panel, and only the
// documented ritual gets in: dummy byte, settle, then PS1; inside two seconds.
//
// Hardware flow control is step 1 of that ritual and is not done here, because
// remoses never turns it on — see internal/transport/serial. Were it ever
// added, this is the place that would have to care.
//
// Neither attempt is verified beyond the probe between them. A radio that has
// just been woken spends seconds booting before it answers anything, so a
// stricter check here would report failure on a radio that is on its way up;
// the connection attempt that follows is the real verdict.
func (k *Rig) PowerOn(ctx context.Context, c backend.Conn) error {
	if err := send(ctx, c, reqPowerOn); err != nil {
		return err
	}
	if _, err := do(ctx, c, reqPS, keyPS); err == nil {
		// It answered, so it was either already awake or the shallow wake took.
		return nil
	}

	// Nothing. Assume the deep standby and perform the documented sequence.
	if err := send(ctx, c, wakeDummy); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wakeSettle):
	}
	return send(ctx, c, reqPowerOn)
}

// PowerOff switches the radio off, shallow by default.
func (k *Rig) PowerOff(ctx context.Context, c backend.Conn, deep bool) error {
	req := reqPowerOff
	if deep {
		req = reqPowerOffDeep
	}
	// Unacknowledged on purpose: the radio is switching itself off, and waiting
	// for a reply from something that has just been told to stop would report a
	// command that worked as a timeout.
	return send(ctx, c, req)
}
