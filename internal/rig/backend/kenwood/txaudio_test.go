package kenwood

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The transmit audio chain, tested against the transcription in txaudio.go
// rather than against a guess. Three things here are model-specific and each has
// its own test, because each of the three fails silently on the wire:
//
//   - MG and PL count to 100 on the TS-480, TS-590S/SG and TS-890S and to 255 on
//     the TS-990S, so the same percentage is a different frame per model.
//   - The processor's switch is PR on the older pair and PR0 on the newer, where
//     PR1 means something else entirely.
//   - PL carries an input level and an output level in ONE frame, so writing the
//     level remoses publishes means restating the one it does not.

func TestSetTXAudioGainScalesAgainstTheModelsOwnCeiling(t *testing.T) {
	for _, tt := range []struct {
		model string
		pct   float64
		want  string
	}{
		// "000 ~ 100" in the TS-480, TS-590S/SG and TS-890S references.
		{"ts480", 0, "MG000;"},
		{"ts480", 50, "MG050;"},
		{"ts480", 100, "MG100;"},
		{"ts590s", 50, "MG050;"},
		{"ts590sg", 100, "MG100;"},
		{"ts890s", 50, "MG050;"},
		// "000 ~ 255 (in steps of 1)" in the TS-990S reference, and this is the
		// whole reason MicGainMax is per model: half scale is 128 here and 050
		// everywhere else. A TS-990S sent MG050; would be at a fifth of the gain
		// its operator asked for, and would report 50%.
		{"ts990s", 0, "MG000;"},
		{"ts990s", 50, "MG128;"},
		{"ts990s", 100, "MG255;"},
	} {
		t.Run(tt.model+"/"+tt.want, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			c := newTestConn(t, k, answersFor(k.profile))
			if err := k.SetTXAudioGain(context.Background(), c, tt.pct); err != nil {
				t.Fatalf("SetTXAudioGain: %v", err)
			}
			// The read-back is not ceremony on this protocol: a set draws no
			// answer at all unless AI happens to be on, and the rig clamps.
			c.wantSent(t, tt.want, reqMG)
		})
	}
}

// TestSetProcSpellsTheSwitchPerGeneration is the test for the collision the file
// comment calls out. PR1; switches a TS-590's processor on; on a TS-890S the
// very same four bytes are the read form of PR1, the processor's EFFECT TYPE.
// The radio answers, nothing is rejected, and the processor stays off — so this
// has to be checked as a wire fact rather than trusted.
func TestSetProcSpellsTheSwitchPerGeneration(t *testing.T) {
	for _, tt := range []struct {
		model string
		on    bool
		set   string
		read  string
	}{
		{"ts480", true, "PR1;", "PR;"},
		{"ts480", false, "PR0;", "PR;"},
		{"ts590s", true, "PR1;", "PR;"},
		{"ts590sg", false, "PR0;", "PR;"},
		{"ts890s", true, "PR01;", "PR0;"},
		{"ts890s", false, "PR00;", "PR0;"},
		{"ts990s", true, "PR01;", "PR0;"},
		{"ts990s", false, "PR00;", "PR0;"},
	} {
		t.Run(tt.model+"/"+tt.set, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			c := newTestConn(t, k, answersFor(k.profile))
			if err := k.SetProc(context.Background(), c, tt.on); err != nil {
				t.Fatalf("SetProc: %v", err)
			}
			c.wantSent(t, tt.set, tt.read)
		})
	}
}

// TestSetProcLevelRestatesTheRadiosOwnOutputLevel is the point of the PL
// decision, checked from the wire.
//
// PL's set form carries P1 (input level) and P2 (output level) together, and
// remoses publishes only the input one. So the sequence has to be read, then
// write with the output level the radio just reported — not with a zero, not
// with the value that was there when the daemon started, and not with anything
// derived from the percentage being set.
func TestSetProcLevelRestatesTheRadiosOwnOutputLevel(t *testing.T) {
	for _, tt := range []struct {
		model  string
		answer string
		pct    float64
		want   string
	}{
		// Output level 075 arrives in the read and goes straight back out.
		{"ts480", "PL025075", 50, "PL050075;"},
		{"ts590s", "PL025075", 50, "PL050075;"},
		{"ts590sg", "PL100000", 0, "PL000000;"},
		{"ts890s", "PL025075", 100, "PL100075;"},
		// And on the TS-990S both fields run to 255, so half scale is 128 in the
		// field being written while the field being preserved is copied
		// verbatim, whatever scale it is on.
		{"ts990s", "PL010200", 50, "PL128200;"},
	} {
		t.Run(tt.model+"/"+tt.want, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			answers := answersFor(k.profile)
			answers[reqPL] = tt.answer
			c := newTestConn(t, k, answers)
			if err := k.SetProcLevel(context.Background(), c, tt.pct); err != nil {
				t.Fatalf("SetProcLevel: %v", err)
			}
			c.wantSent(t, reqPL, tt.want, reqPL)
		})
	}
}

// TestSetProcLevelRefusesRatherThanInventAnOutputLevel covers the case the read
// above exists for. If nothing readable comes back, the alternative to refusing
// is to pick an output level, and picking one would move a control the caller
// never mentioned and that remoses does not publish — so the operator would have
// no way to see it had happened.
func TestSetProcLevelRefusesRatherThanInventAnOutputLevel(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	answers := answersFor(k.profile)
	answers[reqPL] = "PL050" // three digits where the frame carries six
	c := newTestConn(t, k, answers)

	err := k.SetProcLevel(context.Background(), c, 50)
	if err == nil {
		t.Fatal("SetProcLevel wrote PL after an answer it could not read an output level out of")
	}
	if !strings.Contains(err.Error(), "output level") {
		t.Errorf("error does not say what was missing: %v", err)
	}
	// The read went out; nothing was written after it.
	c.wantSent(t, reqPL)
}

func TestTXAudioDecodesPerModel(t *testing.T) {
	for _, tt := range []struct {
		name  string
		model string
		frame string
		check func(t *testing.T, p radio.Patch)
	}{{
		"mic gain on a 100-count radio", "ts590sg", "MG050",
		func(t *testing.T, p radio.Patch) { wantPct(t, "tx_audio_gain", p.TXAudioGain, 50) },
	}, {
		// The same three digits, and a different percentage, because the
		// TS-990S counts to 255. Decoding it against 100 would report 50% as
		// full scale and then clamp everything above it.
		"mic gain on a 255-count radio", "ts990s", "MG128",
		func(t *testing.T, p radio.Patch) { wantPct(t, "tx_audio_gain", p.TXAudioGain, 50.196078) },
	}, {
		"the processor switch on PR", "ts590sg", "PR1",
		func(t *testing.T, p radio.Patch) { wantBool(t, "proc", p.Proc, true) },
	}, {
		"the processor switch on PR0", "ts890s", "PR01",
		func(t *testing.T, p radio.Patch) { wantBool(t, "proc", p.Proc, true) },
	}, {
		"the processor switch off on PR0", "ts990s", "PR00",
		func(t *testing.T, p radio.Patch) { wantBool(t, "proc", p.Proc, false) },
	}, {
		// PR1 on this generation is the effect type, "0: Soft, 1: Hard". A push
		// announcing Hard must not be read as the processor having come on.
		"the effect type is not the switch", "ts890s", "PR11",
		func(t *testing.T, p radio.Patch) {
			if p.Proc != nil {
				t.Errorf("PR11 published proc=%v; on this radio PR1 is the processor's "+
					"effect type, not its switch", *p.Proc)
			}
		},
	}, {
		// proc_level is PL's FIRST field. Publishing the second would report
		// the make-up gain as the compression level.
		"proc level is the input level", "ts590sg", "PL025075",
		func(t *testing.T, p radio.Patch) { wantPct(t, "proc_level", p.ProcLevel, 25) },
	}, {
		"proc level on a 255-count radio", "ts990s", "PL128200",
		func(t *testing.T, p radio.Patch) { wantPct(t, "proc_level", p.ProcLevel, 50.196078) },
	}, {
		// Six digits or nothing: a short frame decoded loosely would take the
		// input level from wherever the digits fell and, worse, leave a bogus
		// output level to be written back on the next set.
		"a short PL publishes nothing", "ts590sg", "PL050",
		func(t *testing.T, p radio.Patch) {
			if p.ProcLevel != nil {
				t.Errorf("a three-digit PL published proc_level=%v", *p.ProcLevel)
			}
		},
	}} {
		t.Run(tt.name, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			u, err := k.Decode([]byte(tt.frame))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			// Whatever the argument turned out to be, the frame still has to
			// complete the request that asked for it, or the poll that sent it
			// would sit out the session's timeout.
			if u.Key != backend.Key(tt.frame[:2]) {
				t.Errorf("key = %q, want %q so the read is still answered", u.Key, tt.frame[:2])
			}
			tt.check(t, u.Patch)
		})
	}
}

func wantPct(t *testing.T, what string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s was not published", what)
	}
	if diff := *got - want; diff > 0.001 || diff < -0.001 {
		t.Errorf("%s = %v, want %v", what, *got, want)
	}
}

func wantBool(t *testing.T, what string, got *bool, want bool) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s was not published", what)
	}
	if *got != want {
		t.Errorf("%s = %v, want %v", what, *got, want)
	}
}

// TestTXAudioCapsFollowTheModel checks the promise side. Every model in the
// registry has a transcribed row for all three, so all three are true — but the
// capability is computed from the profile rather than written out, and this is
// what says so.
func TestTXAudioCapsFollowTheModel(t *testing.T) {
	for _, name := range ModelNames() {
		t.Run(name, func(t *testing.T) {
			k := newModelRig(t, name)
			caps := k.Caps()
			if got, want := caps.TXAudioGainControl, k.profile.MicGainMax > 0; got != want {
				t.Errorf("TXAudioGainControl = %v, want %v", got, want)
			}
			if got, want := caps.ProcControl, k.profile.ProcCmd != ""; got != want {
				t.Errorf("ProcControl = %v, want %v", got, want)
			}
			if got, want := caps.ProcLevelControl, k.profile.ProcLevelMax > 0; got != want {
				t.Errorf("ProcLevelControl = %v, want %v", got, want)
			}
			// And the ceilings are one of the two the references print, never
			// something in between. A third number here would mean somebody had
			// scaled a percentage against a figure nobody read.
			switch k.profile.MicGainMax {
			case 100, 255:
			default:
				t.Errorf("MicGainMax = %d; the references give 000-100 or 000-255 and "+
					"nothing else", k.profile.MicGainMax)
			}
			switch k.profile.ProcLevelMax {
			case 100, 255:
			default:
				t.Errorf("ProcLevelMax = %d; same", k.profile.ProcLevelMax)
			}
			switch k.profile.ProcCmd {
			case "PR", "PR0":
			default:
				t.Errorf("ProcCmd = %q, want PR or PR0", k.profile.ProcCmd)
			}
		})
	}
}

// TestTXAudioIsPolledExactlyWhereItIsClaimed is the invariant that survives from
// the refusing version of this file, turned around: a capability that is FALSE
// must never put its command on the wire, and one that is true must.
//
// No shipped model has any of the three false, so the false half is exercised
// against a profile with the fields blanked. That is the case that matters: the
// gate is what stops a future model with no PL row from being sent one, and a
// test that only ever sees the true side would not notice the gate going away.
func TestTXAudioIsPolledExactlyWhereItIsClaimed(t *testing.T) {
	run := func(t *testing.T, k *Rig) []string {
		t.Helper()
		answers := answersFor(k.profile)
		if _, ok := answers[reqID]; !ok {
			// generic claims no ID of its own, but Init still asks and a radio
			// still answers something.
			answers[reqID] = "ID000"
		}
		answers[reqRM] = "RM10000" // in case the sample IF answer has it keyed
		c := newTestConn(t, k, answers)
		ctx := context.Background()
		if err := k.Init(ctx, c); err != nil {
			t.Fatalf("Init: %v", err)
		}
		if err := k.Poll(ctx, c, backend.PollFast); err != nil {
			t.Fatalf("fast poll: %v", err)
		}
		if err := k.Poll(ctx, c, backend.PollSlow); err != nil {
			t.Fatalf("slow poll: %v", err)
		}
		return c.sent
	}
	sentAny := func(sent []string, prefixes ...string) bool {
		for _, req := range sent {
			for _, p := range prefixes {
				if strings.HasPrefix(strings.ToUpper(req), p) {
					return true
				}
			}
		}
		return false
	}

	for _, name := range ModelNames() {
		t.Run(name, func(t *testing.T) {
			k := newModelRig(t, name)
			sent := run(t, k)
			if !sentAny(sent, "MG") {
				t.Errorf("MG; never went out on the %s, whose reference has a "+
					"microphone gain row", k.profile.Label)
			}
			if !sentAny(sent, k.profile.ProcCmd) {
				t.Errorf("%s; never went out on the %s", k.profile.ProcCmd, k.profile.Label)
			}
			if !sentAny(sent, "PL") {
				t.Errorf("PL; never went out on the %s", k.profile.Label)
			}
		})
	}

	t.Run("a model with no rows is never asked", func(t *testing.T) {
		k := newModelRig(t, "ts590sg")
		k.profile.MicGainMax = 0
		k.profile.ProcCmd = ""
		k.profile.ProcLevelMax = 0

		caps := k.Caps()
		if caps.TXAudioGainControl || caps.ProcControl || caps.ProcLevelControl {
			t.Fatalf("Caps advertises transmit audio control (tx_audio_gain=%v proc=%v "+
				"proc_level=%v) on a profile with no rows for any of it",
				caps.TXAudioGainControl, caps.ProcControl, caps.ProcLevelControl)
		}

		sent := run(t, k)
		// So the check below cannot pass by inspecting nothing: a connect and
		// two poll tiers are a conversation, and if this backend ever stops
		// having one the assertion under it means nothing.
		if len(sent) < 3 {
			t.Fatalf("only %d requests went out across Init and both poll tiers (%q)",
				len(sent), sent)
		}
		if sentAny(sent, "MG", "PR", "PL") {
			t.Errorf("sent %q: nothing is claimed for this profile, so no transmit "+
				"audio command may go on the wire", sent)
		}

		ctx := context.Background()
		c := newTestConn(t, k, answersFor(k.profile))
		for _, tt := range []struct {
			what string
			err  error
		}{
			{"SetTXAudioGain", k.SetTXAudioGain(ctx, c, 50)},
			{"SetProc", k.SetProc(ctx, c, true)},
			{"SetProcLevel", k.SetProcLevel(ctx, c, 50)},
		} {
			if tt.err == nil {
				t.Fatalf("%s accepted a request for a control the profile does not claim", tt.what)
			}
			// ErrUnsupported is what turns this into a 422 carrying the error's
			// own text. A bare error would reach the operator as a 500, which
			// reads as a daemon fault rather than as an answer.
			if !errors.Is(tt.err, backend.ErrUnsupported) {
				t.Errorf("%s error = %v, want backend.ErrUnsupported", tt.what, tt.err)
			}
			if !strings.Contains(tt.err.Error(), k.profile.Label) {
				t.Errorf("%s refusal does not name the radio it is about: %s", tt.what, tt.err)
			}
		}
		c.wantSent(t) // and not one byte went to the radio
	})
}

// TestTXAudioRefusesAPercentageOutsideRange keeps the boundary honest: 0-100 is
// the API's unit at this seam, and a figure outside it would be clamped onto the
// radio's range and reported back as though it had been accepted.
func TestTXAudioRefusesAPercentageOutsideRange(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	ctx := context.Background()
	for _, tt := range []struct {
		what string
		err  error
	}{
		{"SetTXAudioGain(-1)", k.SetTXAudioGain(ctx, newTestConn(t, k, nil), -1)},
		{"SetTXAudioGain(101)", k.SetTXAudioGain(ctx, newTestConn(t, k, nil), 101)},
		{"SetProcLevel(-1)", k.SetProcLevel(ctx, newTestConn(t, k, nil), -1)},
		{"SetProcLevel(101)", k.SetProcLevel(ctx, newTestConn(t, k, nil), 101)},
	} {
		if !errors.Is(tt.err, backend.ErrUnsupported) {
			t.Errorf("%s error = %v, want backend.ErrUnsupported", tt.what, tt.err)
		}
	}
}

// TestTransmitAudioDoesNotReachForVX checks the fence next door, from this
// side of it.
//
// VX is VOX on the TS-480 and TS-590 EXCEPT in CW, where the same command reads
// and writes break-in — and break-in decides whether CW sent over CAT reaches
// the air at all. VOX is transmit audio in an operator's mental model and is not
// this, so the hazard is a transmit-audio change reaching for VX because it
// looks adjacent. Decode must go on dropping a VX frame outside CW rather than
// publishing it as break-in, and it must go on completing the transaction while
// it does, or the read that asked would sit out its timeout.
func TestTransmitAudioDoesNotReachForVX(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	k.mode.Store(uint32(radio.ModeUSB))

	u, err := k.Decode([]byte("VX1"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Patch.BreakIn != nil {
		t.Errorf("a VX frame in USB published break-in %q; in USB that is the VOX setting, "+
			"and the CW guard would be trusting a number about something else", *u.Patch.BreakIn)
	}
	if u.Key != keyVX {
		t.Errorf("key = %q, want %q so the VX; read is still answered", u.Key, keyVX)
	}

	// And the transmit-audio poll must not be the thing that puts VX on the
	// wire by another route.
	for _, r := range k.txAudioReads() {
		if strings.HasPrefix(r.req, "VX") || strings.HasPrefix(r.req, "VG") ||
			strings.HasPrefix(r.req, "VD") {
			t.Errorf("txAudioReads asks for %q; VOX is not this control", r.req)
		}
	}
}
