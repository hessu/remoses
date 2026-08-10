package kenwood

import (
	"strconv"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The transmit meters.
//
// This family splits them across two commands and neither is obvious. Forward
// power has no command of its own: SM, the S-meter read, returns the RF power
// meter instead while the rig is keyed — one command reading two different
// meters depending on whether the transmitter is up. SWR and ALC come from RM,
// which is stranger still: it selects a meter for the rig's own display, and
// reading it produces THREE answers rather than one, because the reference
// states plainly that "there are always three types of responses: SWR, COMP,
// and ALC".
//
// COMP is decoded as far as completing its transaction and then dropped, since
// State has nowhere to put a compression reading.

// RM parameter values, from the reference's P1 table.
const (
	rmNoSelection = '0'
	rmSWR         = '1'
	rmCOMP        = '2'
	rmALC         = '3'
)

// rmScale is the full-scale RM reading: "0000 ~ 0030: Meter value in dots".
//
// Unlike SM, this one does not vary by model — every reference in the family
// prints the same 30 dots — and unlike Icom's SWR meter it comes with no
// calibration, so there is no honest way to turn a deflection into a ratio.
// State.SWRRatio is left unset here and the bar is published on its own.
const rmScale = 30

// decodeRM parses one RM answer: a meter selector and four digits of dots.
func (k *Rig) decodeRM(u *backend.Update, arg []byte) {
	if len(arg) != 5 {
		return
	}
	v, err := strconv.Atoi(string(arg[1:]))
	if err != nil {
		return
	}
	m := radio.Meter{Raw: v, Scale: rmScale}
	switch arg[0] {
	case rmSWR:
		u.Patch.SWR = &m
	case rmALC:
		u.Patch.ALC = &m
	case rmCOMP, rmNoSelection:
		// Nothing to publish. The frame still completes its transaction, which
		// matters because the three answers arrive in whatever order the rig
		// sends them and any one of them may be the one a read is waiting on.
	}
}

// txMeterReads is what the fast poll adds while the transmitter is up.
//
// SM is already on the fast poll in both directions — it is the S-meter in
// receive and the power meter in transmit — so only RM is added here.
//
// A tuning cycle needs no special case here: this radio reports PTT true while
// one runs, so the meters follow by themselves — and they are real, the SWR
// visibly falling as the tuner finds its match.
func (k *Rig) txMeterReads() []read {
	if !k.transmitting.Load() {
		return nil
	}
	return []read{{reqRM, keyRM}}
}
