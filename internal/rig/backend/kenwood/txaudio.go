package kenwood

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hessu/remoses/internal/rig/backend"
)

// The transmit audio chain: MG the gain into the modulator, the speech
// processor's switch, and PL the processor's level.
//
// Every row below is transcribed from the radio's own PC Control Command
// Reference Guide — the TS-590S/TS-590SG guide of January/30/2019 (cross-checked
// against the earlier revision of the same document, which agrees on all three),
// the TS-480's, the TS-890S guide revision 1 and the TS-990S guide revision 2.
// Sentences in quotation marks are the manuals' words. Everything else is this
// backend's reasoning and is written as such.
//
// WHAT THE FOUR REFERENCES ACTUALLY SAY:
//
//	MG   "Sets and reads the microphone gain."      MGnnn;   read MG;
//	     TS-480, TS-590S/SG, TS-890S:  000 ~ 100
//	     TS-990S:                      000 ~ 255 (in steps of 1)
//
//	PR   "Sets and reads the Speech Processor function ON/OFF."   PRn;  read PR;
//	     TS-480, TS-590S/SG only.  0: OFF, 1: ON
//	PR0  "Speech Processor ON/OFF"                               PR0n; read PR0;
//	     TS-890S, TS-990S.        0: OFF, 1: ON
//
//	PL   "Sets and reads the Speech Processor input/output level."
//	     PLnnnmmm;   read PL;    TWO fields, three digits each
//	     P1 (Input level)  and  P2 (Output level)
//	     TS-480, TS-590S/SG, TS-890S:  each 000 (minimum) ~ 100 (maximum)
//	     TS-990S:                      each 000 (minimum) ~ 255 (maximum)
//
// THE SCALE IS PER MODEL, AND THIS IS THE RG TRAP ON A SECOND CONTROL. RG counts
// 000-100 on a TS-480 and 000-255 on everything after it; MG and PL do the same
// thing at a different point in the family, stopping at 100 everywhere except
// the TS-990S. A percentage written against the wrong ceiling puts about two
// fifths of the audio the operator asked for into the modulator and reports it
// as the figure they typed. Model.MicGainMax and Model.ProcLevelMax carry the
// two ceilings so that neither is ever assumed here.
//
// THE PROCESSOR'S SWITCH IS SPELT TWO WAYS, AND THE COLLISION IS EXACTLY THE
// KIND THAT DOES NOT FAIL LOUDLY. On a TS-590, PR1; switches the speech
// processor on. On a TS-890S or TS-990S the switch is PR0, and PR1 is a
// different command — the processor's effect type, "0: Soft, 1: Hard" — so the
// same four bytes are a well-formed read of an unrelated setting there. The
// radio answers, nothing is rejected, and the processor stays off. Model.ProcCmd
// holds the command per model and Model.procSwitchChar reads the answer back,
// checking the command's own digit rather than skipping over it.
//
// PL CARRIES TWO VALUES AND THE API PUBLISHES ONE, SO THE MAPPING IS A DECISION
// RATHER THAN A LOOKUP. It is recorded here because it is not recoverable from
// the wire:
//
//   - proc_level IS PL's P1, THE INPUT LEVEL. The input level is what decides how
//     hard the processor works — how much compression the audio gets — which is
//     what "the processor's level" means to an operator, and it is the field that
//     does nothing at all while the processor is switched off. The output level
//     is a make-up gain feeding the modulator, which is the job tx_audio_gain
//     already has; mapping proc_level onto it would give remoses two names for very
//     nearly the same control and leave the compression amount unreachable.
//
//   - THE OUTPUT LEVEL IS NOT DISCARDED, IT IS PRESERVED. A PL set carries both
//     fields, so writing an input level means restating an output level, and the
//     only honest one to restate is the one the radio is already on. SetProcLevel
//     therefore reads PL before it writes, and refuses rather than invent a
//     number if the radio has not reported one. The output level is not published
//     — radio.State has no home for it — and it is never changed by remoses.
//
// PL IS *NOT* REFUSED WHILE THE PROCESSOR IS OFF, AND THAT IS A TRANSCRIBED
// NEGATIVE RATHER THAN AN UNCHECKED ASSUMPTION. The two neighbouring level
// commands say so outright — NL is "When NB is set to OFF, an error occurs" and
// RL is "When the Noise Reduction setting is OFF, an error occurs" — which is
// why noiseReads skips both while their circuit is off. PL's row carries no such
// note in any of the four references, and neither does MG. So both are polled
// unconditionally, which is also what radio.State asks for: it says ProcLevel is
// "reported whether or not the processor is on, where the radio will answer",
// because a client redrawing the control wants the value it will return to.
//
// MG IS THE SSB AND AM GAIN, NOT THE ONE IN CIRCUIT IN FM. The TS-590S/SG
// reference is explicit: "Sets and reads the microphone gain for SSB and AM
// mode", and "Configure the FM mode microphone gain using the menu" — which is
// menu 047 on a TS-590S and 053 on a TS-590SG, a three-position setting reachable
// only through EX. The TS-890S carries the same menu redirection for FM. Nothing
// in any reference says MG errors in FM, so it is neither fenced nor skipped
// there; what a client sees in FM is a real stored setting that is simply not the
// one modulating the radio at that moment. Reporting it consistently beats
// leaving a hole, but it is worth knowing which number it is.
//
// ONE NEIGHBOURING HAZARD, because whoever revisits these will have the same page
// of the same manual open. VX on the TS-480 and TS-590 is VOX, EXCEPT in CW,
// where the very same command reads and writes break-in — and break-in is what
// decides whether CW sent over CAT reaches the air at all. breakin.go fences that
// off in both directions. VOX is transmit audio in the operator's mental model
// and it is NOT this: reaching for VX from here would put a number about VOX into
// the field the CW guard trusts.

// The read forms. The processor's switch has no constant of its own because its
// command is per model; see Model.procReq.
const (
	reqMG = "MG;"
	reqPL = "PL;"
)

// txAudioReads lists the transmit-audio queries this model answers.
//
// Unconditional, unlike noiseReads, and for a documented reason: neither MG nor
// PL is refused in any state the references name. See the file comment.
func (k *Rig) txAudioReads() []read {
	var reads []read
	if k.profile.MicGainMax > 0 {
		reads = append(reads, read{reqMG, keyMG})
	}
	if k.profile.ProcCmd != "" {
		reads = append(reads, read{k.profile.procReq(), keyPR})
	}
	if k.profile.ProcLevelMax > 0 {
		reads = append(reads, read{reqPL, keyPL})
	}
	return reads
}

// procOutputLevel is PL's second field as last reported, and whether one has
// ever been. Kept for the same reason the AGC and blanker readings are: it
// changes what the next command has to say.
func (k *Rig) procOutputLevel() (int, bool) {
	v, ok := k.procOut.Load().(int)
	return v, ok
}

// SetTXAudioGain sets the transmit audio gain, 0-100% of this model's MG range.
//
// Kenwood calls the command microphone gain and it keeps that name inside this
// backend — MG, Model.MicGainMax — while the API publishes it as tx_audio_gain,
// because what it moves is the gain into the modulator whatever input the radio
// is taking audio from, not something about the microphone socket.
func (k *Rig) SetTXAudioGain(ctx context.Context, c backend.Conn, pct float64) error {
	if k.profile.MicGainMax == 0 {
		return fmt.Errorf("kenwood: the %s has no transmit gain command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("kenwood: transmit gain %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	n := scaleTo(pct, 0, k.profile.MicGainMax)
	if err := send(ctx, c, fmt.Sprintf("MG%03d;", n)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqMG, keyMG)
	return err
}

// SetProc switches the speech processor.
//
// The command is PR or PR0 depending on the model, and getting that wrong is not
// a rejection but a well-formed request to a different setting; see the file
// comment and Model.ProcCmd.
func (k *Rig) SetProc(ctx context.Context, c backend.Conn, on bool) error {
	if k.profile.ProcCmd == "" {
		return fmt.Errorf("kenwood: the %s has no speech processor command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if err := send(ctx, c, k.profile.procSet(on)); err != nil {
		return err
	}
	_, err := do(ctx, c, k.profile.procReq(), keyPR)
	return err
}

// SetProcLevel sets the speech processor's INPUT level, 0-100% of this model's
// PL range, leaving the output level where the operator put it.
//
// The read that comes first is not a check, it is the source of a value this
// command cannot do without: PL's set form carries the input level and the
// output level together, so there is no way to write one without restating the
// other. Sending a made-up output level would move a control nobody mentioned,
// and it would do it silently, since remoses does not publish that field. See
// the file comment for why proc_level is the input level of the two.
func (k *Rig) SetProcLevel(ctx context.Context, c backend.Conn, pct float64) error {
	if k.profile.ProcLevelMax == 0 {
		return fmt.Errorf("kenwood: the %s has no speech processor level command: %w",
			k.profile.Label, backend.ErrUnsupported)
	}
	if pct < 0 || pct > 100 {
		return fmt.Errorf("kenwood: speech processor level %.1f%% is outside 0-100%%: %w",
			pct, backend.ErrUnsupported)
	}
	if _, err := do(ctx, c, reqPL, keyPL); err != nil {
		return err
	}
	out, ok := k.procOutputLevel()
	if !ok {
		return fmt.Errorf("kenwood: the %s answered PL; with nothing this backend could "+
			"read an output level out of, and PL carries the input and output levels in "+
			"one frame: writing the input level alone would mean inventing the field "+
			"beside it", k.profile.Label)
	}
	n := scaleTo(pct, 0, k.profile.ProcLevelMax)
	if err := send(ctx, c, fmt.Sprintf("PL%03d%03d;", n, out)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqPL, keyPL)
	return err
}

// decodeMG reads the transmit gain against this model's own ceiling.
func (k *Rig) decodeMG(u *backend.Update, arg []byte) {
	if len(arg) != 3 {
		return
	}
	n, err := strconv.Atoi(string(arg))
	if err != nil {
		return
	}
	pct := scaleFrom(n, 0, k.profile.MicGainMax)
	u.Patch.TXAudioGain = &pct
}

// decodePR reads the processor's switch, past whatever the command's own digit
// happens to be on this model.
func (k *Rig) decodePR(u *backend.Update, arg []byte) {
	ch, ok := k.profile.procSwitchChar(arg)
	if !ok || (ch != '0' && ch != '1') {
		return
	}
	on := ch == '1'
	u.Patch.Proc = &on
}

// decodePL reads both of PL's fields: the input level, which is published as
// proc_level, and the output level, which is kept so that the next set can
// restate it unchanged.
//
// The length is checked exactly rather than loosely. A short answer here would
// otherwise take three digits of the input level from wherever they happened to
// fall, and the output level that came out of the remainder would be written
// back to the radio on the next set.
func (k *Rig) decodePL(u *backend.Update, arg []byte) {
	if len(arg) != 6 {
		return
	}
	in, err := strconv.Atoi(string(arg[:3]))
	if err != nil {
		return
	}
	out, err := strconv.Atoi(string(arg[3:]))
	if err != nil {
		return
	}
	k.procOut.Store(out)
	pct := scaleFrom(in, 0, k.profile.ProcLevelMax)
	u.Patch.ProcLevel = &pct
}
