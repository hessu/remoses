package kenwood

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The noise processing, the notch filters and the antenna selector.
//
// Three things here are unlike the other two families.
//
// THE BLANKER AND THE REDUCER ARE SETS, NOT LADDERS. NB takes 0, NB1, NB2 — and
// 3, both at once — and NR takes 0, NR1, NR2. NR1 and NR2 are different
// algorithms rather than two strengths of one: NR1's level is an effective
// level 01-10, NR2's is a following speed the reference gives as 00 (2 ms) to
// 09 (20 ms). So the level's SCALE depends on which reducer is running, which
// is why the last reading is kept.
//
// THE LEVELS ARE REFUSED WHILE THEIR CIRCUIT IS OFF. "When NB is set to OFF, an
// error occurs"; "When the Noise Reduction setting is OFF, an error occurs".
// The poll skips them rather than drawing a refusal every slow tick, and the
// session applies each switch before its level so that one request can turn a
// blanker on and set its threshold.
//
// AND THE TWO NOTCHES ARE ONE SELECTOR. NT is off, auto or manual — a radio
// here cannot have both, where an Icom or a Yaesu can. Caps.NotchExclusive says
// so and the session refuses the combination; the setters below map a request
// for one onto the selector without disturbing the other where they can.

const (
	reqNB = "NB;"
	reqNL = "NL;"
	reqNR = "NR;"
	reqRL = "RL;"
	reqNT = "NT;"
	reqBP = "BP;"
	reqAN = "AN;"
)

// noiseReads lists the noise and notch queries this model answers in the state
// it is currently in.
func (k *Rig) noiseReads() []read {
	var reads []read
	if k.profile.NoiseBlanker > 0 {
		reads = append(reads, read{reqNB, keyNB})
		// Only while a blanker is running: NL answers ?; with the blanker off,
		// and with BOTH blankers on, which its reference calls out separately.
		if nb := k.noiseBlanker(); nb >= 1 && nb <= k.profile.NoiseBlanker {
			reads = append(reads, read{reqNL, keyNL})
		}
	}
	if k.profile.NoiseReduction > 0 {
		reads = append(reads, read{reqNR, keyNR})
		if k.noiseReduction() > 0 {
			reads = append(reads, read{reqRL, keyRL})
		}
	}
	if k.profile.Notch {
		reads = append(reads, read{reqNT, keyNT})
		if k.profile.NotchFreq {
			reads = append(reads, read{reqBP, keyBP})
		}
	}
	if k.profile.Antennas > 0 {
		reads = append(reads, read{reqAN, keyAN})
	}
	return reads
}

// The last readings, kept for the same reason the AGC's is: they change what
// the next command may be and what its numbers mean.
func (k *Rig) noiseBlanker() int {
	v, _ := k.nb.Load().(int)
	return v
}

func (k *Rig) noiseReduction() int {
	v, _ := k.nr.Load().(int)
	return v
}

func (k *Rig) notchSel() int {
	v, _ := k.notch.Load().(int)
	return v
}

// NT selector values.
const (
	ntOff    = 0
	ntAuto   = 1
	ntManual = 2
)

// SetNoiseBlanker selects a blanker, 0 for off.
func (k *Rig) SetNoiseBlanker(ctx context.Context, c backend.Conn, level int) error {
	if k.profile.NoiseBlanker == 0 {
		return fmt.Errorf("kenwood: the %s has no noise blanker command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if level < 0 || level > k.profile.NoiseBlanker {
		return fmt.Errorf("kenwood: the %s has noise blankers 0 to %d, not %d: %w",
			k.profile.Label, k.profile.NoiseBlanker, level, backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("NB%d;", level)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqNB, keyNB)
	return err
}

// SetNBLevel sets the blanker threshold, 0-100% of its 001-010 range.
func (k *Rig) SetNBLevel(ctx context.Context, c backend.Conn, pct float64) error {
	if !k.profile.NBLevel {
		return fmt.Errorf("kenwood: the %s has no noise blanker level command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("kenwood: noise blanker level %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	n := scaleTo(pct, nbLevelMin, nbLevelMax)
	if err := send(ctx, c, fmt.Sprintf("NL%03d;", n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqNL, keyNL)
	return err
}

// SetNoiseReduction selects a reducer, 0 for off.
func (k *Rig) SetNoiseReduction(ctx context.Context, c backend.Conn, level int) error {
	if k.profile.NoiseReduction == 0 {
		return fmt.Errorf("kenwood: the %s has no noise reduction command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if level < 0 || level > k.profile.NoiseReduction {
		return fmt.Errorf("kenwood: the %s has noise reducers 0 to %d, not %d: %w",
			k.profile.Label, k.profile.NoiseReduction, level, backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("NR%d;", level)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqNR, keyNR)
	return err
}

// nrLevelRange is the RL range for whichever reducer is running.
//
// Two different scales behind one command: NR1's level is 01-10, and NR2's is a
// following speed of 00 to 09, which the reference labels 2 ms to 20 ms. A
// percentage written against the wrong one is off by a step and a whole unit of
// meaning.
func (k *Rig) nrLevelRange() (lo, hi int) {
	if k.noiseReduction() == 2 {
		return 0, 9
	}
	return 1, 10
}

// SetNRLevel sets the reducer's level, 0-100% of whichever range applies.
func (k *Rig) SetNRLevel(ctx context.Context, c backend.Conn, pct float64) error {
	if !k.profile.NRLevel {
		return fmt.Errorf("kenwood: the %s has no noise reduction level command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("kenwood: noise reduction level %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	lo, hi := k.nrLevelRange()
	if err := send(ctx, c, fmt.Sprintf("RL%02d;", scaleTo(pct, lo, hi))); err != nil {
		return err
	}
	_, err := do(ctx, c, reqRL, keyRL)
	return err
}

// setNotchSel writes the NT selector, reads it back, and CHECKS that it took.
//
// The check is here because this radio has a third answer besides yes and no:
// silence. A TS-590S in CW ignores a request for the automatic notch outright —
// NT10 is accepted with no error and a read still answers NT20 — because the
// automatic notch is an SSB and AM function there, cancelling heterodynes in
// voice. In CW it would notch the wanted signal, so the radio simply declines,
// and switching out of SSB turns it off by itself.
//
// Without this the request would answer 200 with a state that plainly shows the
// change did not happen, which is the worst of both: no error to act on and a
// contradiction to puzzle over. Verified on the radio in both modes.
func (k *Rig) setNotchSel(ctx context.Context, c backend.Conn, sel int) error {
	// P2 is the manual notch bandwidth and is ignored unless P1 is 2. It is
	// sent as 0 because the answer always carries 0 there — the radio will not
	// report which width it is using, so remoses does not claim to set one.
	if err := send(ctx, c, fmt.Sprintf("NT%d0;", sel)); err != nil {
		return err
	}
	if _, err := do(ctx, c, reqNT, keyNT); err != nil {
		return err
	}
	if got := k.notchSel(); got != sel {
		return fmt.Errorf("kenwood: the %s did not take notch setting %s and stayed on "+
			"%s%s: %w", k.profile.Label, notchSelName(sel), notchSelName(got),
			notchModeHint(sel, radio.Mode(k.mode.Load())), backend.ErrUnsupported)
	}
	return nil
}

// notchSelName is for error messages, in the words the API uses.
func notchSelName(sel int) string {
	switch sel {
	case ntAuto:
		return "auto notch"
	case ntManual:
		return "manual notch"
	}
	return "notch off"
}

// notchModeHint names the reason where it is known, and says nothing where it
// is not — a guess in an error message is still a guess.
func notchModeHint(sel int, m radio.Mode) string {
	if sel == ntAuto && (m == radio.ModeCW || m == radio.ModeCWR || m == radio.ModeFSK ||
		m == radio.ModeFSKR) {
		return " (the automatic notch is an SSB and AM function on this radio; " +
			"in " + m.String() + " it would notch the wanted signal)"
	}
	return ""
}

// SetNotch switches the manual notch.
//
// Switching it OFF leaves an automatic notch alone: with one selector, "no
// manual notch" is already true while the radio is in auto, so there is nothing
// to send. Sending 0 there would switch the automatic notch off as well —
// cancelling a control the caller did not mention.
func (k *Rig) SetNotch(ctx context.Context, c backend.Conn, on bool) error {
	if !k.profile.Notch {
		return fmt.Errorf("kenwood: the %s has no notch command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if on {
		return k.setNotchSel(ctx, c, ntManual)
	}
	if k.notchSel() == ntManual {
		return k.setNotchSel(ctx, c, ntOff)
	}
	return nil
}

// SetAutoNotch switches the automatic notch, the mirror of SetNotch.
func (k *Rig) SetAutoNotch(ctx context.Context, c backend.Conn, on bool) error {
	if !k.profile.Notch {
		return fmt.Errorf("kenwood: the %s has no notch command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if on {
		return k.setNotchSel(ctx, c, ntAuto)
	}
	if k.notchSel() == ntAuto {
		return k.setNotchSel(ctx, c, ntOff)
	}
	return nil
}

// SetNotchFreq parks the manual notch, 0-100% of its 000-127 range.
func (k *Rig) SetNotchFreq(ctx context.Context, c backend.Conn, pct float64) error {
	if !k.profile.NotchFreq {
		return fmt.Errorf("kenwood: the %s has no notch position command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("kenwood: notch position %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("BP%03d;", scaleTo(pct, 0, bpMax))); err != nil {
		return err
	}
	_, err := do(ctx, c, reqBP, keyBP)
	return err
}

// SetNotchWidth is refused throughout: NT's P2 carries a bandwidth on a set and
// "will always be 0" in an answer, so remoses could write one and never read it
// back. A setting that cannot be confirmed is the failure this backend keeps
// being bitten by, so it is not offered.
func (k *Rig) SetNotchWidth(ctx context.Context, c backend.Conn, w radio.NotchWidth) error {
	return fmt.Errorf("kenwood: the %s reports no notch width — its NT answer always "+
		"carries 0 there, so a width could be written and never confirmed: %w",
		k.profile.Label, backend.ErrUnsupported)
}

// SetAntenna selects an antenna socket.
//
// AN takes three parameters and 9 means "leave this one alone", which is what
// makes the two halves independent: this writes the antenna and leaves the
// receive input and the drive output as they are.
func (k *Rig) SetAntenna(ctx context.Context, c backend.Conn, n int) error {
	if k.profile.Antennas == 0 {
		return fmt.Errorf("kenwood: the %s has no antenna selection command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if n < 1 || n > k.profile.Antennas {
		return fmt.Errorf("kenwood: the %s has antennas 1 to %d, not %d: %w",
			k.profile.Label, k.profile.Antennas, n, backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("AN%d99;", n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqAN, keyAN)
	return err
}

// SetRXAntenna switches the receive-only input, leaving the antenna alone.
func (k *Rig) SetRXAntenna(ctx context.Context, c backend.Conn, on bool) error {
	if !k.profile.RXAntenna {
		return fmt.Errorf("kenwood: the %s has no receive antenna command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	v := 0
	if on {
		v = 1
	}
	if err := send(ctx, c, fmt.Sprintf("AN9%d9;", v)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqAN, keyAN)
	return err
}

// The parameter ranges, which are not the same for any two of these.
const (
	nbLevelMin = 1
	nbLevelMax = 10
	bpMax      = 127
)

// scaleTo maps 0-100% onto an inclusive integer range.
func scaleTo(pct float64, lo, hi int) int {
	n := lo + int(pct/100*float64(hi-lo)+0.5)
	return min(max(n, lo), hi)
}

// scaleFrom is the reverse, for decoding.
func scaleFrom(n, lo, hi int) float64 {
	if hi <= lo {
		return 0
	}
	pct := float64(n-lo) / float64(hi-lo) * 100
	return min(max(pct, 0), 100)
}

// decodeNB reads the blanker selection. 3 — both blankers at once — is not
// published: it is a combination rather than a level, the same shape as the
// IC-9700's preamp, and calling it "3" would tell a client it is more blanking
// than 2.
func (k *Rig) decodeNB(u *backend.Update, arg []byte) {
	n, err := strconv.Atoi(string(arg))
	if err != nil {
		return
	}
	k.nb.Store(n)
	if n <= k.profile.NoiseBlanker {
		u.Patch.NoiseBlanker = &n
	}
}

func (k *Rig) decodeNL(u *backend.Update, arg []byte) {
	n, err := strconv.Atoi(string(arg))
	if err != nil {
		return
	}
	pct := scaleFrom(n, nbLevelMin, nbLevelMax)
	u.Patch.NBLevel = &pct
}

func (k *Rig) decodeNR(u *backend.Update, arg []byte) {
	n, err := strconv.Atoi(string(arg))
	if err != nil {
		return
	}
	k.nr.Store(n)
	if n <= k.profile.NoiseReduction {
		u.Patch.NoiseReduction = &n
	}
}

func (k *Rig) decodeRL(u *backend.Update, arg []byte) {
	n, err := strconv.Atoi(string(arg))
	if err != nil {
		return
	}
	lo, hi := k.nrLevelRange()
	pct := scaleFrom(n, lo, hi)
	u.Patch.NRLevel = &pct
}

// decodeNT reads the one selector into the two published switches.
func (k *Rig) decodeNT(u *backend.Update, arg []byte) {
	if len(arg) < 1 {
		return
	}
	sel, err := strconv.Atoi(string(arg[:1]))
	if err != nil {
		return
	}
	k.notch.Store(sel)
	manual, auto := sel == ntManual, sel == ntAuto
	u.Patch.Notch = &manual
	u.Patch.AutoNotch = &auto
}

func (k *Rig) decodeBP(u *backend.Update, arg []byte) {
	n, err := strconv.Atoi(string(arg))
	if err != nil {
		return
	}
	pct := scaleFrom(n, 0, bpMax)
	u.Patch.NotchFreq = &pct
}

// decodeAN reads the antenna answer: ANT number, receive input, drive output.
func (k *Rig) decodeAN(u *backend.Update, arg []byte) {
	if len(arg) < 2 {
		return
	}
	if n, err := strconv.Atoi(string(arg[:1])); err == nil &&
		n >= 1 && n <= k.profile.Antennas {
		u.Patch.Antenna = &n
	}
	if k.profile.RXAntenna && (arg[1] == '0' || arg[1] == '1') {
		on := arg[1] == '1'
		u.Patch.RXAntenna = &on
	}
}
