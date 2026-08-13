package selftest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hessu/remoses/internal/cw"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig"
)

// sequence is the run, in the order it happens.
//
// Receive first and transmit last, so that a run which is interrupted has done
// the harmless part and not the other one. Within that, the order is roughly
// the order an operator would work the radio.
func (r *Runner) sequence(ctx context.Context) error {
	for _, group := range []func(context.Context) error{
		r.frequency,
		r.modes,
		r.filters,
		r.power,
		r.vfos,
		r.frontEnd,
		r.noise,
		r.antenna,
		r.breakIn,
		r.deniedControls,
		r.transmit,
		r.powerSwitch,
	} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := group(ctx); err != nil {
			return err
		}
	}
	return nil
}

// inMode runs a group of steps with the radio in a mode that actually offers
// the control, and puts the mode back afterwards.
//
// Several controls exist only in some modes, and testing them anywhere else
// produces a finding about the test rather than about the radio. A TS-590SG
// showed both halves of that: break-in is set with VX there, which addresses
// VOX unless the rig is in CW, and FW is not the filter-width command in SSB at
// all — the SSB passband is shaped with SH and SL. Run in the operator's USB,
// those are six refusals that say nothing; run in CW they are six real results.
//
// It reports the switch as its own step, so a reader can see why the radio was
// moved and can tell a control that is missing from one that was asked for in
// the wrong place.
func (r *Runner) inMode(ctx context.Context, group string, want radio.Mode, fn func()) {
	if !r.caps.SupportsMode(want) {
		// Nothing to switch to. Run where we are and let the results say so.
		fn()
		return
	}
	before := r.s.State().Mode
	if before == want {
		fn()
		return
	}
	no := false
	m := want
	r.do(group, "into-"+want.String(), fmt.Sprintf(`{"mode":%q}`, want),
		func() (Verdict, string, string, string, error) {
			st, err := r.patch(ctx, rig.PatchRequest{Mode: &m, DataMode: &no})
			if err != nil {
				return Info, want.String(), "refused",
					"could not switch modes, so the steps below ran in " + before.String(), err
			}
			return Pass, want.String(), st.Mode.String(),
				"this control is mode-dependent on some radios, so it is exercised here", nil
		})

	fn()

	back := before
	_, _ = r.patch(ctx, rig.PatchRequest{Mode: &back, DataMode: &r.initial.DataMode})
}

// setAndVerify is the shape of nearly every step: write a value, read the state
// back, and say whether the radio actually moved.
//
// The three outcomes are the whole point. A read-back that matches is a pass. A
// read-back that did not move *at all*, when it was asked to move, is the
// failure this project has met more often than any other: a value written and
// never read, reporting a stale figure for ever, with nothing above the backend
// able to tell. Anything else — a radio that clamped, rounded, or refused for a
// reason of its own — is recorded with both numbers and left for a reader,
// because a run that cries wolf gets ignored, and this one only gets run once.
func (r *Runner) setAndVerify(ctx context.Context, group, name, request string, req rig.PatchRequest,
	before string, read func(radio.State) string, want string,
) {
	r.do(group, name, request, func() (Verdict, string, string, string, error) {
		st, err := r.patch(ctx, req)
		if err != nil {
			if errors.Is(err, rig.ErrUnsupported) {
				return Info, want, "refused",
					"the capabilities advertise this control and the request was refused — " +
						"either a restriction the radio enforces in this mode, or support that does not work", err
			}
			return Info, want, "error", "", err
		}
		got := read(st)
		switch {
		case got == want:
			return Pass, want, got, "", nil
		case got == before && before != want:
			return Fail, want, got,
				"the radio accepted the command and the reading did not move: " +
					"a value written but never read back reports a stale figure for ever", nil
		default:
			return Info, want, got,
				"the radio landed somewhere else, which is often a clamp or a step it rounded to", nil
		}
	})
}

// --- receive ---------------------------------------------------------------

// frequency works around wherever the operator left the radio, because that is
// the one frequency known to be legal for this station: in band, on an antenna,
// and not somebody else's. Offsets are small for the same reason.
func (r *Runner) frequency(ctx context.Context) error {
	base := r.initial.Frequency
	if base == 0 {
		r.skip("frequency", "all", "the radio reported no frequency to work from")
		return nil
	}
	for _, off := range []int64{1000, -1000, 10000} {
		hz := uint64(int64(base) + off)
		r.setAndVerify(ctx, "frequency", fmt.Sprintf("offset%+d", off),
			fmt.Sprintf(`{"frequency":%d}`, hz),
			rig.PatchRequest{Frequency: &hz},
			quoteHz(r.s.State().Frequency),
			func(s radio.State) string { return quoteHz(s.Frequency) },
			quoteHz(hz))
	}

	// A frequency finer than any radio here tunes. What comes back says what
	// the step is, and whether the backend rounds or the radio does.
	odd := base + 3
	r.do("frequency", "sub-step-rounding", fmt.Sprintf(`{"frequency":%d}`, odd),
		func() (Verdict, string, string, string, error) {
			st, err := r.patch(ctx, rig.PatchRequest{Frequency: &odd})
			if err != nil {
				return Info, "a rounded frequency", "refused", "", err
			}
			return Info, quoteHz(odd), quoteHz(st.Frequency),
				fmt.Sprintf("landed %d Hz from the request, which is this radio's tuning step or the backend's rounding",
					int64(st.Frequency)-int64(odd)), nil
		})

	// Out of any radio's range. A refusal is the right answer; acceptance means
	// something is not checking.
	r.do("frequency", "out-of-range", `{"frequency":1}`, func() (Verdict, string, string, string, error) {
		one := uint64(1)
		st, err := r.patch(ctx, rig.PatchRequest{Frequency: &one})
		switch {
		case err == nil:
			return Fail, "a refusal", quoteHz(st.Frequency),
				"1 Hz was accepted; no radio here tunes it, so nothing checked the range", nil
		// A refusal by the radio itself counts. remoses declining a request it
		// knows is impossible and the rig declining one it will not tune are
		// the same correct answer, and scoring the second as a finding puts a
		// line in the report that a reader has to rule out by hand — which a
		// TS-590SG report duly did, three times over.
		case errors.Is(err, rig.ErrUnsupported), errors.Is(err, rig.ErrOutOfBand),
			errors.Is(err, rig.ErrNAK):
			return Refused, "a refusal", "refused", "", nil
		default:
			return Info, "a refusal", "refused", "", err
		}
	})

	back := base
	_, _ = r.patch(ctx, rig.PatchRequest{Frequency: &back})
	return nil
}

// modes walks everything Caps advertises, then asks for one it does not.
func (r *Runner) modes(ctx context.Context) error {
	if len(r.caps.Modes) == 0 {
		r.skip("mode", "all", "the radio advertises no modes")
		return nil
	}
	for _, m := range r.caps.Modes {
		mode := m
		no := false
		// .String(), not string(): radio.Mode is a uint8, so a plain conversion
		// yields the rune with that code point. It was one, and every mode step
		// in every report so far is named with a control character because of
		// it — the verdicts were right, since both sides of the comparison were
		// equally wrong, and the file was unreadable, which for a file whose
		// entire job is to be read by somebody else is the same as broken.
		r.setAndVerify(ctx, "mode", m.String(), fmt.Sprintf(`{"mode":%q}`, m),
			rig.PatchRequest{Mode: &mode, DataMode: &no},
			r.s.State().Mode.String(),
			func(s radio.State) string { return s.Mode.String() },
			m.String())
	}

	// Back to the mode the radio was found in, before anything else runs.
	//
	// Several controls exist only in some modes, and the sweep above ends on
	// whichever mode happens to be last in Caps — FSK-R on a Kenwood. A field
	// report from a TS-590SG spent its whole second half in FSK-R because of
	// that, so break-in was refused three times (it is a CW-only command there)
	// and the automatic notch once (SSB and AM only), producing four findings
	// about the test rather than about the radio. The operator's own mode is
	// the least surprising place to do the rest of the work.
	restoreMode := r.initial.Mode
	restoreData := r.initial.DataMode
	if restoreMode != radio.ModeUnknown {
		r.setAndVerify(ctx, "mode", "back-to-as-found",
			fmt.Sprintf(`{"mode":%q,"data_mode":%v}`, restoreMode, restoreData),
			rig.PatchRequest{Mode: &restoreMode, DataMode: &restoreData},
			r.s.State().Mode.String(),
			func(s radio.State) string { return s.Mode.String() },
			restoreMode.String())
	}

	// Data mode, which no capability reports: the only way to find out is to
	// ask. A refusal is a perfectly good answer and is recorded as one.
	cur := r.s.State().Mode
	yes := true
	r.do("mode", "data-mode-on", fmt.Sprintf(`{"mode":%q,"data_mode":true}`, cur),
		func() (Verdict, string, string, string, error) {
			st, err := r.patch(ctx, rig.PatchRequest{Mode: &cur, DataMode: &yes})
			if err != nil {
				return Refused, "data mode on", "refused",
					"this radio has no data-mode spelling of that mode, which is common", err
			}
			if st.DataMode {
				return Pass, "data mode on", "on", "", nil
			}
			return Fail, "data mode on", "off",
				"the request was accepted and the flag did not move", nil
		})
	// And back to the operator's own data-mode flag rather than to false: the
	// radio in that field report was found in USB-DATA on 10.136, which is
	// somebody sitting on FT8, and leaving it in plain USB for the rest of the
	// run is not the same radio.
	_, _ = r.patch(ctx, rig.PatchRequest{Mode: &cur, DataMode: &restoreData})

	// A mode the radio does not have. remoses must refuse it rather than send
	// a code from another radio's table.
	for _, m := range []radio.Mode{radio.ModeDV, radio.ModeWFM, radio.ModeATV} {
		if r.caps.SupportsMode(m) {
			continue
		}
		r.expectRefused(ctx, "mode", "unsupported-"+m.String(), fmt.Sprintf(`{"mode":%q}`, m),
			rig.PatchRequest{Mode: &m})
		break
	}
	return nil
}

func (r *Runner) filters(ctx context.Context) error {
	if r.caps.FilterWidth {
		// In CW, where a filter width is both most meaningful and most widely
		// implemented. A TS-590 has no FW command in SSB at all.
		r.inMode(ctx, "filter", radio.ModeCW, func() {
			before := r.s.State().PassbandHz
			for _, hz := range candidateWidths(before) {
				w := hz
				r.setAndVerify(ctx, "filter", fmt.Sprintf("width-%d", w),
					fmt.Sprintf(`{"passband_hz":%d}`, w),
					rig.PatchRequest{FilterWidthHz: &w},
					fmt.Sprintf("%d", r.s.State().PassbandHz),
					func(s radio.State) string { return fmt.Sprintf("%d", s.PassbandHz) },
					fmt.Sprintf("%d", w))
			}
		})
	} else {
		r.skip("filter", "width", "no filter-width command")
	}

	if r.caps.FilterSlots > 0 {
		for i := 1; i <= r.caps.FilterSlots; i++ {
			slot := i
			r.setAndVerify(ctx, "filter", fmt.Sprintf("slot-%d", slot),
				fmt.Sprintf(`{"filter_slot":%d}`, slot),
				rig.PatchRequest{FilterSlot: &slot},
				fmt.Sprintf("%d", r.s.State().FilterSlot),
				func(s radio.State) string { return fmt.Sprintf("%d", s.FilterSlot) },
				fmt.Sprintf("%d", slot))
		}
		bad := r.caps.FilterSlots + 1
		r.expectRefused(ctx, "filter", "slot-out-of-range", fmt.Sprintf(`{"filter_slot":%d}`, bad),
			rig.PatchRequest{FilterSlot: &bad})
	} else {
		r.skip("filter", "slots", "no filter-slot command")
	}
	return nil
}

// candidateWidths picks two widths that differ from the current one, so that a
// radio which ignores the command cannot pass by accident.
func candidateWidths(current int) []int {
	all := []int{500, 1200, 2400, 3000}
	var out []int
	for _, w := range all {
		if w != current && len(out) < 2 {
			out = append(out, w)
		}
	}
	return out
}

func (r *Runner) power(ctx context.Context) error {
	if !r.caps.PowerControl {
		r.skip("power", "all", "no transmit-power command")
		return nil
	}
	// Low values only. Nothing here transmits, but a run that left somebody's
	// radio wound up to full would be a trap for whatever they did next — and
	// the restore at the end is not a promise this can keep if it crashes.
	for _, pct := range []float64{10, 20} {
		v := pct
		r.setAndVerify(ctx, "power", fmt.Sprintf("pct-%.0f", v),
			fmt.Sprintf(`{"power_pct":%.0f}`, v),
			rig.PatchRequest{Power: &radio.PowerSet{Pct: &v}},
			fmt.Sprintf("%.0f", r.s.State().Power.Pct),
			func(s radio.State) string { return fmt.Sprintf("%.0f", s.Power.Pct) },
			fmt.Sprintf("%.0f", v))
	}
	if !r.caps.PowerWattAccurate {
		w := 10.0
		r.expectRefused(ctx, "power", "watts-on-a-relative-scale", `{"power_w":10}`,
			rig.PatchRequest{Power: &radio.PowerSet{Watts: &w}})
	}
	return nil
}

func (r *Runner) vfos(ctx context.Context) error {
	named := 0
	for _, v := range r.caps.VFOs {
		if v != radio.VFOCurrent {
			named++
		}
	}
	if named == 0 {
		r.skip("vfo", "addressing", "this radio addresses only the VFO it is on")
	} else {
		// Park a frequency on B and prove A did not move with it. The whole
		// value of addressed VFOs is that one can be set without disturbing the
		// other, and a backend that resolved both to "the current VFO" would
		// look correct until somebody checked this.
		base := r.initial.Frequency
		other := base + 2000
		r.do("vfo", "set-b-without-moving-a", fmt.Sprintf(`{"vfo":"B","frequency":%d}`, other),
			func() (Verdict, string, string, string, error) {
				beforeA := r.s.State().VFOA.Frequency
				st, err := r.patch(ctx, rig.PatchRequest{VFO: radio.VFOB, Frequency: &other})
				if err != nil {
					return Info, "B moved, A did not", "refused", "", err
				}
				switch {
				case st.VFOB.Frequency != other:
					return Info, quoteHz(other), quoteHz(st.VFOB.Frequency),
						"VFO B did not land on the requested frequency", nil
				case beforeA != 0 && st.VFOA.Frequency != beforeA:
					return Fail, "A unchanged at " + quoteHz(beforeA), quoteHz(st.VFOA.Frequency),
						"setting VFO B moved VFO A: the two are not being addressed separately", nil
				default:
					return Pass, "B moved, A did not", "B moved, A did not", "", nil
				}
			})
	}

	r.togglePair(ctx, "vfo", "split", r.caps.Split, func(on bool) rig.PatchRequest {
		return rig.PatchRequest{Split: &on}
	}, func(s radio.State) bool { return s.Split })

	r.togglePair(ctx, "vfo", "dual-watch", r.caps.DualWatch, func(on bool) rig.PatchRequest {
		return rig.PatchRequest{DualWatch: &on}
	}, func(s radio.State) bool { return s.DualWatch })

	if r.caps.BandExchange {
		// Twice, which both tests it and undoes it.
		for i := 1; i <= 2; i++ {
			yes := true
			n := fmt.Sprintf("exchange-%d-of-2", i)
			r.do("vfo", n, `{"exchange_bands":true}`, func() (Verdict, string, string, string, error) {
				before := r.s.State().Frequency
				st, err := r.patch(ctx, rig.PatchRequest{ExchangeBands: &yes})
				if err != nil {
					return Info, "the other receiver", "refused", "", err
				}
				if st.Frequency == before {
					return Info, "a different band", quoteHz(st.Frequency),
						"the operating frequency did not change; the two receivers may have been on the same band", nil
				}
				return Pass, "a different band", quoteHz(st.Frequency), "", nil
			})
		}
	} else {
		r.skip("vfo", "band-exchange", "no band-exchange command")
	}

	// Leaving memory mode is safe on a radio already on a VFO, and it is the
	// one part of memory mode remoses implements.
	yes := true
	r.do("vfo", "leave-memory-mode", `{"vfo_mode":true}`, func() (Verdict, string, string, string, error) {
		_, err := r.patch(ctx, rig.PatchRequest{VFOMode: &yes})
		if err != nil {
			if errors.Is(err, rig.ErrUnsupported) {
				return Skipped, "", "", "no command to leave memory mode", nil
			}
			return Info, "", "", "", err
		}
		return Pass, "on a VFO", "on a VFO", "", nil
	})
	return nil
}

// togglePair exercises an on/off control both ways and puts it back.
func (r *Runner) togglePair(ctx context.Context, group, name string, has bool,
	req func(bool) rig.PatchRequest, read func(radio.State) bool,
) {
	if !has {
		r.skip(group, name, "not advertised")
		return
	}
	was := read(r.s.State())
	for _, on := range []bool{!was, was} {
		v := on
		r.setAndVerify(ctx, group, fmt.Sprintf("%s-%v", name, v),
			fmt.Sprintf(`{%q:%v}`, strings.ReplaceAll(name, "-", "_"), v),
			req(v),
			fmt.Sprintf("%v", read(r.s.State())),
			func(s radio.State) string { return fmt.Sprintf("%v", read(s)) },
			fmt.Sprintf("%v", v))
	}
}

func (r *Runner) frontEnd(ctx context.Context) error {
	if r.caps.PreampLevels > 0 {
		for i := 0; i <= r.caps.PreampLevels; i++ {
			v := i
			r.setAndVerify(ctx, "front-end", fmt.Sprintf("preamp-%d", v),
				fmt.Sprintf(`{"preamp":%d}`, v), rig.PatchRequest{Preamp: &v},
				intOr(r.s.State().Preamp), func(s radio.State) string { return intOr(s.Preamp) },
				fmt.Sprintf("%d", v))
		}
		bad := r.caps.PreampLevels + 1
		r.expectRefused(ctx, "front-end", "preamp-out-of-range", fmt.Sprintf(`{"preamp":%d}`, bad),
			rig.PatchRequest{Preamp: &bad})
	} else {
		r.skip("front-end", "preamp", "no preamplifier command")
	}

	if r.caps.AttenuatorControl() {
		for _, db := range append([]int{0}, r.caps.AttenuatorDB...) {
			v := db
			r.setAndVerify(ctx, "front-end", fmt.Sprintf("attenuator-%ddB", v),
				fmt.Sprintf(`{"attenuator_db":%d}`, v), rig.PatchRequest{AttenuatorDB: &v},
				intOr(r.s.State().AttenuatorDB), func(s radio.State) string { return intOr(s.AttenuatorDB) },
				fmt.Sprintf("%d", v))
		}
	} else {
		r.skip("front-end", "attenuator", "no attenuator command")
	}

	if r.caps.RFGainControl {
		for _, pct := range []float64{60, 100} {
			v := pct
			r.setAndVerify(ctx, "front-end", fmt.Sprintf("rf-gain-%.0f", v),
				fmt.Sprintf(`{"rf_gain":%.0f}`, v), rig.PatchRequest{RFGain: &v},
				floatOr(r.s.State().RFGain), func(s radio.State) string { return floatOr(s.RFGain) },
				fmt.Sprintf("%.0f", v))
		}
	} else {
		r.skip("front-end", "rf-gain", "no RF gain command")
	}

	if r.caps.AGCControl() {
		for _, g := range r.caps.AGCSettings {
			v := g
			r.setAndVerify(ctx, "front-end", "agc-"+string(v),
				fmt.Sprintf(`{"agc":%q}`, v), rig.PatchRequest{AGC: &v},
				string(r.s.State().AGC), func(s radio.State) string { return string(s.AGC) },
				string(v))
		}
	} else {
		r.skip("front-end", "agc", "no AGC command")
	}

	r.togglePair(ctx, "front-end", "ip_plus", r.caps.IPPlusControl, func(on bool) rig.PatchRequest {
		return rig.PatchRequest{IPPlus: &on}
	}, func(s radio.State) bool { return s.IPPlus != nil && *s.IPPlus })

	r.togglePair(ctx, "front-end", "digi_sel", r.caps.DigiSelControl, func(on bool) rig.PatchRequest {
		return rig.PatchRequest{DigiSel: &on}
	}, func(s radio.State) bool { return s.DigiSel != nil && *s.DigiSel })
	return nil
}

func (r *Runner) noise(ctx context.Context) error {
	if r.caps.NoiseBlankerLevels > 0 {
		for i := 0; i <= r.caps.NoiseBlankerLevels; i++ {
			v := i
			r.setAndVerify(ctx, "noise", fmt.Sprintf("blanker-%d", v),
				fmt.Sprintf(`{"noise_blanker":%d}`, v), rig.PatchRequest{NoiseBlanker: &v},
				intOr(r.s.State().NoiseBlanker), func(s radio.State) string { return intOr(s.NoiseBlanker) },
				fmt.Sprintf("%d", v))
		}
	} else {
		r.skip("noise", "blanker", "no noise blanker command")
	}
	if r.caps.NoiseReductionLevels > 0 {
		for i := 0; i <= r.caps.NoiseReductionLevels; i++ {
			v := i
			r.setAndVerify(ctx, "noise", fmt.Sprintf("reduction-%d", v),
				fmt.Sprintf(`{"noise_reduction":%d}`, v), rig.PatchRequest{NoiseReduction: &v},
				intOr(r.s.State().NoiseReduction), func(s radio.State) string { return intOr(s.NoiseReduction) },
				fmt.Sprintf("%d", v))
		}
	} else {
		r.skip("noise", "reduction", "no noise reduction command")
	}

	r.togglePair(ctx, "noise", "notch", r.caps.NotchControl, func(on bool) rig.PatchRequest {
		return rig.PatchRequest{Notch: &on}
	}, func(s radio.State) bool { return s.Notch != nil && *s.Notch })

	r.togglePair(ctx, "noise", "auto_notch", r.caps.AutoNotchControl, func(on bool) rig.PatchRequest {
		return rig.PatchRequest{AutoNotch: &on}
	}, func(s radio.State) bool { return s.AutoNotch != nil && *s.AutoNotch })

	// Both notches at once, where the radio says they cannot both run. The
	// radios enforce this without saying so, and a request that applied one and
	// let the other vanish would look like a success.
	if r.caps.NotchExclusive {
		yes := true
		r.expectRefused(ctx, "noise", "both-notches", `{"notch":true,"auto_notch":true}`,
			rig.PatchRequest{Notch: &yes, AutoNotch: &yes})
	}
	return nil
}

func (r *Runner) antenna(ctx context.Context) error {
	if r.caps.Antennas <= 0 {
		r.skip("antenna", "selector", "no antenna selector")
		return nil
	}
	for i := 1; i <= r.caps.Antennas; i++ {
		v := i
		r.setAndVerify(ctx, "antenna", fmt.Sprintf("select-%d", v),
			fmt.Sprintf(`{"antenna":%d}`, v), rig.PatchRequest{Antenna: &v},
			intOr(r.s.State().Antenna), func(s radio.State) string { return intOr(s.Antenna) },
			fmt.Sprintf("%d", v))
	}
	return nil
}

func (r *Runner) breakIn(ctx context.Context) error {
	if !r.caps.BreakInControl {
		r.skip("break-in", "all", "no break-in command")
		return nil
	}
	// In CW, always. Break-in is a CW control by definition, and on a TS-590 it
	// is not merely meaningless elsewhere — the command that carries it, VX,
	// addresses VOX in every other mode, which is why remoses refuses to touch
	// it outside CW rather than switching somebody's voice VOX on behind them.
	r.inMode(ctx, "break-in", radio.ModeCW, func() {
		for _, v := range []radio.BreakIn{radio.BreakInOff, radio.BreakInSemi, radio.BreakInFull} {
			b := v
			r.setAndVerify(ctx, "break-in", string(b), fmt.Sprintf(`{"break_in":%q}`, b),
				rig.PatchRequest{BreakIn: &b},
				string(r.s.State().BreakIn), func(s radio.State) string { return string(s.BreakIn) },
				string(b))
		}
	})
	return nil
}

// deniedControls asks for everything the radio says it has not got.
//
// This is the half that catches a command going somewhere it should not. Every
// one of these has a real precedent: a tuner command that is PTT on one Icom, a
// VFO applied to the wrong receiver, a power request on a radio with no power
// command at all.
func (r *Runner) deniedControls(ctx context.Context) error {
	on := true
	tunerOn := radio.TunerOn
	one := 1
	if !r.caps.TunerControl {
		r.expectRefused(ctx, "denied", "tuner", `{"tuner":"on"}`, rig.PatchRequest{Tuner: &tunerOn})
	}
	// The two probes below key a transmitter if they are *not* refused, and a
	// capability that is wrong is precisely what this run exists to find — so
	// they are gated on the operator having authorised transmitting. Proving a
	// refusal is not worth putting a carrier on the air nobody agreed to.
	if !r.caps.TunerTune {
		if r.opts.TXFrequency == 0 {
			r.skip("denied", "tuner_tune", "would transmit if the capability is wrong; needs -tx-freq")
		} else {
			r.expectRefused(ctx, "denied", "tuner_tune", `{"tuner_tune":true}`, rig.PatchRequest{TunerTune: &on})
		}
	}
	if !r.caps.Split {
		r.expectRefused(ctx, "denied", "split", `{"split":true}`, rig.PatchRequest{Split: &on})
	}
	if !r.caps.DualWatch {
		r.expectRefused(ctx, "denied", "dual_watch", `{"dual_watch":true}`, rig.PatchRequest{DualWatch: &on})
	}
	if !r.caps.SupportsVFO(radio.VFOB) {
		r.expectRefused(ctx, "denied", "vfo-b", `{"vfo":"B"}`, rig.PatchRequest{VFO: radio.VFOB})
	}
	if !r.caps.SupportsVFO(radio.VFOSub) {
		r.expectRefused(ctx, "denied", "vfo-sub", `{"vfo":"sub"}`, rig.PatchRequest{VFO: radio.VFOSub})
	}
	if r.caps.PreampLevels == 0 {
		r.expectRefused(ctx, "denied", "preamp", `{"preamp":1}`, rig.PatchRequest{Preamp: &one})
	}
	if !r.caps.BreakInControl {
		semi := radio.BreakInSemi
		r.expectRefused(ctx, "denied", "break_in", `{"break_in":"semi"}`, rig.PatchRequest{BreakIn: &semi})
	}
	if !r.caps.BandExchange {
		r.expectRefused(ctx, "denied", "exchange_bands", `{"exchange_bands":true}`,
			rig.PatchRequest{ExchangeBands: &on})
	}
	if !r.caps.PTTControl {
		if r.opts.TXFrequency == 0 {
			r.skip("denied", "ptt", "would transmit if the capability is wrong; needs -tx-freq")
		} else {
			r.expectRefused(ctx, "denied", "ptt", `{"ptt":true}`, rig.PatchRequest{PTT: &on})
		}
	}
	if !r.caps.PowerControl {
		pct := 10.0
		r.expectRefused(ctx, "denied", "power", `{"power_pct":10}`,
			rig.PatchRequest{Power: &radio.PowerSet{Pct: &pct}})
	}
	if !r.caps.FilterWidth {
		hz := 500
		r.expectRefused(ctx, "denied", "passband", `{"passband_hz":500}`,
			rig.PatchRequest{FilterWidthHz: &hz})
	}
	return nil
}

// --- transmit --------------------------------------------------------------

// transmit keys the radio, and only where the operator named a frequency.
func (r *Runner) transmit(ctx context.Context) error {
	if r.opts.TXFrequency == 0 {
		r.skip("transmit", "all", "no -tx-freq given, so this run never keys the radio")
		return nil
	}
	if !r.caps.PTTControl {
		r.skip("transmit", "all", "this radio has no transmitter command")
		return nil
	}

	hz := r.opts.TXFrequency
	pct := float64(r.opts.TXPowerPct)
	mode := radio.ModeCW
	if !r.caps.SupportsMode(mode) {
		mode = r.caps.Modes[0]
	}
	no := false
	setup := rig.PatchRequest{Frequency: &hz, Mode: &mode, DataMode: &no}
	if r.caps.PowerControl {
		setup.Power = &radio.PowerSet{Pct: &pct}
	}
	r.do("transmit", "setup", describePatch(setup), func() (Verdict, string, string, string, error) {
		st, err := r.patch(ctx, setup)
		if err != nil {
			return Fail, "ready to transmit", "refused",
				"the transmit frequency could not be set, so nothing below ran", err
		}
		return Pass, quoteHz(hz), quoteHz(st.Frequency), "", nil
	})
	if r.s.State().Frequency != hz {
		r.skip("transmit", "keyed", "the radio is not on the transmit frequency")
		return nil
	}

	// A brief carrier, and the meters while it is up. On a radio whose CW is
	// generated by its own keyer this radiates nothing — the transmitter is
	// keyed and the key is not — which is worth knowing and is why the reading
	// is recorded rather than judged.
	on, off := true, false
	r.do("transmit", "ptt-keyed", `{"ptt":true}`, func() (Verdict, string, string, string, error) {
		st, err := r.patch(ctx, rig.PatchRequest{PTT: &on})
		if err != nil {
			return Fail, "keyed", "refused", "the radio advertises PTT and refused it", err
		}
		if !st.PTT {
			return Fail, "keyed", "not keyed",
				"PTT was accepted and the radio reports itself receiving", nil
		}
		return Pass, "keyed", "keyed", "", nil
	})

	r.do("transmit", "meters-while-keyed", "", func() (Verdict, string, string, string, error) {
		var seen []string
		for i := 0; i < 4; i++ {
			time.Sleep(400 * time.Millisecond)
			st := r.s.State()
			seen = append(seen, describeMeters(st))
			if !st.PTT {
				break
			}
		}
		st := r.s.State()
		switch {
		case r.caps.PowerMeter && st.PowerMeter == nil:
			return Info, "a forward power reading", "absent",
				"the radio advertises a power meter and published none while keyed", nil
		default:
			return Info, "readings while keyed", strings.Join(seen, " | "),
				"sampled four times; a zero here is normal in CW with no key input", nil
		}
	})

	r.do("transmit", "ptt-released", `{"ptt":false}`, func() (Verdict, string, string, string, error) {
		st, err := r.patch(ctx, rig.PatchRequest{PTT: &off})
		if err != nil {
			return Fail, "receiving", "error", "the radio would not unkey", err
		}
		if st.PTT {
			return Fail, "receiving", "still keyed", "the radio is still transmitting", nil
		}
		return Pass, "receiving", "receiving", "", nil
	})

	r.do("transmit", "meters-absent-in-receive", "", func() (Verdict, string, string, string, error) {
		st := r.s.State()
		if st.PowerMeter != nil || st.SWR != nil || st.ALC != nil {
			return Fail, "no transmit meters", describeMeters(st),
				"transmit meters are still published in receive: a bar drawn at zero cannot be told " +
					"from a real reading into a dead load", nil
		}
		return Pass, "no transmit meters", "absent", "", nil
	})

	r.cwTest(ctx)
	r.tunerTest(ctx)
	return nil
}

func (r *Runner) cwTest(ctx context.Context) {
	if r.caps.CWMethod == radio.CWNone {
		r.skip("cw", "send", "this radio has no CW method configured or available")
		return
	}
	sender := r.s.CW()
	if sender == nil {
		r.skip("cw", "send", "no CW sender is attached; cw.enabled may be false in the configuration")
		return
	}
	r.do("cw", "send", fmt.Sprintf("POST /cw %q", r.opts.CWText), func() (Verdict, string, string, string, error) {
		n, err := sender.Enqueue(r.opts.CWText, cw.Append)
		if err != nil {
			return Fail, "queued", "refused", "the CW text was refused", err
		}
		deadline := time.Now().Add(60 * time.Second)
		keyed := false
		for time.Now().Before(deadline) {
			st := r.s.State()
			if st.PTT {
				keyed = true
			}
			if !sender.Status().Busy {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !keyed {
			return Info, "the radio keyed", "never saw PTT true",
				"the message drained without the poll ever catching the transmitter up — " +
					"which is what a message accepted and never transmitted looks like, " +
					"and is also what a fast poll can simply miss. The operator's ears settle it", nil
		}
		return Info, fmt.Sprintf("%d characters keyed", n), "sent, and the radio keyed",
			"whether it was audible is the one thing only the operator can report", nil
	})
	sender.Abort()
}

func (r *Runner) tunerTest(ctx context.Context) {
	if !r.caps.TunerTune {
		r.skip("tuner", "cycle", "no antenna tuner cycle command")
		return
	}
	on := true
	r.do("tuner", "cycle", `{"tuner_tune":true}`, func() (Verdict, string, string, string, error) {
		if _, err := r.patch(ctx, rig.PatchRequest{TunerTune: &on}); err != nil {
			return Info, "a tuning cycle", "refused", "", err
		}
		deadline := time.Now().Add(20 * time.Second)
		sawTuning := false
		for time.Now().Before(deadline) {
			t := r.s.State().Tuner
			if t == radio.TunerTuning {
				sawTuning = true
			} else if sawTuning {
				return Info, "a completed cycle", string(t),
					"the tuner state after a cycle is the only report of whether it found a match: " +
						"on means it did, off means it did not", nil
			}
			time.Sleep(250 * time.Millisecond)
		}
		return Info, "a completed cycle", string(r.s.State().Tuner),
			"the cycle did not visibly finish within 20 s", nil
	})
}

func (r *Runner) powerSwitch(ctx context.Context) error {
	if !r.opts.PowerSwitch {
		r.skip("power-switch", "off-and-on", "not authorised; pass -test-power-switch to include it")
		return nil
	}
	if !r.caps.PowerSwitch {
		r.skip("power-switch", "off-and-on", "no power-switch command")
		return nil
	}
	r.do("power-switch", "off", `{"power_switch":"off"}`, func() (Verdict, string, string, string, error) {
		if _, err := r.s.PowerOff(ctx, false); err != nil {
			return Fail, "standby", "refused", "the radio advertises a power switch and refused it", err
		}
		for i := 0; i < 40; i++ {
			time.Sleep(250 * time.Millisecond)
			if r.s.State().Standby {
				return Pass, "standby", "standby", "", nil
			}
		}
		return Info, "standby", "not detected",
			"the radio went off but remoses never saw standby; some radios go silent instead of refusing", nil
	})
	r.do("power-switch", "on", `{"power_switch":"on"}`, func() (Verdict, string, string, string, error) {
		if _, err := r.s.PowerOn(ctx); err != nil {
			return Fail, "awake", "refused", "the wake command was refused", err
		}
		if err := WaitConnected(ctx, r.s, 30*time.Second); err != nil {
			return Fail, "awake", "still asleep",
				"THE RADIO DID NOT WAKE. Somebody may have to switch it on by hand — " +
					"whether a wake works at all is a wiring question, not a command one", err
		}
		return Pass, "awake", "awake", "", nil
	})
	return nil
}

// --- rendering -------------------------------------------------------------

func intOr(p *int) string {
	if p == nil {
		return "absent"
	}
	return fmt.Sprintf("%d", *p)
}

func floatOr(p *float64) string {
	if p == nil {
		return "absent"
	}
	return fmt.Sprintf("%.0f", *p)
}

func describeMeters(s radio.State) string {
	parts := []string{}
	if m := s.PowerMeter; m != nil {
		parts = append(parts, fmt.Sprintf("pwr=%d/%d", m.Raw, m.Scale))
	}
	if m := s.SWR; m != nil {
		parts = append(parts, fmt.Sprintf("swr=%d/%d", m.Raw, m.Scale))
	}
	if r := s.SWRRatio; r != nil {
		parts = append(parts, fmt.Sprintf("swr_ratio=%.2f", *r))
	}
	if m := s.ALC; m != nil {
		parts = append(parts, fmt.Sprintf("alc=%d/%d", m.Raw, m.Scale))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func describeState(s radio.State) string {
	return fmt.Sprintf("%d Hz %s data=%v passband=%d slot=%d power=%.0f%%",
		s.Frequency, s.Mode, s.DataMode, s.PassbandHz, s.FilterSlot, s.Power.Pct)
}

func describePatch(p rig.PatchRequest) string {
	parts := []string{}
	if p.Frequency != nil {
		parts = append(parts, fmt.Sprintf(`"frequency":%d`, *p.Frequency))
	}
	if p.Mode != nil {
		parts = append(parts, fmt.Sprintf(`"mode":%q`, *p.Mode))
	}
	if p.DataMode != nil {
		parts = append(parts, fmt.Sprintf(`"data_mode":%v`, *p.DataMode))
	}
	if p.FilterWidthHz != nil {
		parts = append(parts, fmt.Sprintf(`"passband_hz":%d`, *p.FilterWidthHz))
	}
	if p.FilterSlot != nil {
		parts = append(parts, fmt.Sprintf(`"filter_slot":%d`, *p.FilterSlot))
	}
	if p.Power != nil && p.Power.Pct != nil {
		parts = append(parts, fmt.Sprintf(`"power_pct":%.0f`, *p.Power.Pct))
	}
	if p.BreakIn != nil {
		parts = append(parts, fmt.Sprintf(`"break_in":%q`, *p.BreakIn))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
