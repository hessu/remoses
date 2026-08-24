package yaesu

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/rig/backend"
)

var _ backend.TXAudioController = (*Rig)(nil)

// TestSetTXAudioGainWireForm is the test the whole feature turns on.
//
// MG<nnn>; — the digits follow the letters with nothing in between. The
// alternative that had to be ruled out is MG0<nnn>;, because RG, AG and SQ all
// carry a receiver selector in front of an identically shaped three-digit
// level, and the note DESIGN.md §5.6 records about MG's 000-255 says nothing
// about what precedes the digits. The FTdx5000 is the case that settles it: it
// has a real sub receiver and does put a MAIN/SUB selector on AG, GT, BC and
// AN, and still prints MIC GAIN as six cells (page 12).
//
// The second half is the scale, which is emphatically not family-wide and does
// not follow the FA/FB generation split either — the FTdx1200 shares the
// FT-950's eight-digit frequency field and the FTdx101's 000-100 gain.
func TestSetTXAudioGainWireForm(t *testing.T) {
	for _, tt := range []struct {
		model string
		pct   float64
		want  string
	}{
		// The 000-100 radios, where the percentage and the parameter coincide.
		{"ftdx101d", 50, "MG050;"},
		{"ft-710", 0, "MG000;"},
		{"ft-891", 100, "MG100;"},
		{"ft-991a", 73, "MG073;"},
		{"ftx-1", 50, "MG050;"},
		// The 000-255 radios, where they do not. Half of 255 is 128 rounded,
		// and a client asking for 50% on an FT-950 that sent MG050 would be
		// setting the rig to a fifth of its gain.
		{"ft-950", 50, "MG128;"},
		{"ftdx5000", 100, "MG255;"},
		{"ftdx9000", 0, "MG000;"},
		// The two FT-950-generation radios that take the newer scale, which is
		// the pair that makes this a per-model field rather than a per-
		// generation one.
		{"ftdx1200", 50, "MG050;"},
		{"ftdx3000", 50, "MG050;"},
	} {
		t.Run(tt.model, func(t *testing.T) {
			y := newModelRig(t, tt.model)
			c := newTestConn(t, y, answersFor(y.profile))
			if err := y.SetTXAudioGain(context.Background(), c, tt.pct); err != nil {
				t.Fatalf("SetTXAudioGain: %v", err)
			}
			c.wantSent(t, tt.want, reqMG)
		})
	}
}

// TestSetProcWireForm covers the command with three shapes and two dialects.
//
// The FT-891 is why the off and on characters are stored per model rather than
// written into a format string. Its P2 is "0: "OFF", 1: "ON"" where the other
// seven two-parameter radios print "1: "OFF", 2: "ON"", so the family's off
// value is the FT-891's on value. That failure is not the quiet one this
// protocol usually produces: it switches the compressor on in transmit while
// the operator is asking for it off.
func TestSetProcWireForm(t *testing.T) {
	for _, tt := range []struct {
		model  string
		on     bool
		want   string
		wantRB string
	}{
		// The two-parameter form. The leading 0 picks the speech processor over
		// the parametric microphone equalizer, and the read-back carries it too.
		{"ftdx101d", true, "PR02;", reqPRSelect},
		{"ftdx101d", false, "PR01;", reqPRSelect},
		{"ft-710", true, "PR02;", reqPRSelect},
		{"ftdx10", false, "PR01;", reqPRSelect},
		{"ftx-1", true, "PR02;", reqPRSelect},
		{"ftdx1200", true, "PR02;", reqPRSelect},
		{"ftdx3000", false, "PR01;", reqPRSelect},
		// The FT-891, alone in the registry, inverts them.
		{"ft-891", true, "PR01;", reqPRSelect},
		{"ft-891", false, "PR00;", reqPRSelect},
		// The single-parameter form: no selector at all, and read with a bare
		// PR; rather than PR0;.
		{"ft-950", true, "PR1;", reqPRSingle},
		{"ft-950", false, "PR0;", reqPRSingle},
		{"ftdx5000", true, "PR1;", reqPRSingle},
	} {
		name := tt.model + "-off"
		if tt.on {
			name = tt.model + "-on"
		}
		t.Run(name, func(t *testing.T) {
			y := newModelRig(t, tt.model)
			c := newTestConn(t, y, answersFor(y.profile))
			if err := y.SetProc(context.Background(), c, tt.on); err != nil {
				t.Fatalf("SetProc: %v", err)
			}
			c.wantSent(t, tt.want, tt.wantRB)
		})
	}
}

// TestSetProcLevelWireForm.
//
// Two things vary. The FTdx5000 and FTdx9000 put the level on the same 000-255
// index they use for MG, PC and RG; and the FT-710 and FTX-1 reserve 000 for
// "OFF", so their level starts at 001 and a request for 0% has to clamp there
// rather than switch the processor off behind PR's back.
func TestSetProcLevelWireForm(t *testing.T) {
	for _, tt := range []struct {
		model string
		pct   float64
		want  string
	}{
		{"ftdx101d", 50, "PL050;"},
		{"ftdx101d", 0, "PL000;"},
		{"ft-891", 100, "PL100;"},
		{"ft-950", 50, "PL050;"},
		{"ftdx1200", 0, "PL000;"},
		// 000 means "OFF" on these two, so 0% is the bottom of the level rather
		// than the switch.
		{"ft-710", 0, "PL001;"},
		{"ft-710", 100, "PL100;"},
		{"ftx-1", 0, "PL001;"},
		// And the 000-255 pair.
		{"ftdx5000", 50, "PL128;"},
		{"ftdx9000", 100, "PL255;"},
	} {
		t.Run(tt.model, func(t *testing.T) {
			y := newModelRig(t, tt.model)
			c := newTestConn(t, y, answersFor(y.profile))
			if err := y.SetProcLevel(context.Background(), c, tt.pct); err != nil {
				t.Fatalf("SetProcLevel: %v", err)
			}
			c.wantSent(t, tt.want, reqPL)
		})
	}
}

// TestDecodeMicGain is the read path, and the offset is the whole point.
//
// MG0128 is what an MG that took a receiver selector would answer for a radio
// at half gain. Read at the wrong offset it is 012 against 255, published as
// 4.7% — a confident wrong number rather than a missing one, which DESIGN.md
// §5.4 puts above every other consideration in this backend. Nothing is
// published for it, because a four-character MG argument means this backend and
// the radio disagree about the command, not that a reading needs rescuing.
func TestDecodeMicGain(t *testing.T) {
	for _, tt := range []struct {
		model string
		frame string
		want  *float64
	}{
		{"ftdx101d", "MG050", ptr(50.0)},
		{"ftdx101d", "MG000", ptr(0.0)},
		{"ftdx101d", "MG100", ptr(100.0)},
		// The same three digits mean something different on a 255-scale radio.
		{"ft-950", "MG128", ptr(128.0 / 255 * 100)},
		{"ftdx5000", "MG255", ptr(100.0)},
		// The frame that must not be read: a selector where there is none.
		{"ftdx5000", "MG0128", nil},
		{"ftdx101d", "MG0050", nil},
		// And the ordinary malformations. Three characters that are not a
		// number get the key — the transaction still has to complete, or an
		// unmatched request fails the whole poll — and publish nothing.
		{"ftdx101d", "MG05", nil},
		{"ftdx101d", "MG12-", nil},
		{"ftdx101d", "MG", nil},
	} {
		t.Run(tt.model+"-"+tt.frame, func(t *testing.T) {
			y := newModelRig(t, tt.model)
			u, err := y.Decode([]byte(tt.frame))
			if err != nil {
				t.Fatalf("Decode(%s): %v", tt.frame, err)
			}
			if u.Key != keyMG {
				t.Errorf("Decode(%s) keyed %q, want %q", tt.frame, u.Key, keyMG)
			}
			switch {
			case tt.want == nil && u.Patch.TXAudioGain != nil:
				t.Errorf("Decode(%s) published %.2f%%; a frame this backend cannot "+
					"place must publish nothing", tt.frame, *u.Patch.TXAudioGain)
			case tt.want != nil && u.Patch.TXAudioGain == nil:
				t.Errorf("Decode(%s) published nothing, want %.2f%%", tt.frame, *tt.want)
			case tt.want != nil && !closeEnough(*u.Patch.TXAudioGain, *tt.want):
				t.Errorf("Decode(%s) = %.2f%%, want %.2f%%",
					tt.frame, *u.Patch.TXAudioGain, *tt.want)
			}
		})
	}
}

// TestDecodeProc covers both PR shapes and what each of them discards.
//
// The parametric microphone equalizer is the reason this is not a one-line
// decoder. On the two-parameter models it has PR's first parameter to itself,
// and this decoder sits on the AI push path, so a PR1<state> really does arrive
// whenever somebody touches the equalizer on the front panel — it says nothing
// about the compressor and must not be folded into Proc. On the
// single-parameter models it is the third value of the one parameter, and there
// it DOES say something: the equalizer has the audio, so the compressor does
// not, and false is the true answer.
func TestDecodeProc(t *testing.T) {
	for _, tt := range []struct {
		model string
		frame string
		want  *bool
	}{
		// Two-parameter, family dialect.
		{"ftdx101d", "PR02", ptrBool(true)},
		{"ftdx101d", "PR01", ptrBool(false)},
		// The parametric equalizer's own frame, discarded on both states.
		{"ftdx101d", "PR12", nil},
		{"ftdx101d", "PR11", nil},
		// A state this model has no value for.
		{"ftdx101d", "PR00", nil},
		// Two-parameter, the FT-891's inverted pair. The same PR01 that means
		// "off" on an FTdx101 means "on" here, which is exactly the reading a
		// family default would get backwards.
		{"ft-891", "PR01", ptrBool(true)},
		{"ft-891", "PR00", ptrBool(false)},
		{"ft-891", "PR02", nil},
		{"ft-891", "PR11", nil},
		// Single-parameter.
		{"ft-950", "PR1", ptrBool(true)},
		{"ft-950", "PR0", ptrBool(false)},
		// "2: Parametric Microphone Equalizer "ON"" — the compressor is out of
		// circuit, so it reads as off rather than as nothing. Publishing
		// nothing would leave a stale true standing.
		{"ft-950", "PR2", ptrBool(false)},
		{"ftdx5000", "PR2", ptrBool(false)},
		// A single-parameter radio sent a two-parameter answer, and the other
		// way round: both are a disagreement about the command, not a reading.
		{"ft-950", "PR02", nil},
		{"ftdx101d", "PR2", nil},
		// And the FTdx9000, which is never asked and whose answers are ignored
		// if they somehow arrive: its manual's PR entry spells the command PC.
		{"ftdx9000", "PR1", nil},
		{"ftdx9000", "PR02", nil},
	} {
		t.Run(tt.model+"-"+tt.frame, func(t *testing.T) {
			y := newModelRig(t, tt.model)
			u, err := y.Decode([]byte(tt.frame))
			if err != nil {
				t.Fatalf("Decode(%s): %v", tt.frame, err)
			}
			if u.Key != keyPR {
				t.Errorf("Decode(%s) keyed %q, want %q", tt.frame, u.Key, keyPR)
			}
			switch {
			case tt.want == nil && u.Patch.Proc != nil:
				t.Errorf("Decode(%s) published proc=%v, want nothing", tt.frame, *u.Patch.Proc)
			case tt.want != nil && u.Patch.Proc == nil:
				t.Errorf("Decode(%s) published nothing, want proc=%v", tt.frame, *tt.want)
			case tt.want != nil && *u.Patch.Proc != *tt.want:
				t.Errorf("Decode(%s) = %v, want %v", tt.frame, *u.Patch.Proc, *tt.want)
			}
		})
	}
}

// TestDecodeProcLevel, with the same exact-length rule as MG and for the same
// reason.
func TestDecodeProcLevel(t *testing.T) {
	for _, tt := range []struct {
		model string
		frame string
		want  *float64
	}{
		{"ftdx101d", "PL050", ptr(50.0)},
		{"ft-950", "PL100", ptr(100.0)},
		{"ftdx5000", "PL128", ptr(128.0 / 255 * 100)},
		{"ftdx9000", "PL255", ptr(100.0)},
		// 000 is a documented "OFF" on these two rather than the bottom of the
		// scale, and it still reads back as 0%: the switch is PR's to report,
		// and a level below the floor clamps rather than going negative.
		{"ft-710", "PL000", ptr(0.0)},
		{"ft-710", "PL001", ptr(0.0)},
		{"ftx-1", "PL000", ptr(0.0)},
		{"ft-710", "PL100", ptr(100.0)},
		{"ftdx101d", "PL0050", nil},
		{"ftdx101d", "PL", nil},
	} {
		t.Run(tt.model+"-"+tt.frame, func(t *testing.T) {
			y := newModelRig(t, tt.model)
			u, err := y.Decode([]byte(tt.frame))
			if err != nil {
				t.Fatalf("Decode(%s): %v", tt.frame, err)
			}
			if u.Key != keyPL {
				t.Errorf("Decode(%s) keyed %q, want %q", tt.frame, u.Key, keyPL)
			}
			switch {
			case tt.want == nil && u.Patch.ProcLevel != nil:
				t.Errorf("Decode(%s) published %.2f%%, want nothing", tt.frame, *u.Patch.ProcLevel)
			case tt.want != nil && u.Patch.ProcLevel == nil:
				t.Errorf("Decode(%s) published nothing, want %.2f%%", tt.frame, *tt.want)
			case tt.want != nil && !closeEnough(*u.Patch.ProcLevel, *tt.want):
				t.Errorf("Decode(%s) = %.2f%%, want %.2f%%",
					tt.frame, *u.Patch.ProcLevel, *tt.want)
			}
		})
	}
}

// TestTXAudioCapsMatchTheProfile.
//
// A capability list is a promise, and this is the half a client reads before it
// draws a slider. The FTdx9000 is the only radio here that has to say no to any
// of the three, and it says no to exactly one of them — which is what having
// three flags rather than one is for.
func TestTXAudioCapsMatchTheProfile(t *testing.T) {
	for _, name := range ModelNames() {
		y := newModelRig(t, name)
		caps := y.Caps()
		if !caps.TXAudioGainControl {
			t.Errorf("%s claims no tx_audio_gain_control; every manual read for this "+
				"backend has an MG row", name)
		}
		if !caps.ProcLevelControl {
			t.Errorf("%s claims no proc_level_control; every manual read for this "+
				"backend has a PL row", name)
		}
		wantProc := name != "ftdx9000"
		if caps.ProcControl != wantProc {
			t.Errorf("%s reports proc_control=%v, want %v", name, caps.ProcControl, wantProc)
		}
	}
}

// TestFTdx9000RefusesTheProcessorSwitch.
//
// Not because the radio lacks one — it has an RF speech processor and its
// command list names PR — but because the manual's PR entry spells the command
// "P C" in all three of its rows, which are the letters of TX Power Level
// sitting directly above it. One of those two statements is a misprint and the
// document does not say which, so the frame is not transcribed. This is the one
// capability in this file gated by a documentation defect rather than by a
// missing feature, and it is worth a test of its own so that the day somebody
// confirms it on hardware, the thing to change is obvious.
func TestFTdx9000RefusesTheProcessorSwitch(t *testing.T) {
	y := newModelRig(t, "ftdx9000")
	c := newTestConn(t, y, answersFor(y.profile))

	err := y.SetProc(context.Background(), c, true)
	if err == nil {
		t.Fatal("SetProc was accepted on the FTdx9000, whose PR frame is not transcribed")
	}
	if !errors.Is(err, backend.ErrUnsupported) {
		t.Errorf("SetProc returned %v, want backend.ErrUnsupported so the API answers "+
			"422 rather than inviting a retry", err)
	}
	if !strings.Contains(err.Error(), y.profile.Label) {
		t.Errorf("SetProc said %q without naming the %s", err, y.profile.Label)
	}
	// Nothing may go out before the refusal, and least of all a PC frame: the
	// letters this manual prints in the PR block belong to TX Power Level.
	if len(c.sent) != 0 {
		t.Errorf("SetProc put %q on the wire before refusing", c.sent)
	}

	// The other two are transcribed and work, which is the point of refusing
	// only the one.
	if err := y.SetTXAudioGain(context.Background(), c, 50); err != nil {
		t.Errorf("SetTXAudioGain on the FTdx9000: %v", err)
	}
	if err := y.SetProcLevel(context.Background(), c, 50); err != nil {
		t.Errorf("SetProcLevel on the FTdx9000: %v", err)
	}
}

// TestTXAudioRangesAreRefused. A percentage outside 0-100 is the caller's
// mistake, and it has to come back as ErrUnsupported rather than as a clamped
// value: no Yaesu documents a rejection, so a parameter the rig will not take
// is answered with silence and costs the session's whole per-command timeout.
func TestTXAudioRangesAreRefused(t *testing.T) {
	y := newModelRig(t, "ftdx101d")
	ctx := context.Background()
	for _, tt := range []struct {
		what string
		call func(c backend.Conn, pct float64) error
	}{
		{"SetTXAudioGain", func(c backend.Conn, pct float64) error { return y.SetTXAudioGain(ctx, c, pct) }},
		{"SetProcLevel", func(c backend.Conn, pct float64) error { return y.SetProcLevel(ctx, c, pct) }},
	} {
		for _, pct := range []float64{-1, 100.5, 1000} {
			c := newTestConn(t, y, answersFor(y.profile))
			err := tt.call(c, pct)
			if !errors.Is(err, backend.ErrUnsupported) {
				t.Errorf("%s(%.1f%%) = %v, want backend.ErrUnsupported", tt.what, pct, err)
			}
			if len(c.sent) != 0 {
				t.Errorf("%s(%.1f%%) put %q on the wire", tt.what, pct, c.sent)
			}
		}
	}
}

// TestNoUnclaimedTransmitAudioCommandReachesTheWire is the durable invariant,
// and it is not "MG is never sent" — it is "a radio whose caps say it has no
// transmit audio control is never asked about that control".
//
// A poll item added ahead of its profile gate is exactly how that breaks, and
// on this protocol it breaks quietly: the rig answers a command it does not
// implement with silence, so the only symptom is a slow tier that costs a
// per-command timeout more than it used to. Today the FTdx9000's PR is the only
// thing this catches, which is the point — it is the case that exists.
func TestNoUnclaimedTransmitAudioCommandReachesTheWire(t *testing.T) {
	for _, name := range ModelNames() {
		t.Run(name, func(t *testing.T) {
			y := newModelRig(t, name)
			caps := y.Caps()
			c := newTestConn(t, y, answersFor(y.profile))
			ctx := context.Background()

			if err := y.Init(ctx, c); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if err := y.Poll(ctx, c, backend.PollFast); err != nil {
				t.Fatalf("PollFast: %v", err)
			}
			if err := y.Poll(ctx, c, backend.PollSlow); err != nil {
				t.Fatalf("PollSlow: %v", err)
			}

			forbidden := map[string]bool{
				"MG": !caps.TXAudioGainControl,
				"PR": !caps.ProcControl,
				"PL": !caps.ProcLevelControl,
			}
			for _, req := range c.sent {
				for cmd, banned := range forbidden {
					if banned && strings.HasPrefix(req, cmd) {
						t.Errorf("asked the %s for %q while its caps report no such control; "+
							"a Yaesu answers a command it does not implement with silence, so "+
							"this costs a per-command timeout every tick", y.profile.Label, req)
					}
				}
			}
		})
	}
}

// TestTransmitAudioIsPolledOnEveryModelThatClaimsIt is the other direction, and
// the one a stale capability flag would break: a radio that promises a control
// has to actually read it, or State publishes a slider that never moves.
func TestTransmitAudioIsPolledOnEveryModelThatClaimsIt(t *testing.T) {
	for _, name := range ModelNames() {
		t.Run(name, func(t *testing.T) {
			y := newModelRig(t, name)
			caps := y.Caps()
			c := newTestConn(t, y, answersFor(y.profile))
			if err := y.Poll(context.Background(), c, backend.PollSlow); err != nil {
				t.Fatalf("PollSlow: %v", err)
			}
			sent := strings.Join(c.sent, " ")
			for _, tt := range []struct {
				claimed bool
				req     string
				cap     string
			}{
				{caps.TXAudioGainControl, reqMG, "tx_audio_gain_control"},
				{caps.ProcControl, y.procRead(), "proc_control"},
				{caps.ProcLevelControl, reqPL, "proc_level_control"},
			} {
				if tt.claimed && !strings.Contains(sent, tt.req) {
					t.Errorf("%s claims %s but the slow poll never sends %q (sent %q)",
						name, tt.cap, tt.req, c.sent)
				}
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }

// closeEnough compares two percentages, which are a fraction of an integer
// count and so rarely land on a round number.
func closeEnough(got, want float64) bool {
	d := got - want
	return d < 0.005 && d > -0.005
}
