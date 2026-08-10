package kenwood

import (
	"context"
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Both of these come from a TS-590S rather than from its reference, and both
// make a set fail if got wrong.

// A set carrying P1=1 is refused: AC010 and AC000 are accepted where AC110 and
// AC100 answer ?;. The tuning form keeps its 1, because that is what the
// reference documents and what was verified transmitting.
func TestSetTunerSendsP1Zero(t *testing.T) {
	tests := []struct {
		name    string
		startAt string
		on      bool
		want    string
	}{
		{"switching on", "AC000", true, "AC010;"},
		{"switching off", "AC110", false, "AC000;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			answers := initAnswers()
			answers[reqAC] = tt.startAt
			c := newTestConn(t, k, answers)

			if err := k.SetTuner(context.Background(), c, tt.on); err != nil {
				t.Fatalf("SetTuner: %v", err)
			}
			// Read, write, read back.
			c.wantSent(t, reqAC, tt.want, reqAC)
		})
	}
}

// The radio refuses a set that changes nothing, so an ordinary idempotent
// request — the same PATCH twice, or the same button pressed twice — would fail
// the second time. SetTuner reads first and writes nothing it does not need to.
func TestSetTunerSkipsANoOp(t *testing.T) {
	for _, tt := range []struct {
		name    string
		startAt string
		on      bool
	}{
		{"already on", "AC110", true},
		{"already off", "AC000", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			answers := initAnswers()
			answers[reqAC] = tt.startAt
			c := newTestConn(t, k, answers)

			if err := k.SetTuner(context.Background(), c, tt.on); err != nil {
				t.Fatalf("SetTuner: %v", err)
			}
			// The read that established there was nothing to do, and no write.
			c.wantSent(t, reqAC)
		})
	}
}

// AC111 exactly: the form the reference names and the one verified on the air.
func TestStartTuneSendsAC111(t *testing.T) {
	k := newRig(t, 2, true)
	answers := initAnswers()
	answers[reqAC] = "AC111"
	c := newTestConn(t, k, answers)

	if err := k.StartTune(context.Background(), c); err != nil {
		t.Fatalf("StartTune: %v", err)
	}
	c.wantSent(t, "AC111;", reqAC)
}

func TestDecodeAC(t *testing.T) {
	tests := []struct {
		frame string
		want  radio.Tuner
	}{
		// P3 wins: a cycle is running whatever P2 says.
		{"AC111", radio.TunerTuning},
		{"AC011", radio.TunerTuning},
		// Otherwise P2 is the tuner: in line or bypassed.
		{"AC110", radio.TunerOn},
		{"AC010", radio.TunerOn},
		{"AC000", radio.TunerOff},
		{"AC100", radio.TunerOff},
	}
	for _, tt := range tests {
		t.Run(tt.frame, func(t *testing.T) {
			k := newRig(t, 2, true)
			u, err := k.Decode([]byte(tt.frame))
			if err != nil {
				t.Fatal(err)
			}
			if u.Key != keyAC {
				t.Errorf("key = %q, want %q", u.Key, keyAC)
			}
			if u.Patch.Tuner == nil || *u.Patch.Tuner != tt.want {
				t.Errorf("tuner = %v, want %s", u.Patch.Tuner, tt.want)
			}
			if k.Tuner() != tt.want {
				t.Errorf("Tuner() = %s, want %s", k.Tuner(), tt.want)
			}
		})
	}
}

// A tuning cycle is a second or two, so while one runs the tuner moves to the
// fast tier: on the slow one a whole cycle could pass between reads.
func TestTunerWatchedOnTheFastTierWhileTuning(t *testing.T) {
	asked := func(t *testing.T, tuning bool) bool {
		t.Helper()
		k := newRig(t, 2, true)
		answers := initAnswers()
		if tuning {
			k.tuner.Store(radio.TunerTuning)
			answers[reqAC] = "AC111"
			// A cycle keys the transmitter, so the meters are read too.
			answers[reqRM] = "RM10012"
		}
		c := newTestConn(t, k, answers)
		if err := k.Poll(context.Background(), c, backend.PollFast); err != nil {
			t.Fatalf("Poll: %v", err)
		}
		for _, s := range c.sent {
			if s == reqAC {
				return true
			}
		}
		return false
	}

	if asked(t, false) {
		t.Error("read the tuner on the fast tier with no cycle running")
	}
	if !asked(t, true) {
		t.Error("did not follow a tuning cycle on the fast tier")
	}
}
