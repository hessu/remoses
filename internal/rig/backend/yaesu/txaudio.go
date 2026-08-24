package yaesu

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hessu/remoses/internal/rig/backend"
)

// The transmit audio chain: MG the gain into the modulator, PR the speech
// processor's switch and PL its level.
//
// All three are read with the bare command and set with the value straight
// behind the letters. None of them takes the receiver selector the front end
// uses — see the Model comment on MicGain for why that is a fact about the
// radios rather than a convenience, and ProcShape for the one command whose
// parameters genuinely differ from model to model.
//
// WHAT IS NOT HERE. The parametric microphone equalizer, which shares PR's
// first parameter on eight of these radios and PR's third value on two more,
// is neither read nor written. It is three bands of frequency, level and
// bandwidth apiece, it lives in EX on every model that has it, and radio.State
// has one Proc field to put an answer in. remoses addresses the compressor and
// leaves the equalizer where the operator left it — which is also why a PR
// answer naming the equalizer is discarded rather than folded into Proc.
//
// Nor is the AMC output level (AO on the FT-710, FTdx101 and FTX-1, absent
// everywhere else), nor the monitor level (ML, which every model has), nor the
// per-mode choice of microphone against the USB codec or the rear DATA jack.
// That last one is the one an operator running digital modes would most want,
// and it is EX on all twelve — no model in this registry has a two-letter
// command for it, and it is per mode on every one of them. So what keeps it out
// is DESIGN.md's "the operator's radio is not ours to reconfigure", the same
// rule that keeps KM out, rather than a gap in what has been read.
//
// Worth knowing while reading State.TXAudioGain's doc comment, which warns that
// the gain belongs to whatever input the radio is taking audio from: on the newer
// radios it is worse than that. The FT-710 carries separate USB MOD GAIN and
// REAR MOD GAIN menu items per mode, and the FT-891 eight per-mode gains, so MG
// is not one setting applied to a routed input — it is one of several the radio
// holds at once, and no manual read here says which one MG reaches. Which is the
// reason the names differ across the seam: Yaesu prints MIC GAIN and this
// backend keeps that word, and the API publishes the setting as tx_audio_gain,
// the same way preamp publishes IPO and AMP under one accurate name.
const (
	reqMG = "MG;"
	reqPL = "PL;"
	// PR is read two different ways. The single-parameter models take a bare
	// PR;, and the two-parameter models take PR0; — the 0 naming the speech
	// processor rather than the parametric equalizer, exactly as it does in a
	// set. Sending the wrong one costs a per-command timeout, so the profile
	// picks it; see procRead.
	reqPRSingle = "PR;"
	reqPRSelect = "PR0;"
	// procSpeech is PR's first parameter on the two-parameter models: "0:
	// Speech Processor". 1 is the parametric microphone equalizer, which this
	// backend does not drive.
	procSpeech = '0'
)

// txAudioReads lists the transmit audio queries this model answers.
func (y *Rig) txAudioReads() []read {
	var reads []read
	if y.profile.MicGain {
		reads = append(reads, read{reqMG, keyMG})
	}
	if y.profile.Proc != ProcNone {
		reads = append(reads, read{y.procRead(), keyPR})
	}
	if y.profile.ProcLevel {
		reads = append(reads, read{reqPL, keyPL})
	}
	return reads
}

// procRead is the PR query this model answers, which is not the same on both
// shapes: the two-parameter models want the parameter that says which of the
// processor and the equalizer is being asked about.
func (y *Rig) procRead() string {
	if y.profile.Proc == ProcSelect {
		return reqPRSelect
	}
	return reqPRSingle
}

// SetTXAudioGain sets the gain into the modulator, 0-100% of this radio's own
// count.
//
// The count is 000-100 on eight models and 000-255 on three, and MicGainMax is
// the only thing that can tell them apart — the frames are identical. See
// Model.MicGainMax.
func (y *Rig) SetTXAudioGain(ctx context.Context, c backend.Conn, pct float64) error {
	if !y.profile.MicGain {
		return fmt.Errorf("yaesu: the %s has no transmit gain command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("yaesu: transmit gain %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	n := scaleTo(pct, 0, y.profile.MicGainMax)
	if err := send(ctx, c, fmt.Sprintf("MG%03d;", n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqMG, keyMG)
	return err
}

// SetProc switches the speech processor.
//
// Which two characters do that is per model, and on the two-parameter radios
// they are preceded by the 0 that picks the compressor over the parametric
// equalizer. Refused on the FTdx9000, whose PR entry contradicts itself about
// its own letters — see ProcNone.
func (y *Rig) SetProc(ctx context.Context, c backend.Conn, on bool) error {
	if y.profile.Proc == ProcNone {
		return fmt.Errorf("yaesu: the %s has no speech processor switch remoses can send; "+
			"its manual's PR entry spells the command PC in every row, and this backend does "+
			"not guess a command shape: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	v := y.profile.ProcOff
	if on {
		v = y.profile.ProcOn
	}
	cmd := fmt.Sprintf("PR%c;", v)
	if y.profile.Proc == ProcSelect {
		cmd = fmt.Sprintf("PR%c%c;", procSpeech, v)
	}
	if err := send(ctx, c, cmd); err != nil {
		return err
	}
	_, err := do(ctx, c, y.procRead(), keyPR)
	return err
}

// SetProcLevel sets how hard the processor compresses, 0-100% of its own range.
//
// The range is 000-100 on nine models and 000-255 on two, and it starts at 001
// rather than 000 on the FT-710 and FTX-1, where 000 is a documented "OFF"
// rather than the bottom of the scale. See Model.ProcLevelMin.
func (y *Rig) SetProcLevel(ctx context.Context, c backend.Conn, pct float64) error {
	if !y.profile.ProcLevel {
		return fmt.Errorf("yaesu: the %s has no speech processor level command: %w",
			y.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("yaesu: speech processor level %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	n := scaleTo(pct, y.profile.ProcLevelMin, y.profile.ProcLevelMax)
	if err := send(ctx, c, fmt.Sprintf("PL%03d;", n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqPL, keyPL)
	return err
}

// decodeMG reads the transmit gain answer: MG<nnn>, and nothing in front of it.
//
// The length is checked exactly rather than as a minimum, and that is the whole
// point of this function. Every neighbouring level — AG, RG, SQ — answers with
// a receiver selector first, so a four-character argument would be one of those
// shapes arriving under MG's letters, which can only mean this backend or the
// radio is not what the other thinks. Reading digits 1 to 3 of MG0128 gives 12
// on a 255 scale, published as 4.7% for a radio sitting at half gain: a
// confident wrong number, which is the failure DESIGN.md §5.4 puts above every
// other consideration here. Nothing published beats that.
func (y *Rig) decodeMG(u *backend.Update, arg []byte) {
	if len(arg) != 3 {
		return
	}
	n, err := strconv.Atoi(string(arg))
	if err != nil {
		return
	}
	pct := scaleFrom(n, 0, y.profile.MicGainMax)
	u.Patch.TXAudioGain = &pct
}

// decodePR reads the speech processor answer, whose parameters differ by model.
//
// Two things are deliberately dropped rather than published. A two-parameter
// answer naming the parametric microphone equalizer — P1 of 1 — says nothing
// about the compressor, and this decoder is on the AI push path, so those
// frames really do arrive whenever somebody touches the equalizer on the front
// panel. And a state character the model's own pair does not contain is left
// alone, because the FT-891's 1 is "ON" where seven other radios' 1 is "OFF"
// and there is no reading of a stray value that is safe on both.
//
// The single-parameter models' third value IS published, as off. "2:
// Parametric Microphone Equalizer "ON"" is that one parameter's way of saying
// the equalizer has the audio and the compressor does not, so false is what is
// true of the speech processor. Publishing nothing there would leave a stale
// true standing while the compressor was out of circuit.
func (y *Rig) decodePR(u *backend.Update, arg []byte) {
	state := arg
	if y.profile.Proc == ProcSelect {
		if len(arg) != 2 || arg[0] != procSpeech {
			return
		}
		state = arg[1:]
	}
	if len(state) != 1 {
		return
	}
	switch {
	case state[0] == y.profile.ProcOn:
		on := true
		u.Patch.Proc = &on
	case state[0] == y.profile.ProcOff:
		on := false
		u.Patch.Proc = &on
	case y.profile.Proc == ProcSingle && state[0] == '2':
		on := false
		u.Patch.Proc = &on
	}
}

// decodePL reads the processor level answer: PL<nnn>, no selector, same exact
// length check and the same reason as decodeMG.
//
// A 000 from an FT-710 or FTX-1 means "OFF" rather than the bottom of the
// scale, and it still reads back as 0% here: scaleFrom clamps below
// ProcLevelMin, so the two answers those radios can give for no compression
// both publish the same number. The switch is PR's to report.
func (y *Rig) decodePL(u *backend.Update, arg []byte) {
	if len(arg) != 3 {
		return
	}
	n, err := strconv.Atoi(string(arg))
	if err != nil {
		return
	}
	pct := scaleFrom(n, y.profile.ProcLevelMin, y.profile.ProcLevelMax)
	u.Patch.ProcLevel = &pct
}
