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
	KeyFilterWidth backend.Key = "1A/03"
	KeyDataMode    backend.Key = "1A/06"
	KeyPTT         backend.Key = "1C/00"
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
		decodeMode(&u.Patch, body)
		if cmd == cmdReadMode && u.Patch.Mode != nil {
			u.Key = KeyMode
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
		if len(body) < 1 || body[0] != subSMeter {
			return u, nil
		}
		if n, ok := decodeBCD2(body[1:]); ok {
			u.Key = KeySMeter
			m := radio.Meter{Raw: n, Scale: sMeterScale}
			u.Patch.SMeter = &m
		}
		return u, nil

	case cmdMisc:
		if len(body) < 1 {
			return u, nil
		}
		switch body[0] {
		case subFilterWidth:
			// Deliberately opaque. The 1A 03 index means a different width in
			// each mode family, and a decoder holds no state, so there is no
			// way to know which table applies. State.PassbandHz is left alone
			// rather than filled with a guess; SetFilterWidth, which can afford
			// to read the mode first, does the conversion in the other
			// direction.
			u.Key = KeyFilterWidth
		case subDataMode:
			if len(body) >= 2 {
				u.Key = KeyDataMode
				on := body[1] != 0x00
				u.Patch.DataMode = &on
				if len(body) >= 3 && body[2] >= 1 && body[2] <= filterSlots {
					slot := int(body[2])
					u.Patch.FilterSlot = &slot
				}
			}
		}
		return u, nil

	case cmdTransceiver:
		if len(body) < 2 || body[0] != subPTT {
			return u, nil
		}
		if body[1] > 0x01 {
			return u, nil
		}
		u.Key = KeyPTT
		tx := body[1] == 0x01
		u.Patch.PTT = &tx
		return u, nil
	}
	return u, nil
}

// decodeMode fills in the mode and, when the rig included it, the filter slot.
// The filter byte is optional in commands 01/04/06 and is absent when the rig
// reports a mode it has no filter selection for.
func decodeMode(p *radio.Patch, body []byte) {
	if len(body) < 1 {
		return
	}
	m, ok := modeFromByte(body[0])
	if !ok {
		return
	}
	p.Mode = &m
	if len(body) >= 2 && body[1] >= 1 && body[1] <= filterSlots {
		slot := int(body[1])
		p.FilterSlot = &slot
	}
}
