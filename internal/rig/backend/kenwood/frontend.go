package kenwood

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The receive front end: PA the preamplifier, RA the attenuator, RG the RF gain,
// and the AGC on GC or GT depending on the generation.
//
// Three shapes of trap here, all of them per model and all of them in the model
// table rather than in this file.
//
// THE SET AND THE ANSWER ARE DIFFERENT WIDTHS. PA takes one digit and answers
// two — "P2: 0: Always 0" — and RA on a TS-480 or TS-590 takes two and answers
// four, for the same reason. Parsing an answer with the setter's width would
// read the padding as the value.
//
// THE SAME LETTERS MEAN DIFFERENT THINGS ACROSS THE FAMILY. A TS-480's AGC is
// GT000/GT001/GT002; on every radio since, GT is the AGC time constant and the
// speed moved to GC. Sending a TS-590's GC to a TS-480 would be a syntax error;
// sending a TS-480's GT to a TS-590 would set a time constant.
//
// AND THE SCALES DIFFER. RG counts 000-100 on a TS-480 and 000-255 on everything
// after it, so the API publishes a percentage and each model states its own top.

// Front-end commands. The read forms; the set forms are built per model,
// because their widths and their band prefixes are not constant.
const (
	reqPA = "PA;"
	reqRA = "RA;"
	reqRG = "RG;"
)

// band is the leading main/sub selector the TS-990S puts on these commands, and
// the empty string everywhere else. remoses works the main band.
func (k *Rig) band() string {
	if k.profile.Banded {
		return "0"
	}
	return ""
}

// frontEndReads lists the front-end queries this model answers in the mode it
// is currently in.
//
// The AGC is conditional on the mode for the same reason break-in is. Every
// reference in this family carries the same note — "this command cannot be
// performed in FM mode (an error sounds)" — and a TS-480 makes it worse by
// answering three spaces rather than refusing. Asking anyway would draw an
// audible error tone at the radio on every slow tick, which is a genuinely
// unpleasant thing to do to somebody wearing headphones.
func (k *Rig) frontEndReads() []read {
	var reads []read
	if k.profile.Preamp > 0 {
		reads = append(reads, read{reqPA, keyPA})
	}
	if len(k.profile.Attenuator) > 0 {
		reads = append(reads, read{reqRA, keyRA})
	}
	if k.profile.RFGainMax > 0 {
		reads = append(reads, read{reqRG, keyRG})
	}
	if len(k.profile.AGC) > 0 && agcLegal(k.lastMode()) {
		reads = append(reads, read{k.profile.AGCCmd + ";", k.agcKey()})
	}
	return reads
}

// agcLegal reports whether the AGC command can be used in this mode. FM is the
// exception every reference in the family names; an unknown mode is attempted,
// since refusing to ask would lose the reading on a radio that has not yet
// reported one.
func agcLegal(m radio.Mode) bool { return m != radio.ModeFM }

// agcKey is the reply key for whichever command this model puts the AGC on.
func (k *Rig) agcKey() backend.Key {
	if k.profile.AGCCmd == "GT" {
		return keyGT
	}
	return keyGC
}

// agcSettings lists a model's AGC speeds in a stable order: off first, then
// fastest to slowest, the way a radio's own menu reads.
func agcSettings(m map[radio.AGC]string) []radio.AGC {
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

// agcValue decodes an AGC parameter against a model's own map.
func agcValue(m map[radio.AGC]string, s string) (radio.AGC, bool) {
	for v, code := range m {
		if code == s {
			return v, true
		}
	}
	return radio.AGCUnknown, false
}

// attenuatorIndex is the RA parameter for a depth in dB, and attenuatorDB the
// reverse. The wire carries a step index, 1-based; the API carries dB.
func (k *Rig) attenuatorIndex(db int) (int, bool) {
	if db == 0 {
		return 0, true
	}
	for i, step := range k.profile.Attenuator {
		if step == db {
			return i + 1, true
		}
	}
	return 0, false
}

func (k *Rig) attenuatorDB(index int) (int, bool) {
	if index == 0 {
		return 0, true
	}
	if index >= 1 && index <= len(k.profile.Attenuator) {
		return k.profile.Attenuator[index-1], true
	}
	return 0, false
}

// SetPreamp selects a preamplifier, 0 for off.
func (k *Rig) SetPreamp(ctx context.Context, c backend.Conn, level int) error {
	if k.profile.Preamp == 0 {
		return fmt.Errorf("kenwood: the %s has no preamplifier command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if level < 0 || level > k.profile.Preamp {
		return fmt.Errorf("kenwood: the %s has preamplifiers 0 to %d, not %d: %w",
			k.profile.Label, k.profile.Preamp, level, backend.ErrUnsupported)
	}
	if err := send(ctx, c, fmt.Sprintf("PA%s%d;", k.band(), level)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqPA, keyPA)
	return err
}

// SetAttenuator sets the attenuator in dB, 0 for switched out.
func (k *Rig) SetAttenuator(ctx context.Context, c backend.Conn, db int) error {
	if len(k.profile.Attenuator) == 0 {
		return fmt.Errorf("kenwood: the %s has no attenuator command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	idx, ok := k.attenuatorIndex(db)
	if !ok {
		return fmt.Errorf("kenwood: the %s has no %d dB attenuator setting, only %v: %w",
			k.profile.Label, db, k.profile.Attenuator, backend.ErrUnsupported)
	}
	// The width is the model's: RA00/RA01 on a TS-590, RA0 on a TS-890S, and
	// RA<band><value> on a TS-990S.
	if err := send(ctx, c, fmt.Sprintf("RA%s%0*d;", k.band(), k.profile.AttenuatorWidth, idx)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqRA, keyRA)
	return err
}

// SetRFGain sets the receiver RF gain, 0-100%.
func (k *Rig) SetRFGain(ctx context.Context, c backend.Conn, pct float64) error {
	if k.profile.RFGainMax == 0 {
		return fmt.Errorf("kenwood: the %s has no RF gain command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("kenwood: RF gain %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	n := int(pct/100*float64(k.profile.RFGainMax) + 0.5)
	if err := send(ctx, c, fmt.Sprintf("RG%s%03d;", k.band(), n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqRG, keyRG)
	return err
}

// SetAGC sets the AGC speed.
//
// Switching the AGC OFF is a one-way trip unless the off-then-on parameter is
// sent first. On a TS-590S with the AGC off, GC1 and GC2 are both refused and
// the radio stays off — so a client that switched it off could never switch it
// back, and would be told only "command rejected". Where the model documents
// that parameter, a request for a speed while the AGC is off sends it first:
// the radio comes back on at whatever speed it had, and the requested speed
// follows immediately.
//
// The rigs also refuse this in FM — "this command cannot be performed in FM
// mode (an error sounds)" — and a TS-480 goes further and answers three spaces
// to a read. Both arrive as an ordinary refusal, which is the truthful answer:
// the AGC is not adjustable in the mode the radio is in.
func (k *Rig) SetAGC(ctx context.Context, c backend.Conn, v radio.AGC) error {
	if len(k.profile.AGC) == 0 {
		return fmt.Errorf("kenwood: the %s has no AGC command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	code, ok := k.profile.AGC[v]
	if !ok {
		return fmt.Errorf("kenwood: the %s has no AGC setting %q, only %v: %w",
			k.profile.Label, v, agcSettings(k.profile.AGC), backend.ErrUnsupported)
	}

	if v != radio.AGCOff && k.profile.AGCOnCode != "" && k.AGC() == radio.AGCOff {
		// Off to on first. Its own answer is not read: the speed that follows
		// is what the caller asked for, and reading the intermediate state
		// would report a value nobody requested.
		if err := send(ctx, c, fmt.Sprintf("%s%s%s;",
			k.profile.AGCCmd, k.band(), k.profile.AGCOnCode)); err != nil {
			return err
		}
	}

	if err := send(ctx, c, fmt.Sprintf("%s%s%s;", k.profile.AGCCmd, k.band(), code)); err != nil {
		return err
	}
	_, err := do(ctx, c, k.profile.AGCCmd+";", k.agcKey())
	return err
}

// AGC is the last reading, which SetAGC consults to decide whether the radio
// has to be brought out of off before a speed will be accepted.
func (k *Rig) AGC() radio.AGC {
	v, _ := k.agc.Load().(radio.AGC)
	return v
}

// decodePA reads the preamplifier answer.
//
// The answer is two digits where the set takes one: "P2: 0: Always 0". Only the
// first is the setting — on a TS-990S the first is the band selector and the
// second the value, which is why the band offset is applied before indexing.
func (k *Rig) decodePA(u *backend.Update, arg []byte) {
	if v, ok := k.frontEndDigit(arg); ok && v <= k.profile.Preamp {
		u.Patch.Preamp = &v
	}
}

// decodeRA reads the attenuator answer, which is twice the width of the set on
// the older pair: RA0000 for off, RA0100 for on.
func (k *Rig) decodeRA(u *backend.Update, arg []byte) {
	off := 0
	if k.profile.Banded {
		off = 1
	}
	w := k.profile.AttenuatorWidth
	if len(arg) < off+w {
		return
	}
	idx, err := strconv.Atoi(string(arg[off : off+w]))
	if err != nil {
		return
	}
	if db, ok := k.attenuatorDB(idx); ok {
		u.Patch.AttenuatorDB = &db
	}
}

// decodeRG reads the RF gain against this model's own ceiling.
func (k *Rig) decodeRG(u *backend.Update, arg []byte) {
	off := 0
	if k.profile.Banded {
		off = 1
	}
	if len(arg) < off+3 || k.profile.RFGainMax == 0 {
		return
	}
	n, err := strconv.Atoi(string(arg[off : off+3]))
	if err != nil {
		return
	}
	pct := float64(n) / float64(k.profile.RFGainMax) * 100
	if pct > 100 {
		pct = 100
	}
	u.Patch.RFGain = &pct
}

// decodeAGC reads the AGC speed off GC or GT.
//
// A TS-480 in FM answers "   " — three spaces, which its reference spells out —
// so an unparseable answer is a mode that has no AGC to report rather than a
// frame to complain about. The key is still set by the caller, so the read
// completes either way.
func (k *Rig) decodeAGC(u *backend.Update, arg []byte) {
	off := 0
	if k.profile.Banded {
		off = 1
	}
	if len(arg) <= off {
		return
	}
	if v, ok := agcValue(k.profile.AGC, string(arg[off:])); ok {
		u.Patch.AGC = &v
		k.agc.Store(v)
	}
}

// frontEndDigit pulls the single significant digit out of a PA answer, skipping
// the TS-990S's leading band selector.
func (k *Rig) frontEndDigit(arg []byte) (int, bool) {
	off := 0
	if k.profile.Banded {
		off = 1
	}
	if len(arg) <= off || arg[off] < '0' || arg[off] > '9' {
		return 0, false
	}
	return int(arg[off] - '0'), true
}
