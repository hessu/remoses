package rig

import (
	"context"
	"errors"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
)

// A request the station's own rules forbid must be rejected on its merits even
// when the radio is unreachable. Answering "not connected" for an out-of-band
// frequency tells the operator to retry something that must never succeed, and
// it hides a configuration or client bug behind a transient-looking error.
//
// These use an unstarted session, so the radio has never connected.

func TestLimitsRejectedBeforeConnectionCheck(t *testing.T) {
	h := newHarness(t, func(rc *config.Radio) {
		rc.Limits.Bands = []config.Band{{LowHz: 14000000, HighHz: 14350000}}
	})
	if h.s.Connected() {
		t.Fatal("session should not be connected before Start")
	}

	t.Run("SetFrequency", func(t *testing.T) {
		_, err := h.s.SetFrequency(context.Background(), radio.VFOCurrent, 7010000)
		if !errors.Is(err, ErrOutOfBand) {
			t.Fatalf("err = %v, want ErrOutOfBand", err)
		}
		if errors.Is(err, ErrDisconnected) {
			t.Error("out-of-band request reported as a connection failure")
		}
	})

	t.Run("ApplyPatch", func(t *testing.T) {
		hz := uint64(7010000)
		_, err := h.s.ApplyPatch(context.Background(), PatchRequest{Frequency: &hz})
		if !errors.Is(err, ErrOutOfBand) {
			t.Fatalf("err = %v, want ErrOutOfBand", err)
		}
		if errors.Is(err, ErrDisconnected) {
			t.Error("out-of-band request reported as a connection failure")
		}
	})

	t.Run("in-band still reports disconnected", func(t *testing.T) {
		hz := uint64(14025000)
		_, err := h.s.ApplyPatch(context.Background(), PatchRequest{Frequency: &hz})
		if !errors.Is(err, ErrDisconnected) {
			t.Fatalf("err = %v, want ErrDisconnected", err)
		}
	})
}
