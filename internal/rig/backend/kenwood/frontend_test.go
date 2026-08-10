package kenwood

import (
	"context"
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

// TestFrontEndSetWidths covers the widths, which are not the same twice and are
// not even the same between a command's set form and its own answer.
//
// PA takes one digit and answers two; RA takes two and answers four on the
// TS-480/TS-590 generation, one and one on the TS-890S, and a band selector
// plus a digit on the TS-990S. A width wrong by one is a syntax error the radio
// answers with ?; — or, worse, a value read out of the padding.
func TestFrontEndSetWidths(t *testing.T) {
	tests := []struct {
		model string
		want  []string
	}{
		// PA1; one digit. RA01; two, the pad being step 1 of one.
		{"ts590sg", []string{"PA1;", "RA01;", "RG128;", "GC2;"}},
		{"ts480", []string{"PA1;", "RA01;", "RG050;", "GT001;"}},
		// One digit on the TS-890S — "R A P1 ;" where the older pair take two —
		// and its AGC is GC with a middle speed, so fast is 3 rather than 2.
		{"ts890s", []string{"PA1;", "RA1;", "RG128;", "GC3;"}},
		// And the TS-990S puts the main-band selector in front of every one.
		{"ts990s", []string{"PA01;", "RA01;", "RG0128;", "GC03;"}},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			c := newTestConn(t, k, answersFor(k.profile))
			ctx := context.Background()

			if err := k.SetPreamp(ctx, c, 1); err != nil {
				t.Fatalf("SetPreamp: %v", err)
			}
			// The first step of whatever ladder this radio has.
			if err := k.SetAttenuator(ctx, c, k.profile.Attenuator[0]); err != nil {
				t.Fatalf("SetAttenuator: %v", err)
			}
			if err := k.SetRFGain(ctx, c, 50); err != nil {
				t.Fatalf("SetRFGain: %v", err)
			}
			if err := k.SetAGC(ctx, c, radio.AGCFast); err != nil {
				t.Fatalf("SetAGC: %v", err)
			}

			// Only the writes; each setter reads back after it, and those reads
			// are the plain query forms.
			var writes []string
			for _, s := range c.sent {
				if len(s) > 3 || (len(s) == 3 && s[2] != ';') {
					writes = append(writes, s)
				}
			}
			if len(writes) != len(tt.want) {
				t.Fatalf("writes were %v, want %v", writes, tt.want)
			}
			for i, w := range writes {
				if w != tt.want[i] {
					t.Errorf("write %d was %q, want %q", i, w, tt.want[i])
				}
			}
		})
	}
}

// TestRFGainScaleIsPerModel is the trap that would misreport the same knob by a
// factor of two and a half: RG counts 000-100 on a TS-480 and 000-255 on every
// radio after it.
func TestRFGainScaleIsPerModel(t *testing.T) {
	for _, tt := range []struct {
		model string
		pct   float64
		want  string
	}{
		{"ts480", 100, "RG100;"},
		{"ts480", 50, "RG050;"},
		{"ts590sg", 100, "RG255;"},
		{"ts590sg", 50, "RG128;"},
	} {
		k := newModelRig(t, tt.model)
		c := newTestConn(t, k, answersFor(k.profile))
		if err := k.SetRFGain(context.Background(), c, tt.pct); err != nil {
			t.Fatalf("%s SetRFGain(%g): %v", tt.model, tt.pct, err)
		}
		if got := c.sent[0]; got != tt.want {
			t.Errorf("%s SetRFGain(%g) sent %q, want %q", tt.model, tt.pct, got, tt.want)
		}
	}
}

// TestAGCCommandMovedBetweenGenerations: a TS-480 keeps the AGC speed on GT,
// and every radio after it keeps a TIME CONSTANT there and the speed on GC.
// Sending one radio's form to the other would set the wrong thing entirely.
func TestAGCCommandMovedBetweenGenerations(t *testing.T) {
	for _, tt := range []struct {
		model, want string
	}{
		{"ts480", "GT;"},
		{"ts590sg", "GC;"},
		{"ts890s", "GC;"},
	} {
		k := newModelRig(t, tt.model)
		k.mode.Store(uint32(radio.ModeCW))
		var found string
		for _, r := range k.frontEndReads() {
			if r.key == keyGC || r.key == keyGT {
				found = r.req
			}
		}
		if found != tt.want {
			t.Errorf("%s reads the AGC with %q, want %q", tt.model, found, tt.want)
		}
	}
}

// TestAGCOffIsNotAOneWayTrip is the bug a TS-590S found on the air.
//
// With the AGC off, GC1 and GC2 are BOTH refused and the radio stays off. So a
// client that switched the AGC off could never switch it back, and would be
// told only "command rejected" — the state would sit on "off" for ever with
// every attempt to leave it failing.
//
// The reference documents the parameter that gets back out — "3: AGC Off → On
// (AGC returns to its Slow/Fast status before turning Off)" — as one option
// among four. That the other two are refused from off, which is what makes it
// the only door, is not in any reference and came from the radio.
func TestAGCOffIsNotAOneWayTrip(t *testing.T) {
	for _, tt := range []struct {
		model, on string
	}{
		{"ts590sg", "GC3;"},
		{"ts890s", "GC4;"},
	} {
		t.Run(tt.model, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			k.mode.Store(uint32(radio.ModeCW))
			c := newTestConn(t, k, answersFor(k.profile))

			// The radio is off, which is what makes a speed request need help.
			k.agc.Store(radio.AGCOff)
			if err := k.SetAGC(context.Background(), c, radio.AGCFast); err != nil {
				t.Fatalf("SetAGC(fast): %v", err)
			}
			if len(c.sent) < 2 || c.sent[0] != tt.on {
				t.Fatalf("sent %v, want the off-then-on %q first", c.sent, tt.on)
			}

			// And from a speed it must NOT be sent: the reference says a 3 while
			// the AGC is on does nothing, but a command that does nothing is
			// still a command, and this one would be sent on every set.
			c.sent = nil
			k.agc.Store(radio.AGCSlow)
			if err := k.SetAGC(context.Background(), c, radio.AGCFast); err != nil {
				t.Fatalf("SetAGC(fast) from slow: %v", err)
			}
			for _, s := range c.sent {
				if s == tt.on {
					t.Errorf("sent %q while the AGC was already on: %v", tt.on, c.sent)
				}
			}
		})
	}

	// The TS-480 has no such parameter documented, so nothing extra is sent —
	// guessing a fourth value to write blind is not how this gets settled.
	k := newModelRig(t, "ts480")
	k.mode.Store(uint32(radio.ModeCW))
	c := newTestConn(t, k, answersFor(k.profile))
	k.agc.Store(radio.AGCOff)
	if err := k.SetAGC(context.Background(), c, radio.AGCFast); err != nil {
		t.Fatalf("ts480 SetAGC(fast): %v", err)
	}
	if len(c.sent) != 2 || c.sent[0] != "GT001;" {
		t.Errorf("ts480 sent %v, want just the speed and its read-back", c.sent)
	}
}

// TestAGCIsNotAskedInFM covers the note every reference in this family carries:
// "this command cannot be performed in FM mode (an error sounds)". The error
// sounds AT THE RADIO, so asking anyway would beep at whoever is listening,
// once per slow poll, for as long as they stay in FM.
func TestAGCIsNotAskedInFM(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	for _, tc := range []struct {
		mode radio.Mode
		want bool
	}{
		{radio.ModeCW, true},
		{radio.ModeUSB, true},
		{radio.ModeFM, false},
	} {
		k.mode.Store(uint32(tc.mode))
		var asked bool
		for _, r := range k.frontEndReads() {
			if r.key == keyGC {
				asked = true
			}
		}
		if asked != tc.want {
			t.Errorf("in %s the AGC read was asked=%v, want %v", tc.mode, asked, tc.want)
		}
	}
}

// TestFrontEndAnswerWidths is the decode half: an answer parsed at the setter's
// width would read the padding instead of the value.
func TestFrontEndAnswerWidths(t *testing.T) {
	k := newModelRig(t, "ts590sg")

	// PA10: preamp on, and a trailing "always 0" that is not the setting.
	u, err := k.Decode([]byte("PA10"))
	if err != nil {
		t.Fatalf("Decode(PA10): %v", err)
	}
	if u.Patch.Preamp == nil || *u.Patch.Preamp != 1 {
		t.Errorf("PA10 decoded preamp as %v, want 1", u.Patch.Preamp)
	}

	// RA0100: attenuator in, and two more digits of "always 00".
	u, err = k.Decode([]byte("RA0100"))
	if err != nil {
		t.Fatalf("Decode(RA0100): %v", err)
	}
	if u.Patch.AttenuatorDB == nil || *u.Patch.AttenuatorDB != k.profile.Attenuator[0] {
		t.Errorf("RA0100 decoded attenuator as %v, want %d dB",
			u.Patch.AttenuatorDB, k.profile.Attenuator[0])
	}

	// RA0000 is switched out, which is 0 dB rather than the ladder's first step.
	u, err = k.Decode([]byte("RA0000"))
	if err != nil {
		t.Fatalf("Decode(RA0000): %v", err)
	}
	if u.Patch.AttenuatorDB == nil || *u.Patch.AttenuatorDB != 0 {
		t.Errorf("RA0000 decoded attenuator as %v, want 0", u.Patch.AttenuatorDB)
	}
}

// TestTS480AGCInFMAnswersSpaces: its reference says the radio "responds with 3
// spaces when the GT command is used in FM mode". That must complete the read
// and publish nothing — not fail it, and not parse as a speed.
func TestTS480AGCInFMAnswersSpaces(t *testing.T) {
	k := newModelRig(t, "ts480")
	u, err := k.Decode([]byte("GT   "))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Key != keyGT {
		t.Errorf("a spaces answer keyed %q, want %q — an unmatched read fails the poll",
			u.Key, keyGT)
	}
	if u.Patch.AGC != nil {
		t.Errorf("published AGC %q from three spaces", *u.Patch.AGC)
	}
}
