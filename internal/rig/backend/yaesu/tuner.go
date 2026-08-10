package yaesu

import (
	"context"
	"fmt"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The internal antenna tuner, command AC.
//
//	AC ;            read
//	AC P1 P2 P3 ;   set
//
//	P1  0 (fixed)
//	P2  0 internal or external tuner, 2 ATAS
//	P3  0 tuner off, 1 tuner on, and "tuning start" — see below
//
// P3's tuning-start value is NOT the same on both generations. The FT-950's
// table reads "0: Tuner OFF, 1: Tuner ON, 2: Tuning Start"; the FT-710's reads
// "0: Tuner OFF (Tuning Stop), 1: Tuner ON, 2: -, 3: Tuning Start". So the
// newer radios moved it to 3 and left 2 as a documented nothing. See
// Model.TunerTuneParam, and note that being wrong fails safe in both directions
// — a no-op or an out-of-range parameter, never an unasked transmission.
//
// P2 selects which tuner is being addressed, and only the internal one is
// offered here. ATAS is a mast-mounted screwdriver antenna whose "tuning" moves
// a motor rather than a matching network, and whose P3 values mean different
// things again; a station with one is not served by pretending it is the same
// device.
const (
	acInternal = '0'
	acFixed    = '0'
	acOff      = '0'
	acOn       = '1'
)

// tunerFromAC turns an AC answer into the published state.
//
// A tuning cycle in progress is reported as the same P3 value that starts one,
// which is why this needs the model: on one generation that is 3 and on the
// other 2.
func (y *Rig) tunerFromAC(arg []byte) (radio.Tuner, bool) {
	if len(arg) != 3 {
		return radio.TunerUnknown, false
	}
	switch arg[2] {
	case acOff:
		return radio.TunerOff, true
	case acOn:
		return radio.TunerOn, true
	case y.profile.TunerTuneParam:
		return radio.TunerTuning, true
	}
	return radio.TunerUnknown, false
}

// SetTuner switches the internal tuner in or out of line.
func (y *Rig) SetTuner(ctx context.Context, c backend.Conn, on bool) error {
	v := byte(acOff)
	if on {
		v = acOn
	}
	if err := send(ctx, c, fmt.Sprintf("AC%c%c%c;", acFixed, acInternal, v)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqAC, keyAC)
	return err
}

// StartTune begins a tuning cycle, which transmits.
//
// It does not wait for the cycle to finish: the rig decides how long it takes
// and reports it in the same field, which the poller follows.
func (y *Rig) StartTune(ctx context.Context, c backend.Conn) error {
	p := y.profile.TunerTuneParam
	if p == 0 {
		return fmt.Errorf("yaesu: no tuning-start parameter is recorded for the %s: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("AC%c%c%c;", acFixed, acInternal, p)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqAC, keyAC)
	return err
}

// Tuner is the last reading, which the poller consults to decide whether a
// cycle is worth watching at the fast rate.
func (y *Rig) Tuner() radio.Tuner {
	v, _ := y.tuner.Load().(radio.Tuner)
	return v
}
