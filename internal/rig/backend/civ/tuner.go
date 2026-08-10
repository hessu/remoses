package civ

import (
	"context"
	"fmt"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The internal antenna tuner, command 1C 01.
//
//	1C 01        read
//	1C 01 00     tuner off (bypassed)
//	1C 01 01     tuner on
//	1C 01 02     start a tuning cycle; also what a read answers while one runs
//
// The IC-703's reference is the one that spells the last part out — "0=OFF,
// 1=ON, 2=Start tuning or while tuning" — and it is the part that makes the
// state worth publishing: a client can watch a cycle run rather than issuing a
// command into silence for two seconds.
//
// This command is guarded by the model, and that guard is a safety interlock
// rather than tidiness. On the IC-718, 1C 01 is PTT: its table has no 1C 00 row
// at all. Sending "start tuning" there would key the transmitter and hold it
// keyed, and nothing in the frame would say so. See Model.Tuner.
const (
	subTuner    = 0x01
	tunerOff    = 0x00
	tunerOn     = 0x01
	tunerTuning = 0x02
)

// tunerFromByte maps the wire value onto the published state.
func tunerFromByte(b byte) (radio.Tuner, bool) {
	switch b {
	case tunerOff:
		return radio.TunerOff, true
	case tunerOn:
		return radio.TunerOn, true
	case tunerTuning:
		return radio.TunerTuning, true
	}
	return radio.TunerUnknown, false
}

// requireTuner refuses on a radio whose 1C 01 is not the tuner.
func (r *Rig) requireTuner() error {
	if r.model.Tuner {
		return nil
	}
	return fmt.Errorf("civ: the %s has no antenna tuner command: %w",
		r.model.Label, backend.ErrUnsupported)
}

// SetTuner switches the tuner in or out of line.
func (r *Rig) SetTuner(ctx context.Context, c backend.Conn, on bool) error {
	if err := r.requireTuner(); err != nil {
		return err
	}
	v := byte(tunerOff)
	if on {
		v = tunerOn
	}
	return r.set(ctx, c, "antenna tuner", r.frame(cmdTransceiver, subTuner, v))
}

// StartTune begins a tuning cycle, which transmits.
//
// It does not wait for the cycle to finish. The radio decides how long that
// takes — a second or two, longer into something it struggles to match — and
// reports it by answering 02 to a read, which the poller turns into
// radio.TunerTuning and follows back to on or off. Blocking here would hold the
// caller's request and the station's lock open for the duration and tell them
// nothing the state does not.
func (r *Rig) StartTune(ctx context.Context, c backend.Conn) error {
	if err := r.requireTuner(); err != nil {
		return err
	}
	return r.set(ctx, c, "antenna tuner cycle", r.frame(cmdTransceiver, subTuner, tunerTuning))
}

// Tuner is the last reading, which the poller consults to decide whether to
// keep watching a cycle at the fast rate.
func (r *Rig) Tuner() radio.Tuner {
	v, _ := r.tuner.Load().(radio.Tuner)
	return v
}
