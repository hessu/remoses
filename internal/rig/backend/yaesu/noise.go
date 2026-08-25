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
// and answer with it still in front — with one exception, which is the whole
// reason this file needs the model as often as it does. Nothing here is
// family-wide except BC.
//
// BP CARRIES TWO DIFFERENT THINGS ON ONE COMMAND, and the family spells that
// two ways. Eleven of the twelve choose between them with a second parameter:
// "P2 0: Manual NOTCH ON/OFF" with P3 000 or 001, and "P2 1: Manual NOTCH
// LEVEL" with P3 the notch frequency in tens of Hz. So the switch and the
// position are two reads and two writes of the same two letters, and a decoder
// has to look at P2 before it can know what P3 means. The FTdx9000 folds all of
// it into one three-digit parameter with no selector and no receiver byte —
// see NotchCombined, which is where that story is written down.
//
// AND THE NOTCH POSITION IS A REAL FREQUENCY, where Icom and Kenwood both give
// an opaque index. It is still published as a percentage, because a client that
// drew Hz from one make and an index from another would need two controls for
// one knob — but the conversion here is against an actual 10 Hz to 3000, 3200
// or 4000 Hz, per model. See Model.NotchFreqMax: that ceiling is three
// different numbers across the twelve and does not follow the generation
// boundary, so a percentage taken against a family constant would be a
// confidently wrong frequency and, on the way out, an out-of-range parameter
// answered with silence.
//
// THE LEVELS ARE NOT ONE SCALE EITHER. NL counts to 010, 100 or 255 depending
// on the radio and RL to 10 or 15, and on the FTX-1 both of them carry their
// own off value because that radio has no NB and no NR command at all. Every
// bound comes from the profile; there are no range constants in this file.

const (
	reqNB = "NB0;"
	reqNL = "NL0;"
	reqNR = "NR0;"
	reqRL = "RL0;"
	reqBC = "BC0;"
	reqAN = "AN0;"
	// The two BP reads on the eleven radios whose BP has a sub-command
	// selector: the switch and the position.
	reqBPSwitch = "BP00;"
	reqBPFreq   = "BP01;"
	// And the one read on the FTdx9000, whose BP has neither a receiver
	// selector nor a sub-command one, so there is a single thing to ask for and
	// a single answer carrying both halves.
	reqBPCombined = "BP;"
)

// notchFreqLo is the bottom of BP's position range, and it is the one number in
// this group that IS family-wide: every manual read for this backend prints the
// position as "001 - " something. 000 is not the bottom of the scale, it is the
// switch's off value — which is what NotchCombined has to live with.
const notchFreqLo = 1 // ×10 Hz

// noiseReads lists the noise and notch queries this model answers.
//
// Each is gated on the profile rather than sent to everyone, because a Yaesu
// answers a command it does not implement with silence: the FTX-1 has no NB and
// no NR row in its command list, and asking it anyway cost two full per-command
// timeouts on every slow tick.
func (y *Rig) noiseReads() []read {
	var reads []read
	if y.profile.NBCircuits > 0 {
		reads = append(reads, read{reqNB, keyNB})
	}
	if y.profile.NBLevelMax > 0 {
		reads = append(reads, read{reqNL, keyNL})
	}
	if y.profile.NRCircuits > 0 {
		reads = append(reads, read{reqNR, keyNR})
	}
	if y.profile.NRLevelMax > 0 {
		reads = append(reads, read{reqRL, keyRL})
	}
	if y.profile.Notch {
		if y.profile.NotchShape == NotchCombined {
			// One read, because one parameter carries both halves.
			reads = append(reads, read{reqBPCombined, keyBP})
		} else {
			// Both halves of BP, and they answer with the same two letters — the
			// P2 in each answer is what tells them apart. See decodeBP.
			reads = append(reads, read{reqBPSwitch, keyBP}, read{reqBPFreq, keyBP})
		}
	}
	if y.profile.AutoNotch {
		reads = append(reads, read{reqBC, keyBC})
	}
	if y.profile.Antennas > 0 {
		reads = append(reads, read{reqAN, keyAN})
	}
	return reads
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

// SetNoiseBlanker switches the blanker, or picks which of two it uses.
//
// The count is per model and it is a count of CIRCUITS, not strengths. The
// whole FT-950 generation prints a third value — "2: Noise Blanker (Wide)
// "ON"" — which is a blanker for a longer pulse rather than a harder setting of
// the same one, so it is offered as level 2 the way a Kenwood's NB2 is. The
// FTdx101 generation has only 0 and 1, and the FTX-1 has no NB command at all.
func (y *Rig) SetNoiseBlanker(ctx context.Context, c backend.Conn, level int) error {
	if y.profile.NBCircuits == 0 {
		if y.profile.NBLevelIsSwitch {
			return fmt.Errorf("yaesu: the %s has no noise blanker switch: its command list has no NB "+
				"row, and NL's 000 is the documented \"OFF\" — set the blanker level instead, "+
				"where 0%% switches it out: %w", y.profile.Label, backend.ErrUnsupported)
		}
		return fmt.Errorf("yaesu: the %s has no noise blanker command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if level < 0 || level > y.profile.NBCircuits {
		return fmt.Errorf("yaesu: the %s has noise blankers 0 to %d, not %d: %w",
			y.profile.Label, y.profile.NBCircuits, level, backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("NB%c%d;", mainRX, level)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqNB, keyNB)
	return err
}

// SetNBLevel sets the blanker threshold, 0-100% of this radio's own count.
//
// The count is 000-010, 000-100 or 000-255 depending on the model and nothing
// in the frame distinguishes them, so Model.NBLevelMax is the only thing that
// can: NL0010 is full scale on an FTdx101 and 4% on an FT-950.
func (y *Rig) SetNBLevel(ctx context.Context, c backend.Conn, pct float64) error {
	if y.profile.NBLevelMax == 0 {
		return fmt.Errorf("yaesu: the %s has no noise blanker level command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("yaesu: noise blanker level %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	n := scaleTo(pct, y.profile.NBLevelMin, y.profile.NBLevelMax)
	if err := send(ctx, c, fmt.Sprintf("NL%c%03d;", mainRX, n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqNL, keyNL)
	return err
}

// SetNoiseReduction switches the reducer.
func (y *Rig) SetNoiseReduction(ctx context.Context, c backend.Conn, level int) error {
	if y.profile.NRCircuits == 0 {
		if y.profile.NRLevelIsSwitch {
			return fmt.Errorf("yaesu: the %s has no noise reduction switch: its command list has no "+
				"NR row, and RL's 00 is the documented \"OFF\" — set the reduction level instead, "+
				"where 0%% switches it out: %w", y.profile.Label, backend.ErrUnsupported)
		}
		return fmt.Errorf("yaesu: the %s has no noise reduction command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if level < 0 || level > y.profile.NRCircuits {
		return fmt.Errorf("yaesu: the %s has noise reducers 0 to %d, not %d: %w",
			y.profile.Label, y.profile.NRCircuits, level, backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("NR%c%d;", mainRX, level)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqNR, keyNR)
	return err
}

// SetNRLevel sets the reducer's strength, 0-100% of its own ladder: 01-15 on
// eleven models and 00-10 on the FTX-1, whose 00 is a documented "OFF".
func (y *Rig) SetNRLevel(ctx context.Context, c backend.Conn, pct float64) error {
	if y.profile.NRLevelMax == 0 {
		return fmt.Errorf("yaesu: the %s has no noise reduction level command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("yaesu: noise reduction level %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	n := scaleTo(pct, y.profile.NRLevelMin, y.profile.NRLevelMax)
	if err := send(ctx, c, fmt.Sprintf("RL%c%02d;", mainRX, n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqRL, keyRL)
	return err
}

// SetNotch switches the manual notch.
//
// On the eleven radios whose BP has a sub-command selector this is BP with P2
// of 0 and P3 000 or 001, which says nothing about where the notch sits.
//
// On the FTdx9000 there is no such value. Its one parameter is "000: Manual
// NOTCH "OFF" / 001 - 300: NOTCH Frequency", so switching the notch OUT is
// exactly BP000; and switching it IN means naming a position — the radio has no
// spelling for "on, where it was". remoses sends back the last position the rig
// itself reported this session rather than inventing one, and refuses when the
// notch has been out for the whole session and there is nothing to restore. That
// is the same conditional shape SetFilterWidth already has in a mode with no
// bandwidth table: a capability that is real and that a particular moment can
// still decline.
func (y *Rig) SetNotch(ctx context.Context, c backend.Conn, on bool) error {
	if !y.profile.Notch {
		return fmt.Errorf("yaesu: the %s has no manual notch command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if y.profile.NotchShape == NotchCombined {
		return y.setNotchCombined(ctx, c, on)
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

// setNotchCombined is SetNotch on the FTdx9000, where the switch and the
// position share one three-digit parameter.
func (y *Rig) setNotchCombined(ctx context.Context, c backend.Conn, on bool) error {
	n := 0
	if on {
		n = int(y.notchPos.Load())
		if n == 0 {
			return fmt.Errorf("yaesu: the %s's manual notch carries its switch and its position on "+
				"one parameter — 000 is \"OFF\" and 001-300 is a frequency in tens of Hz — so "+
				"switching it in means naming a position, and the rig has not reported one this "+
				"session; set the notch position instead, which is what switches it in: %w",
				y.profile.Label, backend.ErrUnsupported)
		}
	}
	if err := send(ctx, c, fmt.Sprintf("BP%03d;", n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqBPCombined, keyBP)
	return err
}

// SetNotchFreq parks the manual notch, 0-100% of its own range in tens of Hz.
//
// On the FTdx9000 this also switches the notch in, because its parameter is the
// switch: any value from 001 up is a position AND an "on".
func (y *Rig) SetNotchFreq(ctx context.Context, c backend.Conn, pct float64) error {
	if !y.profile.Notch {
		return fmt.Errorf("yaesu: the %s has no notch position command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("yaesu: notch position %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	n := scaleTo(pct, notchFreqLo, y.profile.NotchFreqMax)
	y.notchPos.Store(uint32(n))
	if y.profile.NotchShape == NotchCombined {
		if err := send(ctx, c, fmt.Sprintf("BP%03d;", n)); err != nil {
			return err
		}
		_, err := do(ctx, c, reqBPCombined, keyBP)
		return err
	}
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

// SetAntenna selects a transmit antenna socket, counting from 1.
//
// The set is the same six characters on all six radios that have AN — the
// receiver or a fixed 0, then the socket — and it is only the answer that
// differs. See AntennaShape.
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

// SetRXAntenna is refused on every model, and on two of them that is a decision
// rather than a missing command.
//
// Nine of the twelve genuinely have nothing: the FT-891, FT-991A, FT-710,
// FTdx10 and FTX-1 have no AN row at all, and the FT-950, FTdx1200, FTdx3000
// and FTdx101D/MP have an AN whose every value is a transmit socket.
//
// The FTdx5000 and FTdx9000 do have a receive antenna, and remoses publishes
// the READING on both — see decodeAN, where each manual states it plainly. What
// neither manual states is how to switch it out again, and they disagree even
// about what it is:
//
//   - On the FTdx5000 it is an overlay. The answer reports the transmit socket
//     and ANT RX separately, "P3 1: ANT "1" ... 4: ANT "4"" and "P4 0: ANT "RX"
//     "OFF", 1: ANT "RX" "ON"", but the SET has exactly one value that reaches
//     it, "P2 ... 5: ANT "RX"", and no companion that clears it. Whether that 5
//     sets ANT RX or toggles it is not printed (page 4).
//   - On the FTdx9000 it is the fifth position of one selector, "1: Antenna
//     "1" ... 5: Antenna "RX"", so clearing it means naming a transmit antenna
//     — and while ANT RX is selected the answer does not say which transmit
//     antenna it would be going back to (page 3).
//
// So there is one documented direction on one radio and none on the other, and
// this backend does not guess a command's effect any more than it guesses a
// frame. Caps.RXAntennaControl is false and the reading stands on its own. One
// AN05; against a real FTdx5000, followed by a second, would settle it.
func (y *Rig) SetRXAntenna(ctx context.Context, c backend.Conn, on bool) error {
	switch y.profile.AntennaShape {
	case AntennaRXFlag, AntennaRXSlot:
		return fmt.Errorf("yaesu: the %s reports its receive antenna but its AN has no value that "+
			"switches one out — 5 selects ANT \"RX\" and nothing clears it — so remoses reads it "+
			"and does not write it: %w", y.profile.Label, backend.ErrUnsupported)
	}
	return fmt.Errorf("yaesu: the %s has no receive antenna command: %w",
		y.profile.Label, backend.ErrUnsupported)
}

// decodeNB reads the blanker answer, NB0<n>.
//
// The range is the model's own count rather than a flat 0/1: the FT-950
// generation's third value is "Noise Blanker (Wide) "ON"", and rejecting it —
// which is what a hard 0-or-1 check did — left a rig sitting in wide-blanker
// mode reading back as having no blanker at all, because the last good value
// stayed in the cache.
func (y *Rig) decodeNB(u *backend.Update, arg []byte) {
	if len(arg) < 2 {
		return
	}
	n := int(arg[1] - '0')
	if n >= 0 && n <= y.profile.NBCircuits {
		u.Patch.NoiseBlanker = &n
	}
}

// decodeNL reads the blanker threshold, NL0<nnn>, against this model's own full
// scale — 010, 100 or 255.
//
// On the FTX-1 it also carries the switch. That radio has no NB command and its
// NL prints "000: OFF", so the same three digits say both whether the blanker is
// in circuit and how hard it bites, and publishing only the level would leave
// State.NoiseBlanker empty on a radio that can perfectly well answer the
// question.
func (y *Rig) decodeNL(u *backend.Update, arg []byte) {
	if len(arg) < 4 {
		return
	}
	n, err := strconv.Atoi(string(arg[1:4]))
	if err != nil {
		return
	}
	pct := scaleFrom(n, y.profile.NBLevelMin, y.profile.NBLevelMax)
	u.Patch.NBLevel = &pct
	if y.profile.NBLevelIsSwitch {
		on := 0
		if n != 0 {
			on = 1
		}
		u.Patch.NoiseBlanker = &on
	}
}

func (y *Rig) decodeNR(u *backend.Update, arg []byte) {
	if len(arg) < 2 {
		return
	}
	n := int(arg[1] - '0')
	if n >= 0 && n <= y.profile.NRCircuits {
		u.Patch.NoiseReduction = &n
	}
}

// decodeRL reads the reducer's strength, RL0<nn>, and — on the FTX-1, whose
// command list has no NR row and whose RL prints "00: "OFF"" — its switch too.
// Same reasoning as decodeNL.
func (y *Rig) decodeRL(u *backend.Update, arg []byte) {
	if len(arg) < 3 {
		return
	}
	n, err := strconv.Atoi(string(arg[1:3]))
	if err != nil {
		return
	}
	pct := scaleFrom(n, y.profile.NRLevelMin, y.profile.NRLevelMax)
	u.Patch.NRLevel = &pct
	if y.profile.NRLevelIsSwitch {
		on := 0
		if n != 0 {
			on = 1
		}
		u.Patch.NoiseReduction = &on
	}
}

func (y *Rig) decodeBC(u *backend.Update, arg []byte) {
	if len(arg) < 2 {
		return
	}
	on := arg[1] == '1'
	u.Patch.AutoNotch = &on
}

// decodeBP reads whichever half of the command answered, and which halves there
// are is per model.
//
// On the eleven radios with a sub-command selector the second parameter says
// which: 0 is the switch, whose P3 is 000 or 001, and 1 is the position, whose
// P3 is the notch frequency in tens of Hz. Reading one as the other would
// publish a 3200 Hz notch as "on" or a switch as a position at the bottom of the
// range.
//
// On the FTdx9000 there is no selector and no receiver byte: the whole answer is
// BP<nnn>, and those three digits are the switch AND the position at once. Both
// are published from it, which is the one place in this backend where a single
// answer settles two fields — and a length check is what keeps the two shapes
// apart, since three characters of argument cannot be a five-character one.
func (y *Rig) decodeBP(u *backend.Update, arg []byte) {
	if y.profile.NotchShape == NotchCombined {
		y.decodeBPCombined(u, arg)
		return
	}
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
		pct := scaleFrom(n, notchFreqLo, y.profile.NotchFreqMax)
		u.Patch.NotchFreq = &pct
		y.notchPos.Store(uint32(n))
	}
}

// decodeBPCombined is the FTdx9000's answer: BP<nnn>, where 000 is the notch
// switched out and 001-300 is where it sits.
//
// The position is published even when the notch is out, and that is deliberate
// in the other direction from the switch: a 000 carries no position at all, so
// nothing is published for it and the last known position stays in the cache,
// which is the number a client redrawing the control wants. The same argument
// State.DigiSelShift's doc makes.
func (y *Rig) decodeBPCombined(u *backend.Update, arg []byte) {
	if len(arg) != 3 {
		return
	}
	n, err := strconv.Atoi(string(arg))
	if err != nil {
		return
	}
	on := n != 0
	u.Patch.Notch = &on
	if n == 0 {
		return
	}
	pct := scaleFrom(n, notchFreqLo, y.profile.NotchFreqMax)
	u.Patch.NotchFreq = &pct
	y.notchPos.Store(uint32(n))
}

// decodeAN reads the antenna answer, whose layout is per model.
//
// All three shapes put the selector in the first character and the antenna in
// the second. What differs is what comes after it, and on two radios that is a
// receive antenna reported two incompatible ways — see AntennaShape. Neither is
// guessed at: the FTdx5000's P4 is a printed ANT RX on/off and the FTdx9000's
// fifth position is a printed Antenna "RX", and a 5 there means the answer is
// not naming a transmit socket at all, so Antenna is left alone rather than
// being filled in with a socket number the radio did not report.
func (y *Rig) decodeAN(u *backend.Update, arg []byte) {
	if len(arg) < 2 {
		return
	}
	n := int(arg[1] - '0')
	if y.profile.AntennaShape == AntennaRXSlot && n == y.profile.Antennas+1 {
		rx := true
		u.Patch.RXAntenna = &rx
		return
	}
	if n < 1 || n > y.profile.Antennas {
		return
	}
	u.Patch.Antenna = &n
	switch y.profile.AntennaShape {
	case AntennaRXFlag:
		// The FTdx5000 reports the receive antenna in its own parameter behind
		// the socket, so one answer settles both.
		if len(arg) >= 3 && (arg[2] == '0' || arg[2] == '1') {
			rx := arg[2] == '1'
			u.Patch.RXAntenna = &rx
		}
	case AntennaRXSlot:
		// One five-position selector, so naming a transmit socket is also saying
		// the receive antenna is not selected.
		rx := false
		u.Patch.RXAntenna = &rx
	}
}
