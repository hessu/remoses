package civ

import "bytes"

// Wire constants. FE, FD, FB and FA are reserved by the protocol and can never
// appear as an address or in a data field, which is what makes framing on FE FE
// ... FD reliable.
const (
	preamble = 0xFE // two of these open a frame
	eom      = 0xFD // end of message
	codeOK   = 0xFB // rig acknowledgement
	codeNG   = 0xFA // rig rejection

	// addrBroadcast is the destination the rig puts on transceive frames, which
	// are addressed to every controller on the bus rather than to us.
	addrBroadcast = 0x00

	// DefaultRigAddress is the IC-7610 factory default (SET > Connectors >
	// CI-V > CI-V Address). Other Icom models differ, hence the config field.
	DefaultRigAddress = 0x98
	// DefaultControllerAddress is the conventional PC address.
	DefaultControllerAddress = 0xE0
)

// Command numbers used by this backend, from the reference command table.
const (
	cmdXcvFreq     = 0x00 // transceive broadcast: frequency changed
	cmdXcvMode     = 0x01 // transceive broadcast: mode changed
	cmdReadFreq    = 0x03
	cmdReadMode    = 0x04
	cmdSetFreq     = 0x05
	cmdSetMode     = 0x06
	cmdLevel       = 0x14 // analogue levels, sub-command selects which
	cmdMeter       = 0x15 // meters and squelch status
	cmdSendCW      = 0x17 // CW message, up to 30 characters
	cmdReadID      = 0x19 // 19 00, reads the rig's own bus address
	cmdMisc        = 0x1A // memories, filters, and the whole Set-mode menu
	cmdTransceiver = 0x1C // transmitter status, tuner, XFC

	// The dual-VFO commands, from the IC-7610 CI-V Reference Guide. Not every
	// Icom has them — Model.DualVFO gates all of them — and where a radio does,
	// they are strictly better than the single-VFO equivalents above, because
	// they name the VFO instead of operating on whichever one is selected.
	cmdVFO      = 0x07 // VFO selection, band exchange, dual watch
	cmdSplit    = 0x0F // 0F read, 0F 00 off, 0F 01 on
	cmdBandFreq = 0x25 // 25 <band> [freq]: per-VFO frequency
	cmdBandMode = 0x26 // 26 <band> [mode data filter]: per-VFO mode, atomically
	// cmdBand is the prefix that addresses the inactive VFO. "Regardless of
	// active/inactive the Main or Sub band, you can directly specify the Main
	// or Sub band, and send/read the supported command settings." Only the
	// commands its table marks are supported; frequency and mode are NOT among
	// them, which is exactly why 25 and 26 exist.
	cmdBand = 0x29

	subDualWatchOff = 0xC0 // 07 C0
	subDualWatchOn  = 0xC1 // 07 C1
	subDualWatch    = 0xC2 // 07 C2, read/set, 00 off 01 on
	subSelectMain   = 0xD0 // 07 D0
	subSelectSub    = 0xD1 // 07 D1
	subBandSelected = 0xD2 // 07 D2, read which band is selected

	// The band selector carried by 25, 26 and the 29 prefix. remoses addresses
	// these as VFO A and VFO B, which is how the radio's operator thinks of
	// them; the reference calls them the main and sub bands.
	bandMain = 0x00
	bandSub  = 0x01

	subRFPower     = 0x0A // 14 0A, 0000-0255 relative
	subKeyerSpeed  = 0x0C // 14 0C, 0000-0255 mapped to 6-48 wpm
	subSMeter      = 0x02 // 15 02, 0000-0255
	subFilterWidth = 0x03 // 1A 03, mode-dependent index
	subDataMode    = 0x06 // 1A 06, data mode plus filter
	subPTT         = 0x00 // 1C 00, 00 RX / 01 TX
	subReadID      = 0x00 // 19 00, transceiver ID
)

const (
	// minFrameLen is FE FE to from cmd FD, the shortest legal frame (an FB or
	// FA acknowledgement).
	minFrameLen = 6

	// maxFrameLen bounds how far Split will look for the end of a frame before
	// deciding the preamble was noise. It is well above the longest frame the
	// rig can send (the command 27 scope waveform, a few hundred bytes), and
	// exists so that a stuck or noisy line resynchronises instead of growing
	// the scanner's buffer until it errors out.
	maxFrameLen = 1024
)

// frame builds a controller-to-rig frame.
func (r *Rig) frame(cmd byte, data ...byte) []byte {
	f := make([]byte, 0, minFrameLen+len(data))
	f = append(f, preamble, preamble, r.rigAddr, r.ctrlAddr, cmd)
	f = append(f, data...)
	return append(f, eom)
}

// Split is the bufio.SplitFunc over the inbound stream.
//
// Two things it must survive, both routine rather than exotic: a rig that is
// powering up emits noise before its first real frame, and the power-on command
// (18 01) is documented as needing dozens of repeated FE bytes, so a preamble is
// not reliably exactly two bytes long. Anything that is not a well-formed frame
// is therefore skipped rather than reported: erroring here would tear down a
// session over a byte of line noise.
func (r *Rig) Split(data []byte, atEOF bool) (advance int, token []byte, err error) {
	// Find the first FE FE, discarding anything before it.
	i := 0
	for {
		j := bytes.IndexByte(data[i:], preamble)
		if j < 0 {
			return len(data), nil, nil // no preamble byte at all: all noise
		}
		i += j
		if i+1 >= len(data) {
			// A trailing lone FE may be the first half of a preamble that has
			// not arrived yet, so keep it and drop what came before.
			if atEOF {
				return len(data), nil, nil
			}
			return i, nil, nil
		}
		if data[i+1] == preamble {
			break
		}
		i++ // an FE with something else after it cannot start a frame
	}

	// Consume the whole run of FE. The frame proper begins at the last two of
	// them, so an over-long preamble costs nothing.
	k := i
	for k < len(data) && data[k] == preamble {
		k++
	}
	if k == len(data) && !atEOF {
		// Still inside the run. Drop all but the two FE we need to keep, so a
		// long wake-up preamble cannot accumulate in the buffer.
		return k - 2, nil, nil
	}
	start := k - 2

	// Scan the body. Both FD and FE are reserved codes that cannot occur inside
	// a frame, so whichever comes first ends the search: FD terminates the
	// frame, while an FE means the rig stopped part way through one and a new
	// frame is starting, so the incomplete bytes are dropped.
	for j := k; j < len(data); j++ {
		switch data[j] {
		case eom:
			end := j + 1
			if end-start < minFrameLen {
				// A runt such as FE FE FD. Drop it and resynchronise.
				return end, nil, nil
			}
			return end, data[start:end], nil
		case preamble:
			return j, nil, nil
		}
	}

	switch {
	case atEOF:
		return len(data), nil, nil // truncated frame at end of stream
	case len(data)-start >= maxFrameLen:
		// No terminator within a plausible frame's worth of data: treat the
		// preamble as noise and resynchronise past it.
		return k, nil, nil
	default:
		return start, nil, nil // wait for the rest of the frame
	}
}

// wellFormed reports whether a frame from Split has the expected envelope. It
// is a cheap re-check rather than a trust boundary: Split only ever emits
// frames shaped like this, but Decode is also called directly from tests and
// from replayed captures.
func wellFormed(f []byte) bool {
	return len(f) >= minFrameLen && f[0] == preamble && f[1] == preamble && f[len(f)-1] == eom
}

// isEcho reports whether a frame is one of ours coming back.
//
// The 13-pin CI-V bus is a single wire shared by every station on it, so the
// rig repeats our frame before answering, and USB does the same when "CI-V USB
// Echo Back" is enabled. Our own frames are recognisable by their direction:
// addressed to the rig, sent from the controller.
func (r *Rig) isEcho(f []byte) bool {
	return wellFormed(f) && f[2] == r.rigAddr && f[3] == r.ctrlAddr
}

// addressedToUs reports whether a frame is one we should interpret: sent by our
// rig, and addressed either to us or to the whole bus (transceive broadcasts
// use destination 0x00). Frames between other stations on a shared bus are
// none of our business.
func (r *Rig) addressedToUs(f []byte) bool {
	if !wellFormed(f) {
		return false
	}
	if f[3] != r.rigAddr {
		return false
	}
	return f[2] == r.ctrlAddr || f[2] == addrBroadcast
}
