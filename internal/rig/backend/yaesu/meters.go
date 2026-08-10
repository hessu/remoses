package yaesu

import (
	"strconv"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The transmit meters, all on one command: RM, READ METER, whose parameter
// picks which meter to read.
//
// RM appears in the command list of every Yaesu reference read for this
// backend, both generations of it, and the parameter table is the same in each:
// the FT-950's and the FT-710's agree meter for meter. That is why this is
// family-wide rather than per model, unlike most of what this backend records.
//
// The answer is not quite the same on both, and the difference is silent rather
// than an error: the older generation answers RM<meter><nnn>; and the newer
// appends three more fixed digits. Reading the meter and the three digits after
// it and ignoring the rest handles both without needing to know which radio
// this is.
const (
	rmS    = '1' // S meter
	rmCOMP = '3'
	rmALC  = '4'
	rmPO   = '5' // forward power
	rmSWR  = '6'
)

// Read forms for the three meters remoses publishes.
const (
	reqRMPower = "RM5;"
	reqRMSWR   = "RM6;"
	reqRMALC   = "RM4;"
)

// rmScale is the range of RM's value field on every model here: "P2 0 - 255".
//
// No reference gives a calibration for the SWR meter — no table of deflection
// against ratio, as Icom prints — so State.SWRRatio stays unset and a client
// gets the bar and nothing implied about what it means in ratio terms.
const rmScale = 255

// decodeRM parses one RM answer into whichever meter it names.
func (y *Rig) decodeRM(u *backend.Update, arg []byte) {
	if len(arg) < 4 {
		return
	}
	v, err := strconv.Atoi(string(arg[1:4]))
	if err != nil {
		return
	}
	m := radio.Meter{Raw: v, Scale: rmScale}
	switch arg[0] {
	case rmPO:
		u.Patch.PowerMeter = &m
	case rmSWR:
		u.Patch.SWR = &m
	case rmALC:
		u.Patch.ALC = &m
	case rmS:
		// The S-meter is already read with SM on the fast poll, and this is
		// the same reading by another name. Nothing is published, so that two
		// commands cannot disagree about one value.
	case rmCOMP:
		// Compression has no home in State.
	}
}

// txMeterReads is what the fast poll adds while the transmitter is up.
//
// Nothing at all in receive: forward power, SWR and ALC all read zero there and
// would cost three transactions a tick to publish three zeroes that a client
// could not tell from a real reading of a transmitter into a dummy load.
// A tuning cycle gets no special case: whether a radio reports PTT during one
// is per model — a TS-590S does and an IC-7610 does not — so the rig's own PTT
// is followed rather than second-guessed. See radio.State.Apply.
func (y *Rig) txMeterReads() []read {
	if !y.transmitting.Load() {
		return nil
	}
	return []read{
		{reqRMPower, keyRM},
		{reqRMSWR, keyRM},
		{reqRMALC, keyRM},
	}
}
