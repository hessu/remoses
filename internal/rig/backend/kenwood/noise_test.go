package kenwood

import (
	"context"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

// TestNRLevelScaleFollowsTheReducer is the one number in this file that means
// two different things.
//
// RL is the noise reduction level, and its range depends on which reducer is
// running: "When NR1 is ON: 01 ~ 10" and "When NR2 is ON: 00 (2ms) ~ 09
// (20ms)". NR1's is an effective level and NR2's is a following speed in
// milliseconds — not two strengths of one control, two controls sharing a
// command. A percentage written against the wrong range is off by a step and by
// a whole unit of meaning.
func TestNRLevelScaleFollowsTheReducer(t *testing.T) {
	for _, tt := range []struct {
		nr   int
		pct  float64
		want string
	}{
		// NR1: 01 to 10.
		{1, 0, "RL01;"},
		{1, 100, "RL10;"},
		{1, 50, "RL06;"},
		// NR2: 00 to 09, the reference's 2 ms to 20 ms.
		{2, 0, "RL00;"},
		{2, 100, "RL09;"},
		{2, 50, "RL05;"},
	} {
		k := newModelRig(t, "ts590sg")
		c := newTestConn(t, k, answersFor(k.profile))
		k.nr.Store(tt.nr)
		if err := k.SetNRLevel(context.Background(), c, tt.pct); err != nil {
			t.Fatalf("NR%d SetNRLevel(%g): %v", tt.nr, tt.pct, err)
		}
		if c.sent[0] != tt.want {
			t.Errorf("with NR%d, SetNRLevel(%g) sent %q, want %q",
				tt.nr, tt.pct, c.sent[0], tt.want)
		}
	}
}

// TestLevelsAreNotAskedForWhileTheirCircuitIsOff covers the two "an error
// occurs" notes: NL is refused with the blanker off and RL with the reducer
// off. Asking anyway would draw a refusal on every slow tick.
func TestLevelsAreNotAskedForWhileTheirCircuitIsOff(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	k.mode.Store(uint32(radio.ModeCW))

	// Nothing reported yet: neither level is asked for.
	if got := readNames(k.noiseReads()); has(got, reqNL) || has(got, reqRL) {
		t.Errorf("levels asked for before anything was known: %v", got)
	}

	k.nb.Store(1)
	k.nr.Store(2)
	got := readNames(k.noiseReads())
	if !has(got, reqNL) || !has(got, reqRL) {
		t.Errorf("levels not asked for with both circuits on: %v", got)
	}

	// And the blanker's "both on at once", which its reference calls out
	// separately as an error for NL.
	k.nb.Store(3)
	if got := readNames(k.noiseReads()); has(got, reqNL) {
		t.Errorf("NL asked for with both blankers on, which its reference refuses: %v", got)
	}
}

func readNames(rs []read) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.req)
	}
	return out
}

func has(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// TestNotchSelectorMapsToTwoSwitches: NT is one selector carrying off, auto and
// manual, and the API publishes two independent booleans. Both directions have
// to agree or a client would see a notch it did not set.
func TestNotchSelectorMapsToTwoSwitches(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	for _, tt := range []struct {
		answer       string
		manual, auto bool
	}{
		{"NT00", false, false},
		{"NT10", false, true},
		{"NT20", true, false},
	} {
		u, err := k.Decode([]byte(tt.answer))
		if err != nil {
			t.Fatalf("Decode(%s): %v", tt.answer, err)
		}
		if u.Patch.Notch == nil || *u.Patch.Notch != tt.manual ||
			u.Patch.AutoNotch == nil || *u.Patch.AutoNotch != tt.auto {
			t.Errorf("%s decoded as manual=%v auto=%v, want %v/%v",
				tt.answer, u.Patch.Notch, u.Patch.AutoNotch, tt.manual, tt.auto)
		}
	}
}

// TestSwitchingOneNotchOffLeavesTheOther is the subtle half of one selector
// standing in for two switches.
//
// With the radio in auto, "turn the manual notch off" is already true — there
// is no manual notch running. Sending NT0 would switch the AUTOMATIC notch off
// as well, cancelling a control the caller never mentioned.
func TestSwitchingOneNotchOffLeavesTheOther(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	c := newTestConn(t, k, answersFor(k.profile))
	k.notch.Store(ntAuto)

	if err := k.SetNotch(context.Background(), c, false); err != nil {
		t.Fatalf("SetNotch(false): %v", err)
	}
	if len(c.sent) != 0 {
		t.Errorf("sent %v to turn off a manual notch that was not on; the automatic "+
			"one would have gone with it", c.sent)
	}

	// The mirror: turning the automatic one off while it IS on does write.
	if err := k.SetAutoNotch(context.Background(), c, false); err != nil {
		t.Fatalf("SetAutoNotch(false): %v", err)
	}
	if len(c.sent) == 0 || c.sent[0] != "NT00;" {
		t.Errorf("sent %v, want NT00; to switch the automatic notch out", c.sent)
	}
}

// TestNotchSetIsVerified is the TS-590S's silent refusal.
//
// In CW the radio ignores a request for the automatic notch outright: NT10 is
// accepted with no error and a read still answers NT20. The automatic notch is
// an SSB and AM function there — in CW it would notch the wanted signal, which
// is the whole reason somebody is listening. Without the check the request
// would answer 200 carrying a state that plainly shows it did not happen.
func TestNotchSetIsVerified(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	k.mode.Store(uint32(radio.ModeCW))
	// The radio answers "manual" whatever it is asked for, as one in CW does.
	c := newTestConn(t, k, map[string]string{reqNT: "NT20"})

	err := k.SetAutoNotch(context.Background(), c, true)
	if err == nil {
		t.Fatal("SetAutoNotch reported success against a radio that ignored it")
	}
	if !strings.Contains(err.Error(), "SSB") {
		t.Errorf("refusal was %q, which does not say why the radio declined", err)
	}

	// And a set the radio DOES take must not be reported as a failure.
	c = newTestConn(t, k, map[string]string{reqNT: "NT20"})
	if err := k.SetNotch(context.Background(), c, true); err != nil {
		t.Errorf("SetNotch(true) failed against a radio that took it: %v", err)
	}
}

// TestAntennaWritesOnlyWhatItMeans: AN carries three parameters and 9 means
// "leave this one alone", which is what keeps the antenna and the receive input
// independent.
func TestAntennaWritesOnlyWhatItMeans(t *testing.T) {
	k := newModelRig(t, "ts590sg")

	c := newTestConn(t, k, answersFor(k.profile))
	if err := k.SetAntenna(context.Background(), c, 2); err != nil {
		t.Fatalf("SetAntenna(2): %v", err)
	}
	if c.sent[0] != "AN299;" {
		t.Errorf("SetAntenna(2) sent %q, want AN299; — the 9s leave the receive "+
			"input and the drive output alone", c.sent[0])
	}

	c = newTestConn(t, k, answersFor(k.profile))
	if err := k.SetRXAntenna(context.Background(), c, true); err != nil {
		t.Fatalf("SetRXAntenna(true): %v", err)
	}
	if c.sent[0] != "AN919;" {
		t.Errorf("SetRXAntenna(true) sent %q, want AN919;", c.sent[0])
	}

	// The answer is the antenna, the receive input and the drive output, one
	// digit each: AN110 is ANT1 with the receive antenna in use.
	u, err := k.Decode([]byte("AN110"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Patch.Antenna == nil || *u.Patch.Antenna != 1 {
		t.Errorf("AN110 decoded antenna as %v, want 1", u.Patch.Antenna)
	}
	if u.Patch.RXAntenna == nil || !*u.Patch.RXAntenna {
		t.Errorf("AN110 decoded rx antenna as %v, want true", u.Patch.RXAntenna)
	}

	// ANT2 with the receive antenna out.
	u, err = k.Decode([]byte("AN200"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Patch.Antenna == nil || *u.Patch.Antenna != 2 {
		t.Errorf("AN200 decoded antenna as %v, want 2", u.Patch.Antenna)
	}
	if u.Patch.RXAntenna == nil || *u.Patch.RXAntenna {
		t.Errorf("AN200 decoded rx antenna as %v, want false", u.Patch.RXAntenna)
	}
}

// TestBothBlankersOnIsNotALevel: NB answers 3 when NB1 and NB2 are both
// running. That is a combination rather than a third blanker, so it is not
// published — calling it "3" would tell a client it is more blanking than 2.
func TestBothBlankersOnIsNotALevel(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	u, err := k.Decode([]byte("NB3"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Key != keyNB {
		t.Errorf("NB3 keyed %q, want %q — an unmatched read fails the poll", u.Key, keyNB)
	}
	if u.Patch.NoiseBlanker != nil {
		t.Errorf("published noise_blanker %d for both blankers at once",
			*u.Patch.NoiseBlanker)
	}
	// It is still remembered, because it decides whether NL may be asked for.
	if k.noiseBlanker() != 3 {
		t.Errorf("the reading was not kept: %d", k.noiseBlanker())
	}
}
