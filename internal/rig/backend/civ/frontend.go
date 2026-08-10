package civ

import (
	"context"
	"fmt"
	"sort"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The receive front end: the preamplifier (16 02), the attenuator (11), the RF
// gain (14 02) and the AGC (16 12), plus Icom's two extras — IP+ (16 65) and the
// DIGI-SEL preselector (16 4E) with its shift (14 13).
//
// Every one of them is gated by the model, and for the usual reason: an opcode
// that is one control on one radio is another control, or nothing, on the next.
// The concrete traps here are the attenuator byte, which is a depth in BCD
// everywhere except the IC-718 where it is an index, and command 16 12, whose
// five known encodings share nothing but the opcode. Both live in the model
// table rather than in this file.

// attenuatorByte encodes a dB depth as this radio's command 11 data byte.
func (r *Rig) attenuatorByte(db int) (byte, error) {
	if db == 0 {
		return attenuatorOff, nil
	}
	for i, step := range r.model.Attenuator {
		if step != db {
			continue
		}
		if r.model.AttenuatorLiteral {
			// An index into the ladder, 1-based: the IC-718's "01=20dB".
			return byte(i + 1), nil
		}
		// The depth itself, in BCD: 20 dB is the byte 0x20, not 20.
		return bcdByte(db), nil
	}
	return 0, fmt.Errorf("civ: the %s has no %d dB attenuator setting, only %v: %w",
		r.model.Label, db, r.model.Attenuator, backend.ErrUnsupported)
}

// attenuatorDB decodes a command 11 answer back to a depth.
func (r *Rig) attenuatorDB(b byte) (int, bool) {
	if b == attenuatorOff {
		return 0, true
	}
	if r.model.AttenuatorLiteral {
		i := int(b)
		if i >= 1 && i <= len(r.model.Attenuator) {
			return r.model.Attenuator[i-1], true
		}
		return 0, false
	}
	db, ok := unbcdByte(b)
	if !ok {
		return 0, false
	}
	// Only depths this radio is documented to have. A byte outside the ladder
	// is a frame this backend has misread rather than a setting to publish —
	// the IC-910H's table, for one, lists three bytes for a single pad.
	for _, step := range r.model.Attenuator {
		if step == db {
			return db, true
		}
	}
	return 0, false
}

// agcValue decodes a command 16 12 byte against a model's own map.
func agcValue(m map[radio.AGC]byte, b byte) (radio.AGC, bool) {
	for v, code := range m {
		if code == b {
			return v, true
		}
	}
	return radio.AGCUnknown, false
}

// agcSettings lists a model's AGC speeds in a stable, sensible order: off
// first, then fastest to slowest, which is how a radio's own menu reads.
func agcSettings(m map[radio.AGC]byte) []radio.AGC {
	if len(m) == 0 {
		return nil
	}
	order := map[radio.AGC]int{
		radio.AGCOff: 0, radio.AGCFast: 1, radio.AGCMid: 2, radio.AGCSlow: 3,
	}
	out := make([]radio.AGC, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}

// SetPreamp selects a preamplifier, 0 for off.
func (r *Rig) SetPreamp(ctx context.Context, c backend.Conn, level int) error {
	if r.model.Preamp == 0 {
		return fmt.Errorf("civ: the %s has no preamplifier on the bus: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	if level < 0 || level > r.model.Preamp {
		return fmt.Errorf("civ: the %s has preamplifiers 0 to %d, not %d: %w",
			r.model.Label, r.model.Preamp, level, backend.ErrUnsupported)
	}
	err := r.set(ctx, c, "preamplifier", r.frame(cmdFunc, subPreamp, byte(level)))
	if err != nil && level > 0 && r.model.DigiSel && r.digiSel.Load() {
		// The radio refused, and there is a known reason it refuses exactly
		// this. See Rig.digiSel: an IC-7610 with the preselector engaged will
		// not switch a preamplifier in, and says so only as a bare NG.
		//
		// The hint is added rather than substituted, and remoses does not
		// switch DIGI-SEL off to make the request succeed: that would be
		// changing a second control the operator did not ask about, on a
		// receiver they are listening to.
		return fmt.Errorf("%w (DIGI-SEL is engaged, and this radio will not "+
			"switch a preamplifier in behind it; set digi_sel false first)", err)
	}
	return err
}

// SetAttenuator sets the front-end pad, 0 dB for switched out.
func (r *Rig) SetAttenuator(ctx context.Context, c backend.Conn, db int) error {
	if len(r.model.Attenuator) == 0 {
		return fmt.Errorf("civ: the %s has no attenuator on the bus: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	v, err := r.attenuatorByte(db)
	if err != nil {
		return err
	}
	// Command 11 takes no sub-command: the data byte follows the opcode.
	return r.set(ctx, c, "attenuator", r.frame(cmdAttenuator, v))
}

// SetRFGain sets the receiver RF gain, 0-100%.
func (r *Rig) SetRFGain(ctx context.Context, c backend.Conn, pct float64) error {
	if !r.model.RFGain {
		return fmt.Errorf("civ: the %s has no RF gain command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	return r.setLevel(ctx, c, "RF gain", subRFGain, pct)
}

// SetAGC sets the AGC speed.
func (r *Rig) SetAGC(ctx context.Context, c backend.Conn, v radio.AGC) error {
	if len(r.model.AGC) == 0 {
		return fmt.Errorf("civ: the %s has no AGC command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	code, ok := r.model.AGC[v]
	if !ok {
		return fmt.Errorf("civ: the %s has no AGC setting %q, only %v: %w",
			r.model.Label, v, agcSettings(r.model.AGC), backend.ErrUnsupported)
	}
	err := r.set(ctx, c, "AGC", r.frame(cmdFunc, subAGC, code))
	if err != nil && radio.Mode(r.mode.Load()) == radio.ModeFM {
		// Refused, and there is a known reason it is refused in this mode: the
		// AGC is fixed in FM. Verified on an IC-9700, which sets all three
		// speeds in USB and answers NG to every one of them in FM — while still
		// ANSWERING a read with "fast", so nothing but the refusal says so.
		//
		// Kenwood documents the same restriction for its own AGC commands and
		// this backend's Icom references do not, which is why it is reported
		// from what the radio did rather than guarded before the write: on a
		// model that turns out to allow it, the command still goes out.
		return fmt.Errorf("%w (the AGC is fixed in FM on this radio; "+
			"it can be set in the other modes)", err)
	}
	return err
}

// SetIPPlus switches IP+.
func (r *Rig) SetIPPlus(ctx context.Context, c backend.Conn, on bool) error {
	if !r.model.IPPlus {
		return fmt.Errorf("civ: the %s has no IP+ function: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	return r.set(ctx, c, "IP+", r.frame(cmdFunc, subIPPlus, onOff(on)))
}

// SetDigiSel switches the DIGI-SEL preselector in or out of line.
func (r *Rig) SetDigiSel(ctx context.Context, c backend.Conn, on bool) error {
	if !r.model.DigiSel {
		return fmt.Errorf("civ: the %s has no DIGI-SEL preselector: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	return r.set(ctx, c, "DIGI-SEL", r.frame(cmdFunc, subDigiSel, onOff(on)))
}

// SetDigiSelShift moves the preselector, 0-100%.
func (r *Rig) SetDigiSelShift(ctx context.Context, c backend.Conn, pct float64) error {
	if !r.model.DigiSelShift {
		return fmt.Errorf("civ: the %s has no DIGI-SEL shift command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	return r.setLevel(ctx, c, "DIGI-SEL shift", subDigiSelShift, pct)
}

// setLevel writes one of the command 14 analogue levels from a percentage.
//
// The rounding is deliberate rather than a truncation: 0-255 over 0-100% is
// 2.55 counts per point, so truncating would put 100% at 254 and leave a control
// that can never reach its own top.
func (r *Rig) setLevel(ctx context.Context, c backend.Conn, what string, sub byte, pct float64) error {
	if pct < 0 || pct > 100 {
		return fmt.Errorf("civ: %s %.1f%% is outside 0-100%%: %w",
			what, pct, backend.ErrUnsupported)
	}
	n := min(max(int(pct/100*levelMax+0.5), 0), levelMax)
	v := encodeBCD2(n)
	return r.set(ctx, c, what, r.frame(cmdLevel, sub, v[0], v[1]))
}

// onOff is the 00/01 every command 16 function takes.
func onOff(on bool) byte {
	if on {
		return 0x01
	}
	return 0x00
}
