package civ

import (
	"context"
	"fmt"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Both VFOs of an IC-7610, transcribed from its CI-V Reference Guide (A7380-7EX-4,
// Sep 2025 revision).
//
// # Why these commands rather than the ones the rest of the backend uses
//
// Commands 03/05 and 04/06 read and write "the operating frequency" and "the
// operating mode" — whichever VFO the radio is on. There is no way to name the
// other one, so reaching it means selecting it first, which changes what the
// operator is using and races with the front panel.
//
// 25 and 26 name the VFO in the frame:
//
//	25 <band>                       read that VFO's frequency
//	25 <band> <5 bytes BCD>         set it
//	26 <band>                       read that VFO's mode, data mode and filter
//	26 <band> <mode> <data> <filter> set all three
//
// 26 is the better command even for the operating VFO, and the reason is one
// this backend has already paid for: mode, data mode and filter travel
// together in a single frame, so none of them can disturb the others. The
// single-VFO path needs 06 then 1A 06, and those two overwrite each other —
// see SetMode and SetFilterSlot.
//
// # The 29 prefix, and what it does not cover
//
// A third command addresses the inactive VFO for a *set* of other commands:
//
//	29 <band> <command…>
//
// "Regardless of active/inactive the Main or Sub band, you can directly specify
// the Main or Sub band, and send/read the supported command settings." The
// supported ones are marked in the reference's command table, and the two this
// backend wants are there: 15 02, the S-meter, and 1A 03, the filter width.
//
// Frequency and mode are NOT marked, which is the whole reason 25 and 26 exist
// as separate commands rather than as 29-prefixed 03 and 04.
//
// # Naming
//
// The reference says main band and sub band throughout. remoses calls them VFO
// A and VFO B, because that is the operator's model and the API's, and maps
// A to main (00) and B to sub (01).
const (
	// The IC-7610's mode command 26 carries its own data-mode encoding, which
	// is not the 1A 06 one: 00 is data off and 01-03 select DATA1, DATA2 and
	// DATA3. Those three differ only in which modulation input they use, which
	// is station wiring remoses does not model, so it writes DATA1 and reads
	// anything non-zero as "data mode on" — the same rule SetMode already
	// applies to 1A 06.
	bandDataOff = 0x00
	bandData1   = 0x01
)

// bandByte maps a VFO onto the selector 25 and 26 take.
//
// The byte is the same either way — 00 for the first slot, 01 for the second —
// but what the radio does with it is not, and Model.DualVFOBandSelector is what
// records which. On an IC-7610 those are the main and sub bands, two fixed
// receivers. On an IC-9700 they are the selected and unselected VFO of the main
// band. remoses calls both pairs A and B, and Caps.VFOAddressing tells a client
// whether those labels are stable or relative, rather than leaving it to guess.
//
// VFOCurrent is deliberately not accepted. Every caller of these commands is
// naming a VFO on purpose, and quietly resolving "current" to A would make a
// request about one VFO act on another.
func bandByte(vfo radio.VFO) (byte, error) {
	switch vfo {
	case radio.VFOA, radio.VFOMain:
		return bandMain, nil
	case radio.VFOB, radio.VFOSub:
		return bandSub, nil
	}
	return 0, fmt.Errorf("civ: %s is not one of this radio's two VFOs: %w", vfo, backend.ErrUnsupported)
}

// vfoForBand is bandByte's inverse, for decoding.
func vfoForBand(b byte) (radio.VFO, bool) {
	switch b {
	case bandMain:
		return radio.VFOA, true
	case bandSub:
		return radio.VFOB, true
	}
	return radio.VFOCurrent, false
}

// addressableVFOs is what a client may name in a request.
//
// VFOCurrent is on every radio, because commands 03 and 05 always act on
// whatever is selected. A and B appear only where 25 and 26 can name them; on a
// radio without those, offering A would mean acting on the operating VFO and
// calling it A, which is the honest-refusal line the yaesubin backend draws for
// the same reason.
func (r *Rig) addressableVFOs() []radio.VFO {
	if !r.model.DualVFO {
		return []radio.VFO{radio.VFOCurrent}
	}
	return []radio.VFO{radio.VFOCurrent, radio.VFOA, radio.VFOB}
}

// vfoAddressing tells a client whether State.VFOA and VFOB are stable labels or
// relative ones. See radio.Caps.VFOAddressing for what the two mean.
func (r *Rig) vfoAddressing() string {
	switch {
	case !r.model.DualVFO:
		return ""
	case r.model.DualVFOBandSelector:
		return "named"
	default:
		return "relative"
	}
}

// requireDualVFO reports why this radio cannot address a VFO by name, or nil.
func (r *Rig) requireDualVFO() error {
	if !r.model.DualVFO {
		return fmt.Errorf("civ: %s has no command to address one VFO by name "+
			"(25/26); remoses reads and sets the operating VFO only: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	return nil
}

// ReadVFOs refreshes both VFOs, split and dual watch.
//
// The second VFO's S-meter is read only while dual watch is on, and that is not
// an optimisation: with dual watch off the second receiver is not running, so a
// meter reading from it is a stale number that would look live in a client's
// bar. The same reasoning the yaesubin backend applies to a transmit meter in
// receive.
func (r *Rig) ReadVFOs(ctx context.Context, c backend.Conn) error {
	if err := r.requireDualVFO(); err != nil {
		return err
	}
	reqs := []request{
		{KeyVFOFreq, r.frame(cmdBandFreq, bandMain)},
		{KeyVFOFreq, r.frame(cmdBandFreq, bandSub)},
		{KeyVFOMode, r.frame(cmdBandMode, bandMain)},
		{KeyVFOMode, r.frame(cmdBandMode, bandSub)},
	}
	// The passband, per VFO, behind the 29 prefix — which only the IC-7610 has.
	// 1A 03 is one of the commands its table marks as supporting the prefix, so
	// each band's filter width can be read without selecting it, and the mode
	// needed to turn that index into hertz has just been read above for the
	// same band, which is why these come after.
	//
	// An IC-9700 has no 29 at all, so its per-VFO passband is simply not
	// readable. Publishing nothing beats selecting a band to find out.
	if r.model.FilterWidth && r.model.DualVFOBandSelector {
		reqs = append(reqs,
			request{KeyVFOWidth, r.frame(cmdBand, bandMain, cmdMisc, subFilterWidth)},
			request{KeyVFOWidth, r.frame(cmdBand, bandSub, cmdMisc, subFilterWidth)})
	}
	if r.model.Split {
		reqs = append(reqs, request{KeySplit, r.frame(cmdSplit)})
	}
	if r.model.DualWatch {
		reqs = append(reqs, request{KeyDualWatch, r.frame(cmdVFO, subDualWatch)})
	}
	if err := r.readAll(ctx, c, reqs...); err != nil {
		return err
	}

	if !r.dualWatch.Load() {
		return nil
	}
	// 15 02 is one of the commands the reference marks as taking the 29
	// prefix, so the sub receiver's meter can be read without selecting it.
	return r.readAll(ctx, c,
		request{KeySubSMeter, r.frame(cmdBand, bandSub, cmdMeter, subSMeter)})
}

// SetVFOFrequency writes command 25 for one named VFO.
func (r *Rig) SetVFOFrequency(ctx context.Context, c backend.Conn, vfo radio.VFO, hz uint64) error {
	if err := r.requireDualVFO(); err != nil {
		return err
	}
	band, err := bandByte(vfo)
	if err != nil {
		return err
	}
	// The same width rule as command 05: the field follows the target
	// frequency, because a radio expecting five bytes rejects six.
	wide := r.model.WideFrequency && hz >= wideThresholdHz
	f, err := encodeFrequency(hz, wide)
	if err != nil {
		return err
	}
	return r.set(ctx, c, "VFO frequency", r.frame(cmdBandFreq, append([]byte{band}, f...)...))
}

// SetVFOMode writes command 26: mode, data mode and filter for one named VFO,
// in a single frame.
//
// slot 0 means "keep the filter this VFO already has", which needs a read
// first, because 26 has no encoding for "leave it alone" — the reference is
// explicit that a frame omitting the data and filter bytes selects DATA OFF and
// the mode's default filter. Sending a short frame to preserve a filter would
// therefore change both.
func (r *Rig) SetVFOMode(ctx context.Context, c backend.Conn, vfo radio.VFO, m radio.Mode, dataMode bool, slot int) error {
	if err := r.requireDualVFO(); err != nil {
		return err
	}
	band, err := bandByte(vfo)
	if err != nil {
		return err
	}
	if !r.model.supportsMode(m) {
		return fmt.Errorf("civ: %s does not have mode %s: %w", r.model.Label, m, backend.ErrUnsupported)
	}
	mb, ok := r.model.modeByte(m)
	if !ok {
		return fmt.Errorf("civ: mode %s has no CI-V code on %s: %w", m, r.model.Label, backend.ErrUnsupported)
	}
	if dataMode && !supportsDataMode(m) {
		return fmt.Errorf("civ: data mode is not available in %s: %w", m, backend.ErrUnsupported)
	}

	if slot == 0 {
		u, err := r.read(ctx, c, KeyVFOMode, r.frame(cmdBandMode, band))
		if err != nil {
			return err
		}
		cur := u.Patch.VFOA
		if vfo == radio.VFOB {
			cur = u.Patch.VFOB
		}
		if cur == nil {
			return fmt.Errorf("civ: cannot set mode on VFO %s: the rig did not report its current filter", vfo)
		}
		slot = cur.FilterSlot
	}
	if slot < 1 || slot > r.model.FilterSlots {
		return fmt.Errorf("civ: filter slot %d out of range (1-%d): %w",
			slot, r.model.FilterSlots, backend.ErrUnsupported)
	}

	data := byte(bandDataOff)
	if dataMode {
		data = bandData1
	}
	return r.set(ctx, c, "VFO mode", r.frame(cmdBandMode, band, mb, data, byte(slot)))
}

// SetSplit writes command 0F, which decides whether transmit lands on the other
// VFO.
//
// This is the one control here that moves where RF comes out, so it is read
// back by the poll rather than assumed to have taken.
func (r *Rig) SetSplit(ctx context.Context, c backend.Conn, on bool) error {
	if !r.model.Split {
		return fmt.Errorf("civ: %s has no split command (0F): %w", r.model.Label, backend.ErrUnsupported)
	}
	v := byte(0x00)
	if on {
		v = 0x01
	}
	return r.set(ctx, c, "split", r.frame(cmdSplit, v))
}

// SetDualWatch writes 07 C1 or 07 C0.
//
// Note the asymmetry with 07 C2: the reference gives C0 and C1 as the set forms
// and C2 as the read, so this cannot simply write C2 with a parameter the way
// most of this protocol's settings work.
func (r *Rig) SetDualWatch(ctx context.Context, c backend.Conn, on bool) error {
	if !r.model.DualWatch {
		return fmt.Errorf("civ: %s has no dual watch (07 C0/C1): %w", r.model.Label, backend.ErrUnsupported)
	}
	sub := byte(subDualWatchOff)
	if on {
		sub = subDualWatchOn
	}
	return r.set(ctx, c, "dual watch", r.frame(cmdVFO, sub))
}
