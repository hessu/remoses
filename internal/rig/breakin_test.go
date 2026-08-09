package rig

import (
	"context"
	"errors"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
)

// EnsureCWWillTransmit guards the failure that reports success and puts nothing
// on the air: with break-in off, the rig accepts the CW command, drains its
// buffer on schedule, and transmits nothing. It has bitten two real radios.

func TestEnsureCWWillTransmit(t *testing.T) {
	tests := []struct {
		name string
		// policy is cw.break_in. Empty stands for a Session built without the
		// config layer's defaulting, which must behave as semi.
		policy string
		// state is what the rig reports before the send.
		state radio.BreakIn
		// wantErr is whether the send should be refused.
		wantErr bool
		// wantSet is the value written to the rig, or "" for nothing written.
		wantSet radio.BreakIn
	}{
		// Already transmitting-capable: nothing to do, whatever the policy.
		{"semi policy leaves semi alone", "semi", radio.BreakInSemi, false, ""},
		{"full policy leaves full alone", "full", radio.BreakInFull, false, ""},
		// A semi policy does not downgrade a rig already on full: the point is
		// that the Morse reaches the air, and an operator running QSK chose it.
		{"semi policy does not downgrade full", "semi", radio.BreakInFull, false, ""},
		// Nor does a full policy force QSK onto a rig already on semi, which
		// would start clocking the relays of a station that never asked.
		{"full policy does not upgrade semi", "full", radio.BreakInSemi, false, ""},

		// Off, and the policy says to fix it.
		{"semi policy switches off on", "semi", radio.BreakInOff, false, radio.BreakInSemi},
		{"full policy switches off on", "full", radio.BreakInOff, false, radio.BreakInFull},
		{"an unset policy behaves as semi", "", radio.BreakInOff, false, radio.BreakInSemi},

		// Manual: never write, and refuse what would go nowhere.
		{"manual refuses when off", "manual", radio.BreakInOff, true, ""},
		{"manual accepts when semi", "manual", radio.BreakInSemi, false, ""},
		// An unknown is not evidence of anything. Refusing on it is how a
		// safety check turns into an outage.
		{"manual accepts an unknown", "manual", radio.BreakInUnknown, false, ""},

		// Unknown under a managing policy: try to set it, since that is cheap
		// and makes the first send of a session work.
		{"semi policy sets an unknown", "semi", radio.BreakInUnknown, false, radio.BreakInSemi},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, func(rc *config.Radio) {
				rc.CW = config.CW{Enabled: true, Method: "cat", BreakIn: tt.policy}
			}).start(t)
			h.rig.setBreakInState(tt.state)

			err := h.s.EnsureCWWillTransmit(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("EnsureCWWillTransmit = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrUnsupported) {
				t.Errorf("error = %v, want ErrUnsupported so the API answers 422", err)
			}

			var wrote radio.BreakIn
			for _, s := range h.rig.setLog() {
				if len(s) > len("break_in=") && s[:len("break_in=")] == "break_in=" {
					wrote = radio.BreakIn(s[len("break_in="):])
				}
			}
			if wrote != tt.wantSet {
				t.Errorf("wrote break-in %q, want %q", wrote, tt.wantSet)
			}
		})
	}
}

// PTT already up is one of the conditions the rig references name, so an
// operator holding the transmitter down may send into it with break-in off.
func TestEnsureCWWillTransmitAcceptsPTTUp(t *testing.T) {
	h := newHarness(t, func(rc *config.Radio) {
		rc.CW = config.CW{Enabled: true, Method: "cat", BreakIn: "manual"}
	}).start(t)
	h.rig.setBreakInState(radio.BreakInOff)

	if _, err := h.s.SetPTT(context.Background(), true); err != nil {
		t.Fatalf("SetPTT: %v", err)
	}
	waitFor(t, "PTT to be reported up", func() bool { return h.s.State().PTT })

	if err := h.s.EnsureCWWillTransmit(context.Background()); err != nil {
		t.Errorf("refused a send while the transmitter is keyed: %v", err)
	}
}

// When the write itself fails, the answer depends on what was known before it.
func TestEnsureCWWillTransmitWhenTheSetFails(t *testing.T) {
	boom := errors.New("rig said no")

	t.Run("known off is refused", func(t *testing.T) {
		h := newHarness(t, func(rc *config.Radio) {
			rc.CW = config.CW{Enabled: true, Method: "cat", BreakIn: "semi"}
		}).start(t)
		h.rig.setBreakInState(radio.BreakInOff)
		h.rig.setBreakInErr(boom)

		err := h.s.EnsureCWWillTransmit(context.Background())
		if err == nil {
			t.Fatal("queued Morse that is known not to reach the air")
		}
		if !errors.Is(err, boom) {
			t.Errorf("error = %v, want it to carry the radio's own failure", err)
		}
	})

	t.Run("unknown is allowed", func(t *testing.T) {
		h := newHarness(t, func(rc *config.Radio) {
			rc.CW = config.CW{Enabled: true, Method: "cat", BreakIn: "semi"}
		}).start(t)
		h.rig.setBreakInState(radio.BreakInUnknown)
		h.rig.setBreakInErr(boom)

		if err := h.s.EnsureCWWillTransmit(context.Background()); err != nil {
			t.Errorf("refused on an unknown after a failed set: %v", err)
		}
	})
}
