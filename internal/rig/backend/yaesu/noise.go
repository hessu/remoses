package yaesu

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The noise processing, the notch filters and the antenna selector.
//
// Like the rest of this backend they take a leading 0 for the main receiver,
// and answer with it still in front. Two of them are shaped differently from
// everything else here.
//
// BP CARRIES TWO DIFFERENT THINGS ON ONE COMMAND, chosen by its second
// parameter: "P2 0: Manual NOTCH ON/OFF" with P3 000 or 001, and "P2 1: Manual
// NOTCH LEVEL" with P3 001-320, the notch frequency in tens of Hz. So the
// switch and the position are two reads and two writes of the same two letters,
// and a decoder has to look at P2 before it can know what P3 means.
//
// AND THE NOTCH POSITION IS A REAL FREQUENCY, where Icom and Kenwood both give
// an opaque index. It is still published as a percentage, because a client that
// drew Hz from one make and an index from another would need two controls for
// one knob — but the conversion here is against an actual 10 Hz to 3200 Hz.

const (
	reqNB = "NB0;"
	reqNL = "NL0;"
	reqNR = "NR0;"
	reqRL = "RL0;"
	reqBC = "BC0;"
	reqAN = "AN0;"
	// The two BP reads: the switch and the position.
	reqBPSwitch = "BP00;"
	reqBPFreq   = "BP01;"
)

// The parameter ranges, none of which match another family's.
const (
	nbLevelMin  = 0
	nbLevelMax  = 10
	nrLevelMin  = 1
	nrLevelMax  = 15
	notchFreqLo = 1   // ×10 Hz
	notchFreqHi = 320 // ×10 Hz, so 3200 Hz
)

// noiseReads lists the noise and notch queries this model answers.
func (y *Rig) noiseReads() []read {
	var reads []read
	if y.profile.NoiseBlanker {
		reads = append(reads, read{reqNB, keyNB}, read{reqNL, keyNL})
	}
	if y.profile.NoiseReduction {
		reads = append(reads, read{reqNR, keyNR}, read{reqRL, keyRL})
	}
	if y.profile.Notch {
		// Both halves of BP, and they answer with the same two letters — the
		// P2 in each answer is what tells them apart. See decodeBP.
		reads = append(reads, read{reqBPSwitch, keyBP}, read{reqBPFreq, keyBP})
	}
	if y.profile.AutoNotch {
		reads = append(reads, read{reqBC, keyBC})
	}
	if y.profile.Antennas > 0 {
		reads = append(reads, read{reqAN, keyAN})
	}
	return reads
}

// boolCount turns "the radio has this circuit" into the count Caps publishes.
// One is as many as this family has of either.
func boolCount(has bool) int {
	if has {
		return 1
	}
	return 0
}

// scaleTo maps 0-100% onto an inclusive integer range; scaleFrom reverses it.
func scaleTo(pct float64, lo, hi int) int {
	n := lo + int(pct/100*float64(hi-lo)+0.5)
	return min(max(n, lo), hi)
}

func scaleFrom(n, lo, hi int) float64 {
	if hi <= lo {
		return 0
	}
	return min(max(float64(n-lo)/float64(hi-lo)*100, 0), 100)
}

// SetNoiseBlanker switches the blanker. One circuit here, so 0 or 1.
func (y *Rig) SetNoiseBlanker(ctx context.Context, c backend.Conn, level int) error {
	if !y.profile.NoiseBlanker {
		return fmt.Errorf("yaesu: the %s has no noise blanker command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if level < 0 || level > 1 {
		return fmt.Errorf("yaesu: the %s has one noise blanker, so 0 or 1, not %d: %w",
			y.profile.Label, level, backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("NB%c%d;", mainRX, level)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqNB, keyNB)
	return err
}

// SetNBLevel sets the blanker threshold, 0-100% of its 000-010 range.
func (y *Rig) SetNBLevel(ctx context.Context, c backend.Conn, pct float64) error {
	if !y.profile.NoiseBlanker {
		return fmt.Errorf("yaesu: the %s has no noise blanker level command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("yaesu: noise blanker level %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	n := scaleTo(pct, nbLevelMin, nbLevelMax)
	if err := send(ctx, c, fmt.Sprintf("NL%c%03d;", mainRX, n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqNL, keyNL)
	return err
}

// SetNoiseReduction switches the reducer.
func (y *Rig) SetNoiseReduction(ctx context.Context, c backend.Conn, level int) error {
	if !y.profile.NoiseReduction {
		return fmt.Errorf("yaesu: the %s has no noise reduction command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if level < 0 || level > 1 {
		return fmt.Errorf("yaesu: the %s has one noise reducer, so 0 or 1, not %d: %w",
			y.profile.Label, level, backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("NR%c%d;", mainRX, level)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqNR, keyNR)
	return err
}

// SetNRLevel sets the reducer's strength, 0-100% of its 01-15 range.
func (y *Rig) SetNRLevel(ctx context.Context, c backend.Conn, pct float64) error {
	if !y.profile.NoiseReduction {
		return fmt.Errorf("yaesu: the %s has no noise reduction level command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("yaesu: noise reduction level %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	n := scaleTo(pct, nrLevelMin, nrLevelMax)
	if err := send(ctx, c, fmt.Sprintf("RL%c%02d;", mainRX, n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqRL, keyRL)
	return err
}

// SetNotch switches the manual notch: BP with P2 of 0, and P3 000 or 001.
func (y *Rig) SetNotch(ctx context.Context, c backend.Conn, on bool) error {
	if !y.profile.Notch {
		return fmt.Errorf("yaesu: the %s has no manual notch command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	v := 0
	if on {
		v = 1
	}
	if err := send(ctx, c, fmt.Sprintf("BP%c0%03d;", mainRX, v)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqBPSwitch, keyBP)
	return err
}

// SetNotchFreq parks the manual notch: BP with P2 of 1, and P3 in tens of Hz.
func (y *Rig) SetNotchFreq(ctx context.Context, c backend.Conn, pct float64) error {
	if !y.profile.Notch {
		return fmt.Errorf("yaesu: the %s has no notch position command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("yaesu: notch position %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	n := scaleTo(pct, notchFreqLo, notchFreqHi)
	if err := send(ctx, c, fmt.Sprintf("BP%c1%03d;", mainRX, n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqBPFreq, keyBP)
	return err
}

// SetNotchWidth is refused: no command list read for this backend has a notch
// width row. The radios have the control on the panel; the bus does not.
func (y *Rig) SetNotchWidth(ctx context.Context, c backend.Conn, w radio.NotchWidth) error {
	return fmt.Errorf("yaesu: the %s has no notch width command: %w",
		y.profile.Label, backend.ErrUnsupported)
}

// SetAutoNotch switches the automatic notch, which Yaesu calls BC.
func (y *Rig) SetAutoNotch(ctx context.Context, c backend.Conn, on bool) error {
	if !y.profile.AutoNotch {
		return fmt.Errorf("yaesu: the %s has no auto notch command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	v := 0
	if on {
		v = 1
	}
	if err := send(ctx, c, fmt.Sprintf("BC%c%d;", mainRX, v)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqBC, keyBC)
	return err
}

// SetAntenna selects an antenna socket, counting from 1.
func (y *Rig) SetAntenna(ctx context.Context, c backend.Conn, n int) error {
	if y.profile.Antennas == 0 {
		return fmt.Errorf("yaesu: the %s has no antenna selection command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if n < 1 || n > y.profile.Antennas {
		return fmt.Errorf("yaesu: the %s has antennas 1 to %d, not %d: %w",
			y.profile.Label, y.profile.Antennas, n, backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("AN%c%d;", mainRX, n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqAN, keyAN)
	return err
}

// SetRXAntenna is refused: no command list read for this backend has a separate
// receive-antenna row. AN chooses between the transmit sockets and nothing else.
func (y *Rig) SetRXAntenna(ctx context.Context, c backend.Conn, on bool) error {
	return fmt.Errorf("yaesu: the %s has no receive antenna command: %w",
		y.profile.Label, backend.ErrUnsupported)
}

// decodeNB and friends. Each skips the leading receiver selector.
func (y *Rig) decodeNB(u *backend.Update, arg []byte) {
	if len(arg) < 2 {
		return
	}
	n := int(arg[1] - '0')
	if n == 0 || n == 1 {
		u.Patch.NoiseBlanker = &n
	}
}

func (y *Rig) decodeNL(u *backend.Update, arg []byte) {
	if len(arg) < 4 {
		return
	}
	n, err := strconv.Atoi(string(arg[1:4]))
	if err != nil {
		return
	}
	pct := scaleFrom(n, nbLevelMin, nbLevelMax)
	u.Patch.NBLevel = &pct
}

func (y *Rig) decodeNR(u *backend.Update, arg []byte) {
	if len(arg) < 2 {
		return
	}
	n := int(arg[1] - '0')
	if n == 0 || n == 1 {
		u.Patch.NoiseReduction = &n
	}
}

func (y *Rig) decodeRL(u *backend.Update, arg []byte) {
	if len(arg) < 3 {
		return
	}
	n, err := strconv.Atoi(string(arg[1:3]))
	if err != nil {
		return
	}
	pct := scaleFrom(n, nrLevelMin, nrLevelMax)
	u.Patch.NRLevel = &pct
}

func (y *Rig) decodeBC(u *backend.Update, arg []byte) {
	if len(arg) < 2 {
		return
	}
	on := arg[1] == '1'
	u.Patch.AutoNotch = &on
}

// decodeBP reads whichever half of the command answered.
//
// The second parameter says which: 0 is the switch, whose P3 is 000 or 001, and
// 1 is the position, whose P3 is the notch frequency in tens of Hz. Reading one
// as the other would publish a 3200 Hz notch as "on" or a switch as a position
// at the bottom of the range.
func (y *Rig) decodeBP(u *backend.Update, arg []byte) {
	if len(arg) < 5 {
		return
	}
	n, err := strconv.Atoi(string(arg[2:5]))
	if err != nil {
		return
	}
	switch arg[1] {
	case '0':
		on := n != 0
		u.Patch.Notch = &on
	case '1':
		pct := scaleFrom(n, notchFreqLo, notchFreqHi)
		u.Patch.NotchFreq = &pct
	}
}

func (y *Rig) decodeAN(u *backend.Update, arg []byte) {
	if len(arg) < 2 {
		return
	}
	n := int(arg[1] - '0')
	if n >= 1 && n <= y.profile.Antennas {
		u.Patch.Antenna = &n
	}
}
