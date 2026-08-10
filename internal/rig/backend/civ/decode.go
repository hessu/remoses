package civ

import (
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Reply keys. The key space is the command number in hex, with the sub-command
// after a slash where one exists, so a key reads the same as the command table
// entry it came from.
const (
	// KeyAck is carried by both FB (OK) and FA (NG) frames. They share a key on
	// purpose: a set command waits for KeyAck, and a rejection then matches the
	// pending request and becomes an error for the caller instead of a timeout.
	// Update.OK is what tells the two apart.
	KeyAck backend.Key = "FB"

	KeyFrequency   backend.Key = "03"
	KeyMode        backend.Key = "04"
	KeyPower       backend.Key = "14/0A"
	KeyKeyerSpeed  backend.Key = "14/0C"
	KeySMeter      backend.Key = "15/02"
	KeyPOMeter     backend.Key = "15/11"
	KeySWRMeter    backend.Key = "15/12"
	KeyALCMeter    backend.Key = "15/13"
	KeyFilterWidth backend.Key = "1A/03"
	KeyDataMode    backend.Key = "1A/06"
	KeyPTT         backend.Key = "1C/00"
	KeyID          backend.Key = "19/00"

	// The dual-VFO commands. 25 and 26 answer for whichever VFO the request
	// named, so one key covers both bands: the session only ever has one
	// outstanding, and the band is in the answer's own first byte.
	KeyVFOFreq   backend.Key = "25"
	KeyVFOMode   backend.Key = "26"
	KeySplit     backend.Key = "0F"
	KeyDualWatch backend.Key = "07/C2"
	// KeySubSMeter is a 15 02 answer that arrived behind a 29 prefix. It needs
	// its own key because it is a different reading from the main receiver's
	// meter and must not satisfy a pending read of it.
	KeySubSMeter backend.Key = "29/15/02"
	// KeyVFOWidth is a 1A 03 answer behind a 29 prefix: one VFO's passband,
	// separate from the unprefixed read that fills the operating one.
	KeyVFOWidth backend.Key = "29/1A/03"
	// KeyBreakIn is 16 47, the CW break-in setting: the difference between a
	// CW message being transmitted and being accepted and discarded.
	KeyBreakIn backend.Key = "16/47"
)

// Decode turns one framed message into an Update.
//
// It never errors. Anything unrecognised — another controller's traffic on a
// shared bus, our own echoed frame, a command this backend does not implement,
// a field with a bad BCD digit — becomes an empty unsolicited Update, which the
// session applies as nothing. That tolerance is what lets transceive broadcasts
// (commands 00 and 01) and solicited replies share one decode path, and it
// keeps a rig that volunteers scope or memory data from breaking a session.
func (r *Rig) Decode(frame []byte) (backend.Update, error) {
	u := backend.Update{Key: backend.KeyUnsolicited, OK: true, Raw: frame}
	if !wellFormed(frame) || r.isEcho(frame) || !r.addressedToUs(frame) {
		return u, nil
	}

	cmd := frame[4]
	body := frame[5 : len(frame)-1]

	// A 29-prefixed answer carries the band and then the command it was
	// wrapping. Unwrapping it here rather than in each case keeps the decoders
	// below unaware of the prefix, but the key must still distinguish the two
	// bands' readings — see decodeBandPrefixed.
	if cmd == cmdBand && r.model.DualVFO {
		return r.decodeBandPrefixed(u, body)
	}

	switch cmd {
	case codeOK:
		u.Key = KeyAck
		return u, nil
	case codeNG:
		u.Key, u.OK = KeyAck, false
		return u, nil

	case cmdReadFreq, cmdXcvFreq:
		// 00 is the transceive broadcast of the same value 03 reports. It keeps
		// KeyUnsolicited so that a broadcast arriving mid-transaction cannot be
		// mistaken for the reply to a pending read.
		if hz, ok := decodeFrequency(body); ok {
			u.Patch.Frequency = &hz
			if cmd == cmdReadFreq {
				u.Key = KeyFrequency
			}
		}
		return u, nil

	case cmdReadMode, cmdXcvMode:
		r.decodeMode(&u.Patch, body)
		if cmd == cmdReadMode && u.Patch.Mode != nil {
			u.Key = KeyMode
		}
		return u, nil

	case cmdVFO:
		// Only 07 C2 says anything; the rest of command 07 is selection and
		// band exchange, which are actions rather than readings.
		if r.model.DualWatch && len(body) >= 2 && body[0] == subDualWatch {
			u.Key = KeyDualWatch
			on := body[1] != 0x00
			u.Patch.DualWatch = &on
			r.dualWatch.Store(on)
		}
		return u, nil

	case cmdSplit:
		// 0F answers a bare 00 or 01. Guarded by the model because a radio
		// without split would not send this, and a stray frame that shape
		// should not turn into a claim about where the transmitter is pointed.
		if r.model.Split && len(body) == 1 {
			u.Key = KeySplit
			on := body[0] != 0x00
			u.Patch.Split = &on
		}
		return u, nil

	case cmdBandFreq:
		// 25 <band> <frequency>. The band is in the answer, so one key serves
		// both and the reply says which VFO it is about.
		if r.model.DualVFO {
			r.decodeVFOFreq(&u, body)
		}
		return u, nil

	case cmdBandMode:
		// 26 <band> <mode> <data> <filter>.
		if r.model.DualVFO {
			r.decodeVFOMode(&u, body)
		}
		return u, nil

	case cmdFunc:
		// 16 is a group of on/off functions; only break-in is modelled, because
		// it is the one that decides whether CW from the computer is audible.
		if r.model.BreakIn && len(body) >= 2 && body[0] == subBreakIn {
			if v, ok := breakInValue(body[1]); ok {
				u.Key = KeyBreakIn
				u.Patch.BreakIn = &v
				r.breakIn.Store(v)
			}
		}
		return u, nil

	case cmdReadID:
		// 19 00 answers with the rig's own bus address. It carries no state, so
		// the patch stays empty and only the key is set; Init reads the value
		// back out of Raw. See Rig.checkIdentity for why this is a cross-check
		// and not model detection.
		if len(body) >= 2 && body[0] == subReadID {
			u.Key = KeyID
		}
		return u, nil

	case cmdLevel:
		if len(body) < 1 {
			return u, nil
		}
		switch body[0] {
		case subRFPower:
			if n, ok := decodeBCD2(body[1:]); ok {
				u.Key = KeyPower
				p := radio.Power{
					// Watts stays nil: 14 0A is a relative index with no watt
					// meaning anywhere in the reference.
					Pct:    float64(n) / levelMax * 100,
					Native: n,
				}
				u.Patch.Power = &p
			}
		case subKeyerSpeed:
			// radio.Patch has no keyer speed field — the CW layer owns wpm — so
			// this only resolves the pending request.
			if _, ok := decodeBCD2(body[1:]); ok {
				u.Key = KeyKeyerSpeed
			}
		}
		return u, nil

	case cmdMeter:
		if len(body) < 1 {
			return u, nil
		}
		n, ok := decodeBCD2(body[1:])
		if !ok {
			return u, nil
		}
		switch body[0] {
		case subSMeter:
			u.Key = KeySMeter
			m := radio.Meter{Raw: n, Scale: sMeterScale}
			u.Patch.SMeter = &m
		case subPOMeter:
			// Full scale is per model: 255 on an IC-7610, 213 on an IC-9700.
			u.Key = KeyPOMeter
			m := radio.Meter{Raw: n, Scale: r.model.POScale}
			u.Patch.PowerMeter = &m
		case subSWRMeter:
			// Against the top of the calibrated range, not the width of the
			// data field: see swrScale.
			u.Key = KeySWRMeter
			m := radio.Meter{Raw: min(n, swrScale), Scale: swrScale}
			u.Patch.SWR = &m
			// And a ratio too, but only where this radio's own reference
			// prints the four points that define one.
			if r.model.SWRCal {
				if ratio, ok := swrRatio(n); ok {
					u.Patch.SWRRatio = &ratio
				}
			}
		case subALCMeter:
			// "0000=Minimum to 0120=Maximum": the ALC meter runs to 120, not
			// to the 255 its data field could hold.
			u.Key = KeyALCMeter
			m := radio.Meter{Raw: n, Scale: alcScale}
			u.Patch.ALC = &m
		}
		return u, nil

	case cmdMisc:
		if len(body) < 1 {
			return u, nil
		}
		switch {
		case body[0] == subFilterWidth && r.model.FilterWidth:
			// The index means a different width in each mode family, so it can
			// only be read against the mode the rig is in — which decodeMode
			// keeps for exactly this. An earlier version of this decoder
			// published nothing at all here, on the grounds that a decoder is
			// stateless; the cost was that passband_hz stayed 0 for ever on
			// every Icom while caps.filter_width advertised true, which is a
			// promise to a client that nothing ever kept. Confirmed on an
			// IC-7610: it answers 1A 03 with 16 in CW, which is 1200 Hz.
			//
			// Where the mode has no width table — FM, DV, DD, ATV — nothing is
			// published, because a number taken from the wrong column would
			// look exactly like a real one.
			u.Key = KeyFilterWidth
			if len(body) >= 2 {
				if n, ok := unbcdByte(body[1]); ok {
					if hz, ok := filterWidthHz(radio.Mode(r.mode.Load()), n); ok {
						u.Patch.PassbandHz = &hz
					}
				}
			}
		// Guarded by the model for the same reason the setter is: the
		// sub-command that carries data mode on most of the family is RIT on
		// the IC-910H, and decoding an RIT report as a data-mode change would
		// put a wrong value into state rather than merely miss one. Which
		// sub-command it is also varies — 1A 04 on the IC-703 — so this cannot
		// match a constant either.
		case body[0] == r.model.DataModeSub && r.model.DataMode:
			if len(body) >= 2 {
				u.Key = KeyDataMode
				on := body[1] != 0x00
				u.Patch.DataMode = &on
				if len(body) >= 3 {
					if slot, ok := r.model.filterSlot(body[2]); ok {
						u.Patch.FilterSlot = &slot
					}
				}
			}
		}
		return u, nil

	case cmdTransceiver:
		// The sub-command carrying transmitter status is per model: 0x00 on
		// every radio here except the IC-718, whose table puts it on 0x01 and
		// has no 0x00 row at all. Matching on the model's value rather than a
		// constant also keeps 1C 01 on the other radios — where it is the
		// antenna tuner — from being decoded as PTT.
		if len(body) < 2 || body[0] != r.model.PTTSub {
			return u, nil
		}
		if body[1] > 0x01 {
			return u, nil
		}
		u.Key = KeyPTT
		tx := body[1] == 0x01
		u.Patch.PTT = &tx
		// Remembered for the poll, which asks for the transmit meters only
		// while this is set. Storing it here rather than in SetPTT means a
		// transmission started at the radio's own PTT switch counts too.
		r.transmitting.Store(tx)
		return u, nil
	}
	return u, nil
}

// decodeMode fills in the mode and, when the rig included it, the filter slot.
// The filter byte is optional in commands 01/04/06 and is absent when the rig
// reports a mode it has no filter selection for.
func (r *Rig) decodeMode(p *radio.Patch, body []byte) {
	if len(body) < 1 {
		return
	}
	m, ok := r.model.modeFromByte(body[0])
	if !ok {
		return
	}
	p.Mode = &m
	// Kept so that a 1A 03 answer arriving later can be turned into a width.
	// The mode is read on the fast tier and the filter width on the slow one,
	// so by the time a filter frame arrives this is at most one fast tick old —
	// and a mode change re-reads the width anyway, because the session's
	// read-back after a write covers both tiers.
	r.mode.Store(uint32(m))
	// A radio with no filter selection (IC-718, IC-910H) reports FilterSlots 0,
	// and then any trailing byte is not a slot and must not be read as one.
	// Where there is one, the wire byte is not always the slot number: the
	// IC-706 family counts from zero. See Model.filterSlot.
	if len(body) >= 2 {
		if slot, ok := r.model.filterSlot(body[1]); ok {
			p.FilterSlot = &slot
		}
	}
}

// decodeBandPrefixed unwraps a 29-prefixed answer.
//
// The prefix names the band and is followed by the command it wrapped, so the
// reading has to be attributed to that band rather than folded into the main
// receiver's fields. Only the readings remoses asks for behind the prefix are
// decoded; anything else keeps the empty unsolicited Update, because a frame
// this backend did not provoke is not evidence about which band it describes.
func (r *Rig) decodeBandPrefixed(u backend.Update, body []byte) (backend.Update, error) {
	if len(body) < 2 {
		return u, nil
	}
	vfo, ok := vfoForBand(body[0])
	if !ok {
		return u, nil
	}
	inner, rest := body[1], body[2:]

	// 29 <band> 15 02 <two bytes>: the S-meter of one receiver. Published only
	// for the sub band, because the main one already has its own unprefixed
	// read and two sources for one field would fight.
	if inner == cmdMeter && len(rest) >= 1 && rest[0] == subSMeter && vfo == radio.VFOB {
		if n, okv := decodeBCD2(rest[1:]); okv {
			u.Key = KeySubSMeter
			m := radio.Meter{Raw: n, Scale: sMeterScale}
			u.Patch.SubSMeter = &m
		}
	}

	// 29 <band> 1A 03 <index>: that VFO's passband. The index means a different
	// width in each mode family, so it is read against the mode command 26
	// reported for the SAME VFO — not the operating one, which is the whole
	// point of asking per VFO.
	if inner == cmdMisc && r.model.FilterWidth && len(rest) >= 2 && rest[0] == subFilterWidth {
		u.Key = KeyVFOWidth
		if n, okv := unbcdByte(rest[1]); okv {
			st := r.vfoSnapshot(vfo)
			if hz, okw := filterWidthHz(st.Mode, n); okw {
				st.PassbandHz = hz
				r.storeVFO(vfo, st)
				if vfo == radio.VFOA {
					u.Patch.VFOA = &st
				} else {
					u.Patch.VFOB = &st
				}
			}
		}
	}
	return u, nil
}

// decodeVFOFreq parses a command 25 answer: the band, then the ordinary
// frequency field.
func (r *Rig) decodeVFOFreq(u *backend.Update, body []byte) {
	if len(body) < 2 {
		return
	}
	vfo, ok := vfoForBand(body[0])
	if !ok {
		return
	}
	hz, ok := decodeFrequency(body[1:])
	if !ok {
		return
	}
	u.Key = KeyVFOFreq
	// Only the frequency is known here, so the rest of the VFO is carried
	// through unchanged from the cache by the session's Apply. Patch replaces a
	// whole VFOState, so the fields this frame says nothing about are filled
	// from what the backend last read — see r.vfo.
	st := r.vfoSnapshot(vfo)
	st.Frequency = hz
	r.storeVFO(vfo, st)
	if vfo == radio.VFOA {
		u.Patch.VFOA = &st
	} else {
		u.Patch.VFOB = &st
	}
}

// decodeVFOMode parses a command 26 answer: band, operating mode, data mode and
// filter, which the reference gives as one frame because they belong together.
func (r *Rig) decodeVFOMode(u *backend.Update, body []byte) {
	if len(body) < 2 {
		return
	}
	vfo, ok := vfoForBand(body[0])
	if !ok {
		return
	}
	m, ok := r.model.modeFromByte(body[1])
	if !ok {
		return
	}
	st := r.vfoSnapshot(vfo)
	st.Mode = m

	// The data and filter bytes are optional in a set and always present in an
	// answer, but a short frame is read for what it has rather than discarded:
	// the mode is the field a client is most likely to be watching.
	if len(body) >= 3 {
		st.DataMode = body[2] != bandDataOff
	}
	if len(body) >= 4 {
		if slot, ok := r.model.filterSlot(body[3]); ok {
			st.FilterSlot = slot
		}
	}

	u.Key = KeyVFOMode
	r.storeVFO(vfo, st)
	if vfo == radio.VFOA {
		u.Patch.VFOA = &st
		u.Patch.VFO = r.operatingVFO()
	} else {
		u.Patch.VFOB = &st
	}
}

// operatingVFO is which VFO the top-level state fields describe.
//
// Always A on the radios this backend can address by name. The IC-7610 has no
// A/B switch: both of its VFOs are real receivers, A is what it receives and
// transmits on, and B joins in under dual watch or takes the transmit under
// split. So there is nothing to read and nothing to select — publishing A is a
// statement about the radio's design, not a guess about its current state.
//
// A radio that did switch would have to report this from the wire; the shape is
// a pointer so that adding one later is a change here and nowhere else.
func (r *Rig) operatingVFO() *radio.VFO {
	v := radio.VFOA
	return &v
}
