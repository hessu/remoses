package civ

import (
	"context"
	"errors"
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// TestBreakInRoundTrip covers command 16 47 in both directions and all three
// values the reference gives.
func TestBreakInRoundTrip(t *testing.T) {
	s := newSim(t)
	ctx := context.Background()

	for _, tc := range []struct {
		v    radio.BreakIn
		wire byte
	}{
		{radio.BreakInOff, 0x00},
		{radio.BreakInSemi, 0x01},
		{radio.BreakInFull, 0x02},
	} {
		if err := s.backend.SetBreakIn(ctx, s, tc.v); err != nil {
			t.Fatalf("SetBreakIn(%s): %v", tc.v, err)
		}
		if s.breakIn != tc.wire {
			t.Errorf("SetBreakIn(%s) put %02X on the wire, want %02X", tc.v, s.breakIn, tc.wire)
		}
		u, err := s.backend.Decode(fromRig(cmdFunc, subBreakIn, tc.wire))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if u.Key != KeyBreakIn {
			t.Errorf("key = %q, want %q", u.Key, KeyBreakIn)
		}
		if u.Patch.BreakIn == nil || *u.Patch.BreakIn != tc.v {
			t.Errorf("decoded %v, want %s", u.Patch.BreakIn, tc.v)
		}
		if got := s.backend.BreakIn(); got != tc.v {
			t.Errorf("BreakIn() = %q after decoding %s", got, tc.v)
		}
	}
}

// TestBreakInTransmits is the predicate the CW path turns on, and the reason
// this setting is modelled at all: semi and full key the transmitter, off does
// not, and unknown must not be read as off.
func TestBreakInTransmits(t *testing.T) {
	for v, want := range map[radio.BreakIn]bool{
		radio.BreakInOff:     false,
		radio.BreakInSemi:    true,
		radio.BreakInFull:    true,
		radio.BreakInUnknown: false,
	} {
		if got := v.Transmits(); got != want {
			t.Errorf("BreakIn(%q).Transmits() = %v, want %v", v, got, want)
		}
	}
}

// TestBreakInIsInTheSlowPoll is the other half of setting it, and the half that
// was missed.
//
// 16 47 is read on the slow tier, so a request that changes break-in has to
// re-read that tier or the answer carries the value from before the write. It
// did not, and the symptom was ugly: the radio's own display showed BKIN on
// while remoses reported it off and went on refusing to send CW.
func TestBreakInIsInTheSlowPoll(t *testing.T) {
	s := newSim(t)
	s.breakIn = 0x02 // full

	if err := s.backend.Poll(context.Background(), s, backend.PollSlow); err != nil {
		t.Fatalf("Poll(slow): %v", err)
	}
	found := false
	for _, req := range s.requests() {
		if req == "16" {
			found = true
		}
	}
	if !found {
		t.Fatal("the slow poll does not read 16 47; nothing else would ever notice " +
			"break-in changing, at the radio or through remoses")
	}
	if got := s.backend.BreakIn(); got != radio.BreakInFull {
		t.Errorf("BreakIn() = %q after a slow poll, want full", got)
	}
}

// TestBreakInRefusedWithoutTheCommand keeps the capability honest: a radio
// whose reference has not been read for 16 47 must refuse rather than send it.
func TestBreakInRefusedWithoutTheCommand(t *testing.T) {
	r := singleVFORig(t) // an IC-7300 profile, which does not carry the flag
	var c captureConn

	err := r.SetBreakIn(context.Background(), &c, radio.BreakInFull)
	if err == nil {
		t.Fatal("SetBreakIn succeeded on a radio without the command")
	}
	if !errors.Is(err, backend.ErrUnsupported) {
		t.Errorf("refused with %v, which the API reports as a 500", err)
	}
	if len(c.sent) != 0 {
		t.Error("a frame reached a radio that has no such command")
	}
	if r.Caps().BreakInControl {
		t.Error("caps advertise break-in control on a radio without it")
	}
	if got := r.BreakIn(); got != radio.BreakInUnknown {
		t.Errorf("BreakIn() = %q on an unasked radio, want unknown — and unknown "+
			"must never be treated as off, or CW would be refused on every radio "+
			"whose reference has not been read", got)
	}
}
