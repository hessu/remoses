package yaesu

import (
	"context"
	"time"

	"github.com/hessu/remoses/internal/rig/backend"
)

// Switching the radio itself off and on: command PS.
//
//	PS0 ;   power off
//	PS1 ;   power on
//
// One kind of off, so deep is accepted and ignored. Waking is the interesting
// half, and the FT-950's reference states the requirement plainly:
//
//	This command requires dummy data be initially sent. Then after one second
//	and before two seconds the command is sent.
//
// A window rather than a delay, which is what makes it worth writing down: too
// early and the radio has not woken enough to hear, too late and it has gone
// back to sleep. remoses aims at the middle.
const (
	reqPowerOn  = "PS1;"
	reqPowerOff = "PS0;"
	reqPS       = "PS;"
	// wakeDummy is the "dummy data": a bare terminator, which is not a command
	// and so cannot do anything if the radio turns out to be awake already.
	wakeDummy = ";"
)

// wakeSettle sits in the middle of the documented one-to-two-second window.
const wakeSettle = 1500 * time.Millisecond

// PowerOn wakes the radio, escalating only if it needs to.
//
// The plain PS1; goes first: it costs one command, and a radio that is merely
// asleep rather than off may take it. If nothing answers, the documented
// sequence follows — dummy byte, then the command inside the window.
//
// The wait is long enough to be worth explaining: a second and a half of a
// caller's time, once, to wake a station. The alternative is a wake that fails
// silently and a radio nobody can reach.
func (y *Rig) PowerOn(ctx context.Context, c backend.Conn) error {
	if err := send(ctx, c, reqPowerOn); err != nil {
		return err
	}
	if _, err := do(ctx, c, reqPS, keyPS); err == nil {
		return nil
	}

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

// PowerOff switches the radio off.
//
// deep is accepted and ignored: this family documents one off, so PS0 is both
// the shallow and the deep one. Unacknowledged, like the other backends' — a
// radio told to stop is not going to answer.
func (y *Rig) PowerOff(ctx context.Context, c backend.Conn, deep bool) error {
	return send(ctx, c, reqPowerOff)
}
