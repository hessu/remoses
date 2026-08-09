package kenwood

import (
	"context"
	"fmt"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Two VFOs, on the models whose FA and FB are two VFOs of one receiver.
//
// This is real addressing rather than "the VFO the radio is on": FA and FB read
// and set VFO A and VFO B directly, without selecting either, so a client can
// park a frequency on B while the operator works A. FR and FT then say which is
// received and which is transmitted.
//
// What the family does NOT offer is a per-VFO mode: MD applies to whichever VFO
// is selected, and there is no command that addresses the other one's mode.
// Caps.PerVFOMode is false for that reason and SetVFOMode refuses, rather than
// selecting a VFO behind the operator to reach it.

// FR and FT parameters.
const (
	vfoParamA   = '0'
	vfoParamB   = '1'
	vfoParamMem = '2'
)

// vfoParam maps a named VFO onto the FR/FT parameter.
func vfoParam(v radio.VFO) (byte, error) {
	switch v {
	case radio.VFOA:
		return vfoParamA, nil
	case radio.VFOB:
		return vfoParamB, nil
	}
	return 0, fmt.Errorf("kenwood: %s is not VFO A or VFO B: %w", v, backend.ErrUnsupported)
}

// vfoFromParam is the inverse. Memory is reported as unknown: State has no
// memory-channel concept, and calling it a VFO would be worse than saying
// nothing.
func vfoFromParam(b byte) (radio.VFO, bool) {
	switch b {
	case vfoParamA:
		return radio.VFOA, true
	case vfoParamB:
		return radio.VFOB, true
	}
	return radio.VFOCurrent, false
}

// requireTwoVFOs guards every entry point with the model's own answer.
func (k *Rig) requireTwoVFOs() error {
	if k.profile.VFOPair.twoVFOs() {
		return nil
	}
	return fmt.Errorf("kenwood: on the %s, FA and FB are the Main and Sub bands rather "+
		"than VFO A and B, so remoses does not address VFOs by name there: %w",
		k.profile.Label, backend.ErrUnsupported)
}

// rxVFO is the VFO the radio is receiving on, as last decoded from FR; or from
// an IF answer.
//
// VFOCurrent means "not yet known", which works because it is radio.VFO's zero
// value and nothing here ever stores it: an unset atomic.Value and a radio that
// has not answered FR; give the same reading without a second flag.
func (k *Rig) rxVFO() radio.VFO {
	v, _ := k.receiveVFO.Load().(radio.VFO)
	return v
}

// otherVFO is the one that is not v, for a split that has to name a transmit
// VFO the operator did not give.
func otherVFO(v radio.VFO) radio.VFO {
	if v == radio.VFOB {
		return radio.VFOA
	}
	return radio.VFOB
}

// ReadVFOs refreshes both frequencies and the receive/transmit selection.
//
// FR and FT are read as a pair because split is the relationship between them:
// this protocol has no split flag to read on its own. (IF carries one, and the
// fast poll decodes it where the model has IF, but the TS-890S has no IF at all
// and this path has to work there too.)
func (k *Rig) ReadVFOs(ctx context.Context, c backend.Conn) error {
	if err := k.requireTwoVFOs(); err != nil {
		return err
	}
	for _, r := range []read{
		{reqFA, keyFA},
		{reqFB, keyFB},
		{reqFR, keyFR},
		{reqFT, keyFT},
	} {
		if _, err := do(ctx, c, r.req, r.key); err != nil {
			return err
		}
	}
	return nil
}

// SetVFOFrequency writes one VFO's frequency by name, without selecting it.
func (k *Rig) SetVFOFrequency(ctx context.Context, c backend.Conn, vfo radio.VFO, hz uint64) error {
	if err := k.requireTwoVFOs(); err != nil {
		return err
	}
	var cmd string
	var key backend.Key
	switch vfo {
	case radio.VFOA:
		cmd, key = "FA", keyFA
	case radio.VFOB:
		cmd, key = "FB", keyFB
	default:
		return fmt.Errorf("kenwood: %s is not VFO A or VFO B: %w", vfo, backend.ErrUnsupported)
	}
	return k.writeFrequency(ctx, c, cmd, key, hz)
}

// SetVFOMode is refused on every model here.
//
// MD sets the mode of whichever VFO is selected, and nothing in this command set
// addresses the other one's. The alternative would be to select the named VFO,
// send MD and select back, which moves the operator's radio under them and
// leaves it wrong if the sequence fails halfway. Caps.PerVFOMode says so up
// front, so a client need never reach this.
func (k *Rig) SetVFOMode(ctx context.Context, c backend.Conn, vfo radio.VFO, m radio.Mode, dataMode bool, slot int) error {
	return fmt.Errorf("kenwood: the %s has no per-VFO mode command; MD applies to the "+
		"selected VFO only: %w", k.profile.Label, backend.ErrUnsupported)
}

// SetSplit moves transmit to the other VFO, or brings it back.
//
// Neither FR nor FT is a split flag; the reference describes the side effects
// instead. "When using the FR command to select VFO A or VFO B, the selected
// VFO changes to the simplex state. When using the FT command, the selected VFO
// changes to the split state." So switching split off is an FR naming the
// receive VFO, and switching it on is an FT naming the other one.
//
// Which VFO is being received therefore has to be known before either can be
// sent. It normally is — ReadVFOs reads FR at connect and on the slow poll, and
// an IF answer carries it on every fast poll — but if nothing has reported it
// yet, this reads FR rather than guessing: guessing wrong here transmits on the
// wrong frequency.
func (k *Rig) SetSplit(ctx context.Context, c backend.Conn, on bool) error {
	if err := k.requireTwoVFOs(); err != nil {
		return err
	}
	rx := k.rxVFO()
	if rx == radio.VFOCurrent {
		if _, err := do(ctx, c, reqFR, keyFR); err != nil {
			return err
		}
		if rx = k.rxVFO(); rx == radio.VFOCurrent {
			return fmt.Errorf("kenwood: the radio did not report which VFO it receives on, "+
				"so split cannot be set without guessing which VFO would transmit: %w",
				backend.ErrUnsupported)
		}
	}

	cmd, target := "FR", rx
	if on {
		cmd, target = "FT", otherVFO(rx)
	}
	p, err := vfoParam(target)
	if err != nil {
		return err
	}
	if err := send(ctx, c, fmt.Sprintf("%s%c;", cmd, p)); err != nil {
		return err
	}
	// Both are read back: FR alone cannot show that a split was cleared, since
	// the receive VFO is unchanged either way.
	if _, err := do(ctx, c, reqFR, keyFR); err != nil {
		return err
	}
	_, err = do(ctx, c, reqFT, keyFT)
	return err
}

// SetDualWatch is refused. These radios receive on one VFO at a time.
//
// The TS-990S has a second receiver, but that is not dual watch on one VFO pair
// and this backend addresses one of its bands anyway; see VFOPairMainSub.
func (k *Rig) SetDualWatch(ctx context.Context, c backend.Conn, on bool) error {
	return fmt.Errorf("kenwood: the %s receives on one VFO at a time: %w",
		k.profile.Label, backend.ErrUnsupported)
}

// storeVFOFrequency publishes a frequency into its own VFO's slot, and into
// State.Frequency when that VFO is the one being received.
//
// On a model whose FA and FB are not two VFOs (the TS-990S) only
// State.Frequency is touched, from FA: publishing its Sub band as "VFO B" is
// exactly the confusion VFOStyle exists to prevent.
func (k *Rig) storeVFOFrequency(u *backend.Update, vfo radio.VFO, hz uint64) {
	if !k.profile.VFOPair.twoVFOs() {
		if vfo == radio.VFOA {
			u.Patch.Frequency = &hz
		}
		return
	}

	st := radio.VFOState{Frequency: hz}
	if vfo == radio.VFOB {
		u.Patch.VFOB = &st
	} else {
		u.Patch.VFOA = &st
	}
	if rx := k.rxVFO(); rx == vfo || (rx == radio.VFOCurrent && vfo == radio.VFOA) {
		u.Patch.Frequency = &hz
	}
}

// decodeReceiveVFO records an FR parameter and publishes which VFO the radio is
// on. Memory is deliberately not stored: it is neither VFO, and leaving the
// last known one in place beats claiming the radio is on a VFO it has left.
func (k *Rig) decodeReceiveVFO(u *backend.Update, param byte) {
	v, ok := vfoFromParam(param)
	if !ok {
		return
	}
	k.receiveVFO.Store(v)
	u.Patch.VFO = &v
}

// transmitVFOFor is which VFO transmits, given the receive VFO and a split
// flag. It is how an IF answer — which reports split but names only the receive
// VFO — keeps the FR/FT pair consistent.
func transmitVFOFor(rx radio.VFO, split bool) radio.VFO {
	if rx == radio.VFOCurrent {
		return radio.VFOCurrent
	}
	if split {
		return otherVFO(rx)
	}
	return rx
}

// storeSplit recomputes split from the receive and transmit selections and
// returns it, or false when either is still unknown.
func (k *Rig) storeSplit() (bool, bool) {
	rx, _ := k.receiveVFO.Load().(radio.VFO)
	tx, _ := k.transmitVFO.Load().(radio.VFO)
	if rx == radio.VFOCurrent || tx == radio.VFOCurrent {
		return false, false
	}
	return rx != tx, true
}
