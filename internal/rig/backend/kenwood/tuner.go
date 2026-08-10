package kenwood

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
//	P1  0 RX-AT THRU, 1 RX-AT IN
//	P2  0 TX-AT THRU, 1 TX-AT IN
//	P3  0 stop tuning / tuning is stopped, 1 start tuning / tuning is active
//
// Three parameters for what is really two things and an action, and the
// reference's footnotes are what make it workable:
//
//   - "The setting cannot be performed for RX IN/THRU", so P1 is read-only in
//     practice however it is sent.
//   - "AT Tuning will not begin when using the TX THRU status" — the tuner has
//     to be in line before a cycle will start, which is why the session applies
//     a tuner setting before a tune in the same request.
//   - "To begin tuning, you must use command AC111", the only worked example
//     given.
//
// Two more rules are not in the reference at all and were found by asking a
// TS-590S, because both of them make a set fail:
//
//   - AN ON/OFF SET MUST SEND P1 AS 0. AC010 and AC000 are accepted; AC110,
//     AC100 and AC101 all answer ?;. Which is the "cannot be performed" note
//     read strictly — a set may not ask for an RX-AT state, so it has to ask
//     for none. Switching the tuner in with AC010 answers AC110, the radio
//     bringing the receive tuner in on its own.
//   - A SET THAT CHANGES NOTHING IS REJECTED. Asking for off while already off,
//     or on while already on, answers ?; rather than doing nothing. That would
//     make an ordinary idempotent request — the same PATCH sent twice — fail
//     the second time, so SetTuner reads first and skips a write it does not
//     need.
//
// AC111 keeps its P1 of 1 despite the first rule, because that is the form the
// reference names and the form verified on the air; the rig evidently
// special-cases it. Changing it to AC011 on the strength of the pattern would
// be trading something tested for something inferred.
//
// The published state comes from P2 and P3 together: tuning while P3 is 1,
// otherwise on or off as P2 says. P3 is the answer to "is a cycle running", so
// a client can watch one rather than guess how long to wait.
//
// What a finished cycle does NOT report is success, and the radio says it
// anyway: on a TS-590S a cycle that finds a match ends with the tuner IN
// (AC110, published as on) and one that fails ends with it THRU (AC000,
// published as off). Confirmed on the air across four frequencies — three that
// matched and one whose SWR was too high, where the rig gave up in well under a
// second. So a client watching tuner alone can tell the two apart.

// AC parameter values.
const (
	acThru  = '0'
	acIn    = '1'
	acStop  = '0'
	acStart = '1'
)

// tunerFromAC turns an AC answer's parameters into the published state.
func tunerFromAC(arg []byte) (radio.Tuner, bool) {
	if len(arg) != 3 {
		return radio.TunerUnknown, false
	}
	if arg[2] == acStart {
		return radio.TunerTuning, true
	}
	switch arg[1] {
	case acIn:
		return radio.TunerOn, true
	case acThru:
		return radio.TunerOff, true
	}
	return radio.TunerUnknown, false
}

// SetTuner switches the transmit tuner in or out of line.
//
// Only P2 is meant. P1 goes out as 0 because a set carrying 1 is refused, and
// P3 as 0, which is "stop tuning" and so also "do not start one".
//
// The read first is not caution, it is required: this radio refuses a set that
// changes nothing, so without it the second of two identical requests would
// fail. A client repeating a PATCH — or simply pressing the same button twice —
// is doing nothing wrong and should not get a 422 for it.
func (k *Rig) SetTuner(ctx context.Context, c backend.Conn, on bool) error {
	want := radio.TunerOff
	tx := byte(acThru)
	if on {
		want, tx = radio.TunerOn, acIn
	}

	u, err := do(ctx, c, reqAC, keyAC)
	if err != nil {
		return err
	}
	if u.Patch.Tuner != nil && *u.Patch.Tuner == want {
		// Already there. Saying so is the whole answer.
		return nil
	}

	if err := send(ctx, c, fmt.Sprintf("AC%c%c%c;", acThru, tx, acStop)); err != nil {
		return err
	}
	_, err = do(ctx, c, reqAC, keyAC)
	return err
}

// StartTune begins a tuning cycle, which transmits.
//
// AC111 exactly, which the reference names as the way to begin one and which is
// what was verified on the air. It does not wait: the rig decides how long the
// cycle takes and reports it in P3, which the poller follows.
func (k *Rig) StartTune(ctx context.Context, c backend.Conn) error {
	if err := send(ctx, c, fmt.Sprintf("AC%c%c%c;", acIn, acIn, acStart)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqAC, keyAC)
	return err
}

// Tuner is the last reading, which the poller consults to decide whether a
// cycle is worth watching at the fast rate.
func (k *Rig) Tuner() radio.Tuner {
	v, _ := k.tuner.Load().(radio.Tuner)
	return v
}
