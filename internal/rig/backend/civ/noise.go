package civ

import (
	"context"
	"fmt"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The noise processing and the notch filters.
//
// The switches are all in the 16 group and the levels all in the 14 group, and
// on this family they are genuinely independent: the manual notch (16 48) and
// the automatic one (16 41) are separate commands and both can be on at once,
// which is why the API keeps them as two fields rather than one selector.
//
// Every one is gated per model, and the older radios are why that is granular
// rather than a single flag. An IC-703 has the blanker, the reducer and the
// AUTOMATIC notch and no manual one; an IC-718 adds a reducer level and still
// has no manual notch; an IC-706MKIIG has the blanker alone. Only the modern
// sets have the group entire.

// boolCount turns "the radio has this circuit" into the count Caps publishes.
// One is as many as this family has of either.
func boolCount(has bool) int {
	if has {
		return 1
	}
	return 0
}

// notchWidthByte maps a published width onto 16 57's data byte, and back.
//
// "00=WIDE, 01=MID, 02=NAR" — an index, not a bandwidth, and the radio's own
// numbers in Hz depend on the mode, which is why the API carries the names.
var notchWidthByte = map[radio.NotchWidth]byte{
	radio.NotchWidthWide:   0x00,
	radio.NotchWidthMid:    0x01,
	radio.NotchWidthNarrow: 0x02,
}

func notchWidthValue(b byte) (radio.NotchWidth, bool) {
	for w, code := range notchWidthByte {
		if code == b {
			return w, true
		}
	}
	return radio.NotchWidthUnknown, false
}

// notchWidths is what this model accepts, in the order the radio's own menu
// reads: widest first.
func (m Model) notchWidths() []radio.NotchWidth {
	if !m.NotchWidth {
		return nil
	}
	return []radio.NotchWidth{
		radio.NotchWidthWide, radio.NotchWidthMid, radio.NotchWidthNarrow,
	}
}

// SetNoiseBlanker switches the blanker. This family has one, so the only levels
// are 0 and 1.
func (r *Rig) SetNoiseBlanker(ctx context.Context, c backend.Conn, level int) error {
	if !r.model.NoiseBlanker {
		return fmt.Errorf("civ: the %s has no noise blanker command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	if level < 0 || level > 1 {
		return fmt.Errorf("civ: the %s has one noise blanker, so 0 or 1, not %d: %w",
			r.model.Label, level, backend.ErrUnsupported)
	}
	return r.set(ctx, c, "noise blanker", r.frame(cmdFunc, subNB, byte(level)))
}

// SetNBLevel sets the blanker threshold, 0-100%.
func (r *Rig) SetNBLevel(ctx context.Context, c backend.Conn, pct float64) error {
	if !r.model.NBLevel {
		return fmt.Errorf("civ: the %s has no noise blanker level command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	return r.setLevel(ctx, c, "noise blanker level", subNBLevel, pct)
}

// SetNoiseReduction switches the reducer.
func (r *Rig) SetNoiseReduction(ctx context.Context, c backend.Conn, level int) error {
	if !r.model.NoiseReduction {
		return fmt.Errorf("civ: the %s has no noise reduction command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	if level < 0 || level > 1 {
		return fmt.Errorf("civ: the %s has one noise reducer, so 0 or 1, not %d: %w",
			r.model.Label, level, backend.ErrUnsupported)
	}
	return r.set(ctx, c, "noise reduction", r.frame(cmdFunc, subNR, byte(level)))
}

// SetNRLevel sets the reducer's strength, 0-100%.
func (r *Rig) SetNRLevel(ctx context.Context, c backend.Conn, pct float64) error {
	if !r.model.NRLevel {
		return fmt.Errorf("civ: the %s has no noise reduction level command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	return r.setLevel(ctx, c, "noise reduction level", subNRLevel, pct)
}

// SetNotch switches the manual notch.
func (r *Rig) SetNotch(ctx context.Context, c backend.Conn, on bool) error {
	if !r.model.Notch {
		return fmt.Errorf("civ: the %s has no manual notch command; on this radio "+
			"the manual notch is a front-panel control: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	return r.set(ctx, c, "manual notch", r.frame(cmdFunc, subNotch, onOff(on)))
}

// SetNotchFreq parks the manual notch, 0-100% of the radio's own range.
func (r *Rig) SetNotchFreq(ctx context.Context, c backend.Conn, pct float64) error {
	if !r.model.NotchFreq {
		return fmt.Errorf("civ: the %s has no notch position command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	return r.setLevel(ctx, c, "notch position", subNotchFreq, pct)
}

// SetNotchWidth chooses how wide the manual notch bites.
func (r *Rig) SetNotchWidth(ctx context.Context, c backend.Conn, w radio.NotchWidth) error {
	if !r.model.NotchWidth {
		return fmt.Errorf("civ: the %s has no notch width command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	code, ok := notchWidthByte[w]
	if !ok {
		return fmt.Errorf("civ: notch width %q, want one of %v: %w",
			w, r.model.notchWidths(), backend.ErrUnsupported)
	}
	return r.set(ctx, c, "notch width", r.frame(cmdFunc, subNotchWide, code))
}

// SetAutoNotch switches the automatic notch.
func (r *Rig) SetAutoNotch(ctx context.Context, c backend.Conn, on bool) error {
	if !r.model.AutoNotch {
		return fmt.Errorf("civ: the %s has no auto notch command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	return r.set(ctx, c, "auto notch", r.frame(cmdFunc, subAutoNotch, onOff(on)))
}

// noiseReads lists this model's noise and notch queries for the slow poll.
func (r *Rig) noiseReads() []request {
	var reqs []request
	if r.model.NoiseBlanker {
		reqs = append(reqs, request{KeyNB, r.frame(cmdFunc, subNB)})
	}
	if r.model.NBLevel {
		reqs = append(reqs, request{KeyNBLevel, r.frame(cmdLevel, subNBLevel)})
	}
	if r.model.NoiseReduction {
		reqs = append(reqs, request{KeyNR, r.frame(cmdFunc, subNR)})
	}
	if r.model.NRLevel {
		reqs = append(reqs, request{KeyNRLevel, r.frame(cmdLevel, subNRLevel)})
	}
	if r.model.Notch {
		reqs = append(reqs, request{KeyNotch, r.frame(cmdFunc, subNotch)})
	}
	if r.model.NotchFreq {
		reqs = append(reqs, request{KeyNotchFreq, r.frame(cmdLevel, subNotchFreq)})
	}
	if r.model.NotchWidth {
		reqs = append(reqs, request{KeyNotchWidth, r.frame(cmdFunc, subNotchWide)})
	}
	if r.model.AutoNotch {
		reqs = append(reqs, request{KeyAutoNotch, r.frame(cmdFunc, subAutoNotch)})
	}
	return reqs
}
