package yaesu

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The receive front end: PA the preamplifier, RA the attenuator, RG the RF gain
// and GT the AGC.
//
// All four take the same shape — two letters, a fixed 0 for the main receiver,
// then the value — and all four answer with the 0 still in front of it, which is
// why every decoder here indexes past it rather than reading arg[0].
//
// The AGC is the one that does not round-trip, and it is not a bug in either
// direction. A set carries 0 to 4, where 4 is AUTO; an answer carries 0 to 6,
// where 4, 5 and 6 are auto having settled on fast, mid or slow. So a client
// that sets "auto" and reads back "auto-mid" is being told what auto chose, and
// remoses publishes that rather than flattening three readings into the one
// value that was written. See radio.AGCAutoFast and Model.agcReading.
const (
	reqPA = "PA0;"
	reqRA = "RA0;"
	reqRG = "RG0;"
	reqGT = "GT0;"
	// mainRX is the P1 every one of these commands takes. It is 0 for the main
	// receiver on every profiled model, including the two with a real sub
	// receiver, which remoses does not address.
	mainRX = '0'
)

// frontEndReads lists the front-end queries this model answers.
func (y *Rig) frontEndReads() []read {
	var reads []read
	if y.profile.Preamp > 0 {
		reads = append(reads, read{reqPA, keyPA})
	}
	if len(y.profile.Attenuator) > 0 {
		reads = append(reads, read{reqRA, keyRA})
	}
	if y.profile.RFGain {
		reads = append(reads, read{reqRG, keyRG})
	}
	if len(y.profile.AGC) > 0 {
		reads = append(reads, read{reqGT, keyGT})
	}
	return reads
}

// agcSettings lists a model's settable AGC values in a stable order.
func agcSettings(m map[radio.AGC]byte) []radio.AGC {
	if len(m) == 0 {
		return nil
	}
	order := map[radio.AGC]int{
		radio.AGCOff: 0, radio.AGCFast: 1, radio.AGCMid: 2,
		radio.AGCSlow: 3, radio.AGCAuto: 4,
	}
	out := make([]radio.AGC, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}

// attenuatorIndex is the RA parameter for a depth in dB; attenuatorDB reverses
// it. The wire carries a 1-based step, the API carries dB.
func (y *Rig) attenuatorIndex(db int) (int, bool) {
	if db == 0 {
		return 0, true
	}
	for i, step := range y.profile.Attenuator {
		if step == db {
			return i + 1, true
		}
	}
	return 0, false
}

func (y *Rig) attenuatorDB(index int) (int, bool) {
	if index == 0 {
		return 0, true
	}
	if index >= 1 && index <= len(y.profile.Attenuator) {
		return y.profile.Attenuator[index-1], true
	}
	return 0, false
}

// SetPreamp selects a preamplifier, 0 for IPO.
func (y *Rig) SetPreamp(ctx context.Context, c backend.Conn, level int) error {
	if y.profile.Preamp == 0 {
		return fmt.Errorf("yaesu: the %s has no preamplifier command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if level < 0 || level > y.profile.Preamp {
		return fmt.Errorf("yaesu: the %s has preamplifiers 0 to %d, not %d: %w",
			y.profile.Label, y.profile.Preamp, level, backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("PA%c%d;", mainRX, level)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqPA, keyPA)
	return err
}

// SetAttenuator sets the attenuator in dB, 0 for switched out.
func (y *Rig) SetAttenuator(ctx context.Context, c backend.Conn, db int) error {
	if len(y.profile.Attenuator) == 0 {
		return fmt.Errorf("yaesu: the %s has no attenuator command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	idx, ok := y.attenuatorIndex(db)
	if !ok {
		return fmt.Errorf("yaesu: the %s has no %d dB attenuator setting, only %v: %w",
			y.profile.Label, db, y.profile.Attenuator, backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("RA%c%d;", mainRX, idx)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqRA, keyRA)
	return err
}

// SetRFGain sets the receiver RF gain, 0-100% of this radio's own count.
//
// The count comes from the profile because it is not the same on all twelve:
// see Model.RFGainMax and the FT-891, whose RG is "000 - 030". A request for
// full gain sent as RG0255; there would be an out-of-range parameter, which on
// this protocol is answered with silence and a full per-command timeout.
func (y *Rig) SetRFGain(ctx context.Context, c backend.Conn, pct float64) error {
	if !y.profile.RFGain {
		return fmt.Errorf("yaesu: the %s has no RF gain command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("yaesu: RF gain %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	n := scaleTo(pct, 0, y.profile.RFGainMax)
	if err := send(ctx, c, fmt.Sprintf("RG%c%03d;", mainRX, n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqRG, keyRG)
	return err
}

// SetAGC sets the AGC speed.
func (y *Rig) SetAGC(ctx context.Context, c backend.Conn, v radio.AGC) error {
	if len(y.profile.AGC) == 0 {
		return fmt.Errorf("yaesu: the %s has no AGC command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	code, ok := y.profile.AGC[v]
	if !ok {
		return fmt.Errorf("yaesu: the %s has no AGC setting %q, only %v: %w",
			y.profile.Label, v, agcSettings(y.profile.AGC), backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("GT%c%c;", mainRX, code)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqGT, keyGT)
	return err
}

// rfGainMax is the top of the RG range on eleven of the twelve models, and the
// default the two builders in model.go start from. It is NOT a family constant:
// the FT-891 counts to 30, which is why every read and write of RG goes through
// Model.RFGainMax rather than through this.
const rfGainMax = 255

// decodePA reads the preamplifier answer: PA0<n>, the fixed 0 first.
//
// The FTdx5000 is the reason this is not a plain range check. Its PA has four
// values — "0: IPO 1, 1: AMP 1, 2: AMP 2, 3: IPO 2" — and the fourth is a
// second bypass path rather than a third amplifier, so it is published as
// preamp 0, which is exactly what is true of the amplifier. Dropping it, which
// is what a check against Preamp does on its own, would leave the previous
// reading standing in the cache for as long as the operator sat in IPO 2:
// silently stale rather than merely absent. See Model.PreampIPO2.
func (y *Rig) decodePA(u *backend.Update, arg []byte) {
	if len(arg) < 2 {
		return
	}
	n := int(arg[1] - '0')
	if y.profile.PreampIPO2 && n == y.profile.Preamp+1 {
		n = 0
	}
	if n < 0 || n > y.profile.Preamp {
		return
	}
	u.Patch.Preamp = &n
}

// decodeRA reads the attenuator answer: RA0<n>.
func (y *Rig) decodeRA(u *backend.Update, arg []byte) {
	if len(arg) < 2 {
		return
	}
	if db, ok := y.attenuatorDB(int(arg[1] - '0')); ok {
		u.Patch.AttenuatorDB = &db
	}
}

// decodeRG reads the RF gain answer: RG0<nnn>, against this model's own count.
//
// RG0030 is 11.8% on eleven of these radios and full gain on an FT-891, and
// nothing in the frame says which — only the profile does.
func (y *Rig) decodeRG(u *backend.Update, arg []byte) {
	if len(arg) < 4 {
		return
	}
	n, err := strconv.Atoi(string(arg[1:4]))
	if err != nil {
		return
	}
	pct := scaleFrom(n, 0, y.profile.RFGainMax)
	u.Patch.RFGain = &pct
}

// decodeGT reads the AGC answer, which has three values no set may carry.
func (y *Rig) decodeGT(u *backend.Update, arg []byte) {
	if len(arg) < 2 {
		return
	}
	if v, ok := agcReading(arg[1]); ok {
		u.Patch.AGC = &v
	}
}
