package rig

import (
	"context"
	"errors"
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

// Two bugs an FT-857D found on the air, both in ApplyPatch and neither specific
// to that radio. The first is about a VFO the request names and the radio
// cannot address; the second about a mode-and-data-mode pairing the radio has
// no code for. See applyModePair and the VFO check at the top of ApplyPatch.

// A named VFO must be refused by a radio that cannot address one even when the
// request carries nothing else. The VFO is a selector rather than a field, so
// such a request looks empty — and the empty short-circuit used to answer it
// 200 with the current state, telling the client a selection it never made had
// been accepted. On an FT-857D, whose one VFO command is a blind toggle,
// {"vfo": "B"} came back OK having done nothing at all.
//
// The session is deliberately unstarted: this is a validation failure, and it
// must be reported on its merits rather than as "radio unreachable".
func TestPatchNamedVFORefusedWhenNothingElseIsSet(t *testing.T) {
	h := newHarness(t, nil)
	if h.s.Connected() {
		t.Fatal("session should not be connected before Start")
	}

	for _, vfo := range []radio.VFO{radio.VFOA, radio.VFOB, radio.VFOMain, radio.VFOSub} {
		t.Run(vfo.String(), func(t *testing.T) {
			_, err := h.s.ApplyPatch(context.Background(), PatchRequest{VFO: vfo})
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("ApplyPatch{VFO: %s} err = %v, want ErrUnsupported", vfo, err)
			}
			if errors.Is(err, ErrDisconnected) {
				t.Error("an unsupported VFO reported as a connection failure")
			}
		})
	}

	// The zero value addresses whichever VFO the radio is on, which every radio
	// has, so a request carrying only that is still the no-op it always was.
	t.Run("current is still an empty request", func(t *testing.T) {
		if _, err := h.s.ApplyPatch(context.Background(), PatchRequest{VFO: radio.VFOCurrent}); err != nil {
			t.Fatalf("ApplyPatch{VFO: current} err = %v, want nil", err)
		}
	})
}

// A mode change that says nothing about data mode carries the rig's current
// flag forward, which is right when both spellings exist and wrong when the
// target mode has no data code: the operator asked for CW and got a refusal
// naming a data mode they never mentioned. On an FT-857D left in PKT — that
// radio's FM-with-data — every bare mode change was refused, including the one
// to DIG, its other data mode, so there was no way out of PKT at all.
func TestPatchCarriedDataModeFallsBackToPlainMode(t *testing.T) {
	h := newHarness(t, nil).start(t)
	ctx := context.Background()

	usb, yes := radio.ModeUSB, true
	if _, err := h.s.ApplyPatch(ctx, PatchRequest{Mode: &usb, DataMode: &yes}); err != nil {
		t.Fatalf("into USB with data mode: %v", err)
	}
	if st := h.s.State(); !st.DataMode {
		t.Fatalf("data mode = false after asking for it, state %+v", st)
	}

	h.rig.setNoDataMode(radio.ModeCW)

	cw := radio.ModeCW
	st, err := h.s.ApplyPatch(ctx, PatchRequest{Mode: &cw})
	if err != nil {
		t.Fatalf("bare mode change out of a data mode: %v", err)
	}
	if st.Mode != radio.ModeCW {
		t.Errorf("mode = %s, want CW", st.Mode)
	}
	if st.DataMode {
		t.Error("data mode still on after falling back to the plain mode")
	}

	// Both halves should be on the wire log, in order: the carried-forward
	// attempt, then the retry that dropped the flag nobody asked for.
	want := []string{"mode=CW data=true", "mode=CW data=false"}
	got := h.rig.setLog()
	if len(got) < len(want) {
		t.Fatalf("set log = %v, want it to end with %v", got, want)
	}
	tail := got[len(got)-len(want):]
	for i := range want {
		if tail[i] != want[i] {
			t.Fatalf("set log tail = %v, want %v", tail, want)
		}
	}
}

// The fallback covers a flag that was carried forward, never one that was
// asked for. A client that spells out data_mode: true on a mode with no data
// code gets the refusal, because that request really was made — and it must not
// be quietly turned into the plain mode.
func TestPatchExplicitDataModeIsStillRefused(t *testing.T) {
	h := newHarness(t, nil).start(t)
	ctx := context.Background()

	h.rig.setNoDataMode(radio.ModeCW)

	before := len(h.rig.setLog())
	cw, yes := radio.ModeCW, true
	if _, err := h.s.ApplyPatch(ctx, PatchRequest{Mode: &cw, DataMode: &yes}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if n := len(h.rig.setLog()) - before; n != 1 {
		t.Errorf("%d set commands issued, want 1: an explicit request must not be retried", n)
	}
}
