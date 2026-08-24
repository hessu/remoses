package civ

import (
	"context"
	"fmt"

	"github.com/hessu/remoses/internal/rig/backend"
)

// The transmit audio chain: the gain into the modulator (14 0B), the speech
// compressor's switch (16 44) and how hard it squeezes (14 0E).
//
// Transcribed from the IC-7610 CI-V Reference Guide's command table, pages 3
// and 4, and checked against every other Icom reference this backend has: all
// thirteen radios here with a command 14 put the mic gain on 0B over 0-255, and
// all fourteen with a command 16 put the compressor on 44 as a two-value switch.
// Nothing else in this family is that uniform. Which model has WHICH of the
// three is not uniform at all — see the model table.
//
// Two things about these numbers are worth keeping in view, because both are
// the kind of detail that reads as a rounding error and is not:
//
//   - 14 0E's field is 0000-0255 like every other level in the group, but the
//     scale underneath it is usually the radio's own compressor setting of 0 to
//     10 — "00 00=0 ~ 02 55=10" in the reference. remoses publishes a
//     percentage, as it does for the RF gain and the noise levels, so a client
//     reading 50% and a radio displaying 5 are saying the same thing. The
//     IC-910H puts a plain 0-100% on the same field, which needs no special
//     case, and the IC-703 makes the FIELD 0-10, which does — so that one radio
//     does not claim the capability at all. See Model.ProcLevel.
//   - The compressor also moves a setting nobody asked about. 16 58, the SSB
//     transmit bandwidth, takes "one of following values ... depending on the
//     'COMP' status (ON or OFF)" — three different Set-mode items for wide,
//     mid and narrow. So switching the compressor in or out changes the
//     transmit passband as a side effect, at the radio's own choosing. remoses
//     neither models 16 58 nor tries to hold it still; this is recorded because
//     an operator watching a waterfall will see it happen.
//
// The gain is deliberately NOT named after a socket. Which connector feeds the
// modulator is a Set-mode item on this family — 1A 05 00 91 for DATA OFF and
// 1A 05 00 92/93/94 for DATA1/2/3, each choosing between MIC, ACC, USB and LAN
// — and remoses does not read it, let alone write it. So 14 0B is the transmit
// gain and no more; see radio.State.TXAudioGain, which says the same thing to a
// client.
//
// Which is why the names differ either side of this file: Icom's command is
// "MIC gain", and it keeps that name here and in the model table so the
// reference guides stay searchable from the code, but the API publishes it as
// tx_audio_gain, after what the setting actually does.

// SetTXAudioGain sets the gain into the modulator, 0-100% (14 0B).
func (r *Rig) SetTXAudioGain(ctx context.Context, c backend.Conn, pct float64) error {
	if !r.model.MicGain {
		return fmt.Errorf("civ: the %s has no microphone gain command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	return r.setLevel(ctx, c, "microphone gain", subMicGain, pct)
}

// SetProc switches the speech compressor in or out (16 44).
func (r *Rig) SetProc(ctx context.Context, c backend.Conn, on bool) error {
	if !r.model.Proc {
		return fmt.Errorf("civ: the %s has no speech compressor command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	return r.set(ctx, c, "speech compressor", r.frame(cmdFunc, subProc, onOff(on)))
}

// SetProcLevel sets how hard the compressor squeezes, 0-100% of the radio's own
// 0-10 scale (14 0E).
func (r *Rig) SetProcLevel(ctx context.Context, c backend.Conn, pct float64) error {
	if !r.model.ProcLevel {
		return fmt.Errorf("civ: the %s has no speech compressor level command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	return r.setLevel(ctx, c, "speech compressor level", subProcLevel, pct)
}

// txAudioReads lists this model's transmit audio queries for the slow poll.
//
// The compressor's level is read whether or not the compressor is on, which is
// deliberate and is what radio.State.ProcLevel documents: the setting is
// remembered by the radio, and a client redrawing the control wants the value
// it will return to. The same reasoning polls the DIGI-SEL shift with the
// preselector switched out. Nothing in any of these references says the level
// is refused while the compressor is off — that is Kenwood's behaviour with its
// noise levels, not this family's.
func (r *Rig) txAudioReads() []request {
	var reqs []request
	if r.model.MicGain {
		reqs = append(reqs, request{KeyMicGain, r.frame(cmdLevel, subMicGain)})
	}
	if r.model.Proc {
		reqs = append(reqs, request{KeyProc, r.frame(cmdFunc, subProc)})
	}
	if r.model.ProcLevel {
		reqs = append(reqs, request{KeyProcLevel, r.frame(cmdLevel, subProcLevel)})
	}
	return reqs
}
